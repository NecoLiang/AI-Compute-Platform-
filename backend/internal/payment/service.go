package payment

import (
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
	"tokenfactory/internal/compute"
)

type Service struct {
	repo       *Repository
	db         *sqlx.DB
	yeepay     Gateway
	onRefunded func(orderNo string) error
}

// Gateway is the external payment boundary (易宝). Every result that moves
// order state must come from a signed response or a decrypted, verified notify.
type Gateway interface {
	Configured() bool
	PayChannels() []string
	CreatePayment(PaymentOrder) (*Checkout, error)
	QueryPayment(orderNo string) (*PaymentResult, error)
	Divide(DivideOrder) (*DivideResult, error)
	QueryDivide(orderNo, requestID string) (*DivideResult, error)
	Refund(RefundOrder) (*RefundResult, error)
	QueryRefund(orderNo, requestID string) (*RefundResult, error)
	NetInURL(requestNo string) (string, error)
	QueryNetIn(requestNo string) (*NetInResult, error)
	DecryptNotify(raw string) ([]byte, error)
}

func NewService(repo *Repository, db *sqlx.DB, gateway ...Gateway) *Service {
	var client Gateway = &YeepayClient{}
	if len(gateway) > 0 && gateway[0] != nil {
		client = gateway[0]
	}
	return &Service{repo: repo, db: db, yeepay: client}
}

// SetRefundHook 退款成功后回调订单域(释放余量、吊销凭证、通知买家)。
func (s *Service) SetRefundHook(fn func(orderNo string) error) { s.onRefunded = fn }

type PayReq struct {
	OrderNo string `json:"order_no"`
	Channel string `json:"channel"` // wechat/alipay/bank
}

type PayResp struct {
	PayURL string `json:"pay_url"`
	TxID   string `json:"tx_id"`
}

// goodsName 易宝收银台展示的商品名。
func goodsName(gpuModel string, cards, machines int) string {
	name := strings.TrimSpace("算力租赁 " + gpuModel)
	switch {
	case cards > 0:
		return fmt.Sprintf("%s x%d卡", name, cards)
	case machines > 0:
		return fmt.Sprintf("%s x%d台", name, machines)
	}
	return name
}

// PayChannels 买家下单时可选的支付方式(与易宝已开通产品一致)。
func (s *Service) PayChannels() []string { return s.yeepay.PayChannels() }

// T-027: Create payment order
func (s *Service) Pay(buyerID int64, req PayReq) (*PayResp, error) {
	if err := compute.NewRepository(s.db).RequireTradingAccess(buyerID, "buyer"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.OrderNo) == "" || limitPayTypes[req.Channel] == "" {
		return nil, fmt.Errorf("请选择有效订单和支付方式")
	}
	tx, err := s.db.Beginx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	renewalOrder, parent, renewal, err := compute.NewRepository(s.db).LockOrderFamilyTx(tx, req.OrderNo)
	if err != nil {
		return nil, err
	}
	// 买断/算力中心商品没有卡数, 空心机房没有型号: 这些列可能为 NULL。
	var order struct {
		payableOrder
		GPUModel     string `db:"gpu_model"`
		CardCount    int    `db:"card_count"`
		MachineCount int    `db:"machine_count"`
	}
	if err := tx.Get(&order, `SELECT o.buyer_id,o.total_amount,o.status,o.payment_expires_at,
		COALESCE(p.gpu_model,'') AS gpu_model,COALESCE(p.card_count,0) AS card_count,COALESCE(p.machine_count,0) AS machine_count
		FROM orders o JOIN products p ON p.id=o.product_id WHERE o.order_no=? FOR UPDATE`, req.OrderNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("order not found")
		}
		return nil, err
	}
	if order.BuyerID != buyerID {
		return nil, fmt.Errorf("无权支付其他买家的订单")
	}
	if order.Status != "pending_payment" || order.ExpiresAt == nil || !order.ExpiresAt.After(time.Now()) {
		return nil, ErrPaymentConflict
	}
	if order.Amount <= 0 {
		return nil, fmt.Errorf("订单金额无效")
	}
	if s.yeepay.Configured() && !slices.Contains(s.yeepay.PayChannels(), req.Channel) {
		return nil, fmt.Errorf("该支付方式暂未开通, 请选择其他支付方式")
	}
	if err := compute.ValidateRenewalPaymentTx(tx, renewalOrder, parent, renewal); err != nil {
		return nil, err
	}
	var paid int
	if err := tx.Get(&paid, "SELECT COUNT(*) FROM payments WHERE order_no=? AND status IN ('paid','refunded')", req.OrderNo); err != nil {
		return nil, err
	}
	if paid > 0 {
		return nil, ErrPaymentConflict
	}
	var existing Payment
	err = tx.Get(&existing, "SELECT * FROM payments WHERE order_no=? AND status='pending' ORDER BY id DESC LIMIT 1", req.OrderNo)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if err == nil && existing.PayURL != nil && *existing.PayURL != "" {
		if existing.Amount != order.Amount {
			return nil, ErrPaymentConflict
		}
		if existing.Channel != req.Channel {
			return nil, fmt.Errorf("该订单已创建支付，请使用原支付方式继续")
		}
		return &PayResp{PayURL: *existing.PayURL, TxID: existing.YeepayTxID}, nil
	}
	checkout, err := s.yeepay.CreatePayment(PaymentOrder{
		OrderNo: req.OrderNo, Amount: order.Amount, Channel: req.Channel, ExpiresAt: *order.ExpiresAt,
		GoodsName: goodsName(order.GPUModel, order.CardCount, order.MachineCount),
	})
	if err != nil {
		return nil, err
	}
	cashier, urlErr := url.Parse(checkout.PayURL)
	if urlErr != nil || cashier.Scheme != "https" || cashier.Host == "" || cashier.User != nil || checkout.UniqueOrderNo == "" {
		return nil, fmt.Errorf("payment provider returned an invalid checkout")
	}
	payURL, txID := checkout.PayURL, checkout.UniqueOrderNo
	if existing.ID != 0 {
		if existing.YeepayTxID != txID || existing.Amount != order.Amount || existing.Channel != req.Channel {
			return nil, ErrPaymentConflict
		}
		if _, err := tx.Exec("UPDATE payments SET pay_url=? WHERE id=?", payURL, existing.ID); err != nil {
			return nil, err
		}
	} else if _, err := tx.Exec(`INSERT INTO payments (order_no,amount,channel,yeepay_tx_id,pay_url,status) VALUES (?,?,?,?,?,'pending')`, req.OrderNo, order.Amount, req.Channel, txID, payURL); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return &PayResp{PayURL: payURL, TxID: txID}, nil
}

