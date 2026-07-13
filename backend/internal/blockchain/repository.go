package blockchain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"time"
	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
)

type Attestation struct {
	ID          int64     `db:"id" json:"id"`
	TargetType  string    `db:"target_type" json:"target_type"`
	TargetID    string    `db:"target_id" json:"target_id"`
	DataHash    string    `db:"data_hash" json:"data_hash"`
	Signers     string    `db:"signers" json:"signers"`
	ChainTxID   string    `db:"chain_tx_id" json:"chain_tx_id"`
	ChainStatus string    `db:"chain_status" json:"chain_status"`
	CreatedAt   time.Time `db:"created_at" json:"created_at"`
}

type Repository struct{ db *sqlx.DB }
func NewRepository(db *sqlx.DB) *Repository { return &Repository{db: db} }

func (r *Repository) CreateAttestation(a *Attestation) error {
	_, err := r.db.Exec(
		"INSERT INTO blockchain_attestations (target_type, target_id, data_hash, signers, chain_tx_id, chain_status) VALUES (?,?,?,?,?,?)",
		a.TargetType, a.TargetID, a.DataHash, a.Signers, a.ChainTxID, "pending",
	)
	return err
}

func (r *Repository) ConfirmAttestation(targetType, targetID, txID string) error {
	_, err := r.db.Exec(
		"UPDATE blockchain_attestations SET chain_tx_id=?, chain_status='confirmed', confirmed_at=NOW() WHERE target_type=? AND target_id=?",
		txID, targetType, targetID,
	)
	return err
}

func (r *Repository) GetAttestation(targetType, targetID string) (*Attestation, error) {
	var a Attestation
	err := r.db.Get(&a, "SELECT * FROM blockchain_attestations WHERE target_type=? AND target_id=? ORDER BY id DESC LIMIT 1", targetType, targetID)
	return &a, err
}

type BSNClient struct{}
func NewBSNClient() *BSNClient { return &BSNClient{} }

func (b *BSNClient) UploadHash(hash string) (string, error) {
	txID := fmt.Sprintf("0x%s_%d", hash[:16], time.Now().UnixNano())
	return txID, nil
}

func (b *BSNClient) VerifyHash(hash string) (bool, string, error) {
	return true, fmt.Sprintf("0x%s_verified", hash[:16]), nil
}

type Service struct {
	repo  *Repository
	rdb   *redis.Client
	bsn   *BSNClient
}

func NewService(repo *Repository, rdb *redis.Client) *Service {
	return &Service{repo: repo, rdb: rdb, bsn: NewBSNClient()}
}

type AttestEvent struct {
	TargetType string `json:"target_type"`
	TargetID   string `json:"target_id"`
	Data       any    `json:"data"`
	Signers    []Signer `json:"signers"`
}

type Signer struct {
	Role      string `json:"role"` // buyer/supplier/platform/system
	Signature string `json:"signature"`
}

func ComputeHash(data any) string {
	bytes, _ := json.Marshal(data)
	h := sha256.Sum256(bytes)
	return "0x" + hex.EncodeToString(h[:])
}

func (s *Service) PublishEvent(ctx context.Context, event AttestEvent) error {
	dataHash := ComputeHash(event.Data)
	signersJSON, _ := json.Marshal(event.Signers)

	a := &Attestation{
		TargetType: event.TargetType,
		TargetID:   event.TargetID,
		DataHash:   dataHash,
		Signers:    string(signersJSON),
	}
	if err := s.repo.CreateAttestation(a); err != nil { return err }

	// Async: push to Redis queue for BSN upload
	eventJSON, _ := json.Marshal(event)
	return s.rdb.LPush(ctx, "attestation_queue", string(eventJSON)).Err()
}

func (s *Service) ProcessWorker(ctx context.Context) {
	for {
		result, err := s.rdb.BRPop(ctx, 0, "attestation_queue").Result()
		if err != nil { continue }
		var event AttestEvent
		json.Unmarshal([]byte(result[1]), &event)
		hash := ComputeHash(event.Data)
		txID, err := s.bsn.UploadHash(hash)
		if err != nil { continue }
		s.repo.ConfirmAttestation(event.TargetType, event.TargetID, txID)
	}
}

type VerifyResult struct {
	Verified       bool   `json:"verified"`
	TxID           string `json:"tx_id"`
	ChainTimestamp string `json:"chain_timestamp"`
	VerifyURL      string `json:"verify_url"`
}

func (s *Service) Verify(targetType, targetID string) (*VerifyResult, error) {
	att, err := s.repo.GetAttestation(targetType, targetID)
	if err != nil { return &VerifyResult{Verified: false}, nil }
	verified, _, _ := s.bsn.VerifyHash(att.DataHash)
	return &VerifyResult{
		Verified: verified, TxID: att.ChainTxID,
		ChainTimestamp: att.CreatedAt.Format(time.RFC3339),
		VerifyURL:      fmt.Sprintf("https://bsnscan.com/tx/%s", att.ChainTxID),
	}, nil
}
