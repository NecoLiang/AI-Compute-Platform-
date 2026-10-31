package modeldeploy

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

// startEngine 缓存就绪后: 切分配额 → 分配端口 → docker update 租约容器 → 确保镜像 → docker run 引擎 → starting。
func (s *Service) startEngine(ctx context.Context, sh Shell, st RuntimeSettings, d *Deployment, e *ModelEntry, lr *LeaseRuntime, lc liveCheck) error {
	// 配额: 引擎与租约容器对半分(引擎取下整), 两者之和等于租约完整配额, 不超卖节点。
	if lr.CPUs < 2 || lr.MemoryGB < 2 {
		return s.fail(ctx, d, fmt.Sprintf("租约配额 %d 核 / %d GB 过小, 无法与引擎容器切分(各至少 1 核 1 GB)", lr.CPUs, lr.MemoryGB), "")
	}
	engCPU, engMem := lr.CPUs/2, lr.MemoryGB/2
	leaseCPU, leaseMem := lr.CPUs-engCPU, lr.MemoryGB-engMem

	port, reason, err := s.claimPort(d, st, s.rt.ListeningPorts(lc.Listening))
	if err != nil {
		return err
	}
	if reason != "" {
		return s.fail(ctx, d, reason, "")
	}
	ok, err := affected(s.repo.db.Exec(`UPDATE model_deployments SET engine_cpus=?, engine_memory_gb=?, lease_cpus=?, lease_memory_gb=?
		WHERE id=? AND state IN ('checking','pulling','starting','restarting')`, engCPU, engMem, lr.CPUs, lr.MemoryGB, d.ID))
	if err != nil {
		return err
	}
	if !ok && (d.EngineCPUs == nil || *d.EngineCPUs != engCPU || d.EngineMemoryGB == nil || *d.EngineMemoryGB != engMem ||
		d.LeaseCPUs == nil || *d.LeaseCPUs != lr.CPUs || d.LeaseMemoryGB == nil || *d.LeaseMemoryGB != lr.MemoryGB) {
		return nil // 部署已被停止
	}

	q, err := leaseQuotaCommand(lr.Container, leaseCPU, leaseMem)
	if err != nil {
		return s.fail(ctx, d, err.Error(), "")
	}
	r, err := run(ctx, sh, q)
	if err != nil {
		return fmt.Errorf("调整租约容器配额: %w", err)
	}
	switch {
	case r.ExitCode != 0:
		return s.fail(ctx, d, "租约容器配额调整失败(docker update): "+strings.TrimSpace(r.Stderr), "")
	case strings.Contains(r.Stdout, "@@absent"):
		return s.fail(ctx, d, "租约容器 "+lr.Container+" 不存在", "")
	case !strings.Contains(r.Stdout, "@@updated"):
		return fmt.Errorf("docker update 输出异常: %q", r.Stdout)
	}

	img, err := ensureImageCommand(e.EngineImage)
	if err != nil {
		return s.fail(ctx, d, err.Error(), "")
	}
	if r, err := run(ctx, sh, img); err != nil || r.ExitCode != 0 || !strings.Contains(r.Stdout, "@@image") {
		return fmt.Errorf("准备引擎镜像: %v exit=%d %s", err, r.ExitCode, strings.TrimSpace(r.Stderr))
	}

	dir, err := modelDir(st.CacheDir, e)
	if err != nil {
		return s.fail(ctx, d, err.Error(), "")
	}
	cmd, err := engineRunCommand(engineSpec{DeployNo: d.DeployNo, LeaseNo: lr.LeaseNo, Network: lr.Network,
		GPUUUIDs: d.GPUUUIDs(), ModelDir: dir, ModelKey: e.ModelKey, Image: e.EngineImage, Port: port,
		CPUs: engCPU, MemoryGB: engMem, TP: d.TP, DP: d.DP, MaxModelLen: d.MaxModelLen, GPUMemUtil: d.GPUMemUtil,
		EngineArgs: e.EngineArgs})
	if err != nil {
		return s.fail(ctx, d, err.Error(), "")
	}
	r, err = run(ctx, sh, cmd)
	if err != nil {
		return fmt.Errorf("启动引擎容器: %w", err)
	}
	if r.ExitCode != 0 {
		return s.fail(ctx, d, "引擎容器启动失败(docker run): "+truncateStr(strings.TrimSpace(r.Stderr), 400), r.Stderr)
	}
	name := EngineContainer(d.DeployNo)
	if d.State == DepRestarting {
		_, err = s.repo.db.Exec(`UPDATE model_deployments SET container_name=?, state_changed_at=NOW() WHERE id=? AND state='restarting'`, name, d.ID)
	} else {
		_, err = setState(s.repo.db, d.ID, DepStarting, []string{DepChecking, DepPulling, DepStarting}, "container_name=?", name)
	}
	// 未生效 = 部署已在启动期间被停止; 停止任务会删掉刚启动的容器。
	return err
}

