package fleet

import (
	"context"

	"errors"
	"log/slog"
	"time"
)

// 任务队列 worker(docs/25 §25.9): 多 worker 并发, 同一节点串行(由 claimNext 保证)。S1 只执行 kind=probe。

const (
	probeLock    = 45 * time.Minute // 完整探活含拉镜像(≤30min)+压测, 锁要覆盖最坏耗时
	pollInterval = 2 * time.Second
	retryDelay   = time.Minute
)

// RunWorkers 启动 cfg.Workers 个 worker 与锁过期回收; ctx 取消即退出(进行中的探活随 ctx 中止并按可重试失败处理)。
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
		t, err := s.repo.claimNext([]string{KindProbe}, probeLock)
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
	var pl ProbePayload
	if err := jsonUnmarshal(t.Payload, &pl); err != nil {
		s.finish(t, TaskFailed, nil, "payload 无法解析: "+err.Error())
		return
	}
	start := time.Now()
	pctx, cancel := context.WithTimeout(ctx, probeLock-time.Minute)
	defer cancel()
	out, err := s.runProbe(pctx, t.NodeID, pl)
	var aborted errProbeAborted
	switch {
	case errors.As(err, &aborted):
		s.finish(t, TaskFailed, nil, aborted.msg)
	case err != nil:
		dead, rerr := s.repo.retryLater(t.ID, t.Attempts, retryDelay, err.Error())
		if rerr != nil {
			slog.Error("fleet 任务回写失败", "task_id", t.ID, "error", rerr)
			return
		}
		if dead {
			slog.Error("运营告警: fleet 任务进入死信", "task_id", t.ID, "node_id", t.NodeID, "kind", t.Kind, "error", err)
		} else {
			slog.Warn("fleet 任务失败, 稍后重试", "task_id", t.ID, "node_id", t.NodeID, "attempt", t.Attempts, "error", err)
		}
	default:
		slog.Info("fleet 探活完成", "task_id", t.ID, "node_id", t.NodeID, "mode", out.Mode, "pass", out.Pass,
			"failed", out.Failed, "awaiting_host_key", out.AwaitingHostKey != "", "elapsed", time.Since(start).Round(time.Second))
		s.finish(t, TaskSucceeded, out, "")
	}
}

func (s *Service) finish(t *Task, state string, result any, lastErr string) {
	ok, err := s.repo.finishTask(t.ID, t.Attempts, state, result, lastErr)
	if err != nil {
		slog.Error("fleet 任务回写失败", "task_id", t.ID, "error", err)
	} else if !ok {
		slog.Warn("fleet 任务回写被忽略(锁已过期被回收)", "task_id", t.ID)
	}
}

// reclaimLoop 每分钟回收锁过期的 running 任务(worker 崩溃/重启), 转 dead 的发运营告警。
func (s *Service) reclaimLoop(ctx context.Context) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		dead, err := s.repo.reclaimExpired()
		if err != nil {
			slog.Error("fleet 回收过期任务失败", "error", err)
		}
		for _, d := range dead {
			slog.Error("运营告警: fleet 任务执行超时进入死信", "task_id", d.ID, "node_id", d.NodeID, "kind", d.Kind)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
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
