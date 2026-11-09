package modeldeploy

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

// 健康检测轮(docs/27 §27.5): 每 60s 一轮, 覆盖 starting/restarting/running。
//   - starting/restarting: 判定"是否就绪"(L0–L3 全过 → running), 容器退出/启动超时 → 失败或按退避重启;
//   - running: 写采样 + 判定 + 自动处置(§27.5.3);
//   - 节点不可达: running 记 unknown(node_unreachable), 不写采样、不计失败、不重启;
//   - restarting 且退避到期: 下发自动重启(deploy_model)。
// 每轮还会跟踪进行中的权重拉取(PollPulls)。

const (
	HealthInterval    = 60 * time.Second
	healthConcurrency = 8
	healthCheckBudget = 50 * time.Second
	failLogTail       = 200
	baselineWindow    = time.Hour
)

var monitoredStates = []string{DepStarting, DepRunning, DepRestarting}

// HealthRound 一轮健康检测。fleet 未接入(rt=nil)时什么也不做。
func (s *Service) HealthRound(ctx context.Context) error {
	if s == nil || s.rt == nil {
		return nil
	}
	if err := s.PollPulls(ctx); err != nil {
		slog.Warn("跟踪模型拉取失败", "error", err)
	}
	list, err := listDeploymentsInStates(s.repo.db, monitoredStates)
	if err != nil {
		return err
	}
	sem := make(chan struct{}, healthConcurrency)
	var wg sync.WaitGroup
	for i := range list {
		d := &list[i]
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			cctx, cancel := context.WithTimeout(ctx, healthCheckBudget)
			defer cancel()
			if err := s.checkOne(cctx, d); err != nil {
				slog.Warn("部署健康检测失败", "deploy_no", d.DeployNo, "error", err)
			}
		}()
	}
	wg.Wait()
	return nil
}

func (s *Service) checkOne(ctx context.Context, d *Deployment) error {
	now := s.now()
	if d.State == DepRestarting && d.NextRestartAt != nil {
		if !now.Before(*d.NextRestartAt) {
			return s.triggerAutoRestart(ctx, d)
		}
		return nil
	}
	// 部署任务仍在执行(拉取/启动/重启下发中): 不检测, 避免与流水线抢状态。
	busy, err := taskInFlight(s.repo.db, d.ID, TaskDeployModel)
	if err != nil || busy {
		return err
	}
	if d.EnginePort == nil {
		return nil
	}
	e, err := s.repo.GetEntry(d.ModelID)
	if err != nil {
		return err
	}
	if e == nil {
		return fmt.Errorf("模型目录条目 %d 不存在", d.ModelID)
	}
	mm := map[string]string{}
	if len(e.MetricsMap) > 0 {
		if err := json.Unmarshal(e.MetricsMap, &mm); err != nil {
			return fmt.Errorf("metrics_map 解析失败: %w", err)
		}
	}
	cmd, err := probeCommand(probeSpec{DeployNo: d.DeployNo, Port: *d.EnginePort, ModelKey: e.ModelKey,
		Canary: d.CanaryEnabled, MetricsMap: mm})
	if err != nil {
		return err
	}
	sh, err := s.rt.Dial(ctx, d.NodeID)
	if err != nil {
		return s.markUnreachable(d, err)
	}
	defer sh.Close()
	r, err := run(ctx, sh, cmd)
	if err != nil {
		return s.markUnreachable(d, err)
	}
	obs, err := parseProbe(r.Stdout, now, d.CanaryEnabled, mm)
	if err != nil {
		return s.markUnreachable(d, err)
	}
	if obs.Cards, err = s.rt.CardHealth(ctx, d.NodeID, d.GPUUUIDs()); err != nil {
		return err
	}
	recent, err := countEventsSince(s.repo.db, d.ID, EvAutoRestart, now.Add(-restartWindow))
	if err != nil {
		return err
	}
	j := Judge(JudgeInput{Prev: d.monitorState(), Obs: obs, CanaryEnabled: d.CanaryEnabled,
		BaselineCanaryMS: d.BaselineCanaryMS, AutoRestartWithinHour: recent > 0})
	if d.State == DepRunning {
		return s.onRunning(ctx, sh, d, obs, j)
	}
	return s.onStartup(ctx, sh, d, e, obs, j)
}

// markUnreachable 节点不可达/检测输出不完整: running → health=unknown(node_unreachable), 不写采样。
func (s *Service) markUnreachable(d *Deployment, cause error) error {
	if d.State == DepRunning {
		if _, err := saveMonitorState(s.repo.db, d.ID, d.monitorState(), HealthUnknown, ReasonNodeUnreachable, []string{DepRunning}); err != nil {
			return err
		}
	}
	return fmt.Errorf("节点不可达, 本轮健康未知: %w", cause)
}

