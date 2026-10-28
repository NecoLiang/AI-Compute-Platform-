// Package modeldeploy 模型一键部署(docs/27): 模型目录、真机验证记录、可行性判定。
// 部署流水线与健康检测(T-098/T-099)依赖 S2 的租约与节点任务队列, 不在本包当前范围。
package modeldeploy

import (
	"encoding/json"
	"time"
)

// 目录条目状态。只有 draft 可编辑; active 才对买家可见; retired 不影响运行中部署。
const (
	StatusDraft   = "draft"
	StatusActive  = "active"
	StatusRetired = "retired"
)

// commercial_use 取值, 与 docs/12 §12.2 模型版权台账一一对应。
const (
	CommercialAllowed    = "allowed"
	CommercialRestricted = "restricted"
	CommercialUnknown    = "unknown"
)

// ModelEntry 模型目录条目(model_catalog 一行)。
type ModelEntry struct {
	ID                   int64           `db:"id" json:"id"`
	ModelKey             string          `db:"model_key" json:"model_key"`
	DisplayName          string          `db:"display_name" json:"display_name"`
	Source               string          `db:"source" json:"source"`
	Repo                 string          `db:"repo" json:"repo"`
	Revision             string          `db:"revision" json:"revision"`
	Manifest             json.RawMessage `db:"manifest" json:"manifest"`
	WeightsBytes         int64           `db:"weights_bytes" json:"weights_bytes"`
	Quant                string          `db:"quant" json:"quant"`
	MinComputeCapability string          `db:"min_compute_capability" json:"min_compute_capability"`
	ValidTP              string          `db:"valid_tp" json:"valid_tp"`
	KVBytesPerToken      int64           `db:"kv_bytes_per_token" json:"kv_bytes_per_token"`
	KVReplicatedAcrossTP bool            `db:"kv_replicated_across_tp" json:"kv_replicated_across_tp"`
	RuntimeOverheadBytes int64           `db:"runtime_overhead_bytes" json:"runtime_overhead_bytes"`
	MaxModelLen          int             `db:"max_model_len" json:"max_model_len"`
	MinContextTokens     int             `db:"min_context_tokens" json:"min_context_tokens"`
	Engine               string          `db:"engine" json:"engine"`
	EngineImage          string          `db:"engine_image" json:"engine_image"`
	MinDriverVersion     string          `db:"min_driver_version" json:"min_driver_version"`
	EngineArgs           json.RawMessage `db:"engine_args" json:"engine_args"`
	MetricsMap           json.RawMessage `db:"metrics_map" json:"metrics_map"`
	StartupTimeoutS      int             `db:"startup_timeout_s" json:"startup_timeout_s"`
	License              string          `db:"license" json:"license"`
	CommercialUse        string          `db:"commercial_use" json:"commercial_use"`
	Status               string          `db:"status" json:"status"`
	CreatedBy            int64           `db:"created_by" json:"created_by"`
	CreatedAt            time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt            time.Time       `db:"updated_at" json:"updated_at"`
}

const entryColumns = `id, model_key, display_name, source, repo, revision, manifest, weights_bytes, quant,
	min_compute_capability, valid_tp, kv_bytes_per_token, kv_replicated_across_tp, runtime_overhead_bytes,
	max_model_len, min_context_tokens, engine, engine_image, min_driver_version, engine_args, metrics_map,
	startup_timeout_s, license, commercial_use, status, created_by, created_at, updated_at`

// Verification 真机验证记录(model_catalog_verifications 一行)。
type Verification struct {
	ID                    int64     `db:"id" json:"id"`
	ModelID               int64     `db:"model_id" json:"model_id"`
	GPUModel              string    `db:"gpu_model" json:"gpu_model"`
	Cards                 int       `db:"cards" json:"cards"`
	TP                    int       `db:"tp" json:"tp"`
	MaxModelLen           int       `db:"max_model_len" json:"max_model_len"`
	MeasuredOverheadBytes *int64    `db:"measured_overhead_bytes" json:"measured_overhead_bytes"`
	Result                string    `db:"result" json:"result"`
	Notes                 *string   `db:"notes" json:"notes"`
	TestedBy              int64     `db:"tested_by" json:"tested_by"`
	TestedAt              time.Time `db:"tested_at" json:"tested_at"`
}

const verificationColumns = `id, model_id, gpu_model, cards, tp, max_model_len, measured_overhead_bytes, result, notes, tested_by, tested_at`

// ManifestFile 权重清单中的单个文件; 拉取后逐一校验 sha256(docs/27 §27.4.3)。
type ManifestFile struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
}

// PublicModel 买家可见的目录条目(GET /models)。不含清单、引擎参数、指标映射等运维字段。
type PublicModel struct {
	ID                   int64    `json:"id"`
	ModelKey             string   `json:"model_key"`
	DisplayName          string   `json:"display_name"`
	Source               string   `json:"source"`
	Repo                 string   `json:"repo"`
	Revision             string   `json:"revision"`
	Quant                string   `json:"quant"`
	WeightsBytes         int64    `json:"weights_bytes"`
	MinComputeCapability string   `json:"min_compute_capability"`
	ValidTP              []int    `json:"valid_tp"`
	MaxModelLen          int      `json:"max_model_len"`
	MinContextTokens     int      `json:"min_context_tokens"`
	License              string   `json:"license"`
	CommercialUse        string   `json:"commercial_use"`
	VerifiedGPUModels    []string `json:"verified_gpu_models"`
}
