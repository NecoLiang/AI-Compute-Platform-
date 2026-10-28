package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"tokenfactory/internal/compute"
	"tokenfactory/internal/notification"
	"tokenfactory/internal/scheduler"
	"tokenfactory/pkg/config"
	"tokenfactory/pkg/errcode"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 端到端: 真实 MySQL(TEST_MYSQL_DSN, 每次新建库并跑全部迁移) + 真实 compute/scheduler/notification 服务
// + 本地测试 SSH 服务器(真实 x/crypto/ssh 握手, 命令输出按真实格式)。不触碰任何真实节点。

func setupFleetDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN is required for fleet integration tests")
	}
	cfg, err := mysql.ParseDSN(dsn)
	require.NoError(t, err)
	cfg.DBName = ""
	loc, err := time.LoadLocation("Asia/Shanghai")
	require.NoError(t, err)
	cfg.Loc = loc
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["time_zone"] = "'+08:00'"
	root, err := sqlx.Connect("mysql", cfg.FormatDSN())
	require.NoError(t, err)
	name := fmt.Sprintf("omnis_fleet_test_%d", time.Now().UnixNano())
	root.MustExec("CREATE DATABASE " + name + " CHARACTER SET utf8mb4")
	cfg.DBName, cfg.ParseTime, cfg.MultiStatements = name, true, true
	db, err := sqlx.Connect("mysql", cfg.FormatDSN())
	require.NoError(t, err)
	t.Cleanup(func() { db.Close(); root.Exec("DROP DATABASE " + name); root.Close() })
	paths, err := filepath.Glob("../../migrations/*.up.sql")
	require.NoError(t, err)
	require.NotEmpty(t, paths)
	for _, path := range paths {
		data, err := os.ReadFile(path)
		require.NoError(t, err)
		_, err = db.Exec(string(data))
		require.NoError(t, err, path)
	}
	return db
}

type stubGateway struct {
	mu    sync.Mutex
	ports []int
	err   error
}

func (g *stubGateway) SelfTestPort(_ context.Context, _ int64, _ string, port int) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.ports = append(g.ports, port)
	return g.err
}

// syncBuffer slog 输出捕获(多 goroutine 写)。
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

const testProbeImage = "registry.example.com/omnis/probe@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newTestFleet(t *testing.T, db *sqlx.DB) *Service {
	t.Helper()
	comp := compute.NewService(compute.NewRepository(db), db)
	sched := scheduler.NewService(scheduler.NewRepository(db))
	notif := notification.NewService(notification.NewRepository(db))
	cfg := config.FleetConfig{
		Enabled: true, Workers: 1, ControllerEgressIPs: []string{"10.0.0.1"},
		ProbeImage: testProbeImage, MinDriverVersion: "535.104",
		Probe: config.FleetProbe{MinPerfRatio: 0.7},
	}
	key := bytes.Repeat([]byte{7}, 32)
	s := NewService(NewRepository(db), Deps{Products: comp, Health: sched, Notifier: notif}, cfg,
		config.AccessGWConfig{EgressIPs: []string{"10.0.0.2"}}, key)
	s.dialer = &Dialer{AllowLoopback: true, DialTimeout: 2 * time.Second, Attempts: 1}
	s.benchSecs, s.sampleN, s.sampleGap = 5, 1, 1
	return s
}

// runTasks 同步执行所有到期的 probe 任务(与 worker 同一代码路径), 返回执行后的任务行。
func runTasks(t *testing.T, s *Service) []Task {
	t.Helper()
	var done []Task
	for i := 0; i < 10; i++ {
		task, err := s.repo.claimNext([]string{KindProbe}, probeLock)
		require.NoError(t, err)
		if task == nil {
			break
		}
		s.execute(context.Background(), task)
		got, err := s.repo.getTask(s.repo.db, task.ID)
		require.NoError(t, err)
		done = append(done, *got)
	}
	return done
}

func outcomeOf(t *testing.T, task Task) ProbeOutcome {
	t.Helper()
	require.Equal(t, TaskSucceeded, task.State, "任务应成功执行, last_error=%v", task.LastError)
	var o ProbeOutcome
	require.NoError(t, json.Unmarshal(task.Result, &o))
	return o
}

func codeOf(err error) int { return errcode.CodeOf(err, 0) }

func countRows(t *testing.T, db *sqlx.DB, q string, args ...any) int {
	t.Helper()
	var n int
	require.NoError(t, db.Get(&n, q, args...))
	return n
}

