package fleet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"tokenfactory/internal/catalog"
	"tokenfactory/internal/modeldeploy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLeaseNVLinkFromTopoGroups(t *testing.T) {
	sl := func(groups ...string) []Slot {
		out := make([]Slot, len(groups))
		for i, g := range groups {
			out[i] = Slot{TopoGroup: g}
		}
		return out
	}
	for _, c := range []struct {
		name          string
		slots         []Slot
		nvlink, known bool
	}{
		{"单卡", sl("g0-nvlink"), false, true},
		{"跨组", sl("g0-nvlink", "g1-nvlink"), false, true},
		{"同 pcie 组", sl("g0-pcie", "g0-pcie"), false, true},
		{"同 numa 组", sl("g1-numa", "g1-numa"), false, true},
		{"同 nvlink 组无法证明两两直连", sl("g0-nvlink", "g0-nvlink"), false, false},
	} {
		nv, known := leaseNVLink(c.slots)
		assert.Equal(t, c.nvlink, nv, c.name)
		assert.Equal(t, c.known, known, c.name)
	}
}

// 一键部署租约数据源: 真实开通流水线把租约推到 active 后取快照, 再经 modeldeploy 服务端到端判定。
func TestModelDeployLeaseSnapshot(t *testing.T) {
	e := newProvEnv(t, true)
	src := e.s.ModelDeployLeaseSource()
	ctx := context.Background()
	const buyer = int64(21)
	no := e.order.OrderNo

	// 开通前租约未 active。
	_, err := src.ManagedLeaseSnapshot(ctx, buyer, no, 1)
	assert.True(t, errors.Is(err, modeldeploy.ErrLeaseNotActive), "%v", err)

	e.outcome(t, runOne(t, e.s))
	e.db.MustExec(`UPDATE supplier_nodes SET driver_version='550.54.15' WHERE id=?`, e.node)

	// 归属与履约模式。
	_, err = src.ManagedLeaseSnapshot(ctx, 99, no, 1)
	assert.True(t, errors.Is(err, modeldeploy.ErrOrderNotFound), "%v", err)
	_, err = src.ManagedLeaseSnapshot(ctx, buyer, "ORDNOTEXIST01", 1)
	assert.True(t, errors.Is(err, modeldeploy.ErrOrderNotFound), "%v", err)
	e.db.MustExec(`UPDATE orders SET fulfillment_mode='manual' WHERE id=?`, e.order.ID)
	_, err = src.ManagedLeaseSnapshot(ctx, buyer, no, 1)
	assert.True(t, errors.Is(err, modeldeploy.ErrNotManaged), "%v", err)
	e.db.MustExec(`UPDATE orders SET fulfillment_mode='managed' WHERE id=?`, e.order.ID)

	snap, err := src.ManagedLeaseSnapshot(ctx, buyer, no, 1)
	require.NoError(t, err)
	assert.Equal(t, e.lease.LeaseNo, snap.LeaseNo)
	assert.Equal(t, e.node, snap.NodeID)
	assert.Equal(t, "H100-80G", snap.GPUModel)
	assert.Equal(t, "550.54.15", snap.DriverVersion)
	require.Len(t, snap.Cards, 2)
	assert.Equal(t, fakeUUID0, snap.Cards[0].UUID)
	assert.Equal(t, int64(81920)<<20, snap.Cards[0].VRAMTotalBytes)
	assert.False(t, snap.Cards[0].Faulty)
	// 播种拓扑组 "g0"(非 nvlink 组)且无两两矩阵 → 退回分组判定: 确定无 NVLink; 尚无探活遥测的输入全部声明为未知。
	assert.False(t, snap.NVLink)
	assert.False(t, snap.ModelCached)
	assert.Contains(t, snap.Unknown, modeldeploy.InputVRAMUsage)
	assert.Contains(t, snap.Unknown, modeldeploy.InputFatalXid)
	assert.Contains(t, snap.Unknown, modeldeploy.InputModelCache)
	assert.NotContains(t, snap.Unknown, modeldeploy.InputNVLink)

	// 同一 nvlink 组且无两两矩阵 → NVLink 未知; 卡故障如实反映; 节点已校验缓存该模型 → 不再声明缓存空间未知。
	e.db.MustExec(`UPDATE gpu_slots SET topo_group='g0-nvlink' WHERE node_id=?`, e.node)
	e.db.MustExec(`UPDATE gpu_slots SET state='faulty' WHERE node_id=? AND gpu_index=1`, e.node)
	e.db.MustExec(`INSERT INTO node_model_cache (node_id, model_id, state, bytes_done, verified_at) VALUES (?, 7, 'ready', 1, NOW())`, e.node)
	snap, err = src.ManagedLeaseSnapshot(ctx, buyer, no, 7)
	require.NoError(t, err)
	assert.Contains(t, snap.Unknown, modeldeploy.InputNVLink)
	assert.True(t, snap.ModelCached)
	assert.NotContains(t, snap.Unknown, modeldeploy.InputModelCache)
	assert.True(t, snap.Cards[1].Faulty)
	// 缓存行未校验(verified_at NULL)不算已缓存。
	snap, err = src.ManagedLeaseSnapshot(ctx, buyer, no, 1)
	require.NoError(t, err)
	assert.False(t, snap.ModelCached)
	e.db.MustExec(`UPDATE gpu_slots SET state='leased' WHERE node_id=? AND gpu_index=1`, e.node)

	// 端到端: 与 main 同样接线, deploy-plans 对未采集项写"无法判定"并阻断, 不给出可行方案。
	e.db.MustExec(`UPDATE gpu_catalog SET compute_capability='9.0' WHERE model_name='H100-80G'`)
	res := e.db.MustExec(`INSERT INTO model_catalog (model_key, display_name, repo, revision, manifest, weights_bytes, quant,
		min_compute_capability, valid_tp, kv_bytes_per_token, runtime_overhead_bytes, max_model_len, min_context_tokens,
		engine_image, min_driver_version, engine_args, metrics_map, license, commercial_use, status, created_by)
		VALUES ('qwen-test-7b', 'Qwen 测试 7B', 'Qwen/Qwen2.5-7B-Instruct', 'abc123', '[]', 16000000000, 'bf16',
		'8.0', '1,2', 57344, 3000000000, 32768, 8192,
		'vllm/vllm-openai@sha256:0000000000000000000000000000000000000000000000000000000000000000', '535.104.05',
		'{}', '{}', 'apache-2.0', 'allowed', 'active', 1)`)
	modelID, err := res.LastInsertId()
	require.NoError(t, err)
	md := modeldeploy.NewService(modeldeploy.NewRepository(e.db), catalog.NewService(catalog.NewRepository(e.db)))
	md.SetLeaseSource(src)
	out, err := md.DeployPlans(ctx, modeldeploy.PlanRequest{BuyerID: buyer, OrderNo: no, ModelID: modelID, GPUMemUtilPct: 90})
	require.NoError(t, err)
	assert.False(t, out.Feasible)
	assert.Empty(t, out.Plans)
	checks := map[string]modeldeploy.Check{}
	for _, c := range out.Checks {
		checks[c.ID] = c
	}
	assert.True(t, checks["M1"].OK, "显存计算基于实测显存总量: %+v", checks["M1"])
	assert.True(t, checks["F1"].OK, "%+v", checks["F1"])
	assert.True(t, checks["F2"].OK, "%+v", checks["F2"])
	for _, id := range []string{"F3", "F4", "F6"} {
		assert.False(t, checks[id].OK, id)
		assert.Equal(t, modeldeploy.LevelError, checks[id].Level, id)
		assert.True(t, strings.Contains(checks[id].Message, "无法判定"), "%s: %s", id, checks[id].Message)
	}
	assert.Equal(t, modeldeploy.LevelWarning, checks["F5"].Level)
	assert.Contains(t, checks["F5"].Message, "无法判定")
	assert.Contains(t, out.Basis.Undetermined, modeldeploy.InputVRAMUsage)

	// 租约不再 active → 40900 哨兵。
	e.db.MustExec(`UPDATE leases SET state='suspended' WHERE id=?`, e.lease.ID)
	_, err = md.DeployPlans(ctx, modeldeploy.PlanRequest{BuyerID: buyer, OrderNo: no, ModelID: modelID, GPUMemUtilPct: 90})
	var be *modeldeploy.Error
	require.True(t, errors.As(err, &be), "%v", err)
	assert.Equal(t, 40900, be.Code)
}

