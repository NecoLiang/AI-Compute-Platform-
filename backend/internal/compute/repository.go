package compute

import (
	"database/sql"
	"fmt"
	"time"
	"tokenfactory/pkg/errcode"
	"github.com/jmoiron/sqlx"
)

// ===== Models =====

type SupplierQualification struct {
	ID             int64     `db:"id" json:"id"`
	UserID         int64     `db:"user_id" json:"user_id"`
	QualType       string    `db:"qual_type" json:"qual_type"`
	CertName       string    `db:"cert_name" json:"cert_name"`
	CertNumber     string    `db:"cert_number" json:"cert_number"`
	CertURL        string    `db:"cert_url" json:"cert_url"`
	ExpiresAt      *time.Time `db:"expires_at" json:"expires_at"`
	Status         string    `db:"status" json:"status"`
	RejectedReason string    `db:"rejected_reason" json:"rejected_reason,omitempty"`
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
}

type Product struct {
	ID              int64     `db:"id" json:"id"`
	SupplierID      int64     `db:"supplier_id" json:"supplier_id"`
	GpuModel        string    `db:"gpu_model" json:"gpu_model"`
	CardCount       int       `db:"card_count" json:"card_count"`
	CpuSpec         string    `db:"cpu_spec" json:"cpu_spec"`
	MemorySpec      string    `db:"memory_spec" json:"memory_spec"`
	StorageSpec     string    `db:"storage_spec" json:"storage_spec"`
	BandwidthSpec   string    `db:"bandwidth_spec" json:"bandwidth_spec"`
	DeliveryMode    string    `db:"delivery_mode" json:"delivery_mode"`
	PricingMode     string    `db:"pricing_mode" json:"pricing_mode"`
	UnitPrice       int64     `db:"unit_price" json:"unit_price"`       // fen
	AvailableHours  string    `db:"available_hours" json:"available_hours"`
	Stock           int       `db:"stock" json:"stock"`
	MinOrder        int       `db:"min_order" json:"min_order"`
	MinDuration     int       `db:"min_duration" json:"min_duration"`
	Region          string    `db:"region" json:"region"`
	Status          string    `db:"status" json:"status"`
	SelfOperated    bool      `db:"self_operated" json:"self_operated"`
	ComplianceAgreed bool     `db:"compliance_agreed" json:"compliance_agreed"`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time `db:"updated_at" json:"updated_at"`
}

type Order struct {
	ID              int64      `db:"id" json:"id"`
	OrderNo         string     `db:"order_no" json:"order_no"`
	BuyerID         int64      `db:"buyer_id" json:"buyer_id"`
	ProductID       int64      `db:"product_id" json:"product_id"`
	Quantity        int        `db:"quantity" json:"quantity"`
	Duration        int        `db:"duration" json:"duration"`
	UnitPrice       int64      `db:"unit_price" json:"unit_price"`
	TotalAmount     int64      `db:"total_amount" json:"total_amount"`
	PlatformFee     int64      `db:"platform_fee" json:"platform_fee"`
	Status          string     `db:"status" json:"status"`
	PaymentExpires  *time.Time `db:"payment_expires_at" json:"payment_expires_at"`
	LeaseStart      *time.Time `db:"lease_start_at" json:"lease_start_at"`
	LeaseEnd        *time.Time `db:"lease_end_at" json:"lease_end_at"`
	ComplianceAgreed bool      `db:"compliance_agreed" json:"compliance_agreed"`
	CreatedAt       time.Time  `db:"created_at" json:"created_at"`
}

type OrderDelivery struct {
	ID                int64      `db:"id" json:"id"`
	OrderID           int64      `db:"order_id" json:"order_id"`
	CredentialEncrypted string   `db:"credential_encrypted" json:"-"`
	ConfirmedByBuyer  bool       `db:"confirmed_by_buyer" json:"confirmed_by_buyer"`
	BuyerConfirmedAt  *time.Time `db:"buyer_confirmed_at" json:"buyer_confirmed_at"`
}

type CreditScore struct {
	SupplierID     int64     `db:"supplier_id" json:"supplier_id"`
	FulfillRate    float64   `db:"fulfill_rate" json:"fulfill_rate"`
	SlaRate        float64   `db:"sla_rate" json:"sla_rate"`
	ViolationCount int       `db:"violation_count" json:"violation_count"`
	TotalOrders    int       `db:"total_orders" json:"total_orders"`
	UpdatedAt      time.Time `db:"updated_at" json:"updated_at"`
}

