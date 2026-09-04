package catalog

import (
	"strconv"

	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/response"

	"github.com/gin-gonic/gin"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// RegisterPublicRoutes 型号下拉是发布/搜索页的公共基础数据, 不要求登录。
func (h *Handler) RegisterPublicRoutes(r *gin.RouterGroup) {
	r.GET("/gpu-catalog", h.PublicList)
}

func (h *Handler) RegisterAdminRoutes(r *gin.RouterGroup) {
	r.GET("/admin/gpu-catalog", h.AdminList)
	r.POST("/admin/gpu-catalog", h.Create)
	r.PUT("/admin/gpu-catalog/:id", h.Update)
}

func filterFrom(c *gin.Context) Filter {
	return Filter{
		Origin: c.Query("origin"),
		Grade:  c.Query("grade"),
		Vendor: c.Query("vendor"),
		Query:  c.Query("q"),
	}
}

// PublicList GET /gpu-catalog · 型号下拉(仅 enabled)。
func (h *Handler) PublicList(c *gin.Context) {
	list, err := h.svc.PublicList(filterFrom(c))
	if err != nil {
		response.Error(c, errcode.InternalError, "查询型号库失败")
		return
	}
	response.Success(c, gin.H{"list": list, "total": len(list)})
}

// AdminList GET /admin/gpu-catalog · 管理端全量(含 disabled 与 spec_source)。
func (h *Handler) AdminList(c *gin.Context) {
	list, err := h.svc.AdminList(filterFrom(c))
	if err != nil {
		response.Error(c, errcode.InternalError, "查询型号库失败")
		return
	}
	response.Success(c, gin.H{"list": list, "total": len(list)})
}

type modelReq struct {
	Vendor          string   `json:"vendor" binding:"required"`
	ModelName       string   `json:"model_name" binding:"required"`
	Origin          string   `json:"origin" binding:"required"`
	Grade           string   `json:"grade" binding:"required"`
	VRAMGB          *int     `json:"vram_gb"`
	VRAMType        *string  `json:"vram_type"`
	FP16TFLOPS      *float64 `json:"fp16_tflops"`
	Interconnect    *string  `json:"interconnect"`
	SecureCertified bool     `json:"secure_certified"`
	SpecSource      *string  `json:"spec_source"`
	Status          string   `json:"status"`
	SortWeight      int      `json:"sort_weight"`
}

func (r modelReq) toModel() *GPUModel {
	return &GPUModel{
		Vendor: r.Vendor, ModelName: r.ModelName, Origin: r.Origin, Grade: r.Grade,
		VRAMGB: r.VRAMGB, VRAMType: r.VRAMType, FP16TFLOPS: r.FP16TFLOPS,
		Interconnect: r.Interconnect, SecureCertified: r.SecureCertified,
		SpecSource: r.SpecSource, Status: r.Status, SortWeight: r.SortWeight,
	}
}

// Create POST /admin/gpu-catalog · 新增型号(默认 enabled, 即刻出现在下拉)。
func (h *Handler) Create(c *gin.Context) {
	var req modelReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "vendor / model_name / origin / grade 必填")
		return
	}
	m := req.toModel()
	m.Status = "enabled"
	if err := h.svc.Create(m); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	response.Success(c, m)
}

// Update PUT /admin/gpu-catalog/:id · 整体更新(含 status 上下架: disabled 即从下拉隐藏)。
func (h *Handler) Update(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	var req modelReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "vendor / model_name / origin / grade 必填")
		return
	}
	if req.Status == "" {
		req.Status = "enabled"
	}
	updated, err := h.svc.Update(id, req.toModel())
	if err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	response.Success(c, updated)
}
