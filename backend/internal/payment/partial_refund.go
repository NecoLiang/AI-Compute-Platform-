package payment

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"

	"tokenfactory/internal/compute"
	"tokenfactory/pkg/errcode"
)

// ===== 部分退款(托管订单停机补偿 / 供给方违约, docs/25 §25.7.6, T-106) =====
//
// 资金在易宝待分账账户中(托管订单租期末才分账, D-10), 部分退款直接从待分账资金原路退回买家,
// 分账时按已成功的部分退款重算两行应结(见 adjustManagedSplitTx)。不变量: 部分退款合计 + 两行应结 = 实付。
//
// 状态: pending(已登记, 待清算入账后提交) → processing(已向易宝提交, 请求号已落库) → success / failed。
// failed 由定时任务换新请求号重试(1 小时退避); processing 结果不明时按请求号查询, 与全额退款同一套
// 同步结果 / 查询 / 异步通知 三路落地(applyPartialRefundResult 幂等)。
// payment_refunds UNIQUE(kind, source_id) 保证同一停机段 / 同一租约的违约退款至多一行, 重试不会重复退。

const (
	RefundKindOutage         = "outage_compensation"
	RefundKindSupplierBreach = "supplier_breach"

	partialRefundRetryAfter = time.Hour
	partialRefundBatch      = 100
)

// ErrPartialRefundDisabled 易宝部分退款能力未书面确认(fleet.outage.yeepay_partial_refund_confirmed=false)时不发起部分退款。
var ErrPartialRefundDisabled = errcode.New(errcode.InternalError, "易宝部分退款能力未确认, 暂只支持顺延补偿")

// PartialRefund payment_refunds 一行。
type PartialRefund struct {
	ID             int64     `db:"id" json:"id"`
	PaymentID      int64     `db:"payment_id" json:"payment_id"`
	OrderNo        string    `db:"order_no" json:"order_no"`
	RequestID      string    `db:"refund_request_id" json:"refund_request_id"`
	Amount         int64     `db:"amount" json:"amount"`
	Kind           string    `db:"kind" json:"kind"`
	SourceID       *int64    `db:"source_id" json:"source_id"`
	Reason         *string   `db:"reason" json:"reason"`
	Status         string    `db:"status" json:"status"`
	YeepayRefundNo *string   `db:"yeepay_refund_no" json:"yeepay_refund_no"`
	LastError      *string   `db:"last_error" json:"last_error"`
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time `db:"updated_at" json:"updated_at"`
}

const partialRefundColumns = `id, payment_id, order_no, refund_request_id, amount, kind, source_id, reason, status,
	yeepay_refund_no, last_error, created_at, updated_at`

// EnablePartialRefunds 打开部分退款通道。main 仅在 fleet.enabled 且 fleet.outage.yeepay_partial_refund_confirmed=true 时调用;
// 未打开时 RequestOutageRefund / RequestBreachRefund 返回 ErrPartialRefundDisabled, 定时任务也不提交新的部分退款
// (已在 processing 的仍会查询落地, 钱可能已经退出去了)。
func (s *Service) EnablePartialRefunds() { s.partialRefunds = true }

// PartialRefundsEnabled 部分退款通道是否已打开。
func (s *Service) PartialRefundsEnabled() bool { return s.partialRefunds }

