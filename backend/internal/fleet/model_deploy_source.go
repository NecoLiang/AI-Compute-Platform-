package fleet

import (
	"context"
	"fmt"
	"strings"
	"time"

	"tokenfactory/internal/compute"
	"tokenfactory/internal/modeldeploy"
)

// 一键部署(docs/27, S5)可行性判定的托管租约数据源: 实现 modeldeploy.LeaseSource, 由 main 在
// fleet.enabled=true 时注入。只读, 不写库。所有字段都取自平台实测/落库数据:
//
//	订单归属/履约模式  orders(经 compute.OrderForAccess)
//	租约与状态        leases(按原订单 order_id)
//	卡型            products.gpu_model —— 托管商品提审时 P5 已核对节点实测型号与之一致
//	驱动版本         supplier_nodes.driver_version(P4 nvidia-smi 实测, 12h 完整探活刷新)
//	卡与显存总量      gpu_slots(lease_id 归属)的 gpu_uuid / vram_mb(P4 实测)
//	故障标记         gpu_slots.state=faulty
//	NVLink          gpu_slots.topo_group(P6 nvidia-smi topo 分组)——只能确定"没有", 见 leaseNVLink
//	模型已缓存        node_model_cache(state=ready 且已校验)
//
// 尚未采集的输入不给默认值, 列入 LeaseSnapshot.Unknown, 判定侧写明"无法判定":
//   - 各卡当前显存占用: 轻量探活只取 utilization.memory(显存带宽占用率, 且只落节点平均值), 没有按卡 memory.used;
//   - 近 1 小时致命 Xid: 轻量探活只扫最近 2 分钟且不留按卡记录(在租卡命中只写 leases.resource_alert);
//   - 模型缓存区剩余/可清理空间: model_cache_gb 是供给方声明值, 探活尚未核实缓存目录的实际剩余空间;
//   - 卡间逐对 NVLink: 探活只保存分组结果, 同一 nvlink 组内可能有经 PCIe 交换并入的卡。

// ModelDeployLeases 实现 modeldeploy.LeaseSource。
type ModelDeployLeases struct{ s *Service }

var _ modeldeploy.LeaseSource = (*ModelDeployLeases)(nil)

// ModelDeployLeaseSource 返回一键部署用的租约数据源(main: modelDeploySvc.SetLeaseSource)。
func (s *Service) ModelDeployLeaseSource() *ModelDeployLeases { return &ModelDeployLeases{s: s} }

// 未采集输入的原因(面向买家, 也写入判定明细)。
const (
	unknownVRAMUsage  = "节点周期探活尚未按卡采集显存占用(memory.used), 无法确认各卡空闲"
	unknownFatalXid   = "节点周期探活尚未保留近 1 小时的按卡 Xid 记录"
	unknownModelCache = "节点模型缓存区的实际剩余空间尚未经探活采集(model_cache_gb 仅为供给方声明)"
	unknownNVLink     = "探活只保存了卡的拓扑分组, 未保存逐对连接类型, 同组的卡不一定两两 NVLink 直连"
)

