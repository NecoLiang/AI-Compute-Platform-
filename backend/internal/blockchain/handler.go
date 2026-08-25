package blockchain

import (
	"tokenfactory/pkg/response"

	"github.com/gin-gonic/gin"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/blockchain/verify", h.Verify)
	r.GET("/blockchain/attestations/:target_type/:target_id", h.GetAttestation)
}

// RegisterAdminRoutes 运维路由: 死信补推 (REQ-H-021)。挂在 admin RBAC 组下。
func (h *Handler) RegisterAdminRoutes(r *gin.RouterGroup) {
	r.POST("/admin/blockchain/requeue-failed", h.RequeueFailed)
}

// Verify GET /blockchain/verify?type=order&id=ORDxxx (REQ-H-030)
func (h *Handler) Verify(c *gin.Context) {
	targetType, targetID := c.Query("type"), c.Query("id")
	if targetType == "" || targetID == "" {
		response.Success(c, &VerifyResult{Verified: false, Note: "缺少 type/id 参数"})
		return
	}
	result, err := h.svc.Verify(c.Request.Context(), targetType, targetID)
	if err != nil {
		response.Success(c, &VerifyResult{Verified: false, Note: "查询失败"})
		return
	}
	response.Success(c, result)
}

func (h *Handler) GetAttestation(c *gin.Context) {
	att, err := h.svc.GetAttestation(c.Param("target_type"), c.Param("target_id"))
	if err != nil || att == nil {
		response.Success(c, nil)
		return
	}
	response.Success(c, att)
}

func (h *Handler) RequeueFailed(c *gin.Context) {
	n, err := h.svc.RequeueFailed()
	if err != nil {
		response.Success(c, gin.H{"requeued": 0, "error": "补推失败"})
		return
	}
	response.Success(c, gin.H{"requeued": n})
}