// RequestOutageRefund 实现 fleet.OutageRefunder: 对 remedy='refund' 且 remedy_applied_at IS NULL 的已结束停机段,
// 按 remedy_amount 从其所属订单(lease_outages.order_id, 续租后为续租单)的支付原路部分退款。
// 成功落地时同事务写 lease_outages.remedy_applied_at。可重复调用(幂等)。
func (s *Service) RequestOutageRefund(_ context.Context, outageID int64) error {
	if !s.partialRefunds {
		return ErrPartialRefundDisabled
	}
	if !s.yeepay.Configured() {
		return ErrYeepayNotConfigured
	}
	var o struct {
		OrderNo   string     `db:"order_no"`
		Remedy    *string    `db:"remedy"`
		Amount    *int64     `db:"remedy_amount"`
		AppliedAt *time.Time `db:"remedy_applied_at"`
		EndedAt   *time.Time `db:"ended_at"`
	}
	err := s.db.Get(&o, `SELECT od.order_no, lo.remedy, lo.remedy_amount, lo.remedy_applied_at, lo.ended_at
		FROM lease_outages lo JOIN orders od ON od.id=lo.order_id WHERE lo.id=?`, outageID)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("停机段 %d 不存在", outageID)
	}
	if err != nil {
		return err
	}
	if o.Remedy == nil || *o.Remedy != "refund" || o.EndedAt == nil {
		return fmt.Errorf("停机段 %d 不是待退费的已结束停机", outageID)
	}
	if o.AppliedAt != nil {
		return nil
	}
	if o.Amount == nil {
		return fmt.Errorf("停机段 %d 责任金额未核定, 需运营人工处理", outageID)
	}
	if *o.Amount == 0 {
		// 应退 0 分(停机极短, 金额向下取整为 0): 没有可退的钱, 补偿即已完成。
		_, err := s.db.Exec(`UPDATE lease_outages SET remedy_applied_at=NOW()
			WHERE id=? AND remedy='refund' AND remedy_applied_at IS NULL AND remedy_amount=0`, outageID)
		return err
	}
	return s.requestPartialRefund(o.OrderNo, RefundKindOutage, outageID, *o.Amount, "算力订单停机补偿退费")
}

// RequestBreachRefund 供给方违约整单退款(未使用部分, 运营发起): 从 orderNo 的支付部分退款 amount, source_id = 租约 ID。
func (s *Service) RequestBreachRefund(orderNo string, leaseID, amount int64, reason string) error {
	if !s.partialRefunds {
		return ErrPartialRefundDisabled
	}
	if !s.yeepay.Configured() {
		return ErrYeepayNotConfigured
	}
	if amount <= 0 {
		return fmt.Errorf("违约退款金额无效")
	}
	return s.requestPartialRefund(orderNo, RefundKindSupplierBreach, leaseID, amount, reason)
}