// engineTail 取引擎日志末尾(失败留存用); 取不到返回空。
func (s *Service) engineTail(ctx context.Context, sh Shell, d *Deployment) string {
	cmd, err := engineLogsCommand(d.DeployNo, failLogTail)
	if err != nil {
		return ""
	}
	r, err := run(ctx, sh, cmd)
	if err != nil || r.ExitCode != 0 || r.Stdout == "@@absent\n" {
		return ""
	}
	return r.Stdout
}

// onStartup starting/restarting 阶段。
func (s *Service) onStartup(ctx context.Context, sh Shell, d *Deployment, e *ModelEntry, obs Observation, j Judgement) error {
	if j.Reason == ReasonGPUFatal {
		return s.fail(ctx, d, "GPU 出现致命 Xid 或不可纠正 ECC 错误, 停止部署(不自动重启)", s.engineTail(ctx, sh, d))
	}
	o := obs
	ready := o.Container.Running && o.HealthOK != nil && *o.HealthOK && o.ModelsOK != nil && *o.ModelsOK &&
		(!d.CanaryEnabled || (o.CanaryOK != nil && *o.CanaryOK))
	if ready {
		return s.promoteRunning(d, obs.At, j)
	}
	exited := !o.Container.Running
	timedOut := d.StateChangedAt != nil && e.StartupTimeoutS > 0 &&
		s.now().Sub(*d.StateChangedAt) > time.Duration(e.StartupTimeoutS)*time.Second
	if !exited && !timedOut {
		return nil
	}
	why := fmt.Sprintf("引擎在 %d 秒内未就绪", e.StartupTimeoutS)
	reason := ReasonHealthFailed
	if exited {
		why = fmt.Sprintf("引擎容器启动后退出(状态 %s, 退出码 %d)", o.Container.Status, o.Container.ExitCode)
		reason = ReasonContainerDown
		if o.Container.OOMKilled {
			why, reason = "引擎容器内存不足被系统终止(OOM)", ReasonOOMKilled
		}
		if !o.Container.Exists {
			why = "引擎容器不存在"
		}
	}
	tail := s.engineTail(ctx, sh, d)
	if d.State == DepStarting {
		return s.fail(ctx, d, why, tail)
	}
	return s.scheduleRestart(ctx, d, DepRestarting, reason, why, tail)
}

// promoteRunning starting/restarting → running。首次启动记 started_at 并清空基线(重启沿用原基线)。
func (s *Service) promoteRunning(d *Deployment, at time.Time, j Judgement) error {
	raw, err := json.Marshal(j.Next)
	if err != nil {
		return err
	}
	tx, err := s.repo.beginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	extra := "health=?, health_reason=?, monitor_state=?"
	if d.State == DepStarting {
		extra += ", started_at=NOW(), baseline_canary_ms=NULL"
	}
	ok, err := setState(tx, d.ID, DepRunning, []string{d.State}, extra, j.Health, nullIfEmpty(j.Reason), string(raw))
	if err != nil || !ok {
		return err
	}
	if err := insertEvent(tx, d.ID, EvRunning, "", ""); err != nil {
		return err
	}
	if err := insertSample(tx, d.ID, at, j.Sample); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if d.State == DepStarting {
		s.notifyBuyer(d, "模型部署已就绪", fmt.Sprintf("部署 %s 已启动并通过健康检测, 可通过租约网络访问推理服务", d.DeployNo))
	}
	return nil
}

// onRunning running 阶段: 采样 + 判定 + 处置。
func (s *Service) onRunning(ctx context.Context, sh Shell, d *Deployment, obs Observation, j Judgement) error {
	if err := insertSample(s.repo.db, d.ID, obs.At, j.Sample); err != nil {
		return err
	}
	if _, err := saveMonitorState(s.repo.db, d.ID, j.Next, j.Health, j.Reason, []string{DepRunning}); err != nil {
		return err
	}
	if err := s.maybeBaseline(d); err != nil {
		slog.Warn("计算金丝雀基线失败", "deploy_no", d.DeployNo, "error", err)
	}
	switch j.Health {
	case HealthUnhealthy:
		if j.Restartable {
			return s.scheduleRestart(ctx, d, DepRunning, j.Reason, ReasonText(j.Reason), s.engineTail(ctx, sh, d))
		}
		// 卡故障(致命 Xid/ECC)不重启; 金丝雀失败不在自动重启范围: 保持运行, 通知买家。
		return s.noticeOnce(d, EvUnhealthyNotice, j.Reason, "模型部署异常",
			fmt.Sprintf("部署 %s 健康检测异常: %s。该类问题不会自动重启, 请检查或手动重启/停止。", d.DeployNo, ReasonText(j.Reason)))
	case HealthDegraded:
		return s.noticeOnce(d, EvDegradedNotice, j.Reason, "模型部署性能下降",
			fmt.Sprintf("部署 %s 处于降级状态: %s(已持续 5 分钟以上)。", d.DeployNo, ReasonText(j.Reason)))
	}
	return nil
}

