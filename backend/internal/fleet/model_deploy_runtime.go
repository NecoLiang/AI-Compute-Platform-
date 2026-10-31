package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"tokenfactory/internal/modeldeploy"

	"github.com/jmoiron/sqlx"
)

// 一键部署运行期(docs/27 §27.4–§27.6, T-098/T-099): fleet 侧实现 modeldeploy.Runtime, 并把
// deploy_model / pull_model / stop_model 三类节点任务接入 fleet 任务队列(同节点串行、同租约互斥)。
// 只有 fleet.enabled=true 时由 main 调用 AttachModelDeploy; 未调用时 modeldeploy 的运行期为 nil,
// 部署写接口返回 50000 并写明"fleet 未启用", 本文件的执行器、钩子与周期任务都不生效。

const (
	deployModelTimeout = 45 * time.Minute // 实时检查 + 引擎镜像缺失时拉取(≤30min) + docker run
	pullModelTimeout   = 10 * time.Minute // 腾空间 + 写校验清单 + 启动 detached 拉取容器(下载本身不占任务槽)
	stopModelTimeout   = 5 * time.Minute
)

// AttachModelDeploy 注册部署任务执行器并返回运行期适配器(须在 RunWorkers 之前调用)。
func (s *Service) AttachModelDeploy(md *modeldeploy.Service) modeldeploy.Runtime {
	s.modelDeploy = md
	s.registerExecutor(modeldeploy.TaskDeployModel, taskExecutor{timeout: deployModelTimeout,
		run: func(ctx context.Context, t *Task) (any, error) {
			var p struct {
				DeploymentID int64 `json:"deployment_id"`
			}
			if err := json.Unmarshal(t.Payload, &p); err != nil || p.DeploymentID <= 0 {
				return nil, errTaskAborted{"deploy_model payload 缺少 deployment_id"}
			}
			return nil, mapDeployErr(md.RunDeploy(ctx, p.DeploymentID))
		},
		onDead: func(t *Task, lastErr string) {
			if t.DeploymentID != nil {
				md.DeployDead(context.Background(), *t.DeploymentID, lastErr)
			}
		}})
	s.registerExecutor(modeldeploy.TaskPullModel, taskExecutor{timeout: pullModelTimeout,
		run: func(ctx context.Context, t *Task) (any, error) {
			var p struct {
				CacheID int64 `json:"cache_id"`
			}
			if err := json.Unmarshal(t.Payload, &p); err != nil || p.CacheID <= 0 {
				return nil, errTaskAborted{"pull_model payload 缺少 cache_id"}
			}
			return nil, mapDeployErr(md.RunPull(ctx, p.CacheID))
		},
		onDead: func(t *Task, lastErr string) {
			var p struct {
				CacheID int64 `json:"cache_id"`
			}
			if json.Unmarshal(t.Payload, &p) == nil && p.CacheID > 0 {
				md.PullDead(context.Background(), p.CacheID, lastErr)
			}
		}})
	s.registerExecutor(modeldeploy.TaskStopModel, taskExecutor{timeout: stopModelTimeout,
		run: func(ctx context.Context, t *Task) (any, error) {
			var p struct {
				DeploymentID int64  `json:"deployment_id"`
				Final        string `json:"final"`
			}
			if err := json.Unmarshal(t.Payload, &p); err != nil || p.DeploymentID <= 0 {
				return nil, errTaskAborted{"stop_model payload 缺少 deployment_id"}
			}
			return nil, mapDeployErr(md.RunStop(ctx, p.DeploymentID, p.Final))
		},
		onDead: func(t *Task, lastErr string) {
			var p struct {
				Final string `json:"final"`
			}
			_ = json.Unmarshal(t.Payload, &p)
			if t.DeploymentID != nil {
				md.StopDead(context.Background(), *t.DeploymentID, p.Final, lastErr)
			}
		}})
	return &deployRuntime{s: s}
}

// mapDeployErr modeldeploy 的不可重试错误 → fleet errTaskAborted(failed, 不重试)。
func mapDeployErr(err error) error {
	var ab *modeldeploy.AbortError
	if errors.As(err, &ab) {
		return errTaskAborted{ab.Msg}
	}
	return err
}

