package compute

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"tokenfactory/internal/intermediary"
	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/response"

	"github.com/gin-gonic/gin"
)

type ProductInquiryReq struct {
	ContactName  string `json:"contact_name"`
	ContactPhone string `json:"contact_phone"`
	Message      string `json:"message"`
}

var inquiryPhone = regexp.MustCompile(`^\+?[0-9 -]{6,20}$`)

func (s *Service) CreateProductInquiry(buyerID, productID int64, req ProductInquiryReq) (int64, error) {
	if err := s.repo.RequireTradingAccess(buyerID, "buyer"); err != nil {
		return 0, err
	}
	req.ContactName, req.ContactPhone, req.Message = strings.TrimSpace(req.ContactName), strings.TrimSpace(req.ContactPhone), strings.TrimSpace(req.Message)
	if req.ContactName == "" || len([]rune(req.ContactName)) > 64 {
		return 0, fmt.Errorf("请填写 1–64 字的联系人姓名")
	}
	if !inquiryPhone.MatchString(req.ContactPhone) || len(req.ContactPhone) > 20 {
		return 0, fmt.Errorf("请填写有效的联系电话")
	}
	if len([]rune(req.Message)) < 5 || len([]rune(req.Message)) > 2000 {
		return 0, fmt.Errorf("请填写 5–2000 字的采购需求")
	}
	p, err := s.repo.GetProductByID(productID)
	if err != nil {
		return 0, err
	}
	if p.Status != "active" {
		return 0, fmt.Errorf("product not available")
	}
	if !p.PriceNegotiable {
		return 0, fmt.Errorf("该商品支持在线下单，请前往确认订单")
	}
	return intermediary.NewRepository(s.db).CreateLead(&intermediary.Lead{
		Type: "compute", ContactName: req.ContactName, ContactPhone: req.ContactPhone,
		Description: fmt.Sprintf("商品 #%d · %s · %s · 供给方 #%d · 买家 #%d\n%s", p.ID, p.ProductType, p.Region, p.SupplierID, buyerID, req.Message),
	})
}

func (h *Handler) CreateProductInquiry(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 {
		response.Error(c, errcode.ParamInvalid, "商品编号无效")
		return
	}
	var req ProductInquiryReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, "询价资料格式错误")
		return
	}
	leadID, err := h.svc.CreateProductInquiry(c.GetInt64("user_id"), id, req)
	if err != nil {
		response.Error(c, ErrToCode(err), err.Error())
		return
	}
	response.Success(c, gin.H{"id": leadID})
}
