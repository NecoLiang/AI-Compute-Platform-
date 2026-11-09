package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"tokenfactory/internal/catalog"
	"tokenfactory/internal/modeldeploy"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// T-098/T-099 一键部署运行期集成测试: 真实 MySQL + fakeNode(真实 SSH) + fleet 任务队列, 与 main 同样接线
// (AttachModelDeploy + SetDeployRuntime)。节点命令按前缀给出应答, 其余命令走 fakeNode 默认应答。

const (
	testPullImage   = "registry.example.com/omnis/pull@sha256:" + "1111111111111111111111111111111111111111111111111111111111111111"
	mdCacheDir      = "/data/omnis-models"
	mdModelKey      = "qwen-test-7b"
	mdRevision      = "0123456789abcdef0123456789abcdef01234567" // revisionPattern: 40/64 位十六进制提交号
	mdModelDir      = mdCacheDir + "/" + mdModelKey + "@" + mdRevision
	mdBuyer         = int64(21)
	mdPortStart     = 31000
	mdPortEnd       = 31002
	mdWeightsBytes  = int64(16000000000)
	mdManifestFiles = `[{"path":"config.json","size":10,"sha256":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},` +
		`{"path":"model.safetensors","size":15999999990,"sha256":"bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"}]`
)

type mdEnv struct {
	*provEnv
	md      *modeldeploy.Service
	modelID int64
	wantSum string // 完成标记摘要 = 校验清单的 sha256
}

// newMDEnv active 租约(2 卡) + 新鲜探活数据(F3–F6 可判定) + 一条可部署的目录条目。
func newMDEnv(t *testing.T) *mdEnv {
	t.Helper()
	e := newProvEnv(t, true)
	e.outcome(t, runOne(t, e.s))
	ctx := context.Background()
	e.db.MustExec(`UPDATE supplier_nodes SET driver_version='550.54.15' WHERE id=?`, e.node)
	e.db.MustExec(`UPDATE gpu_catalog SET compute_capability='9.0' WHERE model_name='H100-80G'`)
	n, err := e.s.repo.GetNode(e.node)
	require.NoError(t, err)
	e.s.lightProbe(ctx, n)
	matrix, err := ParseTopoMatrix(fakeTopo, 2)
	require.NoError(t, err)
	require.NoError(t, e.s.recordTopo(e.node, n.Generation, []GPU{{Index: 0, UUID: fakeUUID0}, {Index: 1, UUID: fakeUUID1}}, matrix, time.Now()))

	e.s.cfg.ModelPullImage = testPullImage
	e.s.cfg.EnginePortStart, e.s.cfg.EnginePortEnd = mdPortStart, mdPortEnd
	modelID := insertTestModel(t, e.db, mdModelKey, mdWeightsBytes)
	e.db.MustExec(`UPDATE model_catalog SET manifest=?, revision=? WHERE id=?`, mdManifestFiles, mdRevision, modelID)

	md := modeldeploy.NewService(modeldeploy.NewRepository(e.db), catalog.NewService(catalog.NewRepository(e.db)))
	md.SetLeaseSource(e.s.ModelDeployLeaseSource())
	md.SetDeployRuntime(e.s.AttachModelDeploy(md), nil, nil)

	checklist := strings.Repeat("a", 64) + "  config.json\n" + strings.Repeat("b", 64) + "  model.safetensors\n"
	sum := sha256.Sum256([]byte(checklist))
	me := &mdEnv{provEnv: e, md: md, modelID: modelID, wantSum: hex.EncodeToString(sum[:])}
	me.nodeHappy(t)
	return me
}

// liveReply 实时检查应答: 两卡显存几乎为空, listening 为额外的 127.0.0.1 监听端口。
func liveReply(listening ...int) fakeReply {
	var b strings.Builder
	b.WriteString("@@smi\n" + fakeUUID0 + ", 1, 81559\n" + fakeUUID1 + ", 1, 81559\n@@df\n3298534883328\n@@ports\n")
	b.WriteString("LISTEN 0      4096         0.0.0.0:22         0.0.0.0:*\n")
	for _, p := range listening {
		fmt.Fprintf(&b, "LISTEN 0      4096       127.0.0.1:%d       0.0.0.0:*\n", p)
	}
	b.WriteString("@@end\n")
	return fakeReply{Stdout: b.String()}
}

