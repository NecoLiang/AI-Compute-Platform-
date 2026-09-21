package equipment

import (
	"encoding/json"
	"strconv"
	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/masking"
	"tokenfactory/pkg/response"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

// RegisterMarketRoutes 设备市场浏览: 注册到需登录的路由组——
// 与算力市场同口径, 市场列表/详情仅对登录用户开放(前端 middleware + 后端 AuthRequired 双层)。
func (h *Handler) RegisterMarketRoutes(r *gin.RouterGroup) {
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
	r.PUT("/vendor/equipments/:id", h.UpdateEquipment)
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
		result = append(result, buyerProductJSON(&list[i]))
	}
	response.SuccessPage(c, result, total, f.Page, f.PageSize)
}

func (h *Handler) GetEquipment(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 { response.Error(c, errcode.ParamInvalid, "设备ID不合法"); return }
	p, err := h.svc.GetProduct(id)
	if err != nil { response.Error(c, ErrToCode(err), err.Error()); return }
	response.Success(c, buyerProductJSON(p))
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
		result = append(result, vendorProductJSON(&list[i]))
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

// UpdateEquipment 供应方修改重提: 仅 draft(草稿/被驳回)可改, 重提后回 pending 重新审核。
func (h *Handler) UpdateEquipment(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 { response.Error(c, errcode.ParamInvalid, "设备ID不合法"); return }
	var req CreateProductReq
	if err := c.ShouldBindJSON(&req); err != nil { response.Error(c, errcode.ParamInvalid, err.Error()); return }
	if err := h.svc.UpdateProduct(c.GetInt64("user_id"), id, req); err != nil {
		response.Error(c, ErrToCode(err), err.Error()); return
	}
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
		result = append(result, vendorProductJSON(&list[i]))
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
	if err := h.svc.RejectProduct(id, req.Reason); err != nil { response.Error(c, ErrToCode(err), err.Error()); return }
	response.Success(c, nil)
}

func baseProductJSON(p *EquipmentProduct) gin.H {
	var images []string
	if p.Images != nil && *p.Images != "" {
		// 忽略解析失败: 老数据/脏数据不应导致整个列表 500
		_ = json.Unmarshal([]byte(*p.Images), &images)
	}
	return gin.H{
		"id": p.ID, "title": p.Title,
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

// buyerProductJSON 买家侧信息隔离: 不下发 vendor_id, 供应方企业名脱敏(北京***有限公司)。
func buyerProductJSON(p *EquipmentProduct) gin.H {
	j := baseProductJSON(p)
	j["vendor_name"] = masking.MaskCompanyName(p.VendorName)
	return j
}

// vendorProductJSON 供应方本人与运营端视角: 含 vendor_id 与驳回原因。
func vendorProductJSON(p *EquipmentProduct) gin.H {
	j := baseProductJSON(p)
	j["vendor_id"] = p.VendorID
	j["rejected_reason"] = p.RejectedReason
	return j
}