type payableOrder struct {
	BuyerID   int64      `db:"buyer_id"`
	Amount    int64      `db:"total_amount"`
	Status    string     `db:"status"`
	ExpiresAt *time.Time `db:"payment_expires_at"`
}

var (
	ErrPaymentConflict   = errors.New("订单已支付、已关闭或超过支付期限，请刷新订单")
	ErrRefundNotAllowed  = errors.New("该订单当前不可退款")
	ErrUnknownNotify     = errors.New("未知的易宝通知类型")
	errPaymentNotFound   = errors.New("易宝通知对应的支付记录不存在")
	errPaymentNotPaidYet = errors.New("支付尚未确认, 等待支付通知")
)

// ===== 易宝异步通知 =====
//
// 🔴 安全铁律: 通知接口无鉴权(易宝服务器直连调用), 解密验签是唯一信任来源, 必须先于任何
// 业务处理执行。未接入易宝时没有密钥可用, 一律拒绝 —— 否则任何人知道 order_no 就能把订单
// 刷成已支付并触发分账。
//
// 返回 nil 表示已处理(含幂等重放), handler 回 SUCCESS; 返回错误时易宝会按策略重试。

const (
	NotifyPay    = "pay"
	NotifyClear  = "clear"
	NotifyRefund = "refund"
	NotifyNetIn  = "netin"
)

func (s *Service) HandleNotify(kind, raw string) error {
	content, err := s.yeepay.DecryptNotify(raw)
	if err != nil {
		return err
	}
	switch kind {
	case NotifyPay:
		result, err := parsePayNotify(content)
		if err != nil {
			return err
		}
		return s.confirmPayment(result)
	case NotifyClear:
		result, err := parseClearNotify(content)
		if err != nil {
			return err
		}
		return s.markCleared(result)
	case NotifyRefund:
		result, err := parseRefundNotify(content)
		if err != nil {
			return err
		}
		return s.applyRefundResult(result)
	case NotifyNetIn:
		result, err := parseNetInNotify(content)
		if err != nil {
			return err
		}
		return s.applyNetIn(result)
	}
	return ErrUnknownNotify
}

