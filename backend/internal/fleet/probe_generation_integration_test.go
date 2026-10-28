package fleet

import (
	"context"
	"sync"
	"testing"

	"tokenfactory/internal/scheduler"
	"tokenfactory/pkg/errcode"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// CR-S1 M4: 节点代次(generation)让停用/恢复/重装确认/改连接信息之前的探活结论作废;
// 完整探活只复用同代次的完整探活, 节点不会停在 verifying。

const genPW = "pw-gen-123456"

// onboardPending 注册节点、引导装公钥、确认指纹(完整探活已入队未执行), 返回节点 id 与任务 id。
func onboardPending(t *testing.T, s *Service, fake *fakeNode, name string, portStart int) (int64, int64) {
	t.Helper()
	v, err := s.RegisterManagedNode(10, scheduler.ManagedNodeReq{ProductID: 1, NodeName: name, TotalCards: 2,
		LeasePortStart: portStart, LeasePortEnd: portStart + 9})
	require.NoError(t, err)
	id := v.(*Node).ID
	_, err = s.UpdateSSH(context.Background(), 10, id, SSHInput{Host: "127.0.0.1", Port: fake.Port(), User: "root",
		BootstrapPassword: genPW}, "")
	require.NoError(t, err)
	taskID, err := s.ConfirmHostKey(10, id, fake.Fingerprint(), "")
	require.NoError(t, err)
	require.NotZero(t, taskID)
	return id, taskID
}

func onboardVerified(t *testing.T, s *Service, fake *fakeNode, name string, portStart int) int64 {
	t.Helper()
	id, _ := onboardPending(t, s, fake, name, portStart)
	done := runTasks(t, s)
	require.Len(t, done, 1)
	require.True(t, outcomeOf(t, done[0]).Pass)
	return id
}

func taskState(t *testing.T, db *sqlx.DB, id int64) string {
	t.Helper()
	var st string
	require.NoError(t, db.Get(&st, `SELECT state FROM node_tasks WHERE id=?`, id))
	return st
}

func verifyStatus(t *testing.T, s *Service, id int64) string {
	t.Helper()
	n, err := s.repo.GetNode(id)
	require.NoError(t, err)
	require.NotNil(t, n)
	return n.VerifyStatus
}

// once 让 fakeNode 的命令钩子只触发一次(钩子注册后常驻)。
func once(fn func()) func() {
	var o sync.Once
	return func() { o.Do(fn) }
}

// 停用发生在完整探活进行中: 在跑任务被置 failed, 结论作废, 节点保持 suspended 且不生成卡槽;
// 恢复后入队的是新任务, 通过后才回到 verified。
func TestSuspendDuringRunningProbeCannotVerify(t *testing.T) {
	db := setupFleetDB(t)
	seedManaged(t, db)
	s := newTestFleet(t, db)
	s.gateway = &stubGateway{}
	fake := newFakeNode(t, genPW)
	id, taskID := onboardPending(t, s, fake, "gpu-gen-1", 47500)

	fake.OnCommand("docker run --rm --name omnis-probe-", once(func() {
		require.NoError(t, s.Suspend(1, id, "运行中停用", ""))
	}))
	task, err := s.repo.claimNext([]string{KindProbe}, probeLock)
	require.NoError(t, err)
	require.Equal(t, taskID, task.ID)
	s.execute(context.Background(), task)

	assert.Equal(t, TaskFailed, taskState(t, db, taskID), "停用把在跑的探活置 failed, worker 回写被忽略")
	assert.Equal(t, VerifySuspended, verifyStatus(t, s, id))
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE node_id=?`, id))
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM node_probe_reports WHERE node_id=? AND result='pass'`, id))
	stock, _ := productStock(t, db)
	assert.Equal(t, 0, stock)

	resumeTask, err := s.Resume(1, id, "")
	require.NoError(t, err)
	assert.NotEqual(t, taskID, resumeTask, "恢复不复用停用前的探活")
	assert.Equal(t, VerifyVerifying, verifyStatus(t, s, id))
	done := runTasks(t, s)
	require.Len(t, done, 1)
	assert.Equal(t, resumeTask, done[0].ID)
	require.True(t, outcomeOf(t, done[0]).Pass)
	assert.Equal(t, VerifyVerified, verifyStatus(t, s, id))
}

