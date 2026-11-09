package modeldeploy

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"
)

// 权重拉取(docs/27 §27.4.3)。pull_model 任务只负责: 腾空间(LRU) → 写校验清单 → 启动 detached 拉取容器;
// 下载/校验/原子改名在容器内完成, 由健康轮询(PollPulls)跟踪进度与结果, 不长期占用节点任务槽。

// PullTimeout 单次拉取上限, 超时按失败处理(删除拉取容器)。
const PullTimeout = 6 * time.Hour

// cacheHeadroom 需要的空闲空间 = 权重 × 1.1(.partial 与解包余量)。
func cacheNeed(weights int64) int64 { return weights + weights/10 }

// RunPull pull_model 执行体。
func (s *Service) RunPull(ctx context.Context, cacheID int64) error {
	if err := s.mustRuntime(); err != nil {
		return err
	}
	c, err := s.getCacheByID(cacheID)
	if err != nil {
		return err
	}
	if c == nil || c.State != CacheDownloading || c.PullContainer != nil {
		return nil
	}
	e, err := s.repo.GetEntry(c.ModelID)
	if err != nil {
		return err
	}
	if e == nil {
		return s.failCache(ctx, c.ID, "模型目录条目不存在", "")
	}
	st := s.rt.Settings()
	if st.PullImage == "" {
		_ = s.failCache(ctx, c.ID, "平台未配置模型拉取镜像 fleet.model_pull_image", "")
		return abortf("未配置 fleet.model_pull_image")
	}
	checklist, wantSum, err := manifestChecklist(e)
	if err != nil {
		return s.failCache(ctx, c.ID, err.Error(), "")
	}
	sh, err := s.rt.Dial(ctx, c.NodeID)
	if err != nil {
		return fmt.Errorf("连接节点: %w", err)
	}
	defer sh.Close()

	need := cacheNeed(e.WeightsBytes)
	avail, err := cacheAvail(ctx, sh, st.CacheDir)
	if err != nil {
		return err
	}
	if avail < need {
		if err := s.evictLRU(ctx, sh, st.CacheDir, c.NodeID, c.ModelID, need-avail); err != nil {
			return err
		}
		if avail, err = cacheAvail(ctx, sh, st.CacheDir); err != nil {
			return err
		}
	}
	if avail < need {
		return s.failCache(ctx, c.ID, fmt.Sprintf("节点模型缓存区空间不足: 需要 %s, 清理可淘汰缓存后可用 %s", gib(need), gib(avail)), "")
	}

	cmds, err := writeChecklistCommands(st.CacheDir, e, checklist)
	if err != nil {
		return s.failCache(ctx, c.ID, err.Error(), "")
	}
	var last ShellResult
	for _, cmd := range cmds {
		if last, err = run(ctx, sh, cmd); err != nil || last.ExitCode != 0 {
			return fmt.Errorf("写入校验清单: %v exit=%d %s", err, last.ExitCode, last.Stderr)
		}
	}
	if got := lastLine(last.Stdout); got != wantSum {
		return fmt.Errorf("校验清单写入后摘要不符: %s != %s", got, wantSum)
	}
	pull, err := pullRunCommand(st.CacheDir, st.PullImage, c.ID, e)
	if err != nil {
		return s.failCache(ctx, c.ID, err.Error(), "")
	}
	r, err := run(ctx, sh, pull)
	if err != nil {
		return fmt.Errorf("启动拉取容器: %w", err)
	}
	if r.ExitCode != 0 || !strings.Contains(r.Stdout, "@@started") {
		return fmt.Errorf("启动拉取容器失败: exit=%d %s", r.ExitCode, strings.TrimSpace(r.Stderr))
	}
	ok, err := affected(s.repo.db.Exec(`UPDATE node_model_cache SET pull_container=?, pull_started_at=NOW(), bytes_done=0
		WHERE id=? AND state='downloading' AND pull_container IS NULL`, pullContainerName(e.ID), c.ID))
	if err != nil {
		return err
	}
	if !ok {
		slog.Warn("拉取容器已启动但缓存条目状态已变化", "cache_id", c.ID)
	}
	return nil
}

// PullDead pull_model 重试用尽。
func (s *Service) PullDead(ctx context.Context, cacheID int64, lastErr string) {
	if err := s.failCache(ctx, cacheID, "拉取任务多次重试仍失败: "+lastErr, ""); err != nil {
		slog.Error("pull_model dead: 标记失败出错", "cache_id", cacheID, "error", err)
	}
}

func (s *Service) getCacheByID(id int64) (*CacheRow, error) {
	var list []CacheRow
	if err := s.repo.db.Select(&list, "SELECT "+cacheColumns+" FROM node_model_cache WHERE id=?", id); err != nil {
		return nil, err
	}
	if len(list) == 0 {
		return nil, nil
	}
	return &list[0], nil
}

