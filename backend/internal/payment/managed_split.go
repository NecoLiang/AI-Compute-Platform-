package payment

import (
	"fmt"
	"log/slog"
	"math/big"

	"github.com/jmoiron/sqlx"
)

// ===== 托管订单分账金额重算(T-106, docs/25 §25.7.6) =====
//
// 收款时两行应结按 平台费 / 实付-平台费 锁定(base)。租期末分账前按本支付已成功的部分退款重算:
//   - 供给方责任(停机 liable=supplier 与供给方违约退款) S: 先扣供给方应结, 平台费按同比例扣减
//     平台扣 floor(S × 平台base ÷ 实付), 其余扣供给方(供给方承担取整余数, 平台不多扣)。
//   - 平台责任(停机 liable=platform) P: 只扣平台费; 平台费不足的部分 overflow 扣供给方应结,
//     记在供给方行 platform_compensation, 由平台另行补偿给供给方(记账, 不走二清)。
// 不变量: 部分退款合计 + 两行 amount = 实付(trySplit 发起分账前校验, 不成立则报错不分账)。

type managedSplit struct {
	Platform, Supplier             int64 // 重算后应结
	PlatformDeduct, SupplierDeduct int64
	Compensation                   int64 // 平台另行补偿供给方
}

// computeManagedSplit paid=实付, platBase/supBase=收款时锁定的两行应结(和为实付), supplierLiable/platformLiable=已成功部分退款按责任方合计。
func computeManagedSplit(paid, platBase, supBase, supplierLiable, platformLiable int64) (managedSplit, error) {
	if paid <= 0 || platBase < 0 || supBase < 0 || platBase+supBase != paid {
		return managedSplit{}, fmt.Errorf("应结基数 %d+%d 与实付 %d 不一致", platBase, supBase, paid)
	}
	if supplierLiable < 0 || platformLiable < 0 || supplierLiable+platformLiable > paid {
		return managedSplit{}, fmt.Errorf("部分退款合计 %d+%d 超过实付 %d", supplierLiable, platformLiable, paid)
	}
	prod := new(big.Int).Mul(big.NewInt(supplierLiable), big.NewInt(platBase))
	platDed := prod.Quo(prod, big.NewInt(paid)).Int64()
	supDed := supplierLiable - platDed
	fromPlat := min(platformLiable, platBase-platDed)
	overflow := platformLiable - fromPlat
	platDed += fromPlat
	supDed += overflow
	return managedSplit{
		Platform: platBase - platDed, Supplier: supBase - supDed,
		PlatformDeduct: platDed, SupplierDeduct: supDed, Compensation: overflow,
	}, nil
}

// adjustManagedSplitTx 在 trySplit 事务内(已锁 订单族→payments→settlements)按已成功的部分退款重算托管订单两行应结。
// 返回 ok=false 表示有未终结的部分退款, 本轮不分账。rows 原地更新为重算结果。
func adjustManagedSplitTx(tx *sqlx.Tx, pay *Payment, rows []Settlement) (bool, error) {
	var refunds []struct {
		Amount int64   `db:"amount"`
		Kind   string  `db:"kind"`
		Status string  `db:"status"`
		Liable *string `db:"liable"`
	}
	if err := tx.Select(&refunds, `SELECT pr.amount, pr.kind, pr.status, lo.liable FROM payment_refunds pr
		LEFT JOIN lease_outages lo ON pr.kind='outage_compensation' AND lo.id=pr.source_id
		WHERE pr.payment_id=? FOR UPDATE`, pay.ID); err != nil {
		return false, err
	}
	var supLiable, platLiable int64
	for _, r := range refunds {
		if r.Status != "success" {
			return false, nil // docs/25: 有 pending/processing 部分退款时跳过分账(failed 待重试, 同样等待)
		}
		switch {
		case r.Kind == RefundKindOutage && r.Liable != nil && *r.Liable == "platform":
			platLiable += r.Amount
		case r.Kind == RefundKindOutage && r.Liable == nil:
			return false, fmt.Errorf("订单 %s 停机补偿退款缺少停机记录, 需人工核查", pay.OrderNo)
		default: // 供给方责任停机 + 供给方违约退款
			supLiable += r.Amount
		}
	}
	var plat, sup *Settlement
	for i := range rows {
		switch rows[i].PayeeType {
		case "platform":
			plat = &rows[i]
		case "supplier":
			sup = &rows[i]
		}
	}
	if plat == nil || sup == nil || len(rows) != 2 {
		return false, fmt.Errorf("订单 %s 应结记录不完整", pay.OrderNo)
	}
	m, err := computeManagedSplit(pay.Amount, plat.Amount+plat.RefundDeduction, sup.Amount+sup.RefundDeduction, supLiable, platLiable)
	if err != nil {
		return false, fmt.Errorf("订单 %s %w", pay.OrderNo, err)
	}
	if plat.Amount != m.Platform || plat.RefundDeduction != m.PlatformDeduct {
		if _, err := tx.Exec(`UPDATE settlements SET amount=?, refund_deduction=? WHERE id=? AND status IN ('pending','failed')`,
			m.Platform, m.PlatformDeduct, plat.ID); err != nil {
			return false, err
		}
		plat.Amount, plat.RefundDeduction = m.Platform, m.PlatformDeduct
	}
	if sup.Amount != m.Supplier || sup.RefundDeduction != m.SupplierDeduct || sup.PlatformCompensation != m.Compensation {
		if _, err := tx.Exec(`UPDATE settlements SET amount=?, refund_deduction=?, platform_compensation=? WHERE id=? AND status IN ('pending','failed')`,
			m.Supplier, m.SupplierDeduct, m.Compensation, sup.ID); err != nil {
			return false, err
		}
		sup.Amount, sup.RefundDeduction, sup.PlatformCompensation = m.Supplier, m.SupplierDeduct, m.Compensation
		if m.Compensation > 0 {
			slog.Warn("运营提示: 平台责任退款超出平台费, 需平台另行补偿供给方(记账)",
				"order_no", pay.OrderNo, "supplier_id", sup.PayeeID, "amount", m.Compensation)
		}
	}
	if plat.Amount+sup.Amount+supLiable+platLiable != pay.Amount {
		return false, fmt.Errorf("订单 %s 部分退款 + 应结 ≠ 实付", pay.OrderNo)
	}
	return true, nil
}