// confirmPayment 确认一笔易宝支付成功。钱已经进了易宝账户, 因此即使订单已关闭/续租已失效,
// 也必须把这笔支付记为已收款并排队原路退回, 绝不能拒收后不了了之。
func (s *Service) confirmPayment(r *PaymentResult) error {
	if r.Status != "SUCCESS" {
		// 未支付/超时/关闭: 订单状态由本地支付期限驱动, 这里只确认收到。
		return nil
	}
	if r.OrderNo == "" || r.UniqueOrderNo == "" || r.Amount <= 0 {
		return fmt.Errorf("易宝支付结果缺少订单号或金额")
	}
	tx, err := s.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	renewalOrder, parent, renewal, err := compute.NewRepository(s.db).LockOrderFamilyTx(tx, r.OrderNo)
	if err != nil {
		return err
	}
	if renewalOrder == nil {
		return fmt.Errorf("order not found")
	}
	var order struct {
		payableOrder
		SupplierID int64 `db:"supplier_id"`
		Fee        int64 `db:"platform_fee"`
	}
	if err := tx.Get(&order, `SELECT o.buyer_id,o.total_amount,o.platform_fee,o.status,o.payment_expires_at,p.supplier_id
		FROM orders o JOIN products p ON p.id=o.product_id WHERE o.order_no=? FOR UPDATE`, r.OrderNo); err != nil {
		return err
	}
	var record Payment
	if err := tx.Get(&record, "SELECT * FROM payments WHERE yeepay_tx_id=? FOR UPDATE", r.UniqueOrderNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errPaymentNotFound
		}
		return err
	}
	if record.OrderNo != r.OrderNo || record.Amount != r.Amount || order.Amount != r.Amount {
		return fmt.Errorf("易宝支付结果与本地订单/支付金额不一致: order=%s", r.OrderNo)
	}
	if record.Status == "paid" || record.Status == "refunded" {
		return tx.Commit()
	}
	paidAt := time.Now()
	if r.PaidAt != nil {
		paidAt = *r.PaidAt
	}
	payable := order.Status == "pending_payment"
	if payable {
		switch err := compute.ValidateRenewalPaymentTx(tx, renewalOrder, parent, renewal); {
		case errors.Is(err, compute.ErrRenewalConflict):
			payable = false
		case err != nil:
			return err
		}
	}
	if !payable {
		if _, err := tx.Exec(`UPDATE payments SET status='paid',paid_at=?,refund_status='pending',refund_reason=? WHERE id=?`,
			paidAt, "订单已关闭或已失效, 款项原路退回", record.ID); err != nil {
			return err
		}
		slog.Warn("收到已关闭订单的支付, 已排队原路退款", "order_no", r.OrderNo, "order_status", order.Status)
		return tx.Commit()
	}
	if order.Fee < 0 || order.Fee > order.Amount {
		return fmt.Errorf("invalid stored platform fee")
	}
	if _, err := tx.Exec("UPDATE payments SET status='paid',paid_at=? WHERE id=?", paidAt, record.ID); err != nil {
		return err
	}
	if _, err := tx.Exec("UPDATE orders SET status='paid' WHERE order_no=?", r.OrderNo); err != nil {
		return err
	}
	if err := compute.ApplyRenewalPaymentTx(tx, renewalOrder, parent, renewal); err != nil {
		return err
	}
	// 应结记录在收款时按下单时锁定的佣金生成; 实际分账在清算入账且交付确认后发起。
	for _, item := range []struct {
		payee  string
		id     int64
		amount int64
	}{{"platform", 0, order.Fee}, {"supplier", order.SupplierID, order.Amount - order.Fee}} {
		if _, err := tx.Exec(`INSERT INTO settlements (settlement_id,order_no,payee_type,payee_id,amount,status)
			VALUES (?,?,?,?,?,'pending')`, "SET-"+r.OrderNo+"-"+item.payee, r.OrderNo, item.payee, item.id, item.amount); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// markCleared 记录清算入账(之后才能分账/从待分账账户退款), 并尝试分账。
func (s *Service) markCleared(r *PaymentResult) error {
	if r.UniqueOrderNo == "" || !r.Cleared {
		return nil
	}
	at := time.Now()
	if r.ClearedAt != nil {
		at = *r.ClearedAt
	}
	res, err := s.db.Exec(`UPDATE payments SET cleared_at=COALESCE(cleared_at,?) WHERE yeepay_tx_id=? AND status IN ('paid','refunded')`, at, r.UniqueOrderNo)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var status string
		err := s.db.Get(&status, "SELECT status FROM payments WHERE yeepay_tx_id=?", r.UniqueOrderNo)
		if errors.Is(err, sql.ErrNoRows) {
			return errPaymentNotFound
		}
		if err != nil {
			return err
		}
		if status == "pending" {
			return errPaymentNotPaidYet
		}
	}
	var orderNo string
	if err := s.db.Get(&orderNo, "SELECT order_no FROM payments WHERE yeepay_tx_id=?", r.UniqueOrderNo); err != nil {
		return err
	}
	if err := s.trySplit(orderNo); err != nil {
		slog.Error("清算后分账失败, 定时任务会重试", "order_no", orderNo, "error", err)
	}
	return nil
}

// ===== 分账 =====

// splitEligibleSQL 分账时机: 清算入账 + 买家已确认交付(订单 active/completed)。
// 交付前资金留在易宝待分账账户, 交付前退款直接从待分账账户原路退回, 无需向供给方追款。
// 续租订单还要求其租约母单已在使用中(重新开通的续租需等新的交付确认)。
func splitEligible(orderStatus string, parentStatus *string) bool {
	if orderStatus != "active" && orderStatus != "completed" {
		return false
	}
	return parentStatus == nil || *parentStatus == "active" || *parentStatus == "completed"
}

