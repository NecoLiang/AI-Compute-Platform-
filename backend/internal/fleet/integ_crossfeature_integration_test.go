package fleet

import (
	"context"
	"testing"
	"time"

	"tokenfactory/internal/catalog"
	"tokenfactory/internal/modeldeploy"
	"tokenfactory/internal/payment"
	"tokenfactory/pkg/config"
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

	// 连接信息标明整机, 前端据此隐藏一键部署入口、提示登录宿主机。
	a, err := e.s.OrderAccess(21, e.order.OrderNo)
	require.NoError(t, err)
	assert.Equal(t, "bare_metal", a.DeliveryKind)

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

// 网关存活(check 5): 网关验签请求刷新心跳; 心跳超过 gatewayStaleAfter 未刷新 → 在租租约记 gateway_down(平台责任,
// 从最后一次心跳算起); 主服务启动宽限期内与心跳未知时不认定; 网关失联期间对账 Up 不结束, 恢复上报后结束。
func TestCrossGatewayHeartbeatOutage(t *testing.T) {
	e := newGWEnv(t, true)
	ctx := context.Background()
	e.s.SetRedis(e.rdb)
	e.s.cfg.Outage = config.FleetOutage{MinMinutes: 5, RefundRatio: 0.10, RefundSingleHours: 2, MaxSupplierMinutes30d: 360}
	e.db.MustExec(`UPDATE leases SET provisioned_at=NOW()-INTERVAL 1 HOUR WHERE id=?`, e.lease.ID)
	require.NoError(t, e.rdb.Del(ctx, gatewayHeartbeatKey).Err())

	_, err := e.client.Revocations(ctx, "")
	require.NoError(t, err)
	ms, err := e.rdb.Get(ctx, gatewayHeartbeatKey).Int64()
	require.NoError(t, err)
	assert.WithinDuration(t, time.Now(), time.UnixMilli(ms), 5*time.Second)

	stale := time.Now().Add(-20 * time.Minute)
	e.rdb.Set(ctx, gatewayHeartbeatKey, stale.UnixMilli(), 0)
	e.s.OutageRound(ctx)
	assert.Empty(t, e.outages(t), "主服务启动宽限期内不认定")

	e.s.startedAt = time.Now().Add(-time.Hour)
	require.NoError(t, e.rdb.Del(ctx, gatewayHeartbeatKey).Err())
	e.s.OutageRound(ctx)
	assert.Empty(t, e.outages(t), "从未收到网关请求(心跳未知)不认定")

	e.rdb.Set(ctx, gatewayHeartbeatKey, stale.UnixMilli(), 0)
	e.s.OutageRound(ctx)
	rows := e.outages(t)
	require.Len(t, rows, 1)
	assert.Equal(t, OutageGatewayDown, rows[0].Cause)
	assert.Equal(t, "platform", rows[0].Liable)
	assert.WithinDuration(t, stale, rows[0].StartedAt, 2*time.Second)
	assert.Nil(t, rows[0].EndedAt)

	e.recoverContainer(t)
	require.Nil(t, e.outages(t)[0].EndedAt, "网关仍失联: 容器在跑也不结束")

	_, err = e.client.Revocations(ctx, "")
	require.NoError(t, err)
	e.recoverContainer(t)
	rows = e.outages(t)
	require.Len(t, rows, 1)
	require.NotNil(t, rows[0].EndedAt, "网关恢复上报 + 对账确认在跑 → 结束")
	require.NotNil(t, rows[0].Counted)
}

// 供给方结算列表/按单流水逐行带出订单履约模式与分账闸门: managed 行在 settle_ready_at 写入前为 null(前端标「租期结束后分账」)。
func TestCrossSupplierSettlementRowsExposeSettleGate(t *testing.T) {
	e := newSettleEnv(t, true)
	var supplierID int64
	require.NoError(t, e.db.Get(&supplierID, `SELECT p.supplier_id FROM orders o JOIN products p ON p.id=o.product_id WHERE o.id=?`, e.order.ID))
	repo := payment.NewRepository(e.db)

	list, total, err := repo.ListSupplierSettlements(supplierID, "", 1, 20)
	require.NoError(t, err)
	require.EqualValues(t, 1, total)
	assert.Equal(t, "managed", list[0].FulfillmentMode)
	assert.Nil(t, list[0].SettleReadyAt, "租期未结束: 分账闸门未开")

	e.purge(t)
	e.s.SettleRound(context.Background())
	require.True(t, e.settleReady(t, e.order.ID))
	rows, err := repo.GetSettlementsByOrder(supplierID, e.order.OrderNo)
	require.NoError(t, err)
	require.NotEmpty(t, rows)
	for _, r := range rows {
		assert.Equal(t, "managed", r.FulfillmentMode)
		require.NotNil(t, r.SettleReadyAt)
	}
}
