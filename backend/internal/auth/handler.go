package auth

import (
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

func (h *Handler) RegisterRoutes(r *gin.RouterGroup, jwtSecret string, rdb interface{}) {
	r.POST("/auth/register", h.Register)
	r.POST("/auth/login", h.Login)
	r.POST("/auth/refresh", h.RefreshToken)
	r.POST("/auth/logout", h.Logout)
	r.GET("/auth/me", h.Me)
}

func (h *Handler) Register(c *gin.Context) {
	var req RegisterReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	userID, err := h.svc.Register(req)
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, gin.H{"user_id": userID})
}

func (h *Handler) Login(c *gin.Context) {
	var req LoginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	tokens, user, err := h.svc.Login(req)
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, gin.H{
		"access_token":  tokens.AccessToken,
		"refresh_token": tokens.RefreshToken,
		"expires_in":    tokens.ExpiresIn,
		"user":          user,
	})
}

func (h *Handler) RefreshToken(c *gin.Context) {
	var req struct {
		RefreshToken string `json:"refresh_token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	tokens, err := h.svc.RefreshToken(req.RefreshToken)
	if err != nil {
		response.Error(c, errcode.Unauthorized, err.Error())
		return
	}
	response.Success(c, tokens)
}

func (h *Handler) Logout(c *gin.Context) {
	token := c.GetHeader("Authorization")
	if len(token) > 7 {
		token = token[7:]
	}
	if err := h.svc.Logout(c.Request.Context(), token); err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.Success(c, nil)
}

func (h *Handler) Me(c *gin.Context) {
	userID, _ := c.Get("user_id")
	uid := userID.(int64)
	info, err := h.svc.GetUser(uid)
	if err != nil {
		response.Error(c, errcode.NotFound, "用户不存在")
		return
	}

	// Override roles from token claims for speed
	if roles, ok := c.Get("roles"); ok {
		if rl, ok := roles.([]string); ok {
			info.Roles = rl
		}
	}
	info.Phone = c.GetString("phone")
	response.Success(c, info)
}