// trySplit 对一笔已清算、未退款、交付已确认的订单发起分账: 供给方应得部分分到其易宝商编,
// 剩余部分(平台服务费)随分账解冻到平台收款商编。请求号先落库再请求易宝, 结果不明时按请求号查询。
func (s *Service) trySplit(orderNo string) error {
	tx, err := s.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	order, parent, _, err := compute.NewRepository(s.db).LockOrderFamilyTx(tx, orderNo)
	if err != nil || order == nil {
		return err
	}
	var parentStatus *string
	if parent != nil {
		parentStatus = &parent.Status
	}
	if !splitEligible(order.Status, parentStatus) {
		return nil
	}
	var pay Payment
	if err := tx.Get(&pay, "SELECT * FROM payments WHERE order_no=? AND status='paid' ORDER BY id DESC LIMIT 1 FOR UPDATE", orderNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if pay.ClearedAt == nil || pay.RefundStatus != nil {
		return nil
	}
	var rows []Settlement
	if err := tx.Select(&rows, "SELECT * FROM settlements WHERE order_no=? FOR UPDATE", orderNo); err != nil {
		return err
	}
	var supplier *Settlement
	var total int64
	for i := range rows {
		total += rows[i].Amount
		if rows[i].PayeeType == "supplier" {
			supplier = &rows[i]
		}
	}
	if supplier == nil || len(rows) != 2 || total != pay.Amount {
		return fmt.Errorf("订单 %s 应结记录与支付金额不一致", orderNo)
	}
	if supplier.Status != "pending" && supplier.Status != "failed" {
		return nil
	}
	if supplier.Amount <= 0 {
		return fmt.Errorf("订单 %s 供给方应结金额无效", orderNo)
	}
	ledgerNo, err := completedMerchantNo(tx, supplier.PayeeID)
	if err != nil {
		return err
	}
	if ledgerNo == "" {
		// 供给方尚未开通易宝商编: 资金安全地留在待分账账户, 开通后定时任务自动补分。
		return nil
	}
	requestID := newRequestID("D", orderNo)
	if _, err := tx.Exec(`UPDATE settlements SET status='processing',divide_request_id=?,ledger_no=?,fail_reason='' WHERE order_no=?`,
		requestID, ledgerNo, orderNo); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	result, err := s.yeepay.Divide(DivideOrder{OrderNo: orderNo, RequestID: requestID, Items: []DivideItem{
		{LedgerNo: ledgerNo, Amount: supplier.Amount, Desc: "算力订单 " + orderNo + " 供给方结算"},
	}})
	if err != nil {
		if bizCode(err) != "" {
			return s.applyDivideResult(orderNo, requestID, &DivideResult{Status: "FAIL"}, err.Error())
		}
		return err // 结果不明: 保持 processing, 定时任务按请求号查询
	}
	return s.applyDivideResult(orderNo, requestID, result, "")
}

func (s *Service) applyDivideResult(orderNo, requestID string, r *DivideResult, reason string) error {
	switch r.Status {
	case "SUCCESS":
		var detailNo *string
		for _, d := range r.Details {
			if d.DetailNo != "" {
				no := d.DetailNo
				detailNo = &no
			}
		}
		_, err := s.db.Exec(`UPDATE settlements SET status='success',divide_detail_no=CASE WHEN payee_type='supplier' THEN ? ELSE divide_detail_no END
			WHERE order_no=? AND divide_request_id=? AND status='processing'`, detailNo, orderNo, requestID)
		return err
	case "FAIL":
		if reason == "" {
			reason = "易宝分账失败"
		}
		_, err := s.db.Exec(`UPDATE settlements SET status='failed',fail_reason=? WHERE order_no=? AND divide_request_id=? AND status='processing'`,
			truncateRunes(reason, 250), orderNo, requestID)
		return err
	}
	return nil
}

// ===== 退款 =====

// ExecuteRefund 运营审批退款: 买家申请退款(refunding)或已关闭(cancelled)且已收款的订单,
// 经易宝原路退回买家。已分账的订单同时从供给方商编归还其分得部分。
func (s *Service) ExecuteRefund(operatorID int64, orderNo, reason string) (string, error) {
	if operatorID <= 0 {
		return "", fmt.Errorf("无权操作退款")
	}
	if !s.yeepay.Configured() {
		return "", ErrYeepayNotConfigured
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "运营审核通过退款"
	}
	tx, err := s.db.Beginx()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	order, _, _, err := compute.NewRepository(s.db).LockOrderFamilyTx(tx, orderNo)
	if err != nil {
		return "", err
	}
	if order == nil {
		return "", fmt.Errorf("order not found")
	}
	if order.Status != "refunding" && order.Status != "cancelled" {
		return "", ErrRefundNotAllowed
	}
	var pay Payment
	if err := tx.Get(&pay, "SELECT * FROM payments WHERE order_no=? AND status='paid' ORDER BY id DESC LIMIT 1 FOR UPDATE", orderNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrRefundNotAllowed
		}
		return "", err
	}
	if pay.RefundStatus != nil && *pay.RefundStatus != "pending" && *pay.RefundStatus != "failed" {
		return "", fmt.Errorf("退款已在处理中")
	}
	if _, err := tx.Exec("UPDATE payments SET refund_status='pending',refund_reason=? WHERE id=?", truncateRunes(reason, 250), pay.ID); err != nil {
		return "", err
	}
	if _, err := tx.Exec(`INSERT INTO audit_logs (operator_id,action,target_type,target_id,before_value,after_value,ip)
		VALUES (?,'execute_refund','order',?,?,?,'')`, operatorID, order.ID, order.Status, "refund_pending"); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	if pay.ClearedAt == nil {
		return "pending", nil // 清算入账后由定时任务自动提交
	}
	return s.submitRefund(orderNo)
}

