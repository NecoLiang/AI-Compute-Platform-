package fleet

import (
	"github.com/gin-gonic/gin"

	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/response"
)

func (h *Handler) OrderOutages(c *gin.Context) {
	r, err := h.svc.OrderOutages(c.GetInt64("user_id"), c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, r)
}

func (h *Handler) OrderNights(c *gin.Context) {
	r, err := h.svc.OrderNights(c.GetInt64("user_id"), c.Param("id"))
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, r)
}

func (h *Handler) SetOutageRemedy(c *gin.Context) {
	var req struct {
		Remedy string `json:"remedy" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "remedy 必填(extend 或 refund)")
		return
	}
	r, err := h.svc.SetOutageRemedy(c.GetInt64("user_id"), c.Param("id"), req.Remedy)
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, r)
}

func (h *Handler) AdminOutages(c *gin.Context) {
	supplierID, ok := queryID(c, "supplier_id")
	if !ok {
		return
	}
	page, size := pageQuery(c)
	list, total, page, size, err := h.svc.AdminOutages(OutageFilter{
		LeaseNo: c.Query("lease_no"), OrderNo: c.Query("order_no"), Cause: c.Query("cause"), SupplierID: supplierID,
		Open: c.Query("open") == "1", RefundPending: c.Query("refund_pending") == "1",
	}, page, size)
	if err != nil {
		fail(c, err)
		return
	}
	response.SuccessPage(c, list, total, page, size)
}

func (h *Handler) AdminCredits(c *gin.Context) {
	page, size := pageQuery(c)
	list, total, err := h.svc.AdminSupplierCredits(c.Query("stopped") == "1", page, size)
	if err != nil {
		fail(c, err)
		return
	}
	response.SuccessPage(c, list, int64(total), page, size)
}

func (h *Handler) AdminCredit(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	d, err := h.svc.AdminSupplierCredit(id)
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, d)
}

func (h *Handler) AdminOverrideCredit(c *gin.Context) {
	id, ok := pathID(c, "id")
	if !ok {
		return
	}
	var req struct {
		Mode   string `json:"mode" binding:"required"`
		Reason string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "mode 必填(auto / allow / stop)")
		return
	}
	d, err := h.svc.AdminOverrideCredit(c.GetInt64("user_id"), id, req.Mode, req.Reason, c.ClientIP())
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, d)
}

// AdminSupplierBreachRefund 运营发起供给方违约未使用部分退款(T-106; 部分退款能力未确认时 50000)。
func (h *Handler) AdminSupplierBreachRefund(c *gin.Context) {
	var req struct {
		OrderNo string `json:"order_no" binding:"required"`
		Reason  string `json:"reason"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "order_no 必填")
		return
	}
	d, err := h.svc.AdminSupplierBreachRefund(c.Request.Context(), c.GetInt64("user_id"), req.OrderNo, req.Reason, c.ClientIP())
	if err != nil {
		fail(c, err)
		return
	}
	response.Success(c, d)
}
