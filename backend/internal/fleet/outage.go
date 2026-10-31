package fleet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// 停机认定(T-105, docs/25 §25.7.6)。
//
// 一段停机 = lease_outages 一行: 开始时 INSERT(open_claim=lease_id, UNIQUE 保证同一租约同一时刻只有一段未结束),
// 结束时守卫式 UPDATE(open_claim IS NOT NULL → NULL)并在同一事务内结算补偿(默认顺延)。
// 同一时段多个原因只计一段: 已有未结束停机时新原因不另开行。
//
// 认定来源:
//   - 停机扫描(outageInterval): 节点离线/密钥不符 → node_offline; 节点完整探活失败 → full_probe_failed;
//     租约有非对账来源的 resource_alert(掉卡/ECC/Xid) → gpu_fault。整机租约无上述原因即结束;
//     容器租约须等对账确认容器在跑且通过(重启时的)网关自检(Up 观测)才结束 = "恢复在线且自检通过"。
//   - 容器对账观测(ObserveLease): container_down / gateway_down 开始; Up 结束(仍有硬原因时不结束)。
//   - 运营解冻认定误冻结(WrongFreeze): 冻结区间直接记一段已结束的平台责任停机。
//
// 买家原因/暂停的时间不计: 租约被冻结(suspended)时未结束的停机在 suspended_at 结束; 冻结期只有运营认定误冻结才计。
// 租约离开 active(到期/冻结/清除)时未结束的停机在 min(now, access_until, suspended_at) 结束。

const (
	OutageGPUFault        = "gpu_fault"
	OutageFullProbeFailed = "full_probe_failed"
	OutageGatewayDown     = "gateway_down"
	OutageWrongFreeze     = "wrong_freeze"

	outageInterval = time.Minute
)

// outageLiable 责任方(docs/25 §25.7.6 原因表): 网关与误冻结归平台, 其余归供给方。
func outageLiable(cause string) string {
	switch cause {
	case OutageGatewayDown, OutageWrongFreeze, "provision_delay":
		return "platform"
	default:
		return "supplier"
	}
}

// Outage lease_outages 一行。
type Outage struct {
	ID              int64      `db:"id" json:"id"`
	LeaseID         int64      `db:"lease_id" json:"lease_id"`
	OrderID         int64      `db:"order_id" json:"order_id"`
	NodeID          *int64     `db:"node_id" json:"node_id"`
	Cause           string     `db:"cause" json:"cause"`
	Liable          string     `db:"liable" json:"liable"`
	StartedAt       time.Time  `db:"started_at" json:"started_at"`
	EndedAt         *time.Time `db:"ended_at" json:"ended_at"`
	Seconds         *int       `db:"seconds" json:"seconds"`
	Minutes         *int       `db:"minutes" json:"minutes"`
	Counted         *bool      `db:"counted" json:"counted"`
	Remedy          *string    `db:"remedy" json:"remedy"`
	RemedyAppliedAt *time.Time `db:"remedy_applied_at" json:"remedy_applied_at"`
	RemedyAmount    *int64     `db:"remedy_amount" json:"remedy_amount"`
	LiabilityAmount *int64     `db:"liability_amount" json:"liability_amount"`
	Detail          *string    `db:"detail" json:"detail"`
}

const outageColumns = `id, lease_id, order_id, node_id, cause, liable, started_at, ended_at, seconds, minutes, counted,
	remedy, remedy_applied_at, remedy_amount, liability_amount, detail`

// OutageRefunder 停机退费(T-106)接入点: 对 remedy='refund' 且 remedy_applied_at IS NULL 的停机段发起部分退款,
// 成功后由实现方写 remedy_applied_at(payment_refunds UNIQUE(kind, source_id) 保证幂等)。
// 未注入时买家选择退费返回 50000, 已结束租期的未补偿停机停留在待退费状态。
type OutageRefunder interface {
	RequestOutageRefund(ctx context.Context, outageID int64) error
}

// SetOutageRefunder 注入停机退费实现(T-106)。
func (s *Service) SetOutageRefunder(r OutageRefunder) { s.refunder = r }

// outageBook 默认的停机认定实现(NewService 注入; 测试可用 SetOutageRecorder 覆盖)。
type outageBook struct{ s *Service }

func (b outageBook) ObserveLease(ctx context.Context, o LeaseObservation) {
	if err := b.s.observeOutage(ctx, o); err != nil {
		slog.Error("停机认定: 处理租约观测失败", "lease_no", o.LeaseNo, "cause", o.Cause, "up", o.Up, "error", err)
	}
}

func (b outageBook) WrongFreeze(ctx context.Context, leaseID int64, frozenAt, unfrozenAt time.Time) {
	if err := b.s.recordWrongFreeze(ctx, leaseID, frozenAt, unfrozenAt); err != nil {
		slog.Error("运营告警: 误冻结停机记录失败(需人工补记)", "lease_id", leaseID, "frozen_at", frozenAt,
			"unfrozen_at", unfrozenAt, "error", err)
	}
}