// stopDeploymentsTx 租约冻结/到期/清除时同事务停止其部署(未接入一键部署时无操作)。
func (s *Service) stopDeploymentsTx(tx *sqlx.Tx, leaseID int64, reason string) error {
	if s.modelDeploy == nil {
		return nil
	}
	return s.modelDeploy.StopForLeaseTx(context.Background(), tx, leaseID, reason)
}

// resumeNightlyDeploymentsTx 包夜开窗同事务恢复因窗口结束停止且 auto_resume=1 的部署(未接入一键部署时无操作)。
func (s *Service) resumeNightlyDeploymentsTx(tx *sqlx.Tx, leaseID int64) (int, error) {
	if s.modelDeploy == nil {
		return 0, nil
	}
	return s.modelDeploy.ResumeNightlyForLeaseTx(context.Background(), tx, leaseID)
}

type deployRuntime struct{ s *Service }

type runnerShell struct{ r Runner }

func (r runnerShell) Run(ctx context.Context, cmd string) (modeldeploy.ShellResult, error) {
	res, err := r.r.Run(ctx, cmd)
	return modeldeploy.ShellResult{Stdout: res.Stdout, Stderr: res.Stderr, ExitCode: res.ExitCode, Truncated: res.Truncated}, err
}

func (r runnerShell) Close() error { return r.r.Close() }

func (d *deployRuntime) Dial(ctx context.Context, nodeID int64) (modeldeploy.Shell, error) {
	r, err := d.s.dialLeaseNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	return runnerShell{r}, nil
}

func (d *deployRuntime) LeaseRuntime(ctx context.Context, leaseID int64) (*modeldeploy.LeaseRuntime, error) {
	s := d.s
	l, err := s.repo.getLease(s.repo.db, leaseID)
	if err != nil || l == nil {
		return nil, err
	}
	if l.DeliveryKind != "container" {
		// 提交时已拒绝(ErrBareMetalLease); 这里兜底: 整机没有租约容器/网络, 不能按容器租约下发引擎或归还配额。
		return nil, &modeldeploy.AbortError{Msg: fmt.Sprintf("租约 %s 为整机交付(%s), 一键部署仅支持容器租约", l.LeaseNo, l.DeliveryKind)}
	}
	n, err := s.repo.getNode(s.repo.db, l.NodeID)
	if err != nil {
		return nil, err
	}
	if n == nil {
		return nil, fmt.Errorf("租约 %s 的节点 #%d 不存在", l.LeaseNo, l.NodeID)
	}
	slots, err := s.repo.leaseSlotsOnNode(s.repo.db, l.ID, l.NodeID)
	if err != nil {
		return nil, err
	}
	lr := &modeldeploy.LeaseRuntime{LeaseID: l.ID, LeaseNo: l.LeaseNo, State: l.State, NodeID: l.NodeID,
		Container: containerNameOf(l.LeaseNo), Network: networkNameOf(l.LeaseNo)}
	if l.ContainerName != nil && *l.ContainerName != "" {
		lr.Container = *l.ContainerName
	}
	for _, sl := range slots {
		lr.SlotUUIDs = append(lr.SlotUUIDs, sl.GPUUUID)
		if sl.State == SlotFaulty {
			lr.FaultyUUIDs = append(lr.FaultyUUIDs, sl.GPUUUID)
		}
	}
	if n.CPUCores == nil || n.MemGB == nil {
		return nil, fmt.Errorf("节点 #%d 缺少 CPU/内存实测值, 无法计算租约容器配额", n.ID)
	}
	// 与开通时 docker run --cpus/--memory 同一算法(provision.go container ④)。
	if lr.CPUs, lr.MemoryGB, err = cpuMemSplit(*n.CPUCores, *n.MemGB, s.cfg.HostReserveCPU, s.cfg.HostReserveMemGB, l.Cards, n.TotalCards); err != nil {
		return nil, err
	}
	return lr, nil
}

