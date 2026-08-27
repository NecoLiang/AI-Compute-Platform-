package compute

import (
	"database/sql"
	"errors"
	"fmt"
	"github.com/jmoiron/sqlx"
	"strings"
	"time"
	"tokenfactory/pkg/crypto"
	"tokenfactory/pkg/errcode"
)

// ===== Models =====

type SupplierQualification struct {
	ID             int64      `db:"id" json:"id"`
	UserID         int64      `db:"user_id" json:"user_id"`
	QualType       string     `db:"qual_type" json:"qual_type"`
	CertName       string     `db:"cert_name" json:"cert_name"`
	CertNumber     string     `db:"cert_number" json:"cert_number"`
	CertURL        string     `db:"cert_url" json:"cert_url"`
	ExpiresAt      *time.Time `db:"expires_at" json:"expires_at"`
	Status         string     `db:"status" json:"status"`
	RejectedReason string     `db:"rejected_reason" json:"rejected_reason,omitempty"`
	CreatedAt      time.Time  `db:"created_at" json:"created_at"`
}

// Product 商品。C-01 起支持 4 种租赁范围类型, 见 ProductType* 常量。
// gpu_model / card_count / delivery_mode 在 002 迁移后允许为 NULL(colocation 无设备),
// 读取时统一 COALESCE 成零值以保持既有 API 兼容, 写入时零值转 NULL。
type Product struct {
	ID                int64     `db:"id" json:"id"`
	SupplierID        int64     `db:"supplier_id" json:"supplier_id"`
	ProductType       string    `db:"product_type" json:"product_type"`
	GpuModel          string    `db:"gpu_model" json:"gpu_model"`
	CardCount         int       `db:"card_count" json:"card_count"`
	MachineCount      *int      `db:"machine_count" json:"machine_count"`
	TotalPflopsApprox *string   `db:"total_pflops_approx" json:"total_pflops_approx"`
	PowerCapacityKw   *int      `db:"power_capacity_kw" json:"power_capacity_kw"`
	RackCount         *int      `db:"rack_count" json:"rack_count"`
	CpuSpec           string    `db:"cpu_spec" json:"cpu_spec"`
	MemorySpec        string    `db:"memory_spec" json:"memory_spec"`
	StorageSpec       string    `db:"storage_spec" json:"storage_spec"`
	BandwidthSpec     string    `db:"bandwidth_spec" json:"bandwidth_spec"`
	DeliveryMode      string    `db:"delivery_mode" json:"delivery_mode"`
	PricingMode       string    `db:"pricing_mode" json:"pricing_mode"`
	UnitPrice         int64     `db:"unit_price" json:"unit_price"` // fen
	PriceNegotiable   bool      `db:"price_negotiable" json:"price_negotiable"`
	AvailableHours    string    `db:"available_hours" json:"available_hours"`
	Stock             int       `db:"stock" json:"stock"`
	MinOrder          int       `db:"min_order" json:"min_order"`
	MinDuration       int       `db:"min_duration" json:"min_duration"`
	Region            string    `db:"region" json:"region"`
	Status            string    `db:"status" json:"status"`
	SelfOperated      bool      `db:"self_operated" json:"self_operated"`
	ComplianceAgreed  bool      `db:"compliance_agreed" json:"compliance_agreed"`
	CreatedAt         time.Time `db:"created_at" json:"created_at"`
	UpdatedAt         time.Time `db:"updated_at" json:"updated_at"`
}

type Order struct {
	ID               int64      `db:"id" json:"id"`
	OrderNo          string     `db:"order_no" json:"order_no"`
	BuyerID          int64      `db:"buyer_id" json:"buyer_id"`
	ProductID        int64      `db:"product_id" json:"product_id"`
	Quantity         int        `db:"quantity" json:"quantity"`
	Duration         int        `db:"duration" json:"duration"`
	UnitPrice        int64      `db:"unit_price" json:"unit_price"`
	TotalAmount      int64      `db:"total_amount" json:"total_amount"`
	PlatformFee      int64      `db:"platform_fee" json:"platform_fee"`
	Status           string     `db:"status" json:"status"`
	PaymentExpires   *time.Time `db:"payment_expires_at" json:"payment_expires_at"`
	LeaseStart       *time.Time `db:"lease_start_at" json:"lease_start_at"`
	LeaseEnd         *time.Time `db:"lease_end_at" json:"lease_end_at"`
	ComplianceAgreed bool       `db:"compliance_agreed" json:"compliance_agreed"`
	CreatedAt        time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt        time.Time  `db:"updated_at" json:"updated_at"`
}