// outageNode 停机判定所需的节点状态。
type outageNode struct {
	Status          string     `db:"status"`
	VerifyStatus    string     `db:"verify_status"`
	LastProbeOKAt   *time.Time `db:"last_probe_ok_at"`
	LastHeartbeatAt *time.Time `db:"last_heartbeat_at"`
}

// hardCause 由节点状态与租约资源告警得出的停机原因(无则空串)及开始时间。
// node_offline 从最后一次探活成功算起(离线判定仍按 90s 阈值, 但买家自那之后已无法确认可用)。
func hardCause(l *Lease, n *outageNode, now time.Time) (cause, detail string, start time.Time) {
	switch {
	case n == nil:
		return OutageNodeOffline, "租约节点已不在平台托管", now
	case n.Status == "offline" || n.VerifyStatus == VerifyKeyMismatch:
		start = now
		if n.LastProbeOKAt != nil {
			start = *n.LastProbeOKAt
		} else if n.LastHeartbeatAt != nil {
			start = *n.LastHeartbeatAt
		}
		if n.VerifyStatus == VerifyKeyMismatch {
			return OutageNodeOffline, "节点主机密钥与登记不一致, 平台无法确认节点身份", start
		}
		return OutageNodeOffline, "节点探活失败已判离线", start
	case n.VerifyStatus == VerifyFailed:
		return OutageFullProbeFailed, "节点完整探活未通过", now
	case l.ResourceAlert != nil && *l.ResourceAlert != "" && !strings.HasPrefix(*l.ResourceAlert, reconcileAlertTag):
		return OutageGPUFault, *l.ResourceAlert, now
	}
	return "", "", now
}