func (d *deployRuntime) Enqueue(ctx context.Context, tx *sqlx.Tx, spec modeldeploy.TaskSpec) error {
	raw, err := json.Marshal(spec.Payload)
	if err != nil {
		return err
	}
	prio, maxA := spec.Priority, spec.MaxAttempts
	if prio <= 0 {
		prio = 5
	}
	if maxA <= 0 {
		maxA = 3
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO node_tasks (node_id, lease_id, deployment_id, kind, state, priority, max_attempts, payload)
		VALUES (?,?,?,?,'pending',?,?,?)`, spec.NodeID, spec.LeaseID, spec.DeploymentID, spec.Kind, prio, maxA, string(raw))
	return err
}

type cardRow struct {
	UUID        string     `db:"gpu_uuid"`
	Bus         string     `db:"pci_bus_id"`
	TempC       *int       `db:"temp_c"`
	Throttle    *uint64    `db:"throttle_reasons"`
	ECCUncorr   *int64     `db:"ecc_uncorrected"`
	ECCCorr     *int64     `db:"ecc_corrected"`
	TelemetryAt *time.Time `db:"telemetry_at"`
}

// CardHealth 轻探活写入 gpu_slots 的逐卡遥测 + 近 1 小时致命 Xid。遥测超过 3 个轻探活周期未更新的卡不填读数(未知)。
func (d *deployRuntime) CardHealth(ctx context.Context, nodeID int64, uuids []string) ([]modeldeploy.CardHealth, error) {
	if len(uuids) == 0 {
		return nil, nil
	}
	q, args, err := sqlx.In(`SELECT gpu_uuid, pci_bus_id, temp_c, throttle_reasons, ecc_uncorrected, ecc_corrected, telemetry_at
		FROM gpu_slots WHERE node_id=? AND gpu_uuid IN (?)`, nodeID, uuids)
	if err != nil {
		return nil, err
	}
	var rows []cardRow
	if err := d.s.repo.db.SelectContext(ctx, &rows, q, args...); err != nil {
		return nil, err
	}
	now := time.Now()
	fatal, err := d.s.repo.recentFatalXids(nodeID, now.Add(-fatalXidHold))
	if err != nil {
		return nil, err
	}
	out := make([]modeldeploy.CardHealth, 0, len(rows))
	for _, r := range rows {
		c := modeldeploy.CardHealth{UUID: r.UUID, FatalXid: len(fatal[NormalizeBusID(r.Bus)]) > 0, TelemetryAt: r.TelemetryAt}
		if fresh(r.TelemetryAt, now, snapshotLightFresh) {
			c.TempC, c.ECCUncorrected, c.ECCCorrected = r.TempC, r.ECCUncorr, r.ECCCorr
			if r.Throttle != nil {
				t := *r.Throttle&throttleFailMask != 0
				c.Throttled = &t
			}
		}
		out = append(out, c)
	}
	return out, nil
}

func (d *deployRuntime) ListeningPorts(ss string) map[int]bool { return ParseListeningPorts(ss) }

func (d *deployRuntime) Settings() modeldeploy.RuntimeSettings {
	c := d.s.cfg
	return modeldeploy.RuntimeSettings{CacheDir: c.ModelCacheDir, PullImage: c.ModelPullImage,
		PortStart: c.EnginePortStart, PortEnd: c.EnginePortEnd,
		GatewayHost: strings.TrimSpace(d.s.gw.PublicHost), GatewayPort: d.s.gw.PublicPort}
}

// modelSampleCleanupEvery 健康采样清理周期(保留 7 天, docs/27 §27.5)。
const modelSampleCleanupEvery = 24 * time.Hour

func (s *Service) modelHealthRound(ctx context.Context) {
	if err := s.modelDeploy.HealthRound(ctx); err != nil {
		slog.Error("一键部署健康检测轮失败", "error", err)
	}
}

func (s *Service) modelSampleCleanup(_ context.Context) {
	n, err := s.modelDeploy.CleanupSamples(time.Now())
	if err != nil {
		slog.Error("一键部署健康采样清理失败", "error", err)
		return
	}
	slog.Info("一键部署健康采样已清理", "deleted", n)
}

// removeLeaseEnginesPrefix 租约容器停止/清除前先删该租约的引擎容器(按标签; 引擎持有租约的卡且接在租约网络上,
// 不删则卡不释放、网络删不掉)。部署状态由 StopForLeaseTx 同事务迁移, 这里只保证节点上不残留。
func removeLeaseEnginesPrefix(leaseNo string) string {
	if !ValidLeaseNo(leaseNo) {
		return ""
	}
	return "docker ps -aq --filter label=omnis.lease=" + leaseNo + " --filter label=omnis.deploy | xargs -r docker rm -f >/dev/null 2>&1; "
}