// ===== Repository =====

type Repository struct {
	db *sqlx.DB
}

func NewRepository(db *sqlx.DB) *Repository {
	return &Repository{db: db}
}

// Qualifications
func (r *Repository) CreateQualification(q *SupplierQualification) (int64, error) {
	res, err := r.db.Exec(
		"INSERT INTO supplier_qualifications (user_id, qual_type, cert_name, cert_number, cert_url, expires_at, status) VALUES (?,?,?,?,?,?,?)",
		q.UserID, q.QualType, q.CertName, q.CertNumber, q.CertURL, q.ExpiresAt, "pending",
	)
	if err != nil { return 0, err }
	return res.LastInsertId()
}

func (r *Repository) GetQualificationsByUser(userID int64) ([]SupplierQualification, error) {
	var list []SupplierQualification
	err := r.db.Select(&list, "SELECT * FROM supplier_qualifications WHERE user_id = ?", userID)
	return list, err
}

func (r *Repository) GetQualificationByID(id int64) (*SupplierQualification, error) {
	var q SupplierQualification
	err := r.db.Get(&q, "SELECT * FROM supplier_qualifications WHERE id = ?", id)
	return &q, err
}

func (r *Repository) UpdateQualificationStatus(id int64, status, reason string) error {
	_, err := r.db.Exec("UPDATE supplier_qualifications SET status=?, rejected_reason=? WHERE id=?", status, reason, id)
	return err
}

func (r *Repository) GetPendingQualifications() ([]SupplierQualification, error) {
	var list []SupplierQualification
	err := r.db.Select(&list, "SELECT * FROM supplier_qualifications WHERE status='pending' ORDER BY created_at")
	return list, err
}

// Products
func (r *Repository) CreateProduct(p *Product) (int64, error) {
	res, err := r.db.Exec(
		`INSERT INTO products (supplier_id, gpu_model, card_count, cpu_spec, memory_spec, storage_spec, bandwidth_spec,
		delivery_mode, pricing_mode, unit_price, available_hours, stock, min_order, min_duration, region, status, compliance_agreed)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'pending',?)`,
		p.SupplierID, p.GpuModel, p.CardCount, p.CpuSpec, p.MemorySpec, p.StorageSpec, p.BandwidthSpec,
		p.DeliveryMode, p.PricingMode, p.UnitPrice, p.AvailableHours, p.Stock, p.MinOrder, p.MinDuration, p.Region, p.ComplianceAgreed,
	)
	if err != nil { return 0, err }
	return res.LastInsertId()
}

func (r *Repository) GetProductByID(id int64) (*Product, error) {
	var p Product
	err := r.db.Get(&p, "SELECT * FROM products WHERE id = ?", id)
	return &p, err
}

func (r *Repository) UpdateProductStatus(id int64, status string) error {
	_, err := r.db.Exec("UPDATE products SET status=? WHERE id=?", status, id)
	return err
}

func (r *Repository) DecrProductStock(tx *sqlx.Tx, id int64, qty int) error {
	res, err := tx.Exec("UPDATE products SET stock = stock - ? WHERE id = ? AND stock >= ?", qty, id, qty)
	if err != nil { return err }
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("insufficient stock")
	}
	// Auto-mark sold_out
	tx.Exec("UPDATE products SET status='sold_out' WHERE id=? AND stock <= 0", id)
	return nil
}

func (r *Repository) IncrProductStock(tx *sqlx.Tx, id int64, qty int) error {
	_, err := tx.Exec("UPDATE products SET stock = stock + ?, status='active' WHERE id = ?", qty, id)
	return err
}

type ProductFilter struct {
	GpuModel    string
	Region      string
	PricingMode string
	PriceMin    int64
	PriceMax    int64
	Page        int
	PageSize    int
	Sort        string
}

