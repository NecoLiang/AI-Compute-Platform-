package admin

import (
	"strconv"
	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/response"
	"github.com/gin-gonic/gin"
)

type Handler struct{ svc *Service }
func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) RegisterRoutes(r *gin.RouterGroup) {
	// Risk
	r.GET("/admin/risk/alerts", h.ListAlerts)
	r.POST("/admin/risk/alerts/:id/freeze", h.FreezeAlert)
	r.POST("/admin/risk/alerts/:id/dismiss", h.DismissAlert)
	// Audit
	r.GET("/admin/audit-logs", h.ListAuditLogs)
	// Config
	r.GET("/admin/config", h.GetConfig)
	r.PUT("/admin/config", h.UpdateConfig)
	// Users
	r.GET("/admin/users", h.ListUsers)
	r.PATCH("/admin/users/:id/freeze", h.FreezeUser)
	// CMS
	r.POST("/admin/cms/notices", h.CreateNotice)
}

func (h *Handler) ListAlerts(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, _ := h.svc.ListAlerts(c.Query("level"), page, pageSize)
	response.SuccessPage(c, list, total, page, pageSize)
}

func (h *Handler) FreezeAlert(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	h.svc.ResolveAlert(id)
	h.svc.LogAudit(c.GetInt64("user_id"), "freeze_alert", "risk_alert", id, "", "resolved", c.ClientIP())
	response.Success(c, nil)
}

func (h *Handler) DismissAlert(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	h.svc.DismissAlert(id)
	response.Success(c, nil)
}

func (h *Handler) ListAuditLogs(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, _ := h.svc.ListAuditLogs(page, pageSize)
	response.SuccessPage(c, list, total, page, pageSize)
}

var tradingEnabled = true

func (h *Handler) GetConfig(c *gin.Context) {
	response.Success(c, gin.H{"trading_enabled": tradingEnabled, "fee_rate": 500})
}

func (h *Handler) UpdateConfig(c *gin.Context) {
	var req struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	c.ShouldBindJSON(&req)
	if req.Key == "trading_enabled" {
		tradingEnabled = req.Value == "true"
	}
	h.svc.LogAudit(c.GetInt64("user_id"), "update_config", "config", 0, "", req.Key+"="+req.Value, c.ClientIP())
	response.Success(c, nil)
}

func (h *Handler) ListUsers(c *gin.Context) {
	response.Success(c, []string{})
}

func (h *Handler) FreezeUser(c *gin.Context) {
	id := c.Param("id")
	h.svc.LogAudit(c.GetInt64("user_id"), "freeze_user", "user", 0, "", id, c.ClientIP())
	response.Success(c, nil)
}

func (h *Handler) CreateNotice(c *gin.Context) {
	var req struct{ Content string `json:"content"` }
	c.ShouldBindJSON(&req)
	h.svc.LogAudit(c.GetInt64("user_id"), "create_notice", "cms", 0, "", req.Content, c.ClientIP())
	response.Success(c, nil)
}

var _ = errcode.Success
