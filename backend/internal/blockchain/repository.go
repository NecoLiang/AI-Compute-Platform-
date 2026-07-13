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

// ===== BSN-DDC 客户端 =====
// TODO: 接入 BSN-DDC 文昌链后替换本实现
// 所需信息:
//   1. BSN 开放平台 API Key (在 https://www.bsnbase.com 注册后获取)
//   2. 存证合约地址 (使用 BSN 官方标准存证合约模板)
//   3. 用户网关地址 (BSN 开放联盟链网关)
//   4. 区块链信息服务备案号 (向网信办备案后获取，BSN 商务可协助)
// 接入文档: https://ddc.bsnbase.com/

type BSNClient struct {
	// apiKey     string // TODO: 从配置加载
	// gatewayURL string // TODO: 从配置加载
}

func NewBSNClient() *BSNClient { return &BSNClient{} }

func (b *BSNClient) UploadHash(hash string) (string, error) {
	return "", ErrBSNNotConfigured
}

func (b *BSNClient) VerifyHash(hash string) (bool, string, error) {
	return false, "", ErrBSNNotConfigured
}

var ErrBSNNotConfigured = fmt.Errorf("BSN-DDC 区块链未接入: 需配置 BSN API Key + 存证合约地址 + 用户网关地址")

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
		select {
		case <-ctx.Done():
			return
		default:
		}
		result, err := s.rdb.BRPop(ctx, 0, "attestation_queue").Result()
		if err != nil { continue }
		var event AttestEvent
		json.Unmarshal([]byte(result[1]), &event)
		hash := ComputeHash(event.Data)
		txID, err := s.bsn.UploadHash(hash)
		if err != nil {
			// BSN 未接入，暂存 DB 但不更新链上状态
			continue
		}
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
	if att.ChainTxID == "" { return &VerifyResult{Verified: false}, nil }
	verified, _, err := s.bsn.VerifyHash(att.DataHash)
	if err != nil {
		// BSN 未接入时，返回 DB 中的记录但不声称已链上验证
		return &VerifyResult{Verified: false, TxID: att.ChainTxID}, nil
	}
	return &VerifyResult{
		Verified: verified, TxID: att.ChainTxID,
		ChainTimestamp: att.CreatedAt.Format(time.RFC3339),
		VerifyURL:      fmt.Sprintf("https://bsnscan.com/tx/%s", att.ChainTxID),
	}, nil
}
