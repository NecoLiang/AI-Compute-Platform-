package fleet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/ssh"
)

// 周期探活(docs/25 §25.4.4): 每 60s 对 verified 节点做 SSH 轻量探活; 完整探活 12h 一次、端口复测 24h 一次(入队)。

const (
	lightInterval    = 60 * time.Second
	lightTimeout     = 45 * time.Second
	offlineAfter     = 90 * time.Second
	fullEvery        = 12 * time.Hour
	portRecheckEvery = 24 * time.Hour
)

// RunPeriodic 阻塞运行直到 ctx 取消。
func (s *Service) RunPeriodic(ctx context.Context) {
	t := time.NewTicker(lightInterval)
	defer t.Stop()
	var lastPurge time.Time
	for {
		s.periodicRound(ctx)
		if time.Since(lastPurge) >= time.Hour {
			lastPurge = time.Now()
			// 访问会话审计保留 180 天(docs/25 §25.6.3)。
			if n, err := s.repo.PurgeOldSessions(lastPurge.Add(-sessionRetention)); err != nil {
				slog.Error("清理过期访问会话记录失败", "error", err)
			} else if n > 0 {
				slog.Info("已清理过期访问会话记录", "rows", n)
			}
			if n, err := s.repo.PurgeOldXidEvents(lastPurge.Add(-xidEventRetention)); err != nil {
				slog.Error("清理过期 Xid 事件明细失败", "error", err)
			} else if n > 0 {
				slog.Info("已清理过期 Xid 事件明细", "rows", n)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (s *Service) periodicRound(ctx context.Context) {
	nodes, err := s.repo.listVerifiedNodes()
	if err != nil {
		slog.Error("fleet 周期探活读取节点失败", "error", err)
		return
	}
	limit := s.cfg.ProbeConcurrency
	if limit <= 0 {
		limit = 32
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for i := range nodes {
		if ctx.Err() != nil {
			break
		}
		n := &nodes[i]
		sem <- struct{}{}
		wg.Add(1)
		go func() {
			defer func() { <-sem; wg.Done() }()
			s.lightProbe(ctx, n)
			s.schedulePeriodic(n)
		}()
	}
	wg.Wait()
}

// schedulePeriodic 距上次完整探活超过 12h 入队完整探活; 距上次端口检查(完整探活或复测)超过 24h 入队端口复测。
func (s *Service) schedulePeriodic(n *Node) {
	if !n.sshReady() {
		return
	}
	full, err := s.repo.latestReportAt(n.ID, "verify")
	if err != nil {
		slog.Error("fleet 读取探活报告时间失败", "node_id", n.ID, "error", err)
		return
	}
	mode := ""
	if full == nil || time.Since(*full) >= fullEvery {
		mode = ProbeFull
	} else {
		recheck, err := s.repo.latestReportAt(n.ID, "port_recheck")
		if err != nil {
			slog.Error("fleet 读取探活报告时间失败", "node_id", n.ID, "error", err)
			return
		}
		last := *full
		if recheck != nil && recheck.After(last) {
			last = *recheck
		}
		if time.Since(last) >= portRecheckEvery {
			mode = ProbePortRecheck
		}
	}
	if mode == "" {
		return
	}
	err = s.inTx(func(tx *sqlx.Tx) error {
		cur, err := s.repo.lockNode(tx, n.ID)
		if err != nil || cur == nil || cur.VerifyStatus != VerifyVerified {
			return err
		}
		_, err = s.enqueueProbeTx(tx, cur, mode, "periodic", 8)
		return err
	})
	if err != nil {
		slog.Error("fleet 周期探活入队失败", "node_id", n.ID, "mode", mode, "error", err)
	}
}

// lightProbe 一次轻量探活: 成功写在线/心跳/node_heartbeats 并把新发现的故障空闲卡标 faulty;
// 失败计数, 距上次成功超过 90s 转 offline。
func (s *Service) lightProbe(ctx context.Context, n *Node) {
	if !n.sshReady() || !n.hostKeyConfirmed() || n.MgmtPrivkeyEnc == nil {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, lightTimeout)
	defer cancel()
	reason, handled := s.lightCheck(ctx, n)
	if handled || ctx.Err() != nil && errors.Is(context.Cause(ctx), context.Canceled) {
		return
	}
	offline, err := s.repo.recordLightFail(n.ID, offlineAfter)
	if err != nil {
		slog.Error("fleet 记录轻量探活失败出错", "node_id", n.ID, "error", err)
		return
	}
	slog.Warn("fleet 轻量探活失败", "node_id", n.ID, "reason", reason, "offline", offline)
	if !offline {
		return
	}
	if err := s.inTx(func(tx *sqlx.Tx) error { return s.recomputeStockTx(tx, n.ProductID) }); err != nil {
		slog.Error("fleet 离线后重算库存失败", "node_id", n.ID, "error", err)
	}
	s.refreshHealth(n)
	s.notify(n.SupplierID, "节点离线",
		fmt.Sprintf("平台已连续 %d 秒无法完成对节点「%s」的探活(%s), 节点已标记离线并停止接受新租约。恢复连通后将自动上线。",
			int(offlineAfter.Seconds()), n.NodeName, reason), n.ID)
}

// lightCheck 返回失败原因; handled=true 表示已成功落库或已按指纹变化处理, 调用方无需再记失败。
func (s *Service) lightCheck(ctx context.Context, n *Node) (reason string, handled bool) {
	if len(s.credKey) == 0 {
		return errKeyUnavailable.Error(), false
	}
	signer, err := loadNodeSigner(*n.MgmtPrivkeyEnc, s.credKey)
	if err != nil {
		slog.Error("运营告警: 节点管理私钥不可用", "node_id", n.ID, "error", err)
		return "节点管理私钥不可用", false
	}
	conn, err := s.dialer.TCPCheck(ctx, *n.SSHHost, *n.SSHPort)
	if err != nil {
		slog.Warn("周期轻检: 连接管理端口失败", "node_id", n.ID, "error", err)
		return "管理端口不可达", false
	}
	client, info, err := s.dialer.Connect(conn, *n.SSHUser, []ssh.AuthMethod{ssh.PublicKeys(signer)}, *n.HostKeyFingerprint)
	if errors.Is(err, ErrHostKeyMismatch) {
		s.handleKeyMismatch(n.ID, n.Generation, *n.HostKeyFingerprint, info.Fingerprint)
		return "", true
	}
	if err != nil {
		slog.Warn("周期轻检: SSH 握手/登录失败", "node_id", n.ID, "error", err)
		if info.Fingerprint == "" {
			return "SSH 握手失败", false
		}
		return "SSH 登录失败", false
	}
	defer client.Close()
	p := &probeRun{s: s, n: n, client: client}

	out, err := p.runFor(ctx, cmdLight, 20*time.Second)
	if err != nil {
		return "nvidia-smi 执行失败: " + err.Error(), false
	}
	gpus, err := ParseLight(out)
	if err != nil {
		return err.Error(), false
	}
	if _, err := p.runFor(ctx, cmdDockerAlive, 15*time.Second); err != nil {
		return "Docker 守护进程不可用: " + err.Error(), false
	}
	// Xid: 自上次成功扫描起(见 xidWindowSince), 事件与覆盖区间在下方事务内落库。
	lastScan, err := s.repo.lastXidScanAt(n.ID)
	if err != nil {
		slog.Error("fleet 读取 Xid 扫描时间失败", "node_id", n.ID, "error", err)
		return "", true // 平台侧故障不计入节点失败
	}
	since, window := xidSince(xidWindowSince(lastScan, time.Now()))
	cmd, _ := xidCommand(since)
	xout, _ := p.runFor(ctx, cmd, 15*time.Second) // 以解析结果为准(错误输出带 @@err 前缀)
	xidAt := time.Now()
	xid := ParseXid(xout, window)
	tel, telAt := s.collectTelemetry(ctx, p)

	slots, err := s.repo.listSlots(s.repo.db, n.ID)
	if err != nil {
		slog.Error("fleet 读取卡槽失败", "node_id", n.ID, "error", err)
		return "", true // 平台侧故障不计入节点失败
	}
	seen := map[string]LightGPU{}
	for _, g := range gpus {
		seen[g.UUID] = g
	}
	var faulty []int
	var notes, busyAlerts []string
	leaseAlerts := map[int64][]string{}
	free := 0
	for _, sl := range slots {
		if sl.State == SlotFaulty {
			continue
		}
		var why []string
		g, ok := seen[sl.GPUUUID]
		if !ok {
			why = append(why, "掉卡(nvidia-smi 未列出)")
		} else if g.UncorrectedECC > 0 {
			why = append(why, fmt.Sprintf("不可纠正 ECC 错误 %d 次", g.UncorrectedECC))
		}
		if x := xid.FatalXidsFor(sl.PCIBusID); len(x) > 0 {
			why = append(why, fmt.Sprintf("致命 Xid %v", x))
		}
		if len(why) == 0 {
			if sl.State == SlotFree {
				free++
			}
			continue
		}
		msg := fmt.Sprintf("GPU%d %s", sl.GPUIndex, strings.Join(why, "、"))
		if sl.State == SlotFree {
			faulty = append(faulty, sl.GPUIndex)
			notes = append(notes, msg)
		} else {
			busyAlerts = append(busyAlerts, msg+"(状态 "+sl.State+")")
			if sl.LeaseID != nil && (sl.State == SlotLeased || sl.State == SlotReserved) {
				leaseAlerts[*sl.LeaseID] = append(leaseAlerts[*sl.LeaseID], msg)
			}
		}
	}
	if len(busyAlerts) > 0 {
		slog.Error("运营告警: 托管节点非空闲卡异常", "node_id", n.ID, "detail", strings.Join(busyAlerts, "; "))
	}
	s.syncResourceAlerts(n.ID, leaseAlerts)
	var prev string
	var changed int64
	err = s.inTx(func(tx *sqlx.Tx) error {
		// 锁顺序 节点行 → 卡槽 → 商品行(与下单预留一致, 见 reserveSlotsTx)。
		if err := s.lockNodeRow(tx, n.ID); err != nil {
			return err
		}
		var err error
		if changed, err = s.repo.markSlotsFaulty(tx, n.ID, faulty); err != nil {
			return err
		}
		if prev, err = s.repo.recordLightOK(tx, n.ID, free, avgUtil(gpus, false), avgUtil(gpus, true)); err != nil {
			return err
		}
		if err := s.repo.recordTelemetryTx(tx, n.ID, n.Generation, tel, telAt); err != nil {
			return err
		}
		if err := s.repo.recordXidScanTx(tx, n.ID, n.Generation, xid, xidAt); err != nil {
			return err
		}
		if changed > 0 || prev != "online" {
			return s.recomputeStockTx(tx, n.ProductID)
		}
		return nil
	})
	if err != nil {
		slog.Error("fleet 轻量探活落库失败", "node_id", n.ID, "error", err)
		return "", true
	}
	if changed > 0 || prev != "online" {
		s.refreshHealth(n)
	}
	if changed > 0 {
		slog.Warn("fleet 周期探活发现故障卡", "node_id", n.ID, "detail", strings.Join(notes, "; "))
		s.notify(n.SupplierID, "节点有卡故障",
			fmt.Sprintf("节点「%s」周期探活发现: %s。故障卡已停止出租, 处理后重新发起探活即可恢复。", n.NodeName, strings.Join(notes, "; ")), n.ID)
	}
	if prev == "offline" {
		slog.Info("fleet 节点恢复在线", "node_id", n.ID)
	}
	return "", true
}

// collectTelemetry 逐卡遥测(cmdTelemetry, 不认识 clocks_throttle_reasons 的新驱动改用 clocks_event_reasons)。
// 失败只记日志、返回 nil: 不影响本次探活成败, 遥测列随之变旧, 一键部署可行性读取时按"未知"处理。
func (s *Service) collectTelemetry(ctx context.Context, p *probeRun) ([]GPUTelemetry, time.Time) {
	out, err := p.runFor(ctx, cmdTelemetry, 20*time.Second)
	if err != nil && isInvalidFieldErr(out+err.Error()) {
		out, err = p.runFor(ctx, cmdTelemetryEvent, 20*time.Second)
	}
	at := time.Now()
	if err != nil {
		slog.Warn("fleet 逐卡遥测采集失败", "node_id", p.n.ID, "error", err)
		return nil, at
	}
	tel, err := ParseTelemetry(out)
	if err != nil {
		slog.Warn("fleet 逐卡遥测解析失败", "node_id", p.n.ID, "error", err)
		return nil, at
	}
	return tel, at
}

// avgUtil 平均利用率(百分比)。
func avgUtil(gpus []LightGPU, mem bool) int {
	if len(gpus) == 0 {
		return 0
	}
	sum := 0
	for _, g := range gpus {
		if mem {
			sum += g.UtilMem
		} else {
			sum += g.UtilGPU
		}
	}
	return sum / len(gpus)
}

// syncResourceAlerts 在租/预留卡的轻探活异常写入对应租约的 resource_alert, 卡恢复正常的租约清空(docs/25 §25.6)。
// 停机认定、计时与补偿(outage_remedy)属 T-105, 以 resource_alert 为输入; 这里不迁移租约状态、不动卡槽。
func (s *Service) syncResourceAlerts(nodeID int64, alerts map[int64][]string) {
	var ids []int64
	if err := s.repo.db.Select(&ids, `SELECT id FROM leases WHERE node_id=? AND state IN ('provisioning','active','parked','suspended')`,
		nodeID); err != nil {
		slog.Error("fleet 读取节点租约失败", "node_id", nodeID, "error", err)
		return
	}
	for _, id := range ids {
		msg := strings.Join(alerts[id], "; ")
		changed, err := s.repo.setResourceAlert(s.repo.db, id, msg)
		if err != nil {
			slog.Error("fleet 写租约资源告警失败", "lease_id", id, "error", err)
			continue
		}
		if changed && msg != "" {
			slog.Error("运营告警: 在租卡资源异常", "node_id", nodeID, "lease_id", id, "detail", msg)
		} else if changed {
			slog.Info("在租卡资源异常已恢复", "node_id", nodeID, "lease_id", id)
		}
	}
}
