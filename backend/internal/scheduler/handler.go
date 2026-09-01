package scheduler

import (
	"strconv"

	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/response"

	"github.com/gin-gonic/gin"
)

type Handler struct{ svc *Service }

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

// RegisterSupplierRoutes 供应方节点管理(RBAC supplier 组)。
func (h *Handler) RegisterSupplierRoutes(r *gin.RouterGroup) {
	r.POST("/supplier/nodes", h.RegisterNode)
	r.GET("/supplier/nodes", h.ListMyNodes)
	r.DELETE("/supplier/nodes/:id", h.DeleteNode)
	r.GET("/supplier/schedule-advice", h.SupplierAdvice)
}

// RegisterNodeRoutes 节点心跳(无用户 JWT, 凭 node_key 鉴权)。
func (h *Handler) RegisterNodeRoutes(r *gin.RouterGroup) {
	r.POST("/node/heartbeat", h.Heartbeat)
}

// RegisterAdminRoutes 运营视角(RBAC operator/admin 组)。
func (h *Handler) RegisterAdminRoutes(r *gin.RouterGroup) {
	r.GET("/admin/nodes", h.AdminListNodes)
	r.GET("/admin/schedule-advice", h.AdminAdvice)
}

func (h *Handler) RegisterNode(c *gin.Context) {
	var req struct {
		ProductID  int64  `json:"product_id" binding:"required"`
		NodeName   string `json:"node_name" binding:"required"`
		TotalCards int    `json:"total_cards" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "product_id / node_name / total_cards 必填")
		return
	}
	node, key, err := h.svc.RegisterNode(c.GetInt64("user_id"), req.ProductID, req.NodeName, req.TotalCards)
	if err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	// node_key 明文只在此处返回一次, 供应方须妥善保存并配置到节点心跳脚本。
	response.Success(c, gin.H{"node": node, "node_key": key,
		"notice": "node_key 仅本次展示, 平台只存哈希, 丢失需删除节点后重新注册"})
}

func (h *Handler) ListMyNodes(c *gin.Context) {
	nodes, err := h.svc.ListSupplierNodes(c.GetInt64("user_id"))
	if err != nil {
		response.Error(c, errcode.InternalError, "查询节点失败")
		return
	}
	response.Success(c, nodes)
}

func (h *Handler) DeleteNode(c *gin.Context) {
	id, _ := strconv.ParseInt(c.Param("id"), 10, 64)
	if err := h.svc.DeleteNode(id, c.GetInt64("user_id")); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	response.Success(c, gin.H{"deleted": true})
}

// Heartbeat POST /node/heartbeat 节点侧 30s 一次。
func (h *Handler) Heartbeat(c *gin.Context) {
	var req struct {
		NodeID         int64 `json:"node_id" binding:"required"`
		AvailableCards *int  `json:"available_cards" binding:"required"`
		GPUUtilPct     *int  `json:"gpu_util_pct"`
		VRAMUtilPct    *int  `json:"vram_util_pct"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "node_id / available_cards 必填")
		return
	}
	key := c.GetHeader("X-Node-Key")
	if key == "" {
		response.Error(c, errcode.Unauthorized, "缺少 X-Node-Key")
		return
	}
	if err := h.svc.Heartbeat(req.NodeID, key, *req.AvailableCards, req.GPUUtilPct, req.VRAMUtilPct); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	response.Success(c, gin.H{"ok": true})
}

func (h *Handler) SupplierAdvice(c *gin.Context) {
	advice, err := h.svc.Advise(c.Query("order_no"), c.GetInt64("user_id"))
	if err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	response.Success(c, advice)
}

func (h *Handler) AdminAdvice(c *gin.Context) {
	advice, err := h.svc.Advise(c.Query("order_no"), 0)
	if err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	response.Success(c, advice)
}

func (h *Handler) AdminListNodes(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	nodes, total, err := h.svc.ListAllNodes(c.Query("status"), page, pageSize)
	if err != nil {
		response.Error(c, errcode.InternalError, "查询节点失败")
		return
	}
	response.SuccessPage(c, nodes, total, page, pageSize)
}