func (m *ModelDeployLeases) ManagedLeaseSnapshot(ctx context.Context, buyerID int64, orderNo string, modelID int64) (*modeldeploy.LeaseSnapshot, error) {
	s := m.s
	o, err := s.products.OrderForAccess(orderNo)
	if err != nil {
		return nil, err
	}
	// 他人订单与不存在同样处理, 不泄露订单是否存在。
	if o == nil || o.BuyerID != buyerID {
		return nil, modeldeploy.ErrOrderNotFound
	}
	if o.FulfillmentMode != compute.FulfillmentManaged {
		return nil, modeldeploy.ErrNotManaged
	}
	db := s.repo.db
	l, err := s.repo.getLeaseByOrder(db, o.ID)
	if err != nil {
		return nil, err
	}
	if l == nil {
		return nil, fmt.Errorf("%w: 订单 %s 尚无租约", modeldeploy.ErrLeaseNotActive, orderNo)
	}
	if l.State != LeaseActive {
		return nil, fmt.Errorf("%w: 租约 %s 状态 %s", modeldeploy.ErrLeaseNotActive, l.LeaseNo, l.State)
	}
	slots, err := s.repo.leaseSlots(db, l.ID)
	if err != nil {
		return nil, err
	}
	if len(slots) != l.Cards {
		return nil, fmt.Errorf("租约 %s 卡槽数 %d 与租约卡数 %d 不一致", l.LeaseNo, len(slots), l.Cards)
	}
	node, err := s.repo.getNode(db, l.NodeID)
	if err != nil {
		return nil, err
	}
	if node == nil {
		return nil, fmt.Errorf("租约 %s 的节点 %d 不存在", l.LeaseNo, l.NodeID)
	}
	p, err := s.products.ManagedProduct(l.ProductID)
	if err != nil {
		return nil, err
	}
	if p == nil || strings.TrimSpace(p.GpuModel) == "" {
		return nil, fmt.Errorf("租约 %s 的商品 %d 不存在或未填写 GPU 型号", l.LeaseNo, l.ProductID)
	}

	var cached int
	if err := db.GetContext(ctx, &cached, `SELECT COUNT(*) FROM node_model_cache
		WHERE node_id=? AND model_id=? AND state='ready' AND verified_at IS NOT NULL`, l.NodeID, modelID); err != nil {
		return nil, err
	}

	snap := &modeldeploy.LeaseSnapshot{
		LeaseNo:     l.LeaseNo,
		NodeID:      l.NodeID,
		GPUModel:    p.GpuModel,
		ModelCached: cached > 0,
		ObservedAt:  time.Now(),
		Unknown: map[string]string{
			modeldeploy.InputVRAMUsage: unknownVRAMUsage,
			modeldeploy.InputFatalXid:  unknownFatalXid,
		},
	}
	if node.DriverVersion != nil {
		snap.DriverVersion = strings.TrimSpace(*node.DriverVersion) // 空串 = 未知, 判定侧 F2 写"无法判定"
	}
	if !snap.ModelCached {
		snap.Unknown[modeldeploy.InputModelCache] = unknownModelCache
	}
	nvlink, known := leaseNVLink(slots)
	snap.NVLink = nvlink
	if !known {
		snap.Unknown[modeldeploy.InputNVLink] = unknownNVLink
	}
	snap.Cards = make([]modeldeploy.CardSnapshot, len(slots))
	for i, sl := range slots {
		snap.Cards[i] = modeldeploy.CardSnapshot{
			UUID:           sl.GPUUUID,
			VRAMTotalBytes: int64(sl.VRAMMB) << 20, // nvidia-smi memory.total 单位 MiB
			Faulty:         sl.State == SlotFaulty,
		}
	}
	return snap, nil
}

// leaseNVLink 由 P6 拓扑分组推断租约内各卡是否两两 NVLink 互联。ParseTopo 按 NV#/PIX/PXB 连边求连通分量,
// 组标签取组内最强连接, 因此:
//   - 卡分属不同组 → 组间没有任何 NVLink 边 → 确定"否";
//   - 同组但组类型不是 nvlink → 组内没有任何 NVLink 边 → 确定"否";
//   - 同属一个 nvlink 组 → 组内可能混有经 PCIe 交换并入的卡, 分组结果无法证明两两直连 → 未知。
//
// 单卡租约不涉及卡间并行, 视为已知(否)。
func leaseNVLink(slots []Slot) (nvlink, known bool) {
	if len(slots) <= 1 {
		return false, true
	}
	g := slots[0].TopoGroup
	for _, sl := range slots[1:] {
		if sl.TopoGroup != g {
			return false, true
		}
	}
	if !strings.HasSuffix(g, "-nvlink") {
		return false, true
	}
	return false, false
}
