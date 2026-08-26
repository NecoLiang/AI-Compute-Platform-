package notification

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
	r.GET("/notifications", h.List)
	r.POST("/notifications/:id/read", h.MarkRead)
	r.POST("/notifications/read-all", h.MarkAllRead)
	r.DELETE("/notifications/:id", h.Delete)
}

func (h *Handler) List(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, unread, counts, err := h.svc.List(c.GetInt64("user_id"), c.Query("type"), page, pageSize)
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, gin.H{
		"list": list, "total": total, "unread": unread, "type_counts": counts,
		"page": page, "page_size": pageSize,
	})
}

func (h *Handler) MarkRead(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, errcode.ParamInvalid, "invalid notification id")
		return
	}
	if err := h.svc.MarkRead(c.GetInt64("user_id"), id); err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, nil)
}

func (h *Handler) MarkAllRead(c *gin.Context) {
	n, err := h.svc.MarkAllRead(c.GetInt64("user_id"))
	if err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.Success(c, gin.H{"marked": n})
}

func (h *Handler) Delete(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, errcode.ParamInvalid, "invalid notification id")
		return
	}
	if err := h.svc.Delete(c.GetInt64("user_id"), id); err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, nil)
}

// ErrToCode 与兄弟包约定一致。
func ErrToCode(err error) int {
	if err == nil {
		return errcode.Success
	}
	msg := err.Error()
	switch msg {
	case "notification not found":
		return errcode.NotFound
	case "invalid type", "invalid notification payload":
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
