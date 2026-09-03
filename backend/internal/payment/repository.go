package payment

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/jmoiron/sqlx"
	"time"
)

type Payment struct {
	ID         int64      `db:"id" json:"id"`
	OrderNo    string     `db:"order_no" json:"order_no"`
	Amount     int64      `db:"amount" json:"amount"` // fen
	Channel    string     `db:"channel" json:"channel"`
	YeepayTxID string     `db:"yeepay_tx_id" json:"yeepay_tx_id"`
	Status     string     `db:"status" json:"status"`
	PaidAt     *time.Time `db:"paid_at" json:"paid_at"`
	CreatedAt  time.Time  `db:"created_at" json:"created_at"`
}

type Settlement struct {
	ID           int64     `db:"id" json:"id"`
	SettlementID string    `db:"settlement_id" json:"settlement_id"`
	OrderNo      string    `db:"order_no" json:"order_no"`
	PayeeType    string    `db:"payee_type" json:"payee_type"`
	PayeeID      int64     `db:"payee_id" json:"payee_id"`
	Amount       int64     `db:"amount" json:"amount"` // fen
	Status       string    `db:"status" json:"status"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}

type YeepayOnboard struct {
	ID     int64  `db:"id" json:"id"`
	UserID int64  `db:"user_id" json:"user_id"`
	Status string `db:"status" json:"status"`
}

type Repository struct {
	db *sqlx.DB
}

func NewRepository(db *sqlx.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) CreatePayment(p *Payment) error {
	_, err := r.db.Exec(
		"INSERT INTO payments (order_no, amount, channel, yeepay_tx_id, status) VALUES (?,?,?,?,?)",
		p.OrderNo, p.Amount, p.Channel, p.YeepayTxID, "pending",
	)
	return err
}

func (r *Repository) GetPaymentByOrder(orderNo string) (*Payment, error) {
	var p Payment
	err := r.db.Get(&p, "SELECT * FROM payments WHERE order_no = ? ORDER BY id DESC LIMIT 1", orderNo)
	return &p, err
}

func (r *Repository) GetPaymentByYeepayTx(txID string) (*Payment, error) {
	var p Payment
	err := r.db.Get(&p, "SELECT * FROM payments WHERE yeepay_tx_id = ?", txID)
	return &p, err
}

func (r *Repository) UpdatePaymentStatus(orderNo, status string) error {
	_, err := r.db.Exec("UPDATE payments SET status=?, paid_at=NOW() WHERE order_no=?", status, orderNo)
	return err
}

func (r *Repository) CreateSettlement(s *Settlement) error {
	_, err := r.db.Exec(
		"INSERT INTO settlements (settlement_id, order_no, payee_type, payee_id, amount, status) VALUES (?,?,?,?,?,?)",
		s.SettlementID, s.OrderNo, s.PayeeType, s.PayeeID, s.Amount, "pending",
	)
	return err
}

// ListSupplierSettlements 供给方结算流水: 按商品归属过滤。
// 注意 settlements.payee_id 目前不可靠(分账创建时写 0), 归属必须经
// orders -> products 的 supplier_id 判定。
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

// SumSupplierSettlements 供给方结算汇总: 应结总额 / 已分账(success) / 待结(pending+processing)。
func (r *Repository) SumSupplierSettlements(supplierID int64) (total, succeeded, pending int64, err error) {
	err = r.db.QueryRow(
		`SELECT
			COALESCE(SUM(s.amount),0),
			COALESCE(SUM(CASE WHEN s.status='success' THEN s.amount END),0),
			COALESCE(SUM(CASE WHEN s.status IN ('pending','processing') THEN s.amount END),0)
		FROM settlements s
		JOIN orders o ON o.order_no=s.order_no
		JOIN products p ON p.id=o.product_id
		WHERE s.payee_type='supplier' AND p.supplier_id=?`, supplierID).
		Scan(&total, &succeeded, &pending)
	return
}

func (r *Repository) GetSettlementsByOrder(orderNo string) ([]Settlement, error) {
	var list []Settlement
	err := r.db.Select(&list, "SELECT * FROM settlements WHERE order_no = ?", orderNo)
	return list, err
}

func (r *Repository) UpdateSettlementStatus(settlementID, status string) error {
	_, err := r.db.Exec("UPDATE settlements SET status=? WHERE settlement_id=?", status, settlementID)
	return err
}

// Daily reconciliation
type ReconcileResult struct {
	PlatformTotal int64 `db:"platform_total" json:"platform_total"`
	YeepayTotal   int64 `db:"yeepay_total" json:"yeepay_total"`
	Diff          int64 `json:"diff"`
}

func (r *Repository) GetDailyTotal(date string) (int64, error) {
	var total int64
	err := r.db.Get(&total, "SELECT COALESCE(SUM(amount),0) FROM payments WHERE status='paid' AND DATE(paid_at)=?", date)
	return total, err
}

func (r *Repository) ListPaymentsByDate(date string) ([]Payment, error) {
	var list []Payment
	err := r.db.Select(&list, "SELECT * FROM payments WHERE DATE(created_at)=?", date)
	return list, err
}

// Supplier onboard status
func (r *Repository) CreateOnboard(userID int64) error {
	_, err := r.db.Exec("INSERT INTO yeepay_onboard (user_id, status) VALUES (?,'pending') ON DUPLICATE KEY UPDATE status='pending'", userID)
	return err
}

type onboardRow struct {
	Status string `db:"status"`
}

func (r *Repository) GetOnboardStatus(userID int64) (string, error) {
	var row onboardRow
	err := r.db.Get(&row, "SELECT status FROM yeepay_onboard WHERE user_id=?", userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "not_started", nil
	}
	return row.Status, err
}

// Yeepay onboard table (simple)
func EnsureYeepayOnboardTable(db *sqlx.DB) {
	db.Exec(`CREATE TABLE IF NOT EXISTS yeepay_onboard (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		user_id BIGINT NOT NULL UNIQUE,
		status VARCHAR(32) DEFAULT 'pending',
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`)
}

// ===== 易宝支付客户端 =====
// TODO: 接入真实易宝 SDK 后替换本实现
// 所需信息:
//   1. 易宝商户号 (merchant_no)
//   2. RSA2048 私钥 (用于签名，必须存储在 KMS/密钥管理服务中，不入库不入前端)
//   3. 易宝开放平台 API 地址 (沙箱: https://open.yeepay.com/sandbox, 生产: https://open.yeepay.com)
//   4. 分账规则编号 (需易宝商务开通合单支付+分账产品后在商户后台配置)
// 接入文档: https://open.yeepay.com/docs/apis/ptssfk/

type YeepayClient struct {
	// merchantNo string    // TODO: 从配置加载
	// privateKey *rsa.PrivateKey // TODO: 从 KMS 加载
}

func NewYeepayClient() *YeepayClient { return &YeepayClient{} }

func (y *YeepayClient) CreatePayment(orderNo string, amount int64, channel string) (payURL string, txID string, err error) {
	return "", "", ErrYeepayNotConfigured
}

// VerifyCallback 验证易宝回调的 RSA 签名。
//
// 🔴 返回 error 而非 bool 是刻意的：bool 的 false 在调用方很容易被忽略或误读成
// "验签没通过就算了"，而回调接口本身没有鉴权，验签是唯一信任来源。
// 未接入易宝时这里没有公钥可验，必须明确报错阻断，不得放行。
func (y *YeepayClient) VerifyCallback(req CallbackReq) error {
	// TODO: 接入后用易宝公钥验证回调报文 RSA 签名，并校验 amount 与本地订单一致
	return ErrYeepayCallbackUnverifiable
}

func (y *YeepayClient) CreateSplit(orderNo string, settlements []SplitItem) error {
	return ErrYeepayNotConfigured
}

var ErrYeepayNotConfigured = fmt.Errorf("易宝支付未接入: 需配置 merchant_no + RSA私钥 + API地址")

// ErrYeepayCallbackUnverifiable 支付回调无法验签时返回。回调接口无鉴权，
// 放行未验签的回调等于允许任何人伪造支付成功并触发分账，因此一律拒绝。
var ErrYeepayCallbackUnverifiable = fmt.Errorf("支付回调无法验签，已拒绝: 需配置易宝公钥后方可处理回调（未接入期间平台不接受任何支付回调）")

type SplitItem struct {
	PayeeType string
	PayeeID   int64
	Amount    int64 // fen
}
