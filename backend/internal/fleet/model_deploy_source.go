package fleet

import (
	"context"
	"encoding/json"
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
//	各卡显存占用(F3)  gpu_slots.vram_used_mb / telemetry_at(轻探活 60s, migration 031)
//	致命 Xid(F6)     gpu_xid_events + supplier_nodes.xid_covered_from / xid_scan_ok_at(轻探活自上次扫描起)
//	NVLink(F5)      gpu_slots.topo_links / topo_at(完整探活 P6 两两矩阵); 无矩阵时退回 topo_group 只判"没有"
//	模型已缓存        node_model_cache(state=ready 且已校验)
//	缓存区空间(F4)    supplier_nodes.cache_free_bytes / cache_observed_at(完整探活 df) + node_model_cache 可清理条目
//
// 新鲜度(超过即视为未知, 列入 LeaseSnapshot.Unknown, 判定侧写"无法判定", 不用旧值):
//   - 轻探活数据(显存占用、Xid 扫描): 3 个轻探活周期(snapshotLightFresh = 3 × 60s);
//   - 完整探活数据(拓扑、缓存容量): 完整探活周期 + 1 小时(snapshotFullFresh = 13h)。
//
// 时间均由平台(应用)时钟写入(telemetry_repo.go), 这里同样用应用时钟比较。

// ModelDeployLeases 实现 modeldeploy.LeaseSource。
type ModelDeployLeases struct{ s *Service }

var _ modeldeploy.LeaseSource = (*ModelDeployLeases)(nil)

// ModelDeployLeaseSource 返回一键部署用的租约数据源(main: modelDeploySvc.SetLeaseSource)。
func (s *Service) ModelDeployLeaseSource() *ModelDeployLeases { return &ModelDeployLeases{s: s} }

const (
	// snapshotLightFresh 轻探活数据的新鲜度上限: 连续 3 个周期未更新即未知(节点离线判定为 90s, 此时也必然过期)。
	snapshotLightFresh = 3 * lightInterval
	// snapshotFullFresh 完整探活数据的新鲜度上限: 12h 周期 + 1h 排队/执行余量。
	snapshotFullFresh = fullEvery + time.Hour
	// xidLookback F6 "近 1 小时无致命 Xid"。
	xidLookback = time.Hour
)

// 输入未知的原因(面向买家, 也写入判定明细)。
const (
	unknownVRAMUsage      = "节点逐卡显存占用尚未采集(等待下一次周期探活)"
	unknownVRAMStale      = "节点逐卡显存占用超过 3 分钟未更新(节点可能失联), 无法确认各卡空闲"
	unknownFatalXid       = "节点内核日志 Xid 扫描尚未覆盖完整的近 1 小时(新接入、刚恢复或扫描中断后需累计 1 小时)"
	unknownFatalXidStale  = "节点内核日志 Xid 扫描超过 3 分钟未成功, 无法确认近 1 小时无致命 Xid"
	unknownModelCache     = "节点模型缓存区的实际剩余空间尚未经探活采集(model_cache_gb 仅为供给方声明)"
	unknownModelCacheOld  = "节点模型缓存区容量的探活数据已超过 13 小时, 等待下一次完整探活"
	unknownModelCacheRows = "节点有正在下载的模型缓存条目缺少目录信息, 无法估算其占用"
	unknownNVLink         = "卡间逐对连接类型尚未经完整探活采集(或已超过 13 小时), 同组的卡不一定两两 NVLink 直连"
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

	now := time.Now()
	snap := &modeldeploy.LeaseSnapshot{
		LeaseNo:     l.LeaseNo,
		NodeID:      l.NodeID,
		GPUModel:    p.GpuModel,
		ModelCached: cached > 0,
		ObservedAt:  now,
		Unknown:     map[string]string{},
	}
	if node.DriverVersion != nil {
		snap.DriverVersion = strings.TrimSpace(*node.DriverVersion) // 空串 = 未知, 判定侧 F2 写"无法判定"
	}
	tel, err := m.slotTelemetry(ctx, l.ID)
	if err != nil {
		return nil, err
	}
	snap.Cards = make([]modeldeploy.CardSnapshot, len(slots))
	for i, sl := range slots {
		snap.Cards[i] = modeldeploy.CardSnapshot{
			UUID:           sl.GPUUUID,
			VRAMTotalBytes: int64(sl.VRAMMB) << 20, // nvidia-smi memory.total 单位 MiB
			Faulty:         sl.State == SlotFaulty,
		}
	}
	if why := fillVRAMUsage(snap.Cards, slots, tel, now); why != "" {
		snap.Unknown[modeldeploy.InputVRAMUsage] = why
	}
	if why, err := m.fillFatalXid(ctx, snap.Cards, slots, l.NodeID, now); err != nil {
		return nil, err
	} else if why != "" {
		snap.Unknown[modeldeploy.InputFatalXid] = why
	}
	nvlink, known := leaseNVLinkPairwise(slots, tel, now)
	if !known {
		nvlink, known = leaseNVLink(slots)
	}
	snap.NVLink = nvlink
	if !known {
		snap.Unknown[modeldeploy.InputNVLink] = unknownNVLink
	}
	why, err := m.fillCacheSpace(ctx, snap, l.NodeID, modelID, now)
	if err != nil {
		return nil, err
	}
	if why != "" && !snap.ModelCached { // 已缓存时 F4 不需要容量数据
		snap.Unknown[modeldeploy.InputModelCache] = why
	}
	return snap, nil
}

// slotObs 卡槽遥测列(migration 031)。
type slotObs struct {
	ID          int64      `db:"id"`
	VRAMUsedMB  *int64     `db:"vram_used_mb"`
	TelemetryAt *time.Time `db:"telemetry_at"`
	TopoLinks   *string    `db:"topo_links"`
	TopoAt      *time.Time `db:"topo_at"`
}

func (m *ModelDeployLeases) slotTelemetry(ctx context.Context, leaseID int64) (map[int64]slotObs, error) {
	var list []slotObs
	if err := m.s.repo.db.SelectContext(ctx, &list, `SELECT id, vram_used_mb, telemetry_at, topo_links, topo_at
		FROM gpu_slots WHERE lease_id=?`, leaseID); err != nil {
		return nil, err
	}
	out := make(map[int64]slotObs, len(list))
	for _, t := range list {
		out[t.ID] = t
	}
	return out, nil
}

// fresh t 非空且距 now 不超过 d。
func fresh(t *time.Time, now time.Time, d time.Duration) bool {
	return t != nil && !t.Before(now.Add(-d))
}

// fillVRAMUsage F3: 每张卡都有新鲜的 memory.used 才算已知, 否则返回原因(不填部分卡)。
func fillVRAMUsage(cards []modeldeploy.CardSnapshot, slots []Slot, tel map[int64]slotObs, now time.Time) string {
	used := make([]int64, len(slots))
	for i, sl := range slots {
		t := tel[sl.ID]
		if t.TelemetryAt == nil || t.VRAMUsedMB == nil {
			return unknownVRAMUsage
		}
		if !fresh(t.TelemetryAt, now, snapshotLightFresh) {
			return unknownVRAMStale
		}
		used[i] = *t.VRAMUsedMB << 20
	}
	for i := range cards {
		cards[i].VRAMUsedBytes = used[i]
	}
	return ""
}

// fillFatalXid F6: 节点 Xid 扫描新鲜且连续覆盖近 1 小时才算已知; 已知时按卡的 PCI 地址匹配近 1 小时的致命 Xid 事件。
// 覆盖区间的终点是最近一次成功扫描(≤3 分钟前), 其后至今的几分钟由下一次轻探活补上。
func (m *ModelDeployLeases) fillFatalXid(ctx context.Context, cards []modeldeploy.CardSnapshot, slots []Slot, nodeID int64, now time.Time) (string, error) {
	db := m.s.repo.db
	var cov struct {
		OK   *time.Time `db:"xid_scan_ok_at"`
		From *time.Time `db:"xid_covered_from"`
		Note *string    `db:"xid_scan_note"`
	}
	if err := db.GetContext(ctx, &cov, `SELECT xid_scan_ok_at, xid_covered_from, xid_scan_note FROM supplier_nodes WHERE id=?`, nodeID); err != nil {
		return "", err
	}
	if cov.From == nil || cov.OK == nil {
		if cov.Note != nil && *cov.Note != "" {
			return "节点内核日志 Xid 扫描不可用: " + *cov.Note, nil
		}
		return unknownFatalXid, nil
	}
	if !fresh(cov.OK, now, snapshotLightFresh) {
		return unknownFatalXidStale, nil
	}
	since := now.Add(-xidLookback)
	if cov.From.After(since) {
		return unknownFatalXid, nil
	}
	for _, sl := range slots {
		if NormalizeBusID(sl.PCIBusID) == "" {
			return fmt.Sprintf("GPU%d 缺少 PCI 地址, 无法匹配内核日志中的 Xid", sl.GPUIndex), nil
		}
	}
	var events []struct {
		Bus string `db:"pci_bus_id"`
		Xid int    `db:"xid"`
	}
	if err := db.SelectContext(ctx, &events, `SELECT pci_bus_id, xid FROM gpu_xid_events
		WHERE node_id=? AND occurred_at >= ?`, nodeID, since); err != nil {
		return "", err
	}
	fatal := map[string]bool{}
	for _, e := range events {
		if FatalXids[e.Xid] {
			fatal[NormalizeBusID(e.Bus)] = true
		}
	}
	for i, sl := range slots {
		cards[i].FatalXidLastHour = fatal[NormalizeBusID(sl.PCIBusID)]
	}
	return "", nil
}

// leaseNVLinkPairwise 由完整探活保存的两两拓扑矩阵判定租约内各卡是否两两 NVLink(NV#)直连。
// 任一卡缺矩阵或矩阵过期 → 未知; 单卡视为已知(否)。
func leaseNVLinkPairwise(slots []Slot, tel map[int64]slotObs, now time.Time) (nvlink, known bool) {
	if len(slots) <= 1 {
		return false, true
	}
	rows := make([][]string, len(slots))
	for i, sl := range slots {
		t := tel[sl.ID]
		if t.TopoLinks == nil || !fresh(t.TopoAt, now, snapshotFullFresh) {
			return false, false
		}
		if err := json.Unmarshal([]byte(*t.TopoLinks), &rows[i]); err != nil {
			return false, false
		}
	}
	all := true
	for i := range slots {
		for _, b := range slots[i+1:] {
			if b.GPUIndex < 0 || b.GPUIndex >= len(rows[i]) {
				return false, false
			}
			if !strings.HasPrefix(rows[i][b.GPUIndex], "NV") {
				all = false
			}
		}
	}
	return all, true
}

// fillCacheSpace F4 容量: 完整探活 df 的剩余空间, 扣除采集后才占用的空间, 加上可按 LRU 清理的条目。
//   - 剩余 = df 可用 − 正在下载条目的完整权重 − 采集之后才校验完成的条目权重(两者都按最坏情况扣, df 可能已含部分字节);
//   - 可清理 = 已校验 ready 条目中, 没有被该节点未终止部署(停止/失败以外的状态)引用的权重之和(docs/27 §27.4.3)。
//
// 返回非空表示未知及原因。
func (m *ModelDeployLeases) fillCacheSpace(ctx context.Context, snap *modeldeploy.LeaseSnapshot, nodeID, modelID int64, now time.Time) (string, error) {
	db := m.s.repo.db
	var df struct {
		Free *int64     `db:"cache_free_bytes"`
		At   *time.Time `db:"cache_observed_at"`
		Note *string    `db:"cache_note"`
	}
	if err := db.GetContext(ctx, &df, `SELECT cache_free_bytes, cache_observed_at, cache_note FROM supplier_nodes WHERE id=?`, nodeID); err != nil {
		return "", err
	}
	if df.Free == nil || df.At == nil {
		if df.Note != nil && *df.Note != "" {
			return "节点模型缓存区容量未能采集: " + *df.Note, nil
		}
		return unknownModelCache, nil
	}
	if !fresh(df.At, now, snapshotFullFresh) {
		return unknownModelCacheOld, nil
	}
	var rows []struct {
		ModelID  int64      `db:"model_id"`
		State    string     `db:"state"`
		Verified *time.Time `db:"verified_at"`
		Weights  *int64     `db:"weights_bytes"`
		InUse    bool       `db:"in_use"`
	}
	if err := db.SelectContext(ctx, &rows, `SELECT c.model_id, c.state, c.verified_at, mc.weights_bytes,
		EXISTS(SELECT 1 FROM model_deployments d WHERE d.node_id=c.node_id AND d.model_id=c.model_id
		       AND d.state NOT IN ('stopped','failed')) AS in_use
		FROM node_model_cache c LEFT JOIN model_catalog mc ON mc.id=c.model_id WHERE c.node_id=?`, nodeID); err != nil {
		return "", err
	}
	free, evictable := *df.Free, int64(0)
	for _, r := range rows {
		switch {
		case r.State == "downloading":
			if r.Weights == nil {
				return unknownModelCacheRows, nil
			}
			free -= *r.Weights
		case r.State == "ready" && r.Verified != nil && r.Weights != nil:
			if r.Verified.After(*df.At) {
				free -= *r.Weights
			}
			if !r.InUse && r.ModelID != modelID {
				evictable += *r.Weights
			}
		}
	}
	if free < 0 {
		free = 0
	}
	snap.CacheFreeBytes, snap.CacheEvictableBytes = free, evictable
	return "", nil
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
