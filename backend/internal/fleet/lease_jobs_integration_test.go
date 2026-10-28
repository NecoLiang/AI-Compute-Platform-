package fleet

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// T-087 租约周期任务集成测试: 真实 MySQL + fakeNode(真实 SSH) + Redis(提醒去重)。

func expireLease(t *testing.T, e *provEnv) {
	t.Helper()
	e.db.MustExec(`UPDATE leases SET access_until=NOW()-INTERVAL 1 MINUTE WHERE id=?`, e.lease.ID)
	e.db.MustExec(`UPDATE orders SET lease_end_at=NOW()-INTERVAL 1 MINUTE WHERE id=?`, e.order.ID)
}

func orderStatus(t *testing.T, e *provEnv) string {
	t.Helper()
	var st string
	require.NoError(t, e.db.Get(&st, `SELECT status FROM orders WHERE id=?`, e.order.ID))
	return st
}

func (e *provEnv) leaseNow(t *testing.T) *Lease {
	t.Helper()
	l, err := e.s.repo.getLease(e.db, e.lease.ID)
	require.NoError(t, err)
	return l
}

func TestExpirySweepCompletesActiveOrder(t *testing.T) {
	e := activeProvEnv(t)
	e.s.ExpiryRound(context.Background())
	assert.Equal(t, "active", orderStatus(t, e), "未到期不动")

	expireLease(t, e)
	e.s.ExpiryRound(context.Background())
	assert.Equal(t, "completed", orderStatus(t, e))
	l := e.leaseNow(t)
	require.Equal(t, LeaseGrace, l.State)
	require.NotNil(t, l.GraceUntil)
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), *l.GraceUntil, time.Minute, "时计费宽限 24h")
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='draining'`, e.lease.ID))
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='stop' AND state='pending'`, e.lease.ID))

	// 再扫一轮: 幂等, 不重复入队。
	e.s.ExpiryRound(context.Background())
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='stop'`, e.lease.ID))
}

func TestExpirySweepFrozenOrderLeaseSide(t *testing.T) {
	e := activeProvEnv(t)
	e.db.MustExec(`UPDATE orders SET status='refunding' WHERE id=?`, e.order.ID)
	expireLease(t, e)
	e.s.ExpiryRound(context.Background())
	assert.Equal(t, "refunding", orderStatus(t, e), "订单状态不动, 由退款结案流转")
	l := e.leaseNow(t)
	assert.Equal(t, LeaseGrace, l.State, "以租约为准: 到期即转宽限")
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='stop'`, e.lease.ID))
	stock, _ := leaseStock(t, e.db)
	assert.Equal(t, 0, stock, "draining 不计库存")
}

func TestExpirySweepSuspendedKeepsEvidence(t *testing.T) {
	e := activeProvEnv(t)
	e.db.MustExec(`UPDATE orders SET status='frozen' WHERE id=?`, e.order.ID)
	e.db.MustExec(`UPDATE leases SET state='suspended' WHERE id=?`, e.lease.ID)
	expireLease(t, e)
	e.s.ExpiryRound(context.Background())
	l := e.leaseNow(t)
	assert.Equal(t, LeaseSuspended, l.State, "冻结中到期不进宽限")
	assert.Nil(t, l.GraceUntil)
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='draining'`, e.lease.ID))

	e.fake.Override(cmdOK(stopContainerCommand(e.lease.LeaseNo)), fakeReply{Stdout: "@@stopped " + fakeUUID0 + "," + fakeUUID1 + ",\n"})
	done := runOne(t, e.s)
	require.NotNil(t, done)
	require.Equal(t, TaskSucceeded, done.State, "last_error=%v", done.LastError)
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=?`, e.lease.ID))
	assert.Equal(t, LeaseSuspended, e.leaseNow(t).State)
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='purge'`, e.lease.ID), "保留现场, 不清除")

	e.s.ExpiryRound(context.Background())
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='stop'`, e.lease.ID))
}