// 停用前已认领(running)的旧任务在恢复后才被执行: 代次不一致, 开始前即中止, 不改节点状态;
// 新代次的任务照常通过。
func TestStaleProbeAfterSuspendResumeIsAborted(t *testing.T) {
	db := setupFleetDB(t)
	seedManaged(t, db)
	s := newTestFleet(t, db)
	s.gateway = &stubGateway{}
	fake := newFakeNode(t, genPW)
	id, taskID := onboardPending(t, s, fake, "gpu-gen-2", 47520)

	stale, err := s.repo.claimNext([]string{KindProbe}, probeLock)
	require.NoError(t, err)
	require.Equal(t, taskID, stale.ID)
	require.NoError(t, s.Suspend(1, id, "", ""))
	assert.Equal(t, TaskFailed, taskState(t, db, taskID))
	fresh, err := s.Resume(1, id, "")
	require.NoError(t, err)
	require.NotEqual(t, taskID, fresh)

	before := len(fake.Commands())
	s.execute(context.Background(), stale) // 旧 worker 迟到执行
	assert.Len(t, fake.Commands(), before, "旧代次的探活不得连接节点执行命令")
	assert.Equal(t, VerifyVerifying, verifyStatus(t, s, id), "旧任务中止不得回退新代次的 verifying")
	assert.Equal(t, TaskPending, taskState(t, db, fresh))

	done := runTasks(t, s)
	require.Len(t, done, 1)
	require.True(t, outcomeOf(t, done[0]).Pass)
	assert.Equal(t, VerifyVerified, verifyStatus(t, s, id))
}

