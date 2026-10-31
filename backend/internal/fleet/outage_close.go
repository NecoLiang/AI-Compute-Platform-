package fleet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/jmoiron/sqlx"

	"tokenfactory/internal/compute"
)

// closeOutage 结束租约的未结束停机(结束时间夹在 [started_at, now]), 同事务结算补偿。无未结束停机时什么也不做。
// 锁顺序 订单 → 租约 → 停机行。
func (s *Service) closeOutage(ctx context.Context, leaseID int64, end time.Time, note string) error {
	l, err := s.repo.getLease(s.repo.db, leaseID)
	if err != nil || l == nil {
		return err
	}
	if o, err := s.repo.openOutage(s.repo.db, leaseID); err != nil || o == nil {
		return err
	}
	var closedID int64
	err = s.inTx(func(tx *sqlx.Tx) error {
		ord, err := s.products.LockManagedOrderTx(tx, l.OrderID)
		if err != nil {
			return err
		}
		cur, err := s.repo.lockLease(tx, leaseID)
		if err != nil || cur == nil {
			return err
		}
		var o Outage
		err = tx.Get(&o, `SELECT `+outageColumns+` FROM lease_outages WHERE open_claim=? FOR UPDATE`, leaseID)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil {
			return err
		}
		now := time.Now()
		if end.After(now) {
			end = now
		}
		if end.Before(o.StartedAt) {
			end = o.StartedAt
		}
		ok, err := affected(tx.Exec(`UPDATE lease_outages SET ended_at=?, open_claim=NULL WHERE id=? AND open_claim IS NOT NULL`,
			end, o.ID))
		if err != nil || !ok {
			return err
		}
		closedID = o.ID
		slog.Info("停机结束", "lease_no", cur.LeaseNo, "cause", o.Cause, "started_at", o.StartedAt, "ended_at", end, "note", note)
		return s.settleOutageTx(tx, ord, cur, o.ID)
	})
	if err == nil && closedID > 0 {
		s.afterSettle(ctx, closedID)
	}
	return err
}

// settleOutageTx 已结束停机段的计时与补偿(调用方已锁订单与租约)。幂等: counted 非空即已结算。
//   - 有效秒数扣除与同租约其他已结束段的重叠; 原始时长 < min_minutes 或有效秒数为 0 → 不计(counted=0)。
//   - 责任金额 = floor(所属订单实付 × 有效秒数 ÷ 该订单租期秒数), 由 liable 方承担(D-9/D-10)。
//   - 买家选顺延且租约仍在租期内: 同事务顺延 access_until / 订单 lease_end_at / 交付有效期 / 待支付续租的起止,
//     remedy='extend' 并写 remedy_applied_at。
//   - 否则(选了退费, 或租期已结束): remedy='refund', remedy_amount=责任金额, remedy_applied_at 留空由 T-106 退款后写。
func (s *Service) settleOutageTx(tx *sqlx.Tx, ord *compute.Order, l *Lease, id int64) error {
	var o Outage
	if err := tx.Get(&o, `SELECT `+outageColumns+` FROM lease_outages WHERE id=? FOR UPDATE`, id); err != nil {
		return err
	}
	if o.EndedAt == nil || o.Counted != nil {
		return nil
	}
	raw := o.EndedAt.Sub(o.StartedAt)
	secs, err := s.effectiveSeconds(tx, &o)
	if err != nil {
		return err
	}
	minDur := time.Duration(s.cfg.Outage.MinMinutes) * time.Minute
	if raw < minDur || secs <= 0 {
		ok, err := affected(tx.Exec(`UPDATE lease_outages SET seconds=?, minutes=?, counted=0, liability_amount=0,
			remedy_applied_at=NOW() WHERE id=? AND counted IS NULL`, secs, secs/60, id))
		if err == nil && !ok {
			err = fmt.Errorf("停机段 %d 结算守卫未命中", id)
		}
		return err
	}
	liab, err := s.liabilityAmount(tx, o.OrderID, secs)
	if err != nil {
		slog.Error("运营告警: 停机责任金额无法计算(需人工核定)", "outage_id", id, "order_id", o.OrderID, "error", err)
		liab = nil
	}
	now := time.Now()
	if l.OutageRemedy == "extend" && extendable(ord, l, now) {
		ok, err := affected(tx.Exec(`UPDATE lease_outages SET seconds=?, minutes=?, counted=1, liability_amount=?,
			remedy='extend', remedy_applied_at=NOW() WHERE id=? AND counted IS NULL`, secs, secs/60, liab, id))
		if err != nil {
			return err
		}
		if !ok {
			return fmt.Errorf("停机段 %d 结算守卫未命中", id)
		}
		if err := s.extendForOutageTx(tx, ord, l, time.Duration(secs)*time.Second); err != nil {
			return err
		}
		slog.Info("停机补偿: 已顺延", "lease_no", l.LeaseNo, "outage_id", id, "seconds", secs, "liable", o.Liable)
		return nil
	}
	ok, err := affected(tx.Exec(`UPDATE lease_outages SET seconds=?, minutes=?, counted=1, liability_amount=?,
		remedy='refund', remedy_amount=? WHERE id=? AND counted IS NULL`, secs, secs/60, liab, liab, id))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("停机段 %d 结算守卫未命中", id)
	}
	slog.Warn("停机补偿: 待退费(T-106)", "lease_no", l.LeaseNo, "outage_id", id, "seconds", secs, "amount", liab)
	return nil
}