func productStock(t *testing.T, db *sqlx.DB) (int, string) {
	t.Helper()
	var r struct {
		Stock  int    `db:"stock"`
		Status string `db:"status"`
	}
	require.NoError(t, db.Get(&r, `SELECT stock, status FROM products WHERE id=1`))
	return r.Stock, r.Status
}

func latestChecks(t *testing.T, db *sqlx.DB, nodeID int64) (string, map[string]Check) {
	t.Helper()
	var r ProbeReport
	require.NoError(t, db.Get(&r, `SELECT id, node_id, kind, result, checks, inventory, created_at FROM node_probe_reports
		WHERE node_id=? ORDER BY id DESC LIMIT 1`, nodeID))
	var list []Check
	require.NoError(t, json.Unmarshal(r.Checks, &list))
	m := map[string]Check{}
	for _, c := range list {
		m[c.ID] = c
	}
	return r.Result, m
}

func seedManaged(t *testing.T, db *sqlx.DB) {
	t.Helper()
	db.MustExec(`INSERT INTO users (id, phone, password_hash) VALUES (10,'13800000010','x'),(11,'13800000011','x')`)
	// 已审核上架、当前无库存的托管整机商品(sold_out ↔ active 由派生库存切换)。
	db.MustExec(`INSERT INTO products (id, supplier_id, gpu_model, card_count, delivery_mode, fulfillment_mode, pricing_mode,
		unit_price, stock, status, region) VALUES (1, 10, 'H100-80G', 2, 'bare_metal', 'managed', 'hourly', 1500, 0, 'sold_out', 'cn-north')`)
	db.MustExec(`UPDATE gpu_catalog SET probe_baseline_tflops=600 WHERE model_name='H100-80G'`)
	require.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM gpu_catalog WHERE model_name='H100-80G'`))
}

func TestFleetManagedNodeLifecycle(t *testing.T) {
	db := setupFleetDB(t)
	seedManaged(t, db)
	logs := &syncBuffer{}
	prevLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prevLogger) })

	s := newTestFleet(t, db)
	ctx := context.Background()
	const goodPW, badPW = "Boot-Once#7f3a9c", "Wrong-Guess#11aa"
	fake := newFakeNode(t, goodPW)

	// ① 登记节点: 生成专属管理密钥, 私钥加密入库。
	_, err := s.RegisterManagedNode(11, scheduler.ManagedNodeReq{ProductID: 1, NodeName: "x", TotalCards: 2})
	assert.Equal(t, errcode.NotFound, codeOf(err), "他人商品")
	v, err := s.RegisterManagedNode(10, scheduler.ManagedNodeReq{ProductID: 1, NodeName: "gpu-01", TotalCards: 2,
		LeasePortStart: 47000, LeasePortEnd: 47009})
	require.NoError(t, err)
	node := v.(*Node)
	id := node.ID
	require.NotNil(t, node.MgmtPubkey)
	assert.True(t, strings.HasPrefix(*node.MgmtPubkey, "ssh-ed25519 "))
	assert.NotContains(t, *node.MgmtPrivkeyEnc, "PRIVATE KEY", "私钥必须加密存储")
	assert.Equal(t, VerifyUnverified, node.VerifyStatus)
	assert.Equal(t, "ssh", node.MgmtMode)

	// ③ 上架说明
	ob, err := s.Onboarding(10, id)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(ob.AuthorizedKeysLine, `from="10.0.0.1",`))
	assert.Contains(t, ob.FirewallScripts["debian"], "--ctorigdstport 47000:47009")
	assert.Contains(t, ob.FirewallScripts["debian"], "-s 10.0.0.2 -j RETURN")
	_, err = s.Onboarding(11, id)
	assert.Equal(t, errcode.NotFound, codeOf(err), "不泄露他人节点")

	// ⑤ 错误的一次性密码: 连接信息已保存, 返回 40001, 审计记录失败, 公钥未写入。
	in := SSHInput{Host: "127.0.0.1", Port: fake.Port(), User: "root", BootstrapPassword: badPW}
	_, err = s.UpdateSSH(ctx, 10, id, in, "203.0.113.9")
	require.Error(t, err)
	assert.Equal(t, errcode.ParamInvalid, codeOf(err))
	assert.Equal(t, 0, fake.AuthorizedCount())
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM audit_logs WHERE action='node.bootstrap_password_used' AND after_value LIKE '%failed%'`))

	// 正确密码: 写入带 from= 的公钥行, 记录观测指纹(未确认)。
	in.BootstrapPassword = goodPW
	res, err := s.UpdateSSH(ctx, 10, id, in, "203.0.113.9")
	require.NoError(t, err)
	assert.Equal(t, "installed", res.Bootstrap)
	require.NotNil(t, res.HostKey.Fingerprint)
	assert.Equal(t, fake.Fingerprint(), *res.HostKey.Fingerprint)
	assert.False(t, res.HostKey.Confirmed)
	assert.Equal(t, 1, fake.AuthorizedCount())

	// ⑥ 发起探活: 首次连接停在 P2 等确认(TOFU), 不执行任何 GPU 命令。
	taskID, err := s.Verify(10, id)
	require.NoError(t, err)
	again, err := s.Verify(10, id)
	require.NoError(t, err)
	assert.Equal(t, taskID, again, "进行中重复提交返回同一任务")
	done := runTasks(t, s)
	require.Len(t, done, 1)
	o := outcomeOf(t, done[0])
	assert.False(t, o.Pass)
	assert.Equal(t, fake.Fingerprint(), o.AwaitingHostKey)
	n, _ := s.repo.GetNode(id)
	assert.Equal(t, VerifyUnverified, n.VerifyStatus)
	for _, c := range fake.Commands() {
		assert.False(t, strings.HasPrefix(c, "nvidia-smi") || strings.HasPrefix(c, "docker"), "指纹确认前不得执行探活命令: %s", c)
	}
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM notifications WHERE user_id=10 AND title='请确认节点主机指纹'`))

	// ⑦ 确认指纹: 不一致 40001; 一致则钉死并自动入队完整探活。
	_, err = s.ConfirmHostKey(10, id, "SHA256:not-the-right-one", "203.0.113.9")
	assert.Equal(t, errcode.ParamInvalid, codeOf(err))
	taskID, err = s.ConfirmHostKey(10, id, fake.Fingerprint(), "203.0.113.9")
	require.NoError(t, err)
	assert.NotZero(t, taskID)

	// 网关未接入(S2): P11 判不通过并写明原因, 其余项全部通过, 节点 failed, 不生成卡槽。
	done = runTasks(t, s)
	require.Len(t, done, 1)
	o = outcomeOf(t, done[0])
	assert.False(t, o.Pass)
	assert.Equal(t, []string{"P11"}, o.Failed)
	result, checks := latestChecks(t, db, id)
	assert.Equal(t, "fail", result)
	assert.Contains(t, checks["P11"].Detail, "尚未接入")
	for _, cid := range []string{"P1", "P2", "P3", "P4", "P5", "P6", "P7", "P8", "P9", "P10", "P12", "P13"} {
		assert.True(t, checks[cid].OK, "%s: %s", cid, checks[cid].Detail)
	}
	n, _ = s.repo.GetNode(id)
	assert.Equal(t, VerifyFailed, n.VerifyStatus)
	assert.Equal(t, "offline", n.Status)
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE node_id=?`, id))
	cmds := strings.Join(fake.Commands(), "\n")
	assert.Contains(t, cmds, "docker pull -q "+testProbeImage)
	assert.Contains(t, cmds, `--gpus '"device=`+fakeUUID0+","+fakeUUID1+`"'`)
	assert.Contains(t, cmds, "-p 47009:8000", "自检端口从段尾向下选")
	assert.Contains(t, cmds, "docker rm -f omnis-probe-", "临时容器必须清理")

	// 注入网关自检后再次探活: 通过, 生成 2 个空闲卡槽, 派生库存 2, 商品回到在售。
	gw := &stubGateway{}
	s.gateway = gw
	_, err = s.Verify(10, id)
	require.NoError(t, err)
	done = runTasks(t, s)
	require.Len(t, done, 1)
	o = outcomeOf(t, done[0])
	require.True(t, o.Pass, "failed=%v", o.Failed)
	assert.Equal(t, []int{47009}, gw.ports)
	n, _ = s.repo.GetNode(id)
	assert.Equal(t, VerifyVerified, n.VerifyStatus)
	assert.Equal(t, "online", n.Status)
	require.NotNil(t, n.DriverVersion)
	assert.Equal(t, "535.104.05", *n.DriverVersion)
	require.NotNil(t, n.DiskQuotaSupported)
	assert.True(t, *n.DiskQuotaSupported)
	slots, err := s.Slots(10, id)
	require.NoError(t, err)
	require.Len(t, slots, 2)
	for _, sl := range slots {
		assert.Equal(t, SlotFree, sl.State)
		assert.Equal(t, "g0-nvlink", sl.TopoGroup)
	}
	stock, status := productStock(t, db)
	assert.Equal(t, 2, stock)
	assert.Equal(t, "active", status)
	assert.NoError(t, s.CheckManagedSubmit(1, "H100-80G"))
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM audit_logs WHERE action='node.verified' AND target_id=?`, id))

	// 周期轻量探活: 在线心跳 + node_heartbeats。
	s.lightProbe(ctx, n)
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM node_heartbeats WHERE node_id=? AND available_cards=2`, id))

	// 刚通过完整探活: 不入队周期任务; 距上次完整探活超过 12h 才入队。
	s.schedulePeriodic(n)
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM node_tasks WHERE state='pending'`))
	db.MustExec(`UPDATE node_probe_reports SET created_at=NOW() - INTERVAL 13 HOUR WHERE node_id=?`, id)
	s.schedulePeriodic(n)
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM node_tasks WHERE state='pending' AND payload LIKE '%periodic%' AND payload LIKE '%full%'`))
	n, _ = s.repo.GetNode(id)
	assert.Equal(t, VerifyVerified, n.VerifyStatus, "周期完整探活期间节点保持 verified 继续接单")
	done = runTasks(t, s)
	require.Len(t, done, 1)
	assert.True(t, outcomeOf(t, done[0]).Pass)

	// 掉卡: 轻量探活发现 GPU1 消失 → 空闲卡槽 faulty, 库存 1, 通知供给方。
	fake.Override(cmdLight, fakeReply{Stdout: "0, " + fakeUUID0 + ", 37, 12, 41, 0\n"})
	s.lightProbe(ctx, n)
	fake.ClearOverrides()
	assert.Equal(t, SlotFaulty, slotState(t, db, id, 1))
	stock, _ = productStock(t, db)
	assert.Equal(t, 1, stock)
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM notifications WHERE user_id=10 AND title='节点有卡故障'`))

	// 重装系统(主机密钥变化): 轻量探活即转 key_mismatch, 停止分配, 写系统审计。
	newFP := fake.RotateHostKey()
	s.lightProbe(ctx, n)
	n, _ = s.repo.GetNode(id)
	assert.Equal(t, VerifyKeyMismatch, n.VerifyStatus)
	assert.Equal(t, "offline", n.Status)
	assert.Equal(t, newFP, *n.HostKeyFingerprint)
	assert.Nil(t, n.HostKeyConfirmedAt)
	stock, status = productStock(t, db)
	assert.Equal(t, 0, stock)
	assert.Equal(t, "sold_out", status)
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM audit_logs WHERE action='node.key_mismatch' AND target_id=? AND operator_id IS NULL`, id))
	_, err = s.Verify(10, id)
	assert.Equal(t, errcode.Conflict, codeOf(err), "指纹变化后须先确认新指纹")

	// 核对并确认新指纹 → 重新完整探活; 故障卡恢复健康后回到 free。
	_, err = s.ConfirmHostKey(10, id, newFP, "203.0.113.9")
	require.NoError(t, err)
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM audit_logs WHERE action='node.host_key_reconfirmed' AND target_id=?`, id))
	done = runTasks(t, s)
	require.Len(t, done, 1)
	require.True(t, outcomeOf(t, done[0]).Pass)
	assert.Equal(t, SlotFree, slotState(t, db, id, 1))
	stock, status = productStock(t, db)
	assert.Equal(t, 2, stock)
	assert.Equal(t, "active", status)

	// 压测不达标: GPU1 算力低于基准 70% → 该卡 faulty, 其余卡照常出租。
	fake.Override("docker run --rm --name omnis-probe-", fakeReply{Stdout: `{"results":[{"index":0,"uuid":"` + fakeUUID0 +
		`","ok":true,"tflops":648.2,"error":""},{"index":1,"uuid":"` + fakeUUID1 + `","ok":true,"tflops":300.5,"error":""}]}` + "\n"})
	_, err = s.Verify(10, id)
	require.NoError(t, err)
	done = runTasks(t, s)
	fake.ClearOverrides()
	require.True(t, outcomeOf(t, done[0]).Pass)
	_, checks = latestChecks(t, db, id)
	assert.Contains(t, checks["P13"].Detail, "低于基准")
	assert.Equal(t, SlotFaulty, slotState(t, db, id, 1))
	stock, _ = productStock(t, db)
	assert.Equal(t, 1, stock)
	_, err = s.Verify(10, id)
	require.NoError(t, err)
	runTasks(t, s)
	stock, _ = productStock(t, db)
	assert.Equal(t, 2, stock)

	// 单卡停用/启用、排空/恢复。
	require.NoError(t, s.SetSlotState(10, id, 0, SlotWithheld, ""))
	stock, _ = productStock(t, db)
	assert.Equal(t, 1, stock)
	assert.Equal(t, errcode.ParamInvalid, codeOf(s.SetSlotState(10, id, 0, SlotLeased, "")))
	assert.Equal(t, errcode.NotFound, codeOf(s.SetSlotState(11, id, 0, SlotFree, "")))
	assert.Equal(t, errcode.NotFound, codeOf(s.SetSlotState(10, id, 9, SlotFree, "")))
	require.NoError(t, s.SetSlotState(10, id, 0, SlotFree, ""))
	require.NoError(t, s.SetSchedulable(10, id, false, ""))
	stock, status = productStock(t, db)
	assert.Equal(t, 0, stock)
	assert.Equal(t, "sold_out", status)
	require.NoError(t, s.SetSchedulable(10, id, true, ""))
	stock, _ = productStock(t, db)
	assert.Equal(t, 2, stock)

	// 离线判定: 连不上且距上次成功超过 90s → offline, 库存 0, 通知。
	db.MustExec(`UPDATE supplier_nodes SET last_probe_ok_at=NOW() - INTERVAL 5 MINUTE WHERE id=?`, id)
	fake.Override(cmdLight, fakeReply{Stderr: "NVIDIA-SMI has failed because it couldn't communicate with the NVIDIA driver.", Code: 9})
	n, _ = s.repo.GetNode(id)
	s.lightProbe(ctx, n)
	fake.ClearOverrides()
	n, _ = s.repo.GetNode(id)
	assert.Equal(t, "offline", n.Status)
	assert.Equal(t, 1, n.ProbeFailCount)
	stock, _ = productStock(t, db)
	assert.Equal(t, 0, stock)
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM notifications WHERE user_id=10 AND title='节点离线'`))
	s.lightProbe(ctx, n) // 恢复连通 → 自动上线
	n, _ = s.repo.GetNode(id)
	assert.Equal(t, "online", n.Status)
	stock, _ = productStock(t, db)
	assert.Equal(t, 2, stock)

	// 运营停用/恢复。
	require.NoError(t, s.Suspend(1, id, "测试停用", ""))
	assert.Equal(t, errcode.Conflict, codeOf(s.Suspend(1, id, "", "")))
	n, _ = s.repo.GetNode(id)
	assert.Equal(t, VerifySuspended, n.VerifyStatus)
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE node_id=?`, id), "停用释放空闲卡 UUID 占用")
	stock, _ = productStock(t, db)
	assert.Equal(t, 0, stock)
	_, err = s.Verify(10, id)
	assert.Equal(t, errcode.Conflict, codeOf(err))
	resumeTask, err := s.Resume(1, id, "")
	require.NoError(t, err)
	assert.NotZero(t, resumeTask)
	done = runTasks(t, s)
	require.Len(t, done, 1)
	require.True(t, outcomeOf(t, done[0]).Pass)
	stock, _ = productStock(t, db)
	assert.Equal(t, 2, stock)

	// 死信任务: 列表与运营重试。
	res2, err := db.Exec(`INSERT INTO node_tasks (node_id, kind, state, priority, attempts, payload, last_error)
		VALUES (?, 'probe', 'dead', 5, 3, '{"mode":"full","trigger":"supplier"}', 'boom')`, id)
	require.NoError(t, err)
	deadID, _ := res2.LastInsertId()
	list, total, err := s.Tasks("", 0, 1, 20)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	assert.Equal(t, deadID, list[0].ID)
	require.NoError(t, s.RetryTask(1, deadID, ""))
	assert.Equal(t, errcode.Conflict, codeOf(s.RetryTask(1, deadID, "")))
	assert.Equal(t, errcode.NotFound, codeOf(s.RetryTask(1, 999999, "")))
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM audit_logs WHERE action='node_task.retry' AND target_id=?`, deadID))

	// 删除: 他人 40400; 本人删除后卡槽、待执行任务一并处理, 库存 0。
	assert.Equal(t, errcode.NotFound, codeOf(s.DeleteManagedNode(11, id)))
	require.NoError(t, s.DeleteManagedNode(10, id))
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM supplier_nodes WHERE id=?`, id))
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE node_id=?`, id))
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM node_tasks WHERE node_id=? AND state IN ('pending','running')`, id))
	stock, _ = productStock(t, db)
	assert.Equal(t, 0, stock)

	// 一次性密码不得出现在任何表或日志中。
	var dump []string
	for _, q := range []string{
		`SELECT CONCAT_WS('|', action, IFNULL(before_value,''), IFNULL(after_value,'')) FROM audit_logs`,
		`SELECT CONCAT_WS('|', IFNULL(payload,''), IFNULL(result,''), IFNULL(last_error,'')) FROM node_tasks`,
		`SELECT CONCAT_WS('|', checks, IFNULL(inventory,'')) FROM node_probe_reports`,
		`SELECT CONCAT_WS('|', title, content) FROM notifications`,
	} {
		var rows []string
		require.NoError(t, db.Select(&rows, q))
		dump = append(dump, rows...)
	}
	all := strings.Join(dump, "\n") + logs.String()
	assert.NotContains(t, all, goodPW)
	assert.NotContains(t, all, badPW)
	assert.NotContains(t, all, "PRIVATE KEY")
}

