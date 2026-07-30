package equipment

import (
	"encoding/json"
	"strconv"
	"tokenfactory/pkg/errcode"
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
	r.GET("/equipments", h.ListEquipments)
	r.GET("/equipments/:id", h.GetEquipment)
}

func (h *Handler) RegisterBuyerRoutes(r *gin.RouterGroup) {
	// 注意: /equipments/inquiries 必须先于 /equipments/:id/inquiries 之外的同前缀路由考虑,
	// gin 的 httprouter 会把 "inquiries" 当作 :id 的静态兄弟节点处理, 二者不冲突。
	r.GET("/equipments/inquiries", h.MyInquiries)
	r.POST("/equipments/:id/inquiries", h.CreateInquiry)
}

func (h *Handler) RegisterVendorRoutes(r *gin.RouterGroup) {
	r.GET("/vendor/equipments", h.MyEquipments)
	r.POST("/vendor/equipments", h.CreateEquipment)
	r.PATCH("/vendor/equipments/:id/offline", h.OfflineEquipment)
	r.GET("/vendor/equipments/inquiries", h.VendorInquiries)
}

func (h *Handler) RegisterAdminRoutes(r *gin.RouterGroup) {
	r.GET("/admin/equipments", h.AdminListEquipments)
	r.POST("/admin/equipments/:id/approve", h.ApproveEquipment)
	r.POST("/admin/equipments/:id/reject", h.RejectEquipment)
}

// ---- Public ----

func (h *Handler) ListEquipments(c *gin.Context) {
	f := ProductFilter{
		EquipmentType: c.Query("equipment_type"),
		ConditionType: c.Query("condition_type"),
		Region:        c.Query("region"),
		Sort:          c.DefaultQuery("sort", "created_at_desc"),
	}
	f.Page, _ = strconv.Atoi(c.DefaultQuery("page", "1"))
	f.PageSize, _ = strconv.Atoi(c.DefaultQuery("page_size", "20"))
	f.PriceMin, _ = strconv.ParseInt(c.Query("price_min"), 10, 64)
	f.PriceMax, _ = strconv.ParseInt(c.Query("price_max"), 10, 64)
	f.Normalize()

	list, total, err := h.svc.ListProducts(f)
	if err != nil { response.Error(c, ErrToCode(err), err.Error()); return }
	result := make([]gin.H, 0, len(list))
	for i := range list {
		result = append(result, productToJSON(&list[i]))
	}
	response.SuccessPage(c, result, total, f.Page, f.PageSize)
}

func (h *Handler) GetEquipment(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 { response.Error(c, errcode.ParamInvalid, "设备ID不合法"); return }
	p, err := h.svc.GetProduct(id)
	if err != nil { response.Error(c, ErrToCode(err), err.Error()); return }
	response.Success(c, productToJSON(p))
}

// ---- Buyer ----

func (h *Handler) CreateInquiry(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 { response.Error(c, errcode.ParamInvalid, "设备ID不合法"); return }
	var req CreateInquiryReq
	if err := c.ShouldBindJSON(&req); err != nil { response.Error(c, errcode.ParamInvalid, err.Error()); return }
	inquiryID, err := h.svc.CreateInquiry(c.GetInt64("user_id"), id, req)
	if err != nil { response.Error(c, ErrToCode(err), err.Error()); return }
	response.Success(c, gin.H{
		"id": inquiryID,
		// 明确告知买家: 设备交易不走线上支付
		"note": "询价已提交, 平台将转交设备厂商线下联系。设备类交易不支持线上支付, 请通过线下验货与合同完成交易。",
	})
}

func (h *Handler) MyInquiries(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, err := h.svc.ListBuyerInquiries(c.GetInt64("user_id"), page, pageSize)
	if err != nil { response.Error(c, ErrToCode(err), err.Error()); return }
	response.SuccessPage(c, list, total, page, pageSize)
}

// ---- Vendor ----