// extendable 顺延的前提: 租约仍在租期内(在租/停放/冻结)且订单未终结。
func extendable(ord *compute.Order, l *Lease, now time.Time) bool {
	if ord == nil || ord.LeaseEnd == nil || l.AccessUntil == nil || !l.AccessUntil.After(now) {
		return false
	}
	switch l.State {
	case LeaseActive, LeaseParked, LeaseSuspended:
	default:
		return false
	}
	return ord.Status == "active" || ord.Status == "frozen"
}

// extendForOutageTx 把租期整体后移 d(docs/25 §25.7.6 顺延补偿), 全部守卫式。
func (s *Service) extendForOutageTx(tx *sqlx.Tx, ord *compute.Order, l *Lease, d time.Duration) error {
	secs := int(d / time.Second)
	ok, err := affected(tx.Exec(`UPDATE leases SET access_until=? WHERE id=? AND access_until=?
		AND state IN ('active','parked','suspended')`, l.AccessUntil.Add(d), l.ID, *l.AccessUntil))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("顺延失败: 租约 %s 租期已变化", l.LeaseNo)
	}
	if err := s.repo.touchAccess(tx, l.ID); err != nil {
		return err
	}
	oldEnd := *ord.LeaseEnd
	ok, err = affected(tx.Exec(`UPDATE orders SET lease_end_at=? WHERE id=? AND lease_end_at=?`, oldEnd.Add(d), ord.ID, oldEnd))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("顺延失败: 订单 %s 租期已变化", ord.OrderNo)
	}
	if _, err := tx.Exec(`UPDATE order_deliveries SET access_expires_at=access_expires_at + INTERVAL ? SECOND
		WHERE order_id=? AND access_expires_at IS NOT NULL`, secs, ord.ID); err != nil {
		return err
	}
	// 待支付的顺延续租以旧结束时间为起点(支付时校验 parent.lease_end_at == lease_end_at), 一并后移。
	_, err = tx.Exec(`UPDATE order_renewals SET lease_end_at=?, renewed_until=renewed_until + INTERVAL ? SECOND
		WHERE parent_order_id=? AND applied_at IS NULL AND mode='extend' AND lease_end_at=?`,
		oldEnd.Add(d), secs, ord.ID, oldEnd)
	return err
}

