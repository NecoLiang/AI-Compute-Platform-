package payment

import (
	"errors"
	"log/slog"
	"net/http"
	"strconv"

	"github.com/gin-gonic/gin"
	"tokenfactory/internal/compute"
	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/response"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) RegisterBuyerRoutes(r *gin.RouterGroup) {
	r.GET("/payment/channels", h.PayChannels)
	r.POST("/payment/pay", h.Pay)
	r.GET("/payment/status/:order_no", h.PaymentStatus)
}

func (h *Handler) RegisterSupplierRoutes(r *gin.RouterGroup) {
	r.POST("/payment/supplier/onboard", h.StartOnboard)
	r.GET("/payment/supplier/onboard/status", h.OnboardStatus)
	r.GET("/payment/settlements", h.Settlements)
	r.GET("/supplier/settlements", h.SupplierSettlements)
	r.GET("/supplier/settlements/summary", h.SupplierSettlementSummary)
}

func (h *Handler) RegisterAdminRoutes(r *gin.RouterGroup) {
	r.GET("/admin/payment/reconcile", h.Reconcile)
	r.GET("/admin/payment/list", h.ListPayments)
	r.GET("/admin/payment/refunds", h.ListRefunds)
	r.POST("/admin/payment/refunds/:order_no", h.ExecuteRefund)
	r.GET("/admin/payment/merchants", h.ListMerchants)
	r.PUT("/admin/payment/merchants/:user_id", h.BindMerchant)
}

// RegisterCallbackRoutes 易宝异步通知(公开路由, 信任来源是报文解密验签)。
func (h *Handler) RegisterCallbackRoutes(r *gin.RouterGroup) {
	r.POST("/payment/yeepay/notify/:kind", h.Notify)
}

func (h *Handler) PayChannels(c *gin.Context) {
	response.Success(c, gin.H{"channels": h.svc.PayChannels()})
}

func (h *Handler) Pay(c *gin.Context) {
	var req PayReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	resp, err := h.svc.Pay(c.GetInt64("user_id"), req)
	if err != nil {
		response.Error(c, paymentErrorCode(err), err.Error())
		return
	}
	response.Success(c, resp)
}

func (h *Handler) PaymentStatus(c *gin.Context) {
	payments, err := h.svc.GetOrderPayments(c.GetInt64("user_id"), c.Param("order_no"))
	if err != nil {
		response.Error(c, paymentErrorCode(err), err.Error())
		return
	}
	response.Success(c, payments)
}

// Notify 易宝通知以表单字段 response 携带密文; 必须回纯文本 SUCCESS, 否则易宝持续重试。
func (h *Handler) Notify(c *gin.Context) {
	raw := c.PostForm("response")
	if raw == "" {
		raw = c.Query("response")
	}
	kind := c.Param("kind")
	if err := h.svc.HandleNotify(kind, raw); err != nil {
		slog.Warn("易宝通知处理失败", "kind", kind, "error", err)
		c.String(http.StatusOK, "FAIL")
		return
	}
	c.String(http.StatusOK, "SUCCESS")
}

func (h *Handler) StartOnboard(c *gin.Context) {
	status, err := h.svc.StartOnboard(c.GetInt64("user_id"))
	if err != nil {
		response.Error(c, paymentErrorCode(err), err.Error())
		return
	}
	response.Success(c, status)
}

func (h *Handler) OnboardStatus(c *gin.Context) {
	status, err := h.svc.GetOnboardStatus(c.GetInt64("user_id"))
	if err != nil {
		response.Error(c, errcode.InternalError, "收款账户开通状态读取失败")
		return
	}
	response.Success(c, status)
}

func (h *Handler) Settlements(c *gin.Context) {
	orderNo := c.Query("order_no")
	settlements, err := h.svc.GetOrderSettlements(c.GetInt64("user_id"), orderNo)
	if err != nil {
		response.Error(c, errcode.InternalError, "结算流水读取失败")
		return
	}
	response.Success(c, settlements)
}

// SupplierSettlements 供给方结算流水(按商品归属过滤, payee_id 不可靠)。
func (h *Handler) SupplierSettlements(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, err := h.svc.ListSupplierSettlements(c.GetInt64("user_id"), c.Query("status"), page, pageSize)
	if err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	response.SuccessPage(c, list, total, page, pageSize)
}

// SupplierSettlementSummary 应结/已分账/待结合计。
func (h *Handler) SupplierSettlementSummary(c *gin.Context) {
	total, succeeded, pending, err := h.svc.SumSupplierSettlements(c.GetInt64("user_id"))
	if err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.Success(c, gin.H{
		"total_fen": total, "succeeded_fen": succeeded, "pending_fen": pending,
	})
}

func (h *Handler) Reconcile(c *gin.Context) {
	date := c.DefaultQuery("date", "")
	result, err := h.svc.Reconcile(date)
	if err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.Success(c, result)
}

func (h *Handler) ListPayments(c *gin.Context) {
	date := c.DefaultQuery("date", "")
	list, err := h.svc.ListPaymentsByDate(date)
	if err != nil {
		response.Error(c, errcode.InternalError, "支付流水读取失败")
		return
	}
	response.Success(c, list)
}

func (h *Handler) ListRefunds(c *gin.Context) {
	list, err := h.svc.ListRefunds()
	if err != nil {
		response.Error(c, errcode.InternalError, "退款列表读取失败")
		return
	}
	response.Success(c, list)
}

func (h *Handler) ExecuteRefund(c *gin.Context) {
	var req struct {
		Reason string `json:"reason"`
	}
	_ = c.ShouldBindJSON(&req)
	status, err := h.svc.ExecuteRefund(c.GetInt64("user_id"), c.Param("order_no"), req.Reason)
	if err != nil {
		response.Error(c, paymentErrorCode(err), err.Error())
		return
	}
	response.Success(c, gin.H{"refund_status": status})
}

func (h *Handler) ListMerchants(c *gin.Context) {
	list, err := h.svc.ListMerchants()
	if err != nil {
		response.Error(c, errcode.InternalError, "收款商户列表读取失败")
		return
	}
	response.Success(c, list)
}

func (h *Handler) BindMerchant(c *gin.Context) {
	userID, err := strconv.ParseInt(c.Param("user_id"), 10, 64)
	var req struct {
		MerchantNo string `json:"merchant_no"`
	}
	if err != nil || c.ShouldBindJSON(&req) != nil {
		response.Error(c, errcode.ParamInvalid, "参数无效")
		return
	}
	if err := h.svc.BindMerchant(c.GetInt64("user_id"), userID, req.MerchantNo); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	response.Success(c, nil)
}

func paymentErrorCode(err error) int {
	if errors.Is(err, ErrPaymentConflict) || errors.Is(err, ErrRefundNotAllowed) {
		return errcode.Conflict
	}
	if errors.Is(err, ErrYeepayNotConfigured) || errors.Is(err, ErrYeepayCallbackUnverifiable) {
		return errcode.InternalError
	}
	return compute.ErrToCode(err)
}