// claimPort 分配引擎端口: 沿用已占用的端口(未被他人监听时), 否则在区间内找一个库内未占用且节点未监听的端口,
// 以 uk_node_engine_port 唯一键兜底并发。reason 非空为确定性失败。
func (s *Service) claimPort(d *Deployment, st RuntimeSettings, listening map[int]bool) (int, string, error) {
	if d.EnginePortClaim != nil {
		if !listening[*d.EnginePortClaim] {
			return *d.EnginePortClaim, "", nil
		}
		// 旧端口已被节点上其它进程占用: 释放后重新分配。
		if _, err := s.repo.db.Exec(`UPDATE model_deployments SET engine_port_claim=NULL WHERE id=? AND engine_port_claim=?`,
			d.ID, *d.EnginePortClaim); err != nil {
			return 0, "", err
		}
	}
	taken := map[int]bool{}
	var claimed []int
	if err := s.repo.db.Select(&claimed, `SELECT engine_port_claim FROM model_deployments WHERE node_id=? AND engine_port_claim IS NOT NULL`, d.NodeID); err != nil {
		return 0, "", err
	}
	for _, p := range claimed {
		taken[p] = true
	}
	for p := st.PortStart; p <= st.PortEnd; p++ {
		if taken[p] || listening[p] {
			continue
		}
		ok, err := affected(s.repo.db.Exec(`UPDATE model_deployments SET engine_port_claim=?, engine_port=? WHERE id=? AND engine_port_claim IS NULL
			AND state IN ('checking','pulling','starting','restarting')`, p, p, d.ID))
		if isDupKey(err, "uk_node_engine_port") {
			continue // 并发部署抢到同一端口
		}
		if err != nil {
			return 0, "", err
		}
		if !ok {
			return 0, "", fmt.Errorf("部署 %s 端口占用状态已变化, 重试", d.DeployNo)
		}
		return p, "", nil
	}
	return 0, fmt.Sprintf("节点引擎端口区间 %d–%d 已无可用端口", st.PortStart, st.PortEnd), nil
}

// DeployDead deploy_model 重试用尽: 部署失败(清理由 stop_model 完成)。
func (s *Service) DeployDead(ctx context.Context, depID int64, lastErr string) {
	d, err := getDeployment(s.repo.db, depID)
	if err != nil || d == nil || s.rt == nil {
		slog.Error("deploy_model dead: 读取部署失败", "deployment_id", depID, "error", err)
		return
	}
	if err := s.fail(ctx, d, "部署多次重试仍失败: "+lastErr, ""); err != nil {
		slog.Error("deploy_model dead: 标记失败出错", "deployment_id", depID, "error", err)
	}
}

