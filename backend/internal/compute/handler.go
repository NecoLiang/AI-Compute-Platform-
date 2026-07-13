package compute

import (
	"strconv"
	"tokenfactory/pkg/middleware"
	"tokenfactory/pkg/response"
	"tokenfactory/pkg/errcode"

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
	r.POST("/supplier/products", h.CreateProduct)
	r.GET("/supplier/orders", h.ListSupplierOrders)
	r.POST("/orders/:id/deliver", h.Deliver)
}

func (h *Handler) RegisterBuyerRoutes(r *gin.RouterGroup) {
	r.POST("/orders", h.PlaceOrder)
	r.GET("/orders", h.ListBuyerOrders)
	r.GET("/orders/:id", h.GetOrder)
	r.POST("/orders/:id/confirm", h.ConfirmDelivery)
	r.POST("/orders/:id/renew", h.RenewOrder)
	r.POST("/orders/:id/refund", h.RequestRefund)
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
}

// ---- Products ----

func (h *Handler) ListProducts(c *gin.Context) {
	f := ProductFilter{
		GpuModel:    c.Query("gpu_model"),
		Region:      c.Query("region"),
		PricingMode: c.Query("pricing_mode"),
		Sort:        c.DefaultQuery("sort", "created_at_desc"),
	}
	f.Page, _ = strconv.Atoi(c.DefaultQuery("page", "1"))
	f.PageSize, _ = strconv.Atoi(c.DefaultQuery("page_size", "20"))
	f.PriceMin, _ = strconv.ParseInt(c.Query("price_min"), 10, 64)
	f.PriceMax, _ = strconv.ParseInt(c.Query("price_max"), 10, 64)
	list, total, err := h.svc.ListProducts(f)
	if err != nil { response.Error(c, errcode.InternalError, err.Error()); return }
	var result []gin.H
	for _, p := range list {
		result = append(result, productToJSON(&p))
	}
	response.SuccessPage(c, result, total, f.Page, f.PageSize)
}

func (h *Handler) GetProduct(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	p, credit, err := h.svc.GetProduct(id)
	if err != nil { response.Error(c, errcode.NotFound, "商品不存在"); return }
	response.Success(c, gin.H{
		"product": productToJSON(p),
		"credit":  credit,
	})
}

func (h *Handler) CreateProduct(c *gin.Context) {
	var req CreateProductReq
	if err := c.ShouldBindJSON(&req); err != nil { response.Error(c, errcode.ParamInvalid, err.Error()); return }
	id, err := h.svc.CreateProduct(c.GetInt64("user_id"), req)
	if err != nil { response.Error(c, errcode.InternalError, err.Error()); return }
	response.Success(c, gin.H{"id": id})
}

func (h *Handler) GetMyProducts(c *gin.Context) {
	list, _ := h.svc.GetSupplierProducts(c.GetInt64("user_id"))
	var result []gin.H
	for _, p := range list { result = append(result, productToJSON(&p)) }
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
	if err := c.ShouldBindJSON(&req); err != nil { response.Error(c, errcode.ParamInvalid, err.Error()); return }
	id, err := h.svc.SubmitQualification(c.GetInt64("user_id"), req.QualType, req.CertName, req.CertNumber, req.CertURL, nil)
	if err != nil { response.Error(c, errcode.InternalError, err.Error()); return }
	response.Success(c, gin.H{"id": id})
}

func (h *Handler) GetMyQualifications(c *gin.Context) {
	list, _ := h.svc.GetMyQualifications(c.GetInt64("user_id"))
	response.Success(c, list)
}

// ---- Orders ----

func (h *Handler) PlaceOrder(c *gin.Context) {
	var req PlaceOrderReq
	if err := c.ShouldBindJSON(&req); err != nil { response.Error(c, errcode.ParamInvalid, err.Error()); return }
	o, err := h.svc.PlaceOrder(c.GetInt64("user_id"), req)
	if err != nil { response.Error(c, ErrToCode(err), err.Error()); return }
	response.Success(c, gin.H{
		"order_no": o.OrderNo, "total_amount": o.TotalAmount,
		"platform_fee": o.PlatformFee, "status": o.Status,
		"payment_expires_at": o.PaymentExpires,
	})
}

func (h *Handler) GetOrder(c *gin.Context) {
	idOrNo := c.Param("id")
	o, err := h.svc.GetOrder(idOrNo)
	if err != nil || o == nil {
		// try by numeric ID
		id, _ := strconv.ParseInt(idOrNo, 10, 64)
		o, err = h.svc.GetOrderByID(id)
	}
	if err != nil || o == nil { response.Error(c, errcode.NotFound, "订单不存在"); return }
	delivery, _ := h.svc.GetDelivery(o.ID)
	credit, _ := h.svc.GetCreditScore(o.BuyerID)
	_ = credit // credit used in full response
	response.Success(c, gin.H{
		"order": o, "delivery": delivery,
	})
}

