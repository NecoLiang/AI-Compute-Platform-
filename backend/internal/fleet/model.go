// Package fleet 托管零租的节点接入、探活与任务队列(docs/25 §25.4, S1)。
//
// 表归属(docs/25 §25.3): gpu_slots / node_probe_reports / node_tasks 以及 supplier_nodes 的
// 托管列(026 新增)由本包读写; 商品与订单归 compute, 节点基础列与健康度聚合归 scheduler,
// 两者都通过注入的接口调用, compute 与 scheduler 不 import fleet。
package fleet

import (
	"encoding/json"
	"time"
)

func jsonUnmarshal(raw []byte, v any) error {
	if len(raw) == 0 {
		return json.Unmarshal([]byte("null"), v)
	}
	return json.Unmarshal(raw, v)
}

// 节点核验状态(supplier_nodes.verify_status, docs/25 §25.8.1)。
const (
	VerifyUnverified   = "unverified"
	VerifyVerifying    = "verifying"
	VerifyVerified     = "verified"
	VerifyFailed       = "failed"
	VerifyKeyMismatch  = "key_mismatch"
	VerifyNeedsReimage = "needs_reimage"
	VerifySuspended    = "suspended"
)

// 卡槽状态(gpu_slots.state)。
const (
	SlotFree     = "free"
	SlotReserved = "reserved"
	SlotLeased   = "leased"
	SlotDraining = "draining"
	SlotFaulty   = "faulty"
	SlotWithheld = "withheld"
)

// 任务状态与类型(node_tasks)。S1 只执行 kind=probe; 其余类型由 S2 起的 worker 扩展。
const (
	TaskPending   = "pending"
	TaskRunning   = "running"
	TaskSucceeded = "succeeded"
	TaskFailed    = "failed"
	TaskDead      = "dead"

	KindProbe        = "probe"
	KindProvision    = "provision"
	KindKillSessions = "kill_sessions"
	KindStop         = "stop"
	KindPurge        = "purge"
)

// 探活模式(写在 probe 任务 payload 中)。
const (
	ProbeFull        = "full"         // P1-P13
	ProbePortRecheck = "port_recheck" // P1-P3 + P11/P12
)

// Node supplier_nodes 一行(含托管列)。凭证类字段一律不出 JSON。
type Node struct {
	ID                 int64      `db:"id" json:"id"`
	SupplierID         int64      `db:"supplier_id" json:"supplier_id"`
	ProductID          int64      `db:"product_id" json:"product_id"`
	NodeName           string     `db:"node_name" json:"node_name"`
	Status             string     `db:"status" json:"status"`
	TotalCards         int        `db:"total_cards" json:"total_cards"`
	AvailableCards     int        `db:"available_cards" json:"available_cards"`
	LastHeartbeatAt    *time.Time `db:"last_heartbeat_at" json:"last_heartbeat_at"`
	MgmtMode           string     `db:"mgmt_mode" json:"mgmt_mode"`
	SSHHost            *string    `db:"ssh_host" json:"ssh_host"`
	SSHPort            *int       `db:"ssh_port" json:"ssh_port"`
	SSHUser            *string    `db:"ssh_user" json:"ssh_user"`
	MgmtPubkey         *string    `db:"mgmt_pubkey" json:"-"`
	MgmtPrivkeyEnc     *string    `db:"mgmt_privkey_encrypted" json:"-"`
	HostKeyFingerprint *string    `db:"host_key_fingerprint" json:"host_key_fingerprint"`
	HostKeyConfirmedAt *time.Time `db:"host_key_confirmed_at" json:"host_key_confirmed_at"`
	HostFPClaim        *string    `db:"host_fp_claim" json:"-"`
	VerifyStatus       string     `db:"verify_status" json:"verify_status"`
	Generation         int        `db:"generation" json:"-"` // 探活代次(见 ProbePayload.Generation)
	Schedulable        bool       `db:"schedulable" json:"schedulable"`
	// DispatchHold 平台按信用/停机自动停调度(T-105, 不接新托管租约; 与供给方自己的 schedulable 排空互不覆盖)。
	DispatchHold       bool       `db:"dispatch_hold" json:"dispatch_hold"`
	DispatchHoldReason *string    `db:"dispatch_hold_reason" json:"dispatch_hold_reason"`
	VerifiedAt         *time.Time `db:"verified_at" json:"verified_at"`
	LastProbeOKAt      *time.Time `db:"last_probe_ok_at" json:"last_probe_ok_at"`
	ProbeFailCount     int        `db:"probe_fail_count" json:"probe_fail_count"`
	CPUCores           *int       `db:"cpu_cores" json:"cpu_cores"`
	MemGB              *int       `db:"mem_gb" json:"mem_gb"`
	DiskFreeGB         *int       `db:"disk_free_gb" json:"disk_free_gb"`
	DiskPerCardGB      *int       `db:"disk_per_card_gb" json:"disk_per_card_gb"`
	DiskQuotaSupported *bool      `db:"disk_quota_supported" json:"disk_quota_supported"`
	DriverVersion      *string    `db:"driver_version" json:"driver_version"`
	CUDAVersion        *string    `db:"cuda_version" json:"cuda_version"`
	LeasePortStart     *int       `db:"lease_port_start" json:"lease_port_start"`
	LeasePortEnd       *int       `db:"lease_port_end" json:"lease_port_end"`
	CreatedAt          time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt          time.Time  `db:"updated_at" json:"updated_at"`
}

