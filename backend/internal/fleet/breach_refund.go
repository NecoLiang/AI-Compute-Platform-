package fleet

import (
	"context"
	"log/slog"
	"math/big"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"tokenfactory/internal/compute"
	"tokenfactory/pkg/errcode"
)

// ===== 供给方违约整单退款(docs/25 §25.7.6 "供给方约束", T-106) =====
//
// 文档: 供给方责任停机累计 > 租期 10% 或单次 > 2h, 买家可申请"未使用部分整单退款"。
// 买家申请入口、与 Q-47 退费门槛的关系、是否自动触发均未定稿, T-106 只实现运营发起的路径:
//   - 门槛按当前所属订单(续租后为续租单)内的供给方责任停机计: 累计 ≥ 租期 × outage.refund_ratio 或
//     单次 ≥ outage.refund_single_hours(与 Q-47 同参数, 只计 liable=supplier; 进行中的停机按已持续时长计)。
//   - 进行中的停机先在当前时刻结束并按租约的补偿方式结算(顺延的时长随后计入"未使用部分")。
//   - 退款 = floor(所属订单实付 × 剩余访问秒数 ÷ 所属订单租期秒数), 封顶为该支付尚未登记部分退款的余额。
//   - 同一事务内: 访问期限截断到当前时刻(租约 / 订单 / 交付凭证), 登记 kind='supplier_breach'、source_id=租约 ID
//     的部分退款; 之后由到期任务照常进入宽限期 → 清除 → 租期末结算; 违约退款全额计入供给方责任。
//   - 有待支付 / 退款中的续租单, 或已预付的后续续租期时 40900(需先处理续租)。

// BreachRefunder 供给方违约退款接入点(payment 实现), 仅在部分退款能力已确认时注入。
type BreachRefunder interface {
	RegisterBreachRefundTx(tx *sqlx.Tx, orderNo string, leaseID, amount int64, reason string) (int64, error)
	SubmitPartialRefund(id int64) error
	RefundableRemainder(q sqlx.Queryer, orderNo string) (int64, error)
}

// SetBreachRefunder 注入供给方违约退款实现(T-106)。
func (s *Service) SetBreachRefunder(r BreachRefunder) { s.breach = r }

// BreachRefund 运营发起违约退款的结果。
type BreachRefund struct {
	OrderNo       string    `json:"order_no"`
	RefundOrderNo string    `json:"refund_order_no"` // 退款所在订单(续租后为当前所属续租单)
	LeaseNo       string    `json:"lease_no"`
	RefundID      int64     `json:"refund_id"`
	Amount        int64     `json:"amount"`
	AccessUntil   time.Time `json:"access_until"`
}