// nodeHappy 部署流水线各条节点命令的成功应答(引擎相关按部署号另设, 见 engineOK)。
func (e *mdEnv) nodeHappy(t *testing.T) {
	t.Helper()
	f := e.fake
	f.Override("echo @@smi; nvidia-smi --query-gpu=uuid,memory.used", liveReply())
	f.Override("if [ -f "+mdModelDir+"/.omnis-manifest.sha256", fakeReply{Stdout: e.wantSum + "\n"})
	f.Override("mkdir -p "+mdCacheDir+" && df -B1", fakeReply{Stdout: "3298534883328\n"})
	f.Override("mkdir -p "+mdCacheDir+"/.omnis-pull && : > ", fakeReply{})
	f.Override("printf %s ", fakeReply{})
	f.Override("sha256sum < "+mdCacheDir+"/.omnis-pull/", fakeReply{Stdout: e.wantSum + "\n"})
	f.Override("docker rm -f omnis-pull-m", fakeReply{Stdout: "@@started\n"})
	f.Override("if s=$(docker inspect --type container -f '{{.State.Status}} {{.State.ExitCode}}' omnis-pull-m",
		fakeReply{Stdout: "@@pull exited 0\n@@bytes 16000000000\n"})
	f.Override("docker logs --tail 30 omnis-pull-m", fakeReply{Stdout: "Downloading...\n@@done\n@@end\n"})
	f.Override("if docker inspect --type container omnis-l-", fakeReply{Stdout: "@@updated\n"})
	f.Override("docker image inspect vllm/vllm-openai@", fakeReply{Stdout: "@@image\n"})
}

// engineOK 某部署的引擎容器: 删除旧容器(不存在) + docker run 成功 + 日志。
func (e *mdEnv) engineOK(deployNo string) {
	c := modeldeploy.EngineContainer(deployNo)
	e.fake.Override("if docker inspect --type container "+c+" >/dev/null 2>&1; then docker rm -f", fakeReply{Stdout: "@@absent\n"})
	e.fake.Override("if docker inspect --type container "+c+" >/dev/null 2>&1; then docker logs", fakeReply{Stdout: "INFO engine log line\n"})
	e.fake.Override("docker run -d --name "+c+" ", fakeReply{Stdout: strings.Repeat("e", 64) + "\n"})
}

func (e *mdEnv) submit(t *testing.T, canary bool) *modeldeploy.DeploymentView {
	t.Helper()
	v, err := e.md.Submit(context.Background(), modeldeploy.SubmitRequest{BuyerID: mdBuyer, OrderNo: e.order.OrderNo,
		ModelID: e.modelID, TP: 2, DP: 1, GPUMemUtilPct: 90, CanaryEnabled: canary, LicenseAck: true, UntestedAck: true})
	require.NoError(t, err)
	e.engineOK(v.DeployNo)
	return v
}

type depRow struct {
	ID             int64   `db:"id"`
	State          string  `db:"state"`
	Health         string  `db:"health"`
	HealthReason   *string `db:"health_reason"`
	FailReason     *string `db:"fail_reason"`
	EnginePort     *int    `db:"engine_port"`
	PortClaim      *int    `db:"engine_port_claim"`
	EngineCPUs     *int    `db:"engine_cpus"`
	EngineMemGB    *int    `db:"engine_memory_gb"`
	LeaseCPUs      *int    `db:"lease_cpus"`
	LeaseMemGB     *int    `db:"lease_memory_gb"`
	RestartCount   int     `db:"restart_count"`
	NextRestartSet bool    `db:"next_restart_set"`
	LogTail        *string `db:"log_tail"`
	StopReason     *string `db:"stop_reason"`
}

func (e *mdEnv) dep(t *testing.T, deployNo string) depRow {
	t.Helper()
	var r depRow
	require.NoError(t, e.db.Get(&r, `SELECT id, state, health, health_reason, fail_reason, engine_port, engine_port_claim,
		engine_cpus, engine_memory_gb, lease_cpus, lease_memory_gb, restart_count, next_restart_at IS NOT NULL AS next_restart_set, log_tail, stop_reason
		FROM model_deployments WHERE deploy_no=?`, deployNo))
	return r
}

// runKind 认领并执行下一个任务, 断言其类型。
func (e *mdEnv) runKind(t *testing.T, kind string) *Task {
	t.Helper()
	task := runOne(t, e.s)
	require.NotNil(t, task, "期望执行 %s, 队列为空", kind)
	require.Equal(t, kind, task.Kind, "last_error=%v", task.LastError)
	return task
}

func succeeded(t *testing.T, task *Task) {
	t.Helper()
	require.Equal(t, TaskSucceeded, task.State, "kind=%s last_error=%v", task.Kind, task.LastError)
}

