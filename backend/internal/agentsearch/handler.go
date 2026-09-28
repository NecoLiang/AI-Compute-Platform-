package agentsearch

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
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
	r.POST("/market/agent-search/stream", h.Stream)
}

// Search POST /market/agent-search 市场页智能选型入口。
func (h *Handler) Search(c *gin.Context) {
	query, ok := readSearchQuery(c)
	if !ok {
		return
	}
	result, err := h.svc.Search(c.Request.Context(), c.GetInt64("user_id"), query)
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

func readSearchQuery(c *gin.Context) (string, bool) {
	var req struct {
		Query string `json:"query" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "query 必填")
		return "", false
	}
	req.Query = strings.TrimSpace(req.Query)
	if req.Query == "" {
		response.Error(c, errcode.ParamInvalid, "query 必填")
		return "", false
	}
	if utf8.RuneCountInString(req.Query) > maxQueryRunes {
		response.Error(c, errcode.ParamInvalid, "需求描述过长(≤500字)")
		return "", false
	}
	return req.Query, true
}

// Stream preserves the JSON endpoint while delivering the public summary early.
func (h *Handler) Stream(c *gin.Context) {
	query, ok := readSearchQuery(c)
	if !ok {
		return
	}
	started := false
	emit := func(event string, payload any) error {
		if err := c.Request.Context().Err(); err != nil {
			return err
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if !started {
			c.Header("Content-Type", "text/event-stream; charset=utf-8")
			c.Header("Cache-Control", "no-cache, no-transform")
			c.Header("X-Accel-Buffering", "no")
		}
		if _, err = fmt.Fprintf(c.Writer, "event: %s\ndata: %s\n\n", event, encoded); err != nil {
			return err
		}
		started = true
		c.Writer.Flush()
		return nil
	}
	result, err := h.svc.SearchStream(c.Request.Context(), c.GetInt64("user_id"), query, func(text string) error {
		return emit("summary", gin.H{"text": text})
	})
	if c.Request.Context().Err() != nil {
		return
	}
	if err != nil {
		code, message := errcode.InternalError, "算力评估暂不可用，请稍后重试"
		if errors.Is(err, ErrRateLimited) {
			code, message = errcode.TooManyRequests, "请求较频繁，请稍后再试"
		}
		// Do not log upstream payloads or partial user/model text.
		slog.Warn("算力评估流未完成", "error_type", fmt.Sprintf("%T", err), "request_id", c.GetString("request_id"))
		if !started {
			response.Error(c, code, message)
			return
		}
		_ = emit("error", response.Response{Code: code, Message: message, RequestID: c.GetString("request_id")})
		return
	}
	_ = emit("result", response.Response{Code: 0, Message: "success", Data: result, RequestID: c.GetString("request_id")})
}