// requestPartialRefund 登记(幂等)并提交一笔部分退款。登记校验: 托管订单、整条续租链未进入分账、支付未在全额退款、
// 无处理中/已成功的分账、本支付部分退款累计不超过实付。
func (s *Service) requestPartialRefund(orderNo, kind string, sourceID, amount int64, reason string) error {
	tx, err := s.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	order, parent, _, err := compute.NewRepository(s.db).LockOrderFamilyTx(tx, orderNo)
	if err != nil {
		return err
	}
	if order == nil {
		return fmt.Errorf("订单 %s 不存在", orderNo)
	}
	if err := partialRefundableOrder(order, parent); err != nil {
		return err
	}
	var row PartialRefund
	err = tx.Get(&row, `SELECT `+partialRefundColumns+` FROM payment_refunds WHERE kind=? AND source_id=? FOR UPDATE`, kind, sourceID)
	switch {
	case err == nil:
		if row.OrderNo != orderNo || row.Amount != amount {
			return fmt.Errorf("部分退款 %s 已登记的订单/金额(%s/%d)与本次(%s/%d)不一致, 需人工核查",
				row.RequestID, row.OrderNo, row.Amount, orderNo, amount)
		}
		if row.Status == "success" {
			if err := markRefundSourceAppliedTx(tx, &row); err != nil {
				return err
			}
			return tx.Commit()
		}
	case errors.Is(err, sql.ErrNoRows):
		pay, err := lockPartialRefundPaymentTx(tx, orderNo)
		if err != nil {
			return err
		}
		var committed int64
		if err := tx.Get(&committed, `SELECT COALESCE(SUM(amount),0) FROM payment_refunds WHERE payment_id=?`, pay.ID); err != nil {
			return err
		}
		if committed+amount > pay.Amount {
			return fmt.Errorf("订单 %s 部分退款累计 %d + 本次 %d 超过实付 %d", orderNo, committed, amount, pay.Amount)
		}
		res, err := tx.Exec(`INSERT INTO payment_refunds (payment_id, order_no, refund_request_id, amount, kind, source_id, reason, status)
			VALUES (?,?,?,?,?,?,?,'pending')`, pay.ID, orderNo, newRequestID("P", orderNo), amount, kind, sourceID, truncateRunes(reason, 250))
		if err != nil {
			return err
		}
		row.ID, _ = res.LastInsertId()
	default:
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.submitPartialRefund(row.ID)
}

// partialRefundableOrder 部分退款只用于托管订单, 且整条续租链尚未进入分账(根订单 settle_ready_at IS NULL)。
func partialRefundableOrder(order, parent *compute.Order) error {
	root := order
	if parent != nil {
		root = parent
	}
	if root.FulfillmentMode != compute.FulfillmentManaged {
		return fmt.Errorf("订单 %s 不是托管订单, 不支持部分退款", order.OrderNo)
	}
	if root.SettleReadyAt != nil {
		return fmt.Errorf("订单 %s 已进入分账, 不能再从待分账资金部分退款", order.OrderNo)
	}
	return nil
}

// lockPartialRefundPaymentTx 锁定订单的已收款支付及其应结行(先 payments 后 settlements), 校验可部分退款。
func lockPartialRefundPaymentTx(tx *sqlx.Tx, orderNo string) (*Payment, error) {
	var pay Payment
	if err := tx.Get(&pay, "SELECT * FROM payments WHERE order_no=? AND status='paid' ORDER BY id DESC LIMIT 1 FOR UPDATE", orderNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("订单 %s 无已收款支付", orderNo)
		}
		return nil, err
	}
	if pay.RefundStatus != nil {
		return nil, fmt.Errorf("订单 %s 已在全额退款流程(%s), 不再部分退款", orderNo, *pay.RefundStatus)
	}
	var busy int
	if err := tx.Get(&busy, `SELECT COUNT(*) FROM settlements WHERE order_no=? AND status IN ('processing','success') FOR UPDATE`, orderNo); err != nil {
		return nil, err
	}
	if busy > 0 {
		return nil, fmt.Errorf("订单 %s 已发起分账, 不能再部分退款", orderNo)
	}
	return &pay, nil
}

// submitPartialRefund 提交一笔 pending / failed 部分退款。支付未清算入账时保持 pending, 由定时任务在入账后提交。
func (s *Service) submitPartialRefund(id int64) error {
	var orderNo string
	if err := s.db.Get(&orderNo, `SELECT order_no FROM payment_refunds WHERE id=?`, id); err != nil {
		return err
	}
	tx, err := s.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	order, parent, _, err := compute.NewRepository(s.db).LockOrderFamilyTx(tx, orderNo)
	if err != nil {
		return err
	}
	if order == nil {
		return fmt.Errorf("订单 %s 不存在", orderNo)
	}
	var row PartialRefund
	if err := tx.Get(&row, `SELECT `+partialRefundColumns+` FROM payment_refunds WHERE id=? FOR UPDATE`, id); err != nil {
		return err
	}
	if row.Status != "pending" && row.Status != "failed" {
		return tx.Commit()
	}
	if err := partialRefundableOrder(order, parent); err != nil {
		return err
	}
	pay, err := lockPartialRefundPaymentTx(tx, orderNo)
	if err != nil {
		return err
	}
	if pay.ID != row.PaymentID {
		return fmt.Errorf("部分退款 %s 的支付记录已变化, 需人工核查", row.RequestID)
	}
	if pay.ClearedAt == nil {
		return tx.Commit() // 清算入账后由定时任务提交
	}
	requestID := row.RequestID
	if row.Status == "failed" {
		requestID = newRequestID("P", orderNo) // 失败是终态, 换新请求号重提
	}
	ok, err := affected(tx.Exec(`UPDATE payment_refunds SET status='processing', refund_request_id=?, last_error=NULL
		WHERE id=? AND status=?`, requestID, row.ID, row.Status))
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("部分退款 %s 状态已变化", row.RequestID)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	reason := "算力订单部分退款"
	if row.Reason != nil && *row.Reason != "" {
		reason = *row.Reason
	}
	result, err := s.yeepay.Refund(RefundOrder{OrderNo: orderNo, RequestID: requestID, Amount: row.Amount, Reason: reason})
	if err != nil {
		if bizCode(err) != "" {
			return s.applyPartialRefundResult(&RefundResult{OrderNo: orderNo, RequestID: requestID, Status: "FAILED", FailReason: err.Error()})
		}
		return err // 结果不明: 保持 processing, 定时任务按请求号查询
	}
	result.OrderNo, result.RequestID = orderNo, requestID
	return s.applyPartialRefundResult(result)
}

