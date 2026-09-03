package user

import (
	"database/sql"
	"time"

	"github.com/jmoiron/sqlx"
)

type PersonalKYC struct {
	ID        int64     `db:"id" json:"id"`
	UserID    int64     `db:"user_id" json:"user_id"`
	RealName  string    `db:"real_name" json:"real_name"`
	IDCard    string    `db:"id_card" json:"id_card"`
	Status    string    `db:"status" json:"status"` // pending/verified/rejected
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type Enterprise struct {
	ID                 int64     `db:"id" json:"id"`
	UserID             int64     `db:"user_id" json:"user_id"`
	Name               string    `db:"name" json:"name"`
	USCC               string    `db:"uscc" json:"uscc"`
	LicenseURL         string    `db:"license_url" json:"license_url"`
	LegalPerson        string    `db:"legal_person" json:"legal_person"`
	LegalPersonIDCard  string    `db:"legal_person_id_card" json:"-"`
	BankName           string    `db:"bank_name" json:"-"`
	BankAccountName    string    `db:"bank_account_name" json:"-"`
	BankAccountNumber  string    `db:"bank_account_number" json:"-"`
	LicenseFileName    string    `db:"license_file_name" json:"license_file_name"`
	LicenseContentType string    `db:"license_content_type" json:"-"`
	Status             string    `db:"status" json:"status"` // pending/verified/rejected
	CreatedAt          time.Time `db:"created_at" json:"created_at"`
}

type UserRole struct {
	ID     int64  `db:"id" json:"id"`
	UserID int64  `db:"user_id" json:"user_id"`
	Role   string `db:"role" json:"role"` // buyer/supplier/vendor/funder
}

type Repository struct {
	db *sqlx.DB
}

func NewRepository(db *sqlx.DB) *Repository {
	return &Repository{db: db}
}

// Personal KYC
func (r *Repository) CreatePersonalKYC(userID int64, realName, idCard string) error {
	_, err := r.db.Exec(
		"INSERT INTO user_kyc (user_id, real_name, id_card, status) VALUES (?, ?, ?, 'verified') ON DUPLICATE KEY UPDATE real_name=?, id_card=?, status='verified'",
		userID, realName, idCard, realName, idCard,
	)
	return err
}

func (r *Repository) GetPersonalKYC(userID int64) (*PersonalKYC, error) {
	var kyc PersonalKYC
	err := r.db.Get(&kyc, "SELECT id, user_id, real_name, id_card, status, created_at FROM user_kyc WHERE user_id = ?", userID)
	return &kyc, err
}

// Enterprise
func (r *Repository) CreateEnterprise(userID int64, req EnterpriseReq) error {
	_, err := r.db.Exec(
		`INSERT INTO enterprises (user_id, name, uscc, license_url, legal_person,
			legal_person_id_card, bank_name, bank_account_name, bank_account_number,
			license_file_name, license_content_type, license_blob, status)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,'verified')
		ON DUPLICATE KEY UPDATE name=VALUES(name), uscc=VALUES(uscc), license_url=VALUES(license_url),
			legal_person=VALUES(legal_person), legal_person_id_card=VALUES(legal_person_id_card),
			bank_name=VALUES(bank_name), bank_account_name=VALUES(bank_account_name),
			bank_account_number=VALUES(bank_account_number), license_file_name=VALUES(license_file_name),
			license_content_type=VALUES(license_content_type), license_blob=VALUES(license_blob), status='verified'`,
		userID, req.Name, req.USCC, req.LicenseURL, req.LegalPerson,
		req.LegalPersonIDCard, req.BankName, req.BankAccountName, req.BankAccountNumber,
		req.LicenseFileName, req.LicenseContentType, req.LicenseData,
	)
	return err
}

func (r *Repository) GetEnterprise(userID int64) (*Enterprise, error) {
	var ent Enterprise
	err := r.db.Get(&ent, `SELECT id, user_id, name, uscc, COALESCE(license_url,'') AS license_url,
		COALESCE(legal_person,'') AS legal_person, COALESCE(legal_person_id_card,'') AS legal_person_id_card,
		COALESCE(bank_name,'') AS bank_name, COALESCE(bank_account_name,'') AS bank_account_name,
		COALESCE(bank_account_number,'') AS bank_account_number,
		COALESCE(license_file_name,'') AS license_file_name,
		COALESCE(license_content_type,'') AS license_content_type, status, created_at
		FROM enterprises WHERE user_id = ?`, userID)
	return &ent, err
}

// Roles
func (r *Repository) GetRoles(userID int64) ([]string, error) {
	var roles []string
	err := r.db.Select(&roles, "SELECT role FROM user_roles WHERE user_id = ?", userID)
	if err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	if len(roles) == 0 {
		return []string{"buyer"}, nil
	}
	return roles, nil
}

func (r *Repository) UpdateKYCStatus(userID int64, status string) error {
	_, err := r.db.Exec("UPDATE user_kyc SET status = ? WHERE user_id = ?", status, userID)
	return err
}

func (r *Repository) UpdateEnterpriseStatus(userID int64, status string) error {
	_, err := r.db.Exec("UPDATE enterprises SET status = ? WHERE user_id = ?", status, userID)
	return err
}
