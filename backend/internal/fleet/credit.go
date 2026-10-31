package fleet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jmoiron/sqlx"

	"tokenfactory/pkg/errcode"
)

// 供给方信用分(T-105, 落地 docs/15 T-023 的 credit_scores)与自动停调度。
//
// 近 30 天口径:
//   - 停机: 供给方责任且计入(counted=1)的停机秒数/段数(按停机结束时间);
//   - 探活失败: 已验证节点完整探活失败(审计 node.verify_failed);
//   - 违约: 落在该供给方节点上的租约开通失败(leases.state='failed', 按 updated_at)。
//
// score = 100 - P停机 - P探活 - P违约(下限 0):
//   P停机 = ceil(50 × 停机分钟 ÷ fleet.outage.max_supplier_minutes_30d), 封顶 60;
//   P探活 = 5 × 次数, 封顶 20; P违约 = 10 × 次数, 封顶 30。
//
// 停调度(supplier_nodes.dispatch_hold=1, 不接新托管租约, 已在租的不受影响):
//   - 供给方: override=stop, 或 override=auto 且 score < fleet.credit.stop_below → 其全部节点;
//   - 单节点: override≠allow 且该节点近 30 天供给方责任停机分钟 > max_supplier_minutes_30d(docs/25 §25.7.6 供给方约束)。
// 运营覆盖 allow 后若又发生新的计入停机(结束时间晚于覆盖时间), 覆盖自动回到 auto。

const creditInterval = 10 * time.Minute

// CreditScore credit_scores 一行(fleet 视角)。
type CreditScore struct {
	SupplierID       int64      `db:"supplier_id" json:"supplier_id"`
	Score            int        `db:"score" json:"score"`
	OutageMinutes30d int        `db:"outage_minutes_30d" json:"outage_minutes_30d"`
	OutageCount30d   int        `db:"outage_count_30d" json:"outage_count_30d"`
	ProbeFailures30d int        `db:"probe_failures_30d" json:"probe_failures_30d"`
	Defaults30d      int        `db:"defaults_30d" json:"defaults_30d"`
	DispatchStopped  bool       `db:"dispatch_stopped" json:"dispatch_stopped"`
	OverrideMode     string     `db:"override_mode" json:"override_mode"`
	OverrideBy       *int64     `db:"override_by" json:"override_by"`
	OverrideReason   *string    `db:"override_reason" json:"override_reason"`
	OverrideAt       *time.Time `db:"override_at" json:"override_at"`
	ComputedAt       *time.Time `db:"computed_at" json:"computed_at"`
}

const creditCols = `supplier_id, score, outage_minutes_30d, outage_count_30d, probe_failures_30d, defaults_30d,
	dispatch_stopped, override_mode, override_by, override_reason, override_at, computed_at`

func creditScore(outageMin, probeFails, defaults, maxMin int) int {
	if maxMin <= 0 {
		maxMin = 360
	}
	pOut := (50*outageMin + maxMin - 1) / maxMin
	pOut = min(pOut, 60)
	pProbe := min(5*probeFails, 20)
	pDef := min(10*defaults, 30)
	return max(100-pOut-pProbe-pDef, 0)
}

