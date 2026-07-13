package user

import (
	"errors"
	"tokenfactory/pkg/errcode"
)

var (
	ErrAlreadySubmitted = errors.New("认证已提交，请等待审核")
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
	Name        string `json:"enterprise_name" binding:"required"`
	USCC        string `json:"uscc" binding:"required"`
	LicenseURL  string `json:"license_url" binding:"required"`
	LegalPerson string `json:"legal_person" binding:"required"`
}

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
	existing, _ := s.repo.GetPersonalKYC(userID)
	if existing != nil && existing.Status == "pending" {
		return ErrAlreadySubmitted
	}
	return s.repo.CreatePersonalKYC(userID, req.RealName, req.IDCard)
}

func (s *Service) SubmitEnterprise(userID int64, req EnterpriseReq) error {
	existing, _ := s.repo.GetEnterprise(userID)
	if existing != nil && existing.Status == "pending" {
		return ErrAlreadySubmitted
	}
	return s.repo.CreateEnterprise(userID, req.Name, req.USCC, req.LicenseURL, req.LegalPerson)
}

func (s *Service) GetKYCStatus(userID int64) (*KYCStatus, error) {
	status := &KYCStatus{}
	personal, err := s.repo.GetPersonalKYC(userID)
	if err == nil {
		status.Personal = &KYCItem{Status: personal.Status, RealName: personal.RealName}
	} else {
		status.Personal = &KYCItem{Status: "none"}
	}
	enterprise, err := s.repo.GetEnterprise(userID)
	if err == nil {
		status.Enterprise = &KYCItem{Status: enterprise.Status, Name: enterprise.Name}
	} else {
		status.Enterprise = &KYCItem{Status: "none"}
	}
	return status, nil
}

func (s *Service) ApplyRole(userID int64, role string) error {
	validRoles := map[string]bool{"supplier": true, "vendor": true, "funder": true}
	if !validRoles[role] {
		return errors.New("无效的角色类型")
	}
	return s.repo.AddRole(userID, role)
}

func (s *Service) GetRoles(userID int64) ([]string, error) {
	return s.repo.GetRoles(userID)
}

func ErrToCode(err error) int {
	switch {
	case errors.Is(err, ErrAlreadySubmitted):
		return errcode.Conflict
	default:
		return errcode.InternalError
	}
}
