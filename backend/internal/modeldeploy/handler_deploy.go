package modeldeploy

import (
	"encoding/json"
	"strconv"
	"strings"

	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/response"

	"github.com/gin-gonic/gin"
)

// 一键部署接口(docs/27 §27.8)。仅 fleet.enabled=true 时由 main 注册; 运行期未接入时写操作返回 50000 并说明缺什么。

// RegisterDeployRoutes 买家对自己订单的部署操作。
func (h *Handler) RegisterDeployRoutes(r *gin.RouterGroup) {
	r.POST("/orders/:id/deployments", h.Submit)
	r.GET("/orders/:id/deployments", h.ListDeployments)
	r.GET("/orders/:id/deployments/:deploy_no", h.GetDeployment)
	r.GET("/orders/:id/deployments/:deploy_no/health", h.Health)
	r.GET("/orders/:id/deployments/:deploy_no/logs", h.Logs)
	r.POST("/orders/:id/deployments/:deploy_no/stop", h.Stop)
	r.POST("/orders/:id/deployments/:deploy_no/restart", h.Restart)
}

// RegisterAdminDeployRoutes 运营查看全部部署。
func (h *Handler) RegisterAdminDeployRoutes(r *gin.RouterGroup) {
	r.GET("/admin/deployments", h.AdminDeployments)
}

type submitReq struct {
	ModelID       int64           `json:"model_id"`
	TP            int             `json:"tp"`
	DP            int             `json:"dp"`
	MaxModelLen   int             `json:"max_model_len"`
	GPUMemUtil    json.RawMessage `json:"gpu_mem_util"`
	CanaryEnabled *bool           `json:"canary_enabled"`
	LicenseAck    bool            `json:"license_ack"`
	UntestedAck   bool            `json:"untested_ack"`
}

// gpuMemUtilArg gpu_mem_util 可传数字 0.85 或字符串 "0.85"; 未传/null 取默认。
func gpuMemUtilArg(raw json.RawMessage) (string, bool) {
	t := strings.TrimSpace(string(raw))
	if t == "" || t == "null" {
		return "", false
	}
	if uq, err := strconv.Unquote(t); err == nil {
		return uq, true
	}
	return t, true
}

// Submit POST /orders/:id/deployments
func (h *Handler) Submit(c *gin.Context) {
	var req submitReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "请求体须为 JSON")
		return
	}
	if req.ModelID <= 0 || req.TP <= 0 || req.DP <= 0 || req.MaxModelLen < 0 {
		response.Error(c, errcode.ParamInvalid, "model_id/tp/dp 必填且须为正整数, max_model_len 不能为负")
		return
	}
	pct, ok := parseGPUMemUtil(gpuMemUtilArg(req.GPUMemUtil))
	if !ok {
		response.Error(c, errcode.ParamInvalid, "gpu_mem_util 须为 0.80–0.95 之间的小数, 最多两位")
		return
	}
	canary := true
	if req.CanaryEnabled != nil {
		canary = *req.CanaryEnabled
	}
	v, err := h.svc.Submit(c.Request.Context(), SubmitRequest{
		BuyerID: c.GetInt64("user_id"), OrderNo: c.Param("id"), ModelID: req.ModelID, TP: req.TP, DP: req.DP,
		MaxModelLen: req.MaxModelLen, GPUMemUtilPct: pct, CanaryEnabled: canary,
		LicenseAck: req.LicenseAck, UntestedAck: req.UntestedAck,
	})
	if err != nil {
		writeErr(c, err, "提交部署失败")
		return
	}
	response.Success(c, v)
}

// ListDeployments GET /orders/:id/deployments
func (h *Handler) ListDeployments(c *gin.Context) {
	list, err := h.svc.ListDeployments(c.GetInt64("user_id"), c.Param("id"))
	if err != nil {
		writeErr(c, err, "查询部署失败")
		return
	}
	response.Success(c, gin.H{"list": list, "total": len(list)})
}

// GetDeployment GET /orders/:id/deployments/:deploy_no
func (h *Handler) GetDeployment(c *gin.Context) {
	v, err := h.svc.GetDeployment(c.GetInt64("user_id"), c.Param("id"), c.Param("deploy_no"))
	if err != nil {
		writeErr(c, err, "查询部署失败")
		return
	}
	response.Success(c, v)
}

// Health GET .../health?range=1h|24h|7d
func (h *Handler) Health(c *gin.Context) {
	v, err := h.svc.HealthSeries(c.GetInt64("user_id"), c.Param("id"), c.Param("deploy_no"), c.Query("range"))
	if err != nil {
		writeErr(c, err, "查询健康曲线失败")
		return
	}
	response.Success(c, v)
}

// Logs GET .../logs?tail=200
func (h *Handler) Logs(c *gin.Context) {
	tail, err := strconv.Atoi(c.DefaultQuery("tail", "200"))
	if err != nil {
		response.Error(c, errcode.ParamInvalid, "tail 须为整数")
		return
	}
	v, err := h.svc.Logs(c.Request.Context(), c.GetInt64("user_id"), c.Param("id"), c.Param("deploy_no"), tail, c.ClientIP())
	if err != nil {
		writeErr(c, err, "查询部署日志失败")
		return
	}
	response.Success(c, v)
}

// Stop POST .../stop
func (h *Handler) Stop(c *gin.Context) {
	v, err := h.svc.Stop(c.Request.Context(), c.GetInt64("user_id"), c.Param("id"), c.Param("deploy_no"))
	if err != nil {
		writeErr(c, err, "停止部署失败")
		return
	}
	response.Success(c, v)
}

// Restart POST .../restart
func (h *Handler) Restart(c *gin.Context) {
	v, err := h.svc.Restart(c.Request.Context(), c.GetInt64("user_id"), c.Param("id"), c.Param("deploy_no"))
	if err != nil {
		writeErr(c, err, "重启部署失败")
		return
	}
	response.Success(c, v)
}

// AdminDeployments GET /admin/deployments?state=&health=&page=&page_size=
func (h *Handler) AdminDeployments(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	list, total, err := h.svc.AdminDeployments(c.Query("state"), c.Query("health"), page, pageSize)
	if err != nil {
		writeErr(c, err, "查询部署失败")
		return
	}
	response.SuccessPage(c, list, int64(total), page, pageSize)
}
