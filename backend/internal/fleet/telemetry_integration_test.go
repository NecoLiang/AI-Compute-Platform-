package fleet

import (
	"context"
	"database/sql"
	"strconv"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"tokenfactory/internal/scheduler"
)

// fullProbedNode 注册 2 卡节点并跑完一次完整探活(确认指纹即入队); before 在探活执行前调用(设置覆盖等)。
func fullProbedNode(t *testing.T, before func(fake *fakeNode, db *sqlx.DB, id int64)) (*Service, *sqlx.DB, *fakeNode, int64, ProbeOutcome) {
	t.Helper()
	db := setupFleetDB(t)
	seedManaged(t, db)
	s := newTestFleet(t, db)
	s.gateway = &stubGateway{}
	fake := newFakeNode(t, "pw-123456")
	v, err := s.RegisterManagedNode(10, scheduler.ManagedNodeReq{ProductID: 1, NodeName: "gpu-tel", TotalCards: 2,
		LeasePortStart: 47100, LeasePortEnd: 47109})
	require.NoError(t, err)
	id := v.(*Node).ID
	_, err = s.UpdateSSH(context.Background(), 10, id, SSHInput{Host: "127.0.0.1", Port: fake.Port(), User: "root", BootstrapPassword: "pw-123456"}, "")
	require.NoError(t, err)
	if before != nil {
		before(fake, db, id)
	}
	_, err = s.ConfirmHostKey(10, id, fake.Fingerprint(), "")
	require.NoError(t, err)
	done := runTasks(t, s)
	require.Len(t, done, 1)
	return s, db, fake, id, outcomeOf(t, done[0])
}

type slotTel struct {
	Index       int        `db:"gpu_index"`
	State       string     `db:"state"`
	VRAMUsed    *int64     `db:"vram_used_mb"`
	VRAMTotal   *int64     `db:"vram_total_mb"`
	Throttle    *uint64    `db:"throttle_reasons"`
	TelemetryAt *time.Time `db:"telemetry_at"`
	TopoLinks   *string    `db:"topo_links"`
	TopoAt      *time.Time `db:"topo_at"`
}

func slotTelemetry(t *testing.T, db *sqlx.DB, nodeID int64) []slotTel {
	t.Helper()
	var out []slotTel
	require.NoError(t, db.Select(&out, `SELECT gpu_index, state, vram_used_mb, vram_total_mb, throttle_reasons, telemetry_at,
		topo_links, topo_at FROM gpu_slots WHERE node_id=? ORDER BY gpu_index`, nodeID))
	return out
}

type nodeTel struct {
	Gen        int        `db:"generation"`
	XidOK      *time.Time `db:"xid_scan_ok_at"`
	XidFrom    *time.Time `db:"xid_covered_from"`
	XidNote    *string    `db:"xid_scan_note"`
	CacheFree  *int64     `db:"cache_free_bytes"`
	CacheTotal *int64     `db:"cache_total_bytes"`
	CacheAt    *time.Time `db:"cache_observed_at"`
	CacheNote  *string    `db:"cache_note"`
}

func nodeTelemetry(t *testing.T, db *sqlx.DB, nodeID int64) nodeTel {
	t.Helper()
	var n nodeTel
	require.NoError(t, db.Get(&n, `SELECT generation, xid_scan_ok_at, xid_covered_from, xid_scan_note, cache_free_bytes,
		cache_total_bytes, cache_observed_at, cache_note FROM supplier_nodes WHERE id=?`, nodeID))
	return n
}

