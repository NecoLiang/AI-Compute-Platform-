package user

import (
	"database/sql"
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
	existing, err := s.repo.GetEnterprise(userID)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if existing != nil && (existing.Status == "pending" || existing.Status == "verified") {
		return ErrAlreadySubmitted
	}
	return s.repo.CreateEnterprise(userID, req.Name, req.USCC, req.LicenseURL, req.LegalPerson)
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