// effectiveSeconds 本段扣除与同租约其他已结束段重叠部分后的秒数(同一时段多原因只计一次)。
func (s *Service) effectiveSeconds(q sqlx.Queryer, o *Outage) (int, error) {
	var others []struct {
		S time.Time `db:"started_at"`
		E time.Time `db:"ended_at"`
	}
	if err := sqlx.Select(q, &others, `SELECT started_at, ended_at FROM lease_outages
		WHERE lease_id=? AND id<>? AND ended_at IS NOT NULL AND started_at<? AND ended_at>?`,
		o.LeaseID, o.ID, *o.EndedAt, o.StartedAt); err != nil {
		return 0, err
	}
	sort.Slice(others, func(i, j int) bool { return others[i].S.Before(others[j].S) })
	total := o.EndedAt.Sub(o.StartedAt)
	cursor := o.StartedAt
	for _, x := range others {
		st, en := x.S, x.E
		if st.Before(cursor) {
			st = cursor
		}
		if en.After(*o.EndedAt) {
			en = *o.EndedAt
		}
		if en.After(st) {
			total -= en.Sub(st)
			cursor = en
		}
	}
	if total < 0 {
		total = 0
	}
	return int(total / time.Second), nil
}

// liabilityAmount floor(订单实付 × secs ÷ 订单租期秒数), 不超过实付。租期按订单计费方式快照换算。
func (s *Service) liabilityAmount(q sqlx.Queryer, orderID int64, secs int) (*int64, error) {
	var o compute.Order
	if err := sqlx.Get(q, &o, `SELECT id, order_no, total_amount, duration, pricing_mode, lease_start_at
		FROM orders WHERE id=?`, orderID); err != nil {
		return nil, err
	}
	if o.LeaseStart == nil {
		return nil, fmt.Errorf("订单 %s 无租期起点", o.OrderNo)
	}
	mode, err := compute.OrderPricingMode(&o)
	if err != nil {
		return nil, err
	}
	end, err := compute.LeaseEndAt(*o.LeaseStart, mode, o.Duration, nil)
	if err != nil {
		return nil, err
	}
	term := int64(end.Sub(*o.LeaseStart) / time.Second)
	if term <= 0 {
		return nil, fmt.Errorf("订单 %s 租期非法(计费方式 %s)", o.OrderNo, mode)
	}
	amt := o.TotalAmount * int64(secs) / term
	if amt > o.TotalAmount {
		amt = o.TotalAmount
	}
	return &amt, nil
}

// afterSettle 提交后: 待退费段交给 T-106(已注入时), 并重算供给方信用。
func (s *Service) afterSettle(ctx context.Context, id int64) {
	var o struct {
		Remedy    *string    `db:"remedy"`
		AppliedAt *time.Time `db:"remedy_applied_at"`
		Liable    string     `db:"liable"`
		NodeID    *int64     `db:"node_id"`
	}
	if err := s.repo.db.Get(&o, `SELECT remedy, remedy_applied_at, liable, node_id FROM lease_outages WHERE id=?`, id); err != nil {
		slog.Error("停机结算后读取失败", "outage_id", id, "error", err)
		return
	}
	if o.Remedy != nil && *o.Remedy == "refund" && o.AppliedAt == nil && s.refunder != nil {
		if err := s.refunder.RequestOutageRefund(ctx, id); err != nil {
			slog.Error("停机退费发起失败(将由 T-106 补偿扫描重试)", "outage_id", id, "error", err)
		}
	}
	if o.Liable == "supplier" && o.NodeID != nil {
		var supplierID int64
		if err := s.repo.db.Get(&supplierID, `SELECT supplier_id FROM supplier_nodes WHERE id=?`, *o.NodeID); err != nil {
			slog.Error("停机结算后读取节点供给方失败", "node_id", *o.NodeID, "error", err)
			return
		}
		if err := s.RecomputeCredit(supplierID); err != nil {
			slog.Error("供给方信用重算失败", "supplier_id", supplierID, "error", err)
		}
	}
}
