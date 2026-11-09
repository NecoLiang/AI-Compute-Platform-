package fleet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"
)

// 任务队列 worker(docs/25 §25.9/§25.11, T-083): 多 worker 并发, 同一节点串行(由 claimNextFor 保证)。
// 只认领已注册执行器的任务类型; 尚无执行器的类型(stop/purge/kill_sessions 属 T-087)保持 pending 不被消耗。

const (
	probeTimeout     = 44 * time.Minute // 完整探活单次执行上限: 拉镜像(≤30min)+压测
	provisionTimeout = 44 * time.Minute // 开通单次执行上限: 镜像缺失时需拉取(≤30min) + 建容器 + 网关自检
	// 任务锁是短租约 + 心跳续期(CR-S1): worker 存活时每 TTL/3 续期; worker 崩溃后锁在 TTL 内过期,
	// 由 reclaimLoop(每分钟)回收, 同一节点的后续任务(含重新发起的探活)不再被孤儿 running 任务挡住 45 分钟。
	taskLeaseTTL  = 3 * time.Minute
	taskHeartbeat = time.Minute
	pollInterval  = 2 * time.Second
	retryBase     = time.Minute
	retryCap      = time.Hour
)

// errTaskAborted 任务不可执行且重试无意义(节点已删除/停用、租约已取消等): 任务置 failed, 不重试。
type errTaskAborted struct{ msg string }

func (e errTaskAborted) Error() string { return e.msg }

// taskExecutor 一种任务类型的执行器。timeout 为单次执行上限(与锁租约无关)。run 返回 (结果, 错误): errTaskAborted/errProbeAborted → failed;
// 其他错误 → 指数退避重试, 达到 max_attempts → dead 并调用 onDead(若有)。
type taskExecutor struct {
	timeout time.Duration
	run     func(ctx context.Context, t *Task) (any, error)
	onDead  func(t *Task, lastErr string)
}

// executors 已接入的任务类型。provision 由 T-084 注册(registerExecutor)。
func (s *Service) executorFor(kind string) (taskExecutor, bool) {
	if kind == KindProbe {
		return taskExecutor{timeout: probeTimeout, run: s.runProbeTask, onDead: s.probeDead}, true
	}
	e, ok := s.extraExecutors[kind]
	return e, ok
}

func (s *Service) registerExecutor(kind string, e taskExecutor) {
	if s.extraExecutors == nil {
		s.extraExecutors = map[string]taskExecutor{}
	}
	s.extraExecutors[kind] = e
}

// claimLocks 可认领的任务类型 → 认领时的锁租约(locked_until = NOW()+TTL, 执行期间心跳续期)。
func (s *Service) claimLocks() map[string]time.Duration {
	ttl := s.leaseTTL()
	out := map[string]time.Duration{KindProbe: ttl}
	for k := range s.extraExecutors {
		out[k] = ttl
	}
	return out
}

func (s *Service) leaseTTL() time.Duration {
	if s.taskTTL > 0 {
		return s.taskTTL
	}
	return taskLeaseTTL
}

func (s *Service) heartbeatEvery() time.Duration {
	if s.taskBeat > 0 {
		return s.taskBeat
	}
	return taskHeartbeat
}

// heartbeat 执行期间按间隔续期任务锁。续期命中 0 行(任务已被停用/改连接信息作废, 或锁过期被回收)→ 立即取消执行;
// 续期持续出错且距上次成功已接近 TTL → 也取消(锁即将过期, 避免与回收后重新认领的执行并发)。
func (s *Service) heartbeat(ctx context.Context, cancelRun context.CancelFunc, t *Task) {
	ttl, every := s.leaseTTL(), s.heartbeatEvery()
	tk := time.NewTicker(every)
	defer tk.Stop()
	lastOK := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tk.C:
		}
		ok, err := s.repo.renewLock(t.ID, t.Attempts, ttl)
		switch {
		case err != nil:
			slog.Warn("fleet 任务锁续期失败", "task_id", t.ID, "error", err)
			if time.Since(lastOK) >= ttl-every {
				slog.Error("fleet 任务锁续期持续失败, 中止执行以免与回收后的重跑并发", "task_id", t.ID, "node_id", t.NodeID)
				cancelRun()
				return
			}
		case !ok:
			slog.Warn("fleet 任务已不由本 worker 持有(被作废或锁过期回收), 中止执行", "task_id", t.ID, "node_id", t.NodeID, "kind", t.Kind)
			cancelRun()
			return
		default:
			lastOK = time.Now()
		}
	}
}