// markVerified 不再接受 failed / needs_reimage: 探活进行中节点被置为这两种状态, 通过结论作废。
func TestMarkVerifiedRejectsFailedAndNeedsReimage(t *testing.T) {
	for _, st := range []string{VerifyFailed, VerifyNeedsReimage} {
		t.Run(st, func(t *testing.T) {
			db := setupFleetDB(t)
			seedManaged(t, db)
			s := newTestFleet(t, db)
			s.gateway = &stubGateway{}
			fake := newFakeNode(t, genPW)
			id, _ := onboardPending(t, s, fake, "gpu-gen-3", 47540)
			fake.OnCommand("docker run --rm --name omnis-probe-", once(func() {
				db.MustExec(`UPDATE supplier_nodes SET verify_status=? WHERE id=?`, st, id)
			}))
			done := runTasks(t, s)
			require.Len(t, done, 1)
			o := outcomeOf(t, done[0])
			assert.True(t, o.Discarded)
			assert.False(t, o.Pass)
			assert.Equal(t, st, verifyStatus(t, s, id))
			assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE node_id=?`, id))
		})
	}
}

// 重装确认走 needs_reimage → verifying(重置指纹 + 代次 +1): 旧系统上的在跑探活结论作废, 新系统须重新确认指纹后才 verified。
func TestReimageConfirmGoesThroughVerifying(t *testing.T) {
	db := setupFleetDB(t)
	seedManaged(t, db)
	s := newTestFleet(t, db)
	s.gateway = &stubGateway{}
	fake := newFakeNode(t, genPW)
	id := onboardVerified(t, s, fake, "gpu-gen-4", 47560)
	db.MustExec(`UPDATE supplier_nodes SET verify_status='needs_reimage', host_fp_claim=NULL WHERE id=?`, id)
	db.MustExec(`DELETE FROM gpu_slots WHERE node_id=?`, id)

	taskID, err := s.ReimageConfirm(10, id, "")
	require.NoError(t, err)
	require.NotZero(t, taskID)
	assert.Equal(t, VerifyVerifying, verifyStatus(t, s, id))
	done := runTasks(t, s)
	require.Len(t, done, 1)
	o := outcomeOf(t, done[0])
	assert.NotEmpty(t, o.AwaitingHostKey, "重装后须重新确认指纹")
	assert.Equal(t, VerifyUnverified, verifyStatus(t, s, id))
	_, err = s.ConfirmHostKey(10, id, fake.Fingerprint(), "")
	require.NoError(t, err)
	done = runTasks(t, s)
	require.Len(t, done, 1)
	require.True(t, outcomeOf(t, done[0]).Pass)
	assert.Equal(t, VerifyVerified, verifyStatus(t, s, id))
}

// 排队中的 port_recheck 不能顶替完整探活: 请求完整探活时新入队完整探活并作废排队复检,
// 在跑的复检保留, 完整探活排在其后; 节点最终 verified 而不是停在 verifying。
func TestFullProbeNotSatisfiedByPortRecheck(t *testing.T) {
	db := setupFleetDB(t)
	seedManaged(t, db)
	s := newTestFleet(t, db)
	s.gateway = &stubGateway{}
	fake := newFakeNode(t, genPW)
	id := onboardVerified(t, s, fake, "gpu-gen-5", 47580)

	enqueue := func(mode string) int64 {
		var tid int64
		require.NoError(t, s.inTx(func(tx *sqlx.Tx) error {
			n, err := s.repo.lockNode(tx, id)
			if err != nil {
				return err
			}
			tid, err = s.enqueueProbeTx(tx, n, mode, "periodic", 8)
			return err
		}))
		return tid
	}

	// 1) 排队中的复检被完整探活取代。
	recheck := enqueue(ProbePortRecheck)
	assert.Equal(t, recheck, enqueue(ProbePortRecheck), "复检请求复用排队复检")
	db.MustExec(`UPDATE supplier_nodes SET verify_status='failed' WHERE id=?`, id)
	full, err := s.Verify(10, id)
	require.NoError(t, err)
	assert.NotEqual(t, recheck, full)
	assert.Equal(t, TaskFailed, taskState(t, db, recheck))
	assert.Equal(t, VerifyVerifying, verifyStatus(t, s, id))
	again, err := s.Verify(10, id)
	require.NoError(t, err)
	assert.Equal(t, full, again, "进行中重复提交复用同一完整探活")
	assert.Equal(t, full, enqueue(ProbePortRecheck), "复检请求可由完整探活覆盖")
	done := runTasks(t, s)
	require.Len(t, done, 1)
	require.True(t, outcomeOf(t, done[0]).Pass)
	assert.Equal(t, VerifyVerified, verifyStatus(t, s, id))

	// 2) 在跑的复检: 完整探活另行入队, 排在复检之后执行。
	recheck = enqueue(ProbePortRecheck)
	running, err := s.repo.claimNext([]string{KindProbe}, probeLock)
	require.NoError(t, err)
	require.Equal(t, recheck, running.ID)
	db.MustExec(`UPDATE supplier_nodes SET verify_status='failed' WHERE id=?`, id)
	full, err = s.Verify(10, id)
	require.NoError(t, err)
	assert.NotEqual(t, recheck, full)
	assert.Equal(t, TaskRunning, taskState(t, db, recheck))
	assert.Equal(t, VerifyVerifying, verifyStatus(t, s, id))
	none, err := s.repo.claimNext([]string{KindProbe}, probeLock)
	require.NoError(t, err)
	assert.Nil(t, none, "同一节点串行: 复检未结束前不认领完整探活")
	s.execute(context.Background(), running)
	done = runTasks(t, s)
	require.Len(t, done, 1)
	assert.Equal(t, full, done[0].ID)
	require.True(t, outcomeOf(t, done[0]).Pass)
	assert.Equal(t, VerifyVerified, verifyStatus(t, s, id))
}

// 完整探活中止(平台侧缺凭证)或进入死信: verifying → failed 并通知, 不会永远停在 verifying。
func TestAbortedOrDeadFullProbeLeavesVerifying(t *testing.T) {
	db := setupFleetDB(t)
	seedManaged(t, db)
	s := newTestFleet(t, db)
	s.gateway = &stubGateway{}
	fake := newFakeNode(t, genPW)
	id, _ := onboardPending(t, s, fake, "gpu-gen-6", 47600)
	assert.Equal(t, VerifyVerifying, verifyStatus(t, s, id))

	key := s.credKey
	s.credKey = nil
	done := runTasks(t, s)
	require.Len(t, done, 1)
	assert.Equal(t, TaskFailed, done[0].State)
	assert.Equal(t, VerifyFailed, verifyStatus(t, s, id))
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM notifications WHERE user_id=10 AND title='节点探活未能完成'`))

	// 死信: 重试耗尽后同样回退。
	s.credKey = key
	full, err := s.Verify(10, id)
	require.NoError(t, err)
	assert.Equal(t, VerifyVerifying, verifyStatus(t, s, id))
	db.MustExec(`UPDATE node_tasks SET max_attempts=1 WHERE id=?`, full)
	task, err := s.repo.claimNext([]string{KindProbe}, probeLock)
	require.NoError(t, err)
	require.Equal(t, full, task.ID)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // 服务停机中断 → 可重试错误; attempts 已达上限 → dead
	s.execute(ctx, task)
	assert.Equal(t, TaskDead, taskState(t, db, full))
	assert.Equal(t, VerifyFailed, verifyStatus(t, s, id))
}

// 运营重试死信探活: payload 按当前代次重写, 完整探活把节点置 verifying, 执行通过; 停用节点 40900。
func TestRetryDeadProbeUsesCurrentGeneration(t *testing.T) {
	db := setupFleetDB(t)
	seedManaged(t, db)
	s := newTestFleet(t, db)
	s.gateway = &stubGateway{}
	fake := newFakeNode(t, genPW)
	id := onboardVerified(t, s, fake, "gpu-gen-7", 47620)
	db.MustExec(`UPDATE supplier_nodes SET verify_status='failed' WHERE id=?`, id)
	res, err := db.Exec(`INSERT INTO node_tasks (node_id, kind, state, priority, attempts, payload, last_error)
		VALUES (?, 'probe', 'dead', 5, 3, '{"mode":"full","trigger":"supplier","generation":0}', 'boom')`, id)
	require.NoError(t, err)
	dead, _ := res.LastInsertId()
	require.NoError(t, s.RetryTask(1, dead, ""))
	assert.Equal(t, VerifyVerifying, verifyStatus(t, s, id))
	done := runTasks(t, s)
	require.Len(t, done, 1)
	require.True(t, outcomeOf(t, done[0]).Pass)
	assert.Equal(t, VerifyVerified, verifyStatus(t, s, id))

	require.NoError(t, s.Suspend(1, id, "", ""))
	res, err = db.Exec(`INSERT INTO node_tasks (node_id, kind, state, priority, attempts, payload, last_error)
		VALUES (?, 'probe', 'dead', 5, 3, '{"mode":"full","trigger":"supplier"}', 'boom')`, id)
	require.NoError(t, err)
	dead, _ = res.LastInsertId()
	assert.Equal(t, errcode.Conflict, codeOf(s.RetryTask(1, dead, "")))
}
