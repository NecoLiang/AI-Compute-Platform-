package payment

import (
	"fmt"
	"time"
	"github.com/jmoiron/sqlx"
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
	if err != nil { return "not_started", nil }
	return row.Status, nil
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

// Mock Yeepay API (replace with real YeePay SDK in production)
type YeepayClient struct{}

func NewYeepayClient() *YeepayClient { return &YeepayClient{} }

func (y *YeepayClient) CreatePayment(orderNo string, amount int64, channel string) (payURL string, txID string, err error) {
	txID = fmt.Sprintf("YP%s%d", orderNo, time.Now().UnixNano())
	payURL = fmt.Sprintf("https://yeepay-sandbox/pay?tx=%s&amount=%d", txID, amount)
	return payURL, txID, nil
}

func (y *YeepayClient) VerifyCallback(data map[string]string) bool {
	// Verify RSA signature in production
	return true
}

func (y *YeepayClient) CreateSplit(orderNo string, settlements []SplitItem) error {
	// Call Yeepay split API in production
	return nil
}

type SplitItem struct {
	PayeeType string
	PayeeID   int64
	Amount    int64 // fen
}
