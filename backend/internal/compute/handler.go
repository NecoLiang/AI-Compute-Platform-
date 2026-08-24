package compute

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/middleware"
	"tokenfactory/pkg/response"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) RegisterPublicRoutes(r *gin.RouterGroup) {
	r.GET("/products", h.ListProducts)
	r.GET("/products/:id", h.GetProduct)
}

func (h *Handler) RegisterSupplierRoutes(r *gin.RouterGroup) {
	r.GET("/supplier/qualifications", h.GetMyQualifications)
	r.POST("/supplier/qualifications", h.SubmitQualification)
	r.GET("/supplier/products", h.GetMyProducts)
	r.GET("/supplier/products/summary", h.GetMyProductsGrouped)
	r.POST("/supplier/products", h.CreateProduct)
	r.GET("/supplier/orders", h.ListSupplierOrders)
	r.GET("/supplier/resource-syncs", h.ListResourceSyncs)
	r.POST("/supplier/resource-syncs/passive", h.PassiveResourceSync)
	r.POST("/orders/:id/deliver", h.Deliver)
}

func (h *Handler) RegisterBuyerRoutes(r *gin.RouterGroup) {
	r.POST("/orders", h.PlaceOrder)
	r.GET("/orders", h.ListBuyerOrders)
	r.GET("/orders/:id", h.GetOrder)
	r.POST("/orders/:id/confirm", h.ConfirmDelivery)
	r.POST("/orders/:id/renew", h.RenewOrder)
	r.POST("/orders/:id/refund", h.RequestRefund)
	r.GET("/orders/:id/access-credential", h.GetAccessCredential)
	r.POST("/orders/:id/access-credential/reveal", h.RevealAccessCredential)
}

func (h *Handler) RegisterDevRoutes(r *gin.RouterGroup) {
	r.POST("/fixtures/buyer-orders", h.SeedBuyerOrders)
}

func (h *Handler) RegisterAdminRoutes(r *gin.RouterGroup) {
	r.GET("/admin/audits/qualifications", h.ListPendingQualifications)
	r.POST("/admin/audits/qualifications/:id/approve", h.ApproveQualification)
	r.POST("/admin/audits/qualifications/:id/reject", h.RejectQualification)
	r.POST("/admin/audits/products/:id/approve", h.ApproveProduct)
	r.POST("/admin/audits/products/:id/reject", h.RejectProduct)
	r.GET("/admin/products", h.AdminListProducts)
	r.PATCH("/admin/products/:id/offline", h.OfflineProduct)
	r.GET("/admin/orders", h.AdminListOrders)
	r.PATCH("/admin/orders/:id/status", h.AdminUpdateOrderStatus)
	r.POST("/admin/resource-syncs/active", h.ActiveResourceSync)
}

// ---- Products ----

func (h *Handler) ListProducts(c *gin.Context) {
	f := ProductFilter{
		Query:          c.Query("q"),
		ProductType:    c.Query("product_type"),
		GpuModel:       c.Query("gpu_model"),
		Region:         c.Query("region"),
		DeliveryMode:   c.Query("delivery_mode"),
		PricingMode:    c.Query("pricing_mode"),
		AvailableHours: c.Query("available_hours"),
		Sort:           c.DefaultQuery("sort", "created_at_desc"),
	}
	var err error
	if f.Page, err = positiveIntQuery(c, "page", 1); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	if f.PageSize, err = positiveIntQuery(c, "page_size", 20); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	if f.PriceMin, err = nonNegativeInt64Query(c, "price_min"); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	if f.PriceMax, err = nonNegativeInt64Query(c, "price_max"); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	if f.CardCountMin, err = nonNegativeIntQuery(c, "card_count_min"); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	f.Normalize()
	list, total, err := h.svc.ListProducts(f)
	if err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	var result []gin.H
	for _, p := range list {
		result = append(result, productToJSON(&p))
	}
	response.SuccessPage(c, result, total, f.Page, f.PageSize)
}

func positiveIntQuery(c *gin.Context, name string, fallback int) (int, error) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return fallback, nil
	}
	value, err := strconv.Atoi(raw)
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s 必须为正整数", name)
	}
	return value, nil
}