// noticeOnce 同一部署同一原因 30 分钟内只通知一次(以事件表去重)。
func (s *Service) noticeOnce(d *Deployment, kind, reason, title, content string) error {
	var n int
	if err := s.repo.db.Get(&n, `SELECT COUNT(*) FROM deployment_events WHERE deployment_id=? AND kind=? AND reason=? AND created_at>=?`,
		d.ID, kind, reason, s.now().Add(-degradedNotifyEvery)); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	if err := insertEvent(s.repo.db, d.ID, kind, reason, content); err != nil {
		return err
	}
	s.notifyBuyer(d, title, content)
	return nil
}

// scheduleRestart 可自动重启的不健康: 滚动 1 小时内已重启 3 次 → failed; 否则 → restarting 并在退避后重启。
func (s *Service) scheduleRestart(ctx context.Context, d *Deployment, from, reason, why, logTail string) error {
	tx, err := s.repo.beginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	cur, err := lockDeploymentTx(tx, d.ID)
	if err != nil {
		return err
	}
	if cur == nil || cur.State != from || cur.NextRestartAt != nil {
		return nil
	}
	n, err := countEventsSince(tx, d.ID, EvAutoRestart, s.now().Add(-restartWindow))
	if err != nil {
		return err
	}
	delay, giveUp := PlanRestart(n)
	if giveUp {
		ok, err := s.failDeploymentTx(ctx, tx, cur, []string{from},
			fmt.Sprintf("1 小时内已自动重启 %d 次仍不健康(%s), 停止部署", n, why), logTail)
		if err != nil || !ok {
			return err
		}
		return tx.Commit()
	}
	at := s.now().Add(delay)
	ok, err := setState(tx, d.ID, DepRestarting, []string{from}, "next_restart_at=?, health='unhealthy', health_reason=?, log_tail=COALESCE(?, log_tail)",
		at, reason, nullIfEmpty(truncateStr(logTail, 60000)))
	if err != nil || !ok {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.noticeOnce(d, EvUnhealthyNotice, reason, "模型部署异常, 将自动重启",
		fmt.Sprintf("部署 %s 不健康: %s。将在 %d 分钟后自动重启(1 小时内最多 %d 次)。", d.DeployNo, why, int(delay/time.Minute), MaxAutoRestartsPerHour))
}

// triggerAutoRestart 退避到期: 清 next_restart_at, 记一次自动重启, 入队 deploy_model。
func (s *Service) triggerAutoRestart(ctx context.Context, d *Deployment) error {
	tx, err := s.repo.beginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ok, err := affected(tx.Exec(`UPDATE model_deployments SET next_restart_at=NULL, last_restart_at=NOW(), restart_count=restart_count+1,
		state_changed_at=NOW() WHERE id=? AND state='restarting' AND next_restart_at IS NOT NULL AND next_restart_at<=?`, d.ID, s.now()))
	if err != nil || !ok {
		return err
	}
	if err := insertEvent(tx, d.ID, EvAutoRestart, deref(d.HealthReason), ""); err != nil {
		return err
	}
	if err := s.enqueueDeploy(ctx, tx, d.NodeID, d.LeaseID, d.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// maybeBaseline 运行满 1 小时后, 以首小时成功金丝雀耗时中位数作为基线(TTFT 慢判定用)。
func (s *Service) maybeBaseline(d *Deployment) error {
	if d.BaselineCanaryMS != nil || d.StartedAt == nil || !d.CanaryEnabled || s.now().Sub(*d.StartedAt) < baselineWindow {
		return nil
	}
	var ms []int
	if err := s.repo.db.Select(&ms, `SELECT canary_ms FROM deployment_health_samples WHERE deployment_id=? AND canary_ok=1
		AND canary_ms IS NOT NULL AND sampled_at>=? AND sampled_at<?`, d.ID, *d.StartedAt, d.StartedAt.Add(baselineWindow)); err != nil {
		return err
	}
	m, ok := medianInt(ms)
	if !ok {
		return nil
	}
	_, err := s.repo.db.Exec(`UPDATE model_deployments SET baseline_canary_ms=? WHERE id=? AND baseline_canary_ms IS NULL`, m, d.ID)
	return err
}
