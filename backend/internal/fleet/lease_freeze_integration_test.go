package fleet

import (
	"context"
	"testing"
	"time"

	"tokenfactory/pkg/errcode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// T-088 冻结/解冻集成测试: 真实 MySQL + compute 订单事务 + fleet 租约钩子。

type wrongFreezeRec struct {
	recordedObs
	leaseID          int64
	frozen, unfrozen time.Time
}

func (r *wrongFreezeRec) WrongFreeze(_ context.Context, id int64, a, b time.Time) {
	r.leaseID, r.frozen, r.unfrozen = id, a, b
}

func deliveryStatus(t *testing.T, e *provEnv) string {
	t.Helper()
	var st string
	require.NoError(t, e.db.Get(&st, `SELECT access_status FROM order_deliveries WHERE order_id=?`, e.order.ID))
	return st
}

func TestFreezeSuspendsLeaseAndUnfreezeRestores(t *testing.T) {
	e := activeProvEnv(t)
	rec := &wrongFreezeRec{}
	e.s.SetOutageRecorder(rec)
	e.comp.SetLeaseHooks(e.s.LeaseHooks())
	before := e.leaseNow(t).AccessChangedAt

	require.NoError(t, e.comp.FreezeOrder(e.order.OrderNo))
	assert.Equal(t, "frozen", orderStatus(t, e))
	assert.Equal(t, "revoked", deliveryStatus(t, e))
	l := e.leaseNow(t)
	require.Equal(t, LeaseSuspended, l.State)
	require.NotNil(t, l.SuspendedAt)
	assert.False(t, l.AccessChangedAt.Before(before), "撤销游标刷新")
	kill := leaseTaskOf(t, e, KindKillSessions)
	assert.Equal(t, prioKillSessions, kill.Priority)
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='leased'`, e.lease.ID), "不释放卡")

	// 重复冻结幂等: 不重复入队断会话。
	require.NoError(t, e.comp.FreezeOrder(e.order.OrderNo))
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='kill_sessions'`, e.lease.ID))

	out, err := e.comp.AdminUnfreezeOrder(10, e.order.OrderNo, true, "127.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, "active", out.Status)
	assert.Equal(t, "active", orderStatus(t, e))
	assert.Equal(t, "delivered", deliveryStatus(t, e), "凭证恢复")
	l = e.leaseNow(t)
	assert.Equal(t, LeaseActive, l.State)
	assert.Nil(t, l.SuspendedAt)
	assert.Equal(t, e.lease.ID, rec.leaseID, "误冻结交给停机认定")
	assert.False(t, rec.frozen.IsZero())
	assert.True(t, !rec.unfrozen.Before(rec.frozen))
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM audit_logs WHERE action='unfreeze_order' AND target_id=?`, e.order.ID))

	// 非冻结订单解冻 → 40900。
	_, err = e.comp.AdminUnfreezeOrder(10, e.order.OrderNo, false, "127.0.0.1")
	assert.Equal(t, errcode.Conflict, errcode.CodeOf(err, 0))
}

func TestUnfreezeExpiredGoesToGrace(t *testing.T) {
	e := activeProvEnv(t)
	require.NoError(t, e.comp.FreezeOrder(e.order.OrderNo))
	expireLease(t, e)
	e.s.ExpiryRound(context.Background()) // 冻结中到期: 卡 draining + stop, 租约保持 suspended
	require.Equal(t, LeaseSuspended, e.leaseNow(t).State)

	out, err := e.comp.AdminUnfreezeOrder(10, e.order.OrderNo, false, "127.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, "completed", out.Status)
	assert.Equal(t, "completed", orderStatus(t, e))
	l := e.leaseNow(t)
	require.Equal(t, LeaseGrace, l.State)
	require.NotNil(t, l.GraceUntil)
	assert.WithinDuration(t, time.Now().Add(24*time.Hour), *l.GraceUntil, time.Minute, "宽限自解冻起算")
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='stop'`, e.lease.ID), "不重复入队 stop")
	assert.Equal(t, "revoked", deliveryStatus(t, e))
}

func TestFreezeBeforeProvisionThenUnfreezeRequeues(t *testing.T) {
	e := newProvEnv(t, true)
	// 已付款待开通: 冻结后开通任务领取即放弃。
	require.NoError(t, e.comp.FreezeOrder(e.order.OrderNo))
	assert.Equal(t, LeasePending, e.leaseNow(t).State, "未开通的租约不动")
	done := runOne(t, e.s)
	require.NotNil(t, done)
	require.Equal(t, TaskFailed, done.State)

	e.db.MustExec(`UPDATE leases SET access_changed_at=NOW() - INTERVAL 1 HOUR WHERE id=?`, e.lease.ID)
	out, err := e.comp.AdminUnfreezeOrder(10, e.order.OrderNo, false, "127.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, "paid", out.Status)
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM leases WHERE id=? AND state='pending'
		AND access_changed_at > NOW() - INTERVAL 1 MINUTE`, e.lease.ID), "租约状态未变也刷新撤销游标")
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='provision' AND state='pending'`, e.lease.ID),
		"解冻重新入队开通")

	// 重新开通成功。
	e.outcome(t, runOne(t, e.s))
	assert.Equal(t, LeaseActive, e.leaseNow(t).State)
	assert.Equal(t, "active", orderStatus(t, e))
}

func TestUnfreezeRejectsManualAndRunningProvision(t *testing.T) {
	e := newProvEnv(t, true)
	require.NoError(t, e.comp.FreezeOrder(e.order.OrderNo))
	e.db.MustExec(`UPDATE node_tasks SET state='running' WHERE lease_id=? AND kind='provision'`, e.lease.ID)
	_, err := e.comp.AdminUnfreezeOrder(10, e.order.OrderNo, false, "127.0.0.1")
	assert.Equal(t, errcode.Conflict, errcode.CodeOf(err, 0), "开通任务执行中拒绝解冻")
	assert.Equal(t, "frozen", orderStatus(t, e), "整体回滚")

	e.db.MustExec(`UPDATE orders SET fulfillment_mode='manual' WHERE id=?`, e.order.ID)
	_, err = e.comp.AdminUnfreezeOrder(10, e.order.OrderNo, false, "127.0.0.1")
	assert.Equal(t, errcode.ParamInvalid, errcode.CodeOf(err, 0))
}