// BuyerOrder keeps the existing order payload and adds the product/provider
// fields needed by the buyer order table in one query.
type BuyerOrder struct {
	Order
	GPUModel     string `db:"gpu_model" json:"gpu_model"`
	ProductType  string `db:"product_type" json:"product_type"`
	PricingMode  string `db:"pricing_mode" json:"pricing_mode"`
	SelfOperated bool   `db:"self_operated" json:"self_operated"`
	SupplierName string `db:"supplier_name" json:"supplier_name"`
}

// OrderDelivery 交付记录。C-06 起承载访问凭证。
// 加密字段一律 json:"-", 绝不随接口出站。
type OrderDelivery struct {
	ID                   int64      `db:"id" json:"id"`
	OrderID              int64      `db:"order_id" json:"order_id"`
	CredentialEncrypted  string     `db:"credential_encrypted" json:"-"`
	AccessKey            string     `db:"access_key" json:"access_key"`
	AccessValueEncrypted string     `db:"access_value_encrypted" json:"-"`
	AccessStatus         string     `db:"access_status" json:"access_status"`
	AccessExpiresAt      *time.Time `db:"access_expires_at" json:"access_expires_at"`
	RevokedAt            *time.Time `db:"revoked_at" json:"revoked_at"`
	ConfirmedByBuyer     bool       `db:"confirmed_by_buyer" json:"confirmed_by_buyer"`
	BuyerConfirmedAt     *time.Time `db:"buyer_confirmed_at" json:"buyer_confirmed_at"`
	CreatedAt            time.Time  `db:"created_at" json:"created_at"`
}