func cacheAvail(ctx context.Context, sh Shell, dir string) (int64, error) {
	cmd, err := cacheAvailCommand(dir)
	if err != nil {
		return 0, abortf("%s", err.Error())
	}
	r, err := run(ctx, sh, cmd)
	if err != nil || r.ExitCode != 0 {
		return 0, fmt.Errorf("读取缓存区容量: %v %s", err, r.Stderr)
	}
	v, err := strconv.ParseInt(lastLine(r.Stdout), 10, 64)
	if err != nil || v < 0 {
		return 0, fmt.Errorf("缓存区容量输出无法解析: %q", r.Stdout)
	}
	return v, nil
}

type evictCand struct {
	ID      int64 `db:"id"`
	ModelID int64 `db:"model_id"`
	Weights int64 `db:"weights_bytes"`
}

// evictLRU 按 last_used_at 从旧到新淘汰 ready/failed 条目, 直到估计释放 ≥ need。
// 被未终止部署(含 checking/pulling/starting/running/restarting/stopping)引用的条目不淘汰; 判断与置 evicting 在同一行锁内。
func (s *Service) evictLRU(ctx context.Context, sh Shell, cacheDir string, nodeID, keepModel, need int64) error {
	var cands []evictCand
	if err := s.repo.db.Select(&cands, `SELECT c.id, c.model_id, m.weights_bytes FROM node_model_cache c
		JOIN model_catalog m ON m.id=c.model_id
		WHERE c.node_id=? AND c.model_id<>? AND c.state IN ('ready','failed')
		ORDER BY c.last_used_at IS NOT NULL, c.last_used_at, c.id`, nodeID, keepModel); err != nil {
		return err
	}
	var freed int64
	for _, cd := range cands {
		if freed >= need {
			break
		}
		claimed, err := s.claimEviction(cd.ID)
		if err != nil {
			return err
		}
		if !claimed {
			continue
		}
		e, err := s.repo.GetEntry(cd.ModelID)
		if err != nil {
			return err
		}
		cmd, err := evictCommand(cacheDir, e)
		if err == nil {
			var r ShellResult
			r, err = run(ctx, sh, cmd)
			if err == nil && (r.ExitCode != 0 || !strings.Contains(r.Stdout, "@@evicted")) {
				err = fmt.Errorf("exit=%d %s", r.ExitCode, strings.TrimSpace(r.Stderr))
			}
		}
		if err != nil {
			if _, uerr := s.repo.db.Exec(`UPDATE node_model_cache SET state='failed', last_error=? WHERE id=? AND state='evicting'`,
				truncateStr("淘汰删除失败: "+err.Error(), 500), cd.ID); uerr != nil {
				return uerr
			}
			return fmt.Errorf("淘汰缓存 %d: %w", cd.ID, err)
		}
		if _, err := s.repo.db.Exec(`DELETE FROM node_model_cache WHERE id=? AND state='evicting'`, cd.ID); err != nil {
			return err
		}
		slog.Info("模型缓存已按 LRU 淘汰", "node_id", nodeID, "model_id", cd.ModelID)
		freed += cd.Weights
	}
	return nil
}

func (s *Service) claimEviction(cacheID int64) (bool, error) {
	tx, err := s.repo.beginTx()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	c, err := lockCacheByIDTx(tx, cacheID)
	if err != nil || c == nil || (c.State != CacheReady && c.State != CacheFailed) {
		return false, err
	}
	refs, err := liveRefs(tx, c.NodeID, c.ModelID)
	if err != nil || refs > 0 {
		return false, err
	}
	ok, err := affected(tx.Exec(`UPDATE node_model_cache SET state='evicting' WHERE id=? AND state IN ('ready','failed')`, cacheID))
	if err != nil || !ok {
		return false, err
	}
	return true, tx.Commit()
}

// failCache 拉取失败: downloading → failed, 等待该缓存的 pulling 部署一并失败。
func (s *Service) failCache(ctx context.Context, cacheID int64, reason, logTail string) error {
	tx, err := s.repo.beginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err := lockCacheByIDTx(tx, cacheID)
	if err != nil || c == nil || c.State != CacheDownloading {
		return err
	}
	if _, err := tx.Exec(`UPDATE node_model_cache SET state='failed', last_error=?, pull_container=NULL WHERE id=? AND state='downloading'`,
		truncateStr(reason, 500), cacheID); err != nil {
		return err
	}
	deps := []Deployment{}
	if err := tx.Select(&deps, "SELECT "+deploymentColumns+" FROM model_deployments WHERE node_id=? AND model_id=? AND state='pulling' FOR UPDATE",
		c.NodeID, c.ModelID); err != nil {
		return err
	}
	for i := range deps {
		if _, err := s.failDeploymentTx(ctx, tx, &deps[i], []string{DepPulling}, "模型权重拉取失败: "+reason, logTail); err != nil {
			return err
		}
	}
	slog.Warn("模型权重拉取失败", "cache_id", cacheID, "reason", reason)
	return tx.Commit()
}

