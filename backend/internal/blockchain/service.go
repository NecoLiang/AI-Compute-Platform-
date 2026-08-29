package blockchain

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// ===== 存证数据载荷 (docs/14 §14.3) =====
//
// ⚠️ 哈希稳定性契约: 载荷经 json.Marshal 后取 SHA-256, 字段顺序即声明顺序。
// 这些结构体只允许追加字段(追加会改变新数据的 hash, 但不破坏历史数据的可验证性),
// 禁止改名/删除/调序 —— 否则历史存证将全部无法重算比对。
// 字段一律取自业务表中写后不再变更的列, 保证任意时刻重算结果一致。

// OrderPayload 订单创建存证 (REQ-H-010)。
type OrderPayload struct {
	OrderNo        string `json:"order_no"`
	BuyerIDHash    string `json:"buyer_id_hash"`    // 不上链明文 ID, 满足选择性披露
	SupplierIDHash string `json:"supplier_id_hash"`
	Spec           string `json:"spec"` // 规格摘要, 只含下单后不可变的列
	TotalAmountFen int64  `json:"total_amount_fen"`
	PlacedAt       string `json:"placed_at"` // orders.created_at, UTC RFC3339
}

// DeliveryPayload 交付确认存证 (REQ-H-011)。order_hash 与订单存证形成链式关联。
type DeliveryPayload struct {
	OrderNo      string `json:"order_no"`
	OrderHash    string `json:"order_hash"`
	ConfirmedAt  string `json:"confirmed_at"`   // order_deliveries.buyer_confirmed_at
	LeaseStartAt string `json:"lease_start_at"` // 签收时一次性写入; 续费只改 lease_end, 不影响本载荷
}

// ViolationPayload 风控违规存证 (REQ-H-014)。
// 风控规则引擎(T-055)未落地前, 违规类型/结论没有持久化表, 载荷无法从库中重算,
// 验证时只能做「入库 hash ↔ 链上 hash」比对; 引擎落地后应补 source 注册。
type ViolationPayload struct {
	TargetNo   string `json:"target_no"`
	Violation  string `json:"violation"`
	Conclusion string `json:"conclusion"`
}

// HashID 业务主体 ID 的单向摘要。前缀固定, 不可更换 —— 换了历史存证就重算不出来。
func HashID(id int64) string {
	h := sha256.Sum256([]byte(fmt.Sprintf("wanxiang-uid:%d", id)))
	return hex.EncodeToString(h[:])
}

