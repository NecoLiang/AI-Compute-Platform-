package fleet

import (
	"context"
	"sync"
	"testing"
	"time"

	"tokenfactory/internal/compute"
	"tokenfactory/internal/legal"
	"tokenfactory/internal/modeldeploy"
	"tokenfactory/pkg/config"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 包夜(S4 T-094/T-095)集成测试: 真实 MySQL + fakeNode。真实 GPU/宿主机行为(nvidia-smi 计算进程、docker start)未验收,
// 这里由 fakeNode 应答。

const nightlyPrice = 9000 // 分/卡/晚

// nightlyize 把托管商品改为夜间窗口 + 包夜价格。
func nightlyize(t *testing.T, db *sqlx.DB, s *Service, w compute.NightWindow) {
	t.Helper()
	db.MustExec(`UPDATE products SET availability='night_window', night_start_min=?, night_end_min=?, pricing_mode='nightly',
		unit_price=? WHERE id=?`, w.StartMin, w.EndMin, nightlyPrice, leaseProductID)
	db.MustExec(`INSERT INTO product_prices (product_id, pricing_mode, unit_price, min_duration, enabled) VALUES (?, 'nightly', ?, 1, 1)`,
		leaseProductID, nightlyPrice)
	require.NoError(t, s.RefreshManagedStock(leaseProductID))
}

func placeNight(comp *compute.Service, buyer int64, cards int, start time.Time, nights int) (*compute.Order, error) {
	return comp.PlaceOrder(buyer, compute.PlaceOrderReq{ProductID: leaseProductID, Quantity: cards, Duration: nights,
		PricingMode: compute.PricingNightly, NightStartDate: start.Format(dateLayout),
		ComplianceAgreed: true, ComplianceVersion: legal.Version})
}

// currentWindow 当前正处于其中的 4 小时窗口: 开始 = 当前时刻按 30 分钟向下取整再减 30 分钟(开始 30-60 分钟前)。
func currentWindow() compute.NightWindow {
	now := time.Now().In(shanghai())
	start := now.Truncate(30 * time.Minute).Add(-30 * time.Minute)
	m := start.Hour()*60 + start.Minute()
	return compute.NightWindow{StartMin: m, EndMin: (m + 240) % 1440}
}

func TestNightlyConcurrentDoubleBookingAndCalendar(t *testing.T) {
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	seedLeaseMarket(t, db, s, "container", 1, 4, 12)
	w := compute.NightWindow{StartMin: 22 * 60, EndMin: 6 * 60}
	nightlyize(t, db, s, w)
	comp := wireLeases(t, s)
	d := compute.FirstBookableNight(time.Now(), w).AddDate(0, 0, 1)

	// 10 个买家同时订同一节点 4 卡 × 2 晚: 只能成功一单。
	var wg sync.WaitGroup
	gate := make(chan struct{})
	var mu sync.Mutex
	var won []*compute.Order
	var fails []error
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(buyer int64) {
			defer wg.Done()
			<-gate
			o, err := placeNight(comp, buyer, 4, d, 2)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fails = append(fails, err)
				return
			}
			won = append(won, o)
		}(int64(21 + i))
	}
	close(gate)
	wg.Wait()
	require.Len(t, won, 1, "fails=%v", fails)
	for _, err := range fails {
		if codeOf(err) != 40900 {
			assert.Contains(t, err.Error(), "product not available")
		}
	}
	o := won[0]
	assert.EqualValues(t, nightlyPrice*4*2, o.TotalAmount)
	assert.Equal(t, 8, countRows(t, db, `SELECT COUNT(*) FROM night_bookings`))
	assert.Equal(t, 2, countRows(t, db, `SELECT COUNT(*) FROM lease_nights WHERE state='booked'`))
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE state<>'free'`), "下单只写预订, 不动卡槽")

	// 区间重叠被拒, 不重叠的同一组卡可订。
	_, err := placeNight(comp, 31, 1, d.AddDate(0, 0, 1), 2)
	assert.Equal(t, 40900, codeOf(err))
	o2, err := placeNight(comp, 31, 2, d.AddDate(0, 0, 2), 1)
	require.NoError(t, err)

	// 夜间日历(跨零点窗口, night_date = 窗口开始日期)。与 availability 共用限流。
	comp.SetRateLimiter(availabilityRedis(t, 7, 32))
	cal, err := comp.NightCalendar(context.Background(), 32, leaseProductID, 0, d.Format(dateLayout), 4)
	require.NoError(t, err)
	got := map[string]int{}
	for _, n := range cal {
		got[n.NightDate] = n.MaxCards
	}
	assert.Equal(t, map[string]int{d.Format(dateLayout): 0, d.AddDate(0, 0, 1).Format(dateLayout): 0,
		d.AddDate(0, 0, 2).Format(dateLayout): 2, d.AddDate(0, 0, 3).Format(dateLayout): 4}, got)
	cal, err = comp.NightCalendar(context.Background(), 32, leaseProductID, 3, d.Format(dateLayout), 4)
	require.NoError(t, err)
	assert.Equal(t, 0, cal[2].MaxCards, "只剩 2 卡的晚对 3 卡需求记 0")

	// 已截止的晚记 0: 从昨天查起。
	first := compute.FirstBookableNight(time.Now(), w)
	cal, err = comp.NightCalendar(context.Background(), 32, leaseProductID, 0, first.AddDate(0, 0, -1).Format(dateLayout), 2)
	require.NoError(t, err)
	assert.Equal(t, 0, cal[0].MaxCards)
	assert.Equal(t, 4, cal[1].MaxCards)

	// 关单: 预订全部释放, 可再订。
	require.NoError(t, comp.CancelOrder(o.OrderNo))
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM night_bookings WHERE lease_id=(SELECT id FROM leases WHERE order_id=?)`, o.ID))
	_, err = placeNight(comp, 30, 4, d, 2)
	require.NoError(t, err)
	assert.Equal(t, 2, countRows(t, db, `SELECT COUNT(*) FROM night_bookings WHERE lease_id=(SELECT id FROM leases WHERE order_id=?)`, o2.ID))
}

