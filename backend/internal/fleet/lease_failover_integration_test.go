package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// T-083: 通用任务 worker + 开通重试耗尽善后(换节点一次 → 再失败取消订单退款)。

// failingProvision 以总是失败的 provision 执行器覆盖 T-084 的真实流水线(只在测试中, 用来驱动死信路径),
// onDead 用生产的 onProvisionDead。
func failingProvision(s *Service, calls *int) {
	s.registerExecutor(KindProvision, taskExecutor{
		lock: 2 * time.Minute,
		run: func(context.Context, *Task) (any, error) {
			*calls++
			return nil, errors.New("节点 docker 不可达")
		},
		onDead: s.onProvisionDead,
	})
}

// runOne 认领并执行一条任务(与 worker 同一代码路径)。
func runOne(t *testing.T, s *Service) *Task {
	t.Helper()
	task, err := s.repo.claimNextFor(s.claimLocks())
	require.NoError(t, err)
	if task == nil {
		return nil
	}
	s.execute(context.Background(), task)
	got, err := s.repo.getTask(s.repo.db, task.ID)
	require.NoError(t, err)
	return got
}

func provisionTaskOf(t *testing.T, db *sqlx.DB, leaseID int64, state string) *Task {
	t.Helper()
	var tasks []Task
	require.NoError(t, db.Select(&tasks, `SELECT `+taskColumns+` FROM node_tasks WHERE lease_id=? AND kind='provision' AND state=?`,
		leaseID, state))
	if len(tasks) == 0 {
		return nil
	}
	require.Len(t, tasks, 1)
	return &tasks[0]
}

func TestRetryBackoff(t *testing.T) {
	assert.Equal(t, time.Minute, retryBackoff(0))
	assert.Equal(t, time.Minute, retryBackoff(1))
	assert.Equal(t, 2*time.Minute, retryBackoff(2))
	assert.Equal(t, 4*time.Minute, retryBackoff(3))
	assert.Equal(t, 32*time.Minute, retryBackoff(6))
	assert.Equal(t, time.Hour, retryBackoff(7))
	assert.Equal(t, time.Hour, retryBackoff(50))
}

func TestWorkerClaimsOnlyRegisteredKinds(t *testing.T) {
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	nodes := seedLeaseMarket(t, db, s, "container", 2, 8, 0)
	// stop/purge 尚无执行器(T-087): 不得被认领消耗。
	_, err := s.repo.insertLeaseTask(db, nodes[0], 0, KindStop, prioStop, LeaseTaskPayload{})
	require.NoError(t, err)
	_, err = s.repo.insertLeaseTask(db, nodes[0], 0, KindPurge, prioPurge, LeaseTaskPayload{})
	require.NoError(t, err)
	task, err := s.repo.claimNextFor(s.claimLocks())
	require.NoError(t, err)
	assert.Nil(t, task)
	assert.Equal(t, 2, countRows(t, db, `SELECT COUNT(*) FROM node_tasks WHERE state='pending' AND attempts=0`))

	// provision 由 NewService 注册(T-084), 按其锁时长认领。
	_, err = s.repo.insertLeaseTask(db, nodes[1], 0, KindProvision, prioProvision, LeaseProvisionPayload{LeaseID: 1})
	require.NoError(t, err)
	task, err = s.repo.claimNextFor(s.claimLocks())
	require.NoError(t, err)
	require.NotNil(t, task)
	assert.Equal(t, KindProvision, task.Kind)
	require.NotNil(t, task.LockedUntil)
	assert.WithinDuration(t, time.Now().Add(provisionLock), *task.LockedUntil, 30*time.Second)
	assert.Equal(t, 2, countRows(t, db, `SELECT COUNT(*) FROM node_tasks WHERE kind IN ('stop','purge') AND state='pending'`))
}

