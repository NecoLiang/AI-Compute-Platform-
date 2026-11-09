package fleet

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CR-S1 Minor: worker 崩溃留下的 running 探活不能把重新探活挡 45 分钟。任务锁改为短租约 + 心跳续期。

// worker 认领后崩溃(不执行、不续期): 锁只有 taskLeaseTTL; 过期后由回收转回 pending, 重新发起探活复用同一任务并能执行通过。
func TestOrphanedProbeReclaimedWithinLeaseTTL(t *testing.T) {
	db := setupFleetDB(t)
	seedManaged(t, db)
	s := newTestFleet(t, db)
	s.gateway = &stubGateway{}
	fake := newFakeNode(t, genPW)
	id, taskID := onboardPending(t, s, fake, "gpu-lease-1", 47640)

	orphan, err := s.repo.claimNextFor(s.claimLocks())
	require.NoError(t, err)
	require.NotNil(t, orphan)
	require.Equal(t, taskID, orphan.ID)
	require.NotNil(t, orphan.LockedUntil)
	assert.WithinDuration(t, time.Now().Add(taskLeaseTTL), *orphan.LockedUntil, 30*time.Second,
		"认领时只锁一个短租约, 不再一次锁 45 分钟")

	// 租约内重复发起: 复用同一任务, 节点保持 verifying。
	again, err := s.Verify(10, id)
	require.NoError(t, err)
	assert.Equal(t, taskID, again)

	// 模拟 TTL 已过(worker 已崩溃, 无心跳): 回收转回 pending。
	db.MustExec(`UPDATE node_tasks SET locked_until = NOW() - INTERVAL 1 SECOND WHERE id=?`, taskID)
	s.reclaimOnce()
	assert.Equal(t, TaskPending, taskState(t, db, taskID))
	ok, err := s.repo.renewLock(taskID, orphan.Attempts, taskLeaseTTL)
	require.NoError(t, err)
	assert.False(t, ok, "被回收后旧 worker 的心跳续期失败")

	again, err = s.Verify(10, id)
	require.NoError(t, err)
	assert.Equal(t, taskID, again, "回收后的同代次完整探活继续负责本次请求")
	db.MustExec(`UPDATE node_tasks SET run_after = NOW() WHERE id=?`, taskID) // 跳过退避等待
	done := runTasks(t, s)
	require.Len(t, done, 1)
	assert.Equal(t, taskID, done[0].ID)
	assert.Equal(t, 2, done[0].Attempts)
	require.True(t, outcomeOf(t, done[0]).Pass)
	assert.Equal(t, VerifyVerified, verifyStatus(t, s, id))
}

// 执行时间超过租约 TTL 的探活靠心跳续期保持持有, 期间的回收不会把它转回 pending。
func TestHeartbeatKeepsLongProbeAlive(t *testing.T) {
	db := setupFleetDB(t)
	seedManaged(t, db)
	s := newTestFleet(t, db)
	s.gateway = &stubGateway{}
	// TTL 4s / 心跳 250ms: 锁按秒精度写入, 留足余量, 并行跑包时 DB 偶发变慢也不误判。
	s.taskTTL, s.taskBeat = 4*time.Second, 250*time.Millisecond
	fake := newFakeNode(t, genPW)
	id, taskID := onboardPending(t, s, fake, "gpu-lease-2", 47660)

	fake.OnCommand("docker run --rm --name omnis-probe-", once(func() {
		time.Sleep(5500 * time.Millisecond) // 超过 TTL
		s.reclaimOnce()
	}))
	task, err := s.repo.claimNextFor(s.claimLocks())
	require.NoError(t, err)
	require.Equal(t, taskID, task.ID)
	s.execute(context.Background(), task)

	got, err := s.repo.getTask(db, taskID)
	require.NoError(t, err)
	assert.Equal(t, 1, got.Attempts, "心跳续期期间不得被回收重跑")
	require.True(t, outcomeOf(t, *got).Pass)
	assert.Equal(t, VerifyVerified, verifyStatus(t, s, id))
}

// 任务在执行中被作废(停用节点): 下一次心跳续期命中 0 行即取消执行, 不必等到单次执行上限。
func TestLostLeaseCancelsRunningProbe(t *testing.T) {
	db := setupFleetDB(t)
	seedManaged(t, db)
	s := newTestFleet(t, db)
	s.gateway = &stubGateway{}
	s.taskTTL, s.taskBeat = 3*time.Second, 300*time.Millisecond
	fake := newFakeNode(t, genPW)
	id, taskID := onboardPending(t, s, fake, "gpu-lease-3", 47680)

	fake.OnCommand("docker run --rm --name omnis-probe-", once(func() {
		require.NoError(t, s.Suspend(1, id, "执行中停用", ""))
		time.Sleep(6 * time.Second) // 节点侧命令迟迟不返回
	}))
	task, err := s.repo.claimNextFor(s.claimLocks())
	require.NoError(t, err)
	require.Equal(t, taskID, task.ID)
	start := time.Now()
	s.execute(context.Background(), task)
	assert.Less(t, time.Since(start), 5*time.Second, "失去任务后应由心跳取消执行")

	assert.Equal(t, TaskFailed, taskState(t, db, taskID))
	assert.Equal(t, VerifySuspended, verifyStatus(t, s, id))
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE node_id=?`, id))
}
