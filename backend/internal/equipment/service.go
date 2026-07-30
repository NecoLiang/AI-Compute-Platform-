package equipment

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ===== Validation =====

// ValidationError 表示入参校验失败, 由 ErrToCode 映射为 40001。
type ValidationError struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Reason }

func invalid(field, reason string) *ValidationError {
	return &ValidationError{Field: field, Reason: reason}
}

const (
	// MaxQuantity 单条商品最大挂牌数量, 防止录入笔误撑爆库存和后续询价校验。
	MaxQuantity = 1000000
	// MinManufactureYear 二手设备出厂年份下限(早于此年份的算力设备无残值意义)。
	MinManufactureYear = 2010
	// MaxTitleLen / MaxUsageDescLen 与 DDL 中的 VARCHAR 长度保持一致。
	MaxTitleLen     = 128
	MaxUsageDescLen = 256
	MaxBrandLen     = 64
	MaxModelLen     = 64
	MaxRegionLen    = 32
	MaxContactName  = 64
	MaxContactPhone = 20
)

var validEquipmentTypes = map[string]bool{
	"gpu_server": true, "storage": true, "network": true,
	"cooling": true, "ups": true, "rack": true, "other": true,
}

var validConditionTypes = map[string]bool{"new": true, "used": true}

var validProductStatuses = map[string]bool{
	"draft": true, "pending": true, "active": true, "sold_out": true, "offline": true,
}

// ===== Requests =====

type CreateProductReq struct {
	Title           string   `json:"title"`
	EquipmentType   string   `json:"equipment_type"`
	Brand           string   `json:"brand"`
	Model           string   `json:"model"`
	ConditionType   string   `json:"condition_type"`   // new=一手 used=二手
	ManufactureYear *int     `json:"manufacture_year"` // 二手必填
	UsageDesc       string   `json:"usage_desc"`
	Quantity        int      `json:"quantity"`
	UnitPrice       int64    `json:"unit_price"` // fen; 面议时必须为 0
	PriceNegotiable bool     `json:"price_negotiable"`
	Region          string   `json:"region"`
	Description     string   `json:"description"`
	Images          []string `json:"images"`
}

type CreateInquiryReq struct {
	Quantity     int    `json:"quantity"`
	ContactName  string `json:"contact_name"`
	ContactPhone string `json:"contact_phone"`
	Message      string `json:"message"`
}

// ValidateCreateProduct 纯函数校验设备商品发布入参。
// currentYear 由调用方传入(而非函数内部取 time.Now), 便于测试年份边界。
func ValidateCreateProduct(req CreateProductReq, currentYear int) error {
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return invalid("title", "商品标题不能为空")
	}
	if len([]rune(title)) > MaxTitleLen {
		return invalid("title", fmt.Sprintf("商品标题不能超过 %d 个字符", MaxTitleLen))
	}
	if !validEquipmentTypes[req.EquipmentType] {
		return invalid("equipment_type", "设备类型不合法")
	}
	if !validConditionTypes[req.ConditionType] {
		return invalid("condition_type", "新旧程度必须是 new(一手) 或 used(二手)")
	}

	// 二手设备必须交代出厂年份, 否则无法判断残值
	if req.ConditionType == "used" {
		if req.ManufactureYear == nil {
			return invalid("manufacture_year", "二手设备必须填写出厂年份")
		}
		y := *req.ManufactureYear
		if y < MinManufactureYear || y > currentYear {
			return invalid("manufacture_year", fmt.Sprintf("出厂年份必须在 %d ~ %d 之间", MinManufactureYear, currentYear))
		}
	} else if req.ManufactureYear != nil {
		// 一手设备若填了年份也不能是未来年份
		y := *req.ManufactureYear
		if y < MinManufactureYear || y > currentYear {
			return invalid("manufacture_year", fmt.Sprintf("出厂年份必须在 %d ~ %d 之间", MinManufactureYear, currentYear))
		}
	}
	if len([]rune(req.UsageDesc)) > MaxUsageDescLen {
		return invalid("usage_desc", fmt.Sprintf("使用情况不能超过 %d 个字符", MaxUsageDescLen))
	}
	if len([]rune(req.Brand)) > MaxBrandLen {
		return invalid("brand", fmt.Sprintf("品牌不能超过 %d 个字符", MaxBrandLen))
	}
	if len([]rune(req.Model)) > MaxModelLen {
		return invalid("model", fmt.Sprintf("型号不能超过 %d 个字符", MaxModelLen))
	}
	if len([]rune(req.Region)) > MaxRegionLen {
		return invalid("region", fmt.Sprintf("地域不能超过 %d 个字符", MaxRegionLen))
	}

	if req.Quantity <= 0 {
		return invalid("quantity", "数量必须大于 0")
	}
	if req.Quantity > MaxQuantity {
		return invalid("quantity", fmt.Sprintf("数量不能超过 %d", MaxQuantity))
	}

	// 面议与定价互斥: 面议时价格字段必须留空, 避免前端展示出一个未经确认的价格
	if req.PriceNegotiable {
		if req.UnitPrice != 0 {
			return invalid("unit_price", "面议商品的单价必须为 0")
		}
	} else {
		if req.UnitPrice <= 0 {
			return invalid("unit_price", "非面议商品的单价必须大于 0(单位:分)")
		}
	}
	return nil
}

