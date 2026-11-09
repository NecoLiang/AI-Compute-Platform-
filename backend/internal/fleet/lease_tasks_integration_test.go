package fleet

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// T-087 收尾任务执行器集成测试: 真实 MySQL + fakeNode(真实 SSH)。

const testContainerID = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

// activeProvEnv 开通流水线跑完的 active 租约(2 卡 leased)。
func activeProvEnv(t *testing.T) *provEnv {
	t.Helper()
	e := newProvEnv(t, true)
	e.s.cfg.Grace.Short, e.s.cfg.Grace.Long, e.s.cfg.Grace.Nightly = 24*time.Hour, 72*time.Hour, 24*time.Hour
	e.outcome(t, runOne(t, e.s))
	l, err := e.s.repo.getLease(e.db, e.lease.ID)
	require.NoError(t, err)
	require.Equal(t, LeaseActive, l.State)
	e.lease = l
	return e
}

// cmdOK 测试里构造命令模板(参数均为合法值, 出错即测试代码错误)。
func cmdOK(c string, err error) string {
	if err != nil {
		panic(err)
	}
	return c
}

func (e *provEnv) overridePurge(t *testing.T, vol fakeReply) {
	t.Helper()
	no := e.lease.LeaseNo
	e.fake.Override(cmdOK(purgeContainerCommand(containerNameOf(no))), fakeReply{Stdout: "@@removed " + testContainerID + "\n"})
	e.fake.Override(cmdOK(purgeVolumeCommand(volumeNameOf(no))), vol)
	e.fake.Override(cmdOK(purgeNetworkCommand(networkNameOf(no))), fakeReply{Stdout: "@@absent\n"})
}

func leaseTaskOf(t *testing.T, e *provEnv, kind string) *Task {
	t.Helper()
	var tasks []Task
	require.NoError(t, e.db.Select(&tasks, `SELECT `+taskColumns+` FROM node_tasks WHERE lease_id=? AND kind=? ORDER BY id DESC`, e.lease.ID, kind))
	require.NotEmpty(t, tasks)
	return &tasks[0]
}

func TestStopTaskFreesDrainingSlots(t *testing.T) {
	e := activeProvEnv(t)
	require.NoError(t, e.comp.CompleteOrder(e.order.OrderNo))
	assert.Equal(t, 0, realFree(t, e.db))
	stop := leaseTaskOf(t, e, KindStop)
	assert.Equal(t, cleanupMaxAttempts, stop.MaxAttempts)

	e.fake.Override(cmdOK(stopContainerCommand(e.lease.LeaseNo)), fakeReply{Stdout: "@@stopped " + fakeUUID0 + "," + fakeUUID1 + ",\n"})
	done := runOne(t, e.s)
	require.NotNil(t, done)
	require.Equal(t, KindStop, done.Kind)
	require.Equal(t, TaskSucceeded, done.State, "last_error=%v", done.LastError)
	var out StopOutcome
	require.NoError(t, json.Unmarshal(done.Result, &out))
	assert.Equal(t, "stopped", out.Container)
	assert.Equal(t, []string{fakeUUID0, fakeUUID1}, out.GPUs)
	assert.EqualValues(t, 2, out.FreedSlots)

	l, _ := e.s.repo.getLease(e.db, e.lease.ID)
	assert.Equal(t, LeaseGrace, l.State, "停容器不改租约状态, 宽限计时照常")
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=?`, e.lease.ID), "draining → free 并清空 lease_id")
	stock, _ := leaseStock(t, e.db)
	assert.Equal(t, 2, stock, "卡槽释放后派生库存同步恢复")

	// 已不在 grace(例如进入清除)的租约: stop 放弃, 不重试。
	_, err := e.s.repo.insertLeaseTaskMax(e.db, e.node, e.lease.ID, KindStop, prioStop, cleanupMaxAttempts, LeaseTaskPayload{LeaseID: e.lease.ID})
	require.NoError(t, err)
	e.db.MustExec(`UPDATE leases SET state='purging' WHERE id=?`, e.lease.ID)
	done = runOne(t, e.s)
	require.NotNil(t, done)
	assert.Equal(t, TaskFailed, done.State)
	assert.Equal(t, 1, done.Attempts)
}

func TestStopTaskNodeErrorRetries(t *testing.T) {
	e := activeProvEnv(t)
	require.NoError(t, e.comp.CompleteOrder(e.order.OrderNo))
	e.fake.Override(cmdOK(stopContainerCommand(e.lease.LeaseNo)), fakeReply{Stderr: "Cannot connect to the Docker daemon", Code: 1})
	done := runOne(t, e.s)
	require.NotNil(t, done)
	assert.Equal(t, TaskPending, done.State, "节点侧错误可重试")
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='draining'`, e.lease.ID))
}

