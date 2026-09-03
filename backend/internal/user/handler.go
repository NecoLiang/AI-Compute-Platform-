package user

import (
	"fmt"
	"github.com/gin-gonic/gin"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/response"
)

const maxKYCDocumentBytes = 5 << 20

type Handler struct {
	svc *Service
}

func NewHandler(svc *Service) *Handler {
	return &Handler{svc: svc}
}

func (h *Handler) RegisterRoutes(r *gin.RouterGroup) {
	r.GET("/user/profile", h.GetProfile)
	r.PUT("/user/profile", h.UpdateProfile)
	r.POST("/user/kyc/personal", h.SubmitPersonalKYC)
	r.POST("/user/kyc/enterprise", h.SubmitEnterprise)
	r.GET("/user/kyc/status", h.GetKYCStatus)
}

func (h *Handler) GetProfile(c *gin.Context) {
	response.Success(c, gin.H{"user_id": c.GetInt64("user_id"), "phone": c.GetString("phone")})
}

func (h *Handler) UpdateProfile(c *gin.Context) {
	response.Success(c, gin.H{"message": "更新成功"})
}

func (h *Handler) SubmitPersonalKYC(c *gin.Context) {
	var req PersonalKYCReq
	if err := c.ShouldBindJSON(&req); err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	if err := h.svc.SubmitPersonalKYC(c.GetInt64("user_id"), req); err != nil {
		respondKYCError(c, err)
		return
	}
	response.Success(c, nil)
}

func (h *Handler) SubmitEnterprise(c *gin.Context) {
	req, err := readEnterpriseRequest(c)
	if err != nil {
		response.Error(c, errcode.ParamInvalid, err.Error())
		return
	}
	if err := h.svc.SubmitEnterprise(c.GetInt64("user_id"), req); err != nil {
		respondKYCError(c, err)
		return
	}
	response.Success(c, nil)
}

func readEnterpriseRequest(c *gin.Context) (EnterpriseReq, error) {
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxKYCDocumentBytes+(64<<10))
	fileHeader, err := c.FormFile("business_license")
	if err != nil {
		return EnterpriseReq{}, fmt.Errorf("请选择营业执照文件")
	}
	if fileHeader.Size <= 0 || fileHeader.Size > maxKYCDocumentBytes {
		return EnterpriseReq{}, fmt.Errorf("营业执照文件需小于 5MB")
	}
	file, err := fileHeader.Open()
	if err != nil {
		return EnterpriseReq{}, fmt.Errorf("营业执照文件读取失败")
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, maxKYCDocumentBytes+1))
	if err != nil || len(data) == 0 || len(data) > maxKYCDocumentBytes {
		return EnterpriseReq{}, fmt.Errorf("营业执照文件读取失败")
	}
	contentType := http.DetectContentType(data)
	if contentType != "application/pdf" && contentType != "image/jpeg" && contentType != "image/png" {
		return EnterpriseReq{}, fmt.Errorf("营业执照仅支持 PDF、JPG 或 PNG")
	}
	fileName := filepath.Base(strings.TrimSpace(fileHeader.Filename))
	if fileName == "." || len(fileName) > 255 {
		return EnterpriseReq{}, fmt.Errorf("营业执照文件名无效")
	}
	return EnterpriseReq{
		Name:               strings.TrimSpace(c.PostForm("enterprise_name")),
		USCC:               strings.ToUpper(strings.TrimSpace(c.PostForm("uscc"))),
		LicenseURL:         fileName,
		LegalPerson:        strings.TrimSpace(c.PostForm("legal_person")),
		LegalPersonIDCard:  strings.TrimSpace(c.PostForm("legal_person_id_card")),
		BankName:           strings.TrimSpace(c.PostForm("bank_name")),
		BankAccountName:    strings.TrimSpace(c.PostForm("bank_account_name")),
		BankAccountNumber:  strings.TrimSpace(c.PostForm("bank_account_number")),
		LicenseFileName:    fileName,
		LicenseContentType: contentType,
		LicenseData:        data,
	}, nil
}

func (h *Handler) GetKYCStatus(c *gin.Context) {
	status, err := h.svc.GetKYCStatus(c.GetInt64("user_id"))
	if err != nil {
		respondKYCError(c, err)
		return
	}
	response.Success(c, status)
}

func respondKYCError(c *gin.Context, err error) {
	code := ErrToCode(err)
	if code == errcode.InternalError {
		_ = c.Error(err)
		response.Error(c, code, "认证服务暂不可用")
		return
	}
	response.Error(c, code, err.Error())
}