// toStarting 提交后走完 拉取 → 启动, 返回 starting 的部署。
func (e *mdEnv) toStarting(t *testing.T, canary bool) *modeldeploy.DeploymentView {
	t.Helper()
	v := e.submit(t, canary)
	succeeded(t, e.runKind(t, modeldeploy.TaskDeployModel)) // checking → pulling, 入队 pull_model
	assert.Equal(t, "pulling", e.dep(t, v.DeployNo).State)
	succeeded(t, e.runKind(t, modeldeploy.TaskPullModel)) // 启动 detached 拉取容器
	require.NoError(t, e.md.PollPulls(context.Background()))
	succeeded(t, e.runKind(t, modeldeploy.TaskDeployModel)) // 缓存就绪 → 配额切分 → docker run
	require.Equal(t, "starting", e.dep(t, v.DeployNo).State)
	return v
}

func (e *mdEnv) cmds(sub string) []string {
	var out []string
	for _, c := range e.fake.Commands() {
		if strings.Contains(c, sub) {
			out = append(out, c)
		}
	}
	return out
}

func errCode(err error) int {
	var be *modeldeploy.Error
	if errors.As(err, &be) {
		return be.Code
	}
	return 0
}

// 并发提交: 同一租约只有一个成功(uk_lease_active / 未终止部署检查), 其余 40900; 只入队一个 deploy_model。
func TestModelDeployConcurrentSubmitOnce(t *testing.T) {
	e := newMDEnv(t)
	const n = 6
	var wg sync.WaitGroup
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = e.md.Submit(context.Background(), modeldeploy.SubmitRequest{BuyerID: mdBuyer, OrderNo: e.order.OrderNo,
				ModelID: e.modelID, TP: 2, DP: 1, GPUMemUtilPct: 90, CanaryEnabled: true, LicenseAck: true, UntestedAck: true})
		}(i)
	}
	wg.Wait()
	ok := 0
	for _, err := range errs {
		if err == nil {
			ok++
			continue
		}
		assert.Equal(t, 40900, errCode(err), "%v", err)
	}
	assert.Equal(t, 1, ok)
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM model_deployments WHERE lease_id=?`, e.lease.ID))
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE kind='deploy_model' AND lease_id=?`, e.lease.ID))
}