func slotState(t *testing.T, db *sqlx.DB, nodeID int64, idx int) string {
	t.Helper()
	var st string
	require.NoError(t, db.Get(&st, `SELECT state FROM gpu_slots WHERE node_id=? AND gpu_index=?`, nodeID, idx))
	return st
}

// T-079: push 节点的心跳超时逻辑不得作用于 ssh 节点(在线只由 SSH 探活判定)。
func TestMarkStaleOfflineSkipsSSHNodes(t *testing.T) {
	db := setupFleetDB(t)
	seedManaged(t, db)
	db.MustExec(`INSERT INTO supplier_nodes (id, supplier_id, product_id, node_name, node_key_hash, status, total_cards, last_heartbeat_at, mgmt_mode)
		VALUES (1, 10, 1, 'push-1', REPEAT('a',64), 'online', 8, NOW() - INTERVAL 1 HOUR, 'push'),
		       (2, 10, 1, 'ssh-1', NULL, 'online', 8, NOW() - INTERVAL 1 HOUR, 'ssh')`)
	ids, err := scheduler.NewRepository(db).MarkStaleOffline(90 * time.Second)
	require.NoError(t, err)
	assert.Equal(t, []int64{1}, ids)
	var st []string
	require.NoError(t, db.Select(&st, `SELECT status FROM supplier_nodes ORDER BY id`))
	assert.Equal(t, []string{"offline", "online"}, st)
}