// FormatTime 载荷内时间的统一表示。MySQL TIMESTAMP 精度为秒, 统一转 UTC 后格式化,
// 保证不同会话时区下重算结果一致。
func FormatTime(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// ComputeHash 载荷的规范化哈希。依赖 struct 字段声明顺序的稳定性, 见上方契约注释。
func ComputeHash(data any) string {
	bytes, _ := json.Marshal(data)
	h := sha256.Sum256(bytes)
	return "0x" + hex.EncodeToString(h[:])
}

// ===== 签名 (docs/14 §14.4, T-061 分期落地) =====
//
// 当前阶段只有「平台见证签名」: 服务端 Ed25519 密钥对 data_hash 签名, 证明平台在该
// 时点见证了这条数据。买家/机房签名依赖前端签名组件, 属 T-061 后续工作; 落地后
// 签名将并入上链 hash 本体(REQ-H-011 的三方签名), 当前先以 signers 列记录。

type Signer struct {
	Role      string `json:"role"` // buyer/supplier/platform/system/risk
	Signature string `json:"signature"`
	SignedAt  string `json:"signed_at"`
}

type SourceFunc func(targetID string) (any, error)

type Service struct {
	repo    *Repository
	bsn     *BSNClient
	signKey ed25519.PrivateKey // 平台见证签名私钥, 未配置为 nil
	mu      sync.RWMutex
	sources map[string]SourceFunc
}

// NewService 装配存证服务。signSeedHex 为 64 位 hex 的 Ed25519 种子, 传空串表示
// 暂不签名(存证仍可用, signers 为空列表并记警告日志)。
func NewService(repo *Repository, bsn *BSNClient, signSeedHex string) (*Service, error) {
	s := &Service{repo: repo, bsn: bsn, sources: map[string]SourceFunc{}}
	if signSeedHex != "" {
		seed, err := hex.DecodeString(signSeedHex)
		if err != nil || len(seed) != ed25519.SeedSize {
			return nil, fmt.Errorf("blockchain.sign_key_seed 须为 64 位 hex(32 字节 Ed25519 种子)")
		}
		s.signKey = ed25519.NewKeyFromSeed(seed)
	}
	return s, nil
}

// RegisterSource 注册「从业务库重建载荷」的取数函数, Verify 用它重算 hash (REQ-H-030)。
// 未注册的 target_type 验证时退化为入库 hash 与链上比对。
func (s *Service) RegisterSource(targetType string, fn SourceFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sources[targetType] = fn
}

func (s *Service) source(targetType string) SourceFunc {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sources[targetType]
}

// Attest 记录一条存证并排队上链 (REQ-H-020 异步: 本方法只落库, 毫秒级返回,
// 实际上链由 RunWorker 完成, 链故障不影响调用方)。
func (s *Service) Attest(targetType, targetID string, payload any) error {
	dataHash := ComputeHash(payload)
	signers := []Signer{}
	if s.signKey != nil {
		sig := ed25519.Sign(s.signKey, []byte(dataHash))
		signers = append(signers, Signer{
			Role:      "platform",
			Signature: hex.EncodeToString(sig),
			SignedAt:  FormatTime(time.Now()),
		})
	}
	signersJSON, _ := json.Marshal(signers)
	return s.repo.CreateAttestation(&Attestation{
		TargetType: targetType,
		TargetID:   targetID,
		DataHash:   dataHash,
		Signers:    string(signersJSON),
	})
}

// ===== 验证 (REQ-H-030) =====

type VerifyResult struct {
	Verified       bool   `json:"verified"`
	TxID           string `json:"tx_id"`
	ChainStatus    string `json:"chain_status"`
	DataHash       string `json:"data_hash"`
	DBHashMatch    *bool  `json:"db_hash_match,omitempty"` // 业务数据重算 hash 与入库 hash 是否一致; 无 source 时为 null
	ChainTimestamp string `json:"chain_timestamp"`
	VerifyURL      string `json:"verify_url"`
	Note           string `json:"note,omitempty"`
}

// Verify 按 REQ-H-030 的流程验证: 业务库取原始数据 → 重算 hash → 与入库/链上比对。
// verified=true 的唯一条件是「重算 hash 一致 && 链上存在该 hash」——
// BSN 未接入、未上链、数据被改动, 一律如实返回 false, 不虚报。
func (s *Service) Verify(ctx context.Context, targetType, targetID string) (*VerifyResult, error) {
	att, err := s.repo.GetLatestAttestation(targetType, targetID)
	if err != nil {
		return nil, err
	}
	if att == nil {
		return &VerifyResult{Verified: false, Note: "无存证记录"}, nil
	}

	res := &VerifyResult{
		ChainStatus:    att.ChainStatus,
		DataHash:       att.DataHash,
		ChainTimestamp: FormatTime(att.CreatedAt),
	}
	if att.ChainTxID != nil {
		res.TxID = *att.ChainTxID
		res.VerifyURL = s.bsn.TxURL(*att.ChainTxID)
	}
	if att.ConfirmedAt != nil {
		res.ChainTimestamp = FormatTime(*att.ConfirmedAt)
	}

	// 1. 重算业务数据 hash。重算不一致说明库内业务数据在存证后被改动, 直接判 false。
	hashToCheck := att.DataHash
	if fn := s.source(targetType); fn != nil {
		payload, err := fn(targetID)
		if err != nil {
			res.Note = "业务数据读取失败: " + err.Error()
			return res, nil
		}
		recomputed := ComputeHash(payload)
		match := recomputed == att.DataHash
		res.DBHashMatch = &match
		if !match {
			res.Note = "业务数据与存证 hash 不一致: 数据在存证后被改动过"
			return res, nil
		}
		hashToCheck = recomputed
	}

	// 2. 链上比对。
	if !s.bsn.Configured() {
		res.Note = "BSN 未接入, 仅完成库内 hash 比对, 不声称链上已验证"
		return res, nil
	}
	if att.ChainStatus != "confirmed" || att.ChainTxID == nil {
		res.Note = "存证尚未上链 (status=" + att.ChainStatus + ")"
		return res, nil
	}
	exists, txID, err := s.bsn.VerifyHash(ctx, *att.ChainTxID, hashToCheck)
	if err != nil {
		res.Note = "链上查询失败: " + err.Error()
		return res, nil
	}
	if txID != "" {
		res.TxID = txID
		res.VerifyURL = s.bsn.TxURL(txID)
	}
	res.Verified = exists
	if !exists {
		res.Note = "链上未找到该 hash"
	}
	return res, nil
}

// GetAttestation 供 handler 查询原始存证记录。
func (s *Service) GetAttestation(targetType, targetID string) (*Attestation, error) {
	return s.repo.GetLatestAttestation(targetType, targetID)
}

// RequeueFailed 死信补推入口 (REQ-H-021), 供运维接口调用。
func (s *Service) RequeueFailed() (int64, error) { return s.repo.RequeueFailed() }
