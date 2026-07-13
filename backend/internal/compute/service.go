package compute

import (
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

type Service struct {
	repo      *Repository
	db        *sqlx.DB
	feeRate   int64 // platform fee rate in basis points, default 500 = 5%
}

func NewService(repo *Repository, db *sqlx.DB) *Service {
	return &Service{repo: repo, db: db, feeRate: 500}
}

// ===== Qualifications (T-009, T-010) =====

func (s *Service) SubmitQualification(userID int64, qualType, certName, certNumber, certURL string, expiresAt *time.Time) (int64, error) {
	q := &SupplierQualification{
		UserID:    userID,
		QualType:  qualType,
		CertName:  certName,
		CertNumber: certNumber,
		CertURL:   certURL,
		ExpiresAt: expiresAt,
	}
	return s.repo.CreateQualification(q)
}

func (s *Service) GetMyQualifications(userID int64) ([]SupplierQualification, error) {
	return s.repo.GetQualificationsByUser(userID)
}

func (s *Service) ApproveQualification(id int64) error {
	return s.repo.UpdateQualificationStatus(id, "verified", "")
}

func (s *Service) RejectQualification(id int64, reason string) error {
	return s.repo.UpdateQualificationStatus(id, "rejected", reason)
}

func (s *Service) GetPendingQualifications() ([]SupplierQualification, error) {
	return s.repo.GetPendingQualifications()
}

// ===== Products (T-011, T-012) =====

type CreateProductReq struct {
	GpuModel        string `json:"gpu_model"`
	CardCount       int    `json:"card_count"`
	CpuSpec         string `json:"cpu_spec"`
	MemorySpec      string `json:"memory_spec"`
	StorageSpec     string `json:"storage_spec"`
	BandwidthSpec   string `json:"bandwidth_spec"`
	DeliveryMode    string `json:"delivery_mode"`
	PricingMode     string `json:"pricing_mode"`
	UnitPrice       int64  `json:"unit_price"` // fen per card per hour
	AvailableHours  string `json:"available_hours"`
	Stock           int    `json:"stock"`
	MinOrder        int    `json:"min_order"`
	MinDuration     int    `json:"min_duration"`
	Region          string `json:"region"`
	ComplianceAgreed bool  `json:"compliance_agreed"`
}

func (s *Service) CreateProduct(supplierID int64, req CreateProductReq) (int64, error) {
	p := &Product{
		SupplierID:       supplierID,
		GpuModel:         req.GpuModel,
		CardCount:        req.CardCount,
		CpuSpec:          req.CpuSpec,
		MemorySpec:       req.MemorySpec,
		StorageSpec:      req.StorageSpec,
		BandwidthSpec:    req.BandwidthSpec,
		DeliveryMode:     req.DeliveryMode,
		PricingMode:      req.PricingMode,
		UnitPrice:        req.UnitPrice,
		AvailableHours:   req.AvailableHours,
		Stock:            req.Stock,
		MinOrder:         req.MinOrder,
		MinDuration:      req.MinDuration,
		Region:           req.Region,
		ComplianceAgreed: req.ComplianceAgreed,
	}
	return s.repo.CreateProduct(p)
}

func (s *Service) GetProduct(id int64) (*Product, *CreditScore, error) {
	p, err := s.repo.GetProductByID(id)
	if err != nil { return nil, nil, err }
	credit, _ := s.repo.GetCreditScore(p.SupplierID)
	return p, credit, nil
}

func (s *Service) ListProducts(f ProductFilter) ([]Product, int64, error) {
	return s.repo.ListProducts(f)
}

func (s *Service) GetSupplierProducts(supplierID int64) ([]Product, error) {
	return s.repo.GetProductsBySupplier(supplierID)
}

func (s *Service) ApproveProduct(id int64) error {
	return s.repo.UpdateProductStatus(id, "active")
}

func (s *Service) RejectProduct(id int64) error {
	return s.repo.UpdateProductStatus(id, "draft")
}

func (s *Service) OfflineProduct(id int64) error {
	return s.repo.UpdateProductStatus(id, "offline")
}

// ===== Orders (T-015, T-016) =====

type PlaceOrderReq struct {
	ProductID        int64 `json:"product_id"`
	Quantity         int   `json:"quantity"`
	Duration         int   `json:"duration"` // hours
	ComplianceAgreed bool  `json:"compliance_agreed"`
}

func (s *Service) PlaceOrder(buyerID int64, req PlaceOrderReq) (*Order, error) {
	p, err := s.repo.GetProductByID(req.ProductID)
	if err != nil { return nil, fmt.Errorf("product not found") }
	if p.Status != "active" { return nil, fmt.Errorf("product not available") }

	expires := time.Now().Add(15 * time.Minute)
	totalFen := p.UnitPrice * int64(req.Quantity) * int64(req.Duration)
	feeFen := totalFen * s.feeRate / 10000

	orderNo := "ORD" + time.Now().Format("20060102150405") + uuid.New().String()[:6]

	o := &Order{
		OrderNo:         orderNo,
		BuyerID:         buyerID,
		ProductID:       req.ProductID,
		Quantity:        req.Quantity,
		Duration:        req.Duration,
		UnitPrice:       p.UnitPrice,
		TotalAmount:     totalFen,
		PlatformFee:     feeFen,
		PaymentExpires:  &expires,
		ComplianceAgreed: req.ComplianceAgreed,
	}

	tx, err := s.db.Beginx()
	if err != nil { return nil, err }
	defer tx.Rollback()

	if err := s.repo.DecrProductStock(tx, p.ID, req.Quantity); err != nil {
		return nil, fmt.Errorf("insufficient stock")
	}
	if err := s.repo.CreateOrderTx(tx, o); err != nil {
		return nil, err
	}
	return o, tx.Commit()
}

func (s *Service) GetOrder(orderNo string) (*Order, error) {
	o, err := s.repo.GetOrderByNo(orderNo)
	if err != nil { return nil, err }
	if o == nil { return nil, fmt.Errorf("order not found") }
	return o, nil
}

func (s *Service) GetOrderByID(id int64) (*Order, error) {
	return s.repo.GetOrderByID(id)
}

func (s *Service) PayOrder(orderNo string) error {
	return s.repo.UpdateOrderStatus(orderNo, "paid")
}

// Provisioning: supplier confirms they're setting up
func (s *Service) ProvisioningOrder(orderNo string) error {
	return s.repo.UpdateOrderStatus(orderNo, "provisioning")
}

// Active: buyer confirmed delivery
func (s *Service) ActivateOrder(orderNo string) error {
	now := time.Now()
	o, _ := s.repo.GetOrderByNo(orderNo)
	if o == nil { return fmt.Errorf("order not found") }
	end := now.Add(time.Duration(o.Duration) * time.Hour)
	tx, _ := s.db.Beginx()
	defer tx.Rollback()
	s.repo.UpdateOrderStatusTx(tx, orderNo, "active")
	tx.Exec("UPDATE orders SET lease_start_at=?, lease_end_at=? WHERE order_no=?", now, end, orderNo)
	return tx.Commit()
}

func (s *Service) CompleteOrder(orderNo string) error {
	return s.repo.UpdateOrderStatus(orderNo, "completed")
}

func (s *Service) CancelOrder(orderNo string) error {
	return s.repo.UpdateOrderStatus(orderNo, "cancelled")
}

func (s *Service) FreezeOrder(orderNo string) error {
	return s.repo.UpdateOrderStatus(orderNo, "frozen")
}

func (s *Service) ListBuyerOrders(buyerID int64, status string, page, pageSize int) ([]Order, int64, error) {
	return s.repo.ListBuyerOrders(buyerID, status, page, pageSize)
}

func (s *Service) ListSupplierOrders(supplierID int64, status string, page, pageSize int) ([]Order, int64, error) {
	return s.repo.ListSupplierOrders(supplierID, status, page, pageSize)
}

// ===== Delivery (T-017) =====

func (s *Service) Deliver(orderNo string, encryptedCredential string) error {
	o, err := s.repo.GetOrderByNo(orderNo)
	if err != nil || o == nil { return fmt.Errorf("order not found") }
	d := &OrderDelivery{OrderID: o.ID, CredentialEncrypted: encryptedCredential}
	if err := s.repo.CreateDelivery(d); err != nil { return err }
	return s.repo.UpdateOrderStatus(orderNo, "provisioning")
}

func (s *Service) ConfirmDelivery(orderNo string) error {
	o, _ := s.repo.GetOrderByNo(orderNo)
	if o == nil { return fmt.Errorf("order not found") }
	if err := s.repo.ConfirmDelivery(o.ID); err != nil { return err }
	return s.ActivateOrder(orderNo)
}

func (s *Service) GetDelivery(orderID int64) (*OrderDelivery, error) {
	return s.repo.GetDeliveryByOrder(orderID)
}

// ===== Renewal (T-018) =====

func (s *Service) RenewOrder(buyerID int64, orderNo string, additionalDuration int) (*Order, error) {
	o, err := s.repo.GetOrderByNo(orderNo)
	if err != nil || o == nil { return nil, fmt.Errorf("order not found") }
	if o.Status != "active" { return nil, fmt.Errorf("order not active") }

	totalFen := o.UnitPrice * int64(o.Quantity) * int64(additionalDuration)
	feeFen := totalFen * s.feeRate / 10000
	newOrderNo := "REN" + time.Now().Format("20060102150405") + uuid.New().String()[:6]
	expires := time.Now().Add(15 * time.Minute)

	no := &Order{
		OrderNo:         newOrderNo,
		BuyerID:         buyerID,
		ProductID:       o.ProductID,
		Quantity:        o.Quantity,
		Duration:        additionalDuration,
		UnitPrice:       o.UnitPrice,
		TotalAmount:     totalFen,
		PlatformFee:     feeFen,
		PaymentExpires:  &expires,
		ComplianceAgreed: true,
	}
	return no, s.repo.CreateOrderTx(nil, no)
}

// ===== Refund (T-019) =====

func (s *Service) RequestRefund(orderNo string) error {
	o, _ := s.repo.GetOrderByNo(orderNo)
	if o == nil { return fmt.Errorf("order not found") }
	if o.Status != "active" && o.Status != "paid" && o.Status != "provisioning" {
		return fmt.Errorf("invalid status transition")
	}
	return s.repo.UpdateOrderStatus(orderNo, "refunding")
}

func (s *Service) CompleteRefund(orderNo string) error {
	return s.repo.UpdateOrderStatus(orderNo, "refunded")
}

// ===== Credit Score (T-023) =====

func (s *Service) UpdateCreditScore(supplierID int64, fulfillRate, slaRate float64, violations int) error {
	return s.repo.UpsertCreditScore(supplierID, fulfillRate, slaRate, violations)
}

func (s *Service) GetCreditScore(supplierID int64) (*CreditScore, error) {
	return s.repo.GetCreditScore(supplierID)
}

// ===== Admin =====

func (s *Service) ListAllOrders(status string, page, pageSize int) ([]Order, int64, error) {
	return s.repo.ListAllOrders(status, page, pageSize)
}

func (s *Service) ListAllProducts(status string, page, pageSize int) ([]Product, int64, error) {
	return s.repo.ListAllProducts(status, page, pageSize)
}

func (s *Service) AdminUpdateOrderStatus(orderNo string, status string) error {
	return s.repo.UpdateOrderStatus(orderNo, status)
}