func (h *Handler) ListBuyerOrders(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, _ := h.svc.ListBuyerOrders(c.GetInt64("user_id"), c.Query("status"), page, pageSize)
	response.SuccessPage(c, list, total, page, pageSize)
}

func (h *Handler) ListSupplierOrders(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, _ := h.svc.ListSupplierOrders(c.GetInt64("user_id"), c.Query("status"), page, pageSize)
	response.SuccessPage(c, list, total, page, pageSize)
}

func (h *Handler) Deliver(c *gin.Context) {
	idOrNo := c.Param("id")
	var req struct {
		IpAddress       string `json:"ip_address"`
		SshPort         int    `json:"ssh_port"`
		Username        string `json:"username"`
		CredentialNote  string `json:"credential_note"`
	}
	if err := c.ShouldBindJSON(&req); err != nil { response.Error(c, errcode.ParamInvalid, err.Error()); return }
	// TODO: 接入 AES-256 加密后存储交付凭证
	// 所需信息: 加密密钥（32字节，存储在 KMS 中，不入库不硬编码）
	// credentialJSON := json.Marshal(req)
	// encrypted := aes256.Encrypt(credentialJSON, key)
	if err := h.svc.Deliver(idOrNo, req.IpAddress); err != nil { response.Error(c, errcode.InternalError, err.Error()); return }
	response.Success(c, nil)
}

func (h *Handler) ConfirmDelivery(c *gin.Context) {
	idOrNo := c.Param("id")
	if err := h.svc.ConfirmDelivery(idOrNo); err != nil { response.Error(c, errcode.InternalError, err.Error()); return }
	response.Success(c, nil)
}

func (h *Handler) RenewOrder(c *gin.Context) {
	idOrNo := c.Param("id")
	var req struct {
		Duration int `json:"duration"`
	}
	if err := c.ShouldBindJSON(&req); err != nil { response.Error(c, errcode.ParamInvalid, err.Error()); return }
	o, err := h.svc.RenewOrder(c.GetInt64("user_id"), idOrNo, req.Duration)
	if err != nil { response.Error(c, errcode.InternalError, err.Error()); return }
	response.Success(c, gin.H{"order_no": o.OrderNo, "total_amount": o.TotalAmount})
}

func (h *Handler) RequestRefund(c *gin.Context) {
	idOrNo := c.Param("id")
	if err := h.svc.RequestRefund(idOrNo); err != nil { response.Error(c, ErrToCode(err), err.Error()); return }
	response.Success(c, nil)
}

// ---- Admin ----

func (h *Handler) ListPendingQualifications(c *gin.Context) {
	list, _ := h.svc.GetPendingQualifications()
	response.Success(c, list)
}

func (h *Handler) ApproveQualification(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if err := h.svc.ApproveQualification(id); err != nil { response.Error(c, errcode.InternalError, err.Error()); return }
	response.Success(c, nil)
}

func (h *Handler) RejectQualification(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	var req struct {
		Reason string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil { response.Error(c, errcode.ParamInvalid, err.Error()); return }
	if err := h.svc.RejectQualification(id, req.Reason); err != nil { response.Error(c, errcode.InternalError, err.Error()); return }
	response.Success(c, nil)
}

func (h *Handler) ApproveProduct(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if err := h.svc.ApproveProduct(id); err != nil { response.Error(c, errcode.InternalError, err.Error()); return }
	response.Success(c, nil)
}

func (h *Handler) RejectProduct(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if err := h.svc.RejectProduct(id); err != nil { response.Error(c, errcode.InternalError, err.Error()); return }
	response.Success(c, nil)
}

func (h *Handler) OfflineProduct(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if err := h.svc.OfflineProduct(id); err != nil { response.Error(c, errcode.InternalError, err.Error()); return }
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
	if err := c.ShouldBindJSON(&req); err != nil { response.Error(c, errcode.ParamInvalid, err.Error()); return }
	if err := h.svc.AdminUpdateOrderStatus(idOrNo, req.Status); err != nil { response.Error(c, errcode.InternalError, err.Error()); return }
	response.Success(c, nil)
}

func productToJSON(p *Product) gin.H {
	return gin.H{
		"id": p.ID, "supplier_id": p.SupplierID, "gpu_model": p.GpuModel,
		"card_count": p.CardCount, "cpu_spec": p.CpuSpec, "memory_spec": p.MemorySpec,
		"storage_spec": p.StorageSpec, "bandwidth_spec": p.BandwidthSpec,
		"delivery_mode": p.DeliveryMode, "pricing_mode": p.PricingMode,
		"unit_price": p.UnitPrice, "available_hours": p.AvailableHours,
		"stock": p.Stock, "min_order": p.MinOrder, "min_duration": p.MinDuration,
		"region": p.Region, "status": p.Status, "self_operated": p.SelfOperated,
	}
}

var _ = middleware.RBAC