// isPartialRefundRequest 易宝退款通知/查询的请求号是否属于部分退款。
func (s *Service) isPartialRefundRequest(requestID string) (bool, error) {
	var n int
	err := s.db.Get(&n, `SELECT COUNT(*) FROM payment_refunds WHERE refund_request_id=?`, requestID)
	return n > 0, err
}

// applyPartialRefundResult 落地部分退款终态(同步结果/查询/异步通知共用, 幂等)。
func (s *Service) applyPartialRefundResult(r *RefundResult) error {
	if r.RequestID == "" {
		return fmt.Errorf("退款结果缺少请求号")
	}
	switch r.Status {
	case "SUCCESS":
	case "FAILED", "CANCEL":
		reason := r.FailReason
		if reason == "" {
			reason = "易宝退款失败(" + r.Status + ")"
		}
		ok, err := affected(s.db.Exec(`UPDATE payment_refunds SET status='failed', last_error=?
			WHERE refund_request_id=? AND status='processing'`, truncateRunes(reason, 500), r.RequestID))
		if err == nil && ok {
			slog.Error("运营告警: 部分退款失败, 1 小时后自动重试", "request_id", r.RequestID, "order_no", r.OrderNo, "reason", reason)
		}
		return err
	default:
		return nil // PROCESSING / SUSPEND: 等待终态
	}
	var orderNo string
	if err := s.db.Get(&orderNo, `SELECT order_no FROM payment_refunds WHERE refund_request_id=?`, r.RequestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errPaymentNotFound
		}
		return err
	}
	tx, err := s.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, _, _, err := compute.NewRepository(s.db).LockOrderFamilyTx(tx, orderNo); err != nil {
		return err
	}
	var row PartialRefund
	if err := tx.Get(&row, `SELECT `+partialRefundColumns+` FROM payment_refunds WHERE refund_request_id=? FOR UPDATE`, r.RequestID); err != nil {
		return err
	}
	if row.Status == "success" {
		return tx.Commit()
	}
	if r.Amount > 0 && r.Amount != row.Amount {
		return fmt.Errorf("易宝部分退款金额 %d 与登记金额 %d 不一致: request=%s", r.Amount, row.Amount, r.RequestID)
	}
	// 易宝说退成功了就是钱已退出: 无论本地是 processing 还是(先前误判的) failed, 一律记成功。
	if _, err := tx.Exec(`UPDATE payment_refunds SET status='success', yeepay_refund_no=?, last_error=NULL WHERE id=?`,
		nullIfEmpty(r.UniqueRefundNo), row.ID); err != nil {
		return err
	}
	row.Status = "success"
	if err := markRefundSourceAppliedTx(tx, &row); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	slog.Info("部分退款成功", "order_no", orderNo, "kind", row.Kind, "source_id", row.SourceID, "amount", row.Amount)
	return nil
}

// markRefundSourceAppliedTx 停机补偿退款成功后写 lease_outages.remedy_applied_at(守卫式, 重复调用无副作用)。
func markRefundSourceAppliedTx(tx *sqlx.Tx, row *PartialRefund) error {
	if row.Kind != RefundKindOutage || row.SourceID == nil {
		return nil
	}
	_, err := tx.Exec(`UPDATE lease_outages SET remedy_applied_at=NOW()
		WHERE id=? AND remedy='refund' AND remedy_applied_at IS NULL`, *row.SourceID)
	return err
}

