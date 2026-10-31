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

// 清除后 restart 续租开通失败(无其它可用节点): 续租单取消退款、根订单回到 completed、租约 failed。
// 原租期已完整交付, 根订单仍须写 settle_ready_at 进入分账, 不能因租约是 failed(而非 purged)永久卡住。
func TestCrossSettleAfterPurgedRestartFails(t *testing.T) {
	e := graceProvEnv(t)
	ctx := context.Background()
	e.db.MustExec(`UPDATE leases SET grace_until=NOW()-INTERVAL 1 MINUTE WHERE id=?`, e.lease.ID)
	e.s.ExpiryRound(ctx)
	require.Equal(t, LeasePurging, e.leaseNow(t).State)
	e.overridePurge(t, fakeReply{Stdout: "@@removed\n"})
	purge := runOne(t, e.s)
	require.Equal(t, TaskSucceeded, purge.State, "last_error=%v", purge.LastError)
	require.Equal(t, LeasePurged, e.leaseNow(t).State)

	_, child := renewManaged(t, e, 1)
	payRenewal(t, e, child.OrderNo)
	require.Equal(t, LeasePending, e.leaseNow(t).State)
	e.s.onProvisionDead(leaseTaskOf(t, e, KindProvision), "开通失败")

	require.Equal(t, LeaseFailed, e.leaseNow(t).State)
	require.Equal(t, "completed", orderStatus(t, e))
	assert.Equal(t, "cancelled", childStatus(t, e, child.OrderNo))

	e.s.SettleRound(ctx)
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM orders WHERE id=? AND settle_ready_at IS NOT NULL`, e.order.ID),
		"原租期已交付, 根订单须进入分账")
}

// 停机责任金额无法计算(remedy_amount=NULL)的待退费段: 退款扫描不会提交, 结算须给出可告警的阻塞原因而不是静默等待。
func TestCrossSettleBlockedOnUnknownOutageAmount(t *testing.T) {
	e := newSettleEnv(t, true)
	e.purge(t)
	e.db.MustExec(`INSERT INTO lease_outages (lease_id, order_id, cause, liable, started_at, ended_at, seconds, minutes, counted,
		remedy, remedy_amount, liability_amount) VALUES (?, ?, 'node_offline', 'supplier', NOW()-INTERVAL 30 MINUTE, NOW()-INTERVAL 10 MINUTE,
		1200, 20, 1, 'refund', NULL, NULL)`, e.lease.ID, e.order.ID)
	blocked, err := e.s.markSettleReady(e.order.ID)
	require.NoError(t, err)
	assert.Equal(t, "outage_amount_unknown", blocked)
	assert.False(t, e.settleReady(t, e.order.ID))
}