func (h *Handler) MyEquipments(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, err := h.svc.GetVendorProducts(c.GetInt64("user_id"), c.Query("status"), page, pageSize)
	if err != nil { response.Error(c, ErrToCode(err), err.Error()); return }
	result := make([]gin.H, 0, len(list))
	for i := range list {
		result = append(result, productToJSON(&list[i]))
	}
	response.SuccessPage(c, result, total, page, pageSize)
}

func (h *Handler) CreateEquipment(c *gin.Context) {
	var req CreateProductReq
	if err := c.ShouldBindJSON(&req); err != nil { response.Error(c, errcode.ParamInvalid, err.Error()); return }
	id, err := h.svc.CreateProduct(c.GetInt64("user_id"), req)
	if err != nil { response.Error(c, ErrToCode(err), err.Error()); return }
	response.Success(c, gin.H{"id": id, "status": "pending"})
}

func (h *Handler) OfflineEquipment(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 { response.Error(c, errcode.ParamInvalid, "设备ID不合法"); return }
	if err := h.svc.OfflineProduct(id, c.GetInt64("user_id")); err != nil {
		response.Error(c, ErrToCode(err), err.Error()); return
	}
	response.Success(c, nil)
}

func (h *Handler) VendorInquiries(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, err := h.svc.ListVendorInquiries(c.GetInt64("user_id"), c.Query("status"), page, pageSize)
	if err != nil { response.Error(c, ErrToCode(err), err.Error()); return }
	response.SuccessPage(c, list, total, page, pageSize)
}

// ---- Admin ----

func (h *Handler) AdminListEquipments(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, err := h.svc.ListAllProducts(c.Query("status"), page, pageSize)
	if err != nil { response.Error(c, ErrToCode(err), err.Error()); return }
	result := make([]gin.H, 0, len(list))
	for i := range list {
		result = append(result, productToJSON(&list[i]))
	}
	response.SuccessPage(c, result, total, page, pageSize)
}

func (h *Handler) ApproveEquipment(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 { response.Error(c, errcode.ParamInvalid, "设备ID不合法"); return }
	if err := h.svc.ApproveProduct(id); err != nil { response.Error(c, ErrToCode(err), err.Error()); return }
	response.Success(c, nil)
}

func (h *Handler) RejectEquipment(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 { response.Error(c, errcode.ParamInvalid, "设备ID不合法"); return }
	var req struct {
		Reason string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil { response.Error(c, errcode.ParamInvalid, err.Error()); return }
	if err := h.svc.RejectProduct(id); err != nil { response.Error(c, ErrToCode(err), err.Error()); return }
	// NOTE: equipment_products 表没有 rejected_reason 字段, 驳回原因当前不落库。
	// 若需要留痕, 需在后续迁移中加列 (或复用 audit_logs 由运营侧统一写入)。
	response.Success(c, nil)
}

func productToJSON(p *EquipmentProduct) gin.H {
	var images []string
	if p.Images != nil && *p.Images != "" {
		// 忽略解析失败: 老数据/脏数据不应导致整个列表 500
		_ = json.Unmarshal([]byte(*p.Images), &images)
	}
	return gin.H{
		"id": p.ID, "vendor_id": p.VendorID, "title": p.Title,
		"equipment_type": p.EquipmentType, "brand": p.Brand, "model": p.Model,
		"condition_type": p.ConditionType, "manufacture_year": p.ManufactureYear,
		"usage_desc": p.UsageDesc, "quantity": p.Quantity,
		"unit_price": p.UnitPrice, "price_negotiable": p.PriceNegotiable,
		"region": p.Region, "description": p.Description, "images": images,
		"status": p.Status, "created_at": p.CreatedAt,
		// 设备类金额大且需线下验货议价, v1 明确不接在线支付, 只走询价撮合
		"online_payment_supported": false,
		"trade_mode":               "inquiry_only",
	}
}
