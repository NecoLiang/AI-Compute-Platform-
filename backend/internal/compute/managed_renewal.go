package compute

// 托管订单续租的订单侧编排(docs/25 §25.7.2, S2 T-088)。租约侧动作经 LeaseHooks.RenewTx(Renew* 事件)在同一事务内完成:
//
//	报价/下单   renewalQuoteTx → RenewCheck / RenewReserve(restart 预留卡槽, 不调用 DecrProductStock)
//	收款校验    Service.ValidateRenewalPaymentTx → RenewVerify
//	收款落账    Service.ApplyRenewalPaymentTx → RenewApply(extend 顺延 access_until; restart 原订单 → paid 并自动重新开通)
//	关单/取消   releaseManagedRenewalTx → RenewRelease(restart 退回预留)
//	退款        refundRenewalTx → RenewRevert
//
// payment 通过 SetRenewalHooks 注入下面两个 Service 方法; 未注入时包级函数对托管原订单返回 50000(不静默放行)。

import (
	"github.com/jmoiron/sqlx"
)

func isManagedParent(parent *Order) bool {
	return parent != nil && parent.FulfillmentMode == FulfillmentManaged
}

// ValidateRenewalPaymentTx 收款前校验(payment 注入)。人工订单同包级函数; 托管原订单不校验 stock_reserved,
// 改由租约侧校验(extend 租约在租未到期; restart 预留卡槽完整)。
func (s *Service) ValidateRenewalPaymentTx(tx *sqlx.Tx, order, parent *Order, r *Renewal) error {
	if r == nil || !isManagedParent(parent) {
		return ValidateRenewalPaymentTx(tx, order, parent, r)
	}
	if s.leaseHooks.RenewTx == nil {
		return errManagedRenewNotWired
	}
	if err := validateRenewalBaseTx(tx, order, parent, r, true); err != nil {
		return err
	}
	return s.leaseHooks.RenewTx(tx, RenewVerify, parent, r)
}

// ApplyRenewalPaymentTx 收款落账(payment 注入), 与支付状态同事务提交。
func (s *Service) ApplyRenewalPaymentTx(tx *sqlx.Tx, order, parent *Order, r *Renewal) error {
	if r == nil || !isManagedParent(parent) {
		return ApplyRenewalPaymentTx(tx, order, parent, r)
	}
	if err := s.ValidateRenewalPaymentTx(tx, order, parent, r); err != nil {
		return err
	}
	if err := applyRenewalBaseTx(tx, order, parent, r); err != nil {
		return err
	}
	return s.leaseHooks.RenewTx(tx, RenewApply, parent, r)
}

// releaseManagedRenewalTx 未支付的托管 restart 续租单关闭/取消: 退回下单时预留的卡槽。extend 无租约侧占用。
func (s *Service) releaseManagedRenewalTx(tx *sqlx.Tx, parent *Order, r *Renewal) error {
	if !isManagedParent(parent) || r == nil || r.AppliedAt != nil || r.Mode != "restart" {
		return nil
	}
	if s.leaseHooks.RenewTx == nil {
		return errManagedRenewNotWired
	}
	return s.leaseHooks.RenewTx(tx, RenewRelease, parent, r)
}