func TestPurgeTaskWritesReceipt(t *testing.T) {
	e := activeProvEnv(t)
	require.NoError(t, e.comp.AdminUpdateOrderStatus(1, e.order.OrderNo, "cancelled", "127.0.0.1"))
	l, _ := e.s.repo.getLease(e.db, e.lease.ID)
	require.Equal(t, LeasePurging, l.State)

	// 数据卷删除失败 → 可重试, 租约保持 purging, 卡槽保持 draining。
	e.overridePurge(t, fakeReply{Stderr: "volume is in use", Code: 1})
	done := runOne(t, e.s)
	require.NotNil(t, done)
	require.Equal(t, KindPurge, done.Kind)
	assert.Equal(t, TaskPending, done.State)
	assert.Contains(t, *done.LastError, "volume")
	l, _ = e.s.repo.getLease(e.db, e.lease.ID)
	assert.Equal(t, LeasePurging, l.State)

	// 重试成功(容器已不存在 → absent, 幂等)。
	e.fake.ClearOverrides()
	e.overridePurge(t, fakeReply{Stdout: "@@removed\n"})
	e.fake.Override(cmdOK(purgeContainerCommand(containerNameOf(e.lease.LeaseNo))), fakeReply{Stdout: "@@absent\n"})
	e.db.MustExec(`UPDATE node_tasks SET run_after=NOW() WHERE id=?`, done.ID)
	done = runOne(t, e.s)
	require.NotNil(t, done)
	require.Equal(t, TaskSucceeded, done.State, "last_error=%v", done.LastError)
	var out PurgeOutcome
	require.NoError(t, json.Unmarshal(done.Result, &out))
	assert.EqualValues(t, 2, out.FreedSlots)
	require.Len(t, out.Steps, 3)
	assert.Equal(t, "absent", out.Steps[0].Outcome)
	assert.Equal(t, "removed", out.Steps[1].Outcome)

	l, _ = e.s.repo.getLease(e.db, e.lease.ID)
	assert.Equal(t, LeasePurged, l.State)
	assert.NotNil(t, l.PurgedAt)
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=?`, e.lease.ID))
	stock, _ := leaseStock(t, e.db)
	assert.Equal(t, 2, stock)

	var after string
	require.NoError(t, e.db.Get(&after, `SELECT after_value FROM audit_logs WHERE action='lease.purged' AND target_type='lease' AND target_id=?`, e.lease.ID))
	var r PurgeReceipt
	require.NoError(t, json.Unmarshal([]byte(after), &r))
	assert.Equal(t, e.lease.LeaseNo, r.LeaseNo)
	assert.Equal(t, e.order.ID, r.OrderID)
	assert.Equal(t, volumeNameOf(e.lease.LeaseNo), r.Volume)
	assert.Equal(t, "system", r.Executor)
	assert.Contains(t, r.Erasure, "逻辑删除")
	require.NotNil(t, r.UpstreamPort)
	assert.Equal(t, provPortStart, *r.UpstreamPort)
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM notifications WHERE user_id=21 AND title='算力环境已清除'`))

	// 端口在终态释放(port_claim 置空), 新租约可复用。
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM leases WHERE id=? AND port_claim IS NOT NULL`, e.lease.ID))
}

func TestPurgeAfterGraceRefund(t *testing.T) {
	e := activeProvEnv(t)
	require.NoError(t, e.comp.CompleteOrder(e.order.OrderNo))
	tx := e.db.MustBegin()
	o, err := e.comp.GetOrder(e.order.OrderNo)
	require.NoError(t, err)
	require.NoError(t, e.s.releaseLeaseTx(tx, o, "refunded"))
	require.NoError(t, tx.Commit())
	l, _ := e.s.repo.getLease(e.db, e.lease.ID)
	assert.Equal(t, LeasePurging, l.State, "宽限期内退款完成 → 清除")
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='purge'`, e.lease.ID))
	// completed 再次流转: 幂等。
	tx = e.db.MustBegin()
	require.NoError(t, e.s.releaseLeaseTx(tx, o, "completed"))
	require.NoError(t, tx.Commit())
}

