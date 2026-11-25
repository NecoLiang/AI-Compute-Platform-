package payment

import (
	"errors"
	"io"
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
	r.POST("/payment/supplier/onboard", h.ApplyOnboard)
	r.POST("/payment/supplier/onboard/upload", h.UploadOnboardFile)
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
	r.GET("/admin/payment/partial-refunds", h.ListPartialRefunds)
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
		code := paymentErrorCode(err)
		if code == errcode.InternalError {
			slog.Error("发起支付失败", "order_no", req.OrderNo, "channel", req.Channel, "error", err)
		} else if bizCode(err) != "" {
			// 易宝受理后业务拒绝(如 00012 产品未开通): 留下原始返回码与文案, 便于和易宝核对。
			slog.Warn("易宝拒绝下单", "order_no", req.OrderNo, "channel", req.Channel, "error", err)
		}
		response.Error(c, code, err.Error())
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

// ApplyOnboard 供给方提交企业资料, 平台代向易宝进件为分账接收方。
func (h *Handler) ApplyOnboard(c *gin.Context) {
	var req OnboardApplication
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "请完整填写开通资料")
		return
	}
	userID := c.GetInt64("user_id")
	status, err := h.svc.ApplyOnboard(userID, req)
	if err != nil {
		code := errcode.ParamInvalid
		switch {
		case errors.Is(err, ErrOnboardInProgress):
			code = errcode.Conflict
		case errors.Is(err, ErrYeepayNotConfigured):
			code = errcode.InternalError
		}
		// 易宝拒绝或网络错误必须留痕(不记录申请资料本身): 否则无从与易宝核对。
		if bizCode(err) != "" || code == errcode.InternalError {
			slog.Warn("易宝进件失败", "user_id", userID, "error", err)
		}
		response.Error(c, code, err.Error())
		return
	}
	response.Success(c, status)
}

// UploadOnboardFile 进件资质文件上传(multipart 字段 file), 转存易宝后返回其存储地址。
func (h *Handler) UploadOnboardFile(c *gin.Context) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, onboardMaxFileBytes+(1<<20))
	file, header, err := c.Request.FormFile("file")
	if err != nil {
		response.Error(c, errcode.ParamInvalid, "请选择 6MB 以内的文件")
		return
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, onboardMaxFileBytes+1))
	if err != nil {
		response.Error(c, errcode.ParamInvalid, "文件读取失败")
		return
	}
	url, err := h.svc.UploadOnboardFile(header.Filename, data)
	if err != nil {
		code := errcode.ParamInvalid
		if bizCode(err) != "" || errors.Is(err, ErrYeepayNotConfigured) {
			code = errcode.InternalError
			slog.Warn("易宝资质文件上传失败", "user_id", c.GetInt64("user_id"), "error", err)
		}
		response.Error(c, code, err.Error())
		return
	}
	response.Success(c, gin.H{"url": url})
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

// ListPartialRefunds 托管订单部分退款(停机补偿 / 供给方违约, T-106)。
func (h *Handler) ListPartialRefunds(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	size, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	if page < 1 {
		page = 1
	}
	if size < 1 || size > 100 {
		size = 20
	}
	list, total, err := h.svc.ListPartialRefunds(c.Query("status"), c.Query("order_no"), page, size)
	if err != nil {
		var ce *errcode.CodedError
		if errors.As(err, &ce) {
			response.Error(c, ce.Code, ce.Msg)
			return
		}
		response.Error(c, errcode.InternalError, "部分退款列表读取失败")
		return
	}
	response.SuccessPage(c, list, total, page, size)
}