func (r *Repository) ListProducts(f ProductFilter) ([]Product, int64, error) {
	where := "WHERE status='active'"
	args := []interface{}{}
	if f.GpuModel != "" {
		where += " AND gpu_model=?"
		args = append(args, f.GpuModel)
	}
	if f.Region != "" {
		where += " AND region=?"
		args = append(args, f.Region)
	}
	if f.PricingMode != "" {
		where += " AND pricing_mode=?"
		args = append(args, f.PricingMode)
	}
	if f.PriceMin > 0 {
		where += " AND unit_price >= ?"
		args = append(args, f.PriceMin)
	}
	if f.PriceMax > 0 {
		where += " AND unit_price <= ?"
		args = append(args, f.PriceMax)
	}

	var total int64
	r.db.Get(&total, "SELECT COUNT(*) FROM products "+where, args...)

	order := "ORDER BY created_at DESC"
	switch f.Sort {
	case "price_asc": order = "ORDER BY unit_price ASC"
	case "price_desc": order = "ORDER BY unit_price DESC"
	}

	if f.Page <= 0 { f.Page = 1 }
	if f.PageSize <= 0 { f.PageSize = 20 }
	offset := (f.Page - 1) * f.PageSize

	query := fmt.Sprintf("SELECT * FROM products %s %s LIMIT ? OFFSET ?", where, order)
	args = append(args, f.PageSize, offset)
	var list []Product
	err := r.db.Select(&list, query, args...)
	return list, total, err
}

func (r *Repository) GetProductsBySupplier(supplierID int64) ([]Product, error) {
	var list []Product
	err := r.db.Select(&list, "SELECT * FROM products WHERE supplier_id = ? ORDER BY created_at DESC", supplierID)
	return list, err
}

// Orders
func (r *Repository) CreateOrderTx(tx *sqlx.Tx, o *Order) error {
	_, err := tx.Exec(
		`INSERT INTO orders (order_no, buyer_id, product_id, quantity, duration, unit_price, total_amount, platform_fee,
		status, payment_expires_at, compliance_agreed) VALUES (?,?,?,?,?,?,?,?,'pending_payment',?,?)`,
		o.OrderNo, o.BuyerID, o.ProductID, o.Quantity, o.Duration, o.UnitPrice, o.TotalAmount, o.PlatformFee,
		o.PaymentExpires, o.ComplianceAgreed,
	)
	return err
}

func (r *Repository) GetOrderByNo(orderNo string) (*Order, error) {
	var o Order
	err := r.db.Get(&o, "SELECT * FROM orders WHERE order_no = ?", orderNo)
	if err == sql.ErrNoRows { return nil, nil }
	return &o, err
}

func (r *Repository) GetOrderByID(id int64) (*Order, error) {
	var o Order
	err := r.db.Get(&o, "SELECT * FROM orders WHERE id = ?", id)
	return &o, err
}

func (r *Repository) UpdateOrderStatusTx(tx *sqlx.Tx, orderNo string, status string) error {
	var err error
	if tx != nil {
		_, err = tx.Exec("UPDATE orders SET status=? WHERE order_no=?", status, orderNo)
	} else {
		_, err = r.db.Exec("UPDATE orders SET status=? WHERE order_no=?", status, orderNo)
	}
	return err
}

func (r *Repository) UpdateOrderStatus(orderNo string, status string) error {
	return r.UpdateOrderStatusTx(nil, orderNo, status)
}

type OrderListFilter struct {
	BuyerID  int64
	SupplierID int64
	Status   string
	Page     int
	PageSize int
}

func (r *Repository) ListBuyerOrders(buyerID int64, status string, page, pageSize int) ([]Order, int64, error) {
	return r.listOrders("buyer_id", buyerID, status, page, pageSize)
}

func (r *Repository) ListSupplierOrders(supplierID int64, status string, page, pageSize int) ([]Order, int64, error) {
	// Join with products
	where := "WHERE p.supplier_id=?"
	args := []interface{}{supplierID}
	if status != "" {
		where += " AND o.status=?"
		args = append(args, status)
	}
	var total int64
	r.db.Get(&total, "SELECT COUNT(*) FROM orders o JOIN products p ON o.product_id=p.id "+where, args...)

	if page <= 0 { page = 1 }
	if pageSize <= 0 { pageSize = 20 }
	query := fmt.Sprintf("SELECT o.* FROM orders o JOIN products p ON o.product_id=p.id %s ORDER BY o.created_at DESC LIMIT ? OFFSET ?", where)
	args = append(args, pageSize, (page-1)*pageSize)
	var list []Order
	err := r.db.Select(&list, query, args...)
	return list, total, err
}

