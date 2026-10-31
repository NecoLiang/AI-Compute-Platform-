package fleet

import (
	"errors"
	"log/slog"
	"strconv"

	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/response"

	"github.com/gin-gonic/gin"
)

// Handler 托管节点 API(docs/api/managed-rental-api.md)。仅在 fleet.enabled=true 时注册。
type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// RegisterSupplierRoutes 供给方节点接入与运维(RBAC supplier 组)。
func (h *Handler) RegisterSupplierRoutes(r *gin.RouterGroup) {
	r.GET("/supplier/nodes/:id/onboarding", h.Onboarding)
	r.PUT("/supplier/nodes/:id/ssh", h.UpdateSSH)
	r.POST("/supplier/nodes/:id/verify", h.Verify)
	r.POST("/supplier/nodes/:id/host-key/confirm", h.ConfirmHostKey)
	r.GET("/supplier/nodes/:id/probe-reports", h.ProbeReports)
	r.GET("/supplier/nodes/:id/slots", h.Slots)
	r.PUT("/supplier/nodes/:id/slots/:gpu_index", h.SetSlot)
	r.POST("/supplier/nodes/:id/drain", h.Drain)
	r.POST("/supplier/nodes/:id/undrain", h.Undrain)
	r.POST("/supplier/nodes/:id/reimage-confirm", h.ReimageConfirm)
}

// RegisterAdminRoutes 运营节点管理(RBAC operator/admin 组)。
func (h *Handler) RegisterAdminRoutes(r *gin.RouterGroup) {
	r.POST("/admin/nodes/:id/suspend", h.Suspend)
	r.POST("/admin/nodes/:id/resume", h.Resume)
	r.GET("/admin/node-tasks", h.Tasks)
	r.POST("/admin/node-tasks/:id/retry", h.RetryTask)
	r.GET("/admin/leases", h.AdminLeases)
	r.GET("/admin/access-sessions", h.AdminSessions)
	r.GET("/admin/lease-outages", h.AdminOutages)
	r.GET("/admin/supplier-credits", h.AdminCredits)
	r.GET("/admin/supplier-credits/:id", h.AdminCredit)
	r.PUT("/admin/supplier-credits/:id/override", h.AdminOverrideCredit)
}

// fail 业务错误原样返回; 未分类错误记日志并返回 50000(不向前端暴露 SQL 细节)。
func fail(c *gin.Context, err error) {
	var ce *errcode.CodedError
	if errors.As(err, &ce) {
		response.Error(c, ce.Code, ce.Msg)
		return
	}
	slog.Error("fleet 接口内部错误", "path", c.FullPath(), "request_id", c.GetString("request_id"), "error", err)
	response.Error(c, errcode.InternalError, "服务内部错误, 请稍后重试")
}

func pathID(c *gin.Context, name string) (int64, bool) {
	id, err := strconv.ParseInt(c.Param(name), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, errcode.ParamInvalid, name+" 非法")
		return 0, false
	}
	return id, true
}

func (h *Handler) Onboarding(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	o, err := h.svc.Onboarding(c.GetInt64("user_id"), id)
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, o)
}

func (h *Handler) UpdateSSH(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req struct {
		Host              string `json:"host" binding:"required"`
		Port              int    `json:"port" binding:"required"`
		User              string `json:"user" binding:"required"`
		BootstrapPassword string `json:"bootstrap_password"`
		// 推荐填写: 机器上 ssh-keygen -lf 得到的指纹, 首次连接即校验(不一致不发送密码)。
		ExpectedHostFingerprint string `json:"expected_host_fingerprint"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "host / port / user 必填")
		return
	}
	res, err := h.svc.UpdateSSH(c.Request.Context(), c.GetInt64("user_id"), id,
		SSHInput{Host: req.Host, Port: req.Port, User: req.User, BootstrapPassword: req.BootstrapPassword,
			ExpectedHostFingerprint: req.ExpectedHostFingerprint}, c.ClientIP())
	req.BootstrapPassword = ""
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, res)
}

func (h *Handler) Verify(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	taskID, err := h.svc.Verify(c.GetInt64("user_id"), id)
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, gin.H{"task_id": taskID})
}

func (h *Handler) ConfirmHostKey(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req struct {
		Fingerprint string `json:"fingerprint" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "fingerprint 必填")
		return
	}
	taskID, err := h.svc.ConfirmHostKey(c.GetInt64("user_id"), id, req.Fingerprint, c.ClientIP())
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, gin.H{"confirmed": true, "task_id": taskID})
}

func (h *Handler) ProbeReports(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	limit, _ := strconv.Atoi(c.DefaultQuery("limit", "10"))
	list, err := h.svc.ProbeReports(c.GetInt64("user_id"), id, limit)
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, list)
}

func (h *Handler) Slots(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	list, err := h.svc.Slots(c.GetInt64("user_id"), id)
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, list)
}