// newNightEnv 当前窗口内的 2 卡 × 2 晚包夜订单, 已支付(当晚订单: 支付即认领卡槽并入队开通)。
func newNightEnv(t *testing.T) (*provEnv, compute.NightWindow) {
	t.Helper()
	e := newProvNode(t, "container", true)
	e.s.cfg.Grace.Short, e.s.cfg.Grace.Long, e.s.cfg.Grace.Nightly = 24*time.Hour, 72*time.Hour, 24*time.Hour
	e.s.cfg.Outage = config.FleetOutage{MinMinutes: 5, RefundRatio: 0.10, RefundSingleHours: 2, MaxSupplierMinutes30d: 360}
	w := currentWindow()
	nightlyize(t, e.db, e.s, w)
	d := compute.NightDateOf(time.Now(), w)
	var err error
	e.order, err = placeNight(e.comp, 21, 2, d, 2)
	require.NoError(t, err)
	e.payAndLoad(t)
	no := e.lease.LeaseNo
	e.fake.Override(cmdOK(startContainerCommand(no)), fakeReply{Stdout: "@@started\n"})
	e.fake.Override(cmdOK(stopContainerCommand(no)), fakeReply{Stdout: "@@stopped\n"})
	e.fake.Override(cmdGPUApps, fakeReply{Stdout: ""})
	return e, w
}

