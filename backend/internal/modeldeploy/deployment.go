package modeldeploy

import (
	"crypto/rand"
	"encoding/json"
	"strings"
	"time"
)

// 部署生命周期(docs/27 §27.6)。
const (
	DepPending    = "pending"
	DepChecking   = "checking"
	DepPulling    = "pulling"
	DepStarting   = "starting"
	DepRunning    = "running"
	DepRestarting = "restarting"
	DepStopping   = "stopping"
	DepStopped    = "stopped"
	DepFailed     = "failed"
)

// 健康状态, 只在 running/restarting 时有意义。
const (
	HealthUnknown   = "unknown"
	HealthHealthy   = "healthy"
	HealthDegraded  = "degraded"
	HealthUnhealthy = "unhealthy"
)

// liveStates 未终止状态: 占用 lease_active_claim, 引用的缓存条目不可淘汰。
var liveStates = []string{DepPending, DepChecking, DepPulling, DepStarting, DepRunning, DepRestarting, DepStopping}

// stoppableStates 买家/生命周期可发起停止的状态。
var stoppableStates = []string{DepPending, DepChecking, DepPulling, DepStarting, DepRunning, DepRestarting}

var validDepStates = map[string]bool{DepPending: true, DepChecking: true, DepPulling: true, DepStarting: true,
	DepRunning: true, DepRestarting: true, DepStopping: true, DepStopped: true, DepFailed: true}

var validHealth = map[string]bool{HealthUnknown: true, HealthHealthy: true, HealthDegraded: true, HealthUnhealthy: true}

// 停止原因(stop_reason)。
const (
	StopByBuyer      = "buyer"
	StopLeaseExpired = "lease_expired"
	StopLeaseFrozen  = "lease_frozen"
	StopLeasePurged  = "lease_purged"
	StopNightly      = "nightly"
)

// 部署事件(deployment_events.kind)。
const (
	EvAutoRestart     = "auto_restart"
	EvManualRestart   = "manual_restart"
	EvResume          = "resume"
	EvStop            = "stop"
	EvFail            = "fail"
	EvRunning         = "running"
	EvDegradedNotice  = "degraded_notice"
	EvUnhealthyNotice = "unhealthy_notice"
)

// 健康原因代码 → 中文说明(docs/27 §27.5.2)。
const (
	ReasonContainerDown     = "container_down"
	ReasonOOMKilled         = "oom_killed"
	ReasonHealthFailed      = "health_failed"
	ReasonCanaryFailed      = "canary_failed"
	ReasonGPUFatal          = "gpu_fatal"
	ReasonCapacitySaturated = "capacity_saturated"
	ReasonPreempting        = "preempting"
	ReasonTTFTSlow          = "ttft_slow"
	ReasonGPUHot            = "gpu_hot"
	ReasonECCRising         = "ecc_corrected_rising"
	ReasonRecentlyRestarted = "recently_restarted"
	ReasonNodeUnreachable   = "node_unreachable"
)

var reasonText = map[string]string{
	ReasonContainerDown:     "引擎容器未运行",
	ReasonOOMKilled:         "引擎容器因内存不足(OOM)退出, 建议缩短上下文或降低并发",
	ReasonHealthFailed:      "引擎 /health 连续 3 次检测失败",
	ReasonCanaryFailed:      "金丝雀请求连续 3 次失败",
	ReasonGPUFatal:          "所用 GPU 出现致命 Xid 或不可纠正 ECC 错误, 已停止自动重启, 平台将按资源异常处理",
	ReasonCapacitySaturated: "请求排队且 KV 缓存已满，建议降低并发、缩短上下文或增加卡数",
	ReasonPreempting:        "引擎持续发生请求抢占(KV 缓存不足), 建议降低并发或缩短上下文",
	ReasonTTFTSlow:          "首 token 延迟 P95 超过基线 3 倍",
	ReasonGPUHot:            "GPU 温度 ≥85°C 或出现温度/功耗降频",
	ReasonECCRising:         "GPU 可纠正 ECC 错误持续增长",
	ReasonRecentlyRestarted: "近 1 小时内发生过自动重启",
	ReasonNodeUnreachable:   "平台暂时无法连接节点, 健康状态未知(不推断为故障)",
}

// ReasonText 原因代码的中文说明; 未知代码原样返回。
func ReasonText(code string) string {
	if t, ok := reasonText[code]; ok {
		return t
	}
	return code
}

