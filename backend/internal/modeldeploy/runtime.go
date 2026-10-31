package modeldeploy

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"
)

// 节点任务类型(node_tasks.kind, migration 026 已含)。由 internal/fleet 的任务队列认领执行,
// 执行体在本包(Pipeline.RunDeploy/RunPull/RunStop)。
const (
	TaskPullModel   = "pull_model"
	TaskDeployModel = "deploy_model"
	TaskStopModel   = "stop_model"
)

// Shell 节点管理通道(SSH)上的一次连接。由 fleet 的 Runner 适配。
type Shell interface {
	Run(ctx context.Context, cmd string) (ShellResult, error)
	Close() error
}

type ShellResult struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool
}

// LeaseRuntime 部署所在租约的运行期信息(fleet 的 leases/gpu_slots/supplier_nodes 实时读取)。
type LeaseRuntime struct {
	LeaseID   int64
	LeaseNo   string
	State     string // leases.state; 只有 active 才能启动引擎
	NodeID    int64
	Container string // 租约容器名 omnis-<lease_no>
	Network   string // 租约网络 omnis-<lease_no>-net
	// SlotUUIDs gpu_slots.lease_id = 本租约的卡; FaultyUUIDs 其中 state=faulty 的卡。
	SlotUUIDs   []string
	FaultyUUIDs []string
	// CPUs/MemoryGB 租约容器的完整配额(与开通时 docker run --cpus/--memory 同一算法)。
	CPUs     int
	MemoryGB int
}

// CardHealth L5 硬件遥测(与 fleet 周期探活共用 gpu_slots/gpu_xid_events, 不另起采集)。
type CardHealth struct {
	UUID  string
	TempC *int
	// Throttled 温度/功耗降频(HW slowdown/thermal/power brake; 满载时的 SW power cap 属常态不计), nil=未采集。
	Throttled      *bool
	ECCUncorrected *int64
	ECCCorrected   *int64
	FatalXid       bool // 近 1 小时该卡有致命 Xid
	TelemetryAt    *time.Time
}

// TaskSpec 入队一条节点任务。
type TaskSpec struct {
	NodeID       int64
	LeaseID      *int64
	DeploymentID *int64
	Kind         string
	MaxAttempts  int // 0 = 表默认
	Payload      any
}

// RuntimeSettings 平台配置(fleet.*)。
type RuntimeSettings struct {
	CacheDir    string // fleet.model_cache_dir
	PullImage   string // fleet.model_pull_image(ref@sha256); 空 = 未配置, 提交部署 50000
	PortStart   int    // fleet.engine_port_range
	PortEnd     int
	GatewayHost string // accessgw.public_host, 仅用于给买家展示端口转发命令
	GatewayPort int
}

// Runtime 由 internal/fleet 实现(本包不能依赖 fleet: fleet 已依赖本包)。
type Runtime interface {
	Dial(ctx context.Context, nodeID int64) (Shell, error)
	LeaseRuntime(ctx context.Context, leaseID int64) (*LeaseRuntime, error)
	// Enqueue 在调用方事务内插入 node_tasks(与部署状态迁移同事务, 不会丢任务)。
	Enqueue(ctx context.Context, tx *sqlx.Tx, spec TaskSpec) error
	CardHealth(ctx context.Context, nodeID int64, uuids []string) ([]CardHealth, error)
	// ListeningPorts 解析节点 `ss -Htln` 输出。
	ListeningPorts(ssOutput string) map[int]bool
	Settings() RuntimeSettings
}

// Notifier 站内通知(notification.Service 实现)。
type Notifier interface {
	Record(userID int64, notifType, title, content, link string) error
}

// AbortError 任务不可重试的失败(fleet 映射为 errTaskAborted: 直接 failed, 不再重试)。
type AbortError struct{ Msg string }

func (e *AbortError) Error() string { return e.Msg }

func abortf(format string, args ...any) error {
	return &AbortError{Msg: fmt.Sprintf(format, args...)}
}