func (h *Handler) SetSlot(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	idx, err := strconv.Atoi(c.Param("gpu_index"))
	if err != nil || idx < 0 {
		response.Error(c, errcode.ParamInvalid, "gpu_index 非法")
		return
	}
	var req struct {
		State string `json:"state" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "state 必填")
		return
	}
	if err := h.svc.SetSlotState(c.GetInt64("user_id"), id, idx, req.State, c.ClientIP()); err != nil {
		fail(c, err)
		return
	}
	response.Success(c, gin.H{"gpu_index": idx, "state": req.State})
}

func (h *Handler) Drain(c *gin.Context)   { h.setSchedulable(c, false) }
func (h *Handler) Undrain(c *gin.Context) { h.setSchedulable(c, true) }

func (h *Handler) setSchedulable(c *gin.Context, v bool) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	if err := h.svc.SetSchedulable(c.GetInt64("user_id"), id, v, c.ClientIP()); err != nil {
		fail(c, err)
		return
	}
	response.Success(c, gin.H{"schedulable": v})
}

func (h *Handler) ReimageConfirm(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	taskID, err := h.svc.ReimageConfirm(c.GetInt64("user_id"), id, c.ClientIP())
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, gin.H{"task_id": taskID})
}

func (h *Handler) Suspend(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req struct {
		Reason string `json:"reason"`
	}
	_ = c.ShouldBindJSON(&req)
	if len([]rune(req.Reason)) > 200 {
		response.Error(c, errcode.ParamInvalid, "reason 不超过 200 字")
		return
	}
	if err := h.svc.Suspend(c.GetInt64("user_id"), id, req.Reason, c.ClientIP()); err != nil {
		fail(c, err)
		return
	}
	response.Success(c, gin.H{"verify_status": VerifySuspended})
}

func (h *Handler) Resume(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	taskID, err := h.svc.Resume(c.GetInt64("user_id"), id, c.ClientIP())
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, gin.H{"verify_status": VerifyUnverified, "task_id": taskID})
}

func (h *Handler) Tasks(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	nodeID, _ := strconv.ParseInt(c.Query("node_id"), 10, 64)
	list, total, err := h.svc.Tasks(c.Query("state"), nodeID, page, pageSize)
	if err != nil {
		fail(c, err)
		return
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	response.SuccessPage(c, list, total, page, pageSize)
}

func (h *Handler) RetryTask(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	if err := h.svc.RetryTask(c.GetInt64("user_id"), id, c.ClientIP()); err != nil {
		fail(c, err)
		return
	}
	response.Success(c, gin.H{"task_id": id, "state": TaskPending})
}

// RegisterBuyerRoutes 买家公钥与托管订单连接信息(登录即可, 归属在服务层校验)。
func (h *Handler) RegisterBuyerRoutes(r *gin.RouterGroup) {
	r.GET("/user/ssh-keys", h.ListKeys)
	r.POST("/user/ssh-keys", h.AddKey)
	r.DELETE("/user/ssh-keys/:id", h.DeleteKey)
	r.GET("/orders/:id/access", h.OrderAccess)
	r.GET("/orders/:id/sessions", h.OrderSessions)
	r.GET("/orders/:id/outages", h.OrderOutages)
	r.PUT("/orders/:id/outage-remedy", h.SetOutageRemedy)
}

func pageQuery(c *gin.Context) (int, int) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	return normPage(page, size)
}

func (h *Handler) ListKeys(c *gin.Context) {
	list, err := h.svc.ListBuyerKeys(c.GetInt64("user_id"))
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, list)
}

func (h *Handler) AddKey(c *gin.Context) {
	var req struct {
		Name      string `json:"name"`
		PublicKey string `json:"public_key" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "public_key 必填")
		return
	}
	k, err := h.svc.AddBuyerKey(c.GetInt64("user_id"), req.Name, req.PublicKey, c.ClientIP())
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, k)
}

func (h *Handler) DeleteKey(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	if err := h.svc.DeleteBuyerKey(c.GetInt64("user_id"), id, c.ClientIP()); err != nil {
		fail(c, err)
		return
	}
	response.Success(c, gin.H{"id": id, "deleted": true})
}

func (h *Handler) OrderAccess(c *gin.Context) {
	a, err := h.svc.OrderAccess(c.GetInt64("user_id"), c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, a)
}

func (h *Handler) OrderSessions(c *gin.Context) {
	page, size := pageQuery(c)
	list, total, page, size, err := h.svc.OrderSessions(c.GetInt64("user_id"), c.Param("id"), page, size)
	if err != nil {
		fail(c, err)
		return
	}
	response.SuccessPage(c, list, total, page, size)
}

// queryID 可选的正整数查询参数; 缺省为 0, 非法 40001。
func queryID(c *gin.Context, name string) (int64, bool) {
	v := c.Query(name)
	if v == "" {
		return 0, true
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, errcode.ParamInvalid, name+" 非法")
		return 0, false
	}
	return id, true
}

func (h *Handler) AdminLeases(c *gin.Context) {
	nodeID, ok := queryID(c, "node_id")
	if !ok {
		return
	}
	buyerID, ok := queryID(c, "buyer_id")
	if !ok {
		return
	}
	page, size := pageQuery(c)
	list, total, page, size, err := h.svc.AdminLeases(c.Query("state"), nodeID, buyerID, page, size)
	if err != nil {
		fail(c, err)
		return
	}
	response.SuccessPage(c, list, total, page, size)
}

func (h *Handler) AdminSessions(c *gin.Context) {
	page, size := pageQuery(c)
	list, total, page, size, err := h.svc.AdminSessions(c.Query("lease_no"), c.Query("ip"), page, size)
	if err != nil {
		fail(c, err)
		return
	}
	response.SuccessPage(c, list, total, page, size)
}