// Deployment model_deployments 一行(含 035 追加列)。
type Deployment struct {
	ID               int64           `db:"id"`
	DeployNo         string          `db:"deploy_no"`
	LeaseID          int64           `db:"lease_id"`
	OrderID          int64           `db:"order_id"`
	BuyerID          int64           `db:"buyer_id"`
	NodeID           int64           `db:"node_id"`
	ModelID          int64           `db:"model_id"`
	GPUUUIDsRaw      json.RawMessage `db:"gpu_uuids"`
	TP               int             `db:"tp"`
	DP               int             `db:"dp"`
	MaxModelLen      int             `db:"max_model_len"`
	GPUMemUtil       float64         `db:"gpu_mem_util"`
	EnginePort       *int            `db:"engine_port"`
	EnginePortClaim  *int            `db:"engine_port_claim"`
	LeaseActiveClaim *int64          `db:"lease_active_claim"`
	ContainerName    *string         `db:"container_name"`
	EngineCPUs       *int            `db:"engine_cpus"`
	EngineMemoryGB   *int            `db:"engine_memory_gb"`
	LeaseCPUs        *int            `db:"lease_cpus"`
	LeaseMemoryGB    *int            `db:"lease_memory_gb"`
	State            string          `db:"state"`
	StateChangedAt   *time.Time      `db:"state_changed_at"`
	Health           string          `db:"health"`
	HealthReason     *string         `db:"health_reason"`
	CanaryEnabled    bool            `db:"canary_enabled"`
	AutoResume       bool            `db:"auto_resume"`
	RestartCount     int             `db:"restart_count"`
	LastRestartAt    *time.Time      `db:"last_restart_at"`
	NextRestartAt    *time.Time      `db:"next_restart_at"`
	BaselineCanaryMS *int            `db:"baseline_canary_ms"`
	MonitorStateRaw  json.RawMessage `db:"monitor_state"`
	Feasibility      json.RawMessage `db:"feasibility"`
	LicenseAckAt     *time.Time      `db:"license_ack_at"`
	UntestedAckAt    *time.Time      `db:"untested_ack_at"`
	FailReason       *string         `db:"fail_reason"`
	StopReason       *string         `db:"stop_reason"`
	LogTail          *string         `db:"log_tail"`
	StartedAt        *time.Time      `db:"started_at"`
	StoppedAt        *time.Time      `db:"stopped_at"`
	CreatedAt        time.Time       `db:"created_at"`
	UpdatedAt        time.Time       `db:"updated_at"`
}

const deploymentColumns = `id, deploy_no, lease_id, order_id, buyer_id, node_id, model_id, gpu_uuids, tp, dp,
	max_model_len, gpu_mem_util, engine_port, engine_port_claim, lease_active_claim, container_name,
	engine_cpus, engine_memory_gb, lease_cpus, lease_memory_gb, state, state_changed_at, health, health_reason,
	canary_enabled, auto_resume, restart_count, last_restart_at, next_restart_at, baseline_canary_ms,
	COALESCE(monitor_state, 'null') AS monitor_state, feasibility, license_ack_at, untested_ack_at,
	fail_reason, stop_reason, log_tail, started_at, stopped_at, created_at, updated_at`

// GPUUUIDs 部署使用的卡(提交时从租约卡中按序选取)。
func (d *Deployment) GPUUUIDs() []string {
	var out []string
	_ = json.Unmarshal(d.GPUUUIDsRaw, &out)
	return out
}

// EngineContainer 引擎容器名 omnis-<deploy_no>(deploy_no 形如 d-xxxxxxxx, 与租约容器 omnis-l-… 同一命名规则)。
func EngineContainer(deployNo string) string { return "omnis-" + deployNo }

const deployNoAlphabet = "abcdefghijklmnopqrstuvwxyz234567"

// newDeployNo d- + 8 位 base32 小写(40 bit 随机), 冲突由 deploy_no 唯一键兜底重试。
func newDeployNo() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	var sb strings.Builder
	sb.WriteString("d-")
	for _, c := range b {
		sb.WriteByte(deployNoAlphabet[int(c)%32])
	}
	return sb.String(), nil
}

// ValidDeployNo 路径参数校验, 也保证拼进节点命令的容器名安全。
func ValidDeployNo(s string) bool {
	if len(s) != 10 || !strings.HasPrefix(s, "d-") {
		return false
	}
	for _, c := range s[2:] {
		if !(c >= 'a' && c <= 'z') && !(c >= '2' && c <= '7') {
			return false
		}
	}
	return true
}

func strp(s string) *string { return &s }

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func isOneOf(s string, set []string) bool {
	for _, x := range set {
		if x == s {
			return true
		}
	}
	return false
}

// inList 生成 SQL IN 占位与参数。
func inList(vals []string) (string, []any) {
	ph := make([]string, len(vals))
	args := make([]any, len(vals))
	for i, v := range vals {
		ph[i] = "?"
		args[i] = v
	}
	return strings.Join(ph, ","), args
}