// shiftNights 把租约的全部夜晚、订单快照与租期整体前移一天(模拟时间过去一天)。
func shiftNights(t *testing.T, e *provEnv) {
	t.Helper()
	e.db.MustExec(`UPDATE night_bookings SET night_date=night_date - INTERVAL 1 DAY WHERE lease_id=? ORDER BY night_date`, e.lease.ID)
	e.db.MustExec(`UPDATE lease_nights SET night_date=night_date - INTERVAL 1 DAY WHERE lease_id=? ORDER BY night_date`, e.lease.ID)
	e.db.MustExec(`UPDATE orders SET night_start_date=night_start_date - INTERVAL 1 DAY, lease_start_at=lease_start_at - INTERVAL 1 DAY,
		lease_end_at=lease_end_at - INTERVAL 1 DAY WHERE id=?`, e.order.ID)
	e.db.MustExec(`UPDATE leases SET access_until=access_until - INTERVAL 1 DAY WHERE id=?`, e.lease.ID)
	e.db.MustExec(`UPDATE order_deliveries SET access_expires_at=access_expires_at - INTERVAL 1 DAY WHERE order_id=?`, e.order.ID)
}

func nightStates(t *testing.T, db *sqlx.DB, leaseID int64) []string {
	t.Helper()
	var st []string
	require.NoError(t, db.Select(&st, `SELECT state FROM lease_nights WHERE lease_id=? ORDER BY night_date`, leaseID))
	return st
}