func (s *Service) loadOutageNode(q sqlx.Queryer, nodeID int64) (*outageNode, error) {
	var n outageNode
	err := sqlx.Get(q, &n, `SELECT status, verify_status, last_probe_ok_at, last_heartbeat_at FROM supplier_nodes
		WHERE id=? AND mgmt_mode='ssh'`, nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &n, err
}

func (r *Repository) openOutage(q sqlx.Queryer, leaseID int64) (*Outage, error) {
	var o Outage
	err := sqlx.Get(q, &o, `SELECT `+outageColumns+` FROM lease_outages WHERE open_claim=?`, leaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &o, err
}

// startOutage 开一段停机(租约须 active)。已有未结束停机时不另开(同一时段只计一次), 返回 false。
// 开始时间不早于开通时间, 也不早于本租约上一段已结束停机的结束时间(避免回溯重叠)。
func (s *Service) startOutage(leaseID int64, cause, detail string, start time.Time) (bool, error) {
	var opened bool
	err := s.inTx(func(tx *sqlx.Tx) error {
		l, err := s.repo.lockLease(tx, leaseID)
		if err != nil || l == nil || l.State != LeaseActive {
			return err
		}
		if cur, err := s.repo.openOutage(tx, leaseID); err != nil || cur != nil {
			return err
		}
		now := time.Now()
		if start.After(now) {
			start = now
		}
		if l.ProvisionedAt != nil && start.Before(*l.ProvisionedAt) {
			start = *l.ProvisionedAt
		}
		var lastEnd sql.NullTime
		if err := tx.Get(&lastEnd, `SELECT MAX(ended_at) FROM lease_outages WHERE lease_id=?`, leaseID); err != nil {
			return err
		}
		if lastEnd.Valid && start.Before(lastEnd.Time) {
			start = lastEnd.Time
		}
		orderID, err := s.coveringOrderID(tx, l.OrderID, start)
		if err != nil {
			return err
		}
		_, err = tx.Exec(`INSERT INTO lease_outages (lease_id, order_id, node_id, cause, liable, started_at, open_claim, detail)
			VALUES (?,?,?,?,?,?,?,?)`, leaseID, orderID, l.NodeID, cause, outageLiable(cause), start, leaseID, truncate(detail, 254))
		if isDuplicate(err) {
			return nil // 并发已开
		}
		if err != nil {
			return err
		}
		opened = true
		slog.Warn("停机开始", "lease_no", l.LeaseNo, "node_id", l.NodeID, "cause", cause, "started_at", start, "detail", detail)
		return nil
	})
	return opened, err
}

// coveringOrderID 某时刻所属订单: 已生效续租单中开始时间不晚于该时刻的最新一单, 否则原订单。
func (s *Service) coveringOrderID(q sqlx.Queryer, rootID int64, at time.Time) (int64, error) {
	var id int64
	err := sqlx.Get(q, &id, `SELECT o.id FROM order_renewals r JOIN orders o ON o.id=r.order_id
		WHERE r.parent_order_id=? AND r.applied_at IS NOT NULL AND o.lease_start_at IS NOT NULL AND o.lease_start_at<=?
		ORDER BY o.lease_start_at DESC, o.id DESC LIMIT 1`, rootID, at)
	if errors.Is(err, sql.ErrNoRows) {
		return rootID, nil
	}
	return id, err
}

// observeOutage 处理对账观测。
func (s *Service) observeOutage(ctx context.Context, o LeaseObservation) error {
	if !o.Up {
		switch o.Cause {
		case OutageContainerDown, OutageGatewayDown:
			_, err := s.startOutage(o.LeaseID, o.Cause, o.Detail, o.At)
			return err
		default:
			// node_offline 以节点状态为准(探活 90s 阈值), 对账连不上只触发一次即时判定。
			return s.evaluateLease(ctx, o.LeaseID, false)
		}
	}
	return s.evaluateLease(ctx, o.LeaseID, true)
}

// evaluateLease 按当前节点/告警状态开或结束停机。upObserved=true 表示对账刚确认容器在跑(且重启时已过网关自检)。
func (s *Service) evaluateLease(ctx context.Context, leaseID int64, upObserved bool) error {
	l, err := s.repo.getLease(s.repo.db, leaseID)
	if err != nil || l == nil {
		return err
	}
	now := time.Now()
	if l.State != LeaseActive {
		return s.closeOutage(ctx, leaseID, leaveEnd(l, now), "租约已离开在租状态("+l.State+")")
	}
	n, err := s.loadOutageNode(s.repo.db, l.NodeID)
	if err != nil {
		return err
	}
	if cause, detail, start := hardCause(l, n, now); cause != "" {
		_, err := s.startOutage(leaseID, cause, detail, start)
		return err
	}
	if since, down := s.gatewayDownSince(ctx, now); down {
		// 网关失联期间对账 Up 观测也不结束(买家仍无法经网关登录)。
		_, err := s.startOutage(leaseID, OutageGatewayDown, "访问网关自 "+since.Format("2006-01-02 15:04:05")+" 起未向主服务上报", since)
		return err
	}
	if l.DeliveryKind == "container" && !upObserved {
		return nil // 容器租约等对账确认恢复
	}
	return s.closeOutage(ctx, leaseID, now, "")
}

// leaveEnd 租约离开 active 时未结束停机的结束时间 = min(now, access_until, suspended_at)。
func leaveEnd(l *Lease, now time.Time) time.Time {
	end := now
	if l.AccessUntil != nil && l.AccessUntil.Before(end) {
		end = *l.AccessUntil
	}
	if l.State == LeaseSuspended && l.SuspendedAt != nil && l.SuspendedAt.Before(end) {
		end = *l.SuspendedAt
	}
	return end
}

// OutageRound 一轮停机扫描(导出供测试同步调用): 在租租约逐个判定 + 已离开在租的未结束停机收尾。
func (s *Service) OutageRound(ctx context.Context) {
	var ids []int64
	if err := s.repo.db.Select(&ids, `SELECT id FROM leases WHERE state='active'
		UNION SELECT open_claim FROM lease_outages WHERE open_claim IS NOT NULL`); err != nil {
		slog.Error("停机扫描: 查询租约失败", "error", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if err := s.evaluateLease(ctx, id, false); err != nil {
			slog.Error("停机扫描: 判定失败", "lease_id", id, "error", err)
		}
	}
}

// recordWrongFreeze 误冻结: [frozenAt, unfrozenAt) 记一段已结束的平台责任停机并结算补偿。幂等: 同租约同开始时间只记一次。
func (s *Service) recordWrongFreeze(ctx context.Context, leaseID int64, frozenAt, unfrozenAt time.Time) error {
	if !unfrozenAt.After(frozenAt) {
		return fmt.Errorf("误冻结区间非法: %s ~ %s", frozenAt, unfrozenAt)
	}
	l, err := s.repo.getLease(s.repo.db, leaseID)
	if err != nil || l == nil {
		return err
	}
	var id int64
	err = s.inTx(func(tx *sqlx.Tx) error {
		ord, err := s.products.LockManagedOrderTx(tx, l.OrderID)
		if err != nil {
			return err
		}
		cur, err := s.repo.lockLease(tx, leaseID)
		if err != nil || cur == nil {
			return err
		}
		var n int
		if err := tx.Get(&n, `SELECT COUNT(*) FROM lease_outages WHERE lease_id=? AND cause='wrong_freeze' AND started_at=?`,
			leaseID, frozenAt); err != nil || n > 0 {
			return err
		}
		orderID, err := s.coveringOrderID(tx, cur.OrderID, frozenAt)
		if err != nil {
			return err
		}
		res, err := tx.Exec(`INSERT INTO lease_outages (lease_id, order_id, node_id, cause, liable, started_at, ended_at, detail)
			VALUES (?,?,?,?,?,?,?,?)`, leaseID, orderID, cur.NodeID, OutageWrongFreeze, outageLiable(OutageWrongFreeze),
			frozenAt, unfrozenAt, "运营解冻时认定为误冻结")
		if err != nil {
			return err
		}
		if id, err = res.LastInsertId(); err != nil {
			return err
		}
		return s.settleOutageTx(tx, ord, cur, id)
	})
	if err == nil && id > 0 {
		s.afterSettle(ctx, id)
	}
	return err
}