// 完整探活: 两两拓扑落卡槽、缓存目录容量落节点、P7(24h)Xid 扫描建立覆盖区间;
// 轻探活: 逐卡遥测落卡槽、自上次扫描起的 Xid 去重入库, 致命 Xid 走既有路径把空闲卡标 faulty。
func TestProbeTelemetryCollection(t *testing.T) {
	s, db, fake, id, o := fullProbedNode(t, nil)
	require.True(t, o.Pass, "failed=%v", o.Failed)

	slots := slotTelemetry(t, db, id)
	require.Len(t, slots, 2)
	for _, sl := range slots {
		require.NotNil(t, sl.TopoLinks)
		require.NotNil(t, sl.TopoAt)
		assert.Nil(t, sl.TelemetryAt, "完整探活不采逐卡遥测")
	}
	assert.JSONEq(t, `["X","NV18"]`, *slots[0].TopoLinks)
	assert.JSONEq(t, `["NV18","X"]`, *slots[1].TopoLinks)

	nt := nodeTelemetry(t, db, id)
	require.NotNil(t, nt.CacheAt)
	assert.Equal(t, int64(3298534883328), *nt.CacheFree)
	assert.Equal(t, int64(3840755982336), *nt.CacheTotal)
	assert.Nil(t, nt.CacheNote)
	require.NotNil(t, nt.XidOK)
	require.NotNil(t, nt.XidFrom)
	assert.True(t, nt.XidFrom.Before(time.Now().Add(-23*time.Hour)), "P7 扫描 24h 且日志自 48h 前开始: 覆盖 24h, got %v", nt.XidFrom)
	assert.Nil(t, nt.XidNote)
	firstFrom := *nt.XidFrom

	// 轻探活: 遥测落库; 覆盖区间与完整探活相接, 起点不变。
	n, err := s.repo.GetNode(id)
	require.NoError(t, err)
	s.lightProbe(context.Background(), n)
	slots = slotTelemetry(t, db, id)
	for _, sl := range slots {
		require.NotNil(t, sl.TelemetryAt)
		assert.Equal(t, int64(1), *sl.VRAMUsed)
		assert.Equal(t, int64(81559), *sl.VRAMTotal)
		assert.Equal(t, uint64(1), *sl.Throttle)
	}
	nt = nodeTelemetry(t, db, id)
	assert.True(t, nt.XidFrom.Equal(firstFrom), "相接的扫描延续原覆盖起点")
	assert.Contains(t, lastCmdWith(fake, `echo "@@id`), "journalctl -k --since '-2min'", "轻探活只扫自上次成功扫描起的窗口")

	// 致命 Xid 79(GPU1, 总线 2A): 同一行在相邻两次扫描中都会读到, 入库一次; 空闲卡转 faulty。
	fake.SetXidLines(
		fakeXidLine(time.Now().Add(-30*time.Second), "0000:2a:00", 79),
		fakeXidLine(time.Now().Add(-20*time.Second), "0000:18:00", 13),
	)
	s.lightProbe(context.Background(), n)
	s.lightProbe(context.Background(), n)
	assert.Equal(t, 2, countRows(t, db, `SELECT COUNT(*) FROM gpu_xid_events WHERE node_id=?`, id))
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM gpu_xid_events WHERE node_id=? AND pci_bus_id='0000:2a:00' AND xid=79 AND source='journal'`, id))
	assert.Equal(t, SlotFaulty, slotState(t, db, id, 1))
	assert.Equal(t, SlotFree, slotState(t, db, id, 0), "Xid 13 不在致命清单")
}

func lastCmdWith(f *fakeNode, prefix string) string {
	cmds := f.Commands()
	for i := len(cmds) - 1; i >= 0; i-- {
		if len(cmds[i]) >= len(prefix) && cmds[i][:len(prefix)] == prefix {
			return cmds[i]
		}
	}
	return ""
}

// 在租卡出现致命 Xid: 不改卡槽状态, 写 leases.resource_alert(既有路径, T-105 输入);
// 告警保持到最后一次致命 Xid 之后 1 小时(与 F6 同口径), 之后的干净扫描不提前清除。
func TestLightProbeFatalXidOnLeasedSlot(t *testing.T) {
	e := newProvEnv(t, true)
	e.outcome(t, runOne(t, e.s))
	n, err := e.s.repo.GetNode(e.node)
	require.NoError(t, err)
	// seedLeaseMarket 的 GPU1 总线为 0000:01:00.0
	e.fake.SetXidLines(fakeXidLine(time.Now().Add(-time.Minute), "00000000:01:00", 48))
	e.s.lightProbe(context.Background(), n)
	l, _ := e.s.repo.getLease(e.db, e.lease.ID)
	require.NotNil(t, l.ResourceAlert)
	assert.Contains(t, *l.ResourceAlert, "GPU1 致命 Xid [48]")
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='leased'`, e.lease.ID))
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_xid_events WHERE node_id=? AND pci_bus_id='0000:01:00' AND xid=48`, e.node))
	nt := nodeTelemetry(t, e.db, e.node)
	require.NotNil(t, nt.XidFrom)
	assert.True(t, nt.XidFrom.Before(time.Now().Add(-time.Hour)), "首次轻探活扫描 61 分钟, 覆盖完整 1 小时")
	assert.Contains(t, lastCmdWith(e.fake, `echo "@@id`), "--since '-61min'")

	// 下一次扫描窗口里已没有该事件(干净扫描): 告警仍保持。
	e.fake.SetXidLines()
	e.s.lightProbe(context.Background(), n)
	l, _ = e.s.repo.getLease(e.db, e.lease.ID)
	require.NotNil(t, l.ResourceAlert)
	assert.Equal(t, "GPU1 致命 Xid [48]", *l.ResourceAlert)
	// 同卡再次出现致命 Xid(不同 xid): 合并去重。
	e.fake.SetXidLines(fakeXidLine(time.Now().Add(-10*time.Second), "0000:01:00", 79))
	e.s.lightProbe(context.Background(), n)
	l, _ = e.s.repo.getLease(e.db, e.lease.ID)
	assert.Equal(t, "GPU1 致命 Xid [48 79]", *l.ResourceAlert)
	// 最后一次致命 Xid 超过 1 小时 → 下一次轻探活清除。
	e.fake.SetXidLines()
	e.db.MustExec(`UPDATE gpu_xid_events SET occurred_at=NOW() - INTERVAL 61 MINUTE WHERE node_id=?`, e.node)
	e.s.lightProbe(context.Background(), n)
	l, _ = e.s.repo.getLease(e.db, e.lease.ID)
	assert.True(t, l.ResourceAlert == nil || *l.ResourceAlert == "", "%v", l.ResourceAlert)
}

