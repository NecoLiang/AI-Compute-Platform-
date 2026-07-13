package intermediary

import (
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
	if err := c.ShouldBindJSON(&req); err != nil { response.Error(c, errcode.ParamInvalid, err.Error()); return }
	id, err := h.svc.CreateLead(req)
	if err != nil { response.Error(c, errcode.InternalError, err.Error()); return }
	response.Success(c, gin.H{"id": id})
}

func (h *Handler) CreateFinanceLead(c *gin.Context) {
	var req CreateLeadReq
	c.ShouldBindJSON(&req)
	req.Type = "finance_lease"
	id, _ := h.svc.CreateLead(req)
	response.Success(c, gin.H{"id": id})
}

func (h *Handler) ListLeads(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, _ := h.svc.ListLeads(c.Query("status"), page, pageSize)
	response.SuccessPage(c, list, total, page, pageSize)
}

func (h *Handler) VendorLeads(c *gin.Context) {
	h.ListLeads(c)
}

func (h *Handler) AssignLead(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	var req struct{ AssigneeID int64 `json:"assignee_id"` }
	c.ShouldBindJSON(&req)
	h.svc.AssignLead(id, req.AssigneeID)
	response.Success(c, nil)
}

func (h *Handler) QuoteLead(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	h.svc.repo.UpdateLeadStatus(id, "quoted")
	response.Success(c, nil)
}

func (h *Handler) CloseDeal(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	var req CloseDealReq
	c.ShouldBindJSON(&req)
	h.svc.CloseDeal(id, req)
	response.Success(c, nil)
}

func (h *Handler) Commissions(c *gin.Context) {
	list, _ := h.svc.GetCommissions(c.GetInt64("user_id"))
	response.Success(c, list)
}
