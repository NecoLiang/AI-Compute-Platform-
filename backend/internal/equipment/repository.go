package equipment

import (
	"database/sql"
	"fmt"
	"time"
	"tokenfactory/pkg/errcode"

	"github.com/jmoiron/sqlx"
)

// ===== Models =====

// EquipmentProduct 设备商品(一手/二手)。
// 设备类交易金额大且需线下验货议价, v1 不接在线支付, 只走询价撮合。
type EquipmentProduct struct {
	ID              int64     `db:"id" json:"id"`
	VendorID        int64     `db:"vendor_id" json:"vendor_id"`
	Title           string    `db:"title" json:"title"`
	EquipmentType   string    `db:"equipment_type" json:"equipment_type"`
	Brand           string    `db:"brand" json:"brand"`
	Model           string    `db:"model" json:"model"`
	ConditionType   string    `db:"condition_type" json:"condition_type"` // new=一手 used=二手
	ManufactureYear *int      `db:"manufacture_year" json:"manufacture_year"`
	UsageDesc       string    `db:"usage_desc" json:"usage_desc"`
	Quantity        int       `db:"quantity" json:"quantity"`
	UnitPrice       int64     `db:"unit_price" json:"unit_price"` // fen
	PriceNegotiable bool      `db:"price_negotiable" json:"price_negotiable"`
	Region          string    `db:"region" json:"region"`
	Description     string    `db:"description" json:"description"`
	Images          *string   `db:"images" json:"images"`
	Status          string    `db:"status" json:"status"`
	RejectedReason  string    `db:"rejected_reason" json:"rejected_reason"`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time `db:"updated_at" json:"updated_at"`
	// VendorName 供应方企业名, 仅由带 enterprises JOIN 的买家侧查询填充(SELECT * 路径下为空);
	// 对买家输出前必须经 masking.MaskCompanyName 脱敏。
	VendorName string `db:"vendor_name" json:"-"`
}