func TestGraceExpiryPurgesUnlessRestartPending(t *testing.T) {
	e := activeProvEnv(t)
	expireLease(t, e)
	e.s.ExpiryRound(context.Background())
	require.Equal(t, LeaseGrace, e.leaseNow(t).State)
	e.db.MustExec(`UPDATE leases SET grace_until=NOW()-INTERVAL 1 SECOND WHERE id=?`, e.lease.ID)

	// 待支付的 restart 续租单: 暂缓清除。
	res := e.db.MustExec(`INSERT INTO orders (order_no, buyer_id, product_id, quantity, duration, unit_price, total_amount,
		platform_fee, status, pricing_mode, fulfillment_mode, stock_reserved, compliance_agreed)
		VALUES ('REN-T087', 21, ?, 2, 1, 1500, 3000, 0, 'pending_payment', 'hourly', 'managed', 0, 1)`, leaseProductID)
	ren, err := res.LastInsertId()
	require.NoError(t, err)
	e.db.MustExec(`INSERT INTO order_renewals (order_id, parent_order_id, request_id, mode, pricing_mode, lease_end_at)
		VALUES (?, ?, '00000000-0000-4000-8000-000000000087', 'restart', 'hourly', NOW())`, ren, e.order.ID)
	e.s.ExpiryRound(context.Background())
	assert.Equal(t, LeaseGrace, e.leaseNow(t).State)

	e.db.MustExec(`UPDATE orders SET status='cancelled' WHERE id=?`, ren)
	e.s.ExpiryRound(context.Background())
	assert.Equal(t, LeasePurging, e.leaseNow(t).State)
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='purge' AND state='pending'`, e.lease.ID))
}

func TestCurrentRemindTier(t *testing.T) {
	h := time.Hour
	cases := []struct {
		term, remaining time.Duration
		want            string
	}{
		{30 * 24 * h, 23 * h, "24h"},
		{30 * 24 * h, 25 * h, ""},
		{30 * 24 * h, 90 * time.Minute, "2h"},
		{30 * 24 * h, 10 * time.Minute, "30m"},
		{4 * h, 3 * h, ""}, // 租期不足 24h: 不发 24h 档
		{4 * h, 90 * time.Minute, "2h"},
		{h, 50 * time.Minute, ""}, // 租期 1h: 只发 30min 档
		{h, 20 * time.Minute, "30m"},
		{20 * time.Minute, 10 * time.Minute, ""},
	}
	for _, c := range cases {
		got := currentRemindTier(c.term, c.remaining)
		name := ""
		if got != nil {
			name = got.name
		}
		assert.Equal(t, c.want, name, "term=%v remaining=%v", c.term, c.remaining)
	}
}

func remindRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := testRedis(t)
	ctx := context.Background()
	clean := func() {
		keys, err := rdb.Keys(ctx, "fleet:*").Result()
		require.NoError(t, err)
		if len(keys) > 0 {
			require.NoError(t, rdb.Del(ctx, keys...).Err())
		}
	}
	clean()
	t.Cleanup(clean)
	return rdb
}

func TestRemindRound(t *testing.T) {
	rdb := remindRedis(t)
	e := activeProvEnv(t)
	e.s.SetRedis(rdb)
	wall := cmdOK(wallCommand(e.lease.LeaseNo, 90))
	e.fake.Override(wall, fakeReply{Stdout: "@@sent\n"})
	notes := func(title string) int {
		return countRows(t, e.db, `SELECT COUNT(*) FROM notifications WHERE user_id=21 AND title=?`, title)
	}

	// 开通满 2 天、剩 90 分钟: 发 2h 档一次, 重复执行去重。
	e.db.MustExec(`UPDATE leases SET provisioned_at=NOW()-INTERVAL 2 DAY, access_until=NOW()+INTERVAL 5400 SECOND WHERE id=?`, e.lease.ID)
	e.s.RemindRound(context.Background())
	e.s.RemindRound(context.Background())
	assert.Equal(t, 1, notes("租期即将结束"))
	assert.Contains(t, e.fake.Commands(), wall, "容器内 wall 广播")

	// 续租(extend)后 access_until 变化: 新期限重新提醒。
	e.db.MustExec(`UPDATE leases SET access_until=NOW()+INTERVAL 20 MINUTE WHERE id=?`, e.lease.ID)
	e.s.RemindRound(context.Background())
	assert.Equal(t, 2, notes("租期即将结束"))

	// 进入宽限: 通知宽限截止。
	expireLease(t, e)
	e.s.ExpiryRound(context.Background())
	e.s.RemindRound(context.Background())
	e.s.RemindRound(context.Background())
	assert.Equal(t, 1, notes("租期已结束"))
}

type recordedObs struct {
	mu  sync.Mutex
	obs []LeaseObservation
}

func (r *recordedObs) ObserveLease(_ context.Context, o LeaseObservation) {
	r.mu.Lock()
	r.obs = append(r.obs, o)
	r.mu.Unlock()
}
func (r *recordedObs) WrongFreeze(context.Context, int64, time.Time, time.Time) {}
func (r *recordedObs) last() LeaseObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.obs[len(r.obs)-1]
}

func TestReconcileRestartsExitedContainer(t *testing.T) {
	e := activeProvEnv(t)
	rec := &recordedObs{}
	e.s.SetOutageRecorder(rec)
	no := e.lease.LeaseNo
	name := containerNameOf(no)
	start := cmdOK(startContainerCommand(no))
	stop := cmdOK(stopContainerCommand(no))
	e.fake.Override(start, fakeReply{Stdout: "@@started\n"})
	e.fake.Override(stop, fakeReply{Stdout: "@@stopped\n"})

	// 运行中: 可用。
	e.fake.Override(cmdLeaseContainers, fakeReply{Stdout: name + " running\n"})
	e.s.ReconcileRound(context.Background())
	assert.True(t, rec.last().Up)
	assert.Empty(t, e.gw.rechecks)

	// 节点重启后容器 exited: 防火墙 → start → 主机密钥 → 网关复检。
	e.fake.Override(cmdLeaseContainers, fakeReply{Stdout: name + " exited\n"})
	before := len(e.cmdsWith("docker run --rm --network host --cap-drop ALL"))
	e.s.ReconcileRound(context.Background())
	assert.Equal(t, []string{no}, e.gw.rechecks)
	assert.Equal(t, before+1, len(e.cmdsWith("docker run --rm --network host --cap-drop ALL")), "启动前重新下发防火墙规则")
	assert.Contains(t, e.fake.Commands(), start)
	assert.NotContains(t, e.fake.Commands(), stop)
	assert.True(t, rec.last().Up)
	assert.Nil(t, e.leaseNow(t).ResourceAlert)

	// 自检看到的卡与租约不符: 停回去 + 告警 + 停机观测。
	e.gw.mu.Lock()
	e.gw.leaseOut = gpuListL(fakeUUID0)
	e.gw.mu.Unlock()
	e.s.ReconcileRound(context.Background())
	assert.Contains(t, e.fake.Commands(), stop)
	l := e.leaseNow(t)
	require.NotNil(t, l.ResourceAlert)
	assert.Contains(t, *l.ResourceAlert, reconcileAlertTag)
	assert.False(t, rec.last().Up)
	assert.Equal(t, OutageContainerDown, rec.last().Cause)

	// 防火墙下发失败: 不启动容器。
	e.gw.mu.Lock()
	e.gw.leaseOut = gpuListL(fakeUUID0, fakeUUID1)
	e.gw.mu.Unlock()
	e.fake.Override("docker run --rm --network host --cap-drop ALL", fakeReply{Stderr: "iptables: Permission denied", Code: 1})
	starts := 0
	for _, c := range e.fake.Commands() {
		if c == start {
			starts++
		}
	}
	e.s.ReconcileRound(context.Background())
	after := 0
	for _, c := range e.fake.Commands() {
		if c == start {
			after++
		}
	}
	assert.Equal(t, starts, after, "防火墙失败不启动容器")
	assert.False(t, rec.last().Up)

	// 恢复运行: 清除对账告警。
	e.fake.Override(cmdLeaseContainers, fakeReply{Stdout: name + " running\n"})
	e.s.ReconcileRound(context.Background())
	assert.Nil(t, e.leaseNow(t).ResourceAlert)
	assert.True(t, rec.last().Up)

	// 容器丢失。
	e.fake.Override(cmdLeaseContainers, fakeReply{Stdout: ""})
	e.s.ReconcileRound(context.Background())
	require.NotNil(t, e.leaseNow(t).ResourceAlert)
	assert.Contains(t, *e.leaseNow(t).ResourceAlert, "找不到")
}

func TestReconcileHostKeyMismatchStopsBack(t *testing.T) {
	e := activeProvEnv(t)
	no := e.lease.LeaseNo
	e.fake.Override(cmdLeaseContainers, fakeReply{Stdout: containerNameOf(no) + " exited\n"})
	e.fake.Override(cmdOK(startContainerCommand(no)), fakeReply{Stdout: "@@started\n"})
	stop := cmdOK(stopContainerCommand(no))
	e.fake.Override(stop, fakeReply{Stdout: "@@stopped\n"})
	e.db.MustExec(`UPDATE leases SET upstream_host_key_fp='SHA256:other' WHERE id=?`, e.lease.ID)
	e.s.ReconcileRound(context.Background())
	assert.Contains(t, e.fake.Commands(), stop)
	assert.Empty(t, e.gw.rechecks, "主机密钥不符不做网关自检")
	require.NotNil(t, e.leaseNow(t).ResourceAlert)
}

func TestReconcileNodeUnreachable(t *testing.T) {
	e := activeProvEnv(t)
	rec := &recordedObs{}
	e.s.SetOutageRecorder(rec)
	e.db.MustExec(`UPDATE supplier_nodes SET ssh_port=1 WHERE id=?`, e.node)
	e.s.ReconcileRound(context.Background())
	require.NotEmpty(t, rec.obs)
	assert.Equal(t, OutageNodeOffline, rec.last().Cause)
	assert.Nil(t, e.leaseNow(t).ResourceAlert, "节点离线以探活为准, 不写租约告警")
}

func TestRequeueStuckCleanup(t *testing.T) {
	e := activeProvEnv(t)
	require.NoError(t, e.comp.CompleteOrder(e.order.OrderNo))
	e.db.MustExec(`UPDATE node_tasks SET state='failed' WHERE lease_id=? AND kind='stop'`, e.lease.ID)
	e.fake.Override(cmdLeaseContainers, fakeReply{Stdout: ""})
	e.s.ReconcileRound(context.Background())
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='stop' AND state='pending'`, e.lease.ID))
	e.s.ReconcileRound(context.Background())
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='stop' AND state='pending'`, e.lease.ID), "已有未结束任务不重复补")

	// dead 不补(运营处理)。
	e.db.MustExec(`UPDATE node_tasks SET state='dead' WHERE lease_id=? AND kind='stop'`, e.lease.ID)
	e.s.ReconcileRound(context.Background())
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='stop' AND state='pending'`, e.lease.ID))

	// purging 无 purge 任务(cleanup_only 不算) → 补。
	e.db.MustExec(`UPDATE leases SET state='purging' WHERE id=?`, e.lease.ID)
	_, err := e.s.repo.insertLeaseTaskMax(e.db, e.node, e.lease.ID, KindPurge, prioPurge, cleanupMaxAttempts,
		LeaseTaskPayload{LeaseID: e.lease.ID, CleanupOnly: true})
	require.NoError(t, err)
	e.s.ReconcileRound(context.Background())
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='purge' AND state='pending'`, e.lease.ID))
}
