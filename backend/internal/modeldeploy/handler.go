package modeldeploy

import (
	"encoding/json"
	"errors"
	"log/slog"
	"regexp"
	"strconv"
	"time"

	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/response"

	"github.com/gin-gonic/gin"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// RegisterAdminRoutes 运营(operator/admin)模型目录与真机验证记录(docs/27 §27.8)。
func (h *Handler) RegisterAdminRoutes(r *gin.RouterGroup) {
	r.GET("/admin/model-catalog", h.AdminList)
	r.POST("/admin/model-catalog", h.Create)
	r.GET("/admin/model-catalog/:id", h.AdminGet)
	r.PUT("/admin/model-catalog/:id", h.Update)
	r.POST("/admin/model-catalog/:id/verifications", h.AddVerification)
	r.POST("/admin/model-catalog/:id/activate", h.Activate)
	r.POST("/admin/model-catalog/:id/retire", h.Retire)
}

// RegisterMarketRoutes 登录用户可见的模型目录(市场路由组, 不进 Public 组)。
func (h *Handler) RegisterMarketRoutes(r *gin.RouterGroup) {
	r.GET("/models", h.ListModels)
}

// RegisterBuyerRoutes 买家对自己托管租约的部署方案判定。
func (h *Handler) RegisterBuyerRoutes(r *gin.RouterGroup) {
	r.GET("/orders/:id/deploy-plans", h.DeployPlans)
}

// writeErr 业务错误按其错误码返回; 其余为内部错误, 记日志后统一 50000(不外泄 SQL 细节)。
func writeErr(c *gin.Context, err error, internalMsg string) {
	var be *Error
	if errors.As(err, &be) {
		if be.Data != nil {
			c.JSON(200, response.Response{Code: be.Code, Message: be.Msg, Data: be.Data, RequestID: c.GetString("request_id")})
			return
		}
		response.Error(c, be.Code, be.Msg)
		return
	}
	slog.Error(internalMsg, "error", err, "path", c.FullPath())
	response.Error(c, errcode.InternalError, internalMsg)
}

func pathID(c *gin.Context) (int64, bool) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, errcode.ParamInvalid, "id 无效")
		return 0, false
	}
	return id, true
}

type entryReq struct {
	ModelKey             string          `json:"model_key"`
	DisplayName          string          `json:"display_name"`
	Source               string          `json:"source"`
	Repo                 string          `json:"repo"`
	Revision             string          `json:"revision"`
	Manifest             json.RawMessage `json:"manifest"`
	WeightsBytes         int64           `json:"weights_bytes"`
	Quant                string          `json:"quant"`
	MinComputeCapability string          `json:"min_compute_capability"`
	ValidTP              string          `json:"valid_tp"`
	KVBytesPerToken      int64           `json:"kv_bytes_per_token"`
	KVReplicatedAcrossTP bool            `json:"kv_replicated_across_tp"`
	RuntimeOverheadBytes int64           `json:"runtime_overhead_bytes"`
	MaxModelLen          int             `json:"max_model_len"`
	MinContextTokens     int             `json:"min_context_tokens"`
	Engine               string          `json:"engine"`
	EngineImage          string          `json:"engine_image"`
	MinDriverVersion     string          `json:"min_driver_version"`
	EngineArgs           json.RawMessage `json:"engine_args"`
	MetricsMap           json.RawMessage `json:"metrics_map"`
	StartupTimeoutS      int             `json:"startup_timeout_s"`
	License              string          `json:"license"`
	CommercialUse        string          `json:"commercial_use"`
}

func (r entryReq) toEntry() *ModelEntry {
	return &ModelEntry{
		ModelKey: r.ModelKey, DisplayName: r.DisplayName, Source: r.Source, Repo: r.Repo, Revision: r.Revision,
		Manifest: r.Manifest, WeightsBytes: r.WeightsBytes, Quant: r.Quant,
		MinComputeCapability: r.MinComputeCapability, ValidTP: r.ValidTP, KVBytesPerToken: r.KVBytesPerToken,
		KVReplicatedAcrossTP: r.KVReplicatedAcrossTP, RuntimeOverheadBytes: r.RuntimeOverheadBytes,
		MaxModelLen: r.MaxModelLen, MinContextTokens: r.MinContextTokens, Engine: r.Engine,
		EngineImage: r.EngineImage, MinDriverVersion: r.MinDriverVersion, EngineArgs: r.EngineArgs,
		MetricsMap: r.MetricsMap, StartupTimeoutS: r.StartupTimeoutS, License: r.License,
		CommercialUse: r.CommercialUse,
	}
}

// AdminList GET /admin/model-catalog?status=
func (h *Handler) AdminList(c *gin.Context) {
	list, err := h.svc.AdminList(c.Query("status"))
	if err != nil {
		writeErr(c, err, "查询模型目录失败")
		return
	}
	response.Success(c, gin.H{"list": list, "total": len(list)})
}

