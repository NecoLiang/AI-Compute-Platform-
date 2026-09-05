package payment

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/jmoiron/sqlx"
	"net/url"
	"strings"
	"time"
	"tokenfactory/internal/compute"
)

type Service struct {
	repo    *Repository
	db      *sqlx.DB
	yeepay  Gateway
	feeRate int64 // basis points
}

// Gateway is the external payment boundary. Implementations must verify the
// provider signature and make payment/split requests idempotent by orderNo.
type Gateway interface {
	CreatePayment(orderNo string, amount int64, channel string) (string, string, error)
	VerifyCallback(CallbackReq) error
	CreateSplit(orderNo string, settlements []SplitItem) error
}

func NewService(repo *Repository, db *sqlx.DB, gateway ...Gateway) *Service {
	var client Gateway = NewYeepayClient()
	if len(gateway) > 0 && gateway[0] != nil {
		client = gateway[0]
	}
	return &Service{repo: repo, db: db, yeepay: client, feeRate: 500}
}

type PayReq struct {
	OrderNo string `json:"order_no"`
	Channel string `json:"channel"` // wechat/alipay/bank
}

type PayResp struct {
	PayURL string `json:"pay_url"`
	TxID   string `json:"tx_id"`
}

// T-027: Create payment order
func (s *Service) Pay(buyerID int64, req PayReq) (*PayResp, error) {
	if err := compute.NewRepository(s.db).RequireTradingAccess(buyerID, "buyer"); err != nil {
		return nil, err
	}
	if strings.TrimSpace(req.OrderNo) == "" || (req.Channel != "wechat" && req.Channel != "alipay" && req.Channel != "bank") {
		return nil, fmt.Errorf("请选择有效订单和支付方式")
	}
	tx, err := s.db.Beginx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	var order payableOrder
	if err := tx.Get(&order, `SELECT buyer_id,total_amount,status,payment_expires_at FROM orders WHERE order_no=? FOR UPDATE`, req.OrderNo); err != nil {
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
	payURL, txID, err := s.yeepay.CreatePayment(req.OrderNo, order.Amount, req.Channel)
	if err != nil {
		return nil, err
	}
	cashier, urlErr := url.Parse(payURL)
	if urlErr != nil || cashier.Scheme != "https" || cashier.Host == "" || cashier.User != nil || txID == "" {
		return nil, fmt.Errorf("payment provider returned an invalid checkout")
	}
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

var ErrPaymentConflict = errors.New("订单已支付、已关闭或超过支付期限，请刷新订单")

// T-031: Handle payment callback
type CallbackReq struct {
	TxID    string `json:"tx_id"`
	OrderNo string `json:"order_no"`
	Status  string `json:"status"` // success/fail
	Amount  int64  `json:"amount"`
}

// HandleCallback 处理易宝支付异步回调。
//
// 🔴 安全铁律：回调接口无鉴权（由第三方服务器直连调用），因此**验签是唯一的信任来源**，
// 必须先于任何业务处理执行。易宝尚未接入时没有可用公钥来验签，因此直接拒绝全部回调 ——
// 绝不能因为"还没接支付"就放行：那等于任何人知道 order_no 就能把订单刷成已支付并触发分账。
func (s *Service) HandleCallback(req CallbackReq) error {
	if err := s.yeepay.VerifyCallback(req); err != nil {
		return err
	}
	if req.Status != "success" || req.Amount <= 0 || req.TxID == "" || req.OrderNo == "" {
		return fmt.Errorf("payment callback is not a successful payment")
	}
	tx, err := s.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var order struct {
		payableOrder
		SupplierID int64 `db:"supplier_id"`
		Fee        int64 `db:"platform_fee"`
	}
	if err := tx.Get(&order, `SELECT o.buyer_id,o.total_amount,o.platform_fee,o.status,o.payment_expires_at,p.supplier_id
        FROM orders o JOIN products p ON p.id=o.product_id WHERE o.order_no=? FOR UPDATE`, req.OrderNo); err != nil {
		return err
	}
	var record Payment
	if err := tx.Get(&record, "SELECT * FROM payments WHERE yeepay_tx_id=? FOR UPDATE", req.TxID); err != nil {
		return err
	}
	if record.OrderNo != req.OrderNo || record.Amount != req.Amount || order.Amount != req.Amount {
		return fmt.Errorf("payment callback does not match the stored order and payment")
	}
	if record.Status != "paid" {
		if record.Status != "pending" || order.Status != "pending_payment" || order.ExpiresAt == nil || !order.ExpiresAt.After(time.Now()) {
			return ErrPaymentConflict
		}
		if order.Fee < 0 || order.Fee > order.Amount {
			return fmt.Errorf("invalid stored platform fee")
		}
		if _, err := tx.Exec("UPDATE payments SET status='paid',paid_at=NOW() WHERE id=?", record.ID); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE orders SET status='paid' WHERE order_no=?", req.OrderNo); err != nil {
			return err
		}
		for _, item := range []SplitItem{{PayeeType: "platform", Amount: order.Fee}, {PayeeType: "supplier", PayeeID: order.SupplierID, Amount: order.Amount - order.Fee}} {
			if _, err := tx.Exec(`INSERT INTO settlements (settlement_id,order_no,payee_type,payee_id,amount,status)
                VALUES (?,?,?,?,?,'pending')`, "SET-"+req.OrderNo+"-"+item.PayeeType, req.OrderNo, item.PayeeType, item.PayeeID, item.Amount); err != nil {
				return err
			}
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.CreateSplit(req.OrderNo, req.Amount)
}

// CreateSplit submits persisted settlement amounts. Gateway acceptance is
// processing, not proof that funds have reached the payees.
func (s *Service) CreateSplit(orderNo string, totalAmount int64) error {
	settlements, err := s.repo.GetSettlementsByOrder(orderNo)
	if err != nil {
		return err
	}
	if len(settlements) != 2 {
		return fmt.Errorf("settlement records missing")
	}
	var total int64
	var pending bool
	items := make([]SplitItem, 0, len(settlements))
	for _, item := range settlements {
		total += item.Amount
		pending = pending || item.Status == "pending" || item.Status == "failed"
		items = append(items, SplitItem{PayeeType: item.PayeeType, PayeeID: item.PayeeID, Amount: item.Amount})
	}
	if total != totalAmount {
		return fmt.Errorf("settlement amount mismatch")
	}
	if !pending {
		return nil
	}
	if err := s.yeepay.CreateSplit(orderNo, items); err != nil {
		return err
	}
	_, err = s.db.Exec("UPDATE settlements SET status='processing' WHERE order_no=? AND status IN ('pending','failed')", orderNo)
	return err
}

// T-030: Refund
func (s *Service) Refund(orderNo string) error {
	return s.repo.UpdatePaymentStatus(orderNo, "refunded")
}

// T-032: Daily reconciliation
func (s *Service) Reconcile(date string) (*ReconcileResult, error) {
	platformTotal, err := s.repo.GetDailyTotal(date)
	if err != nil {
		return nil, err
	}
	// TODO: 接入易宝后，通过易宝 API 拉取当日流水进行比对
	return &ReconcileResult{PlatformTotal: platformTotal, YeepayTotal: 0, Diff: platformTotal}, fmt.Errorf("对账未完成: 需接入易宝 API 拉取流水")
}

// T-026: Supplier Yeepay onboard
func (s *Service) StartOnboard(userID int64) error {
	return s.repo.CreateOnboard(userID)
}

func (s *Service) GetOnboardStatus(userID int64) (string, error) {
	return s.repo.GetOnboardStatus(userID)
}

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
	return append(list, *p), nil
}

func (s *Service) GetOrderSettlements(orderNo string) ([]Settlement, error) {
	return s.repo.GetSettlementsByOrder(orderNo)
}

// ListSupplierSettlements 供给方结算流水(按商品归属)。
func (s *Service) ListSupplierSettlements(supplierID int64, status string, page, pageSize int) ([]Settlement, int64, error) {
	if status != "" && status != "pending" && status != "processing" && status != "success" && status != "failed" {
		return nil, 0, fmt.Errorf("invalid status")
	}
	return s.repo.ListSupplierSettlements(supplierID, status, page, pageSize)
}

// SumSupplierSettlements 应结/已分账/待结合计。
func (s *Service) SumSupplierSettlements(supplierID int64) (int64, int64, int64, error) {
	return s.repo.SumSupplierSettlements(supplierID)
}

func (s *Service) ListPaymentsByDate(date string) ([]Payment, error) {
	return s.repo.ListPaymentsByDate(date)
}

var _ = fmt.Sprintf