// CreditRound 重算所有托管节点供给方的信用与停调度(导出供测试同步调用)。
func (s *Service) CreditRound(ctx context.Context) {
	var ids []int64
	if err := s.repo.db.Select(&ids, `SELECT DISTINCT supplier_id FROM supplier_nodes WHERE mgmt_mode='ssh'`); err != nil {
		slog.Error("信用重算: 查询供给方失败", "error", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if err := s.RecomputeCredit(id); err != nil {
			slog.Error("信用重算失败", "supplier_id", id, "error", err)
		}
	}
}

// RecomputeCredit 重算一个供给方的信用分并同步其节点 dispatch_hold。
func (s *Service) RecomputeCredit(supplierID int64) error {
	db := s.repo.db
	var agg struct {
		Secs  int64 `db:"secs"`
		Count int   `db:"cnt"`
	}
	if err := db.Get(&agg, `SELECT COALESCE(SUM(o.seconds),0) AS secs, COUNT(*) AS cnt FROM lease_outages o
		JOIN supplier_nodes n ON n.id=o.node_id
		WHERE n.supplier_id=? AND o.liable='supplier' AND o.counted=1 AND o.ended_at >= NOW() - INTERVAL 30 DAY`,
		supplierID); err != nil {
		return err
	}
	var probeFails, defaults int
	if err := db.Get(&probeFails, `SELECT COUNT(*) FROM audit_logs a JOIN supplier_nodes n ON n.id=a.target_id
		WHERE a.target_type='supplier_node' AND a.action='node.verify_failed' AND n.supplier_id=?
		  AND a.created_at >= NOW() - INTERVAL 30 DAY`, supplierID); err != nil {
		return err
	}
	if err := db.Get(&defaults, `SELECT COUNT(*) FROM leases l JOIN supplier_nodes n ON n.id=l.node_id
		WHERE n.supplier_id=? AND l.state='failed' AND l.updated_at >= NOW() - INTERVAL 30 DAY`, supplierID); err != nil {
		return err
	}
	maxMin := s.cfg.Outage.MaxSupplierMinutes30d
	outMin := int(agg.Secs / 60)
	score := creditScore(outMin, probeFails, defaults, maxMin)
	if _, err := db.Exec(`INSERT INTO credit_scores (supplier_id, score, outage_minutes_30d, outage_count_30d,
		probe_failures_30d, defaults_30d, computed_at) VALUES (?,?,?,?,?,?,NOW())
		ON DUPLICATE KEY UPDATE score=VALUES(score), outage_minutes_30d=VALUES(outage_minutes_30d),
		outage_count_30d=VALUES(outage_count_30d), probe_failures_30d=VALUES(probe_failures_30d),
		defaults_30d=VALUES(defaults_30d), computed_at=NOW()`,
		supplierID, score, outMin, agg.Count, probeFails, defaults); err != nil {
		return err
	}
	c, err := s.getCredit(db, supplierID)
	if err != nil || c == nil {
		return err
	}
	if c.OverrideMode == "allow" && c.OverrideAt != nil {
		var fresh int
		if err := db.Get(&fresh, `SELECT COUNT(*) FROM lease_outages o JOIN supplier_nodes n ON n.id=o.node_id
			WHERE n.supplier_id=? AND o.liable='supplier' AND o.counted=1 AND o.ended_at > ?`, supplierID, *c.OverrideAt); err != nil {
			return err
		}
		if fresh > 0 {
			ok, err := affected(db.Exec(`UPDATE credit_scores SET override_mode='auto' WHERE supplier_id=? AND override_mode='allow'
				AND override_at=?`, supplierID, *c.OverrideAt))
			if err != nil {
				return err
			}
			if ok {
				slog.Warn("运营放行后再次发生供给方责任停机, 覆盖恢复为自动", "supplier_id", supplierID)
				c.OverrideMode = "auto"
			}
		}
	}
	stopped := c.OverrideMode == "stop" || (c.OverrideMode == "auto" && score < s.cfg.Credit.StopBelow)
	if _, err := db.Exec(`UPDATE credit_scores SET dispatch_stopped=? WHERE supplier_id=? AND dispatch_stopped<>?`,
		stopped, supplierID, stopped); err != nil {
		return err
	}
	return s.syncDispatchHolds(supplierID, c.OverrideMode, score, stopped)
}

func (s *Service) getCredit(q sqlx.Queryer, supplierID int64) (*CreditScore, error) {
	var c CreditScore
	err := sqlx.Get(q, &c, `SELECT `+creditCols+` FROM credit_scores WHERE supplier_id=?`, supplierID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &c, err
}

// nodeOutageMinutes30d 每个节点近 30 天供给方责任且计入的停机分钟。
func (s *Service) nodeOutageMinutes30d(supplierID int64) (map[int64]int, error) {
	var rows []struct {
		NodeID int64 `db:"node_id"`
		Secs   int64 `db:"secs"`
	}
	if err := s.repo.db.Select(&rows, `SELECT o.node_id, COALESCE(SUM(o.seconds),0) AS secs FROM lease_outages o
		JOIN supplier_nodes n ON n.id=o.node_id
		WHERE n.supplier_id=? AND o.liable='supplier' AND o.counted=1 AND o.ended_at >= NOW() - INTERVAL 30 DAY
		GROUP BY o.node_id`, supplierID); err != nil {
		return nil, err
	}
	m := make(map[int64]int, len(rows))
	for _, r := range rows {
		m[r.NodeID] = int(r.Secs / 60)
	}
	return m, nil
}

// syncDispatchHolds 按供给方结论与单节点停机累计设置 dispatch_hold; 变化的节点同事务重算派生库存。
func (s *Service) syncDispatchHolds(supplierID int64, mode string, score int, stopped bool) error {
	var nodes []struct {
		ID     int64   `db:"id"`
		Hold   bool    `db:"dispatch_hold"`
		Name   string  `db:"node_name"`
		Reason *string `db:"dispatch_hold_reason"`
	}
	if err := s.repo.db.Select(&nodes, `SELECT id, dispatch_hold, node_name, dispatch_hold_reason FROM supplier_nodes
		WHERE supplier_id=? AND mgmt_mode='ssh'`, supplierID); err != nil {
		return err
	}
	perNode, err := s.nodeOutageMinutes30d(supplierID)
	if err != nil {
		return err
	}
	maxMin := s.cfg.Outage.MaxSupplierMinutes30d
	for _, n := range nodes {
		want, reason := false, ""
		switch {
		case stopped && mode == "stop":
			want, reason = true, "运营暂停该供给方调度"
		case stopped:
			want, reason = true, fmt.Sprintf("供给方信用分 %d 低于阈值 %d", score, s.cfg.Credit.StopBelow)
		case mode != "allow" && perNode[n.ID] > maxMin:
			want, reason = true, fmt.Sprintf("近 30 天供给方责任停机 %d 分钟, 超过上限 %d 分钟", perNode[n.ID], maxMin)
		}
		if want == n.Hold && (!want || n.Reason != nil && *n.Reason == reason) {
			continue
		}
		var changed bool
		err := s.inTx(func(tx *sqlx.Tx) error {
			cur, err := s.repo.lockNode(tx, n.ID)
			if err != nil || cur == nil {
				return err
			}
			if want {
				changed, err = affected(tx.Exec(`UPDATE supplier_nodes SET dispatch_hold=1, dispatch_hold_reason=?,
					dispatch_hold_at=IF(dispatch_hold=1, dispatch_hold_at, NOW()) WHERE id=? AND NOT (dispatch_hold=1 AND dispatch_hold_reason <=> ?)`,
					reason, n.ID, reason))
			} else {
				changed, err = affected(tx.Exec(`UPDATE supplier_nodes SET dispatch_hold=0, dispatch_hold_reason=NULL,
					dispatch_hold_at=NULL WHERE id=? AND dispatch_hold=1`, n.ID))
			}
			if err != nil || !changed {
				return err
			}
			return s.recomputeStockTx(tx, cur.ProductID)
		})
		if err != nil {
			return err
		}
		if !changed || want == n.Hold {
			continue
		}
		if want {
			slog.Warn("节点已自动停调度", "node_id", n.ID, "supplier_id", supplierID, "reason", reason)
			s.notify(supplierID, "节点已暂停接单",
				fmt.Sprintf("节点「%s」已暂停分配新租约(%s), 已在租的租约不受影响。如有异议请联系运营复核。", n.Name, reason), n.ID)
		} else {
			slog.Info("节点恢复调度", "node_id", n.ID, "supplier_id", supplierID)
			s.notify(supplierID, "节点恢复接单", fmt.Sprintf("节点「%s」已恢复分配新租约。", n.Name), n.ID)
		}
	}
	return nil
}

// ===== 运营接口 =====

// SupplierCreditDetail 运营查看: 信用分 + 节点停调度状态。
type SupplierCreditDetail struct {
	CreditScore
	StopBelow int                  `json:"stop_below"`
	Nodes     []SupplierCreditNode `json:"nodes"`
}

type SupplierCreditNode struct {
	ID                 int64      `db:"id" json:"id"`
	NodeName           string     `db:"node_name" json:"node_name"`
	DispatchHold       bool       `db:"dispatch_hold" json:"dispatch_hold"`
	DispatchHoldReason *string    `db:"dispatch_hold_reason" json:"dispatch_hold_reason"`
	DispatchHoldAt     *time.Time `db:"dispatch_hold_at" json:"dispatch_hold_at"`
	Schedulable        bool       `db:"schedulable" json:"schedulable"`
	OutageMinutes30d   int        `db:"-" json:"outage_minutes_30d"`
}

// AdminSupplierCredit 查看(先即时重算, 保证展示与停调度结论一致)。供给方没有托管节点时 40400。
func (s *Service) AdminSupplierCredit(supplierID int64) (*SupplierCreditDetail, error) {
	var n int
	if err := s.repo.db.Get(&n, `SELECT COUNT(*) FROM supplier_nodes WHERE supplier_id=? AND mgmt_mode='ssh'`, supplierID); err != nil {
		return nil, err
	}
	if n == 0 {
		return nil, errcode.New(errcode.NotFound, "该供给方没有平台托管节点")
	}
	if err := s.RecomputeCredit(supplierID); err != nil {
		return nil, err
	}
	c, err := s.getCredit(s.repo.db, supplierID)
	if err != nil || c == nil {
		return nil, err
	}
	d := &SupplierCreditDetail{CreditScore: *c, StopBelow: s.cfg.Credit.StopBelow, Nodes: []SupplierCreditNode{}}
	if err := s.repo.db.Select(&d.Nodes, `SELECT id, node_name, dispatch_hold, dispatch_hold_reason, dispatch_hold_at, schedulable
		FROM supplier_nodes WHERE supplier_id=? AND mgmt_mode='ssh' ORDER BY id`, supplierID); err != nil {
		return nil, err
	}
	per, err := s.nodeOutageMinutes30d(supplierID)
	if err != nil {
		return nil, err
	}
	for i := range d.Nodes {
		d.Nodes[i].OutageMinutes30d = per[d.Nodes[i].ID]
	}
	return d, nil
}

// AdminSupplierCredits 列表(按分数升序; stopped=1 只看已停调度)。读已落库结果, 不逐个重算。
func (s *Service) AdminSupplierCredits(onlyStopped bool, page, size int) ([]CreditScore, int, error) {
	where := "1=1"
	if onlyStopped {
		where = "dispatch_stopped=1"
	}
	var total int
	if err := s.repo.db.Get(&total, `SELECT COUNT(*) FROM credit_scores WHERE `+where); err != nil {
		return nil, 0, err
	}
	list := []CreditScore{}
	err := s.repo.db.Select(&list, `SELECT `+creditCols+` FROM credit_scores WHERE `+where+`
		ORDER BY score ASC, supplier_id ASC LIMIT ? OFFSET ?`, size, (page-1)*size)
	return list, total, err
}

// AdminOverrideCredit 运营覆盖: auto=按分数; allow=强制放行(含单节点停机超限); stop=强制停调度。allow/stop 须填理由。
func (s *Service) AdminOverrideCredit(operatorID, supplierID int64, mode, reason, ip string) (*SupplierCreditDetail, error) {
	switch mode {
	case "auto", "allow", "stop":
	default:
		return nil, errcode.New(errcode.ParamInvalid, "mode 只能是 auto / allow / stop")
	}
	if mode != "auto" && reason == "" {
		return nil, errcode.New(errcode.ParamInvalid, "放行或停调度必须填写理由")
	}
	if _, err := s.AdminSupplierCredit(supplierID); err != nil { // 校验存在 + 确保行已建
		return nil, err
	}
	err := s.inTx(func(tx *sqlx.Tx) error {
		var before CreditScore
		if err := tx.Get(&before, `SELECT `+creditCols+` FROM credit_scores WHERE supplier_id=? FOR UPDATE`, supplierID); err != nil {
			return err
		}
		ok, err := affected(tx.Exec(`UPDATE credit_scores SET override_mode=?, override_by=?, override_reason=?, override_at=NOW()
			WHERE supplier_id=?`, mode, operatorID, nullStr(truncate(reason, 254)), supplierID))
		if err != nil {
			return err
		}
		if !ok {
			return errcode.New(errcode.Conflict, "信用记录已变化, 请刷新后重试")
		}
		return s.repo.audit(tx, operatorID, "supplier.credit_override", "supplier", supplierID,
			map[string]any{"override_mode": before.OverrideMode, "score": before.Score},
			map[string]any{"override_mode": mode, "reason": reason}, ip)
	})
	if err != nil {
		return nil, err
	}
	return s.AdminSupplierCredit(supplierID)
}