// AdminGet GET /admin/model-catalog/:id · 条目 + 验证记录
func (h *Handler) AdminGet(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	d, err := h.svc.AdminGet(id)
	if err != nil {
		writeErr(c, err, "查询模型目录失败")
		return
	}
	response.Success(c, d)
}

// Create POST /admin/model-catalog · 新建 draft 条目
func (h *Handler) Create(c *gin.Context) {
	var req entryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "请求体须为 JSON 对象")
		return
	}
	e, err := h.svc.Create(req.toEntry(), c.GetInt64("user_id"))
	if err != nil {
		writeErr(c, err, "新建模型目录条目失败")
		return
	}
	response.Success(c, e)
}

// Update PUT /admin/model-catalog/:id · 整体更新, 仅 draft
func (h *Handler) Update(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var req entryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "请求体须为 JSON 对象")
		return
	}
	e, err := h.svc.Update(id, req.toEntry())
	if err != nil {
		writeErr(c, err, "更新模型目录条目失败")
		return
	}
	response.Success(c, e)
}

type verificationReq struct {
	GPUModel              string    `json:"gpu_model"`
	Cards                 int       `json:"cards"`
	TP                    int       `json:"tp"`
	MaxModelLen           int       `json:"max_model_len"`
	MeasuredOverheadBytes *int64    `json:"measured_overhead_bytes"`
	Result                string    `json:"result"`
	Notes                 *string   `json:"notes"`
	TestedAt              time.Time `json:"tested_at"`
}

// AddVerification POST /admin/model-catalog/:id/verifications
func (h *Handler) AddVerification(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	var req verificationReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "请求体非法; tested_at 须为 RFC3339 时间")
		return
	}
	v := &Verification{
		GPUModel: req.GPUModel, Cards: req.Cards, TP: req.TP, MaxModelLen: req.MaxModelLen,
		MeasuredOverheadBytes: req.MeasuredOverheadBytes, Result: req.Result, Notes: req.Notes, TestedAt: req.TestedAt,
	}
	out, err := h.svc.AddVerification(id, v, c.GetInt64("user_id"))
	if err != nil {
		writeErr(c, err, "录入验证记录失败")
		return
	}
	response.Success(c, out)
}

// Activate POST /admin/model-catalog/:id/activate
func (h *Handler) Activate(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	e, err := h.svc.Activate(id)
	if err != nil {
		writeErr(c, err, "上线模型目录条目失败")
		return
	}
	response.Success(c, e)
}

// Retire POST /admin/model-catalog/:id/retire
func (h *Handler) Retire(c *gin.Context) {
	id, ok := pathID(c)
	if !ok {
		return
	}
	e, err := h.svc.Retire(id)
	if err != nil {
		writeErr(c, err, "退役模型目录条目失败")
		return
	}
	response.Success(c, e)
}

// ListModels GET /models
func (h *Handler) ListModels(c *gin.Context) {
	list, err := h.svc.PublicModels()
	if err != nil {
		writeErr(c, err, "查询模型目录失败")
		return
	}
	response.Success(c, gin.H{"list": list, "total": len(list)})
}

// utilPattern gpu_mem_util 以小数提交, 最多两位, 如 0.9 / 0.90 / 0.85。
var utilPattern = regexp.MustCompile(`^0\.([0-9])([0-9])?$`)

// parseGPUMemUtil 把 "0.85" 解析为 85。参数未提供(present=false)时取默认值 90;
// 一旦提供(包括空串、"0"、"0.0"), 就必须是 0.80–0.95 之间的合法小数, 否则 ok=false(40001),
// 不会把越界值静默替换为默认值。
func parseGPUMemUtil(s string, present bool) (int, bool) {
	if !present {
		return DefaultGPUMemUtilPct, true
	}
	m := utilPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, false
	}
	pct := int(m[1][0]-'0') * 10
	if m[2] != "" {
		pct += int(m[2][0] - '0')
	}
	if pct < MinGPUMemUtilPct || pct > MaxGPUMemUtilPct {
		return 0, false
	}
	return pct, true
}

// DeployPlans GET /orders/:id/deploy-plans?model_id=&gpu_mem_util=
func (h *Handler) DeployPlans(c *gin.Context) {
	modelID, err := strconv.ParseInt(c.Query("model_id"), 10, 64)
	if err != nil || modelID <= 0 {
		response.Error(c, errcode.ParamInvalid, "model_id 必填且须为正整数")
		return
	}
	pct, okU := parseGPUMemUtil(c.GetQuery("gpu_mem_util"))
	if !okU {
		response.Error(c, errcode.ParamInvalid, "gpu_mem_util 须为 0.80–0.95 之间的小数, 最多两位")
		return
	}
	res, err := h.svc.DeployPlans(c.Request.Context(), PlanRequest{
		BuyerID: c.GetInt64("user_id"), OrderNo: c.Param("id"), ModelID: modelID, GPUMemUtilPct: pct,
	})
	if err != nil {
		writeErr(c, err, "计算部署方案失败")
		return
	}
	response.Success(c, res)
}
