package blockchain

import (
	"database/sql"
	"time"

	"github.com/jmoiron/sqlx"
)

// Attestation 对应 blockchain_attestations 表。该表同时充当上链队列:
// chain_status=pending 的行等待 worker 推送, confirmed=已上链, failed=死信(重试耗尽)。
// docs/14 原设计用 Redis/RabbitMQ 做队列, 这里用 DB 行状态替代 —— 存证行创建与业务
// 在同一进程内先落库, 崩溃/链故障都不丢消息, 且省去一套 MQ 运维。低流量下无性能顾虑。
type Attestation struct {
	ID          int64      `db:"id" json:"id"`
	TargetType  string     `db:"target_type" json:"target_type"`
	TargetID    string     `db:"target_id" json:"target_id"`
	DataHash    string     `db:"data_hash" json:"data_hash"`
	Signers     string     `db:"signers" json:"signers"`
	ChainTxID   *string    `db:"chain_tx_id" json:"chain_tx_id"`
	ChainStatus string     `db:"chain_status" json:"chain_status"`
	Attempts    int        `db:"attempts" json:"attempts"`
	LastError   *string    `db:"last_error" json:"last_error"`
	CreatedAt   time.Time  `db:"created_at" json:"created_at"`
	ConfirmedAt *time.Time `db:"confirmed_at" json:"confirmed_at"`
}

const attestationColumns = "id, target_type, target_id, data_hash, signers, chain_tx_id, chain_status, attempts, last_error, created_at, confirmed_at"

type Repository struct{ db *sqlx.DB }

func NewRepository(db *sqlx.DB) *Repository { return &Repository{db: db} }

func (r *Repository) CreateAttestation(a *Attestation) error {
	res, err := r.db.Exec(
		"INSERT INTO blockchain_attestations (target_type, target_id, data_hash, signers, chain_status) VALUES (?,?,?,?,'pending')",
		a.TargetType, a.TargetID, a.DataHash, a.Signers,
	)
	if err != nil {
		return err
	}
	a.ID, err = res.LastInsertId()
	return err
}

// GetLatestAttestation 取某业务对象最新一条存证。无记录返回 (nil, nil)。
func (r *Repository) GetLatestAttestation(targetType, targetID string) (*Attestation, error) {
	var a Attestation
	err := r.db.Get(&a,
		"SELECT "+attestationColumns+" FROM blockchain_attestations WHERE target_type=? AND target_id=? ORDER BY id DESC LIMIT 1",
		targetType, targetID)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// ListPending 取待上链的存证批次, 先进先出。
func (r *Repository) ListPending(limit int) ([]Attestation, error) {
	var list []Attestation
	err := r.db.Select(&list,
		"SELECT "+attestationColumns+" FROM blockchain_attestations WHERE chain_status='pending' ORDER BY id ASC LIMIT ?", limit)
	return list, err
}

func (r *Repository) MarkConfirmed(id int64, txID string) error {
	_, err := r.db.Exec(
		"UPDATE blockchain_attestations SET chain_tx_id=?, chain_status='confirmed', confirmed_at=NOW(), last_error=NULL WHERE id=?",
		txID, id)
	return err
}

// RecordFailure 记一次上链失败。attempts 持久化在行上, 进程重启不清零;
// 达到 maxAttempts 后置 failed(死信), 不再被 ListPending 捞起 (REQ-H-021)。
// MySQL 的 SET 从左到右求值, 后面的 IF 读到的是 +1 之后的 attempts。
func (r *Repository) RecordFailure(id int64, errMsg string, maxAttempts int) error {
	if len(errMsg) > 500 {
		errMsg = errMsg[:500]
	}
	_, err := r.db.Exec(
		"UPDATE blockchain_attestations SET attempts=attempts+1, last_error=?, chain_status=IF(attempts>=?, 'failed', 'pending') WHERE id=? AND chain_status='pending'",
		errMsg, maxAttempts, id)
	return err
}

// RequeueFailed 把死信重置回 pending(REQ-H-021 的"恢复后补推"), 供运维在修复故障后调用。
// 返回被重置的行数。
func (r *Repository) RequeueFailed() (int64, error) {
	res, err := r.db.Exec("UPDATE blockchain_attestations SET chain_status='pending', attempts=0 WHERE chain_status='failed'")
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
