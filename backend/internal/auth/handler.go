package auth

import (
	"log/slog"
	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/response"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	svc             *Service
	captchaVerifier *CapVerifier
}

func NewHandler(svc *Service, captchaVerifier *CapVerifier) *Handler {
	return &Handler{svc: svc, captchaVerifier: captchaVerifier}
}

func (h *Handler) RegisterRoutes(r *gin.RouterGroup) {
	r.POST("/auth/captcha/verify", h.VerifyCaptcha)
	r.POST("/auth/sms/code", h.SendSMSCode)
	r.POST("/auth/sms/login", h.SMSLogin)
	r.POST("/auth/register", h.Register)
	r.POST("/auth/refresh", h.RefreshToken)
	r.POST("/auth/logout", h.Logout)
	r.GET("/auth/me", h.Me)
}

func (h *Handler) VerifyCaptcha(c *gin.Context) {
	var req struct {
		CaptchaToken string `json:"captcha_token" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	if err := h.captchaVerifier.Verify(c.Request.Context(), req.CaptchaToken); err != nil {
		writeAuthError(c, err)
		return
	}
	response.Success(c, nil)
}

func (h *Handler) SendSMSCode(c *gin.Context) {
	var req SendSMSCodeReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	if err := h.captchaVerifier.Verify(c.Request.Context(), req.CaptchaToken); err != nil {
		writeAuthError(c, err)
		return
	}
	if err := h.svc.SendSMSCode(c.Request.Context(), req.Phone, req.Purpose, c.ClientIP()); err != nil {
		writeAuthError(c, err)
		return
	}
	response.Success(c, gin.H{"expires_in": int(h.svc.smsCodeTTL.Seconds())})
}

func (h *Handler) Register(c *gin.Context) {
	var req RegisterReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	userID, err := h.svc.Register(c.Request.Context(), req)
	if err != nil {
		writeAuthError(c, err)
		return
	}
	response.Success(c, gin.H{"user_id": userID})
}

func (h *Handler) SMSLogin(c *gin.Context) {
	var req SMSLoginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	tokens, user, err := h.svc.SMSLogin(c.Request.Context(), req)
	if err != nil {
		writeAuthError(c, err)
		return
	}
	response.Success(c, gin.H{
		"access_token":  tokens.AccessToken,
		"refresh_token": tokens.RefreshToken,
		"expires_in":    tokens.ExpiresIn,
		"user":          user,
	})
}

func (h *Handler) Login(c *gin.Context) {
	var req LoginReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	tokens, user, err := h.svc.Login(req)
	if err != nil {
		writeAuthError(c, err)
		return
	}
	response.Success(c, gin.H{
		"access_token":  tokens.AccessToken,
		"refresh_token": tokens.RefreshToken,
		"expires_in":    tokens.ExpiresIn,
		"user":          user,
	})
}

func writeAuthError(c *gin.Context, err error) {
	code := ErrToCode(err)
	message := err.Error()
	if code == errcode.InternalError {
		slog.Error("authentication request failed", "error", err)
		message = errcode.Message(code)
	}
	response.Error(c, code, message)
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
