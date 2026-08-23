package compute

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"tokenfactory/pkg/crypto"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
)

// ===== 商品类型 / 计费模式常量 (C-01, C-04) =====

const (
	ProductTypeCardRental = "card_rental" // 零租: 按卡租, 计费 h/d/w
	ProductTypeOutright   = "outright"    // 零售买断: 使用权永久, 一次性交付机器使用权
	ProductTypeCenter     = "center"      // 成熟算力中心: 整体打包(x卡 / x台 / 约xxxP)
	ProductTypeColocation = "colocation"  // 空心机房: 有机房无设备, 面议
)

const (
	PricingHourly    = "hourly"
	PricingDaily     = "daily"
	PricingWeekly    = "weekly"
	PricingMonthly   = "monthly"
	PricingPerpetual = "perpetual"
)

// 访问凭证状态 (C-06)
const (
	AccessStatusNone      = "none"
	AccessStatusGenerated = "generated"
	AccessStatusDelivered = "delivered"
	AccessStatusRevoked   = "revoked"
)

// 下单参数硬上限: 防止 int64 溢出与恶意超大值。
const (
	MaxOrderQuantity = 100000      // 单笔最大数量
	MaxOrderTotalFen = int64(1e12) // 单笔订单金额上限(分) = 100 亿元
	AccessKeyPrefix  = "ak-"       // 访问凭证标识前缀
	accessKeyRandLen = 16          // 16 字节 -> 32 位 hex
	accessValRandLen = 24          // 24 字节 -> 48 位 hex
)

// ===== duration 语义（C-04 口径） =====
//
// `duration` 表示**计费周期数**，不是小时数：
//   hourly  -> 小时数     daily -> 天数
//   weekly  -> 周数       monthly -> 月数
//   perpetual -> 买断无租期，强制归一为 1
//
// 单价 `unit_price` 相应是「元/卡·该计费周期」。这样前后端只need传一个数字，
// 不引入除不尽的取整误差（涉及资金，取整规则越少越安全）。

// maxDurationByPricingMode 各计费模式的单笔最大周期数，统一折合约 10 年。
var maxDurationByPricingMode = map[string]int{
	PricingHourly:    87600, // 10 年 ≈ 87600 小时
	PricingDaily:     3650,  // 10 年
	PricingWeekly:    520,   // 10 年
	PricingMonthly:   120,   // 10 年
	PricingPerpetual: 1,     // 买断无租期
}

// durationUnitLabel 用于拼中文错误提示，避免"租期超出上限 120"这种没有单位的歧义提示。
var durationUnitLabel = map[string]string{
	PricingHourly:    "小时",
	PricingDaily:     "天",
	PricingWeekly:    "周",
	PricingMonthly:   "个月",
	PricingPerpetual: "次",
}

// MaxDurationFor 返回该计费模式允许的最大周期数；未知模式回退到最严格的上限。
func MaxDurationFor(pricingMode string) int {
	if m, ok := maxDurationByPricingMode[pricingMode]; ok {
		return m
	}
	return maxDurationByPricingMode[PricingMonthly]
}

// DurationUnit 返回该计费模式下 duration 的单位中文名。
func DurationUnit(pricingMode string) string {
	if u, ok := durationUnitLabel[pricingMode]; ok {
		return u
	}
	return "个周期"
}

// LeaseEndAt 按计费模式把「计费周期数」换算成租期结束时间。
// monthly / weekly 走自然日历（AddDate），不用固定 30 天/720 小时近似，
// 避免"买 1 个月却在 2 月少给 2 天"这类与账单不符的情况。
// 买断（perpetual）无结束时间，返回零值 time.Time，调用方必须判空。
func LeaseEndAt(start time.Time, pricingMode string, duration int) time.Time {
	switch pricingMode {
	case PricingHourly:
		return start.Add(time.Duration(duration) * time.Hour)
	case PricingDaily:
		return start.AddDate(0, 0, duration)
	case PricingWeekly:
		return start.AddDate(0, 0, duration*7)
	case PricingMonthly:
		return start.AddDate(0, duration, 0)
	case PricingPerpetual:
		return time.Time{}
	default:
		return start.Add(time.Duration(duration) * time.Hour)
	}
}

// AnomalyRatioThreshold 盘点异常阈值: |diff|/stock_before > 0.3 判为异常 (C-05)。
// 用整数比值(3/10)判定, 避免 float 精度问题。
const (
	anomalyRatioNum = 3
	anomalyRatioDen = 10
)

// validProductTypes 全部合法商品类型。
var validProductTypes = map[string]bool{
	ProductTypeCardRental: true,
	ProductTypeOutright:   true,
	ProductTypeCenter:     true,
	ProductTypeColocation: true,
}

var validDeliveryModes = map[string]bool{
	"bare_metal": true,
	"container":  true,
	"rack":       true,
	"vm":         true,
}

// allowedPricingModes 各商品类型允许的计费模式 (C-01, C-04)。
var allowedPricingModes = map[string]map[string]bool{
	ProductTypeCardRental: {PricingHourly: true, PricingDaily: true, PricingWeekly: true},
	ProductTypeOutright:   {PricingPerpetual: true},
	ProductTypeCenter:     {PricingDaily: true, PricingWeekly: true, PricingMonthly: true, PricingPerpetual: true},
	ProductTypeColocation: {PricingHourly: true, PricingDaily: true, PricingWeekly: true, PricingMonthly: true, PricingPerpetual: true},
}

// pricingModeLabels 用于拼中文错误提示。
var pricingModeLabels = map[string]string{
	PricingHourly:    "按小时",
	PricingDaily:     "按天",
	PricingWeekly:    "按周",
	PricingMonthly:   "按月",
	PricingPerpetual: "永久",
}

type Service struct {
	repo    *Repository
	db      *sqlx.DB
	feeRate int64 // platform fee rate in basis points, default 500 = 5%
	// credentialKey 是访问凭证的 AES-256-GCM 密钥。为 nil/空 表示未配置:
	// 此时任何需要加解密的操作都会返回 crypto.ErrKeyNotConfigured, 绝不降级存明文。
	credentialKey []byte
}

