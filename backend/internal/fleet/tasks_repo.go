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

// activeProbes 节点上全部未结束的 probe 任务(按 id 升序)。
func (r *Repository) activeProbes(q sqlx.Queryer, nodeID int64) ([]Task, error) {
	var list []Task
	err := sqlx.Select(q, &list, `SELECT `+taskColumns+` FROM node_tasks
		WHERE node_id=? AND kind='probe' AND state IN ('pending','running') ORDER BY id`, nodeID)
	return list, err
}

// failPendingTask 作废一条仍在排队的任务(running 不动: 由执行方自行结束)。
func (r *Repository) failPendingTask(q sqlx.Execer, id int64, reason string) error {
	_, err := q.Exec(`UPDATE node_tasks SET state='failed', last_error=?, locked_until=NULL WHERE id=? AND state='pending'`, reason, id)
	return err
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

// insertLeaseTaskMax 同 insertLeaseTask, 显式指定最大尝试次数(stop/purge/kill_sessions 关系到释放卡槽与断开访问,
// 节点短暂失联不应很快转死信, 默认 10 次: 退避 1m,2m,4m…封顶 1h, 合计约 6 小时)。
func (r *Repository) insertLeaseTaskMax(q sqlx.Execer, nodeID, leaseID int64, kind string, priority, maxAttempts int, payload any) (int64, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return 0, err
	}
	res, err := q.Exec(`INSERT INTO node_tasks (node_id, lease_id, kind, state, priority, max_attempts, payload) VALUES (?,?,?,'pending',?,?,?)`,
		nodeID, leaseID, kind, priority, maxAttempts, string(raw))
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// liveLeaseTasks 租约上未结束(pending/running)的指定类型任务。
func (r *Repository) liveLeaseTasks(q sqlx.Queryer, leaseID int64, kind string) ([]Task, error) {
	var list []Task
	err := sqlx.Select(q, &list, `SELECT `+taskColumns+` FROM node_tasks
		WHERE lease_id=? AND kind=? AND state IN ('pending','running') ORDER BY id`, leaseID, kind)
	return list, err
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

// expressKinds 快车道任务(docs/25 §25.11 S2 实现口径): stop / kill_sessions 只执行一两条短命令且关系到
// 断开买家访问与释放卡槽, 不应排在同节点的长任务(拉镜像最长 30 分钟)之后。快车道任务不受"同节点同时只运行
// 一个任务"约束, 也不占用该约束; 但同一节点上同一租约同时只运行一个任务(快车道与普通任务一视同仁)。
var expressKinds = []string{KindStop, KindKillSessions}

func isExpressKind(kind string) bool {
	for _, k := range expressKinds {
		if k == kind {
			return true
		}
	}
	return false
}

// claimNextFor 认领一条到期的 pending 任务, locks 为可认领类型 → 锁时长。
// 普通任务: 同一节点同时只运行一个(快车道任务不计); 任意任务: 同一节点上同一租约同时只运行一个。
// 先锁节点行复查上述条件, 再守卫式 pending→running(RowsAffected=1 才算认领成功)。
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
		  AND (kind IN (?) OR node_id NOT IN (SELECT node_id FROM node_tasks
		        WHERE state='running' AND locked_until>NOW() AND kind NOT IN (?)))
		  AND (lease_id IS NULL OR (node_id, lease_id) NOT IN (SELECT node_id, lease_id FROM node_tasks
		        WHERE state='running' AND locked_until>NOW() AND lease_id IS NOT NULL))
		ORDER BY priority, run_after, id LIMIT 8`, kinds, expressKinds, expressKinds)
	if err != nil {
		return nil, err
	}
	var cands []Task
	if err := r.db.Select(&cands, q, args...); err != nil {
		return nil, err
	}
	for _, c := range cands {
		t, err := r.tryClaim(c, locks[c.Kind])
		if err != nil {
			return nil, err
		}
		if t != nil {
			return t, nil
		}
	}
	return nil, nil
}

func (r *Repository) tryClaim(c Task, lock time.Duration) (*Task, error) {
	taskID, nodeID := c.ID, c.NodeID
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
	if !isExpressKind(c.Kind) {
		q, args, err := sqlx.In(`SELECT COUNT(*) FROM node_tasks WHERE node_id=? AND state='running' AND locked_until>NOW()
			AND kind NOT IN (?)`, nodeID, expressKinds)
		if err != nil {
			return nil, err
		}
		var running int
		if err := tx.Get(&running, q, args...); err != nil {
			return nil, err
		}
		if running > 0 {
			return nil, nil
		}
	}
	if c.LeaseID != nil {
		// 同一节点上同一租约的任务互斥(节点行锁已串行化)。不同节点上的同租约任务(换节点后旧节点的残留清理)
		// 操作的是各自节点上的资源, 不互斥。
		var running int
		if err := tx.Get(&running, `SELECT COUNT(*) FROM node_tasks WHERE node_id=? AND lease_id=? AND state='running' AND locked_until>NOW()`,
			nodeID, *c.LeaseID); err != nil {
			return nil, err
		}
		if running > 0 {
			return nil, nil
		}
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

// renewLock 心跳续期: 只续仍由本次认领持有的任务(state='running' AND attempts=?)。
// 同一秒内两次续期写入值相同时 MySQL 返回 RowsAffected=0, 因此未改变时按守卫条件复查是否仍持有。
func (r *Repository) renewLock(id int64, attempts int, ttl time.Duration) (bool, error) {
	ok, err := affected(r.db.Exec(`UPDATE node_tasks SET locked_until = NOW() + INTERVAL ? SECOND
		WHERE id=? AND state='running' AND attempts=?`, int(ttl.Seconds()), id, attempts))
	if err != nil || ok {
		return ok, err
	}
	var n int
	if err := r.db.Get(&n, `SELECT COUNT(*) FROM node_tasks WHERE id=? AND state='running' AND attempts=?`, id, attempts); err != nil {
		return false, err
	}
	return n == 1, nil
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

// listTasks 运营查询任务; state 为空 = 全部状态。
func (r *Repository) listTasks(state string, nodeID int64, page, pageSize int) ([]Task, int64, error) {
	where, args := `WHERE 1=1`, []any{}
	if state != "" {
		where += ` AND state=?`
		args = append(args, state)
	}
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