func nonNegativeIntQuery(c *gin.Context, name string) (int, error) {
	value, err := nonNegativeInt64Query(c, name)
	if err != nil {
		return 0, err
	}
	if int64(int(value)) != value {
		return 0, fmt.Errorf("%s 超出范围", name)
	}
	return int(value), nil
}

func nonNegativeInt64Query(c *gin.Context, name string) (int64, error) {
	raw := strings.TrimSpace(c.Query(name))
	if raw == "" {
		return 0, nil
	}
	value, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || value < 0 {
		return 0, fmt.Errorf("%s 必须为非负整数", name)
	}
	return value, nil
}

func (h *Handler) GetProduct(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	p, credit, err := h.svc.GetProduct(id)
	if err != nil {
		response.Error(c, errcode.NotFound, "商品不存在")
		return
	}
	response.Success(c, gin.H{
		"product": productToJSON(p),
		"credit":  credit,
	})
}

// CreateProduct 发布商品。差异化校验在 service 层做 (C-02), 校验失败回 40001 + 中文原因。
func (h *Handler) CreateProduct(c *gin.Context) {
	var req CreateProductReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	id, err := h.svc.CreateProduct(c.GetInt64("user_id"), req)
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, gin.H{"id": id})
}

func (h *Handler) GetMyProducts(c *gin.Context) {
	list, err := h.svc.GetSupplierProducts(c.GetInt64("user_id"))
	if err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	var result []gin.H
	for _, p := range list {
		result = append(result, productToJSON(&p))
	}
	response.Success(c, result)
}

// GetMyProductsGrouped 供给方工作台: 按 product_type 分组 + 每组统计 (C-03)。
// GET /supplier/products/summary
func (h *Handler) GetMyProductsGrouped(c *gin.Context) {
	groups, err := h.svc.GetSupplierProductsGrouped(c.GetInt64("user_id"))
	if err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	result := make([]gin.H, 0, len(groups))
	for _, g := range groups {
		items := make([]gin.H, 0, len(g.Products))
		for i := range g.Products {
			items = append(items, productToJSON(&g.Products[i]))
		}
		result = append(result, gin.H{
			"product_type": g.ProductType, "label": g.Label,
			"count": g.Count, "active_count": g.ActiveCount,
			"total_machine": g.TotalMachine, "total_card": g.TotalCard, "total_stock": g.TotalStock,
			"products": items,
		})
	}
	response.Success(c, result)
}

// ---- Qualifications ----

