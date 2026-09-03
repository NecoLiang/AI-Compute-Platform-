package payment

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type Service struct {
	repo    *Repository
	db      *sqlx.DB
	yeepay  *YeepayClient
	feeRate int64 // basis points
}

func NewService(repo *Repository, db *sqlx.DB) *Service {
	return &Service{repo: repo, db: db, yeepay: NewYeepayClient(), feeRate: 500}
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
func (s *Service) Pay(req PayReq, totalAmount int64) (*PayResp, error) {
	payURL, txID, err := s.yeepay.CreatePayment(req.OrderNo, totalAmount, req.Channel)
	if err != nil {
		return nil, err
	}

	p := &Payment{OrderNo: req.OrderNo, Amount: totalAmount, Channel: req.Channel, YeepayTxID: txID}
	if err := s.repo.CreatePayment(p); err != nil {
		return nil, err
	}

	return &PayResp{PayURL: payURL, TxID: txID}, nil
}

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

	existing, err := s.repo.GetPaymentByYeepayTx(req.TxID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if existing != nil && existing.Status == "paid" {
		return nil // idempotent
	}
	if err := s.repo.UpdatePaymentStatus(req.OrderNo, "paid"); err != nil {
		return err
	}
	// Auto-trigger settlement split
	return s.CreateSplit(req.OrderNo, req.Amount)
}

// T-029: Split settlement (supplier + platform fee)
func (s *Service) CreateSplit(orderNo string, totalAmount int64) error {
	feeAmount := totalAmount * s.feeRate / 10000
	supplierAmount := totalAmount - feeAmount

	// Platform fee
	sid1 := uuid.New().String()
	if err := s.repo.CreateSettlement(&Settlement{SettlementID: sid1, OrderNo: orderNo, PayeeType: "platform", PayeeID: 0, Amount: feeAmount}); err != nil {
		return err
	}

	// Supplier
	sid2 := uuid.New().String()
	if err := s.repo.CreateSettlement(&Settlement{SettlementID: sid2, OrderNo: orderNo, PayeeType: "supplier", PayeeID: 0, Amount: supplierAmount}); err != nil {
		return err
	}

	if err := s.yeepay.CreateSplit(orderNo, []SplitItem{
		{PayeeType: "platform", Amount: feeAmount},
		{PayeeType: "supplier", Amount: supplierAmount},
	}); err != nil {
		return err
	}
	return nil
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

func (s *Service) GetOrderPayments(orderNo string) ([]Payment, error) {
	var list []Payment
	p, err := s.repo.GetPaymentByOrder(orderNo)
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
