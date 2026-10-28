package fleet

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"github.com/jmoiron/sqlx"
)

// node_tasks: DB 行即队列(docs/25 §25.11), 与区块链 worker 同一模式。

// activeProbe 节点上未结束的 probe 任务(进行中重复提交返回同一任务)。
func (r *Repository) activeProbe(q sqlx.Queryer, nodeID int64) (*Task, error) {
	var t Task
	err := sqlx.Get(q, &t, `SELECT `+taskColumns+` FROM node_tasks
		WHERE node_id=? AND kind='probe' AND state IN ('pending','running') ORDER BY id LIMIT 1`, nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &t, err
}

func (r *Repository) insertTask(q sqlx.Execer, nodeID int64, kind string, priority int, payload any) (int64, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	res, err := q.Exec(`INSERT INTO node_tasks (node_id, kind, state, priority, payload) VALUES (?,?,'pending',?,?)`,
		nodeID, kind, priority, string(raw))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// insertLeaseTask 租约相关任务(provision/stop/purge/kill_sessions), 写 lease_id 便于运营按租约检索。
func (r *Repository) insertLeaseTask(q sqlx.Execer, nodeID, leaseID int64, kind string, priority int, payload any) (int64, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	res, err := q.Exec(`INSERT INTO node_tasks (node_id, lease_id, kind, state, priority, payload) VALUES (?,?,?,'pending',?,?)`,
		nodeID, leaseID, kind, priority, string(raw))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (r *Repository) getTask(q sqlx.Queryer, id int64) (*Task, error) {
	var t Task
	err := sqlx.Get(q, &t, `SELECT `+taskColumns+` FROM node_tasks WHERE id=?`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &t, err
}

// claimNext 认领一条到期的指定类型任务(单一锁时长; 测试与 S1 调用方使用)。
func (r *Repository) claimNext(kinds []string, lock time.Duration) (*Task, error) {
	locks := make(map[string]time.Duration, len(kinds))
	for _, k := range kinds {
		locks[k] = lock
	}
	return r.claimNextFor(locks)
}

// claimNextFor 认领一条到期的 pending 任务, locks 为可认领类型 → 锁时长。同一节点同时只运行一个任务:
// 先锁节点行, 确认无未过期的 running 任务, 再守卫式 pending→running(RowsAffected=1 才算认领成功)。
// 候选按 priority(小者优先)、run_after、id 排序。
func (r *Repository) claimNextFor(locks map[string]time.Duration) (*Task, error) {
	if len(locks) == 0 {
		return nil, nil
	}
	kinds := make([]string, 0, len(locks))
	for k := range locks {
		kinds = append(kinds, k)
	}
	q, args, err := sqlx.In(`SELECT `+taskColumns+` FROM node_tasks
		WHERE state='pending' AND run_after<=NOW() AND kind IN (?)
		  AND node_id NOT IN (SELECT node_id FROM node_tasks WHERE state='running' AND locked_until>NOW())
		ORDER BY priority, run_after, id LIMIT 8`, kinds)
	if err != nil {
		return nil, err
	}
	var cands []Task
	if err := r.db.Select(&cands, q, args...); err != nil {
		return nil, err
	}
	for _, c := range cands {
		t, err := r.tryClaim(c.ID, c.NodeID, locks[c.Kind])
		if err != nil {
			return nil, err
		}
		if t != nil {
			return t, nil
		}
	}
	return nil, nil
}

func (r *Repository) tryClaim(taskID, nodeID int64, lock time.Duration) (*Task, error) {
	tx, err := r.db.Beginx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// 节点行锁串行化同一节点的认领; 节点已删除时任务直接作废。
	var exists int
	if err := tx.Get(&exists, `SELECT COUNT(*) FROM supplier_nodes WHERE id=? FOR UPDATE`, nodeID); err != nil {
		return nil, err
	}
	if exists == 0 {
		if _, err := tx.Exec(`UPDATE node_tasks SET state='failed', last_error='节点已删除' WHERE id=? AND state='pending'`, taskID); err != nil {
			return nil, err
		}
		return nil, tx.Commit()
	}
	var running int
	if err := tx.Get(&running, `SELECT COUNT(*) FROM node_tasks WHERE node_id=? AND state='running' AND locked_until>NOW()`, nodeID); err != nil {
		return nil, err
	}
	if running > 0 {
		return nil, nil
	}
	ok, err := affected(tx.Exec(`UPDATE node_tasks SET state='running', attempts=attempts+1,
		locked_until=NOW() + INTERVAL ? SECOND WHERE id=? AND state='pending'`, int(lock.Seconds()), taskID))
	if err != nil || !ok {
		return nil, err
	}
	t, err := r.getTask(tx, taskID)
	if err != nil {
		return nil, err
	}
	return t, tx.Commit()
}

// finishTask 任务结束: running → succeeded/failed, 只写回仍由本次认领持有的任务。
func (r *Repository) finishTask(id int64, attempts int, state string, result any, lastErr string) (bool, error) {
	var raw any
	if result != nil {
		b, err := json.Marshal(result)
		if err != nil {
			return false, err
		}
		raw = string(b)
	}
	return affected(r.db.Exec(`UPDATE node_tasks SET state=?, result=?, last_error=?, locked_until=NULL
		WHERE id=? AND state='running' AND attempts=?`, state, raw, nullStr(truncate(lastErr, 500)), id, attempts))
}

// retryLater 可重试的失败: running → pending 并延后; 达到上限 → dead。
// 只有本次调用确实把任务转为 dead 才返回 dead=true(锁已过期被回收的陈旧回写不触发死信善后, 避免重复执行)。
func (r *Repository) retryLater(id int64, attempts int, delay time.Duration, lastErr string) (dead bool, err error) {
	tx, err := r.db.Beginx()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var maxAttempts []int
	if err := tx.Select(&maxAttempts, `SELECT max_attempts FROM node_tasks WHERE id=? AND state='running' AND attempts=? FOR UPDATE`,
		id, attempts); err != nil {
		return false, err
	}
	if len(maxAttempts) == 0 {
		return false, nil
	}
	dead = attempts >= maxAttempts[0]
	state := TaskPending
	if dead {
		state = TaskDead
	}
	if _, err := tx.Exec(`UPDATE node_tasks SET state=?, run_after = NOW() + INTERVAL ? SECOND, locked_until=NULL, last_error=?
		WHERE id=?`, state, int(delay.Seconds()), nullStr(truncate(lastErr, 500)), id); err != nil {
		return false, err
	}
	return dead, tx.Commit()
}

// reclaimExpired 锁过期视为 worker 崩溃: 回到 pending(延后按退避); attempts ≥ max → dead。返回本次转 dead 的任务。
// 同一事务内先锁定再更新, 只返回确由本次转 dead 的任务(死信善后不会重复执行)。
func (r *Repository) reclaimExpired() ([]Task, error) {
	tx, err := r.db.Beginx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var expired []Task
	if err := tx.Select(&expired, `SELECT `+taskColumns+` FROM node_tasks
		WHERE state='running' AND locked_until<=NOW() FOR UPDATE`); err != nil {
		return nil, err
	}
	var dead []Task
	for _, t := range expired {
		if t.Attempts >= t.MaxAttempts {
			if _, err := tx.Exec(`UPDATE node_tasks SET state='dead', locked_until=NULL,
				last_error=COALESCE(last_error, '执行超时(锁过期)') WHERE id=? AND state='running'`, t.ID); err != nil {
				return nil, err
			}
			t.State = TaskDead
			dead = append(dead, t)
			continue
		}
		if _, err := tx.Exec(`UPDATE node_tasks SET state='pending', locked_until=NULL, run_after=NOW() + INTERVAL ? SECOND
			WHERE id=? AND state='running'`, int(retryBackoff(t.Attempts).Seconds()), t.ID); err != nil {
			return nil, err
		}
	}
	return dead, tx.Commit()
}

// listTasks 运营查询任务(默认 dead)。
func (r *Repository) listTasks(state string, nodeID int64, page, pageSize int) ([]Task, int64, error) {
	where, args := `WHERE state=?`, []any{state}
	if nodeID > 0 {
		where += ` AND node_id=?`
		args = append(args, nodeID)
	}
	var total int64
	if err := r.db.Get(&total, `SELECT COUNT(*) FROM node_tasks `+where, args...); err != nil {
		return nil, 0, err
	}
	var list []Task
	err := r.db.Select(&list, `SELECT `+taskColumns+` FROM node_tasks `+where+` ORDER BY id DESC LIMIT ? OFFSET ?`,
		append(args, pageSize, (page-1)*pageSize)...)
	return list, total, err
}

// latestReportAt 最近一次指定类型探活报告时间(周期补跑判断)。
func (r *Repository) latestReportAt(nodeID int64, kind string) (*time.Time, error) {
	var t *time.Time
	err := r.db.Get(&t, `SELECT MAX(created_at) FROM node_probe_reports WHERE node_id=? AND kind=?`, nodeID, kind)
	return t, err
}

// ===== node_probe_reports =====

func (r *Repository) insertReport(q sqlx.Execer, nodeID int64, kind string, pass bool, checks []Check, inv any) (int64, error) {
	c, err := json.Marshal(checks)
	if err != nil {
		return 0, err
	}
	var invRaw any
	if inv != nil {
		b, err := json.Marshal(inv)
		if err != nil {
			return 0, err
		}
		invRaw = string(b)
	}
	result := "fail"
	if pass {
		result = "pass"
	}
	res, err := q.Exec(`INSERT INTO node_probe_reports (node_id, kind, result, checks, inventory) VALUES (?,?,?,?,?)`,
		nodeID, kind, result, string(c), invRaw)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// inventory 可空(连接阶段失败/待确认指纹的报告不含清单), 以 JSON null 返回, 否则 json.RawMessage 扫描失败。
func (r *Repository) listReports(nodeID int64, limit int) ([]ProbeReport, error) {
	var list []ProbeReport
	err := r.db.Select(&list, `SELECT id, node_id, kind, result, checks, COALESCE(inventory, 'null') AS inventory, created_at FROM node_probe_reports
		WHERE node_id=? ORDER BY id DESC LIMIT ?`, nodeID, limit)
	return list, err
}

// latestReports 每个节点最近一份报告(运营列表展示)。
func (r *Repository) latestReports(nodeIDs []int64) (map[int64]ProbeReport, error) {
	out := map[int64]ProbeReport{}
	if len(nodeIDs) == 0 {
		return out, nil
	}
	q, args, err := sqlx.In(`SELECT p.id, p.node_id, p.kind, p.result, p.checks, COALESCE(p.inventory, 'null') AS inventory, p.created_at
		FROM node_probe_reports p JOIN (SELECT node_id, MAX(id) mid FROM node_probe_reports WHERE node_id IN (?) GROUP BY node_id) m
		ON p.id=m.mid`, nodeIDs)
	if err != nil {
		return nil, err
	}
	var list []ProbeReport
	if err := r.db.Select(&list, q, args...); err != nil {
		return nil, err
	}
	for _, p := range list {
		out[p.NodeID] = p
	}
	return out, nil
}
