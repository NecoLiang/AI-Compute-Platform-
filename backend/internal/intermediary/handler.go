package intermediary

import (
	"errors"
	"strconv"

	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/response"

	"github.com/gin-gonic/gin"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) RegisterPublicRoutes(r *gin.RouterGroup) {
	r.POST("/leads", h.CreateLead)
	r.POST("/finance/lease/contact", h.CreateFinanceLead)
}

func (h *Handler) RegisterVendorRoutes(r *gin.RouterGroup) {
	r.GET("/vendor/leads", h.VendorLeads)
	r.POST("/leads/:id/quote", h.QuoteLead)
	r.POST("/leads/:id/close", h.CloseDeal)
	r.GET("/commissions", h.Commissions)
}

func (h *Handler) RegisterAdminRoutes(r *gin.RouterGroup) {
	r.GET("/admin/leads", h.ListLeads)
	r.POST("/admin/leads/:id/assign", h.AssignLead)
}

func (h *Handler) CreateLead(c *gin.Context) {
	var req CreateLeadReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	if req.Type == "compute" {
		response.Error(c, errcode.ParamInvalid, "请从算力商品详情提交询价")
		return
	}
	id, err := h.svc.CreateLead(req)
	if err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.Success(c, gin.H{"id": id})
}

func (h *Handler) CreateFinanceLead(c *gin.Context) {
	var req CreateLeadReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	req.Type = "finance_lease"
	id, err := h.svc.CreateLead(req)
	if err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	response.Success(c, gin.H{"id": id})
}

// ListLeads 运营视角全量(挂在 admin RBAC 组)。
func (h *Handler) ListLeads(c *gin.Context) {
	h.listLeads(c, 0)
}

// VendorLeads 厂商视角: 只返回分配给当前账号的线索。
// 线索含留资人姓名/手机号等 PII, 不做归属过滤等于把全平台客户名单开放给任意厂商。
func (h *Handler) VendorLeads(c *gin.Context) {
	h.listLeads(c, c.GetInt64("user_id"))
}

func (h *Handler) listLeads(c *gin.Context, assigneeID int64) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, err := h.svc.ListLeads(assigneeID, c.Query("status"), page, pageSize)
	if err != nil {
		response.Error(c, errcode.InternalError, "线索读取失败")
		return
	}
	response.SuccessPage(c, list, total, page, pageSize)
}

func (h *Handler) AssignLead(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, errcode.ParamInvalid, "线索编号不正确")
		return
	}
	var req struct {
		AssigneeID int64 `json:"assignee_id"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || req.AssigneeID <= 0 {
		response.Error(c, errcode.ParamInvalid, "分配信息不正确")
		return
	}
	if err := h.svc.AssignLead(id, req.AssigneeID); err != nil {
		response.Error(c, errcode.InternalError, "线索分配失败")
		return
	}
	response.Success(c, nil)
}

func (h *Handler) QuoteLead(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, errcode.ParamInvalid, "线索编号不正确")
		return
	}
	if err := h.svc.QuoteLead(id, c.GetInt64("user_id")); err != nil {
		if errors.Is(err, ErrLeadNotActionable) {
			response.Error(c, errcode.Forbidden, err.Error())
			return
		}
		response.Error(c, errcode.InternalError, "报价状态更新失败")
		return
	}
	response.Success(c, nil)
}

func (h *Handler) CloseDeal(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, errcode.ParamInvalid, "线索编号不正确")
		return
	}
	var req CloseDealReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "成交信息不正确")
		return
	}
	if err := h.svc.CloseDeal(id, c.GetInt64("user_id"), req); err != nil {
		switch {
		case errors.Is(err, ErrLeadNotActionable):
			response.Error(c, errcode.Forbidden, err.Error())
		case isValidationErr(err):
			response.Error(c, errcode.ParamInvalid, err.Error())
		default:
			response.Error(c, errcode.InternalError, "成交登记失败")
		}
		return
	}
	response.Success(c, nil)
}

func (h *Handler) Commissions(c *gin.Context) {
	list, err := h.svc.GetCommissions(c.GetInt64("user_id"))
	if err != nil {
		response.Error(c, errcode.InternalError, "佣金读取失败")
		return
	}
	response.Success(c, list)
}

// isValidationErr 本包校验错误均为中文业务提示, 可直接展示; 其余按内部错误处理。
func isValidationErr(err error) bool {
	for _, r := range err.Error() {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}