// ResourceSnapshot 资源盘点快照(C-05)。sync_type: active=平台主动盘点 passive=机房主动上报。
type ResourceSnapshot struct {
	ID          int64     `db:"id" json:"id"`
	ProductID   int64     `db:"product_id" json:"product_id"`
	SupplierID  int64     `db:"supplier_id" json:"supplier_id"`
	SyncType    string    `db:"sync_type" json:"sync_type"`
	StockBefore int       `db:"stock_before" json:"stock_before"`
	StockAfter  int       `db:"stock_after" json:"stock_after"`
	Diff        int       `db:"diff" json:"diff"`
	Reason      string    `db:"reason" json:"reason"`
	OperatorID  int64     `db:"operator_id" json:"operator_id"`
	Anomaly     bool      `db:"anomaly" json:"anomaly"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
}

type CreditScore struct {
	SupplierID     int64     `db:"supplier_id" json:"supplier_id"`
	FulfillRate    float64   `db:"fulfill_rate" json:"fulfill_rate"`
	SlaRate        float64   `db:"sla_rate" json:"sla_rate"`
	ViolationCount int       `db:"violation_count" json:"violation_count"`
	TotalOrders    int       `db:"total_orders" json:"total_orders"`
	UpdatedAt      time.Time `db:"updated_at" json:"updated_at"`
}

// ===== Column lists =====
// 显式列清单替代 SELECT *: 一是可空列必须 COALESCE 成零值(否则 sqlx 扫 NULL 进 string/int 直接报错),
// 二是表新增列时不会因结构体缺字段导致 StructScan 失败。

const productColumns = `id, supplier_id, product_type,
	COALESCE(gpu_model,'') AS gpu_model, COALESCE(card_count,0) AS card_count,
	machine_count, total_pflops_approx, power_capacity_kw, rack_count,
	COALESCE(cpu_spec,'') AS cpu_spec, COALESCE(memory_spec,'') AS memory_spec,
	COALESCE(storage_spec,'') AS storage_spec, COALESCE(bandwidth_spec,'') AS bandwidth_spec,
	COALESCE(delivery_mode,'') AS delivery_mode, pricing_mode, unit_price,
	COALESCE(price_negotiable,0) AS price_negotiable, COALESCE(available_hours,'') AS available_hours,
	stock, COALESCE(min_order,1) AS min_order, COALESCE(min_duration,1) AS min_duration,
	COALESCE(region,'') AS region, status, COALESCE(self_operated,0) AS self_operated,
	COALESCE(compliance_agreed,0) AS compliance_agreed, created_at, updated_at`

const orderColumns = `id, order_no, buyer_id, product_id, quantity, duration, unit_price,
	total_amount, platform_fee, status, payment_expires_at, lease_start_at, lease_end_at,
	COALESCE(compliance_agreed,0) AS compliance_agreed, created_at, updated_at`

const buyerOrderColumns = `o.id, o.order_no, o.buyer_id, o.product_id, o.quantity, o.duration, o.unit_price,
	o.total_amount, o.platform_fee, o.status, o.payment_expires_at, o.lease_start_at, o.lease_end_at,
	COALESCE(o.compliance_agreed,0) AS compliance_agreed, o.created_at, o.updated_at,
	COALESCE(p.gpu_model,'') AS gpu_model, COALESCE(p.product_type,'') AS product_type,
	COALESCE(p.pricing_mode,'') AS pricing_mode,
	COALESCE(p.self_operated,0) AS self_operated,
	CASE WHEN COALESCE(p.self_operated,0)=1 THEN '平台自营' ELSE COALESCE(e.name,'') END AS supplier_name`

const deliveryColumns = `id, order_id, COALESCE(credential_encrypted,'') AS credential_encrypted,
	COALESCE(access_key,'') AS access_key, COALESCE(access_value_encrypted,'') AS access_value_encrypted,
	COALESCE(access_status,'none') AS access_status, access_expires_at, revoked_at,
	COALESCE(confirmed_by_buyer,0) AS confirmed_by_buyer, buyer_confirmed_at, created_at`

const snapshotColumns = `id, product_id, supplier_id, sync_type, stock_before, stock_after, diff,
	COALESCE(reason,'') AS reason, operator_id, COALESCE(anomaly,0) AS anomaly, created_at`

const creditColumns = `supplier_id, fulfill_rate, sla_rate, violation_count, total_orders, updated_at`

// nullString 把空串写成 NULL(colocation 无 gpu_model / delivery_mode)。
func nullString(s string) interface{} {
	if s == "" {
		return nil
	}
	return s
}

// nullInt 把非正数写成 NULL(colocation 无 card_count, 非 center 无 machine_count)。
func nullInt(i int) interface{} {
	if i <= 0 {
		return nil
	}
	return i
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
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// qualificationColumns 显式列 + COALESCE: 可空列(cert_number/cert_url/rejected_reason)
// 若为 NULL 会让 sqlx 扫描 string 报错, 且 handler 吞错后前端只能看到 data:null。
const qualificationColumns = `id, user_id, qual_type, cert_name,
	COALESCE(cert_number,'') AS cert_number, COALESCE(cert_url,'') AS cert_url,
	expires_at, status, COALESCE(rejected_reason,'') AS rejected_reason, created_at`

func (r *Repository) GetQualificationsByUser(userID int64) ([]SupplierQualification, error) {
	var list []SupplierQualification
	err := r.db.Select(&list, "SELECT "+qualificationColumns+" FROM supplier_qualifications WHERE user_id = ? ORDER BY created_at DESC", userID)
	return list, err
}

func (r *Repository) GetQualificationByID(id int64) (*SupplierQualification, error) {
	var q SupplierQualification
	err := r.db.Get(&q, "SELECT "+qualificationColumns+" FROM supplier_qualifications WHERE id = ?", id)
	return &q, err
}

func (r *Repository) UpdateQualificationStatus(id int64, status, reason string) error {
	_, err := r.db.Exec("UPDATE supplier_qualifications SET status=?, rejected_reason=? WHERE id=?", status, reason, id)
	return err
}

func (r *Repository) GetPendingQualifications() ([]SupplierQualification, error) {
	var list []SupplierQualification
	err := r.db.Select(&list, "SELECT "+qualificationColumns+" FROM supplier_qualifications WHERE status='pending' ORDER BY created_at")
	return list, err
}

// Products
func (r *Repository) CreateProduct(p *Product) (int64, error) {
	res, err := r.db.Exec(
		`INSERT INTO products (supplier_id, product_type, gpu_model, card_count, machine_count, total_pflops_approx,
		power_capacity_kw, rack_count, cpu_spec, memory_spec, storage_spec, bandwidth_spec,
		delivery_mode, pricing_mode, unit_price, price_negotiable, available_hours, stock, min_order, min_duration,
		region, status, compliance_agreed)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,'pending',?)`,
		p.SupplierID, p.ProductType, nullString(p.GpuModel), nullInt(p.CardCount), p.MachineCount, p.TotalPflopsApprox,
		p.PowerCapacityKw, p.RackCount, p.CpuSpec, p.MemorySpec, p.StorageSpec, p.BandwidthSpec,
		nullString(p.DeliveryMode), p.PricingMode, p.UnitPrice, p.PriceNegotiable, p.AvailableHours, p.Stock,
		p.MinOrder, p.MinDuration, p.Region, p.ComplianceAgreed,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (r *Repository) GetProductByID(id int64) (*Product, error) {
	var p Product
	err := r.db.Get(&p, "SELECT "+productColumns+" FROM products WHERE id = ?", id)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("product not found")
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *Repository) UpdateProductStatus(id int64, status string) error {
	_, err := r.db.Exec("UPDATE products SET status=? WHERE id=?", status, id)
	return err
}

func (r *Repository) DecrProductStock(tx *sqlx.Tx, id int64, qty int) error {
	// 双重保险: qty 必须为正, 否则 stock = stock - (负数) 会凭空增加库存并通过 stock >= qty 校验。
	// 上层 ValidateOrderParams 已拦截, 这里在数据层再兜一次底。
	if qty <= 0 {
		return fmt.Errorf("invalid quantity")
	}
	res, err := tx.Exec("UPDATE products SET stock = stock - ? WHERE id = ? AND stock >= ?", qty, id, qty)
	if err != nil {
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		return fmt.Errorf("insufficient stock")
	}
	// Auto-mark sold_out
	tx.Exec("UPDATE products SET status='sold_out' WHERE id=? AND stock <= 0", id)
	return nil
}

func (r *Repository) IncrProductStock(tx *sqlx.Tx, id int64, qty int) error {
	if qty <= 0 {
		return fmt.Errorf("invalid quantity")
	}
	if _, err := tx.Exec("UPDATE products SET stock = stock + ? WHERE id = ?", qty, id); err != nil {
		return err
	}
	// 只把「售罄」恢复成「在售」。不能无条件置 active:
	// 运营强制下架(offline)或风控冻结(frozen)的商品, 不该因为一笔退款/关单就自动重新上架。
	_, err := tx.Exec("UPDATE products SET status='active' WHERE id=? AND status='sold_out' AND stock > 0", id)
	return err
}

// LockProductForUpdate 在事务内用 SELECT ... FOR UPDATE 锁住商品行后读取,
// 供盘点(C-05)读 stock_before 用, 防止并发盘点丢更新。
func (r *Repository) LockProductForUpdate(tx *sqlx.Tx, id int64) (*Product, error) {
	var p Product
	err := tx.Get(&p, "SELECT "+productColumns+" FROM products WHERE id = ? FOR UPDATE", id)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("product not found")
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// SetProductStockTx 在事务内把库存改为绝对值(盘点用)。负库存直接拒绝。
func (r *Repository) SetProductStockTx(tx *sqlx.Tx, id int64, stock int) error {
	if stock < 0 {
		return fmt.Errorf("stock cannot be negative")
	}
	if _, err := tx.Exec("UPDATE products SET stock = ? WHERE id = ?", stock, id); err != nil {
		return err
	}
	var err error
	if stock == 0 {
		_, err = tx.Exec("UPDATE products SET status='sold_out' WHERE id=? AND status='active'", id)
	} else {
		_, err = tx.Exec("UPDATE products SET status='active' WHERE id=? AND status='sold_out'", id)
	}
	return err
}

type ProductFilter struct {
	Query          string
	ProductType    string
	GpuModel       string
	Region         string
	DeliveryMode   string
	PricingMode    string
	AvailableHours string
	PriceMin       int64
	PriceMax       int64
	CardCountMin   int
	Page           int
	PageSize       int
	Sort           string
}

const (
	maxProductPage     = 1000000
	maxProductPageSize = 100
)

func (f *ProductFilter) Normalize() {
	f.Query = strings.TrimSpace(f.Query)
	f.ProductType = strings.TrimSpace(f.ProductType)
	f.GpuModel = strings.TrimSpace(f.GpuModel)
	f.Region = strings.TrimSpace(f.Region)
	f.DeliveryMode = strings.TrimSpace(f.DeliveryMode)
	f.PricingMode = strings.TrimSpace(f.PricingMode)
	f.AvailableHours = strings.TrimSpace(f.AvailableHours)
	if f.Page <= 0 {
		f.Page = 1
	}
	if f.Page > maxProductPage {
		f.Page = maxProductPage
	}
	if f.PageSize <= 0 {
		f.PageSize = 20
	}
	if f.PageSize > maxProductPageSize {
		f.PageSize = maxProductPageSize
	}
}

func (f ProductFilter) buildWhere() (string, []interface{}) {
	where := "WHERE status='active'"
	args := []interface{}{}
	if f.Query != "" {
		pattern := "%" + f.Query + "%"
		where += " AND (gpu_model LIKE ? OR memory_spec LIKE ? OR region LIKE ? OR bandwidth_spec LIKE ? OR available_hours LIKE ?)"
		args = append(args, pattern, pattern, pattern, pattern, pattern)
	}
	if f.ProductType != "" {
		where += " AND product_type=?"
		args = append(args, f.ProductType)
	}
	if f.GpuModel != "" {
		where += " AND gpu_model=?"
		args = append(args, f.GpuModel)
	}
	if f.Region != "" {
		where += " AND region=?"
		args = append(args, f.Region)
	}
	if f.DeliveryMode != "" {
		where += " AND delivery_mode=?"
		args = append(args, f.DeliveryMode)
	}
	if f.PricingMode != "" {
		where += " AND pricing_mode=?"
		args = append(args, f.PricingMode)
	}
	if f.AvailableHours != "" {
		where += " AND available_hours LIKE ?"
		args = append(args, "%"+f.AvailableHours+"%")
	}
	if f.PriceMin > 0 {
		where += " AND price_negotiable=0 AND unit_price >= ?"
		args = append(args, f.PriceMin)
	}
	if f.PriceMax > 0 {
		where += " AND price_negotiable=0 AND unit_price <= ?"
		args = append(args, f.PriceMax)
	}
	if f.CardCountMin > 0 {
		where += " AND card_count >= ?"
		args = append(args, f.CardCountMin)
	}
	return where, args
}

func (f ProductFilter) orderBy() string {
	switch f.Sort {
	case "price_asc":
		return "ORDER BY price_negotiable ASC, unit_price ASC"
	case "price_desc":
		return "ORDER BY price_negotiable ASC, unit_price DESC"
	case "stock_desc":
		return "ORDER BY stock DESC"
	default:
		return "ORDER BY created_at DESC"
	}
}

func (r *Repository) ListProducts(f ProductFilter) ([]Product, int64, error) {
	f.Normalize()
	where, args := f.buildWhere()

	var total int64
	if err := r.db.Get(&total, "SELECT COUNT(*) FROM products "+where, args...); err != nil {
		return nil, 0, err
	}
	offset := (f.Page - 1) * f.PageSize

	query := fmt.Sprintf("SELECT %s FROM products %s %s LIMIT ? OFFSET ?", productColumns, where, f.orderBy())
	args = append(args, f.PageSize, offset)
	var list []Product
	err := r.db.Select(&list, query, args...)
	return list, total, err
}

func (r *Repository) GetProductsBySupplier(supplierID int64) ([]Product, error) {
	var list []Product
	err := r.db.Select(&list, "SELECT "+productColumns+" FROM products WHERE supplier_id = ? ORDER BY product_type, created_at DESC", supplierID)
	return list, err
}

// Orders
func (r *Repository) CreateOrderTx(tx *sqlx.Tx, o *Order) error {
	const q = `INSERT INTO orders (order_no, buyer_id, product_id, quantity, duration, unit_price, total_amount, platform_fee,
		status, payment_expires_at, compliance_agreed) VALUES (?,?,?,?,?,?,?,?,'pending_payment',?,?)`
	args := []interface{}{o.OrderNo, o.BuyerID, o.ProductID, o.Quantity, o.Duration, o.UnitPrice, o.TotalAmount,
		o.PlatformFee, o.PaymentExpires, o.ComplianceAgreed}
	var err error
	// tx 为 nil 时退回库句柄, 避免 nil *sqlx.Tx 调 Exec 直接 panic。
	if tx != nil {
		_, err = tx.Exec(q, args...)
	} else {
		_, err = r.db.Exec(q, args...)
	}
	return err
}

func (r *Repository) GetOrderByNo(orderNo string) (*Order, error) {
	var o Order
	err := r.db.Get(&o, "SELECT "+orderColumns+" FROM orders WHERE order_no = ?", orderNo)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &o, err
}

func (r *Repository) GetBuyerOrderByNo(buyerID int64, orderNo string) (*BuyerOrder, error) {
	var o BuyerOrder
	err := r.db.Get(&o, fmt.Sprintf(`SELECT %s FROM orders o
		LEFT JOIN products p ON p.id=o.product_id
		LEFT JOIN enterprises e ON e.user_id=p.supplier_id AND e.status='verified'
		WHERE o.buyer_id=? AND o.order_no=?`, buyerOrderColumns), buyerID, orderNo)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &o, err
}

func (r *Repository) GetOrderByID(id int64) (*Order, error) {
	var o Order
	err := r.db.Get(&o, "SELECT "+orderColumns+" FROM orders WHERE id = ?", id)
	if err == sql.ErrNoRows {
		return nil, fmt.Errorf("order not found")
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
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

// TransitionOrderStatusTx 守卫式状态流转: 仅当订单当前处于 from 之一时才改为 to,
// 返回是否真的发生了流转。
//
// 这是库存归还幂等性的基石: 归还库存必须与状态流转在同一事务、同一条 UPDATE 的判定下完成。
// 若用「先查状态再改」的写法, 两个并发请求会同时读到 pending_payment 并各自归还一次库存,
// 导致库存凭空变多。条件 UPDATE 由 InnoDB 行锁串行化, 只有一个能拿到 RowsAffected=1。
func (r *Repository) TransitionOrderStatusTx(tx *sqlx.Tx, orderNo string, from []string, to string) (bool, error) {
	if len(from) == 0 {
		return false, fmt.Errorf("from status required")
	}
	q := "UPDATE orders SET status=? WHERE order_no=? AND status IN (?)"
	query, args, err := sqlx.In(q, to, orderNo, from)
	if err != nil {
		return false, err
	}
	res, err := tx.Exec(query, args...)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// GetOrderForUpdateTx 在事务内读订单(不加额外锁, 供流转成功后取 product_id/quantity 用)。
func (r *Repository) GetOrderForUpdateTx(tx *sqlx.Tx, orderNo string) (*Order, error) {
	var o Order
	err := tx.Get(&o, "SELECT "+orderColumns+" FROM orders WHERE order_no = ?", orderNo)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}

// ListExpiredUnpaidOrderNos 待支付且已过支付期限的订单号 (REQ-A-023)。
func (r *Repository) ListExpiredUnpaidOrderNos(limit int) ([]string, error) {
	var nos []string
	err := r.db.Select(&nos,
		`SELECT order_no FROM orders
		 WHERE status='pending_payment' AND payment_expires_at IS NOT NULL AND payment_expires_at < NOW()
		 ORDER BY payment_expires_at LIMIT ?`, limit)
	return nos, err
}

// ListExpiredLeaseOrderNos 履约中且租期已到的订单号 (REQ-A-043)。
// lease_end_at IS NULL 的是买断(perpetual)订单, 使用权永久, 永不到期, 必须排除。
func (r *Repository) ListExpiredLeaseOrderNos(limit int) ([]string, error) {
	var nos []string
	err := r.db.Select(&nos,
		`SELECT order_no FROM orders
		 WHERE status='active' AND lease_end_at IS NOT NULL AND lease_end_at < NOW()
		 ORDER BY lease_end_at LIMIT ?`, limit)
	return nos, err
}

func (r *Repository) UpdateOrderStatus(orderNo string, status string) error {
	return r.UpdateOrderStatusTx(nil, orderNo, status)
}

type OrderListFilter struct {
	BuyerID    int64
	SupplierID int64
	Status     string
	OrderNo    string
	Page       int
	PageSize   int
}

const (
	maxOrderPage     = 1000000
	maxOrderPageSize = 100
)

func (f *OrderListFilter) Normalize() {
	f.Status = strings.TrimSpace(f.Status)
	f.OrderNo = strings.TrimPrefix(strings.TrimSpace(f.OrderNo), "#")
	if f.Page <= 0 {
		f.Page = 1
	}
	if f.Page > maxOrderPage {
		f.Page = maxOrderPage
	}
	if f.PageSize <= 0 {
		f.PageSize = 20
	}
	if f.PageSize > maxOrderPageSize {
		f.PageSize = maxOrderPageSize
	}
}

func (f OrderListFilter) buyerWhere() (string, []interface{}) {
	where := "WHERE o.buyer_id=?"
	args := []interface{}{f.BuyerID}
	if f.Status != "" {
		where += " AND o.status=?"
		args = append(args, f.Status)
	}
	if f.OrderNo != "" {
		where += " AND o.order_no LIKE ?"
		args = append(args, "%"+f.OrderNo+"%")
	}
	return where, args
}

func (r *Repository) ListBuyerOrders(f OrderListFilter) ([]BuyerOrder, int64, error) {
	f.Normalize()
	where, args := f.buyerWhere()

	var total int64
	if err := r.db.Get(&total, "SELECT COUNT(*) FROM orders o "+where, args...); err != nil {
		return nil, 0, err
	}

	query := fmt.Sprintf(`SELECT %s FROM orders o
		LEFT JOIN products p ON p.id=o.product_id
		LEFT JOIN enterprises e ON e.user_id=p.supplier_id AND e.status='verified'
		%s ORDER BY o.created_at DESC LIMIT ? OFFSET ?`, buyerOrderColumns, where)
	args = append(args, f.PageSize, (f.Page-1)*f.PageSize)
	list := make([]BuyerOrder, 0)
	if err := r.db.Select(&list, query, args...); err != nil {
		return nil, 0, err
	}
	return list, total, nil
}

// SupplierOrder 履约订单 + 产品摘要(订单管理页需要型号与计费模式展示)。
type SupplierOrder struct {
	Order
	GPUModel    string `db:"gpu_model" json:"gpu_model"`
	ProductType string `db:"product_type" json:"product_type"`
	PricingMode string `db:"pricing_mode" json:"pricing_mode"`
}

// ListSupplierOrders 履约订单列表。
// status 支持逗号分隔多值(Tab 语义组: 待交付=paid,provisioning; 已完成=终态合集),
// 多值走 IN, 单值与原行为一致; statusCounts 为该供给方全部订单按状态的计数(Tab 角标)。
func (r *Repository) ListSupplierOrders(supplierID int64, status string, page, pageSize int) ([]SupplierOrder, int64, map[string]int64, error) {
	where := "WHERE p.supplier_id=?"
	args := []interface{}{supplierID}
	if status != "" {
		parts := strings.Split(status, ",")
		if len(parts) > 1 {
			where += " AND o.status IN (?"
			for range parts[1:] {
				where += ",?"
			}
			where += ")"
		} else {
			where += " AND o.status=?"
		}
		for _, s := range parts {
			args = append(args, strings.TrimSpace(s))
		}
	}
	var total int64
	if err := r.db.Get(&total, "SELECT COUNT(*) FROM orders o JOIN products p ON o.product_id=p.id "+where, args...); err != nil {
		return nil, 0, nil, err
	}

	counts := map[string]int64{}
	var countRows []struct {
		Status string `db:"status"`
		Count  int64  `db:"count"`
	}
	if err := r.db.Select(&countRows,
		"SELECT o.status AS status, COUNT(*) AS count FROM orders o JOIN products p ON o.product_id=p.id WHERE p.supplier_id=? GROUP BY o.status",
		supplierID); err != nil {
		return nil, 0, nil, err
	}
	for _, row := range countRows {
		counts[row.Status] = row.Count
	}

	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	query := fmt.Sprintf(`SELECT o.*, COALESCE(p.gpu_model,'') AS gpu_model,
		COALESCE(p.product_type,'') AS product_type, COALESCE(p.pricing_mode,'') AS pricing_mode
		FROM orders o JOIN products p ON o.product_id=p.id %s ORDER BY o.created_at DESC LIMIT ? OFFSET ?`, where)
	args = append(args, pageSize, (page-1)*pageSize)
	list := make([]SupplierOrder, 0)
	err := r.db.Select(&list, query, args...)
	return list, total, counts, err
}

// Delivery
func (r *Repository) CreateDelivery(d *OrderDelivery) error {
	_, err := r.db.Exec(
		"INSERT INTO order_deliveries (order_id, credential_encrypted) VALUES (?,?) ON DUPLICATE KEY UPDATE credential_encrypted=?",
		d.OrderID, d.CredentialEncrypted, d.CredentialEncrypted,
	)
	return err
}

// SaveDeliveryWithAccess 落库交付凭证 + 访问凭证(C-06)。
// access_value_encrypted 必须是 AES-256-GCM 密文, 调用方负责加密, 本方法不做任何明文兜底。
func (r *Repository) SaveDeliveryWithAccess(d *OrderDelivery) error {
	_, err := r.db.Exec(
		`INSERT INTO order_deliveries (order_id, credential_encrypted, access_key, access_value_encrypted, access_status, access_expires_at)
		VALUES (?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE credential_encrypted=VALUES(credential_encrypted), access_key=VALUES(access_key),
		access_value_encrypted=VALUES(access_value_encrypted), access_status=VALUES(access_status),
		access_expires_at=VALUES(access_expires_at), revoked_at=NULL`,
		d.OrderID, d.CredentialEncrypted, d.AccessKey, d.AccessValueEncrypted, d.AccessStatus, d.AccessExpiresAt,
	)
	return err
}

func (r *Repository) GetDeliveryByOrder(orderID int64) (*OrderDelivery, error) {
	var d OrderDelivery
	err := r.db.Get(&d, "SELECT "+deliveryColumns+" FROM order_deliveries WHERE order_id = ?", orderID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// RevokeAccessByOrder 吊销访问凭证(退款/取消/到期)。幂等: 重复调用不改变已吊销记录。
func (r *Repository) RevokeAccessByOrder(orderID int64) error {
	_, err := r.db.Exec(
		"UPDATE order_deliveries SET access_status='revoked', revoked_at=NOW() WHERE order_id=? AND access_status IN ('generated','delivered')",
		orderID,
	)
	return err
}

// RevokeExpiredAccess 批量吊销已过期的访问凭证, 返回受影响行数。供定时任务调用。
func (r *Repository) RevokeExpiredAccess() (int64, error) {
	res, err := r.db.Exec(
		`UPDATE order_deliveries SET access_status='revoked', revoked_at=NOW()
		WHERE access_status IN ('generated','delivered') AND access_expires_at IS NOT NULL AND access_expires_at < NOW()`,
	)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// Resource snapshots (C-05)
func (r *Repository) CreateSnapshotTx(tx *sqlx.Tx, s *ResourceSnapshot) (int64, error) {
	res, err := tx.Exec(
		`INSERT INTO resource_snapshots (product_id, supplier_id, sync_type, stock_before, stock_after, diff, reason, operator_id, anomaly)
		VALUES (?,?,?,?,?,?,?,?,?)`,
		s.ProductID, s.SupplierID, s.SyncType, s.StockBefore, s.StockAfter, s.Diff, s.Reason, s.OperatorID, s.Anomaly,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (r *Repository) ListSnapshotsByProduct(productID int64, page, pageSize int) ([]ResourceSnapshot, int64, error) {
	var total int64
	r.db.Get(&total, "SELECT COUNT(*) FROM resource_snapshots WHERE product_id=?", productID)
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	var list []ResourceSnapshot
	err := r.db.Select(&list, "SELECT "+snapshotColumns+" FROM resource_snapshots WHERE product_id=? ORDER BY created_at DESC LIMIT ? OFFSET ?",
		productID, pageSize, (page-1)*pageSize)
	return list, total, err
}

func (r *Repository) ListSnapshotsBySupplier(supplierID int64, page, pageSize int) ([]ResourceSnapshot, int64, error) {
	var total int64
	r.db.Get(&total, "SELECT COUNT(*) FROM resource_snapshots WHERE supplier_id=?", supplierID)
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	var list []ResourceSnapshot
	err := r.db.Select(&list, "SELECT "+snapshotColumns+" FROM resource_snapshots WHERE supplier_id=? ORDER BY created_at DESC LIMIT ? OFFSET ?",
		supplierID, pageSize, (page-1)*pageSize)
	return list, total, err
}

// Audit log: 凭证明文查看等敏感操作必须留痕(C-06)。
func (r *Repository) CreateAuditLog(operatorID int64, action, targetType string, targetID int64, before, after, ip string) error {
	_, err := r.db.Exec(
		"INSERT INTO audit_logs (operator_id, action, target_type, target_id, before_value, after_value, ip) VALUES (?,?,?,?,?,?,?)",
		operatorID, action, targetType, targetID, before, after, ip,
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
	err := r.db.Get(&c, "SELECT "+creditColumns+" FROM credit_scores WHERE supplier_id = ?", supplierID)
	if err == sql.ErrNoRows {
		return &CreditScore{SupplierID: supplierID, FulfillRate: 100, SlaRate: 100}, nil
	}
	return &c, err
}

func (r *Repository) FindCreditScore(supplierID int64) (*CreditScore, error) {
	var c CreditScore
	err := r.db.Get(&c, "SELECT "+creditColumns+" FROM credit_scores WHERE supplier_id = ?", supplierID)
	if err == sql.ErrNoRows {
		return nil, nil
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
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	query := fmt.Sprintf("SELECT %s FROM orders %s ORDER BY created_at DESC LIMIT ? OFFSET ?", orderColumns, where)
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
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	query := fmt.Sprintf("SELECT %s FROM products %s ORDER BY created_at DESC LIMIT ? OFFSET ?", productColumns, where)
	args = append(args, pageSize, (page-1)*pageSize)
	var list []Product
	err := r.db.Select(&list, query, args...)
	return list, total, err
}

// ErrToCode 把 service 层错误映射成业务错误码。
// 分三类: 已知的英文哨兵串按语义映射; 加密密钥未配置属服务端配置缺失 -> 500;
// 其余为参数/权限校验产生的中文提示 -> 40001 / 40300, 直接把原因回给前端展示。
func ErrToCode(err error) int {
	if err == nil {
		return errcode.Success
	}
	if errors.Is(err, crypto.ErrKeyNotConfigured) || errors.Is(err, crypto.ErrInvalidKeySize) {
		return errcode.InternalError
	}
	msg := err.Error()
	switch msg {
	case "insufficient stock":
		return errcode.Conflict
	case "product not found":
		return errcode.NotFound
	case "order not found":
		return errcode.NotFound
	case "delivery not found":
		return errcode.NotFound
	case "product not available":
		return errcode.Conflict
	case "order not active":
		return errcode.Conflict
	case errOrderNotConfirmable, errDeliveryNotConfirmable:
		return errcode.Conflict
	case "invalid status transition":
		return errcode.ParamInvalid
	case "invalid quantity":
		return errcode.ParamInvalid
	case "stock cannot be negative":
		return errcode.ParamInvalid
	}
	// 归属/权限类提示统一 40300, 便于前端区分"没权限"与"参数错"。
	if strings.HasPrefix(msg, "无权") {
		return errcode.Forbidden
	}
	if errors.Is(err, crypto.ErrInvalidCiphertext) {
		return errcode.InternalError
	}
	if containsCJK(msg) {
		return errcode.ParamInvalid
	}
	return errcode.InternalError
}

// containsCJK 判断是否含中日韩统一表意文字。本项目所有面向用户的校验提示都是中文,
// 以此区分"可直接展示的业务校验错误"与"内部错误"。
func containsCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}