func (h *Handler) SubmitQualification(c *gin.Context) {
	var req struct {
		QualType   string `json:"qual_type"`
		CertName   string `json:"cert_name"`
		CertNumber string `json:"cert_number"`
		CertURL    string `json:"cert_url"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	id, err := h.svc.SubmitQualification(c.GetInt64("user_id"), req.QualType, req.CertName, req.CertNumber, req.CertURL, nil)
	if err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.Success(c, gin.H{"id": id})
}

func (h *Handler) GetMyQualifications(c *gin.Context) {
	list, _ := h.svc.GetMyQualifications(c.GetInt64("user_id"))
	response.Success(c, list)
}

// ---- Orders ----

func (h *Handler) PlaceOrder(c *gin.Context) {
	var req PlaceOrderReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	o, err := h.svc.PlaceOrder(c.GetInt64("user_id"), req)
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, gin.H{
		"order_no": o.OrderNo, "total_amount": o.TotalAmount,
		"platform_fee": o.PlatformFee, "status": o.Status,
		"payment_expires_at": o.PaymentExpires,
	})
}

func (h *Handler) GetOrder(c *gin.Context) {
	orderNo := c.Param("id")
	if !validOrderNo(orderNo) {
		response.Error(c, errcode.ParamInvalid, "订单编号无效")
		return
	}
	detail, err := h.svc.GetBuyerOrderDetail(c.GetInt64("user_id"), orderNo)
	if err != nil {
		if ErrToCode(err) == errcode.NotFound {
			response.Error(c, errcode.NotFound, "订单不存在")
			return
		}
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.Success(c, detail)
}

var orderNoPattern = regexp.MustCompile(`^(?:ORD|REN)[A-Za-z0-9-]{6,29}$`)

func validOrderNo(orderNo string) bool {
	return orderNoPattern.MatchString(orderNo)
}

func (h *Handler) SeedBuyerOrders(c *gin.Context) {
	orders, err := h.svc.SeedBuyerOrders(c.GetInt64("user_id"))
	if err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.Success(c, gin.H{"orders": orders, "count": len(orders)})
}

func (h *Handler) ListBuyerOrders(c *gin.Context) {
	page, err := positiveIntQuery(c, "page", 1)
	if err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	pageSize, err := positiveIntQuery(c, "page_size", 20)
	if err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	f := OrderListFilter{
		BuyerID: c.GetInt64("user_id"), Status: c.Query("status"), OrderNo: c.Query("order_no"),
		Page: page, PageSize: pageSize,
	}
	f.Normalize()
	list, total, err := h.svc.ListBuyerOrders(f)
	if err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.SuccessPage(c, list, total, f.Page, f.PageSize)
}

func (h *Handler) ListSupplierOrders(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, _ := h.svc.ListSupplierOrders(c.GetInt64("user_id"), c.Query("status"), page, pageSize)
	response.SuccessPage(c, list, total, page, pageSize)
}

// Deliver 供给方回填交付信息, 平台随即生成并加密存储访问凭证 (C-06)。
// 未配置 security.credential_key 时返回明确错误, 不会降级存明文。
func (h *Handler) Deliver(c *gin.Context) {
	idOrNo := c.Param("id")
	var req DeliverInfo
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	ac, err := h.svc.DeliverWithAccess(c.GetInt64("user_id"), idOrNo, req, false)
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, ac)
}

// GetAccessCredential 买家/供给方查看访问凭证, access_value 脱敏(前4后4) (C-06)。
// GET /orders/:id/access-credential
func (h *Handler) GetAccessCredential(c *gin.Context) {
	ac, err := h.svc.GetAccessCredentialMasked(c.GetInt64("user_id"), c.Param("id"))
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, ac)
}

// RevealAccessCredential 返回完整访问凭证明文, 强制写 audit_logs (C-06)。
// POST /orders/:id/access-credential/reveal
func (h *Handler) RevealAccessCredential(c *gin.Context) {
	ac, err := h.svc.RevealAccessCredential(c.GetInt64("user_id"), c.Param("id"), c.ClientIP())
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, ac)
}

// ---- 资源同步与盘点 (C-05) ----

// ListResourceSyncs GET /supplier/resource-syncs?product_id=
func (h *Handler) ListResourceSyncs(c *gin.Context) {
	productID, _ := strconv.ParseInt(c.Query("product_id"), 10, 64)
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, err := h.svc.ListResourceSyncs(c.GetInt64("user_id"), hasAdminRole(c), productID, page, pageSize)
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.SuccessPage(c, list, total, page, pageSize)
}

// PassiveResourceSync POST /supplier/resource-syncs/passive 机房被动上报。
func (h *Handler) PassiveResourceSync(c *gin.Context) {
	h.resourceSync(c, "passive")
}

// ActiveResourceSync POST /admin/resource-syncs/active 平台主动盘点。
func (h *Handler) ActiveResourceSync(c *gin.Context) {
	h.resourceSync(c, "active")
}

func (h *Handler) resourceSync(c *gin.Context, syncType string) {
	// 先取原始 body: stock_after 是绝对库存值而非增量, 缺字段时 JSON 零值 0 会被
	// 当成"清空库存", 必须区分"显式传 0"与"没传"。ShouldBindJSON 会消费 body,
	// 所以这里自己读一次再解析。
	body, err := io.ReadAll(c.Request.Body)
	if err != nil {
		response.Error(c, errcode.ParamInvalid, "读取请求体失败: "+err.Error())
		return
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		response.Error(c, errcode.ParamInvalid, "请求体不是合法 JSON: "+err.Error())
		return
	}
	if _, ok := raw["stock_after"]; !ok {
		response.Error(c, errcode.ParamInvalid, "stock_after 必填: 该字段为盘点后的绝对库存值, 不是增量")
		return
	}

	var req ResourceSyncReq
	if err := json.Unmarshal(body, &req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}

	snap, err := h.svc.SyncResource(c.GetInt64("user_id"), hasAdminRole(c), syncType, req)
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, snap)
}

// hasAdminRole 判断当前身份是否运营/管理员。盘点归属校验依赖它 (C-05)。
func hasAdminRole(c *gin.Context) bool {
	roles, _ := c.Get("roles")
	list, _ := roles.([]string)
	for _, r := range list {
		if r == "admin" || r == "operator" {
			return true
		}
	}
	return false
}

func (h *Handler) ConfirmDelivery(c *gin.Context) {
	idOrNo := c.Param("id")
	if err := h.svc.ConfirmDelivery(c.GetInt64("user_id"), idOrNo); err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, nil)
}

func (h *Handler) RenewOrder(c *gin.Context) {
	idOrNo := c.Param("id")
	var req struct {
		Duration int `json:"duration"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	o, err := h.svc.RenewOrder(c.GetInt64("user_id"), idOrNo, req.Duration)
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, gin.H{"order_no": o.OrderNo, "total_amount": o.TotalAmount, "platform_fee": o.PlatformFee})
}