// runPartialRefunds 定时推进: 通道打开时提交 pending(已入账)与退避期满的 failed; 无论通道是否打开都查询 processing。
func (s *Service) runPartialRefunds() error {
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if s.partialRefunds {
		var ids []int64
		err := s.db.Select(&ids, `SELECT pr.id FROM payment_refunds pr JOIN payments p ON p.id=pr.payment_id
			WHERE p.cleared_at IS NOT NULL AND p.status='paid' AND p.refund_status IS NULL
			AND (pr.status='pending' OR (pr.status='failed' AND pr.updated_at < ?)) ORDER BY pr.id LIMIT ?`,
			time.Now().Add(-partialRefundRetryAfter), partialRefundBatch)
		keep(err)
		for _, id := range ids {
			if err := s.submitPartialRefund(id); err != nil {
				slog.Warn("部分退款提交失败", "id", id, "error", err)
				keep(err)
			}
		}
	}
	var processing []struct {
		OrderNo   string `db:"order_no"`
		RequestID string `db:"refund_request_id"`
	}
	if err := s.db.Select(&processing, `SELECT order_no, refund_request_id FROM payment_refunds WHERE status='processing'
		ORDER BY id LIMIT ?`, partialRefundBatch); err != nil {
		return err
	}
	for _, row := range processing {
		result, err := s.yeepay.QueryRefund(row.OrderNo, row.RequestID)
		if err != nil {
			if bizCode(err) != "" {
				err = s.applyPartialRefundResult(&RefundResult{OrderNo: row.OrderNo, RequestID: row.RequestID, Status: "FAILED", FailReason: err.Error()})
			}
		} else {
			result.OrderNo, result.RequestID = row.OrderNo, row.RequestID
			err = s.applyPartialRefundResult(result)
		}
		keep(err)
	}
	return firstErr
}

// partialRefundTotals 某笔支付的部分退款: 已成功合计与未终结(pending/processing/failed)笔数。
func partialRefundTotals(q sqlx.Queryer, paymentID int64) (succeeded int64, open int, err error) {
	var r struct {
		Succeeded int64 `db:"succeeded"`
		Open      int   `db:"open"`
	}
	err = sqlx.Get(q, &r, `SELECT COALESCE(SUM(CASE WHEN status='success' THEN amount ELSE 0 END),0) AS succeeded,
		COALESCE(SUM(status<>'success'),0) AS open FROM payment_refunds WHERE payment_id=?`, paymentID)
	return r.Succeeded, r.Open, err
}

// ListPartialRefunds 运营查看部分退款(status 为空时全部), 按创建时间倒序。
func (s *Service) ListPartialRefunds(status, orderNo string, page, size int) ([]PartialRefund, int64, error) {
	where, args := []string{"1=1"}, []any{}
	if status != "" {
		if !containsString([]string{"pending", "processing", "success", "failed"}, status) {
			return nil, 0, errcode.New(errcode.ParamInvalid, "status 仅支持 pending/processing/success/failed")
		}
		where, args = append(where, "status=?"), append(args, status)
	}
	if orderNo != "" {
		where, args = append(where, "order_no=?"), append(args, orderNo)
	}
	cond := strings.Join(where, " AND ")
	var total int64
	if err := s.db.Get(&total, `SELECT COUNT(*) FROM payment_refunds WHERE `+cond, args...); err != nil {
		return nil, 0, err
	}
	list := []PartialRefund{}
	err := s.db.Select(&list, `SELECT `+partialRefundColumns+` FROM payment_refunds WHERE `+cond+`
		ORDER BY id DESC LIMIT ? OFFSET ?`, append(args, size, (page-1)*size)...)
	return list, total, err
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

func nullIfEmpty(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func affected(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n > 0, err
}
