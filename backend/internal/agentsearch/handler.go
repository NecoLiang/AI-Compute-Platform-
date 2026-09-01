package agentsearch

import (
	"errors"
	"log/slog"
	"unicode/utf8"

	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/response"

	"github.com/gin-gonic/gin"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// RegisterBuyerRoutes 挂在登录组: 智能搜索调用模型有成本, 匿名开放会被刷。
func (h *Handler) RegisterBuyerRoutes(r *gin.RouterGroup) {
	r.POST("/market/agent-search", h.Search)
}

// Search POST /market/agent-search 市场页智能选型入口。
func (h *Handler) Search(c *gin.Context) {
	var req struct {
		Query string `json:"query" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "query 必填")
		return
	}
	if utf8.RuneCountInString(req.Query) > maxQueryRunes {
		response.Error(c, errcode.ParamInvalid, "需求描述过长(≤500字)")
		return
	}
	result, err := h.svc.Search(c.Request.Context(), c.GetInt64("user_id"), req.Query)
	if err != nil {
		switch {
		case errors.Is(err, ErrRateLimited):
			response.Error(c, errcode.TooManyRequests, err.Error())
		case errors.Is(err, ErrAINotConfigured):
			response.Error(c, errcode.InternalError, err.Error())
		default:
			// 对外话术保持模糊, 但内部必须留全量错误 —— 排障靠它。
			slog.Error("智能搜索失败", "error", err, "request_id", c.GetString("request_id"))
			response.Error(c, errcode.InternalError, "智能搜索暂时不可用, 请稍后再试")
		}
		return
	}
	response.Success(c, result)
}
