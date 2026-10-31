package modeldeploy

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"
)

// 部署流水线(docs/27 §27.4/§27.6), 由 fleet 节点任务队列调用:
//   deploy_model: checking(实时 F3/F4/F6) → pulling(同节点同模型一次 pull_model) → starting(docker update + docker run)
//   stop_model:   删除引擎容器 + 归还租约容器配额 → stopped/failed
//   pull_model:   见 pull.go
// 返回 error 表示可重试(fleet 指数退避, 次数用尽调用 *Dead); *AbortError 表示不可重试。
// 确定性失败(卡被占用、配额不足、docker run 失败等)直接走 failDeploymentTx 并返回 nil。

// deployRunStates RunDeploy 接受的状态; restarting 且处于退避等待(next_restart_at 非空)时不执行。
var deployRunStates = []string{DepPending, DepChecking, DepPulling, DepStarting, DepRestarting}

// vramBusyPermille F3: 单卡已用显存 > 总量 2% 视为被占用(docs/27 §27.3.2)。
const vramBusyPermille = 20

func (s *Service) mustRuntime() error {
	if s.rt == nil {
		return abortf("一键部署运行期未接入")
	}
	return nil
}

// fail 确定性失败: 记 fail_reason(+日志末尾), 入队清理。部署已不在可失败状态时什么也不做。
func (s *Service) fail(ctx context.Context, d *Deployment, reason, logTail string) error {
	tx, err := s.repo.beginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ok, err := s.failDeploymentTx(ctx, tx, d, stoppableStates, reason, logTail)
	if err != nil {
		return err
	}
	if !ok {
		return nil
	}
	slog.Warn("模型部署失败", "deploy_no", d.DeployNo, "reason", reason)
	return tx.Commit()
}

func run(ctx context.Context, sh Shell, cmd string) (ShellResult, error) {
	r, err := sh.Run(ctx, cmd)
	if err != nil {
		return r, err
	}
	return r, nil
}

func lastLine(s string) string {
	s = strings.TrimRight(s, "\n")
	if i := strings.LastIndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return strings.TrimSpace(s)
}

// RunDeploy deploy_model 执行体。
func (s *Service) RunDeploy(ctx context.Context, depID int64) error {
	if err := s.mustRuntime(); err != nil {
		return err
	}
	d, err := getDeployment(s.repo.db, depID)
	if err != nil {
		return err
	}
	if d == nil {
		return abortf("部署 %d 不存在", depID)
	}
	if !isOneOf(d.State, deployRunStates) || (d.State == DepRestarting && d.NextRestartAt != nil) {
		return nil
	}
	st := s.rt.Settings()
	entry, err := s.repo.GetEntry(d.ModelID)
	if err != nil {
		return err
	}
	if entry == nil {
		return s.fail(ctx, d, "模型目录条目不存在", "")
	}
	lr, err := s.rt.LeaseRuntime(ctx, d.LeaseID)
	if err != nil {
		return err
	}
	if lr == nil || lr.State != "active" {
		return s.fail(ctx, d, "托管租约未处于运行中", "")
	}
	if d.State == DepPending {
		if ok, err := setState(s.repo.db, d.ID, DepChecking, []string{DepPending}, ""); err != nil || !ok {
			return err
		}
		d.State = DepChecking
	}
	sh, err := s.rt.Dial(ctx, d.NodeID)
	if err != nil {
		return fmt.Errorf("连接节点: %w", err)
	}
	defer sh.Close()

	// 先删旧引擎(重启/重试), 释放显存与端口后再做实时检查。
	rm, err := removeEngineCommand(d.DeployNo)
	if err != nil {
		return s.fail(ctx, d, err.Error(), "")
	}
	r, err := run(ctx, sh, rm)
	if err != nil || r.ExitCode != 0 {
		return fmt.Errorf("删除旧引擎容器: %v exit=%d %s", err, r.ExitCode, r.Stderr)
	}
	removed := strings.Contains(r.Stdout, "@@removed")
	lc, reason, err := s.liveCheck(ctx, sh, st, d, lr)
	if err == nil && reason != "" && removed {
		// 刚删除的引擎进程退出后驱动回收显存可能有短暂延迟, 等一下再测一次。
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
		lc, reason, err = s.liveCheck(ctx, sh, st, d, lr)
	}
	if err != nil {
		return err
	}
	if reason != "" {
		return s.fail(ctx, d, reason, "")
	}

	ready, err := s.ensureCache(ctx, sh, st, d, entry)
	if err != nil || !ready {
		return err
	}
	return s.startEngine(ctx, sh, st, d, entry, lr, lc)
}

