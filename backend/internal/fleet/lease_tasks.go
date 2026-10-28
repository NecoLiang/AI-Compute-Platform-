package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// 租约收尾任务执行器(docs/25 §25.7, T-087): stop / purge(含 cleanup_only)/ kill_sessions。
//
// 共同约束:
//   - 执行前读取租约当前状态, 已不需要执行(解冻、续租恢复、换节点等)→ errTaskAborted(failed, 不重试);
//   - 节点侧命令全部幂等(对象不存在视为已完成), 节点/网络问题返回可重试错误(cleanupMaxAttempts 次, 约 6 小时);
//   - 改卡槽的事务锁顺序: 租约 → 节点行 → 卡槽 → 商品行(与其他路径一致, 不需要锁订单);
//   - 卡槽只迁移任务所在节点上属于本租约的 draining 卡(换节点后旧节点的残留由各自的 cleanup 任务处理)。

const (
	stopExecTimeout  = 5 * time.Minute
	purgeExecTimeout = 10 * time.Minute
	killExecTimeout  = 3 * time.Minute
)

func (s *Service) registerLeaseExecutors() {
	s.registerExecutor(KindStop, taskExecutor{timeout: stopExecTimeout, run: s.runStopTask, onDead: s.leaseTaskDead})
	s.registerExecutor(KindPurge, taskExecutor{timeout: purgeExecTimeout, run: s.runPurgeTask, onDead: s.leaseTaskDead})
	s.registerExecutor(KindKillSessions, taskExecutor{timeout: killExecTimeout, run: s.runKillSessionsTask, onDead: s.leaseTaskDead})
}

// leaseTaskDead stop/purge/kill_sessions 重试耗尽: 运营告警 + 租约 resource_alert(运营租约列表可见)。
// 卡槽保持 draining(不会被再分配), 由运营排查节点后在任务列表重试或人工处理。
func (s *Service) leaseTaskDead(t *Task, lastErr string) {
	var pl LeaseTaskPayload
	_ = jsonUnmarshal(t.Payload, &pl)
	slog.Error("运营告警: 租约收尾任务重试耗尽, 需人工处理", "task_id", t.ID, "kind", t.Kind, "node_id", t.NodeID,
		"lease_id", pl.LeaseID, "error", lastErr)
	if pl.LeaseID > 0 {
		if _, err := s.repo.setResourceAlert(s.repo.db, pl.LeaseID, fmt.Sprintf("%s 任务失败(节点 #%d): %s", t.Kind, t.NodeID, lastErr)); err != nil {
			slog.Error("写租约资源告警失败", "lease_id", pl.LeaseID, "error", err)
		}
	}
}

func parseLeaseTask(t *Task) (LeaseTaskPayload, error) {
	var pl LeaseTaskPayload
	if err := jsonUnmarshal(t.Payload, &pl); err != nil || pl.LeaseID == 0 {
		return pl, errTaskAborted{fmt.Sprintf("payload 无法解析: %v", err)}
	}
	return pl, nil
}

// dialLeaseNode 收尾任务的管理连接: 不要求节点 verified/online(停调度、离线待恢复的节点同样要回收),
// 但必须有已确认指纹与管理私钥; 连不上返回可重试错误。
func (s *Service) dialLeaseNode(ctx context.Context, nodeID int64) (Runner, error) {
	n, err := s.repo.GetNode(nodeID)
	if err != nil {
		return nil, err
	}
	switch {
	case n == nil:
		return nil, errTaskAborted{fmt.Sprintf("节点 #%d 已不存在", nodeID)}
	case !n.sshReady() || !n.hostKeyConfirmed() || n.MgmtPrivkeyEnc == nil:
		return nil, fmt.Errorf("节点 #%d 缺少管理连接信息或主机指纹未确认", nodeID)
	case len(s.credKey) == 0:
		return nil, errKeyUnavailable
	}
	return s.dialManaged(ctx, n)
}