// retryBackoff 第 attempts 次失败后的等待: 1m × 2^(attempts−1), 上限 1h。
func retryBackoff(attempts int) time.Duration {
	if attempts < 1 {
		attempts = 1
	}
	d := retryBase
	for i := 1; i < attempts; i++ {
		d *= 2
		if d >= retryCap {
			return retryCap
		}
	}
	return d
}

// RunWorkers 启动 cfg.Workers 个 worker 与锁过期回收; ctx 取消即退出(进行中的任务随 ctx 中止并按可重试失败处理)。
func (s *Service) RunWorkers(ctx context.Context) {
	n := s.cfg.Workers
	if n <= 0 {
		n = 8
	}
	for i := 0; i < n; i++ {
		go s.workerLoop(ctx, i)
	}
	go s.reclaimLoop(ctx)
}

func (s *Service) workerLoop(ctx context.Context, id int) {
	for {
		if ctx.Err() != nil {
			return
		}
		t, err := s.repo.claimNextFor(s.claimLocks())
		if err != nil {
			slog.Error("fleet worker 认领任务失败", "worker", id, "error", err)
			sleepCtx(ctx, 5*time.Second)
			continue
		}
		if t == nil {
			sleepCtx(ctx, pollInterval)
			continue
		}
		s.execute(ctx, t)
	}
}

// execute 执行一条已认领的任务并写回结果。
func (s *Service) execute(ctx context.Context, t *Task) {
	e, ok := s.executorFor(t.Kind)
	if !ok {
		// 认领时已按类型过滤, 正常不会走到这里; 防御性置 failed 并写明原因。
		s.finish(t, TaskFailed, nil, "该任务类型尚无执行器: "+t.Kind)
		return
	}
	start := time.Now()
	rctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	hbCtx, stopHB := context.WithCancel(rctx)
	hbDone := make(chan struct{})
	go func() {
		defer close(hbDone)
		s.heartbeat(hbCtx, cancel, t)
	}()
	out, err := e.run(rctx, t)
	stopHB()
	<-hbDone
	var aborted errTaskAborted
	var probeAborted errProbeAborted
	switch {
	case errors.As(err, &aborted):
		s.finish(t, TaskFailed, nil, aborted.msg)
	case errors.As(err, &probeAborted):
		s.finish(t, TaskFailed, nil, probeAborted.msg)
	case err != nil:
		dead, rerr := s.repo.retryLater(t.ID, t.Attempts, retryBackoff(t.Attempts), err.Error())
		if rerr != nil {
			slog.Error("fleet 任务回写失败", "task_id", t.ID, "error", rerr)
			return
		}
		if dead {
			s.taskDead(t, err.Error())
		} else {
			slog.Warn("fleet 任务失败, 稍后重试", "task_id", t.ID, "node_id", t.NodeID, "kind", t.Kind,
				"attempt", t.Attempts, "retry_in", retryBackoff(t.Attempts), "error", err)
		}
	default:
		slog.Info("fleet 任务完成", "task_id", t.ID, "node_id", t.NodeID, "kind", t.Kind, "lease_id", taskLeaseID(t),
			"elapsed", time.Since(start).Round(time.Second))
		s.finish(t, TaskSucceeded, out, "")
	}
}

// taskLeaseID 日志用: 无租约的任务记 0(直接记 *int64 会打印成指针地址)。
func taskLeaseID(t *Task) int64 {
	if t.LeaseID == nil {
		return 0
	}
	return *t.LeaseID
}

