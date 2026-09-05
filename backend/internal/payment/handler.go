package payment

import (
	"errors"
	"github.com/gin-gonic/gin"
	"strconv"
	"tokenfactory/internal/compute"
	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/response"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler { return &Handler{svc: svc} }

func (h *Handler) RegisterBuyerRoutes(r *gin.RouterGroup) {
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
}

func (h *Handler) RegisterCallbackRoutes(r *gin.RouterGroup) {
	r.POST("/payment/callback", h.Callback)
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

func (h *Handler) Callback(c *gin.Context) {
	var req CallbackReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	if err := h.svc.HandleCallback(req); err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.Success(c, nil)
}

func (h *Handler) StartOnboard(c *gin.Context) {
	if err := h.svc.StartOnboard(c.GetInt64("user_id")); err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.Success(c, nil)
}

func (h *Handler) OnboardStatus(c *gin.Context) {
	status, err := h.svc.GetOnboardStatus(c.GetInt64("user_id"))
	if err != nil {
		response.Error(c, errcode.InternalError, "支付进件状态读取失败")
		return
	}
	response.Success(c, gin.H{"status": status})
}

func (h *Handler) Settlements(c *gin.Context) {
	orderNo := c.Query("order_no")
	settlements, err := h.svc.GetOrderSettlements(orderNo)
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

func paymentErrorCode(err error) int {
	if errors.Is(err, ErrPaymentConflict) {
		return errcode.Conflict
	}
	if errors.Is(err, ErrYeepayNotConfigured) || errors.Is(err, ErrYeepayCallbackUnverifiable) {
		return errcode.InternalError
	}
	return compute.ErrToCode(err)
}