func TestProvisionRetryThenReallocateThenFail(t *testing.T) {
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	nodes := seedLeaseMarket(t, db, s, "container", 2, 8, 1)
	comp := wireLeases(t, s)
	calls := 0
	failingProvision(s, &calls)

	o, err := placeManaged(comp, 21, 4)
	require.NoError(t, err)
	payOrder(t, db, comp, o.OrderNo)
	l, err := s.repo.getLeaseByOrder(db, o.ID)
	require.NoError(t, err)
	first := l.NodeID
	second := nodes[0]
	if second == first {
		second = nodes[1]
	}
	stock, _ := leaseStock(t, db)
	require.Equal(t, 12, stock)

	// 步骤 ⓪ 已执行且已开始建容器(T-084 写入 container_name)的数据面状态。
	db.MustExec(`UPDATE orders SET status='provisioning' WHERE id=?`, o.ID)
	db.MustExec(`UPDATE leases SET state='provisioning', container_name=?, volume_name=?, port_claim=30001, upstream_port=30001 WHERE id=?`,
		containerNameOf(l.LeaseNo), volumeNameOf(l.LeaseNo), l.ID)

	// ① 可重试失败: 指数退避回到 pending(1 分钟后), 未达上限不触发善后。
	got := runOne(t, s)
	require.NotNil(t, got)
	assert.Equal(t, TaskPending, got.State)
	assert.Equal(t, 1, got.Attempts)
	require.NotNil(t, got.LastError)
	assert.Contains(t, *got.LastError, "docker 不可达")
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM node_tasks WHERE id=? AND run_after > NOW() + INTERVAL 50 SECOND`, got.ID))
	assert.Nil(t, runOne(t, s), "退避期内不得再次认领")

	// ② 第 3 次失败 → dead → 换节点重新分配一次。
	db.MustExec(`UPDATE node_tasks SET run_after=NOW(), attempts=2 WHERE id=?`, got.ID)
	got = runOne(t, s)
	require.NotNil(t, got)
	assert.Equal(t, TaskDead, got.State)
	assert.Equal(t, 2, calls)

	l, _ = s.repo.getLease(db, l.ID)
	assert.Equal(t, LeasePending, l.State)
	assert.Equal(t, second, l.NodeID)
	assert.Nil(t, l.ContainerName)
	assert.Nil(t, l.PortClaim)
	assert.Nil(t, l.UpstreamPort)
	var ostatus string
	require.NoError(t, db.Get(&ostatus, `SELECT status FROM orders WHERE id=?`, o.ID))
	assert.Equal(t, "provisioning", ostatus, "换节点期间订单保持开通中")
	// 原节点已建容器 → 卡槽 draining + CleanupOnly purge; 新节点 4 卡 reserved。
	assert.Equal(t, 4, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND node_id=? AND state='draining'`, l.ID, first))
	assert.Equal(t, 4, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND node_id=? AND state='reserved'`, l.ID, second))
	var purgePayload string
	require.NoError(t, db.Get(&purgePayload, `SELECT payload FROM node_tasks WHERE lease_id=? AND node_id=? AND kind='purge'`, l.ID, first))
	var pp LeaseTaskPayload
	require.NoError(t, json.Unmarshal([]byte(purgePayload), &pp))
	assert.True(t, pp.CleanupOnly)
	assert.Equal(t, containerNameOf(l.LeaseNo), pp.ContainerName)
	assert.Equal(t, volumeNameOf(l.LeaseNo), pp.VolumeName)
	next := provisionTaskOf(t, db, l.ID, TaskPending)
	require.NotNil(t, next)
	assert.Equal(t, second, next.NodeID)
	var np LeaseProvisionPayload
	require.NoError(t, jsonUnmarshal(next.Payload, &np))
	assert.True(t, np.Reallocated)
	stock, _ = leaseStock(t, db)
	assert.Equal(t, 8, stock, "原节点 4 卡 draining 待清理 + 新节点 4 卡 reserved")
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM notifications WHERE user_id=20 AND title='托管租约开通失败'`))

	// 死信的开通任务不可手动重试。
	err = s.RetryTask(1, got.ID, "127.0.0.1")
	require.Error(t, err)
	assert.Equal(t, 40900, codeOf(err))

	// ③ 新节点上再次耗尽 → 订单取消(退款由 payment 自动发起)、租约 failed、卡槽释放。
	db.MustExec(`UPDATE node_tasks SET max_attempts=1 WHERE id=?`, next.ID)
	got = runOne(t, s)
	require.NotNil(t, got)
	assert.Equal(t, TaskDead, got.State)
	l, _ = s.repo.getLease(db, l.ID)
	assert.Equal(t, LeaseFailed, l.State)
	require.NotNil(t, l.FailReason)
	assert.Contains(t, *l.FailReason, "docker 不可达")
	var ord struct {
		Status string     `db:"status"`
		Start  *time.Time `db:"lease_start_at"`
	}
	require.NoError(t, db.Get(&ord, `SELECT status, lease_start_at FROM orders WHERE id=?`, o.ID))
	assert.Equal(t, "cancelled", ord.Status)
	assert.Nil(t, ord.Start)
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND node_id=?`, l.ID, second))
	assert.Equal(t, 4, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='draining'`, l.ID),
		"原节点残留仍等 CleanupOnly purge(T-087)清理")
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='provision' AND state='pending'`, l.ID))
	stock, _ = leaseStock(t, db)
	assert.Equal(t, 12, stock)
	assert.Equal(t, realFree(t, db), stock)
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM notifications WHERE user_id=21 AND title='算力开通失败, 将全额退款'`))
	assert.Equal(t, 2, countRows(t, db, `SELECT COUNT(*) FROM notifications WHERE user_id=20 AND title='托管租约开通失败'`))
}