// taskDead 任务进入死信: 运营告警 + 类型自定义的善后(provision 换节点/退款)。
func (s *Service) taskDead(t *Task, lastErr string) {
	slog.Error("运营告警: fleet 任务进入死信", "task_id", t.ID, "node_id", t.NodeID, "kind", t.Kind,
		"lease_id", taskLeaseID(t), "attempts", t.Attempts, "error", lastErr)
	if e, ok := s.executorFor(t.Kind); ok && e.onDead != nil {
		e.onDead(t, lastErr)
	}
}

// runProbeTask probe 任务执行器(S1 逻辑不变)。
func (s *Service) runProbeTask(ctx context.Context, t *Task) (any, error) {
	var pl ProbePayload
	if err := jsonUnmarshal(t.Payload, &pl); err != nil {
		return nil, errTaskAborted{"payload 无法解析: " + err.Error()}
	}
	out, err := s.runProbe(ctx, t.NodeID, pl)
	var aborted errProbeAborted
	if errors.As(err, &aborted) && !aborted.stale && pl.Mode != ProbePortRecheck {
		s.abandonVerifying(t, pl.Generation, aborted.msg)
	}
	if err != nil {
		return nil, err
	}
	slog.Info("fleet 探活完成", "task_id", t.ID, "node_id", t.NodeID, "mode", out.Mode, "pass", out.Pass,
		"failed", out.Failed, "awaiting_host_key", out.AwaitingHostKey != "")
	return out, nil
}

// probeDead 探活重试耗尽: 本代次的完整探活不会再执行, 节点不能停在 verifying。
func (s *Service) probeDead(t *Task, lastErr string) {
	var pl ProbePayload
	if err := jsonUnmarshal(t.Payload, &pl); err != nil || pl.Mode == ProbePortRecheck {
		return
	}
	s.abandonVerifying(t, pl.Generation, "探活多次执行失败")
}

// abandonVerifying 本代次的完整探活未能完成(中止/死信): verifying → failed 并通知供给方重新发起。
// 代次守卫保证不会覆盖之后的停用/恢复/改连接信息等操作; 节点还有其他未结束的探活时不动(由它结束 verifying)。
// 对供给方只给通用提示, 细节在日志与任务 last_error。
func (s *Service) abandonVerifying(t *Task, gen int, reason string) {
	nodeID := t.NodeID
	ok, err := s.repo.abandonVerifying(nodeID, gen, t.ID)
	if err != nil {
		slog.Error("fleet 回退 verifying 失败", "node_id", nodeID, "error", err)
		return
	}
	if !ok {
		return
	}
	slog.Warn("fleet 完整探活未能完成, 节点 verifying → failed", "node_id", nodeID, "reason", reason)
	n, err := s.repo.GetNode(nodeID)
	if err != nil || n == nil {
		return
	}
	s.refreshHealth(n)
	s.notify(n.SupplierID, "节点探活未能完成",
		fmt.Sprintf("节点「%s」的探活未能完成, 请稍后在节点页重新发起探活; 如持续失败请联系平台运营。", n.NodeName), nodeID)
}

func (s *Service) finish(t *Task, state string, result any, lastErr string) {
	ok, err := s.repo.finishTask(t.ID, t.Attempts, state, result, lastErr)
	if err != nil {
		slog.Error("fleet 任务回写失败", "task_id", t.ID, "error", err)
	} else if !ok {
		slog.Warn("fleet 任务回写被忽略(锁已过期被回收)", "task_id", t.ID)
	}
}

// reclaimLoop 每分钟回收锁过期的 running 任务(worker 崩溃/重启: 心跳停止后锁在 TTL 内过期), 转 dead 的走死信善后。
func (s *Service) reclaimLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		s.reclaimOnce()
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) reclaimOnce() {
	dead, err := s.repo.reclaimExpired()
	if err != nil {
		slog.Error("fleet 回收过期任务失败", "error", err)
	}
	for i := range dead {
		s.taskDead(&dead[i], "执行超时(锁过期)")
	}
}

func sleepCtx(ctx context.Context, d time.Duration) {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
	case <-t.C:
	}
}