// submitRefund 提交一笔 pending 退款。分账处理中时等待分账终态, 避免退款与分账交叉。
func (s *Service) submitRefund(orderNo string) (string, error) {
	tx, err := s.db.Beginx()
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	if _, _, _, err := compute.NewRepository(s.db).LockOrderFamilyTx(tx, orderNo); err != nil {
		return "", err
	}
	var pay Payment
	if err := tx.Get(&pay, "SELECT * FROM payments WHERE order_no=? AND status='paid' AND refund_status='pending' ORDER BY id DESC LIMIT 1 FOR UPDATE", orderNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", err
	}
	var rows []Settlement
	if err := tx.Select(&rows, "SELECT * FROM settlements WHERE order_no=? FOR UPDATE", orderNo); err != nil {
		return "", err
	}
	var back []DivideBackItem
	for _, row := range rows {
		if row.Status == "processing" {
			return "pending", nil
		}
		if row.PayeeType == "supplier" && row.Status == "success" {
			if row.LedgerNo == nil || row.DivideDetailNo == nil {
				return "", fmt.Errorf("订单 %s 已分账但缺少分账明细, 需人工处理", orderNo)
			}
			back = append(back, DivideBackItem{LedgerNo: *row.LedgerNo, DivideDetailNo: *row.DivideDetailNo, Amount: row.Amount})
		}
	}
	requestID := newRequestID("R", orderNo)
	if _, err := tx.Exec("UPDATE payments SET refund_status='processing',refund_request_id=? WHERE id=?", requestID, pay.ID); err != nil {
		return "", err
	}
	if err := tx.Commit(); err != nil {
		return "", err
	}
	result, err := s.yeepay.Refund(RefundOrder{OrderNo: orderNo, RequestID: requestID, Amount: pay.Amount, Reason: pay.RefundReason, DivideBack: back})
	if err != nil {
		if bizCode(err) != "" {
			return "failed", s.applyRefundResult(&RefundResult{OrderNo: orderNo, RequestID: requestID, Status: "FAILED", FailReason: err.Error()})
		}
		return "processing", err
	}
	result.OrderNo, result.RequestID = orderNo, requestID
	if err := s.applyRefundResult(result); err != nil {
		return "", err
	}
	return strings.ToLower(result.Status), nil
}