// AdminSupplierBreachRefund 运营对供给方违约的托管订单发起未使用部分退款。
func (s *Service) AdminSupplierBreachRefund(ctx context.Context, operatorID int64, orderNo, reason, ip string) (*BreachRefund, error) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return nil, errcode.New(errcode.ParamInvalid, "须填写违约退款理由")
	}
	if s.breach == nil || !s.cfg.Outage.YeepayPartialRefundConfirmed {
		return nil, errRefundUnavailable
	}
	o, err := s.products.OrderForAccess(orderNo)
	if err != nil {
		return nil, err
	}
	if o == nil || o.FulfillmentMode != compute.FulfillmentManaged {
		return nil, errcode.New(errcode.NotFound, "托管订单不存在")
	}
	l, err := s.repo.getLeaseByOrder(s.repo.db, o.ID)
	if err != nil {
		return nil, err
	}
	if l == nil {
		return nil, errcode.New(errcode.ParamInvalid, "订单无租约(续租单请使用原订单号)")
	}
	now := time.Now().Truncate(time.Second)
	if !remedyChangeable(l, now) {
		return nil, errcode.New(errcode.Conflict, "租约不在租期内, 不能发起违约退款")
	}
	// 进行中的停机在当前时刻结束并结算(独立事务, 与正常恢复同一路径)。
	if err := s.closeOutage(ctx, l.ID, now, "运营发起供给方违约退款"); err != nil {
		return nil, err
	}
	out := &BreachRefund{OrderNo: o.OrderNo, LeaseNo: l.LeaseNo}
	err = s.inTx(func(tx *sqlx.Tx) error {
		root, err := s.products.LockManagedOrderTx(tx, o.ID)
		if err != nil {
			return err
		}
		if root == nil {
			return errcode.New(errcode.NotFound, "托管订单不存在")
		}
		l, err := s.repo.lockLease(tx, l.ID)
		if err != nil {
			return err
		}
		if !remedyChangeable(l, now) || root.LeaseEnd == nil {
			return errcode.New(errcode.Conflict, "租约不在租期内, 不能发起违约退款")
		}
		var blocking int
		if err := tx.Get(&blocking, `SELECT COUNT(*) FROM order_renewals r JOIN orders c ON c.id=r.order_id
			WHERE r.parent_order_id=? AND (c.status NOT IN ('completed','cancelled','refunded')
				OR (r.applied_at IS NOT NULL AND c.status='completed' AND c.lease_start_at > ?))`, root.ID, now); err != nil {
			return err
		}
		if blocking > 0 {
			return errcode.New(errcode.Conflict, "存在待处理或已预付的后续续租, 请先处理续租单")
		}
		cover, err := s.coveringOrderID(tx, root.ID, now)
		if err != nil {
			return err
		}
		paid, term, err := orderTerm(tx, cover)
		if err != nil {
			return err
		}
		if term <= 0 {
			return errcode.New(errcode.Conflict, "订单租期无法计算, 需人工处理")
		}
		if ok, err := s.supplierBreached(tx, l.ID, cover, term, now); err != nil {
			return err
		} else if !ok {
			return errcode.Newf(errcode.Conflict, "未达到供给方违约条件(供给方责任停机累计 ≥ 租期的 %.0f%% 或单次 ≥ %d 小时)",
				s.cfg.Outage.RefundRatio*100, s.cfg.Outage.RefundSingleHours)
		}
		var coverNo string
		if err := tx.Get(&coverNo, `SELECT order_no FROM orders WHERE id=?`, cover); err != nil {
			return err
		}
		remaining := int64(l.AccessUntil.Sub(now) / time.Second)
		amount := int64(0)
		if remaining > 0 {
			amount = min(mulDiv(paid, remaining, term), paid)
		}
		left, err := s.breach.RefundableRemainder(tx, coverNo)
		if err != nil {
			return err
		}
		amount = min(amount, left)
		if amount <= 0 {
			return errcode.New(errcode.Conflict, "没有可退的未使用金额")
		}
		// 截断访问期限: 与停机顺延同一套守卫式更新(负时长)。
		if err := s.extendForOutageTx(tx, root, l, -l.AccessUntil.Sub(now)); err != nil {
			return err
		}
		id, err := s.breach.RegisterBreachRefundTx(tx, coverNo, l.ID, amount, "算力订单供给方违约退款: "+reason)
		if err != nil {
			return err
		}
		out.RefundOrderNo, out.RefundID, out.Amount, out.AccessUntil = coverNo, id, amount, now
		return s.repo.audit(tx, operatorID, "lease.supplier_breach_refund", "lease", l.ID,
			map[string]any{"access_until": l.AccessUntil},
			map[string]any{"access_until": now, "refund_order_no": coverNo, "amount": amount, "reason": reason}, ip)
	})
	if err != nil {
		return nil, err
	}
	if err := s.breach.SubmitPartialRefund(out.RefundID); err != nil {
		// 已登记、租期已截断; 提交失败由 payment 定时任务继续推进。
		slog.Warn("供给方违约退款已登记, 提交易宝失败将自动重试", "order_no", out.RefundOrderNo, "error", err)
	}
	return out, nil
}

// supplierBreached 当前所属订单内供给方责任停机是否达到违约门槛。
func (s *Service) supplierBreached(q sqlx.Queryer, leaseID, cover, term int64, now time.Time) (bool, error) {
	var rows []Outage
	if err := sqlx.Select(q, &rows, `SELECT `+outageColumns+` FROM lease_outages
		WHERE lease_id=? AND order_id=? AND liable='supplier'`, leaseID, cover); err != nil {
		return false, err
	}
	var total, longest int64
	for _, x := range rows {
		var secs int64
		switch {
		case x.EndedAt == nil:
			secs = int64(now.Sub(x.StartedAt) / time.Second)
		case x.Counted != nil && *x.Counted && x.Seconds != nil:
			secs = int64(*x.Seconds)
		}
		total += secs
		longest = max(longest, secs)
	}
	return float64(total) >= s.cfg.Outage.RefundRatio*float64(term) ||
		longest >= int64(s.cfg.Outage.RefundSingleHours)*3600, nil
}

// mulDiv floor(a × b ÷ c), 中间结果不溢出 int64。
func mulDiv(a, b, c int64) int64 {
	p := new(big.Int).Mul(big.NewInt(a), big.NewInt(b))
	return p.Quo(p, big.NewInt(c)).Int64()
}