// NewService 兼容既有调用 NewService(repo, db)。
// 可选第三参传 config.Security.CredentialKey (64 位 hex), 用于交付访问凭证加解密;
// 不传或传空串时凭证功能返回明确的"密钥未配置"错误, 不做任何明文降级。
func NewService(repo *Repository, db *sqlx.DB, credentialKeyHex ...string) *Service {
	s := &Service{repo: repo, db: db, feeRate: 500}
	if len(credentialKeyHex) > 0 {
		// 配置非法只记录为未配置状态, 由调用点返回明确错误, 不在此处 panic 影响其他模块启动。
		if key, err := crypto.ParseKeyHex(credentialKeyHex[0]); err == nil {
			s.credentialKey = key
		}
	}
	return s
}

// SetCredentialKey 显式注入凭证密钥(供 main.go 装配时调用)。key 非法时返回错误。
func (s *Service) SetCredentialKey(hexKey string) error {
	key, err := crypto.ParseKeyHex(hexKey)
	if err != nil {
		return err
	}
	s.credentialKey = key
	return nil
}

// ===== Qualifications (T-009, T-010) =====

func (s *Service) SubmitQualification(userID int64, qualType, certName, certNumber, certURL string, expiresAt *time.Time) (int64, error) {
	q := &SupplierQualification{
		UserID:     userID,
		QualType:   qualType,
		CertName:   certName,
		CertNumber: certNumber,
		CertURL:    certURL,
		ExpiresAt:  expiresAt,
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

// ===== Products (T-011, T-012, C-01, C-02) =====

type CreateProductReq struct {
	ProductType       string `json:"product_type"`
	GpuModel          string `json:"gpu_model"`
	CardCount         int    `json:"card_count"`
	MachineCount      int    `json:"machine_count"`
	TotalPflopsApprox string `json:"total_pflops_approx"`
	PowerCapacityKw   int    `json:"power_capacity_kw"`
	RackCount         int    `json:"rack_count"`
	PriceNegotiable   bool   `json:"price_negotiable"`
	CpuSpec           string `json:"cpu_spec"`
	MemorySpec        string `json:"memory_spec"`
	StorageSpec       string `json:"storage_spec"`
	BandwidthSpec     string `json:"bandwidth_spec"`
	DeliveryMode      string `json:"delivery_mode"`
	PricingMode       string `json:"pricing_mode"`
	UnitPrice         int64  `json:"unit_price"` // fen
	AvailableHours    string `json:"available_hours"`
	Stock             int    `json:"stock"`
	MinOrder          int    `json:"min_order"`
	MinDuration       int    `json:"min_duration"`
	Region            string `json:"region"`
	ComplianceAgreed  bool   `json:"compliance_agreed"`
}

// NormalizeProductReq 填补可省略字段的默认值。纯函数, 便于测试。
// 注意: 只补默认值, 不做任何"猜测式修正" —— 非法输入交给 ValidateProductReq 报错。
func NormalizeProductReq(req CreateProductReq) CreateProductReq {
	if req.ProductType == "" {
		req.ProductType = ProductTypeCardRental
	}
	if req.MinOrder <= 0 {
		req.MinOrder = 1
	}
	if req.MinDuration <= 0 {
		req.MinDuration = 1
	}

	switch req.ProductType {
	case ProductTypeOutright:
		// 买断型库存等于可售台数; 计费模式恒为永久。
		if req.Stock <= 0 {
			req.Stock = req.MachineCount
		}
		req.PricingMode = PricingPerpetual
	case ProductTypeCenter:
		// 算力中心整体打包, 默认 1 份可售。
		if req.Stock <= 0 {
			req.Stock = 1
		}
		if req.PricingMode == "" {
			req.PricingMode = PricingMonthly
		}
	case ProductTypeColocation:
		// 空心机房面议: 强制 price_negotiable=1 且不带在线价格。
		req.PriceNegotiable = true
		req.UnitPrice = 0
		if req.Stock <= 0 {
			req.Stock = 1
		}
		if req.PricingMode == "" {
			req.PricingMode = PricingMonthly
		}
	case ProductTypeCardRental:
		if req.PricingMode == "" {
			req.PricingMode = PricingHourly
		}
	}
	return req
}

// ValidateProductReq 按 product_type 做服务端强校验 (C-02)。
// 前端校验只是体验优化, 这里是唯一可信的关卡。
func ValidateProductReq(req CreateProductReq) error {
	if !validProductTypes[req.ProductType] {
		return fmt.Errorf("商品类型非法: 仅支持 card_rental(零租)/outright(买断)/center(算力中心)/colocation(空心机房)")
	}
	if req.UnitPrice < 0 {
		return fmt.Errorf("单价不能为负数")
	}
	if req.Stock < 0 {
		return fmt.Errorf("库存不能为负数")
	}
	if req.UnitPrice > MaxOrderTotalFen {
		return fmt.Errorf("单价超出上限(不得超过 100 亿元)")
	}
	if req.Stock > MaxOrderQuantity {
		return fmt.Errorf("库存超出上限(不得超过 %d)", MaxOrderQuantity)
	}

	if modes := allowedPricingModes[req.ProductType]; !modes[req.PricingMode] {
		return fmt.Errorf("计费模式 %q 不适用于该商品类型, 允许: %s", req.PricingMode, allowedModesText(req.ProductType))
	}

	switch req.ProductType {
	case ProductTypeCardRental:
		if strings.TrimSpace(req.GpuModel) == "" {
			return fmt.Errorf("零租商品必须填写 GPU 型号")
		}
		if req.CardCount <= 0 {
			return fmt.Errorf("零租商品的卡数必须大于 0")
		}
		if req.Stock <= 0 {
			return fmt.Errorf("零租商品的可租库存必须大于 0")
		}
		if req.UnitPrice <= 0 {
			return fmt.Errorf("零租商品的单价必须大于 0")
		}
		if req.PriceNegotiable {
			return fmt.Errorf("零租商品不支持面议, 必须给出明确单价")
		}
	case ProductTypeOutright:
		if strings.TrimSpace(req.GpuModel) == "" {
			return fmt.Errorf("买断商品必须填写 GPU 型号")
		}
		if req.MachineCount <= 0 {
			return fmt.Errorf("买断商品的台数必须大于 0")
		}
		if req.UnitPrice <= 0 {
			return fmt.Errorf("买断商品的单价必须大于 0")
		}
		if req.PriceNegotiable {
			return fmt.Errorf("买断商品不支持面议, 必须给出明确单价")
		}
	case ProductTypeCenter:
		if req.MachineCount <= 0 {
			return fmt.Errorf("算力中心商品的台数必须大于 0")
		}
		if strings.TrimSpace(req.TotalPflopsApprox) == "" {
			return fmt.Errorf("算力中心商品必须填写约总算力(如 128P)")
		}
		if !req.PriceNegotiable && req.UnitPrice <= 0 {
			return fmt.Errorf("算力中心商品需填写单价, 或勾选面议")
		}
	case ProductTypeColocation:
		if req.PowerCapacityKw <= 0 {
			return fmt.Errorf("空心机房必须填写电力容量(kW)且大于 0")
		}
		if req.RackCount <= 0 {
			return fmt.Errorf("空心机房必须填写机柜数且大于 0")
		}
		if !req.PriceNegotiable {
			return fmt.Errorf("空心机房仅支持面议, price_negotiable 必须为 1")
		}
		if req.UnitPrice != 0 {
			return fmt.Errorf("空心机房为面议商品, 单价必须为 0")
		}
	}
	return nil
}

func allowedModesText(productType string) string {
	order := []string{PricingHourly, PricingDaily, PricingWeekly, PricingMonthly, PricingPerpetual}
	modes := allowedPricingModes[productType]
	var parts []string
	for _, m := range order {
		if modes[m] {
			parts = append(parts, fmt.Sprintf("%s(%s)", m, pricingModeLabels[m]))
		}
	}
	return strings.Join(parts, "/")
}

func (s *Service) CreateProduct(supplierID int64, req CreateProductReq) (int64, error) {
	req = NormalizeProductReq(req)
	if err := ValidateProductReq(req); err != nil {
		return 0, err
	}

	p := &Product{
		SupplierID:       supplierID,
		ProductType:      req.ProductType,
		GpuModel:         strings.TrimSpace(req.GpuModel),
		CardCount:        req.CardCount,
		CpuSpec:          req.CpuSpec,
		MemorySpec:       req.MemorySpec,
		StorageSpec:      req.StorageSpec,
		BandwidthSpec:    req.BandwidthSpec,
		DeliveryMode:     req.DeliveryMode,
		PricingMode:      req.PricingMode,
		UnitPrice:        req.UnitPrice,
		PriceNegotiable:  req.PriceNegotiable,
		AvailableHours:   req.AvailableHours,
		Stock:            req.Stock,
		MinOrder:         req.MinOrder,
		MinDuration:      req.MinDuration,
		Region:           req.Region,
		ComplianceAgreed: req.ComplianceAgreed,
	}
	// 可空字段: 只在有意义时写入, 否则留 NULL, 避免 0/"" 被当成真实值展示。
	if req.MachineCount > 0 {
		v := req.MachineCount
		p.MachineCount = &v
	}
	if strings.TrimSpace(req.TotalPflopsApprox) != "" {
		v := strings.TrimSpace(req.TotalPflopsApprox)
		p.TotalPflopsApprox = &v
	}
	if req.PowerCapacityKw > 0 {
		v := req.PowerCapacityKw
		p.PowerCapacityKw = &v
	}
	if req.RackCount > 0 {
		v := req.RackCount
		p.RackCount = &v
	}

	return s.repo.CreateProduct(p)
}

func (s *Service) GetProduct(id int64) (*Product, *CreditScore, error) {
	p, err := s.repo.GetProductByID(id)
	if err != nil {
		return nil, nil, err
	}
	credit, _ := s.repo.GetCreditScore(p.SupplierID)
	return p, credit, nil
}

func (s *Service) ListProducts(f ProductFilter) ([]Product, int64, error) {
	f.Normalize()
	if f.ProductType != "" && !validProductTypes[f.ProductType] {
		return nil, 0, fmt.Errorf("商品类型非法: %s", f.ProductType)
	}
	if f.PricingMode != "" {
		if _, ok := maxDurationByPricingMode[f.PricingMode]; !ok {
			return nil, 0, fmt.Errorf("计费模式非法: %s", f.PricingMode)
		}
	}
	if f.DeliveryMode != "" && !validDeliveryModes[f.DeliveryMode] {
		return nil, 0, fmt.Errorf("交付方式非法: %s", f.DeliveryMode)
	}
	if f.PriceMin > 0 && f.PriceMax > 0 && f.PriceMin > f.PriceMax {
		return nil, 0, fmt.Errorf("最低价格不能高于最高价格")
	}
	if len([]rune(f.Query)) > 100 || len([]rune(f.GpuModel)) > 100 ||
		len([]rune(f.Region)) > 100 || len([]rune(f.AvailableHours)) > 100 {
		return nil, 0, fmt.Errorf("搜索条件不能超过 100 个字符")
	}
	return s.repo.ListProducts(f)
}

func (s *Service) GetSupplierProducts(supplierID int64) ([]Product, error) {
	return s.repo.GetProductsBySupplier(supplierID)
}

// ===== C-03 供给方工作台按类型分组 =====

// ProductTypeGroup 单个商品类型的分组结果 + 统计。
type ProductTypeGroup struct {
	ProductType  string    `json:"product_type"`
	Label        string    `json:"label"`
	Count        int       `json:"count"`         // 商品数
	TotalMachine int       `json:"total_machine"` // 总台数
	TotalCard    int       `json:"total_card"`    // 总卡数
	TotalStock   int       `json:"total_stock"`   // 总可售库存
	ActiveCount  int       `json:"active_count"`  // 在售数
	Products     []Product `json:"products"`
}

var productTypeLabels = map[string]string{
	ProductTypeCardRental: "零租(按卡租)",
	ProductTypeOutright:   "零售买断",
	ProductTypeCenter:     "成熟算力中心",
	ProductTypeColocation: "空心机房",
}

// productTypeOrder 固定分组顺序, 保证前端展示稳定。
var productTypeOrder = []string{ProductTypeCardRental, ProductTypeOutright, ProductTypeCenter, ProductTypeColocation}

// GroupProductsByType 纯函数: 按 product_type 分组并统计。四种类型恒定出现(空组返回 0),
// 前端不必自己补空态。
func GroupProductsByType(list []Product) []ProductTypeGroup {
	// 用 map 累积, 最后再按固定顺序物化成切片, 避免持有切片元素指针被 append 扩容打断。
	acc := make(map[string]*ProductTypeGroup, len(productTypeOrder)+2)
	order := make([]string, 0, len(productTypeOrder)+2)
	for _, t := range productTypeOrder {
		acc[t] = &ProductTypeGroup{ProductType: t, Label: productTypeLabels[t], Products: []Product{}}
		order = append(order, t)
	}

	for _, p := range list {
		t := p.ProductType
		if t == "" {
			t = ProductTypeCardRental
		}
		g, ok := acc[t]
		if !ok {
			// 未知类型(理论上被 DB ENUM 挡住)也如实归档, 不静默丢弃数据。
			g = &ProductTypeGroup{ProductType: t, Label: t, Products: []Product{}}
			acc[t] = g
			order = append(order, t)
		}
		g.Count++
		g.TotalStock += p.Stock
		g.TotalCard += p.CardCount
		if p.MachineCount != nil {
			g.TotalMachine += *p.MachineCount
		}
		if p.Status == "active" {
			g.ActiveCount++
		}
		g.Products = append(g.Products, p)
	}

	out := make([]ProductTypeGroup, 0, len(order))
	for _, t := range order {
		out = append(out, *acc[t])
	}
	return out
}

// GetSupplierProductsGrouped 供给方工作台: 按商品类型分组 + 每组统计 (C-03)。
func (s *Service) GetSupplierProductsGrouped(supplierID int64) ([]ProductTypeGroup, error) {
	list, err := s.repo.GetProductsBySupplier(supplierID)
	if err != nil {
		return nil, err
	}
	return GroupProductsByType(list), nil
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
	Duration         int   `json:"duration"` // 计费周期数: hourly=小时 daily=天 weekly=周 monthly=月; perpetual 忽略并强制为 1
	ComplianceAgreed bool  `json:"compliance_agreed"`
}

// ValidateOrderParams 校验并归一化下单数量与时长。纯函数, 是资金安全的第一道闸门。
// 返回归一化后的 (quantity, duration)。
//
// 修复的历史漏洞:
//  1. Quantity 负数 -> totalFen 为负, 且 DecrProductStock 的 `stock = stock - ?` 配
//     `stock >= ?` 会凭空增加库存并通过校验。
//  2. Duration = 0 -> 总价 0, 白嫖。
//  3. 超大值 -> int64 溢出。
func ValidateOrderParams(p *Product, quantity, duration int) (int, int, error) {
	return validateOrderParams(p, quantity, duration, true)
}

// ValidateRenewParams 校验续租参数。规则与下单完全一致, 但**不校验库存**:
// 续租是在买家已持有的同一批卡上延长租期, 不再占用新库存。
// (下单时库存已扣减, 若此处再校验, 会出现"卡已卖光 -> 老客户无法续租"的错误拒绝。)
func ValidateRenewParams(p *Product, quantity, duration int) (int, int, error) {
	return validateOrderParams(p, quantity, duration, false)
}

func validateOrderParams(p *Product, quantity, duration int, checkStock bool) (int, int, error) {
	if p == nil {
		return 0, 0, fmt.Errorf("product not found")
	}

	// 面议商品禁止在线下单。
	if p.PriceNegotiable || p.ProductType == ProductTypeColocation {
		return 0, 0, fmt.Errorf("该商品为面议商品，请通过询价联系")
	}
	if p.UnitPrice <= 0 {
		return 0, 0, fmt.Errorf("该商品未设置有效单价，暂不支持在线下单")
	}

	minOrder := p.MinOrder
	if minOrder < 1 {
		minOrder = 1
	}
	if quantity < minOrder {
		return 0, 0, fmt.Errorf("购买数量不能少于起订量 %d", minOrder)
	}
	if quantity > MaxOrderQuantity {
		return 0, 0, fmt.Errorf("购买数量超出上限 %d", MaxOrderQuantity)
	}
	if p.Stock > 0 && checkStock && quantity > p.Stock {
		return 0, 0, fmt.Errorf("insufficient stock")
	}

	// 买断/永久使用权: 时长概念不存在, 强制归一为 1, 忽略客户端传值。
	if p.PricingMode == PricingPerpetual {
		return quantity, 1, nil
	}

	unit := DurationUnit(p.PricingMode)
	minDuration := p.MinDuration
	if minDuration < 1 {
		minDuration = 1
	}
	if duration < minDuration {
		return 0, 0, fmt.Errorf("租期不能少于最短租期 %d%s", minDuration, unit)
	}
	maxDuration := MaxDurationFor(p.PricingMode)
	if duration > maxDuration {
		return 0, 0, fmt.Errorf("租期超出上限 %d%s", maxDuration, unit)
	}
	return quantity, duration, nil
}

// CalcOrderAmount 计算订单总额与平台佣金(单位: 分)。全程 int64, 每步乘法都做溢出检查。
func CalcOrderAmount(unitPriceFen int64, quantity, duration int, feeRateBp int64) (totalFen int64, feeFen int64, err error) {
	if unitPriceFen <= 0 {
		return 0, 0, fmt.Errorf("单价必须大于 0")
	}
	if quantity <= 0 {
		return 0, 0, fmt.Errorf("购买数量必须大于 0")
	}
	if duration <= 0 {
		return 0, 0, fmt.Errorf("租期必须大于 0")
	}

	q := int64(quantity)
	d := int64(duration)

	step := unitPriceFen * q
	if step/unitPriceFen != q {
		return 0, 0, fmt.Errorf("订单金额计算溢出，请减少购买数量")
	}
	total := step * d
	if total/step != d {
		return 0, 0, fmt.Errorf("订单金额计算溢出，请缩短租期")
	}
	if total <= 0 {
		return 0, 0, fmt.Errorf("订单金额必须大于 0")
	}
	if total > MaxOrderTotalFen {
		return 0, 0, fmt.Errorf("订单金额超出单笔上限(100 亿元)，请拆单或联系商务")
	}

	fee := total / 10000 * feeRateBp
	// total/10000 的余数部分单独算, 保证佣金结果与 total*rate/10000 一致又不溢出。
	fee += (total % 10000) * feeRateBp / 10000
	if fee < 0 || fee > total {
		return 0, 0, fmt.Errorf("平台佣金计算异常")
	}
	return total, fee, nil
}

func (s *Service) PlaceOrder(buyerID int64, req PlaceOrderReq) (*Order, error) {
	p, err := s.repo.GetProductByID(req.ProductID)
	if err != nil {
		return nil, fmt.Errorf("product not found")
	}
	if p.Status != "active" {
		return nil, fmt.Errorf("product not available")
	}

	qty, dur, err := ValidateOrderParams(p, req.Quantity, req.Duration)
	if err != nil {
		return nil, err
	}
	totalFen, feeFen, err := CalcOrderAmount(p.UnitPrice, qty, dur, s.feeRate)
	if err != nil {
		return nil, err
	}

	expires := time.Now().Add(15 * time.Minute)
	orderNo := "ORD" + time.Now().Format("20060102150405") + uuid.New().String()[:6]

	o := &Order{
		OrderNo:          orderNo,
		BuyerID:          buyerID,
		ProductID:        req.ProductID,
		Quantity:         qty,
		Duration:         dur,
		UnitPrice:        p.UnitPrice,
		TotalAmount:      totalFen,
		PlatformFee:      feeFen,
		Status:           "pending_payment",
		PaymentExpires:   &expires,
		ComplianceAgreed: req.ComplianceAgreed,
	}

	tx, err := s.db.Beginx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	if err := s.repo.DecrProductStock(tx, p.ID, qty); err != nil {
		return nil, fmt.Errorf("insufficient stock")
	}
	if err := s.repo.CreateOrderTx(tx, o); err != nil {
		return nil, err
	}
	return o, tx.Commit()
}

func (s *Service) GetOrder(orderNo string) (*Order, error) {
	o, err := s.repo.GetOrderByNo(orderNo)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, fmt.Errorf("order not found")
	}
	return o, nil
}

func (s *Service) GetOrderByID(id int64) (*Order, error) {
	return s.repo.GetOrderByID(id)
}

// CanAccessOrder 判断某用户是否有权查看该订单: 买家本人 / 商品所属供给方 / 运营。
func (s *Service) CanAccessOrder(userID int64, o *Order, isAdmin bool) (bool, error) {
	if o == nil {
		return false, fmt.Errorf("order not found")
	}
	if isAdmin || o.BuyerID == userID {
		return true, nil
	}
	p, err := s.repo.GetProductByID(o.ProductID)
	if err != nil {
		return false, err
	}
	return p.SupplierID == userID, nil
}

func (s *Service) PayOrder(orderNo string) error {
	return s.repo.UpdateOrderStatus(orderNo, "paid")
}

// Provisioning: supplier confirms they're setting up
func (s *Service) ProvisioningOrder(orderNo string) error {
	return s.repo.UpdateOrderStatus(orderNo, "provisioning")
}

func (s *Service) CompleteOrder(orderNo string) error {
	return s.repo.UpdateOrderStatus(orderNo, "completed")
}

// CancelOrder 取消订单并吊销访问凭证 (C-06)。
func (s *Service) CancelOrder(orderNo string) error {
	if err := s.repo.UpdateOrderStatus(orderNo, "cancelled"); err != nil {
		return err
	}
	return s.revokeAccessByOrderNo(orderNo)
}

// FreezeOrder 冻结订单(风控/违规)。冻结意味着买家不应再访问算力,
// 因此必须同步吊销访问凭证 —— 与 C-06「订单到期/退款/冻结 → 凭证自动失效」一致。
func (s *Service) FreezeOrder(orderNo string) error {
	if err := s.repo.UpdateOrderStatus(orderNo, "frozen"); err != nil {
		return err
	}
	return s.revokeAccessByOrderNo(orderNo)
}

func (s *Service) ListBuyerOrders(f OrderListFilter) ([]BuyerOrder, int64, error) {
	return s.repo.ListBuyerOrders(f)
}

func (s *Service) ListSupplierOrders(supplierID int64, status string, page, pageSize int) ([]Order, int64, error) {
	return s.repo.ListSupplierOrders(supplierID, status, page, pageSize)
}

// ===== Delivery + 访问凭证 (T-017, C-06) =====

// DeliverInfo 供给方回填的交付信息。整体加密后存 credential_encrypted, 绝不落明文。
type DeliverInfo struct {
	IpAddress      string `json:"ip_address"`
	SshPort        int    `json:"ssh_port"`
	Username       string `json:"username"`
	Password       string `json:"password"`
	CredentialNote string `json:"credential_note"`
}

// AccessCredential 返回给调用方的访问凭证视图。Value 是否脱敏由调用入口决定。
type AccessCredential struct {
	AccessKey   string     `json:"access_key"`
	AccessValue string     `json:"access_value"`
	Status      string     `json:"access_status"`
	ExpiresAt   *time.Time `json:"access_expires_at"`
	RevokedAt   *time.Time `json:"revoked_at"`
	Masked      bool       `json:"masked"`
}

// GenerateAccessKey 生成访问凭证标识: "ak-" + 32 位随机 hex。使用 crypto/rand。
func GenerateAccessKey() (string, error) {
	h, err := crypto.RandomHex(accessKeyRandLen)
	if err != nil {
		return "", err
	}
	return AccessKeyPrefix + h, nil
}

// GenerateAccessValue 生成 48 位随机 hex 的访问凭证明文。使用 crypto/rand。
func GenerateAccessValue() (string, error) {
	return crypto.RandomHex(accessValRandLen)
}

// IsValidAccessKey 校验 access_key 格式: "ak-" + 32 位小写 hex。
func IsValidAccessKey(k string) bool {
	if !strings.HasPrefix(k, AccessKeyPrefix) {
		return false
	}
	body := k[len(AccessKeyPrefix):]
	if len(body) != accessKeyRandLen*2 {
		return false
	}
	for i := 0; i < len(body); i++ {
		c := body[i]
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// MaskAccessValue 脱敏: 只保留前 4 后 4, 中间固定打码。
// 长度不足 8 时全部打码, 避免短串泄露全部内容。
func MaskAccessValue(v string) string {
	if v == "" {
		return ""
	}
	if len(v) <= 8 {
		return strings.Repeat("*", len(v))
	}
	return v[:4] + "********" + v[len(v)-4:]
}

// Deliver 保留原签名以兼容既有调用: 只带 IP 的极简交付。
// 注意: 交付信息同样走加密存储, 未配置密钥时返回明确错误。
func (s *Service) Deliver(orderNo string, ipAddress string) error {
	_, err := s.DeliverWithAccess(0, orderNo, DeliverInfo{IpAddress: ipAddress}, true)
	return err
}

// DeliverWithAccess 供给方回填交付信息 -> 平台生成访问凭证 -> 加密存储 (C-06)。
// skipOwnerCheck 仅供内部/兼容调用; 对外必须传 false 并带真实 supplierID。
// 未配置加密密钥时返回 crypto.ErrKeyNotConfigured, 不做任何明文降级。
func (s *Service) DeliverWithAccess(supplierID int64, orderNo string, info DeliverInfo, skipOwnerCheck bool) (*AccessCredential, error) {
	if len(s.credentialKey) == 0 {
		return nil, crypto.ErrKeyNotConfigured
	}

	o, err := s.repo.GetOrderByNo(orderNo)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, fmt.Errorf("order not found")
	}

	p, err := s.repo.GetProductByID(o.ProductID)
	if err != nil {
		return nil, err
	}
	if !skipOwnerCheck && p.SupplierID != supplierID {
		return nil, fmt.Errorf("无权操作该订单: 商品不属于当前供给方")
	}
	if o.Status != "paid" && o.Status != "provisioning" {
		return nil, fmt.Errorf("invalid status transition")
	}

	infoJSON, err := json.Marshal(info)
	if err != nil {
		return nil, err
	}
	credEnc, err := crypto.Encrypt(string(infoJSON), s.credentialKey)
	if err != nil {
		return nil, err
	}

	accessKey, err := GenerateAccessKey()
	if err != nil {
		return nil, err
	}
	accessVal, err := GenerateAccessValue()
	if err != nil {
		return nil, err
	}
	accessEnc, err := crypto.Encrypt(accessVal, s.credentialKey)
	if err != nil {
		return nil, err
	}

	// 永久使用权无到期时间; 其余按租期换算, 到期后由 RevokeExpiredAccess 吊销。
	// duration 是计费周期数, 必须走 LeaseEndAt 换算, 否则 monthly 订单的凭证会在几小时后就失效。
	var expiresAt *time.Time
	if p.PricingMode != PricingPerpetual {
		e := LeaseEndAt(time.Now(), p.PricingMode, o.Duration)
		expiresAt = &e
	}

	d := &OrderDelivery{
		OrderID:              o.ID,
		CredentialEncrypted:  credEnc,
		AccessKey:            accessKey,
		AccessValueEncrypted: accessEnc,
		AccessStatus:         AccessStatusGenerated,
		AccessExpiresAt:      expiresAt,
	}
	if err := s.repo.SaveDeliveryWithAccess(d); err != nil {
		return nil, err
	}
	if err := s.repo.UpdateOrderStatus(orderNo, "provisioning"); err != nil {
		return nil, err
	}

	// 回给供给方的也是脱敏值: 明文只在买家 reveal 时下发并留审计。
	return &AccessCredential{
		AccessKey: accessKey, AccessValue: MaskAccessValue(accessVal),
		Status: AccessStatusGenerated, ExpiresAt: expiresAt, Masked: true,
	}, nil
}

const (
	errOrderNotConfirmable    = "订单当前状态不可确认签收"
	errDeliveryNotConfirmable = "订单尚未生成可签收的交付凭证"
)

func validateDeliveryConfirmation(o *Order, d *OrderDelivery, buyerID int64) error {
	if o == nil {
		return fmt.Errorf("order not found")
	}
	if o.BuyerID != buyerID {
		return fmt.Errorf("无权操作该订单: 订单不属于当前买家")
	}
	if o.Status != "provisioning" {
		return fmt.Errorf(errOrderNotConfirmable)
	}
	if d == nil || d.AccessStatus != AccessStatusGenerated || d.ConfirmedByBuyer {
		return fmt.Errorf(errDeliveryNotConfirmable)
	}
	return nil
}

// ConfirmDelivery 仅允许订单本人签收已生成凭证的 provisioning 订单。
// 两次状态更新在同一事务内并带条件，防止并发退款/改单后又被覆盖为 active。
func (s *Service) ConfirmDelivery(buyerID int64, orderNo string) error {
	o, err := s.repo.GetOrderByNo(orderNo)
	if err != nil {
		return err
	}
	if o == nil {
		return fmt.Errorf("order not found")
	}
	d, err := s.repo.GetDeliveryByOrder(o.ID)
	if err != nil {
		return err
	}
	if err := validateDeliveryConfirmation(o, d, buyerID); err != nil {
		return err
	}
	p, err := s.repo.GetProductByID(o.ProductID)
	if err != nil || p == nil {
		return fmt.Errorf("product not found")
	}

	now := time.Now()
	var leaseEnd interface{}
	if p.PricingMode != PricingPerpetual {
		leaseEnd = LeaseEndAt(now, p.PricingMode, o.Duration)
	}

	tx, err := s.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	result, err := tx.Exec(`UPDATE order_deliveries d
		JOIN orders o ON o.id=d.order_id
		SET d.confirmed_by_buyer=1, d.buyer_confirmed_at=?, d.access_status='delivered'
		WHERE d.order_id=? AND d.access_status='generated' AND d.confirmed_by_buyer=0
		AND o.buyer_id=? AND o.status='provisioning'`, now, o.ID, buyerID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf(errDeliveryNotConfirmable)
	}

	result, err = tx.Exec(`UPDATE orders SET status='active', lease_start_at=?, lease_end_at=?
		WHERE id=? AND buyer_id=? AND status='provisioning'`, now, leaseEnd, o.ID, buyerID)
	if err != nil {
		return err
	}
	if changed, err := result.RowsAffected(); err != nil || changed != 1 {
		if err != nil {
			return err
		}
		return fmt.Errorf(errOrderNotConfirmable)
	}
	return tx.Commit()
}

func (s *Service) GetDelivery(orderID int64) (*OrderDelivery, error) {
	return s.repo.GetDeliveryByOrder(orderID)
}

// GetAccessCredentialMasked 买家/供给方查看访问凭证, 明文一律脱敏 (C-06)。
func (s *Service) GetAccessCredentialMasked(userID int64, orderNo string) (*AccessCredential, error) {
	o, d, err := s.loadOrderAccess(userID, orderNo)
	if err != nil {
		return nil, err
	}
	_ = o

	ac := &AccessCredential{
		AccessKey: d.AccessKey, Status: d.AccessStatus,
		ExpiresAt: d.AccessExpiresAt, RevokedAt: d.RevokedAt, Masked: true,
	}
	if d.AccessValueEncrypted == "" {
		return ac, nil
	}
	// 脱敏也需要先解密才知道明文长度与首尾; 密钥未配置时如实报错, 不返回假数据。
	plain, err := crypto.Decrypt(d.AccessValueEncrypted, s.credentialKey)
	if err != nil {
		return nil, err
	}
	ac.AccessValue = MaskAccessValue(plain)
	return ac, nil
}

// RevealAccessCredential 返回完整访问凭证明文, 必须写 audit_logs (C-06)。
func (s *Service) RevealAccessCredential(userID int64, orderNo, ip string) (*AccessCredential, error) {
	o, d, err := s.loadOrderAccess(userID, orderNo)
	if err != nil {
		return nil, err
	}

	if d.AccessStatus == AccessStatusRevoked {
		return nil, fmt.Errorf("访问凭证已吊销，无法查看")
	}
	if d.AccessStatus == AccessStatusNone || d.AccessValueEncrypted == "" {
		return nil, fmt.Errorf("访问凭证尚未生成，请等待供给方完成交付")
	}
	if d.AccessExpiresAt != nil && d.AccessExpiresAt.Before(time.Now()) {
		return nil, fmt.Errorf("访问凭证已过期，请联系供给方或申请续期")
	}

	plain, err := crypto.Decrypt(d.AccessValueEncrypted, s.credentialKey)
	if err != nil {
		return nil, err
	}

	// 审计写失败必须让整个操作失败: 无法留痕的明文下发不允许发生。
	if err := s.repo.CreateAuditLog(userID, "reveal_access_credential", "order", o.ID,
		"", "access_key="+d.AccessKey, ip); err != nil {
		return nil, fmt.Errorf("审计日志写入失败，已阻止凭证明文下发: %w", err)
	}

	return &AccessCredential{
		AccessKey: d.AccessKey, AccessValue: plain, Status: d.AccessStatus,
		ExpiresAt: d.AccessExpiresAt, RevokedAt: d.RevokedAt, Masked: false,
	}, nil
}

// loadOrderAccess 载入订单与交付记录, 并校验访问归属(买家本人或商品所属供给方)。
func (s *Service) loadOrderAccess(userID int64, orderNo string) (*Order, *OrderDelivery, error) {
	if len(s.credentialKey) == 0 {
		return nil, nil, crypto.ErrKeyNotConfigured
	}
	o, err := s.repo.GetOrderByNo(orderNo)
	if err != nil {
		return nil, nil, err
	}
	if o == nil {
		return nil, nil, fmt.Errorf("order not found")
	}

	if o.BuyerID != userID {
		p, err := s.repo.GetProductByID(o.ProductID)
		if err != nil {
			return nil, nil, err
		}
		if p.SupplierID != userID {
			return nil, nil, fmt.Errorf("无权查看该订单的访问凭证")
		}
	}

	d, err := s.repo.GetDeliveryByOrder(o.ID)
	if err != nil {
		return nil, nil, err
	}
	if d == nil {
		return nil, nil, fmt.Errorf("delivery not found")
	}
	return o, d, nil
}

// RevokeExpiredAccess 供定时任务调用: 吊销所有已过期凭证, 返回处理条数。
func (s *Service) RevokeExpiredAccess() (int64, error) {
	return s.repo.RevokeExpiredAccess()
}

func (s *Service) revokeAccessByOrderNo(orderNo string) error {
	o, err := s.repo.GetOrderByNo(orderNo)
	if err != nil {
		return err
	}
	if o == nil {
		return fmt.Errorf("order not found")
	}
	return s.repo.RevokeAccessByOrder(o.ID)
}

// ===== C-05 资源同步与盘点 =====

type ResourceSyncReq struct {
	ProductID  int64  `json:"product_id"`
	StockAfter int    `json:"stock_after"`
	Reason     string `json:"reason"`
}

// ComputeAnomaly 盘点异常判定 (C-05): stock_before > 0 且 |diff|/stock_before > 0.3 -> 异常。
// 纯整数比较, 无浮点误差。
func ComputeAnomaly(stockBefore, stockAfter int) (diff int, anomaly bool) {
	diff = stockAfter - stockBefore
	if stockBefore <= 0 {
		return diff, false
	}
	abs := diff
	if abs < 0 {
		abs = -abs
	}
	// abs/before > 3/10  <=>  abs*10 > before*3
	return diff, abs*anomalyRatioDen > stockBefore*anomalyRatioNum
}

// SyncResource 资源盘点: 主动盘(admin)/被动报(supplier)。
//
// 资金与数量安全要点:
//   - stock_after >= 0, 否则拒绝
//   - SELECT ... FOR UPDATE 锁 product 行后再读 stock_before, 防并发盘点丢更新
//   - 快照写入与 products.stock 更新在同一事务, 要么都成要么都不成
//   - 只有商品所属 supplier 或运营(isAdmin)可操作
func (s *Service) SyncResource(operatorID int64, isAdmin bool, syncType string, req ResourceSyncReq) (*ResourceSnapshot, error) {
	if syncType != "active" && syncType != "passive" {
		return nil, fmt.Errorf("盘点类型非法: 仅支持 active(平台主动盘点)/passive(机房主动上报)")
	}
	if req.ProductID <= 0 {
		return nil, fmt.Errorf("product_id 必填")
	}
	if req.StockAfter < 0 {
		return nil, fmt.Errorf("盘点后库存不能为负数")
	}
	if req.StockAfter > MaxOrderQuantity {
		return nil, fmt.Errorf("盘点后库存超出上限 %d", MaxOrderQuantity)
	}
	if len(req.Reason) > 256 {
		return nil, fmt.Errorf("盘点原因不得超过 256 字")
	}

	tx, err := s.db.Beginx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// 行锁后再读, stock_before 才是可信基线。
	p, err := s.repo.LockProductForUpdate(tx, req.ProductID)
	if err != nil {
		return nil, err
	}

	// 归属校验: 非运营时必须是商品所属供给方。
	if !isAdmin && p.SupplierID != operatorID {
		return nil, fmt.Errorf("无权盘点该商品: 商品不属于当前供给方")
	}
	// 被动上报只能由供给方发起, 主动盘点只能由运营发起, 防止身份与类型错配。
	if syncType == "active" && !isAdmin {
		return nil, fmt.Errorf("主动盘点仅限平台运营操作")
	}
	if syncType == "passive" && p.SupplierID != operatorID {
		return nil, fmt.Errorf("被动上报仅限商品所属机房操作")
	}

	diff, anomaly := ComputeAnomaly(p.Stock, req.StockAfter)

	snap := &ResourceSnapshot{
		ProductID: p.ID, SupplierID: p.SupplierID, SyncType: syncType,
		StockBefore: p.Stock, StockAfter: req.StockAfter, Diff: diff,
		Reason: req.Reason, OperatorID: operatorID, Anomaly: anomaly,
	}
	id, err := s.repo.CreateSnapshotTx(tx, snap)
	if err != nil {
		return nil, err
	}
	if err := s.repo.SetProductStockTx(tx, p.ID, req.StockAfter); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	// C-05: 盘点差异超阈值必须触发告警, 不能只在库里留个标记。
	// v1 先落到结构化日志(可被日志平台/风控消费), 后续接入风控告警通道见 Q-CR-05。
	if anomaly {
		slog.Warn("资源盘点异常",
			"product_id", p.ID, "supplier_id", p.SupplierID, "sync_type", syncType,
			"stock_before", p.Stock, "stock_after", req.StockAfter, "diff", diff,
			"reason", req.Reason, "operator_id", operatorID,
			"threshold", "|diff|/stock_before > 30%",
		)
	}

	snap.ID = id
	snap.CreatedAt = time.Now()
	return snap, nil
}

// ListResourceSyncs 盘点记录。product_id > 0 时按商品查(需校验归属), 否则查该供给方全部。
func (s *Service) ListResourceSyncs(operatorID int64, isAdmin bool, productID int64, page, pageSize int) ([]ResourceSnapshot, int64, error) {
	if productID > 0 {
		p, err := s.repo.GetProductByID(productID)
		if err != nil {
			return nil, 0, err
		}
		if !isAdmin && p.SupplierID != operatorID {
			return nil, 0, fmt.Errorf("无权查看该商品的盘点记录")
		}
		return s.repo.ListSnapshotsByProduct(productID, page, pageSize)
	}
	return s.repo.ListSnapshotsBySupplier(operatorID, page, pageSize)
}

// ===== Renewal (T-018) =====

func (s *Service) RenewOrder(buyerID int64, orderNo string, additionalDuration int) (*Order, error) {
	o, err := s.repo.GetOrderByNo(orderNo)
	if err != nil {
		return nil, err
	}
	if o == nil {
		return nil, fmt.Errorf("order not found")
	}
	if o.BuyerID != buyerID {
		return nil, fmt.Errorf("无权续租该订单")
	}
	if o.Status != "active" {
		return nil, fmt.Errorf("order not active")
	}

	p, err := s.repo.GetProductByID(o.ProductID)
	if err != nil {
		return nil, err
	}
	// 买断/永久使用权无续租概念。
	if p.PricingMode == PricingPerpetual {
		return nil, fmt.Errorf("买断商品为永久使用权，无需续租")
	}

	// 续租沿用原订单数量, 仅时长来自请求; 与下单同一套校验与溢出保护, 但不重复占用库存。
	qty, dur, err := ValidateRenewParams(p, o.Quantity, additionalDuration)
	if err != nil {
		return nil, err
	}
	totalFen, feeFen, err := CalcOrderAmount(o.UnitPrice, qty, dur, s.feeRate)
	if err != nil {
		return nil, err
	}

	newOrderNo := "REN" + time.Now().Format("20060102150405") + uuid.New().String()[:6]
	expires := time.Now().Add(15 * time.Minute)

	no := &Order{
		OrderNo:          newOrderNo,
		BuyerID:          buyerID,
		ProductID:        o.ProductID,
		Quantity:         qty,
		Duration:         dur,
		UnitPrice:        o.UnitPrice,
		TotalAmount:      totalFen,
		PlatformFee:      feeFen,
		Status:           "pending_payment",
		PaymentExpires:   &expires,
		ComplianceAgreed: true,
	}
	return no, s.repo.CreateOrderTx(nil, no)
}

// ===== Refund (T-019) =====

// RequestRefund 买家申请退款。必须校验订单归属:
// 否则任意登录买家都能把他人正常使用中的订单打成 refunding 状态。
func (s *Service) RequestRefund(buyerID int64, orderNo string) error {
	o, err := s.repo.GetOrderByNo(orderNo)
	if err != nil {
		return err
	}
	if o == nil {
		return fmt.Errorf("order not found")
	}
	if o.BuyerID != buyerID {
		return fmt.Errorf("无权操作该订单: 订单不属于当前买家")
	}
	if o.Status != "active" && o.Status != "paid" && o.Status != "provisioning" {
		return fmt.Errorf("invalid status transition")
	}
	return s.repo.UpdateOrderStatus(orderNo, "refunding")
}

// CompleteRefund 退款完成: 同时吊销访问凭证 (C-06)。
func (s *Service) CompleteRefund(orderNo string) error {
	if err := s.repo.UpdateOrderStatus(orderNo, "refunded"); err != nil {
		return err
	}
	return s.revokeAccessByOrderNo(orderNo)
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

// AdminUpdateOrderStatus 运营改单。改为 cancelled/refunded 时同步吊销访问凭证。
func (s *Service) AdminUpdateOrderStatus(orderNo string, status string) error {
	if err := s.repo.UpdateOrderStatus(orderNo, status); err != nil {
		return err
	}
	if status == "cancelled" || status == "refunded" {
		return s.revokeAccessByOrderNo(orderNo)
	}
	return nil
}
