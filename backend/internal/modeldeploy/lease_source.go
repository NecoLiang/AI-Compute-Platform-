package modeldeploy

import (
	"context"
	"errors"
	"time"
)

// LeaseSource 托管租约与卡的实时数据源, 由 S2 的 internal/fleet 实现并在 main.go 注入
// (Service.SetLeaseSource)。未注入时 deploy-plans 返回 50000, 不使用任何假数据。
//
// 实现方负责: 订单归属(buyerID 必须是订单买家)、订单为托管履约、租约 active;
// 卡数据取自 gpu_slots(lease_id 归属)与最近一次探活/周期采样, 不得用商品声明值代替。
type LeaseSource interface {
	ManagedLeaseSnapshot(ctx context.Context, buyerID int64, orderNo string, modelID int64) (*LeaseSnapshot, error)
}

// LeaseSource 实现返回以下哨兵错误(可用 errors.Is 包装), 其余错误按 50000 处理。
var (
	ErrOrderNotFound  = errors.New("order not found or not owned by buyer") // → 40400
	ErrNotManaged     = errors.New("order is not a managed rental")         // → 40001
	ErrLeaseNotActive = errors.New("managed lease is not active")           // → 40900
)

// LeaseSnapshot 某托管租约在判定时刻的实时状态。
type LeaseSnapshot struct {
	// LeaseID/OrderID 租约主键与其原订单 id(提交部署时写入 model_deployments)。
	LeaseID  int64
	OrderID  int64
	LeaseNo  string
	NodeID   int64
	GPUModel string // gpu_catalog.model_name, 用于取 compute_capability 与匹配验证记录(F1/F8)
	// DriverVersion 节点实测驱动版本(nvidia-smi), 空串表示未知。
	DriverVersion string
	// NVLink 租约内各卡两两经 NVLink/NVSwitch 互联(docs/25 P6 topo 解析)。
	NVLink bool
	Cards  []CardSnapshot
	// ModelCached 节点上该模型(model_id)的缓存为 ready 且校验通过。
	ModelCached bool
	// CacheFreeBytes 模型缓存区当前剩余空间; CacheEvictableBytes 可按 LRU 清理的缓存
	// (不含被运行中或启动中部署引用的条目, docs/27 §27.4.3)。
	CacheFreeBytes      int64
	CacheEvictableBytes int64
	ObservedAt          time.Time
	// Unknown 实现方尚未采集或本次取不到的输入: 键为下方 Input* 常量, 值为面向买家的原因。
	// 列在这里的输入, 快照中对应字段一律不可信, 判定时相应检查项写明"无法判定"并说明原因
	// (F3/F4/F6 为 error 阻断; F5 本身只是提示, 无法判定时同样只提示)。实现方不得用默认值或商品声明值代替。
	Unknown map[string]string
}

// LeaseSnapshot.Unknown 的键。
const (
	InputVRAMUsage  = "vram_usage"  // CardSnapshot.VRAMUsedBytes(F3)
	InputModelCache = "model_cache" // CacheFreeBytes/CacheEvictableBytes(F4; ModelCached=true 时不需要)
	InputNVLink     = "nvlink"      // NVLink(F5)
	InputFatalXid   = "fatal_xid"   // CardSnapshot.FatalXidLastHour(F6)
)

// CardSnapshot 租约内单卡的实时状态。
type CardSnapshot struct {
	UUID           string
	VRAMTotalBytes int64
	VRAMUsedBytes  int64
	// Faulty gpu_slots.state=faulty; FatalXidLastHour 近 1 小时出现致命 Xid(F6)。
	Faulty           bool
	FatalXidLastHour bool
}