// 本次扫描的事件按节点时钟偏差换算后再按 1 小时截取; 不可用的扫描不贡献事件。
func TestXidScanFatalSince(t *testing.T) {
	at := time.Now().Truncate(time.Second)
	nodeNow := at.Add(-10 * time.Minute) // 节点时钟慢 10 分钟
	scan := XidScan{Known: true, NodeNow: nodeNow, List: []XidEvent{
		{Bus: "0000:18:00", Xid: 79, At: nodeNow.Add(-30 * time.Minute)},
		{Bus: "0000:18:00", Xid: 13, At: nodeNow.Add(-1 * time.Minute)},
		{Bus: "0000:2a:00", Xid: 48, At: nodeNow.Add(-70 * time.Minute)},
	}}
	got := scan.fatalSince(at, at.Add(-time.Hour))
	assert.Equal(t, map[string][]int{"0000:18:00": {79}}, got)
	scan.Known = false
	assert.Empty(t, scan.fatalSince(at, at.Add(-time.Hour)))
	m := map[string][]int{"a": {79, 48}}
	mergeFatalXids(m, map[string][]int{"a": {48}, "b": {74}})
	assert.Equal(t, map[string][]int{"a": {48, 79}, "b": {74}}, m)
}

// 无权读内核日志 / 读取出错: 覆盖中断(xid_covered_from=NULL)并记原因, 上次成功时间不动; 恢复后从本次窗口重新累计。
func TestXidScanUnavailableBreaksCoverage(t *testing.T) {
	e := newProvEnv(t, true)
	e.outcome(t, runOne(t, e.s))
	n, err := e.s.repo.GetNode(e.node)
	require.NoError(t, err)
	e.s.lightProbe(context.Background(), n)
	before := nodeTelemetry(t, e.db, e.node)
	require.NotNil(t, before.XidOK)
	require.NotNil(t, before.XidFrom)

	now := strconv.FormatInt(time.Now().Unix(), 10)
	e.fake.Override(`echo "@@id`, fakeReply{Stdout: "@@id uid=1000 groups=omnis docker\n@@now " + now + "\n@@src journal\n@@first \n"})
	e.s.lightProbe(context.Background(), n)
	nt := nodeTelemetry(t, e.db, e.node)
	assert.Nil(t, nt.XidFrom)
	require.NotNil(t, nt.XidNote)
	assert.Contains(t, *nt.XidNote, "内核日志不可读")
	require.NotNil(t, nt.XidOK)
	assert.True(t, nt.XidOK.Equal(*before.XidOK))

	e.fake.Override(`echo "@@id`, fakeReply{Stdout: "@@id uid=0 groups=root\n@@now " + now + "\n@@src journal\n@@first 2026-10-01T00:00:00+0800\n" +
		"@@err Failed to open journal: Input/output error\n"})
	e.s.lightProbe(context.Background(), n)
	nt = nodeTelemetry(t, e.db, e.node)
	assert.Nil(t, nt.XidFrom)
	assert.Contains(t, *nt.XidNote, "读取内核日志出错")

	e.fake.ClearOverrides()
	restoreProvOverrides(t, e)
	time.Sleep(1100 * time.Millisecond) // 时间列为整秒
	e.s.lightProbe(context.Background(), n)
	nt = nodeTelemetry(t, e.db, e.node)
	require.NotNil(t, nt.XidFrom)
	assert.Nil(t, nt.XidNote)
	assert.True(t, nt.XidFrom.After(time.Now().Add(-time.Hour)), "中断后重新累计, 不足 1 小时")
	assert.True(t, nt.XidOK.After(*before.XidOK))
}