// 全流水线: 单次拉取 → 配额对半切分(docker update 租约容器)→ 引擎 docker run 参数 → 健康检测转 running。
func TestModelDeployPipelineMemorySplit(t *testing.T) {
	e := newMDEnv(t)
	v := e.toStarting(t, true)
	d := e.dep(t, v.DeployNo)

	// 节点 128 核 / 1024 GB, 预留 2 核 / 8 GB, 租约占 2/2 卡 → 租约完整配额 126 核 / 1016 GB; 引擎取下半。
	require.NotNil(t, d.LeaseCPUs)
	assert.Equal(t, 126, *d.LeaseCPUs)
	assert.Equal(t, 1016, *d.LeaseMemGB)
	assert.Equal(t, 63, *d.EngineCPUs)
	assert.Equal(t, 508, *d.EngineMemGB)
	lease := containerNameOf(e.lease.LeaseNo)
	upd := e.cmds("docker update --cpus 63 --memory 508g --memory-swap 508g " + lease)
	assert.Len(t, upd, 1, "部署前缩减租约容器配额")
	// 引擎 + 缩减后的租约容器之和不超过租约完整配额(lease_* 记录完整配额, 停止时据此归还)。
	assert.LessOrEqual(t, *d.EngineMemGB+508, *d.LeaseMemGB, "--memory 之和 ≤ 租约配额")
	assert.LessOrEqual(t, *d.EngineCPUs+63, *d.LeaseCPUs, "--cpus 之和 ≤ 租约配额")

	runs := e.cmds("docker run -d --name " + modeldeploy.EngineContainer(v.DeployNo))
	require.Len(t, runs, 1)
	run := runs[0]
	assert.Contains(t, run, `--gpus '"device=`+fakeUUID0+","+fakeUUID1+`"'`, "只用本租约的卡")
	assert.Contains(t, run, "--label omnis.lease="+e.lease.LeaseNo+" --label omnis.deploy="+v.DeployNo)
	assert.Contains(t, run, "--network "+networkNameOf(e.lease.LeaseNo)+" --network-alias omnis-engine")
	assert.Contains(t, run, fmt.Sprintf("-p 127.0.0.1:%d:8000", mdPortStart), "端口只绑回环, 取区间首个空闲")
	assert.Contains(t, run, "--cpus 63 --memory 508g --memory-swap 508g")
	assert.Contains(t, run, "-v "+mdModelDir+":/model:ro")
	assert.Contains(t, run, "vllm/vllm-openai@sha256:", "镜像按 digest")
	assert.Contains(t, run, "--tensor-parallel-size 2 --data-parallel-size 1")
	require.NotNil(t, d.EnginePort)
	assert.Equal(t, mdPortStart, *d.EnginePort)

	// 拉取: 固定 revision 下载到 .partial, 校验后原子改名; 缓存条目 ready。
	pulls := e.cmds("modelscope download --model Qwen/Qwen2.5-7B-Instruct --revision " + mdRevision)
	require.Len(t, pulls, 1)
	assert.Contains(t, pulls[0], "sha256sum -c --strict")
	assert.Contains(t, pulls[0], `mv "$P" "$D"`)
	assert.Contains(t, pulls[0], "docker run -d --name omnis-pull-m")
	var cacheState string
	require.NoError(t, e.db.Get(&cacheState, `SELECT state FROM node_model_cache WHERE node_id=? AND model_id=?`, e.node, e.modelID))
	assert.Equal(t, "ready", cacheState)

	// 健康检测: L0–L3 全过 → running, 写采样。
	e.fake.Override("echo @@inspect", fakeReply{Stdout: "@@inspect\nrunning|false|0\n@@health\n200\n@@models\nok\n@@canary\n200 0.150\n@@end\n"})
	require.NoError(t, e.md.HealthRound(context.Background()))
	d = e.dep(t, v.DeployNo)
	assert.Equal(t, "running", d.State)
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM deployment_health_samples WHERE deployment_id=?`, d.ID))
	probe := e.cmds("echo @@inspect")
	require.NotEmpty(t, probe)
	assert.Contains(t, probe[0], fmt.Sprintf("http://127.0.0.1:%d/health", mdPortStart))

	// 买家停止: stopping → stop_model 删除引擎并归还租约完整配额 → stopped, 端口释放。
	_, err := e.md.Stop(context.Background(), mdBuyer, e.order.OrderNo, v.DeployNo)
	require.NoError(t, err)
	assert.Equal(t, "stopping", e.dep(t, v.DeployNo).State)
	e.fake.Override("if docker inspect --type container "+modeldeploy.EngineContainer(v.DeployNo)+" >/dev/null 2>&1; then docker rm -f",
		fakeReply{Stdout: "@@removed\n"})
	succeeded(t, e.runKind(t, modeldeploy.TaskStopModel))
	d = e.dep(t, v.DeployNo)
	assert.Equal(t, "stopped", d.State)
	assert.Nil(t, d.PortClaim)
	assert.Len(t, e.cmds("docker update --cpus 126 --memory 1016g --memory-swap 1016g "+lease), 1, "停止后归还完整配额")
}

// docker update 失败: 部署失败(经 stopping → stop_model → failed), 不启动引擎。
func TestModelDeployDockerUpdateFails(t *testing.T) {
	e := newMDEnv(t)
	v := e.submit(t, false)
	succeeded(t, e.runKind(t, modeldeploy.TaskDeployModel))
	succeeded(t, e.runKind(t, modeldeploy.TaskPullModel))
	require.NoError(t, e.md.PollPulls(context.Background()))

	lease := containerNameOf(e.lease.LeaseNo)
	e.fake.Override("if docker inspect --type container "+lease+" >/dev/null 2>&1; then docker update --cpus 63",
		fakeReply{Stderr: "Error response from daemon: Cannot update container: memory limit should be larger than already set memory usage\n", Code: 1})
	e.fake.Override("if docker inspect --type container "+lease+" >/dev/null 2>&1; then docker update --cpus 126", fakeReply{Stdout: "@@updated\n"})
	e.fake.mu.Lock()
	delete(e.fake.overrides, "if docker inspect --type container omnis-l-")
	e.fake.mu.Unlock()
	succeeded(t, e.runKind(t, modeldeploy.TaskDeployModel))
	d := e.dep(t, v.DeployNo)
	assert.Equal(t, "stopping", d.State)
	require.NotNil(t, d.FailReason)
	assert.Contains(t, *d.FailReason, "docker update")
	assert.Contains(t, *d.FailReason, "memory limit should be larger")
	assert.Empty(t, e.cmds("docker run -d --name "+modeldeploy.EngineContainer(v.DeployNo)), "配额失败不启动引擎")

	succeeded(t, e.runKind(t, modeldeploy.TaskStopModel))
	d = e.dep(t, v.DeployNo)
	assert.Equal(t, "failed", d.State)
	assert.Len(t, e.cmds("docker update --cpus 126 --memory 1016g --memory-swap 1016g "+lease), 1, "失败后仍归还完整配额")
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM model_deployments WHERE lease_active_claim IS NOT NULL`), "释放租约占用, 可重新提交")
}