// RunStop stop_model 执行体: 删除引擎容器、归还租约容器完整配额后落 final(stopped/failed)。
func (s *Service) RunStop(ctx context.Context, depID int64, final string) error {
	if err := s.mustRuntime(); err != nil {
		return err
	}
	if final != DepStopped && final != DepFailed {
		return abortf("stop_model final 非法: %q", final)
	}
	d, err := getDeployment(s.repo.db, depID)
	if err != nil {
		return err
	}
	if d == nil || d.State != DepStopping {
		return nil
	}
	sh, err := s.rt.Dial(ctx, d.NodeID)
	if err != nil {
		return fmt.Errorf("连接节点: %w", err)
	}
	defer sh.Close()
	rm, err := removeEngineCommand(d.DeployNo)
	if err != nil {
		return abortf("%s", err.Error())
	}
	if r, err := run(ctx, sh, rm); err != nil || r.ExitCode != 0 {
		return fmt.Errorf("删除引擎容器: %v exit=%d %s", err, r.ExitCode, r.Stderr)
	}
	if d.LeaseCPUs != nil && d.LeaseMemoryGB != nil {
		lr, err := s.rt.LeaseRuntime(ctx, d.LeaseID)
		if err != nil {
			return err
		}
		if lr != nil && lr.Container != "" {
			q, err := leaseQuotaCommand(lr.Container, *d.LeaseCPUs, *d.LeaseMemoryGB)
			if err != nil {
				return abortf("%s", err.Error())
			}
			r, err := run(ctx, sh, q)
			if err != nil || r.ExitCode != 0 {
				return fmt.Errorf("归还租约容器配额(docker update): %v %s", err, strings.TrimSpace(r.Stderr))
			}
		}
	}
	return s.finishStop(d, final, "")
}

// finishStop stopping → final, 释放租约占用与端口; 失败时通知买家。
func (s *Service) finishStop(d *Deployment, final, note string) error {
	tx, err := s.repo.beginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ok, err := setState(tx, d.ID, final, []string{DepStopping}, `health='unknown', health_reason=NULL, engine_port_claim=NULL,
		lease_active_claim=NULL, next_restart_at=NULL, container_name=NULL, stopped_at=NOW()`)
	if err != nil || !ok {
		return err
	}
	if final == DepFailed {
		if err := insertEvent(tx, d.ID, EvFail, "", strings.TrimSpace(deref(d.FailReason)+" "+note)); err != nil {
			return err
		}
	} else if note != "" {
		if err := insertEvent(tx, d.ID, EvStop, deref(d.StopReason), note); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	if final == DepFailed {
		s.notifyBuyer(d, "模型部署失败", fmt.Sprintf("部署 %s 失败: %s", d.DeployNo, deref(d.FailReason)))
	}
	return nil
}

// StopDead stop_model 重试用尽(节点长期不可达等): 仍落终态以释放租约占用, 并记录"节点清理未确认"。
// 引擎若仍在运行, 下次部署的实时 F3 检查会发现显存被占用而拒绝启动; fleet 的租约停止/清除命令也会删除引擎容器。
func (s *Service) StopDead(ctx context.Context, depID int64, final, lastErr string) {
	d, err := getDeployment(s.repo.db, depID)
	if err != nil || d == nil || d.State != DepStopping {
		return
	}
	if final != DepFailed {
		final = DepStopped
	}
	if err := s.finishStop(d, final, "节点清理未确认(引擎容器可能仍在运行): "+lastErr); err != nil {
		slog.Error("stop_model dead: 落终态失败", "deployment_id", depID, "error", err)
	}
}

func (s *Service) notifyBuyer(d *Deployment, title, content string) {
	if s.notifier == nil {
		slog.Warn("模型部署通知未发送: notifier 未接入", "deploy_no", d.DeployNo, "title", title)
		return
	}
	var orderNo string
	if err := s.repo.db.Get(&orderNo, `SELECT order_no FROM orders WHERE id=?`, d.OrderID); err != nil {
		slog.Error("模型部署通知: 查询订单号失败", "deploy_no", d.DeployNo, "error", err)
		return
	}
	if err := s.notifier.Record(d.BuyerID, "order", title, content, "/console/buyer/orders/"+orderNo); err != nil {
		slog.Error("模型部署通知发送失败", "deploy_no", d.DeployNo, "error", err)
	}
}