func TestLeaseNVLinkPairwise(t *testing.T) {
	now := time.Now()
	old := now.Add(-14 * time.Hour)
	links := func(s string) *string { return &s }
	slots := []Slot{{ID: 1, GPUIndex: 0}, {ID: 2, GPUIndex: 1}, {ID: 3, GPUIndex: 3}}
	all := map[int64]slotObs{
		1: {TopoLinks: links(`["X","NV18","NV18","NV18"]`), TopoAt: &now},
		2: {TopoLinks: links(`["NV18","X","NV18","NV18"]`), TopoAt: &now},
		3: {TopoLinks: links(`["NV18","NV18","NV18","X"]`), TopoAt: &now},
	}
	nv, known := leaseNVLinkPairwise(slots, all, now)
	assert.True(t, known)
	assert.True(t, nv)

	mixed := map[int64]slotObs{1: all[1], 2: {TopoLinks: links(`["NV18","X","SYS","PIX"]`), TopoAt: &now}, 3: all[3]}
	nv, known = leaseNVLinkPairwise(slots, mixed, now)
	assert.True(t, known)
	assert.False(t, nv, "GPU1-GPU3 经 PCIe")

	stale := map[int64]slotObs{1: all[1], 2: {TopoLinks: all[2].TopoLinks, TopoAt: &old}, 3: all[3]}
	_, known = leaseNVLinkPairwise(slots, stale, now)
	assert.False(t, known, "矩阵超过 13h 视为未知")
	_, known = leaseNVLinkPairwise(slots, map[int64]slotObs{1: all[1], 3: all[3]}, now)
	assert.False(t, known, "有卡缺矩阵")
	short := map[int64]slotObs{1: {TopoLinks: links(`["X","NV18"]`), TopoAt: &now}, 2: all[2], 3: all[3]}
	_, known = leaseNVLinkPairwise(slots, short, now)
	assert.False(t, known, "矩阵列数不足")
	nv, known = leaseNVLinkPairwise(slots[:1], nil, now)
	assert.True(t, known)
	assert.False(t, nv)
}