// 首晚开通 → 窗口结束 park(停容器、卡归还) → 下一晚开窗 unpark(同一容器 docker start、卡重新认领)。
func TestNightlyParkUnparkCycle(t *testing.T) {
	e, w := newNightEnv(t)
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='reserved'`, e.lease.ID),
		"当晚订单支付即认领本晚卡槽")
	e.outcome(t, runOne(t, e.s))
	l := e.leaseNow(t)
	require.Equal(t, LeaseActive, l.State)
	assert.Equal(t, []string{"active", "booked"}, nightStates(t, e.db, l.ID))
	d := compute.NightDateOf(time.Now(), w)
	assert.True(t, l.AccessUntil.Equal(compute.NightEndAt(d.AddDate(0, 0, 1), w)), "access_until = 末晚窗口结束")
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM lease_outages WHERE lease_id=?`, l.ID), "开通及时不记 provision_delay")

	// 一天过去: 首晚窗口已结束, 次晚窗口正在进行。
	shiftNights(t, e)
	e.s.NightlyRound(context.Background())
	l = e.leaseNow(t)
	assert.Equal(t, LeaseParked, l.State)
	assert.Equal(t, []string{"ended", "booked"}, nightStates(t, e.db, l.ID))
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='draining'`, l.ID))
	// 停容器任务未完成前不开窗(记一次尝试)。
	var attempts int
	require.NoError(t, e.db.Get(&attempts, `SELECT attempts FROM lease_nights WHERE lease_id=? AND state='booked'`, l.ID))
	assert.Equal(t, 1, attempts)

	task := runOne(t, e.s)
	require.NotNil(t, task)
	assert.Equal(t, KindStop, task.Kind)
	assert.Equal(t, TaskSucceeded, task.State, "last_error=%v", task.LastError)
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE state='free' AND lease_id IS NULL`), "白天卡归还供给方")

	e.s.NightlyRound(context.Background())
	l = e.leaseNow(t)
	require.Equal(t, LeaseActive, l.State)
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='leased'`, l.ID))
	assert.Contains(t, e.fake.Commands(), cmdGPUApps, "开窗前检查宿主机 GPU 进程")
	assert.Contains(t, e.fake.Commands(), cmdOK(startContainerCommand(l.LeaseNo)))
	assert.Equal(t, []string{"ended", "active"}, nightStates(t, e.db, l.ID)[:2])

	// 开窗晚于窗口开始(本测试中窗口已开始 30-60 分钟) → 记停机; 默认顺延 = 补加一晚(同卡同窗口)。
	var oc struct {
		Cause  string  `db:"cause"`
		Liable string  `db:"liable"`
		Remedy *string `db:"remedy"`
	}
	require.NoError(t, e.db.Get(&oc, `SELECT cause, liable, remedy FROM lease_outages WHERE lease_id=?`, l.ID))
	assert.Equal(t, OutageNightUnparkFailed, oc.Cause)
	assert.Equal(t, "supplier", oc.Liable)
	require.NotNil(t, oc.Remedy)
	assert.Equal(t, "extend", *oc.Remedy)
	assert.Equal(t, []string{"ended", "active", "booked"}, nightStates(t, e.db, l.ID))
	assert.Equal(t, 6, countRows(t, e.db, `SELECT COUNT(*) FROM night_bookings WHERE lease_id=?`, l.ID))
	assert.True(t, l.AccessUntil.Equal(compute.NightEndAt(d.AddDate(0, 0, 1), w)), "补加一晚: 租期后移 24h")
}

// 开窗时宿主机进程占用本晚卡, 超过 应开通时刻+10min → 本晚失败: 违约记录 + 整晚停机(供给方) + 通知。
func TestNightlyUnparkOccupiedRecordsOutage(t *testing.T) {
	e, w := newNightEnv(t)
	e.outcome(t, runOne(t, e.s))
	shiftNights(t, e)
	e.s.NightlyRound(context.Background())
	require.Equal(t, KindStop, runOne(t, e.s).Kind)

	e.fake.Override(cmdGPUApps, fakeReply{Stdout: fakeUUID0 + ", 4242\n"})
	e.s.NightlyRound(context.Background())
	l := e.leaseNow(t)
	assert.Equal(t, LeaseParked, l.State)
	assert.NotContains(t, e.fake.Commands(), cmdOK(startContainerCommand(l.LeaseNo)), "占用时不启动容器")
	var n struct {
		State     string  `db:"state"`
		Occupied  bool    `db:"occupied"`
		FailCause *string `db:"fail_cause"`
		OutageID  *int64  `db:"outage_id"`
	}
	require.NoError(t, e.db.Get(&n, `SELECT state, occupied, fail_cause, outage_id FROM lease_nights WHERE lease_id=?
		ORDER BY night_date LIMIT 1 OFFSET 1`, l.ID))
	assert.Equal(t, "failed", n.State)
	assert.True(t, n.Occupied)
	require.NotNil(t, n.FailCause)
	assert.Equal(t, OutageNightUnparkFailed, *n.FailCause)
	require.NotNil(t, n.OutageID)

	var oc struct {
		Cause   string    `db:"cause"`
		Liable  string    `db:"liable"`
		Started time.Time `db:"started_at"`
		Ended   time.Time `db:"ended_at"`
	}
	require.NoError(t, e.db.Get(&oc, `SELECT cause, liable, started_at, ended_at FROM lease_outages WHERE id=?`, *n.OutageID))
	d := compute.NightDateOf(time.Now(), w)
	assert.Equal(t, OutageNightUnparkFailed, oc.Cause)
	assert.Equal(t, "supplier", oc.Liable)
	assert.True(t, oc.Started.Equal(compute.NightStartAt(d, w)), "整晚: 自窗口开始")
	assert.True(t, oc.Ended.Equal(compute.NightEndAt(d, w)), "整晚: 至窗口结束")
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM audit_logs WHERE action='lease.night_unpark_failed' AND target_id=?`, l.ID))
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM notifications WHERE user_id=20 AND title LIKE '包夜节点违约%'`))
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM notifications WHERE user_id=21 AND title='包夜本晚未能开通'`))
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state IN ('reserved','leased')`, l.ID),
		"认领的卡退回(draining → stop 后 free)")
	assert.Equal(t, []string{"ended", "failed", "booked"}, nightStates(t, e.db, l.ID), "顺延补加一晚")

	// 后续轮次不重复处理。
	e.s.NightlyRound(context.Background())
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM lease_outages WHERE lease_id=?`, l.ID))
}