// liveCheck 实时 F3(显存未被占用)/F6(卡健康)+ 卡归属校验。reason 非空为确定性失败。
func (s *Service) liveCheck(ctx context.Context, sh Shell, st RuntimeSettings, d *Deployment, lr *LeaseRuntime) (liveCheck, string, error) {
	cmd, err := liveCheckCommand(st.CacheDir)
	if err != nil {
		return liveCheck{}, err.Error(), nil
	}
	r, err := run(ctx, sh, cmd)
	if err != nil {
		return liveCheck{}, "", fmt.Errorf("节点实时检查: %w", err)
	}
	lc, err := parseLiveCheck(r.Stdout)
	if err != nil {
		return lc, "", fmt.Errorf("%w: %s", err, r.Stderr)
	}
	uuids := d.GPUUUIDs()
	if len(uuids) != d.TP*d.DP {
		return lc, fmt.Sprintf("部署卡数 %d 与 tp×dp=%d 不一致", len(uuids), d.TP*d.DP), nil
	}
	own := map[string]bool{}
	for _, u := range lr.SlotUUIDs {
		own[u] = true
	}
	for _, u := range lr.FaultyUUIDs {
		if isOneOf(u, uuids) {
			return lc, "GPU " + u + " 已被标记故障, 不能启动引擎", nil
		}
	}
	for _, u := range uuids {
		if !own[u] {
			return lc, "GPU " + u + " 不属于本租约(gpu_slots.lease_id 不符), 拒绝启动", nil
		}
		v, ok := lc.VRAM[u]
		if !ok {
			return lc, "节点 nvidia-smi 未列出 GPU " + u, nil
		}
		if v[1] <= 0 || v[0]*1000 > v[1]*vramBusyPermille {
			return lc, fmt.Sprintf("GPU %s 显存已被占用 %d/%d MiB(F3), 请先结束租约内占用显卡的进程", u, v[0], v[1]), nil
		}
	}
	cards, err := s.rt.CardHealth(ctx, d.NodeID, uuids)
	if err != nil {
		return lc, "", err
	}
	for _, c := range cards {
		if c.FatalXid || (c.ECCUncorrected != nil && *c.ECCUncorrected > 0) {
			return lc, "GPU " + c.UUID + " 近 1 小时出现致命 Xid 或不可纠正 ECC 错误(F6)", nil
		}
	}
	return lc, "", nil
}

// ensureCache 权重缓存就绪返回 true; 否则确保有且仅有一个拉取在进行, 部署置 pulling 后返回 false。
func (s *Service) ensureCache(ctx context.Context, sh Shell, st RuntimeSettings, d *Deployment, e *ModelEntry) (bool, error) {
	dir, err := modelDir(st.CacheDir, e)
	if err != nil {
		return false, s.fail(ctx, d, err.Error(), "")
	}
	_, wantSum, err := manifestChecklist(e)
	if err != nil {
		return false, s.fail(ctx, d, err.Error(), "")
	}
	c, err := getCache(s.repo.db, d.NodeID, d.ModelID)
	if err != nil {
		return false, err
	}
	if c != nil && c.State == CacheReady {
		r, err := run(ctx, sh, markerCheckCommand(dir))
		if err != nil || r.ExitCode != 0 {
			return false, fmt.Errorf("读取完成标记: %v %s", err, r.Stderr)
		}
		if lastLine(r.Stdout) == wantSum {
			return s.touchReady(c.ID)
		}
		// 标记缺失/不一致(目录被改动或清单变化): 作废, 重新拉取。
		if _, err := s.repo.db.Exec(`UPDATE node_model_cache SET state='failed', last_error=? WHERE id=? AND state='ready'`,
			"完成标记缺失或与清单不一致, 需重新拉取", c.ID); err != nil {
			return false, err
		}
	}

	tx, err := s.repo.beginTx()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	c, err = lockCacheTx(tx, d.NodeID, d.ModelID)
	if err != nil {
		return false, err
	}
	enqueue := false
	switch {
	case c == nil:
		res, err := tx.Exec(`INSERT INTO node_model_cache (node_id, model_id, state, bytes_done) VALUES (?,?,'downloading',0)`, d.NodeID, d.ModelID)
		if isDupKey(err, "uk_node_model") {
			return false, fmt.Errorf("缓存条目并发创建, 稍后重试")
		}
		if err != nil {
			return false, err
		}
		id, _ := res.LastInsertId()
		c = &CacheRow{ID: id, State: CacheDownloading}
		enqueue = true
	case c.State == CacheFailed:
		ok, err := affected(tx.Exec(`UPDATE node_model_cache SET state='downloading', bytes_done=0, last_error=NULL,
			pull_container=NULL, pull_started_at=NULL, verified_at=NULL WHERE id=? AND state='failed'`, c.ID))
		if err != nil || !ok {
			return false, errOr(err, "缓存条目状态已变化")
		}
		enqueue = true
	case c.State == CacheReady:
		// 并发拉取刚好完成: 交给下一次重试按 ready 路径走(标记校验)。
		return false, fmt.Errorf("缓存条目刚就绪, 重新校验")
	case c.State == CacheEvicting:
		return false, fmt.Errorf("缓存条目正在被淘汰, 稍后重试")
	}
	if enqueue {
		cid := c.ID
		if err := s.rt.Enqueue(ctx, tx, TaskSpec{NodeID: d.NodeID, Kind: TaskPullModel, Priority: 5, MaxAttempts: 3,
			Payload: map[string]any{"cache_id": cid}}); err != nil {
			return false, err
		}
	}
	if d.State != DepPulling {
		ok, err := setState(tx, d.ID, DepPulling, []string{DepChecking, DepStarting, DepRestarting}, "")
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil // 部署已被停止
		}
	}
	return false, tx.Commit()
}

// touchReady 在行锁内确认缓存仍为 ready 并刷新 LRU 时间(淘汰也持同一行锁, 且不淘汰被未终止部署引用的条目)。
func (s *Service) touchReady(cacheID int64) (bool, error) {
	if _, err := s.repo.db.Exec(`UPDATE node_model_cache SET last_used_at=NOW() WHERE id=? AND state='ready'`, cacheID); err != nil {
		return false, err
	}
	// 同一秒内重复刷新时 RowsAffected=0, 因此按守卫条件复查而不看影响行数。
	var n int
	if err := s.repo.db.Get(&n, `SELECT COUNT(*) FROM node_model_cache WHERE id=? AND state='ready'`, cacheID); err != nil {
		return false, err
	}
	if n != 1 {
		return false, fmt.Errorf("缓存条目已不是 ready, 重新检查")
	}
	return true, nil
}

func affected(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}

func errOr(err error, msg string) error {
	if err != nil {
		return err
	}
	return errors.New(msg)
}