// drain 依次执行队列中的任务; 队列空时跑一轮拉取轮询(可能入队 deploy_model)再看一次。返回执行过的任务类型。
func (e *mdEnv) drain(t *testing.T) []string {
	t.Helper()
	var kinds []string
	for i := 0; i < 20; i++ {
		task := runOne(t, e.s)
		if task == nil {
			require.NoError(t, e.md.PollPulls(context.Background()))
			if task = runOne(t, e.s); task == nil {
				return kinds
			}
		}
		kinds = append(kinds, task.Kind)
	}
	t.Fatalf("队列未在 20 个任务内清空: %v", kinds)
	return nil
}

// 同节点同模型并发拉取: 多个 deploy_model 执行体同时走到缓存认领, 只建一条缓存条目、只入队一个 pull_model、只下载一次。
func TestModelDeployConcurrentPullSingleDownload(t *testing.T) {
	e := newMDEnv(t)
	v := e.submit(t, false)
	d := e.dep(t, v.DeployNo)
	// 跳过 pending→checking 的守卫(该守卫本身只放行一个), 让所有执行体都进入缓存认领。
	e.db.MustExec(`UPDATE model_deployments SET state='checking' WHERE id=?`, d.ID)

	const n = 6
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = e.md.RunDeploy(context.Background(), d.ID) // 并发冲突返回可重试错误, 由任务队列重试
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM node_model_cache WHERE node_id=? AND model_id=?`, e.node, e.modelID))
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE kind='pull_model' AND node_id=?`, e.node))
	assert.Equal(t, "pulling", e.dep(t, v.DeployNo).State)

	// 队列中 Submit 入队的 deploy_model 再次执行: 缓存下载中, 不再入队拉取。
	e.drain(t)
	assert.Equal(t, "starting", e.dep(t, v.DeployNo).State)
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE kind='pull_model' AND node_id=?`, e.node))
	assert.Len(t, e.cmds("modelscope download --model Qwen/Qwen2.5-7B-Instruct"), 1, "只下载一次")
}

// 引擎端口唯一: 跳过库内已被同节点其它部署占用的端口与节点上已在监听的端口; uk_node_engine_port 兜底。
func TestModelDeployEnginePortUnique(t *testing.T) {
	e := newMDEnv(t)
	// 同节点另一部署(其它租约)已占 31000。
	e.db.MustExec(`INSERT INTO model_deployments (deploy_no, lease_id, order_id, buyer_id, node_id, model_id, gpu_uuids, tp, dp,
		max_model_len, gpu_mem_util, engine_port, engine_port_claim, state, feasibility)
		VALUES ('d-neighbor1', 999999, 999999, 99, ?, ?, '[]', 1, 1, 4096, 0.90, ?, ?, 'running', '{}')`,
		e.node, e.modelID, mdPortStart, mdPortStart)
	// 节点上 31001 已被其它进程监听。
	e.fake.Override("echo @@smi; nvidia-smi --query-gpu=uuid,memory.used", liveReply(mdPortStart+1))

	v := e.toStarting(t, false)
	d := e.dep(t, v.DeployNo)
	require.NotNil(t, d.PortClaim)
	assert.Equal(t, mdPortEnd, *d.PortClaim)
	assert.Equal(t, mdPortEnd, *d.EnginePort)
	assert.Len(t, e.cmds(fmt.Sprintf("-p 127.0.0.1:%d:8000", mdPortEnd)), 1)

	_, err := e.db.Exec(`UPDATE model_deployments SET engine_port_claim=? WHERE deploy_no='d-neighbor1'`, mdPortEnd)
	require.Error(t, err, "同节点同端口第二次占用应被唯一键拒绝")
	assert.Contains(t, err.Error(), "uk_node_engine_port")

	// 区间已满: 新部署确定性失败, 不启动引擎。
	_, err = e.md.Stop(context.Background(), mdBuyer, e.order.OrderNo, v.DeployNo)
	require.NoError(t, err)
	e.fake.Override("if docker inspect --type container "+modeldeploy.EngineContainer(v.DeployNo)+" >/dev/null 2>&1; then docker rm -f",
		fakeReply{Stdout: "@@removed\n"})
	succeeded(t, e.runKind(t, modeldeploy.TaskStopModel))
	e.db.MustExec(`UPDATE model_deployments SET engine_port_claim=?, engine_port=? WHERE deploy_no='d-neighbor1'`, mdPortEnd, mdPortEnd)
	e.db.MustExec(`INSERT INTO model_deployments (deploy_no, lease_id, order_id, buyer_id, node_id, model_id, gpu_uuids, tp, dp,
		max_model_len, gpu_mem_util, engine_port, engine_port_claim, state, feasibility)
		VALUES ('d-neighbor2', 999998, 999998, 99, ?, ?, '[]', 1, 1, 4096, 0.90, ?, ?, 'running', '{}')`,
		e.node, e.modelID, mdPortStart, mdPortStart)
	v2 := e.submit(t, false)
	e.drain(t)
	d2 := e.dep(t, v2.DeployNo)
	assert.Equal(t, "failed", d2.State)
	require.NotNil(t, d2.FailReason)
	assert.Contains(t, *d2.FailReason, "已无可用端口")
	assert.Empty(t, e.cmds("docker run -d --name "+modeldeploy.EngineContainer(v2.DeployNo)))
}

// 采样清理只删 7 天前(含所有部署), 边界内的保留。
func TestModelDeploySampleCleanup(t *testing.T) {
	e := newMDEnv(t)
	v := e.submit(t, false)
	id := e.dep(t, v.DeployNo).ID
	now := time.Now().Truncate(time.Second)
	for _, at := range []time.Time{
		now.Add(-30 * 24 * time.Hour),
		now.Add(-modeldeploy.SampleRetention - time.Minute),
		now.Add(-modeldeploy.SampleRetention + time.Minute),
		now.Add(-time.Hour),
	} {
		e.db.MustExec(`INSERT INTO deployment_health_samples (deployment_id, sampled_at, container_up, health) VALUES (?, ?, 1, 'healthy')`, id, at)
	}
	e.db.MustExec(`INSERT INTO deployment_health_samples (deployment_id, sampled_at, container_up, health) VALUES (?, ?, 1, 'healthy')`,
		id+1000, now.Add(-8*24*time.Hour))

	n, err := e.md.CleanupSamples(now)
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)
	var kept []time.Time
	require.NoError(t, e.db.Select(&kept, `SELECT sampled_at FROM deployment_health_samples ORDER BY sampled_at`))
	require.Len(t, kept, 2)
	assert.True(t, kept[0].Equal(now.Add(-modeldeploy.SampleRetention+time.Minute)), "%v", kept[0])
	assert.True(t, kept[1].Equal(now.Add(-time.Hour)), "%v", kept[1])
}

// 租约冻结 / 到期: fleet 在租约状态迁移的同一事务内把部署置 stopping 并入队 stop_model, 执行后 stopped。
func TestModelDeployStopsWithLease(t *testing.T) {
	cases := []struct {
		name, reason string
		trigger      func(t *testing.T, e *mdEnv)
	}{
		{"freeze", modeldeploy.StopLeaseFrozen, func(t *testing.T, e *mdEnv) {
			require.NoError(t, e.comp.FreezeOrder(e.order.OrderNo))
		}},
		{"expire", modeldeploy.StopLeaseExpired, func(t *testing.T, e *mdEnv) {
			expireLease(t, e.provEnv)
			e.s.expireActive(context.Background())
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := newMDEnv(t)
			v := e.toStarting(t, false)
			tc.trigger(t, e)
			d := e.dep(t, v.DeployNo)
			assert.Equal(t, "stopping", d.State)
			require.NotNil(t, d.StopReason)
			assert.Equal(t, tc.reason, *d.StopReason)
			assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE kind='stop_model' AND lease_id=?`, e.lease.ID))

			e.fake.Override("if docker inspect --type container "+modeldeploy.EngineContainer(v.DeployNo)+" >/dev/null 2>&1; then docker rm -f",
				fakeReply{Stdout: "@@removed\n"})
			e.drain(t)
			d = e.dep(t, v.DeployNo)
			assert.Equal(t, "stopped", d.State)
			assert.Nil(t, d.PortClaim)
			assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM model_deployments WHERE lease_active_claim IS NOT NULL`))
		})
	}
}