// 非占用的开窗失败(连不上节点)本晚内只记尝试; 窗口结束仍未开通 → 整晚按最后原因记停机。
func TestNightlyUnparkUnreachableThenSweep(t *testing.T) {
	e, _ := newNightEnv(t)
	e.outcome(t, runOne(t, e.s))
	shiftNights(t, e)
	e.s.NightlyRound(context.Background())
	require.Equal(t, KindStop, runOne(t, e.s).Kind)

	e.db.MustExec(`UPDATE supplier_nodes SET ssh_port=1 WHERE id=?`, e.node)
	e.s.NightlyRound(context.Background())
	var n struct {
		State     string  `db:"state"`
		Attempts  int     `db:"attempts"`
		FailCause *string `db:"fail_cause"`
	}
	require.NoError(t, e.db.Get(&n, `SELECT state, attempts, fail_cause FROM lease_nights WHERE lease_id=? ORDER BY night_date LIMIT 1 OFFSET 1`, e.lease.ID))
	assert.Equal(t, "booked", n.State)
	assert.GreaterOrEqual(t, n.Attempts, 1)
	require.NotNil(t, n.FailCause)
	assert.Equal(t, OutageNodeOffline, *n.FailCause)
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM lease_outages WHERE lease_id=?`, e.lease.ID))

	// 再过一天: 该晚窗口已结束 → 收尾记整晚停机。
	shiftNights(t, e)
	e.s.NightlyRound(context.Background())
	require.NoError(t, e.db.Get(&n, `SELECT state, attempts, fail_cause FROM lease_nights WHERE lease_id=? ORDER BY night_date LIMIT 1 OFFSET 1`, e.lease.ID))
	assert.Equal(t, "failed", n.State)
	var cause string
	require.NoError(t, e.db.Get(&cause, `SELECT cause FROM lease_outages WHERE lease_id=?`, e.lease.ID))
	assert.Equal(t, OutageNodeOffline, cause)
}

// 首晚卡槽被上一租约占用(draining)到 应开通时刻+10min 仍未释放 → 整单开通失败退款, 预订全部释放。
func TestNightlyFirstNightClaimFailsRefunds(t *testing.T) {
	e := newProvNode(t, "container", true)
	w := currentWindow()
	nightlyize(t, e.db, e.s, w)
	e.db.MustExec(`UPDATE gpu_slots SET state='draining' WHERE node_id=? AND gpu_index=0`, e.node)
	var err error
	e.order, err = placeNight(e.comp, 21, 2, compute.NightDateOf(time.Now(), w), 2)
	require.NoError(t, err)
	e.payAndLoad(t)
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=?`, e.lease.ID), "卡未就绪不入队开通")

	// 支付时刻 = 应开通时刻; 10 分钟内只重试。
	e.s.NightlyRound(context.Background())
	assert.Equal(t, LeasePending, e.leaseNow(t).State)
	e.db.MustExec(`UPDATE lease_nights SET due_at=due_at - INTERVAL 11 MINUTE WHERE lease_id=?`, e.lease.ID)
	// 让应开通时刻落在 10 分钟前: 窗口开始已在 30 分钟前, due = max(due_at, 窗口开始)。
	e.s.NightlyRound(context.Background())
	assert.Equal(t, LeaseFailed, e.leaseNow(t).State)
	assert.Equal(t, "cancelled", orderStatus(t, e))
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM night_bookings`))
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=?`, e.lease.ID))
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM notifications WHERE user_id=21 AND title='包夜订单开通失败'`))
}

func TestNightlyGatewayAccessEnd(t *testing.T) {
	w := compute.NightWindow{StartMin: 22 * 60, EndMin: 6 * 60}
	mode := compute.PricingNightly
	until := time.Date(2026, 11, 5, 6, 0, 0, 0, shanghai())
	r := &gwLeaseRoute{AccessUntil: &until, PricingMode: &mode, NightStart: &w.StartMin, NightEnd: &w.EndMin}
	at := func(s string) time.Time {
		v, err := time.ParseInLocation("2006-01-02 15:04", s, shanghai())
		require.NoError(t, err)
		return v
	}
	_, ok := r.accessEnd(at("2026-11-02 21:55"))
	assert.False(t, ok, "提前开通的几分钟不放行")
	end, ok := r.accessEnd(at("2026-11-02 23:00"))
	assert.True(t, ok)
	assert.Equal(t, at("2026-11-03 06:00"), end, "截断到当晚窗口结束")
	_, ok = r.accessEnd(at("2026-11-03 12:00"))
	assert.False(t, ok, "白天不可登录")
	_, ok = r.accessEnd(at("2026-11-05 06:00"))
	assert.False(t, ok, "租期结束")

	hourly := compute.PricingHourly
	r.PricingMode = &hourly
	end, ok = r.accessEnd(at("2026-11-03 12:00"))
	assert.True(t, ok)
	assert.Equal(t, until, end)
}

// 一键部署随包夜窗口停/起(docs/27 §27.6): park 时部署 → stopped(stop_reason=nightly, 保留 auto_resume),
// 下一晚 unpark 同事务恢复为 pending 并入队部署。
func TestNightlyDeploymentStopsAtParkResumesAtUnpark(t *testing.T) {
	e := newMDEnv(t)
	v := e.toStarting(t, false)

	// 把已开通的 2 卡租约改成包夜: 上一晚(已结束, active) + 今晚(booked)。
	w := currentWindow()
	today := compute.NightDateOf(time.Now(), w)
	prev := today.AddDate(0, 0, -1)
	e.db.MustExec(`UPDATE orders SET pricing_mode='nightly', night_start_date=?, night_start_min=?, night_end_min=?, duration=2,
		lease_end_at=? WHERE id=?`, prev.Format(dateLayout), w.StartMin, w.EndMin, compute.NightEndAt(today, w), e.order.ID)
	e.db.MustExec(`UPDATE leases SET access_until=? WHERE id=?`, compute.NightEndAt(today, w), e.lease.ID)
	e.db.MustExec(`INSERT INTO lease_nights (lease_id, night_date, state, unparked_at) VALUES (?, ?, 'active', NOW()), (?, ?, 'booked', NULL)`,
		e.lease.ID, prev.Format(dateLayout), e.lease.ID, today.Format(dateLayout))
	e.db.MustExec(`INSERT INTO night_bookings (slot_id, night_date, lease_id)
		SELECT id, ?, lease_id FROM gpu_slots WHERE lease_id=? UNION ALL SELECT id, ?, lease_id FROM gpu_slots WHERE lease_id=?`,
		prev.Format(dateLayout), e.lease.ID, today.Format(dateLayout), e.lease.ID)
	no := e.lease.LeaseNo
	e.fake.Override(cmdOK(startContainerCommand(no)), fakeReply{Stdout: "@@started\n"})
	e.fake.Override(cmdOK(stopContainerCommand(no)), fakeReply{Stdout: "@@stopped\n"})
	e.fake.Override(cmdGPUApps, fakeReply{Stdout: ""})
	e.fake.Override("if docker inspect --type container "+modeldeploy.EngineContainer(v.DeployNo)+" >/dev/null 2>&1; then docker rm -f",
		fakeReply{Stdout: "@@removed\n"})

	e.s.NightlyRound(context.Background())
	assert.Equal(t, LeaseParked, e.leaseNow(t).State)
	d := e.dep(t, v.DeployNo)
	assert.Equal(t, "stopping", d.State)
	require.NotNil(t, d.StopReason)
	assert.Equal(t, modeldeploy.StopNightly, *d.StopReason)

	kinds := e.drain(t)
	assert.Contains(t, kinds, KindStop)
	assert.Contains(t, kinds, modeldeploy.TaskStopModel)
	d = e.dep(t, v.DeployNo)
	assert.Equal(t, "stopped", d.State)
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM model_deployments WHERE deploy_no=? AND auto_resume=1`, v.DeployNo))

	e.s.NightlyRound(context.Background())
	require.Equal(t, LeaseActive, e.leaseNow(t).State)
	d = e.dep(t, v.DeployNo)
	assert.Equal(t, "pending", d.State, "包夜开窗自动恢复")
	assert.Nil(t, d.StopReason)
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE kind=? AND state='pending' AND lease_id=?`,
		modeldeploy.TaskDeployModel, e.lease.ID))
}