func (h *Handler) RequestRefund(c *gin.Context) {
	idOrNo := c.Param("id")
	if err := h.svc.RequestRefund(c.GetInt64("user_id"), idOrNo); err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, nil)
}

// ---- Admin ----

func (h *Handler) ListPendingQualifications(c *gin.Context) {
	list, _ := h.svc.GetPendingQualifications()
	response.Success(c, list)
}

func (h *Handler) ApproveQualification(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if err := h.svc.ApproveQualification(id); err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.Success(c, nil)
}

func (h *Handler) RejectQualification(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	var req struct {
		Reason string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	if err := h.svc.RejectQualification(id, req.Reason); err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.Success(c, nil)
}

func (h *Handler) ApproveProduct(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if err := h.svc.ApproveProduct(id); err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.Success(c, nil)
}

func (h *Handler) RejectProduct(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if err := h.svc.RejectProduct(id); err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.Success(c, nil)
}

func (h *Handler) OfflineProduct(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if err := h.svc.OfflineProduct(id); err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.Success(c, nil)
}

func (h *Handler) AdminListOrders(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, _ := h.svc.ListAllOrders(c.Query("status"), page, pageSize)
	response.SuccessPage(c, list, total, page, pageSize)
}

func (h *Handler) AdminListProducts(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, _ := h.svc.ListAllProducts(c.Query("status"), page, pageSize)
	response.SuccessPage(c, list, total, page, pageSize)
}

func (h *Handler) AdminUpdateOrderStatus(c *gin.Context) {
	idOrNo := c.Param("id")
	var req struct {
		Status string `json:"status" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	if err := h.svc.AdminUpdateOrderStatus(idOrNo, req.Status); err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.Success(c, nil)
}

func productToJSON(p *Product) gin.H {
	return gin.H{
		"id": p.ID, "supplier_id": p.SupplierID, "product_type": p.ProductType,
		"gpu_model":  p.GpuModel,
		"card_count": p.CardCount, "machine_count": p.MachineCount,
		"total_pflops_approx": p.TotalPflopsApprox,
		"power_capacity_kw":   p.PowerCapacityKw, "rack_count": p.RackCount,
		"cpu_spec": p.CpuSpec, "memory_spec": p.MemorySpec,
		"storage_spec": p.StorageSpec, "bandwidth_spec": p.BandwidthSpec,
		"delivery_mode": p.DeliveryMode, "pricing_mode": p.PricingMode,
		"unit_price": p.UnitPrice, "price_negotiable": p.PriceNegotiable,
		"available_hours": p.AvailableHours,
		"stock":           p.Stock, "min_order": p.MinOrder, "min_duration": p.MinDuration,
		"region": p.Region, "status": p.Status, "self_operated": p.SelfOperated,
	}
}

var _ = middleware.RBAC