// restoreProvOverrides ClearOverrides 之后恢复 newProvEnv 的命令覆盖(轻探活本身不依赖, 保持环境一致)。
func restoreProvOverrides(t *testing.T, e *provEnv) {
	line, _ := fakeContainerHostKey(t)
	e.fake.Override("docker run --rm --network host --cap-drop ALL", fakeReply{Stdout: "@@applied iptables-nft\n"})
	e.fake.Override("if docker image inspect --format '{{.Id}}' "+testProbeImage, fakeReply{Stdout: "@@have\n"})
	e.fake.Override("if docker image inspect --format '{{.Id}}' "+testBaseImage, fakeReply{Stdout: "@@have\n"})
	e.fake.Override("set -e; (docker network inspect omnis-l-", fakeReply{Stdout: "@@created\n"})
	e.fake.Override("for i in $(seq 1 30); do if", fakeReply{Stdout: line + "\n"})
}

// 新驱动不认 clocks_throttle_reasons → 改用 clocks_event_reasons; nvidia-smi 失败即本次轻探活失败(90s 内仍在线),
// 遥测列保持旧值(读取方按时间判过期)。
func TestTelemetryFallbackAndFailure(t *testing.T) {
	e := newProvEnv(t, true)
	e.outcome(t, runOne(t, e.s))
	n, err := e.s.repo.GetNode(e.node)
	require.NoError(t, err)

	e.fake.Override(cmdTelemetry, fakeReply{Stdout: "Field \"clocks_throttle_reasons.active\" is not a valid field to query.\n", Code: 2})
	e.fake.Override(cmdTelemetryEvent, fakeReply{Stdout: fakeUUID0 + ", 70000, 81559, 61, 512.30, 0x0000000000000000, 0, 3, 97, 40\n" +
		fakeUUID1 + ", [N/A], 81559, 60, 498.00, 0x0000000000000004, 0, 0, 95, 38\n"})
	e.s.lightProbe(context.Background(), n)
	slots := slotTelemetry(t, e.db, e.node)
	require.NotNil(t, slots[0].TelemetryAt)
	assert.Equal(t, int64(70000), *slots[0].VRAMUsed)
	assert.Nil(t, slots[1].VRAMUsed, "N/A 存 NULL, 不当 0")
	assert.Equal(t, uint64(4), *slots[1].Throttle)
	at := *slots[0].TelemetryAt

	e.fake.Override(cmdTelemetry, fakeReply{Stderr: "NVIDIA-SMI has failed\n", Code: 9})
	time.Sleep(1100 * time.Millisecond)
	e.s.lightProbe(context.Background(), n)
	slots = slotTelemetry(t, e.db, e.node)
	assert.True(t, slots[0].TelemetryAt.Equal(at), "采集失败不刷新遥测时间")
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM supplier_nodes WHERE id=? AND probe_fail_count>0`, e.node))
	var status string
	require.NoError(t, e.db.Get(&status, `SELECT status FROM supplier_nodes WHERE id=?`, e.node))
	assert.Equal(t, "online", status)
}

// 缓存目录不存在: 容量与采集时间清空并记原因(读取方即为未知), 不保留旧值。
func TestFullProbeCacheDirMissing(t *testing.T) {
	_, db, fake, id, o := fullProbedNode(t, func(fake *fakeNode, db *sqlx.DB, id int64) {
		db.MustExec(`UPDATE supplier_nodes SET cache_free_bytes=1, cache_total_bytes=2, cache_observed_at=NOW() WHERE id=?`, id)
		fake.Override("if [ -d '", fakeReply{Stdout: "@@missing\n"})
	})
	require.True(t, o.Pass, "缓存目录缺失不影响探活结论: failed=%v", o.Failed)
	nt := nodeTelemetry(t, db, id)
	assert.Nil(t, nt.CacheFree)
	assert.Nil(t, nt.CacheTotal)
	assert.Nil(t, nt.CacheAt)
	require.NotNil(t, nt.CacheNote)
	assert.Equal(t, "模型缓存目录不存在", *nt.CacheNote)
	assert.Contains(t, lastCmdWith(fake, "if [ -d '"), "'/data/omnis-models'")
}

// 节点代次变化(停用/改连接/确认指纹)之后, 旧连接采到的遥测一律不写回。
func TestTelemetryWritesGuardedByGeneration(t *testing.T) {
	s, db, _, id, o := fullProbedNode(t, nil)
	require.True(t, o.Pass)
	before := nodeTelemetry(t, db, id)
	stale := before.Gen - 1
	now := time.Now()
	gpus := []GPU{{Index: 0, UUID: fakeUUID0}, {Index: 1, UUID: fakeUUID1}}

	require.NoError(t, s.recordTopo(id, stale, gpus, [][]string{{"X", "PIX"}, {"PIX", "X"}}, now))
	require.NoError(t, s.recordCacheDF(id, stale, CacheDF{Missing: true}, "", now))
	used := int64(9)
	require.NoError(t, s.inTx(func(tx *sqlx.Tx) error {
		return s.repo.recordTelemetryTx(tx, id, stale, []GPUTelemetry{{UUID: fakeUUID0, VRAMUsedMiB: &used}}, now)
	}))
	require.NoError(t, s.recordXidScan(id, stale, XidScan{Known: false, Note: "x"}, now))

	slots := slotTelemetry(t, db, id)
	assert.JSONEq(t, `["X","NV18"]`, *slots[0].TopoLinks)
	assert.Nil(t, slots[0].VRAMUsed)
	after := nodeTelemetry(t, db, id)
	assert.Equal(t, before.CacheFree, after.CacheFree)
	require.NotNil(t, after.XidFrom)
	assert.True(t, after.XidFrom.Equal(*before.XidFrom))
}

// 覆盖区间: 相接延续起点、有缺口重新累计、乱序(结束时刻早于已记录)的扫描只补事件。
func TestXidCoverageIntervals(t *testing.T) {
	s, db, _, id, o := fullProbedNode(t, nil)
	require.True(t, o.Pass)
	gen := nodeTelemetry(t, db, id).Gen
	db.MustExec(`UPDATE supplier_nodes SET xid_scan_ok_at=NULL, xid_covered_from=NULL WHERE id=?`, id)
	db.MustExec(`DELETE FROM gpu_xid_events WHERE node_id=?`, id)

	base := time.Now().Add(-3 * time.Hour).Truncate(time.Second)
	scan := func(nodeNow time.Time, window time.Duration, lines ...XidEvent) XidScan {
		return XidScan{Known: true, Source: "journal", NodeNow: nodeNow, CoverFrom: nodeNow.Add(-window), List: lines}
	}
	get := func() (time.Time, time.Time) {
		nt := nodeTelemetry(t, db, id)
		require.NotNil(t, nt.XidOK)
		require.NotNil(t, nt.XidFrom)
		return *nt.XidFrom, *nt.XidOK
	}

	// 节点时钟慢 10 分钟: 覆盖起点按 平台时间-节点时间 换算。
	skew := 10 * time.Minute
	require.NoError(t, s.recordXidScan(id, gen, scan(base.Add(-skew), 61*time.Minute), base))
	from, ok := get()
	assert.True(t, from.Equal(base.Add(-61*time.Minute)), "got %v", from)
	assert.True(t, ok.Equal(base))

	// 相接(本次起点早于上次结束): 起点延续。
	t2 := base.Add(time.Minute)
	ev := XidEvent{Bus: "0000:18:00", Xid: 79, At: t2.Add(-skew - 30*time.Second), Line: "line-79"}
	require.NoError(t, s.recordXidScan(id, gen, scan(t2.Add(-skew), 2*time.Minute, ev), t2))
	from, ok = get()
	assert.True(t, from.Equal(base.Add(-61*time.Minute)))
	assert.True(t, ok.Equal(t2))
	var occurred time.Time
	require.NoError(t, db.Get(&occurred, `SELECT occurred_at FROM gpu_xid_events WHERE node_id=? AND xid=79`, id))
	assert.True(t, occurred.Equal(t2.Add(-30*time.Second)), "事件时间换算到平台时钟: got %v", occurred)

	// 乱序: 结束时刻早于已记录的扫描只补事件, 不回退区间。
	ev2 := XidEvent{Bus: "0000:2a:00", Xid: 48, At: base.Add(-skew), Line: "line-48"}
	require.NoError(t, s.recordXidScan(id, gen, scan(base.Add(-skew), 2*time.Minute, ev2), base))
	from, ok = get()
	assert.True(t, ok.Equal(t2))
	assert.True(t, from.Equal(base.Add(-61*time.Minute)))
	assert.Equal(t, 2, countRows(t, db, `SELECT COUNT(*) FROM gpu_xid_events WHERE node_id=?`, id))

	// 缺口(离线 30 分钟后只扫到 2 分钟): 重新累计。
	t3 := t2.Add(30 * time.Minute)
	require.NoError(t, s.recordXidScan(id, gen, scan(t3.Add(-skew), 2*time.Minute), t3))
	from, ok = get()
	assert.True(t, from.Equal(t3.Add(-2*time.Minute)), "got %v", from)
	assert.True(t, ok.Equal(t3))
}

func TestPurgeOldXidEvents(t *testing.T) {
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	db.MustExec(`INSERT INTO gpu_xid_events (node_id, pci_bus_id, xid, occurred_at, seen_at, source, message, event_key)
		VALUES (1,'0000:18:00',79,NOW() - INTERVAL 40 DAY, NOW() - INTERVAL 40 DAY,'journal','old',REPEAT('a',64)),
		       (1,'0000:18:00',79,NOW(), NOW(),'journal','new',REPEAT('b',64))`)
	n, err := s.repo.PurgeOldXidEvents(time.Now().Add(-xidEventRetention))
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	var msg sql.NullString
	require.NoError(t, db.Get(&msg, `SELECT message FROM gpu_xid_events`))
	assert.Equal(t, "new", msg.String)
}
