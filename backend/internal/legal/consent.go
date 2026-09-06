package legal

import (
	"errors"
	"time"

	"github.com/jmoiron/sqlx"
)

const Version = "2026-09-06.1"

var ErrVersion = errors.New("协议已更新，请刷新页面并阅读、同意当前版本后重试")

type Acceptance struct {
	Document string `json:"document" db:"document_key"`
	Version  string `json:"version" db:"document_version"`
}

type Consent struct {
	Acceptance
	Action     string    `json:"action" db:"action"`
	Reference  string    `json:"reference" db:"reference_id"`
	AcceptedAt time.Time `json:"accepted_at" db:"accepted_at"`
}

func ValidateVersion(version string) error {
	if version != Version {
		return ErrVersion
	}
	return nil
}

// Record participates in the business transaction; failed writes cannot leave unrecorded consent.
func Record(tx *sqlx.Tx, userID int64, acceptance Acceptance, action, reference string) error {
	if err := ValidateVersion(acceptance.Version); err != nil {
		return err
	}
	_, err := tx.Exec(`INSERT INTO legal_consents (user_id,document_key,document_version,action,reference_id)
		VALUES (?,?,?,?,?)`, userID, acceptance.Document, acceptance.Version, action, reference)
	return err
}

func List(db *sqlx.DB, userID int64) ([]Consent, error) {
	items := []Consent{}
	err := db.Select(&items, `SELECT document_key,document_version,action,reference_id,accepted_at
		FROM legal_consents WHERE user_id=? ORDER BY id DESC LIMIT 100`, userID)
	return items, err
}