// applyRefundResult 落地退款终态(同步结果/查询/异步通知共用, 幂等)。
func (s *Service) applyRefundResult(r *RefundResult) error {
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
		_, err := s.db.Exec("UPDATE payments SET refund_status='failed',refund_reason=? WHERE refund_request_id=? AND refund_status='processing'",
			truncateRunes(reason, 250), r.RequestID)
		return err
	default:
		return nil // PROCESSING / SUSPEND: 等待终态
	}
	tx, err := s.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var orderNo string
	if err := tx.Get(&orderNo, "SELECT order_no FROM payments WHERE refund_request_id=?", r.RequestID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return errPaymentNotFound
		}
		return err
	}
	if _, _, _, err := compute.NewRepository(s.db).LockOrderFamilyTx(tx, orderNo); err != nil {
		return err
	}
	var pay Payment
	if err := tx.Get(&pay, "SELECT * FROM payments WHERE refund_request_id=? FOR UPDATE", r.RequestID); err != nil {
		return err
	}
	if pay.Status == "refunded" {
		return tx.Commit()
	}
	if r.Amount > 0 && r.Amount != pay.Amount {
		return fmt.Errorf("易宝退款金额与支付金额不一致: order=%s", orderNo)
	}
	if _, err := tx.Exec("UPDATE payments SET status='refunded',refund_status='success',refunded_at=NOW() WHERE id=?", pay.ID); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE settlements SET status=CASE WHEN status='success' THEN 'reversed' ELSE 'cancelled' END WHERE order_no=?`, orderNo); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if s.onRefunded != nil {
		if err := s.onRefunded(orderNo); err != nil {
			slog.Error("退款已完成但订单状态流转失败", "order_no", orderNo, "error", err)
		}
	}
	return nil
}

// ===== 主动查询与定时任务 =====

// RunJobs 支付状态兜底同步(漏通知/通知延迟)、分账与退款推进。须在关闭超时订单之前执行,
// 以免最后一刻付款成功的订单因通知延迟被误关。
func (s *Service) RunJobs() error {
	if !s.yeepay.Configured() {
		return nil
	}
	var firstErr error
	keep := func(err error) {
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	keep(s.syncPendingPayments())
	keep(s.syncClearing())
	keep(s.queueCancelledPaidRefunds())
	keep(s.syncDivides())
	keep(s.runSplits())
	keep(s.runRefunds())
	return firstErr
}

func (s *Service) syncPayment(orderNo string) error {
	result, err := s.yeepay.QueryPayment(orderNo)
	if err != nil {
		return err
	}
	if result.OrderNo == "" {
		result.OrderNo = orderNo
	}
	if err := s.confirmPayment(result); err != nil {
		return err
	}
	if result.Status == "SUCCESS" && result.Cleared {
		return s.markCleared(result)
	}
	return nil
}

func (s *Service) syncPendingPayments() error {
	var nos []string
	if err := s.db.Select(&nos, `SELECT DISTINCT order_no FROM payments WHERE status='pending' AND created_at > NOW() - INTERVAL 2 DAY LIMIT 200`); err != nil {
		return err
	}
	return eachOrder(nos, s.syncPayment)
}

func (s *Service) syncClearing() error {
	var nos []string
	if err := s.db.Select(&nos, `SELECT DISTINCT order_no FROM payments WHERE status IN ('paid','refunded') AND cleared_at IS NULL
		AND paid_at > NOW() - INTERVAL 30 DAY LIMIT 200`); err != nil {
		return err
	}
	return eachOrder(nos, s.syncPayment)
}

// queueCancelledPaidRefunds 已收款但被运营关闭的订单: 关单即运营决定, 自动排队原路退款。
// queueCancelledPaidRefunds 租期从未开始就被关闭的已付订单自动原路退款。
// 租期已开始(供给方已交付)后关闭的订单不自动退: 是否全额退回、追回供给方分账
// 属于运营判断, 进入退款待办由运营执行(见 ListRefunds / ExecuteRefund)。
func (s *Service) queueCancelledPaidRefunds() error {
	_, err := s.db.Exec(`UPDATE payments p JOIN orders o ON o.order_no=p.order_no
		SET p.refund_status='pending',p.refund_reason='订单已关闭, 款项原路退回'
		WHERE p.status='paid' AND p.refund_status IS NULL AND o.status='cancelled' AND o.lease_start_at IS NULL
		AND NOT EXISTS (SELECT 1 FROM settlements st WHERE st.order_no=p.order_no AND st.status IN ('processing','success'))`)
	return err
}

func (s *Service) syncDivides() error {
	var rows []struct {
		OrderNo   string `db:"order_no"`
		RequestID string `db:"divide_request_id"`
	}
	if err := s.db.Select(&rows, `SELECT order_no,divide_request_id FROM settlements
		WHERE payee_type='supplier' AND status='processing' AND divide_request_id IS NOT NULL LIMIT 200`); err != nil {
		return err
	}
	var firstErr error
	for _, row := range rows {
		result, err := s.yeepay.QueryDivide(row.OrderNo, row.RequestID)
		if err != nil {
			if bizCode(err) != "" {
				err = s.applyDivideResult(row.OrderNo, row.RequestID, &DivideResult{Status: "FAIL"}, err.Error())
			}
		} else {
			err = s.applyDivideResult(row.OrderNo, row.RequestID, result, "")
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

func (s *Service) runSplits() error {
	var nos []string
	if err := s.db.Select(&nos, `SELECT DISTINCT s.order_no FROM settlements s
		JOIN payments p ON p.order_no=s.order_no AND p.status='paid'
		WHERE s.payee_type='supplier' AND p.cleared_at IS NOT NULL AND p.refund_status IS NULL
		AND (s.status='pending' OR (s.status='failed' AND s.updated_at < NOW() - INTERVAL 1 HOUR)) LIMIT 200`); err != nil {
		return err
	}
	return eachOrder(nos, s.trySplit)
}

func (s *Service) runRefunds() error {
	var pending []string
	if err := s.db.Select(&pending, `SELECT order_no FROM payments WHERE status='paid' AND refund_status='pending' AND cleared_at IS NOT NULL LIMIT 100`); err != nil {
		return err
	}
	firstErr := eachOrder(pending, func(no string) error { _, err := s.submitRefund(no); return err })
	var processing []struct {
		OrderNo   string `db:"order_no"`
		RequestID string `db:"refund_request_id"`
	}
	if err := s.db.Select(&processing, `SELECT order_no,refund_request_id FROM payments WHERE refund_status='processing' AND refund_request_id IS NOT NULL LIMIT 100`); err != nil {
		return err
	}
	for _, row := range processing {
		result, err := s.yeepay.QueryRefund(row.OrderNo, row.RequestID)
		if err != nil {
			if bizCode(err) != "" {
				err = s.applyRefundResult(&RefundResult{OrderNo: row.OrderNo, RequestID: row.RequestID, Status: "FAILED", FailReason: err.Error()})
			}
		} else {
			result.OrderNo, result.RequestID = row.OrderNo, row.RequestID
			err = s.applyRefundResult(result)
		}
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// eachOrder 单笔失败不中断整批, 否则一笔坏数据会卡住后面所有订单。
func eachOrder(nos []string, fn func(string) error) error {
	var firstErr error
	for _, no := range nos {
		if err := fn(no); err != nil {
			slog.Warn("易宝同步单笔失败", "order_no", no, "error", err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

func newRequestID(prefix, orderNo string) string {
	return prefix + orderNo + "-" + strconv.FormatInt(time.Now().UnixNano(), 36)
}

// T-032: Daily reconciliation —— 逐笔向易宝核对当日已收款支付的状态与金额。
func (s *Service) Reconcile(date string) (*ReconcileResult, error) {
	if !s.yeepay.Configured() {
		return nil, ErrYeepayNotConfigured
	}
	if date == "" {
		date = time.Now().In(shanghai).Format("2006-01-02")
	}
	if _, err := time.Parse("2006-01-02", date); err != nil {
		return nil, fmt.Errorf("日期格式应为 YYYY-MM-DD")
	}
	var list []Payment
	if err := s.db.Select(&list, "SELECT * FROM payments WHERE status IN ('paid','refunded') AND DATE(paid_at)=? ORDER BY id", date); err != nil {
		return nil, err
	}
	result := &ReconcileResult{Date: date, Mismatches: make([]string, 0)}
	for _, p := range list {
		result.Checked++
		result.PlatformTotal += p.Amount
		remote, err := s.yeepay.QueryPayment(p.OrderNo)
		if err != nil {
			result.Mismatches = append(result.Mismatches, p.OrderNo+": 易宝查询失败 "+err.Error())
			continue
		}
		if remote.Status != "SUCCESS" || remote.UniqueOrderNo != p.YeepayTxID {
			result.Mismatches = append(result.Mismatches, fmt.Sprintf("%s: 易宝状态 %s 流水 %s", p.OrderNo, remote.Status, remote.UniqueOrderNo))
			continue
		}
		result.YeepayTotal += remote.Amount
		if remote.Amount != p.Amount {
			result.Mismatches = append(result.Mismatches, fmt.Sprintf("%s: 金额不一致 平台 %d 分 / 易宝 %d 分", p.OrderNo, p.Amount, remote.Amount))
		}
	}
	result.Diff = result.PlatformTotal - result.YeepayTotal
	return result, nil
}

// ===== 供给方易宝商编(入网) =====

type OnboardStatus struct {
	Status            string  `json:"status"` // not_started / pending / completed
	ApplicationStatus string  `json:"application_status"`
	MerchantNo        *string `json:"merchant_no"`
	AuditOpinion      string  `json:"audit_opinion"`
	NetInURL          string  `json:"netin_url,omitempty"`
}

func onboardView(m *Merchant) *OnboardStatus {
	if m == nil {
		return &OnboardStatus{Status: "not_started"}
	}
	view := &OnboardStatus{Status: "pending", ApplicationStatus: m.ApplicationStatus, MerchantNo: m.MerchantNo, AuditOpinion: m.AuditOpinion}
	if m.ApplicationStatus == "COMPLETED" {
		view.Status = "completed"
	} else if m.NetInURL != nil {
		view.NetInURL = *m.NetInURL
	}
	return view
}

// T-026: StartOnboard 获取(或复用)供给方 H5 入网链接, 供给方在易宝页面自助完成资料、签约与开户。
func (s *Service) StartOnboard(userID int64) (*OnboardStatus, error) {
	m, err := s.repo.GetMerchant(userID)
	if err != nil {
		return nil, err
	}
	if m != nil && m.ApplicationStatus == "COMPLETED" {
		return onboardView(m), nil
	}
	requestNo := "NI" + strconv.FormatInt(userID, 10) + "-" + strconv.FormatInt(time.Now().Unix(), 36)
	if m != nil {
		requestNo = m.RequestNo
	}
	link, err := s.yeepay.NetInURL(requestNo)
	if err != nil {
		return nil, err
	}
	if _, err := s.db.Exec(`INSERT INTO yeepay_merchants (user_id,request_no,netin_url) VALUES (?,?,?)
		ON DUPLICATE KEY UPDATE netin_url=VALUES(netin_url)`, userID, requestNo, link); err != nil {
		return nil, err
	}
	m, err = s.repo.GetMerchant(userID)
	if err != nil {
		return nil, err
	}
	return onboardView(m), nil
}

func (s *Service) GetOnboardStatus(userID int64) (*OnboardStatus, error) {
	m, err := s.repo.GetMerchant(userID)
	if err != nil || m == nil {
		return onboardView(m), err
	}
	if m.ApplicationStatus != "COMPLETED" && m.BoundBy == nil && s.yeepay.Configured() {
		if result, err := s.yeepay.QueryNetIn(m.RequestNo); err == nil {
			result.RequestNo = m.RequestNo
			if err := s.applyNetIn(result); err != nil {
				return nil, err
			}
			if m, err = s.repo.GetMerchant(userID); err != nil {
				return nil, err
			}
		} else {
			slog.Warn("易宝入网进度查询失败", "user_id", userID, "error", err)
		}
	}
	return onboardView(m), nil
}

func (s *Service) applyNetIn(r *NetInResult) error {
	if r.RequestNo == "" || r.ApplicationStatus == "" {
		return fmt.Errorf("入网结果缺少请求号或状态")
	}
	var merchantNo *string
	if r.ApplicationStatus == "COMPLETED" {
		if r.MerchantNo == "" {
			return fmt.Errorf("入网完成但缺少商编")
		}
		merchantNo = &r.MerchantNo
	}
	_, err := s.db.Exec(`UPDATE yeepay_merchants SET application_status=?,merchant_no=COALESCE(?,merchant_no),audit_opinion=?
		WHERE request_no=? AND application_status<>'COMPLETED'`, r.ApplicationStatus, merchantNo, truncateRunes(r.AuditOpinion, 500), r.RequestNo)
	return err
}

// BindMerchant 运营人工绑定供给方已在易宝开通的商编(线下入网的兜底通道)。
func (s *Service) BindMerchant(operatorID, userID int64, merchantNo string) error {
	merchantNo = strings.TrimSpace(merchantNo)
	if operatorID <= 0 || userID <= 0 {
		return fmt.Errorf("无权操作")
	}
	if len(merchantNo) < 8 || len(merchantNo) > 32 || strings.Trim(merchantNo, "0123456789") != "" {
		return fmt.Errorf("易宝商编应为数字")
	}
	var supplier int
	if err := s.db.Get(&supplier, "SELECT COUNT(*) FROM user_roles WHERE user_id=? AND role='supplier'", userID); err != nil {
		return err
	}
	if supplier == 0 {
		return fmt.Errorf("该用户不是供给方")
	}
	tx, err := s.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`INSERT INTO yeepay_merchants (user_id,request_no,merchant_no,application_status,bound_by) VALUES (?,?,?,'COMPLETED',?)
		ON DUPLICATE KEY UPDATE merchant_no=VALUES(merchant_no),application_status='COMPLETED',bound_by=VALUES(bound_by),audit_opinion=''`,
		userID, "MANUAL"+strconv.FormatInt(userID, 10), merchantNo, operatorID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO audit_logs (operator_id,action,target_type,target_id,before_value,after_value,ip)
		VALUES (?,'bind_yeepay_merchant','user',?,'',?,'')`, operatorID, userID, merchantNo); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) ListMerchants() ([]Merchant, error) { return s.repo.ListMerchants() }

