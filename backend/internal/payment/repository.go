package payment

import (
	"database/sql"
	"errors"
	"github.com/jmoiron/sqlx"
	"time"
)

type Payment struct {
	ID              int64      `db:"id" json:"id"`
	OrderNo         string     `db:"order_no" json:"order_no"`
	Amount          int64      `db:"amount" json:"amount"` // fen
	Channel         string     `db:"channel" json:"channel"`
	YeepayTxID      string     `db:"yeepay_tx_id" json:"yeepay_tx_id"` // 易宝收款订单号 uniqueOrderNo
	PayURL          *string    `db:"pay_url" json:"-"`
	Status          string     `db:"status" json:"status"`
	PaidAt          *time.Time `db:"paid_at" json:"paid_at"`
	ClearedAt       *time.Time `db:"cleared_at" json:"cleared_at"`
	RefundStatus    *string    `db:"refund_status" json:"refund_status"`
	RefundRequestID *string    `db:"refund_request_id" json:"refund_request_id"`
	RefundReason    string     `db:"refund_reason" json:"refund_reason"`
	RefundedAt      *time.Time `db:"refunded_at" json:"refunded_at"`
	CreatedAt       time.Time  `db:"created_at" json:"created_at"`
}

type Settlement struct {
	ID              int64      `db:"id" json:"id"`
	SettlementID    string     `db:"settlement_id" json:"settlement_id"`
	OrderNo         string     `db:"order_no" json:"order_no"`
	PayeeType       string     `db:"payee_type" json:"payee_type"`
	PayeeID         int64      `db:"payee_id" json:"payee_id"`
	Amount          int64      `db:"amount" json:"amount"` // fen
	Status          string     `db:"status" json:"status"`
	DivideRequestID *string    `db:"divide_request_id" json:"-"`
	DivideDetailNo  *string    `db:"divide_detail_no" json:"-"`
	LedgerNo        *string    `db:"ledger_no" json:"-"`
	FailReason      string     `db:"fail_reason" json:"fail_reason"`
	CreatedAt       time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt       *time.Time `db:"updated_at" json:"updated_at"`
}

// Merchant 供给方在易宝开通的分账接收商编。
type Merchant struct {
	ID                int64     `db:"id" json:"id"`
	UserID            int64     `db:"user_id" json:"user_id"`
	RequestNo         string    `db:"request_no" json:"-"`
	MerchantNo        *string   `db:"merchant_no" json:"merchant_no"`
	ApplicationStatus string    `db:"application_status" json:"application_status"`
	NetInURL          *string   `db:"netin_url" json:"-"`
	AuditOpinion      string    `db:"audit_opinion" json:"audit_opinion"`
	BoundBy           *int64    `db:"bound_by" json:"bound_by"`
	CreatedAt         time.Time `db:"created_at" json:"created_at"`
	UpdatedAt         time.Time `db:"updated_at" json:"updated_at"`
}

type Repository struct {
	db *sqlx.DB
}

func NewRepository(db *sqlx.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) GetPaymentByOrder(orderNo string) (*Payment, error) {
	var p Payment
	err := r.db.Get(&p, "SELECT * FROM payments WHERE order_no = ? ORDER BY id DESC LIMIT 1", orderNo)
	return &p, err
}

// ListSupplierSettlements 供给方结算流水: 按商品归属过滤, 归属经
// orders -> products 的 supplier_id 判定(不依赖 payee_id)。
func (r *Repository) ListSupplierSettlements(supplierID int64, status string, page, pageSize int) ([]Settlement, int64, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	where := "WHERE s.payee_type='supplier' AND p.supplier_id=?"
	args := []interface{}{supplierID}
	if status != "" {
		where += " AND s.status=?"
		args = append(args, status)
	}
	var total int64
	if err := r.db.Get(&total,
		"SELECT COUNT(*) FROM settlements s JOIN orders o ON o.order_no=s.order_no JOIN products p ON p.id=o.product_id "+where,
		args...); err != nil {
		return nil, 0, err
	}
	list := make([]Settlement, 0)
	err := r.db.Select(&list,
		`SELECT s.* FROM settlements s
		JOIN orders o ON o.order_no=s.order_no
		JOIN products p ON p.id=o.product_id
		`+where+` ORDER BY s.created_at DESC LIMIT ? OFFSET ?`,
		append(args, pageSize, (page-1)*pageSize)...)
	return list, total, err
}

