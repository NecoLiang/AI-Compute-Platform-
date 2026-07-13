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

func (h *Handler) Verify(c *gin.Context) {
	result, _ := h.svc.Verify(c.Query("type"), c.Query("id"))
	response.Success(c, result)
}

func (h *Handler) GetAttestation(c *gin.Context) {
	att, err := h.svc.repo.GetAttestation(c.Param("target_type"), c.Param("target_id"))
	if err != nil { response.Success(c, nil); return }
	response.Success(c, att)
}