// markReady 拉取成功: downloading → ready, 为等待中的 pulling 部署入队 deploy_model。
func (s *Service) markReady(ctx context.Context, cacheID, weights int64) error {
	tx, err := s.repo.beginTx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	c, err := lockCacheByIDTx(tx, cacheID)
	if err != nil || c == nil || c.State != CacheDownloading {
		return err
	}
	if _, err := tx.Exec(`UPDATE node_model_cache SET state='ready', verified_at=NOW(), last_used_at=NOW(), bytes_done=?,
		pull_container=NULL, last_error=NULL WHERE id=? AND state='downloading'`, weights, cacheID); err != nil {
		return err
	}
	deps := []Deployment{}
	if err := tx.Select(&deps, "SELECT "+deploymentColumns+" FROM model_deployments WHERE node_id=? AND model_id=? AND state='pulling' FOR UPDATE",
		c.NodeID, c.ModelID); err != nil {
		return err
	}
	for _, d := range deps {
		if err := s.enqueueDeploy(ctx, tx, d.NodeID, d.LeaseID, d.ID); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PollPulls 跟踪进行中的拉取容器(健康轮询每轮调用)。节点不可达时跳过, 下一轮再看。
func (s *Service) PollPulls(ctx context.Context) error {
	if s.rt == nil {
		return nil
	}
	var rows []CacheRow
	if err := s.repo.db.Select(&rows, "SELECT "+cacheColumns+" FROM node_model_cache WHERE state='downloading' AND pull_container IS NOT NULL"); err != nil {
		return err
	}
	for i := range rows {
		if err := s.pollPull(ctx, &rows[i]); err != nil {
			slog.Warn("跟踪模型拉取失败", "cache_id", rows[i].ID, "error", err)
		}
	}
	return nil
}

func (s *Service) pollPull(ctx context.Context, c *CacheRow) error {
	e, err := s.repo.GetEntry(c.ModelID)
	if err != nil {
		return err
	}
	if e == nil {
		return s.failCache(ctx, c.ID, "模型目录条目不存在", "")
	}
	st := s.rt.Settings()
	cmd, err := pullStatusCommand(st.CacheDir, e)
	if err != nil {
		return s.failCache(ctx, c.ID, err.Error(), "")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	sh, err := s.rt.Dial(ctx, c.NodeID)
	if err != nil {
		return err
	}
	defer sh.Close()
	r, err := run(ctx, sh, cmd)
	if err != nil || r.ExitCode != 0 {
		return fmt.Errorf("查询拉取状态: %v %s", err, r.Stderr)
	}
	status, exit, bytes := parsePullStatus(r.Stdout)
	finish := func() string {
		fr, _ := run(ctx, sh, pullFinishCommand(e.ID))
		return strings.TrimSuffix(strings.TrimRight(fr.Stdout, "\n"), "@@end")
	}
	switch {
	case status == "":
		return fmt.Errorf("拉取状态输出无法解析: %q", r.Stdout)
	case status == "running" || status == "created" || status == "restarting":
		if c.PullStartedAt != nil && s.now().Sub(*c.PullStartedAt) > PullTimeout {
			return s.failCache(ctx, c.ID, fmt.Sprintf("拉取超过 %s 未完成", PullTimeout), finish())
		}
		if bytes >= 0 {
			_, err := s.repo.db.Exec(`UPDATE node_model_cache SET bytes_done=? WHERE id=? AND state='downloading'`, bytes, c.ID)
			return err
		}
		return nil
	case status == "exited" && exit == 0:
		logs := finish()
		dir, err := modelDir(st.CacheDir, e)
		if err != nil {
			return s.failCache(ctx, c.ID, err.Error(), logs)
		}
		_, want, err := manifestChecklist(e)
		if err != nil {
			return s.failCache(ctx, c.ID, err.Error(), logs)
		}
		mr, err := run(ctx, sh, markerCheckCommand(dir))
		if err != nil || mr.ExitCode != 0 {
			return fmt.Errorf("读取完成标记: %v %s", err, mr.Stderr)
		}
		if lastLine(mr.Stdout) != want {
			return s.failCache(ctx, c.ID, "拉取容器正常退出但完成标记缺失或不一致", logs)
		}
		return s.markReady(ctx, c.ID, e.WeightsBytes)
	case status == "absent":
		return s.failCache(ctx, c.ID, "拉取容器已不存在(节点重启或被删除)", "")
	default:
		logs := finish()
		return s.failCache(ctx, c.ID, fmt.Sprintf("拉取/校验失败(容器状态 %s, 退出码 %d): %s", status, exit, lastLine(logs)), logs)
	}
}

// parsePullStatus 解析 "@@pull <status> <exit>"/"@@pull absent" 与 "@@bytes <n>"(未读到为 -1)。
func parsePullStatus(out string) (status string, exit int, bytes int64) {
	bytes = -1
	for _, l := range strings.Split(out, "\n") {
		f := strings.Fields(l)
		if len(f) == 0 {
			continue
		}
		switch f[0] {
		case "@@pull":
			if len(f) >= 2 {
				status = f[1]
			}
			if len(f) >= 3 {
				exit, _ = strconv.Atoi(f[2])
			}
		case "@@bytes":
			if len(f) == 2 {
				if v, err := strconv.ParseInt(f[1], 10, 64); err == nil {
					bytes = v
				}
			}
		}
	}
	return
}
