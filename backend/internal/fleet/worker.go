package fleet

import (
	"context"
	"errors"
	"log/slog"
	"time"
)

// 任务队列 worker(docs/25 §25.9/§25.11, T-083): 多 worker 并发, 同一节点串行(由 claimNextFor 保证)。
// 只认领已注册执行器的任务类型; 尚无执行器的类型(stop/purge/kill_sessions 属 T-087)保持 pending 不被消耗。

const (
	probeLock     = 45 * time.Minute // 完整探活含拉镜像(≤30min)+压测, 锁要覆盖最坏耗时
	provisionLock = 45 * time.Minute // 开通: 镜像缺失时需拉取(≤30min) + 建容器 + 网关自检
	pollInterval  = 2 * time.Second
	retryBase     = time.Minute
	retryCap      = time.Hour
)

// errTaskAborted 任务不可执行且重试无意义(节点已删除/停用、租约已取消等): 任务置 failed, 不重试。
type errTaskAborted struct{ msg string }

func (e errTaskAborted) Error() string { return e.msg }

// taskExecutor 一种任务类型的执行器。run 返回 (结果, 错误): errTaskAborted/errProbeAborted → failed;
// 其他错误 → 指数退避重试, 达到 max_attempts → dead 并调用 onDead(若有)。
type taskExecutor struct {
	lock   time.Duration
	run    func(ctx context.Context, t *Task) (any, error)
	onDead func(t *Task, lastErr string)
}

// executors 已接入的任务类型。provision 由 T-084 注册(registerExecutor)。
func (s *Service) executorFor(kind string) (taskExecutor, bool) {
	if kind == KindProbe {
		return taskExecutor{lock: probeLock, run: s.runProbeTask}, true
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

// claimLocks 各可执行类型的锁时长(认领时按任务类型设置 locked_until)。
func (s *Service) claimLocks() map[string]time.Duration {
	out := map[string]time.Duration{KindProbe: probeLock}
	for k, e := range s.extraExecutors {
		out[k] = e.lock
	}
	return out
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
	rctx, cancel := context.WithTimeout(ctx, e.lock-time.Minute)
	defer cancel()
	out, err := e.run(rctx, t)
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
	if err != nil {
		return nil, err
	}
	slog.Info("fleet 探活完成", "task_id", t.ID, "node_id", t.NodeID, "mode", out.Mode, "pass", out.Pass,
		"failed", out.Failed, "awaiting_host_key", out.AwaitingHostKey != "")
	return out, nil
}

func (s *Service) finish(t *Task, state string, result any, lastErr string) {
	ok, err := s.repo.finishTask(t.ID, t.Attempts, state, result, lastErr)
	if err != nil {
		slog.Error("fleet 任务回写失败", "task_id", t.ID, "error", err)
	} else if !ok {
		slog.Warn("fleet 任务回写被忽略(锁已过期被回收)", "task_id", t.ID)
	}
}

// reclaimLoop 每分钟回收锁过期的 running 任务(worker 崩溃/重启), 转 dead 的走死信善后。
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
