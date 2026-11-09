package compute

// 托管订单解冻(docs/25 §25.7.3, S2 T-088)。
//
// 冻结走既有路径(FreezeOrderTx: 运营 PATCH status=frozen / 风控告警冻结), 托管订单在同一事务内经
// LeaseHooks.FreezeTx 把在租租约转 suspended 并断开会话。解冻是运营核查结案的动作, 只对托管订单开放:
// 订单回到哪个状态由租约实况决定(ManagedUnfreeze), 而不是简单恢复冻结前状态 —— 冻结期间租期可能已到,
// 开通可能被放弃。人工订单沿用原口径(冻结后只能关闭/退款), 不在本接口范围。

import (
	"fmt"
	"log/slog"
	"time"

	"tokenfactory/pkg/errcode"
)

// UnfreezeResult 解冻结果(POST /admin/orders/:id/unfreeze 的 data)。
type UnfreezeResult struct {
	OrderNo     string `json:"order_no"`
	Status      string `json:"status"`
	WrongFreeze bool   `json:"wrong_freeze"`
}

// AdminUnfreezeOrder 运营解冻托管订单。wrongFreeze=true 表示核查结论为误冻结: 提交后把租约冻结时长
// 交给停机认定(LeaseHooks.WrongFreeze, T-105)计入平台责任停机。
func (s *Service) AdminUnfreezeOrder(operatorID int64, orderNo string, wrongFreeze bool, ip string) (*UnfreezeResult, error) {
	if operatorID <= 0 {
		return nil, errcode.New(errcode.Forbidden, "无权操作订单")
	}
	if s.leaseHooks.UnfreezeTx == nil {
		return nil, errManagedLeaseNotWired
	}
	tx, err := s.beginManagedAware(true)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	order, _, renewal, err := s.repo.LockOrderFamilyTx(tx, orderNo)
	if err != nil {
		return nil, err
	}
	switch {
	case order == nil:
		return nil, errcode.New(errcode.NotFound, "订单不存在")
	case renewal != nil:
		return nil, errcode.New(errcode.ParamInvalid, "续租单不支持单独解冻, 请处理其原订单")
	case order.FulfillmentMode != FulfillmentManaged:
		return nil, errcode.New(errcode.ParamInvalid, "仅托管订单支持解冻; 人工订单冻结后请关闭或退款")
	case order.Status != "frozen":
		return nil, errcode.Newf(errcode.Conflict, "订单当前状态为 %s, 不是冻结状态", order.Status)
	}
	plan, err := s.leaseHooks.UnfreezeTx(tx, order)
	if err != nil {
		return nil, err
	}
	moved, err := s.repo.TransitionOrderStatusTx(tx, orderNo, []string{"frozen"}, plan.OrderStatus)
	if err != nil {
		return nil, err
	}
	if !moved {
		return nil, errcode.New(errcode.Conflict, "订单状态已变化, 请刷新后重试")
	}
	switch plan.OrderStatus {
	case "active":
		// 冻结时吊销的访问凭证恢复(凭证值不变, 有效期仍为原 access_expires_at)。
		if _, err := tx.Exec(`UPDATE order_deliveries SET access_status='delivered', revoked_at=NULL
			WHERE order_id=? AND access_status='revoked'`, order.ID); err != nil {
			return nil, err
		}
	case "completed":
		// 冻结期间租期已到: 与到期流转同一释放路径(租约 suspended → grace, 宽限计时自解冻起算)。
		if err := s.releaseHoldTx(tx, order, "completed"); err != nil {
			return nil, err
		}
	case "pending_payment", "paid":
	default:
		return nil, fmt.Errorf("解冻目标状态非法: %s", plan.OrderStatus)
	}
	if _, err := tx.Exec(`INSERT INTO audit_logs
		(operator_id,action,target_type,target_id,before_value,after_value,ip)
		VALUES (?,'unfreeze_order','order',?,?,?,?)`, operatorID, order.ID, "frozen",
		fmt.Sprintf(`{"status":%q,"wrong_freeze":%t}`, plan.OrderStatus, wrongFreeze), ip); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	now := time.Now()
	if wrongFreeze {
		switch {
		case plan.SuspendedAt == nil:
			slog.Info("误冻结解冻: 冻结时租约尚未在租, 无停机时长", "order_no", orderNo)
		case s.leaseHooks.WrongFreeze == nil:
			slog.Warn("误冻结解冻: 停机认定未接入, 仅记录", "order_no", orderNo, "frozen_at", plan.SuspendedAt, "unfrozen_at", now)
		default:
			s.leaseHooks.WrongFreeze(plan.LeaseID, *plan.SuspendedAt, now)
		}
	}
	s.notifyOrderEvent(orderNo, "订单已解冻", unfreezeNotice(orderNo, plan.OrderStatus))
	return &UnfreezeResult{OrderNo: orderNo, Status: plan.OrderStatus, WrongFreeze: wrongFreeze}, nil
}

func unfreezeNotice(orderNo, status string) string {
	switch status {
	case "active":
		return fmt.Sprintf("您的订单 %s 已解冻, 访问已恢复。", orderNo)
	case "paid":
		return fmt.Sprintf("您的订单 %s 已解冻, 平台正在为您开通算力。", orderNo)
	case "completed":
		return fmt.Sprintf("您的订单 %s 已解冻; 冻结期间租期已结束, 数据在宽限期内保留, 可续租恢复。", orderNo)
	default:
		return fmt.Sprintf("您的订单 %s 已解冻, 请在支付期限内完成付款。", orderNo)
	}
}