func TestCleanupOnlyPurge(t *testing.T) {
	e := activeProvEnv(t)
	no := e.lease.LeaseNo
	pl := LeaseTaskPayload{LeaseID: e.lease.ID, CleanupOnly: true, Reason: "换节点", ContainerName: containerNameOf(no), VolumeName: volumeNameOf(no)}

	// 租约仍在本节点运行: 同名资源属当前环境, 放弃。
	_, err := e.s.repo.insertLeaseTaskMax(e.db, e.node, e.lease.ID, KindPurge, prioPurge, cleanupMaxAttempts, pl)
	require.NoError(t, err)
	done := runOne(t, e.s)
	require.NotNil(t, done)
	assert.Equal(t, TaskFailed, done.State)
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='leased'`, e.lease.ID))

	// 租约已失败(开通失败善后): 清理残留, 释放本节点 draining 卡, 租约状态不变, 回执 lease.cleanup。
	e.db.MustExec(`UPDATE leases SET state='failed', port_claim=NULL WHERE id=?`, e.lease.ID)
	e.db.MustExec(`UPDATE gpu_slots SET state='draining' WHERE lease_id=?`, e.lease.ID)
	e.overridePurge(t, fakeReply{Stdout: "@@removed\n"})
	_, err = e.s.repo.insertLeaseTaskMax(e.db, e.node, e.lease.ID, KindPurge, prioPurge, cleanupMaxAttempts, pl)
	require.NoError(t, err)
	done = runOne(t, e.s)
	require.NotNil(t, done)
	require.Equal(t, TaskSucceeded, done.State, "last_error=%v", done.LastError)
	var out PurgeOutcome
	require.NoError(t, json.Unmarshal(done.Result, &out))
	assert.True(t, out.CleanupOnly)
	assert.EqualValues(t, 2, out.FreedSlots)
	l, _ := e.s.repo.getLease(e.db, e.lease.ID)
	assert.Equal(t, LeaseFailed, l.State)
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM audit_logs WHERE action='lease.cleanup' AND target_id=?`, e.lease.ID))
	assert.Equal(t, 2, realFree(t, e.db))
}

func TestKillSessionsTask(t *testing.T) {
	e := activeProvEnv(t)
	kill := cmdOK(killSessionsCommand(e.lease.LeaseNo))
	e.fake.Override(kill, fakeReply{Stdout: "@@killed 3\n"})

	// 未冻结: 放弃。
	_, err := e.s.repo.insertLeaseTaskMax(e.db, e.node, e.lease.ID, KindKillSessions, prioKillSessions, cleanupMaxAttempts, LeaseTaskPayload{LeaseID: e.lease.ID})
	require.NoError(t, err)
	done := runOne(t, e.s)
	require.NotNil(t, done)
	assert.Equal(t, TaskFailed, done.State)

	e.db.MustExec(`UPDATE leases SET state='suspended' WHERE id=?`, e.lease.ID)
	_, err = e.s.repo.insertLeaseTaskMax(e.db, e.node, e.lease.ID, KindKillSessions, prioKillSessions, cleanupMaxAttempts, LeaseTaskPayload{LeaseID: e.lease.ID})
	require.NoError(t, err)
	done = runOne(t, e.s)
	require.NotNil(t, done)
	require.Equal(t, TaskSucceeded, done.State, "last_error=%v", done.LastError)
	var out KillSessionsOutcome
	require.NoError(t, json.Unmarshal(done.Result, &out))
	assert.Equal(t, KillSessionsOutcome{LeaseNo: e.lease.LeaseNo, Killed: 3, Running: true}, out)
	assert.Contains(t, e.fake.Commands(), kill)
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='leased'`, e.lease.ID), "冻结保全现场, 不释放卡")
}

func TestExpressClaimAndPerLeaseExclusivity(t *testing.T) {
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	nodes := seedLeaseMarket(t, db, s, "container", 1, 4, 0)
	n := nodes[0]
	ins := func(lease int64, kind string, prio int) int64 {
		id, err := s.repo.insertLeaseTaskMax(db, n, lease, kind, prio, cleanupMaxAttempts, LeaseTaskPayload{LeaseID: lease})
		require.NoError(t, err)
		return id
	}
	// 节点上一条长时间运行的 provision(租约 1)。
	run := ins(1, KindProvision, prioProvision)
	db.MustExec(`UPDATE node_tasks SET state='running', attempts=1, locked_until=NOW() + INTERVAL 3 MINUTE WHERE id=?`, run)

	purge := ins(2, KindPurge, prioPurge)
	stop := ins(3, KindStop, prioStop)
	sameLease := ins(1, KindKillSessions, prioKillSessions)

	got, err := s.repo.claimNextFor(s.claimLocks())
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, stop, got.ID, "stop 不受同节点单任务限制")
	got, err = s.repo.claimNextFor(s.claimLocks())
	require.NoError(t, err)
	assert.Nil(t, got, "purge 仍按同节点单任务; 同一租约的任务互斥")
	_ = purge
	_ = sameLease

	// provision 结束后: 同租约 kill_sessions(优先级 1)先于 purge。
	db.MustExec(`UPDATE node_tasks SET state='succeeded', locked_until=NULL WHERE id=?`, run)
	got, err = s.repo.claimNextFor(s.claimLocks())
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, sameLease, got.ID)
	// 运行中的 stop/kill_sessions 不占"节点单任务"名额: purge 可被认领。
	got, err = s.repo.claimNextFor(s.claimLocks())
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.Equal(t, purge, got.ID)
}