// 探活中途连接信息被修改: 结论作废, 不写 verified。
func TestProbeDiscardedWhenSSHChanged(t *testing.T) {
	db := setupFleetDB(t)
	seedManaged(t, db)
	s := newTestFleet(t, db)
	s.gateway = &stubGateway{}
	fake := newFakeNode(t, "pw-123456")
	v, err := s.RegisterManagedNode(10, scheduler.ManagedNodeReq{ProductID: 1, NodeName: "gpu-02", TotalCards: 2,
		LeasePortStart: 47100, LeasePortEnd: 47109})
	require.NoError(t, err)
	id := v.(*Node).ID
	_, err = s.UpdateSSH(context.Background(), 10, id, SSHInput{Host: "127.0.0.1", Port: fake.Port(), User: "root", BootstrapPassword: "pw-123456"}, "")
	require.NoError(t, err)
	_, err = s.ConfirmHostKey(10, id, fake.Fingerprint(), "")
	require.NoError(t, err)
	// 压测进行中供给方改了 SSH 用户名(同一 host/port, 指纹保留): commit 时守卫不匹配, 结论作废。
	fake.OnCommand("docker run --rm --name omnis-probe-", func() {
		db.MustExec(`UPDATE supplier_nodes SET ssh_user='omnis' WHERE id=?`, id)
	})
	task, err := s.repo.claimNext([]string{KindProbe}, probeLock)
	require.NoError(t, err)
	require.NotNil(t, task)
	s.execute(context.Background(), task)
	got, _ := s.repo.getTask(db, task.ID)
	o := outcomeOf(t, *got)
	assert.True(t, o.Discarded)
	n, _ := s.repo.GetNode(id)
	assert.NotEqual(t, VerifyVerified, n.VerifyStatus)
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE node_id=?`, id))
}