// moveLeaseSlotsOnNode 同 moveLeaseSlots, 限定节点。
func (r *Repository) moveLeaseSlotsOnNode(tx *sqlx.Tx, leaseID, nodeID int64, from []string, to string) (int64, error) {
	set := `state=?, state_changed_at=NOW()`
	if to == SlotFree {
		set += `, lease_id=NULL`
	}
	q, args, err := sqlx.In(`UPDATE gpu_slots SET `+set+` WHERE lease_id=? AND node_id=? AND state IN (?)`, to, leaseID, nodeID, from)
	if err != nil {
		return 0, err
	}
	res, err := tx.Exec(q, args...)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// ===== stop =====

// StopOutcome stop 任务结果(node_tasks.result)。GPUs 为容器原卡, restart 续租优先在原卡上 docker start。
type StopOutcome struct {
	LeaseNo    string   `json:"lease_no"`
	Container  string   `json:"container"` // stopped / absent
	GPUs       []string `json:"gpus,omitempty"`
	FreedSlots int64    `json:"freed_slots"`
}

// stopApplies 租约是否仍需要停容器: grace(到期)、suspended 且已到期(冻结中到期, 停容器释放卡但保留现场)。
func stopApplies(l *Lease, now time.Time) bool {
	switch l.State {
	case LeaseGrace:
		return true
	case LeaseSuspended:
		return l.AccessUntil != nil && !l.AccessUntil.After(now)
	}
	return false
}

func (s *Service) runStopTask(ctx context.Context, t *Task) (any, error) {
	pl, err := parseLeaseTask(t)
	if err != nil {
		return nil, err
	}
	l, err := s.repo.getLease(s.repo.db, pl.LeaseID)
	if err != nil {
		return nil, err
	}
	if l == nil {
		return nil, errTaskAborted{"租约不存在"}
	}
	if l.NodeID != t.NodeID || !stopApplies(l, time.Now()) {
		return nil, errTaskAborted{fmt.Sprintf("租约 %s 当前状态 %s(节点 #%d), 无需停止", l.LeaseNo, l.State, l.NodeID)}
	}
	client, err := s.dialLeaseNode(ctx, t.NodeID)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	cmd, err := stopContainerCommand(l.LeaseNo)
	if err != nil {
		return nil, errTaskAborted{err.Error()}
	}
	out, err := runRemote(ctx, client, cmd, stopTimeout)
	if err != nil {
		return nil, fmt.Errorf("停止租约容器失败: %w", err)
	}
	exists, gpus, err := parseStopped(out)
	if err != nil {
		return nil, err
	}
	res := StopOutcome{LeaseNo: l.LeaseNo, Container: "absent", GPUs: gpus}
	if exists {
		res.Container = "stopped"
	}
	err = s.inTx(func(tx *sqlx.Tx) error {
		cur, err := s.repo.lockLease(tx, l.ID)
		if err != nil {
			return err
		}
		if cur == nil || cur.NodeID != t.NodeID || !stopApplies(cur, time.Now()) {
			// 停容器期间租约已恢复(续租)等: 卡槽归属由恢复路径处理, 这里不动。
			return nil
		}
		if err := s.lockNodeRow(tx, t.NodeID); err != nil {
			return err
		}
		if res.FreedSlots, err = s.repo.moveLeaseSlotsOnNode(tx, l.ID, t.NodeID, []string{SlotDraining}, SlotFree); err != nil {
			return err
		}
		return s.recomputeStockTx(tx, l.ProductID)
	})
	if err != nil {
		return nil, err
	}
	slog.Info("租约容器已停止, 卡槽已释放", "lease_no", l.LeaseNo, "node_id", t.NodeID, "container", res.Container, "freed", res.FreedSlots)
	return res, nil
}

// ===== purge =====

// PurgeOutcome purge 任务结果(node_tasks.result), 与清除回执(audit_logs)内容一致。
type PurgeOutcome struct {
	LeaseNo     string      `json:"lease_no"`
	CleanupOnly bool        `json:"cleanup_only,omitempty"`
	Skipped     string      `json:"skipped,omitempty"`
	Steps       []PurgeStep `json:"steps"`
	FreedSlots  int64       `json:"freed_slots"`
}

// PurgeReceipt 清除回执(docs/25 §25.7.5), 写入 audit_logs.after_value(action=lease.purged / lease.cleanup)。
// 清除是逻辑删除(删除容器可写层、数据卷、租约网络与本地提交镜像), 不承诺介质级擦除。
type PurgeReceipt struct {
	LeaseNo      string      `json:"lease_no"`
	OrderID      int64       `json:"order_id"`
	NodeID       int64       `json:"node_id"`
	ContainerID  string      `json:"container_id,omitempty"`
	Container    string      `json:"container"`
	Volume       string      `json:"volume"`
	Network      string      `json:"network"`
	CommitImage  string      `json:"commit_image,omitempty"`
	UpstreamPort *int        `json:"upstream_port,omitempty"`
	PortRules    string      `json:"port_rules"`
	Steps        []PurgeStep `json:"steps"`
	Reason       string      `json:"reason,omitempty"`
	Erasure      string      `json:"erasure"`
	Executor     string      `json:"executor"`
	FinishedAt   time.Time   `json:"finished_at"`
}

const (
	portRulesNote = "容器端口映射(-p)随容器删除; 节点级 DOCKER-USER 租约端口段规则为节点共用, 保留"
	erasureNote   = "逻辑删除: 删除容器可写层、数据卷、租约网络与本地提交镜像, 不做介质级擦除"
)

func (s *Service) runPurgeTask(ctx context.Context, t *Task) (any, error) {
	pl, err := parseLeaseTask(t)
	if err != nil {
		return nil, err
	}
	l, err := s.repo.getLease(s.repo.db, pl.LeaseID)
	if err != nil {
		return nil, err
	}
	if l == nil {
		return nil, errTaskAborted{"租约不存在"}
	}
	if pl.CleanupOnly {
		return s.runCleanupOnly(ctx, t, pl, l)
	}
	if l.State != LeasePurging || l.NodeID != t.NodeID {
		return nil, errTaskAborted{fmt.Sprintf("租约 %s 当前状态 %s(节点 #%d), 无需清除", l.LeaseNo, l.State, l.NodeID)}
	}
	if !ValidLeaseNo(l.LeaseNo) {
		return nil, errTaskAborted{"租约号非法"}
	}
	commit := deref(l.CommitImage)
	steps, err := s.purgeResources(ctx, t.NodeID, containerNameOf(l.LeaseNo), volumeNameOf(l.LeaseNo), networkNameOf(l.LeaseNo), commit)
	if err != nil {
		return nil, err
	}
	out := PurgeOutcome{LeaseNo: l.LeaseNo, Steps: steps}
	receipt := s.receiptFor(l, t.NodeID, steps, pl.Reason)
	var purged bool
	err = s.inTx(func(tx *sqlx.Tx) error {
		cur, err := s.repo.lockLease(tx, l.ID)
		if err != nil {
			return err
		}
		if cur == nil || cur.State != LeasePurging || cur.NodeID != t.NodeID {
			out.Skipped = "清除期间租约状态已变化"
			return nil
		}
		if err := s.mustTransition(tx, cur, LeasePurged, ", purged_at=NOW(), commit_image=NULL"); err != nil {
			return err
		}
		if err := s.lockNodeRow(tx, t.NodeID); err != nil {
			return err
		}
		if out.FreedSlots, err = s.repo.moveLeaseSlotsOnNode(tx, l.ID, t.NodeID, []string{SlotDraining}, SlotFree); err != nil {
			return err
		}
		if err := s.repo.audit(tx, 0, "lease.purged", "lease", l.ID, nil, receipt, ""); err != nil {
			return err
		}
		purged = true
		return s.recomputeStockTx(tx, l.ProductID)
	})
	if err != nil {
		return nil, err
	}
	if purged {
		slog.Info("租约已清除", "lease_no", l.LeaseNo, "node_id", t.NodeID, "freed", out.FreedSlots)
		if o, err := s.products.ManagedOrderByID(l.OrderID); err == nil && o != nil {
			s.notifyLink(o.BuyerID, "算力环境已清除",
				fmt.Sprintf("订单 %s 的租用环境(租约 %s)已按规则清除, 容器与数据卷已删除(逻辑删除)。", o.OrderNo, l.LeaseNo),
				"/console/buyer/orders/"+o.OrderNo)
		}
	}
	return out, nil
}

// runCleanupOnly 换节点/开通失败后清理旧节点上的半成品(T-083 入队): 只删 payload 中的资源, 释放本租约在该节点上的
// draining 卡槽, 不迁移租约状态。租约已回到本节点继续使用时(清除后在原节点重新开通), 同名资源已属新环境, 不做清理。
func (s *Service) runCleanupOnly(ctx context.Context, t *Task, pl LeaseTaskPayload, l *Lease) (any, error) {
	if l.NodeID == t.NodeID && !containsStr(leaseTerminal, l.State) && l.State != LeasePurging {
		return nil, errTaskAborted{fmt.Sprintf("租约 %s 已回到节点 #%d(%s), 同名资源属于当前环境, 不做残留清理", l.LeaseNo, t.NodeID, l.State)}
	}
	c, v := pl.ContainerName, pl.VolumeName
	if c == "" {
		c = containerNameOf(l.LeaseNo)
	}
	if v == "" {
		v = volumeNameOf(l.LeaseNo)
	}
	if err := checkLeaseRes(c, v); err != nil {
		return nil, errTaskAborted{err.Error()}
	}
	steps, err := s.purgeResources(ctx, t.NodeID, c, v, networkNameOf(l.LeaseNo), "")
	if err != nil {
		return nil, err
	}
	out := PurgeOutcome{LeaseNo: l.LeaseNo, CleanupOnly: true, Steps: steps}
	receipt := s.receiptFor(l, t.NodeID, steps, pl.Reason)
	receipt.Container, receipt.Volume, receipt.UpstreamPort = c, v, nil
	err = s.inTx(func(tx *sqlx.Tx) error {
		if _, err := s.repo.lockLease(tx, l.ID); err != nil {
			return err
		}
		if err := s.lockNodeRow(tx, t.NodeID); err != nil {
			return err
		}
		var err error
		if out.FreedSlots, err = s.repo.moveLeaseSlotsOnNode(tx, l.ID, t.NodeID, []string{SlotDraining}, SlotFree); err != nil {
			return err
		}
		if err := s.repo.audit(tx, 0, "lease.cleanup", "lease", l.ID, nil, receipt, ""); err != nil {
			return err
		}
		return s.recomputeStockTx(tx, l.ProductID)
	})
	if err != nil {
		return nil, err
	}
	slog.Info("旧节点残留已清理", "lease_no", l.LeaseNo, "node_id", t.NodeID, "freed", out.FreedSlots)
	return out, nil
}

// purgeResources 依次删除容器 → 数据卷 → 租约网络 → 提交镜像, 每步记录退出码与结论; 任一步失败返回可重试错误
// (已删除的对象重试时为 absent, 幂等)。
func (s *Service) purgeResources(ctx context.Context, nodeID int64, container, volume, network, commit string) ([]PurgeStep, error) {
	client, err := s.dialLeaseNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	type step struct {
		name, target string
		build        func(string) (string, error)
	}
	plan := []step{{"container", container, purgeContainerCommand}, {"volume", volume, purgeVolumeCommand},
		{"network", network, purgeNetworkCommand}}
	if commit != "" {
		plan = append(plan, step{"commit_image", commit, removeCommitImageCommand})
	}
	var steps []PurgeStep
	var failed []string
	for _, p := range plan {
		cmd, err := p.build(p.target)
		if err != nil {
			return nil, errTaskAborted{err.Error()}
		}
		st := PurgeStep{Step: p.name, Target: p.target}
		rctx, cancel := context.WithTimeout(ctx, cmdTimeout)
		res, err := client.Run(rctx, cmd)
		cancel()
		if err != nil {
			return nil, fmt.Errorf("清除步骤 %s 执行失败: %w", p.name, err)
		}
		st.ExitCode = res.ExitCode
		st.Outcome, st.ID = parsePurgeOut(res.Stdout)
		if res.ExitCode != 0 || st.Outcome == "error" {
			st.Outcome = "error"
			st.Detail = truncate(strings.TrimSpace(res.Stderr+" "+res.Stdout), 200)
			failed = append(failed, p.name+": "+st.Detail)
		}
		steps = append(steps, st)
	}
	if len(failed) > 0 {
		return nil, fmt.Errorf("清除未完成: %s", strings.Join(failed, "; "))
	}
	return steps, nil
}

func (s *Service) receiptFor(l *Lease, nodeID int64, steps []PurgeStep, reason string) PurgeReceipt {
	r := PurgeReceipt{LeaseNo: l.LeaseNo, OrderID: l.OrderID, NodeID: nodeID,
		Container: containerNameOf(l.LeaseNo), Volume: volumeNameOf(l.LeaseNo), Network: networkNameOf(l.LeaseNo),
		CommitImage: deref(l.CommitImage), UpstreamPort: l.UpstreamPort, PortRules: portRulesNote, Steps: steps,
		Reason: reason, Erasure: erasureNote, Executor: "system", FinishedAt: time.Now()}
	for _, st := range steps {
		if st.Step == "container" && st.ID != "" {
			r.ContainerID = st.ID
		}
	}
	return r
}

// ===== kill_sessions =====

// KillSessionsOutcome kill_sessions 任务结果。
type KillSessionsOutcome struct {
	LeaseNo string `json:"lease_no"`
	Killed  int    `json:"killed"`
	Running bool   `json:"running"`
}

func (s *Service) runKillSessionsTask(ctx context.Context, t *Task) (any, error) {
	pl, err := parseLeaseTask(t)
	if err != nil {
		return nil, err
	}
	l, err := s.repo.getLease(s.repo.db, pl.LeaseID)
	if err != nil {
		return nil, err
	}
	if l == nil {
		return nil, errTaskAborted{"租约不存在"}
	}
	// 冻结(suspended)才需要断会话; 已解冻或已进入收尾的不再执行(收尾由 stop/purge 停容器断开)。
	if l.State != LeaseSuspended || l.NodeID != t.NodeID {
		return nil, errTaskAborted{fmt.Sprintf("租约 %s 当前状态 %s, 无需断开会话", l.LeaseNo, l.State)}
	}
	client, err := s.dialLeaseNode(ctx, t.NodeID)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	cmd, err := killSessionsCommand(l.LeaseNo)
	if err != nil {
		return nil, errTaskAborted{err.Error()}
	}
	out, err := runRemote(ctx, client, cmd, cmdTimeout)
	if err != nil {
		return nil, fmt.Errorf("断开容器内会话失败: %w", err)
	}
	res := KillSessionsOutcome{LeaseNo: l.LeaseNo}
	for _, line := range nonEmptyLines(out) {
		if line == "@@notrunning" {
			return res, nil
		}
		if strings.HasPrefix(line, "@@killed ") {
			if _, err := fmt.Sscanf(line, "@@killed %d", &res.Killed); err != nil {
				return nil, fmt.Errorf("断开会话输出无法解析: %q", truncate(line, 80))
			}
			res.Running = true
			slog.Info("冻结租约容器内会话已断开", "lease_no", l.LeaseNo, "killed", res.Killed)
			return res, nil
		}
	}
	return nil, fmt.Errorf("断开会话无结论输出: %q", truncate(strings.TrimSpace(out), 120))
}
