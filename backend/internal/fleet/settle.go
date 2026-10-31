package fleet

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jmoiron/sqlx"
)

// ===== 租期末结算(settle_ready_at, D-10 / docs/25 §25.7.6, T-106) =====
//
// 托管订单整条续租链(根订单 + 续租单)在租期末一次分账。根订单写 settle_ready_at 的条件:
//   1. 根订单 completed, 且所有租约已进入 purging/purged/failed/cancelled(后两者见清除后 restart 续租失败)。不在 grace 写:
//      宽限期内的 restart 续租会把根订单重新打开(completed → active), 而 purged 后的续租在
//      settle_ready_at 写入后被拒绝(compute.renewalQuoteTx), 两者在根订单行锁下互斥。
//   2. 续租单全部 completed/cancelled/refunded(无待支付/退款中的续租)。
//   3. 租约的每段停机都已结束且补偿已落地(ended_at、remedy_applied_at 均非空)。
//   4. 链上订单没有未成功的部分退款(已全额退款的支付除外)。
// 写入用守卫式 UPDATE(settle_ready_at IS NULL AND status='completed'), 续租单沿用根订单的值
// (payment.managedSettleBlocked 看母单), 之后由 payment 定时任务照常分账。

const settleBatch = 200

// SettleRound 推进待退费停机的退款提交, 并为满足条件的托管订单写 settle_ready_at。
func (s *Service) SettleRound(ctx context.Context) {
	if s.refundAvailable() {
		s.retryOutageRefunds(ctx)
	}
	var ids []int64
	err := s.repo.db.Select(&ids, `SELECT o.id FROM orders o
		WHERE o.fulfillment_mode='managed' AND o.status='completed' AND o.settle_ready_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM order_renewals r WHERE r.order_id=o.id)
		AND EXISTS (SELECT 1 FROM leases l WHERE l.order_id=o.id)
		AND NOT EXISTS (SELECT 1 FROM leases l WHERE l.order_id=o.id AND l.state NOT IN ('purging','purged','failed','cancelled'))
		ORDER BY o.id LIMIT ?`, settleBatch)
	if err != nil {
		slog.Warn("租期末结算候选查询失败", "error", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		blocked, err := s.markSettleReady(id)
		if err != nil {
			slog.Warn("写入租期末结算失败", "order_id", id, "error", err)
			continue
		}
		if blocked != "" && s.alertOnce(ctx, fmt.Sprintf("fleet:settle_blocked:%d:%s", id, blocked)) {
			slog.Error("运营告警: 托管订单租期已结束但未能进入分账, 资金继续留在易宝待分账账户", "order_id", id, "reason", blocked)
		}
	}
}

// markSettleReady 单个根订单: 锁订单 → 租约 → 停机段, 校验后守卫式写 settle_ready_at。
// 返回 blocked 非空表示需运营关注的阻塞原因(正常等待返回空)。
func (s *Service) markSettleReady(orderID int64) (blocked string, err error) {
	err = s.inTx(func(tx *sqlx.Tx) error {
		o, err := s.products.LockManagedOrderTx(tx, orderID)
		if err != nil || o == nil {
			return err
		}
		if o.Status != "completed" || o.SettleReadyAt != nil {
			return nil
		}
		var states []string
		if err := tx.Select(&states, `SELECT state FROM leases WHERE order_id=? ORDER BY id FOR UPDATE`, orderID); err != nil {
			return err
		}
		// 未交付的托管订单开通失败/取消时订单落 cancelled(compute.FailManagedOrderTx), 不会是 completed;
		// completed 且租约 failed/cancelled 只出现在清除后 restart 续租开通失败/取消: 原租期已完整交付, 照常分账。
		if len(states) == 0 {
			return nil
		}
		for _, st := range states {
			switch st {
			case LeasePurging, LeasePurged, LeaseFailed, LeaseCancelled:
			default:
				return nil
			}
		}
		var openChildren int
		if err := tx.Get(&openChildren, `SELECT COUNT(*) FROM order_renewals r JOIN orders c ON c.id=r.order_id
			WHERE r.parent_order_id=? AND c.status NOT IN ('completed','cancelled','refunded')`, orderID); err != nil {
			return err
		}
		if openChildren > 0 {
			blocked = "renewal_open"
			return nil
		}
		var outages struct {
			Open          int `db:"open_n"`
			RefundPending int `db:"refund_n"`
		}
		if err := tx.Get(&outages, `SELECT COALESCE(SUM(lo.ended_at IS NULL),0) AS open_n,
			COALESCE(SUM(lo.ended_at IS NOT NULL AND lo.remedy_applied_at IS NULL),0) AS refund_n
			FROM lease_outages lo JOIN leases l ON l.id=lo.lease_id WHERE l.order_id=?`, orderID); err != nil {
			return err
		}
		if outages.Open > 0 {
			blocked = "outage_open"
			return nil
		}
		if outages.RefundPending > 0 {
			if !s.refundAvailable() {
				blocked = "outage_refund_unavailable"
			}
			return nil
		}
		var openRefunds int
		if err := tx.Get(&openRefunds, `SELECT COUNT(*) FROM payment_refunds pr
			JOIN payments p ON p.id=pr.payment_id AND p.status<>'refunded'
			JOIN orders c ON c.order_no=pr.order_no
			WHERE pr.status<>'success' AND (c.id=? OR c.id IN (SELECT order_id FROM order_renewals WHERE parent_order_id=?))`,
			orderID, orderID); err != nil {
			return err
		}
		if openRefunds > 0 {
			return nil
		}
		ok, err := affected(tx.Exec(`UPDATE orders SET settle_ready_at=NOW()
			WHERE id=? AND settle_ready_at IS NULL AND status='completed' AND fulfillment_mode='managed'`, orderID))
		if err != nil {
			return err
		}
		if ok {
			slog.Info("托管订单租期末结算就绪, 进入分账", "order_no", o.OrderNo)
		}
		return nil
	})
	return blocked, err
}

// retryOutageRefunds 已结束、待退费、尚未登记退款的停机段交给退费实现(afterSettle 调用失败或通道后开时的兜底)。
// 已登记的由 payment 定时任务推进, 不在此重复提交。
func (s *Service) retryOutageRefunds(ctx context.Context) {
	var ids []int64
	if err := s.repo.db.Select(&ids, `SELECT lo.id FROM lease_outages lo
		WHERE lo.remedy='refund' AND lo.remedy_applied_at IS NULL AND lo.ended_at IS NOT NULL AND lo.remedy_amount IS NOT NULL
		AND NOT EXISTS (SELECT 1 FROM payment_refunds pr WHERE pr.kind='outage_compensation' AND pr.source_id=lo.id)
		ORDER BY lo.id LIMIT ?`, settleBatch); err != nil {
		slog.Warn("待退费停机查询失败", "error", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if err := s.refunder.RequestOutageRefund(ctx, id); err != nil {
			slog.Warn("停机补偿退费登记失败, 下一轮重试", "outage_id", id, "error", err)
		}
	}
}

// alertOnce 运营告警去重(Redis 可用时同一 key 在 TTL 内只告警一次; 未注入 Redis 时每轮都告警)。
func (s *Service) alertOnce(ctx context.Context, key string) bool {
	if s.rdb == nil {
		return true
	}
	return s.firstSend(ctx, key)
}