// ValidateInquiryQuantity 校验询价数量: 必须为正且不超过商品挂牌数量。
func ValidateInquiryQuantity(qty, available int) error {
	if qty <= 0 {
		return invalid("quantity", "询价数量必须大于 0")
	}
	if available <= 0 {
		return invalid("quantity", "该商品当前无可售数量")
	}
	if qty > available {
		return invalid("quantity", fmt.Sprintf("询价数量不能超过商品挂牌数量 %d", available))
	}
	return nil
}

// ValidateCreateInquiry 校验询价单入参(不含与商品的交叉校验)。
func ValidateCreateInquiry(req CreateInquiryReq) error {
	name := strings.TrimSpace(req.ContactName)
	if name == "" {
		return invalid("contact_name", "联系人不能为空")
	}
	if len([]rune(name)) > MaxContactName {
		return invalid("contact_name", fmt.Sprintf("联系人不能超过 %d 个字符", MaxContactName))
	}
	phone := strings.TrimSpace(req.ContactPhone)
	if phone == "" {
		return invalid("contact_phone", "联系电话不能为空")
	}
	if len(phone) > MaxContactPhone {
		return invalid("contact_phone", fmt.Sprintf("联系电话不能超过 %d 个字符", MaxContactPhone))
	}
	if req.Quantity <= 0 {
		return invalid("quantity", "询价数量必须大于 0")
	}
	if req.Quantity > MaxQuantity {
		return invalid("quantity", fmt.Sprintf("询价数量不能超过 %d", MaxQuantity))
	}
	return nil
}

// NormalizeSort 收敛排序参数, 只允许白名单值(防 SQL 注入)。
func NormalizeSort(sort string) string {
	switch sort {
	case "price_asc", "price_desc", "created_at_desc":
		return sort
	default:
		return "created_at_desc"
	}
}

// NormalizeStatusFilter 只允许合法商品状态作为筛选值, 其余按空处理。
func NormalizeStatusFilter(status string) string {
	if validProductStatuses[status] {
		return status
	}
	return ""
}

// ===== Service =====

type Service struct {
	repo *Repository
	// now 可注入, 便于测试年份边界; 生产环境为 time.Now。
	now func() time.Time
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo, now: time.Now}
}

// CreateProduct 发布设备商品。发布后状态为 pending, 需运营审核才 active(与算力商品一致)。
func (s *Service) CreateProduct(vendorID int64, req CreateProductReq) (int64, error) {
	if vendorID <= 0 {
		return 0, invalid("vendor_id", "未识别到发布人身份")
	}
	if err := ValidateCreateProduct(req, s.now().Year()); err != nil {
		return 0, err
	}

	var imagesJSON *string
	if len(req.Images) > 0 {
		b, err := json.Marshal(req.Images)
		if err != nil {
			return 0, invalid("images", "图片列表格式不合法")
		}
		str := string(b)
		imagesJSON = &str
	}

	p := &EquipmentProduct{
		VendorID:        vendorID,
		Title:           strings.TrimSpace(req.Title),
		EquipmentType:   req.EquipmentType,
		Brand:           req.Brand,
		Model:           req.Model,
		ConditionType:   req.ConditionType,
		ManufactureYear: req.ManufactureYear,
		UsageDesc:       req.UsageDesc,
		Quantity:        req.Quantity,
		UnitPrice:       req.UnitPrice,
		PriceNegotiable: req.PriceNegotiable,
		Region:          req.Region,
		Description:     req.Description,
		Images:          imagesJSON,
	}
	return s.repo.CreateProduct(p)
}