func insertTestModel(t *testing.T, db *sqlx.DB, key string, weights int64) int64 {
	t.Helper()
	res := db.MustExec(`INSERT INTO model_catalog (model_key, display_name, repo, revision, manifest, weights_bytes, quant,
		min_compute_capability, valid_tp, kv_bytes_per_token, runtime_overhead_bytes, max_model_len, min_context_tokens,
		engine_image, min_driver_version, engine_args, metrics_map, license, commercial_use, status, created_by)
		VALUES (?, ?, 'Qwen/Qwen2.5-7B-Instruct', 'abc123', '[]', ?, 'bf16',
		'8.0', '1,2', 57344, 3000000000, 32768, 8192,
		'vllm/vllm-openai@sha256:0000000000000000000000000000000000000000000000000000000000000000', '535.104.05',
		'{}', '{}', 'apache-2.0', 'allowed', 'active', 1)`, key, key, weights)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

// 探活真实采集链路(轻探活遥测+Xid、完整探活的拓扑与缓存写入函数)喂给快照: 数据新鲜时 F3–F6 可判定并可行;
// 过期/覆盖不足即退回"无法判定"; 致命 Xid 按卡命中。
func TestModelDeployLeaseSnapshotFromProbeData(t *testing.T) {
	e := newProvEnv(t, true)
	e.outcome(t, runOne(t, e.s))
	ctx := context.Background()
	const buyer = int64(21)
	no := e.order.OrderNo
	src := e.s.ModelDeployLeaseSource()
	e.db.MustExec(`UPDATE supplier_nodes SET driver_version='550.54.15' WHERE id=?`, e.node)
	e.db.MustExec(`UPDATE gpu_catalog SET compute_capability='9.0' WHERE model_name='H100-80G'`)
	modelID := insertTestModel(t, e.db, "qwen-test-7b", 16000000000)
	md := modeldeploy.NewService(modeldeploy.NewRepository(e.db), catalog.NewService(catalog.NewRepository(e.db)))
	md.SetLeaseSource(src)
	plans := func() (*modeldeploy.Result, map[string]modeldeploy.Check) {
		out, err := md.DeployPlans(ctx, modeldeploy.PlanRequest{BuyerID: buyer, OrderNo: no, ModelID: modelID, GPUMemUtilPct: 90})
		require.NoError(t, err)
		checks := map[string]modeldeploy.Check{}
		for _, c := range out.Checks {
			checks[c.ID] = c
		}
		return out, checks
	}

	// 轻探活(首扫 61 分钟, 节点日志自 48h 前)+ 完整探活的拓扑/缓存写入。
	n, err := e.s.repo.GetNode(e.node)
	require.NoError(t, err)
	e.s.lightProbe(ctx, n)
	matrix, err := ParseTopoMatrix(fakeTopo, 2)
	require.NoError(t, err)
	gpus := []GPU{{Index: 0, UUID: fakeUUID0}, {Index: 1, UUID: fakeUUID1}}
	require.NoError(t, e.s.recordTopo(e.node, n.Generation, gpus, matrix, time.Now()))
	require.NoError(t, e.s.recordCacheDF(e.node, n.Generation, CacheDF{AvailBytes: 3298534883328, TotalBytes: 3840755982336}, "", time.Now()))

	snap, err := src.ManagedLeaseSnapshot(ctx, buyer, no, modelID)
	require.NoError(t, err)
	assert.Empty(t, snap.Unknown)
	assert.Equal(t, int64(1)<<20, snap.Cards[0].VRAMUsedBytes)
	assert.Equal(t, int64(1)<<20, snap.Cards[1].VRAMUsedBytes)
	assert.False(t, snap.Cards[0].FatalXidLastHour)
	assert.True(t, snap.NVLink, "fakeTopo 两卡 NV18")
	assert.Equal(t, int64(3298534883328), snap.CacheFreeBytes)
	assert.Zero(t, snap.CacheEvictableBytes)

	out, checks := plans()
	for _, id := range []string{"F3", "F4", "F5", "F6"} {
		assert.True(t, checks[id].OK, "%s: %+v", id, checks[id])
	}
	assert.True(t, out.Feasible, "%+v", out.Checks)
	assert.NotEmpty(t, out.Plans)
	assert.Empty(t, out.Basis.Undetermined)

	// 缓存账目: 下载中条目按完整权重预扣; 未被未终止部署引用的已校验条目计入可清理; 被运行中部署引用的不计。
	other := insertTestModel(t, e.db, "m-evictable", 100<<30)
	busy := insertTestModel(t, e.db, "m-running", 60<<30)
	dl := insertTestModel(t, e.db, "m-downloading", 50<<30)
	e.db.MustExec(`INSERT INTO node_model_cache (node_id, model_id, state, bytes_done, verified_at) VALUES
		(?, ?, 'ready', 0, NOW() - INTERVAL 1 DAY), (?, ?, 'ready', 0, NOW() - INTERVAL 1 DAY), (?, ?, 'downloading', 0, NULL)`,
		e.node, other, e.node, busy, e.node, dl)
	e.db.MustExec(`INSERT INTO model_deployments (deploy_no, lease_id, order_id, buyer_id, node_id, model_id, gpu_uuids, tp, dp,
		max_model_len, gpu_mem_util, state, feasibility) VALUES ('d-test-1', ?, ?, ?, ?, ?, '[]', 1, 1, 8192, 0.90, 'running', '{}')`,
		e.lease.ID, e.order.ID, buyer, e.node, busy)
	snap, err = src.ManagedLeaseSnapshot(ctx, buyer, no, modelID)
	require.NoError(t, err)
	assert.Equal(t, int64(3298534883328)-50<<30, snap.CacheFreeBytes)
	assert.Equal(t, int64(100)<<30, snap.CacheEvictableBytes)
	e.db.MustExec(`UPDATE model_deployments SET state='stopped' WHERE deploy_no='d-test-1'`)
	snap, err = src.ManagedLeaseSnapshot(ctx, buyer, no, modelID)
	require.NoError(t, err)
	assert.Equal(t, int64(160)<<30, snap.CacheEvictableBytes, "停止的部署不再占用缓存")
	// 采集之后才校验完成的条目, df 里可能还没扣: 按完整权重预扣。
	e.db.MustExec(`UPDATE node_model_cache SET verified_at=NOW() + INTERVAL 1 MINUTE WHERE node_id=? AND model_id=?`, e.node, other)
	snap, err = src.ManagedLeaseSnapshot(ctx, buyer, no, modelID)
	require.NoError(t, err)
	assert.Equal(t, int64(3298534883328)-50<<30-100<<30, snap.CacheFreeBytes)
	e.db.MustExec(`DELETE FROM node_model_cache WHERE node_id=?`, e.node)
	e.db.MustExec(`DELETE FROM model_deployments WHERE deploy_no='d-test-1'`)

	// 致命 Xid 79 打在 GPU1(播种总线 0000:01:00.0): 该卡 FatalXidLastHour, F6 阻断。
	e.fake.SetXidLines(fakeXidLine(time.Now().Add(-30*time.Second), "0000:01:00", 79))
	e.s.lightProbe(ctx, n)
	e.fake.SetXidLines()
	snap, err = src.ManagedLeaseSnapshot(ctx, buyer, no, modelID)
	require.NoError(t, err)
	assert.False(t, snap.Cards[0].FatalXidLastHour)
	assert.True(t, snap.Cards[1].FatalXidLastHour)
	_, checks = plans()
	assert.False(t, checks["F6"].OK)
	assert.Contains(t, checks["F6"].Message, "致命 Xid")
	// 事件超过 1 小时 → 不再计入。
	e.db.MustExec(`UPDATE gpu_xid_events SET occurred_at=NOW() - INTERVAL 61 MINUTE WHERE node_id=?`, e.node)
	snap, err = src.ManagedLeaseSnapshot(ctx, buyer, no, modelID)
	require.NoError(t, err)
	assert.False(t, snap.Cards[1].FatalXidLastHour)

	// 过期规则。
	e.db.MustExec(`UPDATE gpu_slots SET telemetry_at=? WHERE node_id=? AND gpu_index=1`, time.Now().Add(-4*time.Minute), e.node)
	e.db.MustExec(`UPDATE supplier_nodes SET cache_observed_at=? WHERE id=?`, time.Now().Add(-14*time.Hour), e.node)
	e.db.MustExec(`UPDATE gpu_slots SET topo_at=?, topo_group='g0-nvlink' WHERE node_id=? AND gpu_index=0`, time.Now().Add(-14*time.Hour), e.node)
	e.db.MustExec(`UPDATE gpu_slots SET topo_group='g0-nvlink' WHERE node_id=?`, e.node)
	e.db.MustExec(`UPDATE supplier_nodes SET xid_scan_ok_at=? WHERE id=?`, time.Now().Add(-4*time.Minute), e.node)
	snap, err = src.ManagedLeaseSnapshot(ctx, buyer, no, modelID)
	require.NoError(t, err)
	assert.Equal(t, unknownVRAMStale, snap.Unknown[modeldeploy.InputVRAMUsage])
	assert.Zero(t, snap.Cards[0].VRAMUsedBytes, "部分卡新鲜也不填, 整体未知")
	assert.Equal(t, unknownFatalXidStale, snap.Unknown[modeldeploy.InputFatalXid])
	assert.Equal(t, unknownModelCacheOld, snap.Unknown[modeldeploy.InputModelCache])
	assert.Equal(t, unknownNVLink, snap.Unknown[modeldeploy.InputNVLink])
	out, checks = plans()
	assert.False(t, out.Feasible)
	for _, id := range []string{"F3", "F4", "F6"} {
		assert.Contains(t, checks[id].Message, "无法判定", id)
	}
	// 拓扑过期但分组能确定"没有": 退回分组判定。
	e.db.MustExec(`UPDATE gpu_slots SET topo_group=CONCAT('g', gpu_index) WHERE node_id=?`, e.node)
	snap, err = src.ManagedLeaseSnapshot(ctx, buyer, no, modelID)
	require.NoError(t, err)
	assert.NotContains(t, snap.Unknown, modeldeploy.InputNVLink)
	assert.False(t, snap.NVLink)

	// Xid 覆盖不足 1 小时 / 扫描中断(带原因); 缓存目录不存在(带原因); 已缓存时不需要容量。
	e.db.MustExec(`UPDATE supplier_nodes SET xid_scan_ok_at=NOW(), xid_covered_from=NOW() - INTERVAL 30 MINUTE WHERE id=?`, e.node)
	snap, err = src.ManagedLeaseSnapshot(ctx, buyer, no, modelID)
	require.NoError(t, err)
	assert.Equal(t, unknownFatalXid, snap.Unknown[modeldeploy.InputFatalXid])
	e.db.MustExec(`UPDATE supplier_nodes SET xid_covered_from=NULL, xid_scan_note='管理账户无权读取内核日志' WHERE id=?`, e.node)
	require.NoError(t, e.s.recordCacheDF(e.node, n.Generation, CacheDF{Missing: true}, "", time.Now()))
	snap, err = src.ManagedLeaseSnapshot(ctx, buyer, no, modelID)
	require.NoError(t, err)
	assert.Contains(t, snap.Unknown[modeldeploy.InputFatalXid], "管理账户无权读取内核日志")
	assert.Contains(t, snap.Unknown[modeldeploy.InputModelCache], "模型缓存目录不存在")
	e.db.MustExec(`INSERT INTO node_model_cache (node_id, model_id, state, bytes_done, verified_at) VALUES (?, ?, 'ready', 0, NOW())`, e.node, modelID)
	snap, err = src.ManagedLeaseSnapshot(ctx, buyer, no, modelID)
	require.NoError(t, err)
	assert.True(t, snap.ModelCached)
	assert.NotContains(t, snap.Unknown, modeldeploy.InputModelCache)
}
