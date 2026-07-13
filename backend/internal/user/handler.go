package user

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

func (h *Handler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/user/profile", h.GetProfile)
	r.PUT("/user/profile", h.UpdateProfile)
	r.POST("/user/kyc/personal", h.SubmitPersonalKYC)
	r.POST("/user/kyc/enterprise", h.SubmitEnterprise)
	r.GET("/user/kyc/status", h.GetKYCStatus)
	r.POST("/user/roles", h.ApplyRole)
}

func (h *Handler) GetProfile(c *gin.Context) {
	response.Success(c, gin.H{"user_id": c.GetInt64("user_id"), "phone": c.GetString("phone")})
}

func (h *Handler) UpdateProfile(c *gin.Context) {
	response.Success(c, gin.H{"message": "更新成功"})
}

func (h *Handler) SubmitPersonalKYC(c *gin.Context) {
	var req PersonalKYCReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	if err := h.svc.SubmitPersonalKYC(c.GetInt64("user_id"), req); err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, nil)
}

func (h *Handler) SubmitEnterprise(c *gin.Context) {
	var req EnterpriseReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	if err := h.svc.SubmitEnterprise(c.GetInt64("user_id"), req); err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, nil)
}

func (h *Handler) GetKYCStatus(c *gin.Context) {
	status, err := h.svc.GetKYCStatus(c.GetInt64("user_id"))
	if err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.Success(c, status)
}

func (h *Handler) ApplyRole(c *gin.Context) {
	var req struct {
		Role string `json:"role" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	if err := h.svc.ApplyRole(c.GetInt64("user_id"), req.Role); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	response.Success(c, nil)
}