const nodeColumns = `id, supplier_id, product_id, node_name, status, total_cards, available_cards, last_heartbeat_at,
	mgmt_mode, ssh_host, ssh_port, ssh_user, mgmt_pubkey, mgmt_privkey_encrypted, host_key_fingerprint,
	host_key_confirmed_at, host_fp_claim, verify_status, generation, schedulable, dispatch_hold, dispatch_hold_reason, verified_at, last_probe_ok_at,
	probe_fail_count, cpu_cores, mem_gb, disk_free_gb, disk_per_card_gb, disk_quota_supported,
	driver_version, cuda_version, lease_port_start, lease_port_end, created_at, updated_at`

// hostKeyConfirmed 指纹已由供给方确认(钉死)。
func (n *Node) hostKeyConfirmed() bool {
	return n.HostKeyFingerprint != nil && *n.HostKeyFingerprint != "" && n.HostKeyConfirmedAt != nil
}

func (n *Node) sshReady() bool {
	return n.SSHHost != nil && *n.SSHHost != "" && n.SSHPort != nil && n.SSHUser != nil && *n.SSHUser != ""
}

// Slot gpu_slots 一行。
type Slot struct {
	ID             int64     `db:"id" json:"id"`
	NodeID         int64     `db:"node_id" json:"node_id"`
	GPUIndex       int       `db:"gpu_index" json:"gpu_index"`
	GPUUUID        string    `db:"gpu_uuid" json:"gpu_uuid"`
	PCIBusID       string    `db:"pci_bus_id" json:"pci_bus_id"`
	VRAMMB         int       `db:"vram_mb" json:"vram_mb"`
	TopoGroup      string    `db:"topo_group" json:"topo_group"`
	State          string    `db:"state" json:"state"`
	LeaseID        *int64    `db:"lease_id" json:"lease_id"`
	StateChangedAt time.Time `db:"state_changed_at" json:"state_changed_at"`
}

// ProbeReport node_probe_reports 一行。checks/inventory 不含任何凭证。
type ProbeReport struct {
	ID        int64           `db:"id" json:"id"`
	NodeID    int64           `db:"node_id" json:"node_id"`
	Kind      string          `db:"kind" json:"kind"`
	Result    string          `db:"result" json:"result"`
	Checks    json.RawMessage `db:"checks" json:"checks"`
	Inventory json.RawMessage `db:"inventory" json:"inventory"`
	CreatedAt time.Time       `db:"created_at" json:"created_at"`
}

// Task node_tasks 一行。
type Task struct {
	ID           int64           `db:"id" json:"id"`
	NodeID       int64           `db:"node_id" json:"node_id"`
	LeaseID      *int64          `db:"lease_id" json:"lease_id"`
	DeploymentID *int64          `db:"deployment_id" json:"deployment_id"`
	Kind         string          `db:"kind" json:"kind"`
	State        string          `db:"state" json:"state"`
	Priority     int             `db:"priority" json:"priority"`
	Attempts     int             `db:"attempts" json:"attempts"`
	MaxAttempts  int             `db:"max_attempts" json:"max_attempts"`
	RunAfter     time.Time       `db:"run_after" json:"run_after"`
	LockedUntil  *time.Time      `db:"locked_until" json:"locked_until"`
	Payload      json.RawMessage `db:"payload" json:"payload"`
	Result       json.RawMessage `db:"result" json:"result"`
	LastError    *string         `db:"last_error" json:"last_error"`
	CreatedAt    time.Time       `db:"created_at" json:"created_at"`
	UpdatedAt    time.Time       `db:"updated_at" json:"updated_at"`
}

// payload/result 为可空 JSON 列, json.RawMessage 不能扫描 NULL: 统一以 JSON 字面量 null 返回。
const taskColumns = `id, node_id, lease_id, deployment_id, kind, state, priority, attempts, max_attempts, run_after,
	locked_until, COALESCE(payload, 'null') AS payload, COALESCE(result, 'null') AS result, last_error, created_at, updated_at`

// ProbePayload probe 任务参数。
type ProbePayload struct {
	Mode    string `json:"mode"`    // full / port_recheck
	Trigger string `json:"trigger"` // supplier / operator / periodic / host_key_confirm / reimage_confirm / resume
	// Generation 入队时节点的 supplier_nodes.generation。停用/恢复/重装确认/改连接信息/确认指纹会把节点代次 +1,
	// 代次不一致的探活在开始前中止、提交时作废(不写报告、不改节点状态), 防止旧探活把节点"复活"。
	Generation int `json:"generation"`
}

// Check 探活单项结论。Required=false 的项不通过不阻断上架(P10)。
type Check struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	OK       bool   `json:"ok"`
	Required bool   `json:"required"`
	Skipped  bool   `json:"skipped,omitempty"`
	Detail   string `json:"detail"`
}
