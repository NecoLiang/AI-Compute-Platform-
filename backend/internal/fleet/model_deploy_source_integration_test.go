package fleet

import (
	"context"
	"errors"
	"strings"
	"testing"

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
	// 播种拓扑组 "g0"(非 nvlink 组)→ 确定无 NVLink; 未采集项全部声明为未知。
	assert.False(t, snap.NVLink)
	assert.False(t, snap.ModelCached)
	assert.Contains(t, snap.Unknown, modeldeploy.InputVRAMUsage)
	assert.Contains(t, snap.Unknown, modeldeploy.InputFatalXid)
	assert.Contains(t, snap.Unknown, modeldeploy.InputModelCache)
	assert.NotContains(t, snap.Unknown, modeldeploy.InputNVLink)

	// 同一 nvlink 组 → NVLink 未知; 卡故障如实反映; 节点已校验缓存该模型 → 不再声明缓存空间未知。
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
