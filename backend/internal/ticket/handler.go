package ticket

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

func (h *Handler) RegisterBuyerRoutes(r *gin.RouterGroup) {
	r.POST("/tickets", h.Create)
	r.GET("/tickets", h.List)
	r.GET("/tickets/:ticket_no", h.Detail)
	r.POST("/tickets/:ticket_no/messages", h.AppendMessage)
	r.POST("/tickets/:ticket_no/close", h.Close)
}

func (h *Handler) RegisterAdminRoutes(r *gin.RouterGroup) {
	r.GET("/admin/tickets", h.AdminList)
	r.GET("/admin/tickets/:id", h.AdminDetail)
	r.POST("/admin/tickets/:id/claim", h.AdminClaim)
	r.POST("/admin/tickets/:id/resolve", h.AdminResolve)
	r.POST("/admin/tickets/:id/close", h.AdminClose)
	r.POST("/admin/tickets/:id/messages", h.AdminAppendMessage)
}

// ---- Buyer ----

func (h *Handler) Create(c *gin.Context) {
	var req struct {
		OrderNo string `json:"order_no" binding:"required"`
		Type    string `json:"type" binding:"required"`
		Title   string `json:"title" binding:"required"`
		Content string `json:"content" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	t, err := h.svc.Create(c.GetInt64("user_id"), req.OrderNo, req.Type, req.Title, req.Content)
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, gin.H{"ticket_no": t.TicketNo, "status": t.Status})
}

func (h *Handler) List(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, err := h.svc.List(
		c.GetInt64("user_id"), c.Query("status"), c.Query("keyword"), page, pageSize)
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.SuccessPage(c, list, total, page, pageSize)
}

func (h *Handler) Detail(c *gin.Context) {
	t, msgs, err := h.svc.Detail(c.GetInt64("user_id"), c.Param("ticket_no"))
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, gin.H{"ticket": t, "messages": msgs})
}

func (h *Handler) AppendMessage(c *gin.Context) {
	var req struct {
		Content string `json:"content" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	if err := h.svc.AppendBuyerMessage(c.GetInt64("user_id"), c.Param("ticket_no"), req.Content); err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, nil)
}

func (h *Handler) Close(c *gin.Context) {
	if err := h.svc.Close(c.GetInt64("user_id"), c.Param("ticket_no")); err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, nil)
}

// ---- Admin ----

func (h *Handler) AdminList(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, err := h.svc.AdminList(c.Query("status"), c.Query("keyword"), page, pageSize)
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.SuccessPage(c, list, total, page, pageSize)
}

func (h *Handler) AdminDetail(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, errcode.ParamInvalid, "invalid ticket id")
		return
	}
	t, msgs, err := h.svc.AdminDetail(id)
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, gin.H{"ticket": t, "messages": msgs})
}

func (h *Handler) AdminClaim(c *gin.Context) {
	h.adminTransition(c, StatusProcessing)
}

func (h *Handler) AdminResolve(c *gin.Context) {
	h.adminTransition(c, StatusResolved)
}

func (h *Handler) AdminClose(c *gin.Context) {
	h.adminTransition(c, StatusClosed)
}

func (h *Handler) adminTransition(c *gin.Context, to string) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, errcode.ParamInvalid, "invalid ticket id")
		return
	}
	if err := h.svc.AdminTransition(id, to); err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, nil)
}

func (h *Handler) AdminAppendMessage(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, errcode.ParamInvalid, "invalid ticket id")
		return
	}
	var req struct {
		Content string `json:"content" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	if err := h.svc.AdminAppendMessage(c.GetInt64("user_id"), id, req.Content); err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, nil)
}

// ErrToCode 与 compute/invoice 约定一致: 已知短语映射稳定错误码,
// 中文提示视为可直接展示的业务校验错误, 其余归 50000。
func ErrToCode(err error) int {
	if err == nil {
		return errcode.Success
	}
	msg := err.Error()
	switch msg {
	case "ticket not found":
		return errcode.NotFound
	case "invalid status", "invalid status transition":
		return errcode.ParamInvalid
	}
	if strings.HasPrefix(msg, "无权") {
		return errcode.Forbidden
	}
	if containsCJK(msg) {
		return errcode.ParamInvalid
	}
	return errcode.InternalError
}

func containsCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}
