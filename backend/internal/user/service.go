package user

import (
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"tokenfactory/pkg/errcode"
)

var (
	ErrAlreadySubmitted = errors.New("认证已提交，请等待审核")
	ErrInvalidKYC       = errors.New("认证资料格式不正确")
)

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
}

type PersonalKYCReq struct {
	RealName string `json:"real_name" binding:"required"`
	IDCard   string `json:"id_card" binding:"required"`
}

type EnterpriseReq struct {
	Name               string `json:"enterprise_name"`
	USCC               string `json:"uscc"`
	LicenseURL         string `json:"license_url"`
	LegalPerson        string `json:"legal_person"`
	LegalPersonIDCard  string `json:"legal_person_id_card"`
	BankName           string `json:"bank_name"`
	BankAccountName    string `json:"bank_account_name"`
	BankAccountNumber  string `json:"bank_account_number"`
	LicenseFileName    string `json:"license_file_name"`
	LicenseContentType string `json:"license_content_type"`
	LicenseData        []byte `json:"-"`
}

var identityNumberPattern = regexp.MustCompile(`^(?:\d{15}|\d{17}[\dXx])$`)
var bankAccountNumberPattern = regexp.MustCompile(`^\d{8,32}$`)

type KYCStatus struct {
	Personal   *KYCItem `json:"personal"`
	Enterprise *KYCItem `json:"enterprise"`
}

type KYCItem struct {
	Status   string `json:"status"` // none/pending/verified/rejected
	RealName string `json:"real_name,omitempty"`
	Name     string `json:"name,omitempty"`
}

func (s *Service) SubmitPersonalKYC(userID int64, req PersonalKYCReq) error {
	existing, err := s.repo.GetPersonalKYC(userID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if existing != nil && (existing.Status == "pending" || existing.Status == "verified") {
		return ErrAlreadySubmitted
	}
	// ponytail: pilot submissions auto-verify; replace this write with a provider verdict when real KYC is required.
	return s.repo.CreatePersonalKYC(userID, req.RealName, req.IDCard)
}

func (s *Service) SubmitEnterprise(userID int64, req EnterpriseReq) error {
	if err := validateEnterpriseReq(req); err != nil {
		return err
	}
	existing, err := s.repo.GetEnterprise(userID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if existing != nil && (existing.Status == "pending" || existing.Status == "verified") {
		return ErrAlreadySubmitted
	}
	return s.repo.CreateEnterprise(userID, req)
}

func validateEnterpriseReq(req EnterpriseReq) error {
	required := []struct{ name, value string }{
		{"企业名称", req.Name}, {"统一社会信用代码", req.USCC},
		{"法定代表人", req.LegalPerson}, {"法定代表人证件号", req.LegalPersonIDCard},
		{"开户行", req.BankName}, {"账户名称", req.BankAccountName},
		{"银行账号", req.BankAccountNumber}, {"营业执照", req.LicenseFileName},
	}
	for _, field := range required {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%w: 请填写%s", ErrInvalidKYC, field.name)
		}
	}
	if len(strings.TrimSpace(req.USCC)) != 18 {
		return fmt.Errorf("%w: 统一社会信用代码需为 18 位", ErrInvalidKYC)
	}
	if !identityNumberPattern.MatchString(strings.TrimSpace(req.LegalPersonIDCard)) {
		return fmt.Errorf("%w: 法定代表人证件号格式不正确", ErrInvalidKYC)
	}
	if !bankAccountNumberPattern.MatchString(strings.TrimSpace(req.BankAccountNumber)) {
		return fmt.Errorf("%w: 银行账号需为 8–32 位数字", ErrInvalidKYC)
	}
	if len(req.LicenseData) == 0 {
		return fmt.Errorf("%w: 营业执照文件为空", ErrInvalidKYC)
	}
	return nil
}

func (s *Service) GetKYCStatus(userID int64) (*KYCStatus, error) {
	status := &KYCStatus{}
	personal, err := s.repo.GetPersonalKYC(userID)
	switch {
	case err == nil:
		status.Personal = &KYCItem{Status: personal.Status, RealName: personal.RealName}
	case errors.Is(err, sql.ErrNoRows):
		status.Personal = &KYCItem{Status: "none"}
	default:
		return nil, err
	}
	enterprise, err := s.repo.GetEnterprise(userID)
	switch {
	case err == nil:
		status.Enterprise = &KYCItem{Status: enterprise.Status, Name: enterprise.Name}
	case errors.Is(err, sql.ErrNoRows):
		status.Enterprise = &KYCItem{Status: "none"}
	default:
		return nil, err
	}
	return status, nil
}

func (s *Service) GetRoles(userID int64) ([]string, error) {
	return s.repo.GetRoles(userID)
}

func ErrToCode(err error) int {
	switch {
	case errors.Is(err, ErrAlreadySubmitted):
		return errcode.Conflict
	case errors.Is(err, ErrInvalidKYC):
		return errcode.ParamInvalid
	default:
		return errcode.InternalError
	}
}
