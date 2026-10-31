package fleet

import (
	"context"
	"testing"

	"tokenfactory/internal/catalog"
	"tokenfactory/internal/modeldeploy"
	"tokenfactory/pkg/errcode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// v1.2 集成 CR(2026-10-31): 各特性分支并行开发, 合入 integ 后的跨特性交互测试。

// 整机租约不支持一键部署: deploy-plans 与提交都返回 40001(中文原因), 不写部署行、不入队任务;
// 整机到期清除时 stopDeploymentsTx 对无部署的整机租约无副作用。
func TestCrossBareMetalRejectsModelDeploy(t *testing.T) {
	e := newBMEnv(t, true)
	e.s.cfg.ModelPullImage = testPullImage
	e.s.cfg.EnginePortStart, e.s.cfg.EnginePortEnd = mdPortStart, mdPortEnd
	modelID := insertTestModel(t, e.db, mdModelKey, mdWeightsBytes)
	md := modeldeploy.NewService(modeldeploy.NewRepository(e.db), catalog.NewService(catalog.NewRepository(e.db)))
	md.SetLeaseSource(e.s.ModelDeployLeaseSource())
	md.SetDeployRuntime(e.s.AttachModelDeploy(md), nil, nil)
	ctx := context.Background()

	_, err := md.DeployPlans(ctx, modeldeploy.PlanRequest{BuyerID: 21, OrderNo: e.order.OrderNo, ModelID: modelID, GPUMemUtilPct: 90})
	require.Error(t, err)
	assert.Equal(t, errcode.ParamInvalid, errCode(err), "整机 deploy-plans 须 40001: %v", err)
	assert.Contains(t, err.Error(), "整机")

	_, err = md.Submit(ctx, modeldeploy.SubmitRequest{BuyerID: 21, OrderNo: e.order.OrderNo, ModelID: modelID,
		TP: 2, DP: 1, GPUMemUtilPct: 90, LicenseAck: true, UntestedAck: true})
	require.Error(t, err)
	assert.Equal(t, errcode.ParamInvalid, errCode(err), "整机提交部署须 40001: %v", err)

	var n int
	require.NoError(t, e.db.Get(&n, `SELECT COUNT(*) FROM model_deployments WHERE lease_id=?`, e.lease.ID))
	assert.Zero(t, n)
	require.NoError(t, e.db.Get(&n, `SELECT COUNT(*) FROM node_tasks WHERE kind IN ('deploy_model','stop_model')`))
	assert.Zero(t, n)

	// 运行期兜底: 整机租约不给出容器配额/网络。
	_, err = e.s.AttachModelDeploy(md).LeaseRuntime(ctx, e.lease.ID)
	require.Error(t, err)

	// 到期: 整机直接 purging(无宽限), 部署联动不报错。
	expireLease(t, e.provEnv)
	e.s.ExpiryRound(ctx)
	assert.Equal(t, LeasePurging, e.leaseNow(t).State)
}

// 租约被重新分配节点/卡(宽限期后 restart 续租、开通失败改配)后, 恢复原部署须 40900 而不是在旧节点上以
// "GPU 不属于本租约"失败; 同一节点同一批卡时恢复照常进入 pending。
func TestCrossRestartRejectsMovedLease(t *testing.T) {
	e := newMDEnv(t)
	ctx := context.Background()
	v := e.toStarting(t, false)
	stopAndDrain := func() {
		t.Helper()
		_, err := e.md.Stop(ctx, mdBuyer, e.order.OrderNo, v.DeployNo)
		require.NoError(t, err)
		e.fake.Override("if docker inspect --type container "+modeldeploy.EngineContainer(v.DeployNo)+" >/dev/null 2>&1; then docker rm -f",
			fakeReply{Stdout: "@@removed\n"})
		e.drain(t)
		require.Equal(t, "stopped", e.dep(t, v.DeployNo).State)
	}
	stopAndDrain()

	r, err := e.md.Restart(ctx, mdBuyer, e.order.OrderNo, v.DeployNo)
	require.NoError(t, err)
	assert.Equal(t, "pending", r.State)
	stopAndDrain()

	// 租约换卡: 部署记录里的卡已不在 gpu_slots.lease_id=本租约中。
	e.db.MustExec(`UPDATE model_deployments SET gpu_uuids='["GPU-moved-0","GPU-moved-1"]' WHERE deploy_no=?`, v.DeployNo)
	before := countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE kind='deploy_model'`)
	_, err = e.md.Restart(ctx, mdBuyer, e.order.OrderNo, v.DeployNo)
	require.Error(t, err)
	assert.Equal(t, errcode.Conflict, errCode(err), "%v", err)
	assert.Equal(t, "stopped", e.dep(t, v.DeployNo).State)
	assert.Equal(t, before, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE kind='deploy_model'`))
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM model_deployments WHERE lease_active_claim IS NOT NULL`))
}
