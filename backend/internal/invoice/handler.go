package invoice

import (
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/response"

	"github.com/gin-gonic/gin"
)

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) RegisterBuyerRoutes(r *gin.RouterGroup) {
	r.GET("/invoices/title", h.GetTitle)
	r.PUT("/invoices/title", h.SaveTitle)
	r.GET("/invoices/billable-orders", h.ListBillableOrders)
	r.POST("/invoices/apply", h.Apply)
	r.GET("/invoices", h.ListInvoices)
	r.GET("/invoices/:invoice_no/download", h.Download)
}

func (h *Handler) RegisterAdminRoutes(r *gin.RouterGroup) {
	r.GET("/admin/invoices", h.AdminListInvoices)
	r.POST("/admin/invoices/:id/issue", h.AdminIssue)
	r.POST("/admin/invoices/:id/reject", h.AdminReject)
}

// ---- Buyer ----

func (h *Handler) GetTitle(c *gin.Context) {
	title, err := h.svc.GetTitle(c.GetInt64("user_id"))
	if err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.Success(c, title)
}

func (h *Handler) SaveTitle(c *gin.Context) {
	var req SaveTitleReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	title, err := h.svc.SaveTitle(c.GetInt64("user_id"), req)
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, title)
}

func (h *Handler) ListBillableOrders(c *gin.Context) {
	list, err := h.svc.ListBillableOrders(c.GetInt64("user_id"))
	if err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.Success(c, gin.H{"list": list, "total": len(list)})
}

func (h *Handler) Apply(c *gin.Context) {
	var req struct {
		OrderNos []string `json:"order_nos" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	inv, err := h.svc.Apply(c.GetInt64("user_id"), req.OrderNos)
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, gin.H{
		"invoice_no": inv.InvoiceNo, "amount_fen": inv.AmountFen, "status": inv.Status,
	})
}

func (h *Handler) ListInvoices(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, err := h.svc.ListInvoices(c.GetInt64("user_id"), page, pageSize)
	if err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	response.SuccessPage(c, list, total, page, pageSize)
}

// Download 流式返回发票 PDF。归属与已开票校验在 service 层完成。
func (h *Handler) Download(c *gin.Context) {
	pdf, filename, err := h.svc.Download(c.GetInt64("user_id"), c.Param("invoice_no"))
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	c.Header("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	c.Data(http.StatusOK, "application/pdf", pdf)
}

// ---- Admin ----

func (h *Handler) AdminListInvoices(c *gin.Context) {
	page, _ := strconv.Atoi(c.DefaultQuery("page", "1"))
	pageSize, _ := strconv.Atoi(c.DefaultQuery("page_size", "20"))
	list, total, err := h.svc.ListAllInvoices(c.Query("status"), page, pageSize)
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.SuccessPage(c, list, total, page, pageSize)
}

// AdminIssue 完成开票: multipart 上传 PDF + 可选税务号码。
// 这是后端首个文件上传点, Gin 侧用 MaxMultipartMemory 控制内存占用。
func (h *Handler) AdminIssue(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, errcode.ParamInvalid, "invalid invoice id")
		return
	}
	file, err := c.FormFile("pdf")
	if err != nil {
		response.Error(c, errcode.ParamInvalid, "缺少 PDF 文件")
		return
	}
	if file.Size > MaxInvoicePDFBytes {
		response.Error(c, errcode.ParamInvalid, "PDF 大小必须在 5MB 以内")
		return
	}
	f, err := file.Open()
	if err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	defer f.Close()
	pdf, err := io.ReadAll(f)
	if err != nil {
		response.Error(c, errcode.InternalError, err.Error())
		return
	}
	if err := h.svc.Issue(id, c.PostForm("tax_invoice_no"), file.Filename, pdf); err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, nil)
}

func (h *Handler) AdminReject(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		response.Error(c, errcode.ParamInvalid, "invalid invoice id")
		return
	}
	var req struct {
		Reason string `json:"reason" binding:"required"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	if err := h.svc.Reject(id, req.Reason); err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, nil)
}

// ErrToCode 与 compute 包约定一致: 已知业务短语映射到稳定错误码,
// 中文提示视为可直接展示的业务校验错误, 其余归 50000。
func ErrToCode(err error) int {
	if err == nil {
		return errcode.Success
	}
	msg := err.Error()
	switch msg {
	case "invoice not found":
		return errcode.NotFound
	case "invalid status":
		return errcode.ParamInvalid
	}
	if strings.HasPrefix(msg, "无权") {
		return errcode.Forbidden
	}
	if containsCJK(msg) {
		return errcode.ParamInvalid
	}
	return errcode.InternalError
}

func containsCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}