type EquipmentInquiry struct {
	ID           int64     `db:"id" json:"id"`
	EquipmentID  int64     `db:"equipment_id" json:"equipment_id"`
	BuyerID      int64     `db:"buyer_id" json:"buyer_id"`
	Quantity     int       `db:"quantity" json:"quantity"`
	ContactName  string    `db:"contact_name" json:"contact_name"`
	ContactPhone string    `db:"contact_phone" json:"contact_phone"`
	Message      string    `db:"message" json:"message"`
	Status       string    `db:"status" json:"status"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}

// ===== Repository =====

type Repository struct {
	db *sqlx.DB
}

func NewRepository(db *sqlx.DB) *Repository {
	return &Repository{db: db}
}

// Products

func (r *Repository) CreateProduct(p *EquipmentProduct) (int64, error) {
	res, err := r.db.Exec(
		`INSERT INTO equipment_products (vendor_id, title, equipment_type, brand, model, condition_type,
		manufacture_year, usage_desc, quantity, unit_price, price_negotiable, region, description, images, status)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,'pending')`,
		p.VendorID, p.Title, p.EquipmentType, p.Brand, p.Model, p.ConditionType,
		p.ManufactureYear, p.UsageDesc, p.Quantity, p.UnitPrice, p.PriceNegotiable,
		p.Region, p.Description, p.Images,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (r *Repository) GetProductByID(id int64) (*EquipmentProduct, error) {
	var p EquipmentProduct
	err := r.db.Get(&p, "SELECT * FROM equipment_products WHERE id = ?", id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *Repository) UpdateProductStatus(id int64, status string) error {
	res, err := r.db.Exec("UPDATE equipment_products SET status=? WHERE id=?", status, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrProductNotFound
	}
	return nil
}

// ReviewProduct 运营审核落库: 通过时清空驳回原因, 驳回时记录原因供供应方修改重提参考。
func (r *Repository) ReviewProduct(id int64, status, reason string) error {
	res, err := r.db.Exec("UPDATE equipment_products SET status=?, rejected_reason=? WHERE id=?", status, reason, id)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrProductNotFound
	}
	return nil
}

// ResubmitProduct 供应方修改重提: 草稿/被驳回(draft)与已下架(offline)可改,
// 重提后回 pending 重新审核并清空驳回原因。与算力商品同口径, WHERE 同时锁 vendor_id 防越权。
func (r *Repository) ResubmitProduct(id int64, p *EquipmentProduct) error {
	res, err := r.db.Exec(`UPDATE equipment_products SET title=?, equipment_type=?, brand=?, model=?,
		condition_type=?, manufacture_year=?, usage_desc=?, quantity=?, unit_price=?, price_negotiable=?,
		region=?, description=?, images=?, status='pending', rejected_reason=''
		WHERE id=? AND vendor_id=? AND status IN ('draft','offline')`,
		p.Title, p.EquipmentType, p.Brand, p.Model, p.ConditionType, p.ManufactureYear, p.UsageDesc,
		p.Quantity, p.UnitPrice, p.PriceNegotiable, p.Region, p.Description, p.Images, id, p.VendorID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		// 区分"不存在/不属于本人"与"状态不允许": 前者按不存在处理(防枚举), 后者给出明确提示。
		var status string
		if err := r.db.Get(&status, "SELECT status FROM equipment_products WHERE id=? AND vendor_id=?", id, p.VendorID); err != nil {
			return ErrProductNotFound
		}
		return invalid("status", "只有草稿/被驳回或已下架的商品可以修改重提, 在售商品请先下架")
	}
	return nil
}

// UpdateVendorProductStatus 限定 vendor 只能改自己的商品, 防越权。
func (r *Repository) UpdateVendorProductStatus(id, vendorID int64, status string) error {
	res, err := r.db.Exec("UPDATE equipment_products SET status=? WHERE id=? AND vendor_id=?", status, id, vendorID)
	if err != nil {
		return err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if affected == 0 {
		return ErrProductNotFound
	}
	return nil
}

type ProductFilter struct {
	EquipmentType string
	ConditionType string
	Region        string
	PriceMin      int64 // fen
	PriceMax      int64 // fen
	Page          int
	PageSize      int
	Sort          string
}

const maxPageSize = 100

// Normalize 收敛分页参数, 防止 page_size 被放大成全表扫描。
func (f *ProductFilter) Normalize() {
	if f.Page <= 0 {
		f.Page = 1
	}
	if f.PageSize <= 0 {
		f.PageSize = 20
	}
	if f.PageSize > maxPageSize {
		f.PageSize = maxPageSize
	}
}

// buildWhere 列名带 p. 前缀: 买家侧查询 JOIN enterprises 后, status 等列名会歧义。
func (f *ProductFilter) buildWhere(base string) (string, []interface{}) {
	where := base
	args := []interface{}{}
	if f.EquipmentType != "" {
		where += " AND p.equipment_type=?"
		args = append(args, f.EquipmentType)
	}
	if f.ConditionType != "" {
		where += " AND p.condition_type=?"
		args = append(args, f.ConditionType)
	}
	if f.Region != "" {
		where += " AND p.region=?"
		args = append(args, f.Region)
	}
	// 面议商品 unit_price=0, 落入价格区间筛选时应被排除
	if f.PriceMin > 0 {
		where += " AND p.price_negotiable=0 AND p.unit_price >= ?"
		args = append(args, f.PriceMin)
	}
	if f.PriceMax > 0 {
		where += " AND p.price_negotiable=0 AND p.unit_price <= ?"
		args = append(args, f.PriceMax)
	}
	return where, args
}

func (f *ProductFilter) orderBy() string {
	switch f.Sort {
	case "price_asc":
		return "ORDER BY p.unit_price ASC"
	case "price_desc":
		return "ORDER BY p.unit_price DESC"
	default:
		return "ORDER BY p.created_at DESC"
	}
}

// vendorNameJoin 买家侧查询带出供应方企业名(与算力市场 supplier_name 同口径), 输出前由 handler 脱敏。
const vendorNameJoin = `SELECT p.*, COALESCE(e.name,'') AS vendor_name
	FROM equipment_products p
	LEFT JOIN enterprises e ON e.user_id=p.vendor_id AND e.status='verified'`

func (r *Repository) ListProducts(f ProductFilter) ([]EquipmentProduct, int64, error) {
	f.Normalize()
	where, args := f.buildWhere("WHERE p.status='active'")

	var total int64
	if err := r.db.Get(&total, "SELECT COUNT(*) FROM equipment_products p "+where, args...); err != nil {
		return nil, 0, err
	}

	query := fmt.Sprintf("%s %s %s LIMIT ? OFFSET ?", vendorNameJoin, where, f.orderBy())
	args = append(args, f.PageSize, (f.Page-1)*f.PageSize)
	var list []EquipmentProduct
	err := r.db.Select(&list, query, args...)
	return list, total, err
}

// GetProductWithVendor 买家侧详情: 附带供应方企业名(输出前脱敏)。
func (r *Repository) GetProductWithVendor(id int64) (*EquipmentProduct, error) {
	var p EquipmentProduct
	err := r.db.Get(&p, vendorNameJoin+" WHERE p.id = ?", id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (r *Repository) GetProductsByVendor(vendorID int64, status string, page, pageSize int) ([]EquipmentProduct, int64, error) {
	f := ProductFilter{Page: page, PageSize: pageSize}
	f.Normalize()

	where := "WHERE vendor_id=?"
	args := []interface{}{vendorID}
	if status != "" {
		where += " AND status=?"
		args = append(args, status)
	}
	var total int64
	if err := r.db.Get(&total, "SELECT COUNT(*) FROM equipment_products "+where, args...); err != nil {
		return nil, 0, err
	}
	query := fmt.Sprintf("SELECT * FROM equipment_products %s ORDER BY created_at DESC LIMIT ? OFFSET ?", where)
	args = append(args, f.PageSize, (f.Page-1)*f.PageSize)
	var list []EquipmentProduct
	err := r.db.Select(&list, query, args...)
	return list, total, err
}

func (r *Repository) ListAllProducts(status string, page, pageSize int) ([]EquipmentProduct, int64, error) {
	f := ProductFilter{Page: page, PageSize: pageSize}
	f.Normalize()

	where := ""
	args := []interface{}{}
	if status != "" {
		where = "WHERE status=?"
		args = append(args, status)
	}
	var total int64
	if err := r.db.Get(&total, "SELECT COUNT(*) FROM equipment_products "+where, args...); err != nil {
		return nil, 0, err
	}
	query := fmt.Sprintf("SELECT * FROM equipment_products %s ORDER BY created_at DESC LIMIT ? OFFSET ?", where)
	args = append(args, f.PageSize, (f.Page-1)*f.PageSize)
	var list []EquipmentProduct
	err := r.db.Select(&list, query, args...)
	return list, total, err
}

// Inquiries

func (r *Repository) CreateInquiry(q *EquipmentInquiry) (int64, error) {
	res, err := r.db.Exec(
		`INSERT INTO equipment_inquiries (equipment_id, buyer_id, quantity, contact_name, contact_phone, message, status)
		VALUES (?,?,?,?,?,?,'new')`,
		q.EquipmentID, q.BuyerID, q.Quantity, q.ContactName, q.ContactPhone, q.Message,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (r *Repository) ListInquiriesByBuyer(buyerID int64, page, pageSize int) ([]EquipmentInquiry, int64, error) {
	f := ProductFilter{Page: page, PageSize: pageSize}
	f.Normalize()

	var total int64
	if err := r.db.Get(&total, "SELECT COUNT(*) FROM equipment_inquiries WHERE buyer_id=?", buyerID); err != nil {
		return nil, 0, err
	}
	var list []EquipmentInquiry
	err := r.db.Select(&list,
		"SELECT * FROM equipment_inquiries WHERE buyer_id=? ORDER BY created_at DESC LIMIT ? OFFSET ?",
		buyerID, f.PageSize, (f.Page-1)*f.PageSize,
	)
	return list, total, err
}

// ListInquiriesByVendor 通过 equipment_products 关联, 只返回该 vendor 自己商品收到的询价。
func (r *Repository) ListInquiriesByVendor(vendorID int64, status string, page, pageSize int) ([]EquipmentInquiry, int64, error) {
	f := ProductFilter{Page: page, PageSize: pageSize}
	f.Normalize()

	where := "WHERE e.vendor_id=?"
	args := []interface{}{vendorID}
	if status != "" {
		where += " AND q.status=?"
		args = append(args, status)
	}
	var total int64
	if err := r.db.Get(&total,
		"SELECT COUNT(*) FROM equipment_inquiries q JOIN equipment_products e ON q.equipment_id=e.id "+where, args...); err != nil {
		return nil, 0, err
	}
	query := fmt.Sprintf(
		"SELECT q.* FROM equipment_inquiries q JOIN equipment_products e ON q.equipment_id=e.id %s ORDER BY q.created_at DESC LIMIT ? OFFSET ?",
		where,
	)
	args = append(args, f.PageSize, (f.Page-1)*f.PageSize)
	var list []EquipmentInquiry
	err := r.db.Select(&list, query, args...)
	return list, total, err
}

// ===== Errors =====

var (
	ErrProductNotFound  = fmt.Errorf("equipment not found")
	ErrProductNotActive = fmt.Errorf("equipment not available")
)

func ErrToCode(err error) int {
	if err == nil {
		return errcode.Success
	}
	switch err.Error() {
	case ErrProductNotFound.Error():
		return errcode.NotFound
	case ErrProductNotActive.Error():
		return errcode.Conflict
	}
	if _, ok := err.(*ValidationError); ok {
		return errcode.ParamInvalid
	}
	return errcode.InternalError
}
