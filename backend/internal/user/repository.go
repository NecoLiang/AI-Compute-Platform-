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
	ID             int64     `db:"id" json:"id"`
	UserID         int64     `db:"user_id" json:"user_id"`
	Name           string    `db:"name" json:"name"`
	USCC           string    `db:"uscc" json:"uscc"`
	LicenseURL     string    `db:"license_url" json:"license_url"`
	LegalPerson    string    `db:"legal_person" json:"legal_person"`
	Status         string    `db:"status" json:"status"` // pending/verified/rejected
	CreatedAt      time.Time `db:"created_at" json:"created_at"`
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
		"INSERT INTO user_kyc (user_id, real_name, id_card, status) VALUES (?, ?, ?, 'pending') ON DUPLICATE KEY UPDATE real_name=?, id_card=?, status='pending'",
		userID, realName, idCard, realName, idCard,
	)
	return err
}

func (r *Repository) GetPersonalKYC(userID int64) (*PersonalKYC, error) {
	var kyc PersonalKYC
	err := r.db.Get(&kyc, "SELECT * FROM user_kyc WHERE user_id = ?", userID)
	return &kyc, err
}

// Enterprise
func (r *Repository) CreateEnterprise(userID int64, name, uscc, licenseURL, legalPerson string) error {
	_, err := r.db.Exec(
		"INSERT INTO enterprises (user_id, name, uscc, license_url, legal_person, status) VALUES (?,?,?,?,?,'pending') ON DUPLICATE KEY UPDATE name=?, uscc=?, license_url=?, legal_person=?, status='pending'",
		userID, name, uscc, licenseURL, legalPerson, name, uscc, licenseURL, legalPerson,
	)
	return err
}

func (r *Repository) GetEnterprise(userID int64) (*Enterprise, error) {
	var ent Enterprise
	err := r.db.Get(&ent, "SELECT * FROM enterprises WHERE user_id = ?", userID)
	return &ent, err
}

// Roles
func (r *Repository) AddRole(userID int64, role string) error {
	_, err := r.db.Exec(
		"INSERT IGNORE INTO user_roles (user_id, role) VALUES (?, ?)", userID, role,
	)
	return err
}

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