func (s *Service) ListRefunds() ([]RefundItem, error) { return s.repo.ListRefunds() }

// GetOrderPayments 买家查询支付状态; 从收银台返回时支付仍 pending 则主动向易宝查询一次,
// 不必等异步通知。
func (s *Service) GetOrderPayments(buyerID int64, orderNo string) ([]Payment, error) {
	var list = make([]Payment, 0)
	var owner int64
	if err := s.db.Get(&owner, "SELECT buyer_id FROM orders WHERE order_no=?", orderNo); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("order not found")
		}
		return nil, err
	}
	if owner != buyerID {
		return nil, fmt.Errorf("无权查看其他买家的支付记录")
	}
	p, err := s.repo.GetPaymentByOrder(orderNo)
	if errors.Is(err, sql.ErrNoRows) {
		return list, nil
	}
	if err != nil {
		return list, err
	}
	if p.Status == "pending" && s.yeepay.Configured() {
		if err := s.syncPayment(orderNo); err != nil {
			slog.Warn("易宝支付状态查询失败", "order_no", orderNo, "error", err)
		} else if p, err = s.repo.GetPaymentByOrder(orderNo); err != nil {
			return list, err
		}
	}
	return append(list, *p), nil
}

func (s *Service) GetOrderSettlements(supplierID int64, orderNo string) ([]Settlement, error) {
	return s.repo.GetSettlementsByOrder(supplierID, orderNo)
}

var settlementStatuses = map[string]bool{"pending": true, "processing": true, "success": true, "failed": true, "cancelled": true, "reversed": true}

// ListSupplierSettlements 供给方结算流水(按商品归属)。
func (s *Service) ListSupplierSettlements(supplierID int64, status string, page, pageSize int) ([]Settlement, int64, error) {
	if status != "" && !settlementStatuses[status] {
		return nil, 0, fmt.Errorf("invalid status")
	}
	return s.repo.ListSupplierSettlements(supplierID, status, page, pageSize)
}

// SumSupplierSettlements 应结/已分账/待结合计。
func (s *Service) SumSupplierSettlements(supplierID int64) (int64, int64, int64, error) {
	return s.repo.SumSupplierSettlements(supplierID)
}

func (s *Service) ListPaymentsByDate(date string) ([]Payment, error) {
	if date == "" {
		date = time.Now().In(shanghai).Format("2006-01-02")
	}
	return s.repo.ListPaymentsByDate(date)
}