// SumSupplierSettlements 供给方结算汇总: 应结总额 / 已分账(success) / 待结(pending+processing+failed)。
// 已取消(cancelled)和已退回(reversed)的应结随退款作废, 不计入。
func (r *Repository) SumSupplierSettlements(supplierID int64) (total, succeeded, pending int64, err error) {
	err = r.db.QueryRow(
		`SELECT
			COALESCE(SUM(CASE WHEN s.status IN ('pending','processing','success','failed') THEN s.amount END),0),
			COALESCE(SUM(CASE WHEN s.status='success' THEN s.amount END),0),
			COALESCE(SUM(CASE WHEN s.status IN ('pending','processing','failed') THEN s.amount END),0)
		FROM settlements s
		JOIN orders o ON o.order_no=s.order_no
		JOIN products p ON p.id=o.product_id
		WHERE s.payee_type='supplier' AND p.supplier_id=?`, supplierID).
		Scan(&total, &succeeded, &pending)
	return
}

// GetSettlementsByOrder 归属过滤在 SQL 内: 只能看自己商品所属订单的结算流水。
// 不带 supplier 条件的版本曾允许任意供给方按订单号窥探他人分账金额(2026-09-07 修复)。
func (r *Repository) GetSettlementsByOrder(supplierID int64, orderNo string) ([]Settlement, error) {
	list := make([]Settlement, 0)
	err := r.db.Select(&list, `SELECT s.* FROM settlements s
		JOIN orders o ON o.order_no = s.order_no
		JOIN products p ON p.id = o.product_id
		WHERE s.order_no = ? AND p.supplier_id = ?`, orderNo, supplierID)
	return list, err
}

// Daily reconciliation
type ReconcileResult struct {
	Date          string   `json:"date"`
	Checked       int      `json:"checked"`
	PlatformTotal int64    `json:"platform_total"`
	YeepayTotal   int64    `json:"yeepay_total"`
	Diff          int64    `json:"diff"`
	Mismatches    []string `json:"mismatches"`
}

func (r *Repository) ListPaymentsByDate(date string) ([]Payment, error) {
	list := make([]Payment, 0)
	err := r.db.Select(&list, "SELECT * FROM payments WHERE DATE(created_at)=? ORDER BY id DESC", date)
	return list, err
}

// ListRefunds 待运营处理的退款: 买家已申请退款(refunding)、交付后被关闭(cancelled)的已付订单,
// 以及已进入退款流程的支付。
type RefundItem struct {
	OrderNo      string     `db:"order_no" json:"order_no"`
	OrderStatus  string     `db:"order_status" json:"order_status"`
	BuyerID      int64      `db:"buyer_id" json:"buyer_id"`
	Amount       int64      `db:"amount" json:"amount"`
	PaymentState string     `db:"payment_status" json:"payment_status"`
	RefundStatus *string    `db:"refund_status" json:"refund_status"`
	RefundReason string     `db:"refund_reason" json:"refund_reason"`
	PaidAt       *time.Time `db:"paid_at" json:"paid_at"`
	ClearedAt    *time.Time `db:"cleared_at" json:"cleared_at"`
	RefundedAt   *time.Time `db:"refunded_at" json:"refunded_at"`
}

func (r *Repository) ListRefunds() ([]RefundItem, error) {
	list := make([]RefundItem, 0)
	err := r.db.Select(&list, `SELECT o.order_no,o.status AS order_status,o.buyer_id,p.amount,p.status AS payment_status,
		p.refund_status,p.refund_reason,p.paid_at,p.cleared_at,p.refunded_at
		FROM payments p JOIN orders o ON o.order_no=p.order_no
		WHERE p.status IN ('paid','refunded') AND (p.refund_status IS NOT NULL OR o.status IN ('refunding','cancelled'))
		ORDER BY (p.refund_status='success'), p.id DESC LIMIT 200`)
	return list, err
}

func (r *Repository) GetMerchant(userID int64) (*Merchant, error) {
	var m Merchant
	err := r.db.Get(&m, "SELECT * FROM yeepay_merchants WHERE user_id=?", userID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &m, err
}

func (r *Repository) ListMerchants() ([]Merchant, error) {
	list := make([]Merchant, 0)
	err := r.db.Select(&list, "SELECT * FROM yeepay_merchants ORDER BY updated_at DESC LIMIT 500")
	return list, err
}

// completedMerchantNo 已开通的供给方商编; 未开通返回空串。
func completedMerchantNo(q sqlx.Queryer, userID int64) (string, error) {
	var no sql.NullString
	err := sqlx.Get(q, &no, "SELECT merchant_no FROM yeepay_merchants WHERE user_id=? AND application_status='COMPLETED'", userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return no.String, err
}
