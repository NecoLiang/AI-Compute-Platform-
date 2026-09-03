package admin

import (
	"strconv"
	"strings"
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
	r.GET("/admin/cms/notices", h.ListNotices)
	r.POST("/admin/cms/notices", h.CreateNotice)
}

func (h *Handler) ListAlerts(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, err := h.svc.ListAlerts(c.Query("level"), page, pageSize)
	if err != nil {
		response.Error(c, errcode.InternalError, "风控告警读取失败")
		return
	}
	response.SuccessPage(c, list, total, page, pageSize)
}

func (h *Handler) FreezeAlert(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, errcode.ParamInvalid, "告警编号不正确")
		return
	}
	if err := h.svc.ResolveAlert(id); err != nil {
		response.Error(c, errcode.InternalError, "告警处置失败")
		return
	}
	if err := h.svc.LogAudit(c.GetInt64("user_id"), "freeze_alert", "risk_alert", id, "", "resolved", c.ClientIP()); err != nil {
		response.Error(c, errcode.InternalError, "审计日志写入失败")
		return
	}
	response.Success(c, nil)
}

func (h *Handler) DismissAlert(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, errcode.ParamInvalid, "告警编号不正确")
		return
	}
	if err := h.svc.DismissAlert(id); err != nil {
		response.Error(c, errcode.InternalError, "告警处置失败")
		return
	}
	response.Success(c, nil)
}

func (h *Handler) ListAuditLogs(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, err := h.svc.ListAuditLogs(page, pageSize)
	if err != nil {
		response.Error(c, errcode.InternalError, "审计日志读取失败")
		return
	}
	response.SuccessPage(c, list, total, page, pageSize)
}

func (h *Handler) GetConfig(c *gin.Context) {
	config, err := h.svc.GetConfig()
	if err != nil {
		response.Error(c, errcode.InternalError, "系统配置读取失败")
		return
	}
	response.Success(c, config)
}

func (h *Handler) UpdateConfig(c *gin.Context) {
	var req struct {
		Key   string `json:"key"`
		Value string `json:"value"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "配置格式不正确")
		return
	}
	switch req.Key {
	case "trading_enabled":
		if req.Value != "true" && req.Value != "false" {
			response.Error(c, errcode.ParamInvalid, "交易开关参数不正确")
			return
		}
	case "fee_rate":
		feeRate, err := strconv.Atoi(req.Value)
		if err != nil || feeRate < 0 || feeRate > 10000 {
			response.Error(c, errcode.ParamInvalid, "费率需为 0–10000 的整数基点")
			return
		}
	default:
		response.Error(c, errcode.ParamInvalid, "不支持的配置项")
		return
	}
	if err := h.svc.UpdateConfig(c.GetInt64("user_id"), req.Key, req.Value, c.ClientIP()); err != nil {
		response.Error(c, errcode.InternalError, "系统配置保存失败")
		return
	}
	response.Success(c, nil)
}

func (h *Handler) ListUsers(c *gin.Context) {
	list, err := h.svc.ListUsers()
	if err != nil {
		response.Error(c, errcode.InternalError, "用户列表读取失败")
		return
	}
	response.Success(c, list)
}

func (h *Handler) FreezeUser(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, errcode.ParamInvalid, "用户编号不正确")
		return
	}
	if id == c.GetInt64("user_id") {
		response.Error(c, errcode.ParamInvalid, "不能冻结当前登录账户")
		return
	}
	if err := h.svc.FreezeUser(id); err != nil {
		response.Error(c, errcode.ParamInvalid, "账户不存在或已冻结")
		return
	}
	if err := h.svc.LogAudit(c.GetInt64("user_id"), "freeze_user", "user", id, "active", "frozen", c.ClientIP()); err != nil {
		response.Error(c, errcode.InternalError, "账户审计写入失败")
		return
	}
	response.Success(c, nil)
}

func (h *Handler) CreateNotice(c *gin.Context) {
	var req struct {
		Content string `json:"content"`
	}
	if err := c.ShouldBindJSON(&req); err != nil || strings.TrimSpace(req.Content) == "" {
		response.Error(c, errcode.ParamInvalid, "请填写公告内容")
		return
	}
	req.Content = strings.TrimSpace(req.Content)
	id, err := h.svc.CreateNotice(c.GetInt64("user_id"), req.Content, c.ClientIP())
	if err != nil {
		response.Error(c, errcode.InternalError, "公告写入失败")
		return
	}
	response.Success(c, gin.H{"id": id})
}

func (h *Handler) ListNotices(c *gin.Context) {
	list, err := h.svc.ListNotices()
	if err != nil {
		response.Error(c, errcode.InternalError, "公告读取失败")
		return
	}
	response.Success(c, list)
}

var _ = errcode.Success