func TestProvisionDeadNoOtherNodeFailsDirectly(t *testing.T) {
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	seedLeaseMarket(t, db, s, "container", 1, 8, 1)
	comp := wireLeases(t, s)
	calls := 0
	failingProvision(s, &calls)
	o, err := placeManaged(comp, 21, 4)
	require.NoError(t, err)
	payOrder(t, db, comp, o.OrderNo)
	l, _ := s.repo.getLeaseByOrder(db, o.ID)
	// 步骤 ⓪ 之前即失败(租约仍 pending, 订单 paid, 未建容器)。
	db.MustExec(`UPDATE node_tasks SET max_attempts=1 WHERE lease_id=?`, l.ID)
	got := runOne(t, s)
	require.NotNil(t, got)
	assert.Equal(t, TaskDead, got.State)
	l, _ = s.repo.getLease(db, l.ID)
	assert.Equal(t, LeaseFailed, l.State)
	var status string
	require.NoError(t, db.Get(&status, `SELECT status FROM orders WHERE id=?`, o.ID))
	assert.Equal(t, "cancelled", status)
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id IS NOT NULL`))
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM node_tasks WHERE kind='purge'`), "未建容器无需清理")
	stock, _ := leaseStock(t, db)
	assert.Equal(t, 8, stock)
}

func TestProvisionDeadAbandonedWhenOrderChanged(t *testing.T) {
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	seedLeaseMarket(t, db, s, "container", 2, 8, 1)
	comp := wireLeases(t, s)
	o, err := placeManaged(comp, 21, 4)
	require.NoError(t, err)
	payOrder(t, db, comp, o.OrderNo)
	l, _ := s.repo.getLeaseByOrder(db, o.ID)
	task := provisionTaskOf(t, db, l.ID, TaskPending)
	require.NotNil(t, task)
	// 买家在死信善后之前取消: 租约已 cancelled, 善后必须放弃, 不得再分配卡槽。
	require.NoError(t, comp.CancelOrder(o.OrderNo))
	s.onProvisionDead(task, "超时")
	l, _ = s.repo.getLease(db, l.ID)
	assert.Equal(t, LeaseCancelled, l.State)
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id IS NOT NULL`))
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM node_tasks WHERE kind='provision' AND state='pending'`))
	stock, _ := leaseStock(t, db)
	assert.Equal(t, 16, stock)
}

// 锁过期回收: 达到上限的转 dead 并只触发一次善后; 陈旧的 worker 回写不重复触发。
func TestReclaimExpiredTriggersDeadOnce(t *testing.T) {
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	seedLeaseMarket(t, db, s, "container", 2, 8, 1)
	comp := wireLeases(t, s)
	calls := 0
	failingProvision(s, &calls)
	o, err := placeManaged(comp, 21, 2)
	require.NoError(t, err)
	payOrder(t, db, comp, o.OrderNo)
	l, _ := s.repo.getLeaseByOrder(db, o.ID)
	task := provisionTaskOf(t, db, l.ID, TaskPending)
	db.MustExec(`UPDATE node_tasks SET state='running', attempts=max_attempts, locked_until=NOW() - INTERVAL 1 SECOND WHERE id=?`, task.ID)

	s.reclaimOnce()
	got, _ := s.repo.getTask(db, task.ID)
	assert.Equal(t, TaskDead, got.State)
	l2, _ := s.repo.getLease(db, l.ID)
	assert.Equal(t, LeasePending, l2.State)
	assert.NotEqual(t, l.NodeID, l2.NodeID)
	s.reclaimOnce()
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='provision' AND state='pending'`, l.ID))

	// 被回收的 worker 迟到的失败回写: 任务已非 running, 不得再次进入死信善后。
	dead, err := s.repo.retryLater(task.ID, task.MaxAttempts, time.Minute, "迟到")
	require.NoError(t, err)
	assert.False(t, dead)
}