func (r *Repository) listOrders(field string, id int64, status string, page, pageSize int) ([]Order, int64, error) {
	where := fmt.Sprintf("WHERE %s=?", field)
	args := []interface{}{id}
	if status != "" {
		where += " AND status=?"
		args = append(args, status)
	}
	var total int64
	r.db.Get(&total, "SELECT COUNT(*) FROM orders "+where, args...)

	if page <= 0 { page = 1 }
	if pageSize <= 0 { pageSize = 20 }
	query := fmt.Sprintf("SELECT * FROM orders %s ORDER BY created_at DESC LIMIT ? OFFSET ?", where)
	args = append(args, pageSize, (page-1)*pageSize)
	var list []Order
	err := r.db.Select(&list, query, args...)
	return list, total, err
}

// Delivery
func (r *Repository) CreateDelivery(d *OrderDelivery) error {
	_, err := r.db.Exec(
		"INSERT INTO order_deliveries (order_id, credential_encrypted) VALUES (?,?) ON DUPLICATE KEY UPDATE credential_encrypted=?",
		d.OrderID, d.CredentialEncrypted, d.CredentialEncrypted,
	)
	return err
}

func (r *Repository) GetDeliveryByOrder(orderID int64) (*OrderDelivery, error) {
	var d OrderDelivery
	err := r.db.Get(&d, "SELECT * FROM order_deliveries WHERE order_id = ?", orderID)
	return &d, err
}

func (r *Repository) ConfirmDelivery(orderID int64) error {
	_, err := r.db.Exec(
		"UPDATE order_deliveries SET confirmed_by_buyer=1, buyer_confirmed_at=NOW() WHERE order_id=?",
		orderID,
	)
	return err
}

// Credit
func (r *Repository) UpsertCreditScore(supplierID int64, fulfillRate, slaRate float64, violations int) error {
	_, err := r.db.Exec(
		`INSERT INTO credit_scores (supplier_id, fulfill_rate, sla_rate, violation_count, total_orders)
		VALUES (?,?,?,?,1) ON DUPLICATE KEY UPDATE
		fulfill_rate=?, sla_rate=?, violation_count=?, total_orders=total_orders+1`,
		supplierID, fulfillRate, slaRate, violations, fulfillRate, slaRate, violations,
	)
	return err
}

func (r *Repository) GetCreditScore(supplierID int64) (*CreditScore, error) {
	var c CreditScore
	err := r.db.Get(&c, "SELECT * FROM credit_scores WHERE supplier_id = ?", supplierID)
	if err == sql.ErrNoRows {
		return &CreditScore{SupplierID: supplierID, FulfillRate: 100, SlaRate: 100}, nil
	}
	return &c, err
}

// All orders (admin)
func (r *Repository) ListAllOrders(status string, page, pageSize int) ([]Order, int64, error) {
	where := ""
	args := []interface{}{}
	if status != "" {
		where = "WHERE status=?"
		args = append(args, status)
	}
	var total int64
	r.db.Get(&total, "SELECT COUNT(*) FROM orders "+where, args...)
	if page <= 0 { page = 1 }
	if pageSize <= 0 { pageSize = 20 }
	query := fmt.Sprintf("SELECT * FROM orders %s ORDER BY created_at DESC LIMIT ? OFFSET ?", where)
	args = append(args, pageSize, (page-1)*pageSize)
	var list []Order
	err := r.db.Select(&list, query, args...)
	return list, total, err
}

// Admin products
func (r *Repository) ListAllProducts(status string, page, pageSize int) ([]Product, int64, error) {
	where := ""
	args := []interface{}{}
	if status != "" {
		where = "WHERE status=?"
		args = append(args, status)
	}
	var total int64
	r.db.Get(&total, "SELECT COUNT(*) FROM products "+where, args...)
	if page <= 0 { page = 1 }
	if pageSize <= 0 { pageSize = 20 }
	query := fmt.Sprintf("SELECT * FROM products %s ORDER BY created_at DESC LIMIT ? OFFSET ?", where)
	args = append(args, pageSize, (page-1)*pageSize)
	var list []Product
	err := r.db.Select(&list, query, args...)
	return list, total, err
}

func ErrToCode(err error) int {
	switch err.Error() {
	case "insufficient stock": return errcode.Conflict
	case "product not found": return errcode.NotFound
	case "order not found": return errcode.NotFound
	case "invalid status transition": return errcode.ParamInvalid
	default: return errcode.InternalError
	}
}