func (s *Service) GetProduct(id int64) (*EquipmentProduct, error) {
	p, err := s.repo.GetProductByID(id)
	if err != nil { return nil, err }
	if p == nil { return nil, ErrProductNotFound }
	return p, nil
}

func (s *Service) ListProducts(f ProductFilter) ([]EquipmentProduct, int64, error) {
	f.Sort = NormalizeSort(f.Sort)
	return s.repo.ListProducts(f)
}

func (s *Service) GetVendorProducts(vendorID int64, status string, page, pageSize int) ([]EquipmentProduct, int64, error) {
	return s.repo.GetProductsByVendor(vendorID, NormalizeStatusFilter(status), page, pageSize)
}

// OfflineProduct 厂商下架自己的商品; 只允许操作本人商品。
func (s *Service) OfflineProduct(id, vendorID int64) error {
	return s.repo.UpdateVendorProductStatus(id, vendorID, "offline")
}

func (s *Service) ListAllProducts(status string, page, pageSize int) ([]EquipmentProduct, int64, error) {
	return s.repo.ListAllProducts(NormalizeStatusFilter(status), page, pageSize)
}

// ApproveProduct 运营审核通过, pending -> active。
func (s *Service) ApproveProduct(id int64) error {
	p, err := s.repo.GetProductByID(id)
	if err != nil { return err }
	if p == nil { return ErrProductNotFound }
	if p.Status != "pending" {
		return invalid("status", "只有待审核(pending)的商品可以审核通过")
	}
	return s.repo.UpdateProductStatus(id, "active")
}

// RejectProduct 运营审核驳回, pending -> draft(回到厂商草稿箱可修改后重提)。
func (s *Service) RejectProduct(id int64) error {
	p, err := s.repo.GetProductByID(id)
	if err != nil { return err }
	if p == nil { return ErrProductNotFound }
	if p.Status != "pending" {
		return invalid("status", "只有待审核(pending)的商品可以驳回")
	}
	return s.repo.UpdateProductStatus(id, "draft")
}

// CreateInquiry 买家提交询价。设备不接在线支付, 询价即撮合线索。
func (s *Service) CreateInquiry(buyerID, equipmentID int64, req CreateInquiryReq) (int64, error) {
	if buyerID <= 0 {
		return 0, invalid("buyer_id", "未识别到询价人身份")
	}
	if err := ValidateCreateInquiry(req); err != nil {
		return 0, err
	}
	p, err := s.repo.GetProductByID(equipmentID)
	if err != nil { return 0, err }
	if p == nil { return 0, ErrProductNotFound }
	if p.Status != "active" { return 0, ErrProductNotActive }
	if err := ValidateInquiryQuantity(req.Quantity, p.Quantity); err != nil {
		return 0, err
	}
	return s.repo.CreateInquiry(&EquipmentInquiry{
		EquipmentID:  equipmentID,
		BuyerID:      buyerID,
		Quantity:     req.Quantity,
		ContactName:  strings.TrimSpace(req.ContactName),
		ContactPhone: strings.TrimSpace(req.ContactPhone),
		Message:      req.Message,
	})
}

func (s *Service) ListBuyerInquiries(buyerID int64, page, pageSize int) ([]EquipmentInquiry, int64, error) {
	return s.repo.ListInquiriesByBuyer(buyerID, page, pageSize)
}

func (s *Service) ListVendorInquiries(vendorID int64, status string, page, pageSize int) ([]EquipmentInquiry, int64, error) {
	if status != "" && status != "new" && status != "replied" && status != "closed" {
		status = ""
	}
	return s.repo.ListInquiriesByVendor(vendorID, status, page, pageSize)
}
