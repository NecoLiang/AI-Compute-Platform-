package fleet

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"tokenfactory/pkg/config"
	"tokenfactory/pkg/errcode"
)

// T-105 停机认定 / 补偿 / 供给方信用集成测试: 真实 MySQL + fakeNode(真实 SSH) + 网关桩。

// outageEnv active 租约 + 生产默认的停机/信用参数; 开通时间回拨 1 小时以便停机开始时间可回溯。
func outageEnv(t *testing.T) *provEnv {
	t.Helper()
	e := activeProvEnv(t)
	e.s.cfg.Outage = config.FleetOutage{MinMinutes: 5, RefundRatio: 0.10, RefundSingleHours: 2, MaxSupplierMinutes30d: 360}
	e.s.cfg.Credit.StopBelow = 60
	e.db.MustExec(`UPDATE leases SET provisioned_at=NOW()-INTERVAL 1 HOUR WHERE id=?`, e.lease.ID)
	e.lease = e.leaseNow(t)
	return e
}

type leaseTimes struct {
	AccessUntil time.Time `db:"access_until"`
	LeaseEnd    time.Time `db:"lease_end_at"`
	Delivery    time.Time `db:"access_expires_at"`
	Total       int64     `db:"total_amount"`
}

func (e *provEnv) times(t *testing.T) leaseTimes {
	t.Helper()
	var r leaseTimes
	require.NoError(t, e.db.Get(&r, `SELECT l.access_until, o.lease_end_at, d.access_expires_at, o.total_amount
		FROM leases l JOIN orders o ON o.id=l.order_id JOIN order_deliveries d ON d.order_id=o.id WHERE l.id=?`, e.lease.ID))
	return r
}

func (e *provEnv) outages(t *testing.T) []Outage {
	t.Helper()
	var rows []Outage
	require.NoError(t, e.db.Select(&rows, `SELECT `+outageColumns+` FROM lease_outages WHERE lease_id=? ORDER BY id`, e.lease.ID))
	return rows
}

// nodeDown 节点判离线, 最后一次探活成功在 ago 之前。
func (e *provEnv) nodeDown(ago time.Duration) {
	e.db.MustExec(`UPDATE supplier_nodes SET status='offline', last_probe_ok_at=? WHERE id=?`, time.Now().Add(-ago), e.node)
}

// backdate 已结束的停机段整体前移 d(模拟它们发生在更早时候), 以便下一段可回溯开始时间。
func (e *provEnv) backdate(d time.Duration) {
	e.db.MustExec(`UPDATE lease_outages SET started_at=started_at-INTERVAL ? SECOND, ended_at=ended_at-INTERVAL ? SECOND
		WHERE lease_id=? AND ended_at IS NOT NULL`, int(d/time.Second), int(d/time.Second), e.lease.ID)
}

// recoverContainer 节点恢复在线 + 对账确认容器在跑(Up 观测)。
func (e *provEnv) recoverContainer(t *testing.T) {
	t.Helper()
	e.db.MustExec(`UPDATE supplier_nodes SET status='online', last_probe_ok_at=NOW() WHERE id=?`, e.node)
	e.fake.Override(cmdLeaseContainers, fakeReply{Stdout: containerNameOf(e.lease.LeaseNo) + " running\n"})
	e.s.ReconcileRound(context.Background())
}

func TestOutageNodeOfflineOpenCloseExtendsOnce(t *testing.T) {
	e := outageEnv(t)
	ctx := context.Background()
	before := e.times(t)

	e.nodeDown(10 * time.Minute)
	e.s.OutageRound(ctx)
	e.s.OutageRound(ctx) // 重复扫描不另开
	rows := e.outages(t)
	require.Len(t, rows, 1)
	o := rows[0]
	assert.Equal(t, OutageNodeOffline, o.Cause)
	assert.Equal(t, "supplier", o.Liable)
	assert.Nil(t, o.EndedAt)
	assert.Equal(t, e.order.ID, o.OrderID)
	require.NotNil(t, o.NodeID)
	assert.Equal(t, e.node, *o.NodeID)
	assert.InDelta(t, 10*60, time.Since(o.StartedAt).Seconds(), 5, "从最后一次探活成功算起")

	// 节点恢复在线但容器租约尚未经对账确认: 不结束。
	e.db.MustExec(`UPDATE supplier_nodes SET status='online', last_probe_ok_at=NOW() WHERE id=?`, e.node)
	e.s.OutageRound(ctx)
	require.Nil(t, e.outages(t)[0].EndedAt)

	e.recoverContainer(t)
	o = e.outages(t)[0]
	require.NotNil(t, o.EndedAt)
	require.NotNil(t, o.Counted)
	assert.True(t, *o.Counted)
	require.NotNil(t, o.Seconds)
	secs := *o.Seconds
	assert.InDelta(t, 600, secs, 5)
	require.NotNil(t, o.Remedy)
	assert.Equal(t, "extend", *o.Remedy)
	assert.NotNil(t, o.RemedyAppliedAt)
	require.NotNil(t, o.LiabilityAmount)
	assert.Equal(t, before.Total*int64(secs)/7200, *o.LiabilityAmount, "实付 × 秒数 ÷ 租期秒数")

	d := time.Duration(secs) * time.Second
	after := e.times(t)
	assert.Equal(t, before.AccessUntil.Add(d), after.AccessUntil)
	assert.Equal(t, before.LeaseEnd.Add(d), after.LeaseEnd)
	assert.Equal(t, before.Delivery.Add(d), after.Delivery)

	// 再次结束/结算/扫描都不重复顺延。
	require.NoError(t, e.s.closeOutage(ctx, e.lease.ID, time.Now(), "again"))
	require.NoError(t, e.s.inTx(func(tx *sqlx.Tx) error {
		ord, err := e.s.products.LockManagedOrderTx(tx, e.order.ID)
		if err != nil {
			return err
		}
		l, err := e.s.repo.lockLease(tx, e.lease.ID)
		if err != nil {
			return err
		}
		return e.s.settleOutageTx(tx, ord, l, o.ID)
	}))
	e.recoverContainer(t)
	e.s.OutageRound(ctx)
	assert.Equal(t, after, e.times(t))
	assert.Len(t, e.outages(t), 1)

	// 供给方信用已按该段重算(10 分钟 → 扣 ceil(50×10/360)=2 分), 未停调度。
	c, err := e.s.getCredit(e.db, 20)
	require.NoError(t, err)
	require.NotNil(t, c)
	assert.Equal(t, secs/60, c.OutageMinutes30d)
	assert.Equal(t, 1, c.OutageCount30d)
	assert.Equal(t, creditScore(secs/60, 0, 0, 360), c.Score)
	assert.False(t, c.DispatchStopped)
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM supplier_nodes WHERE dispatch_hold=1`))
}

func TestOutageShortNotCountedAndSuspendCloses(t *testing.T) {
	e := outageEnv(t)
	ctx := context.Background()
	before := e.times(t)

	// 2 分钟 < min_minutes: 不计, 不顺延, 但视为已处理(remedy_applied_at 非空)。
	e.nodeDown(2 * time.Minute)
	e.s.OutageRound(ctx)
	e.recoverContainer(t)
	o := e.outages(t)[0]
	require.NotNil(t, o.Counted)
	assert.False(t, *o.Counted)
	assert.Nil(t, o.Remedy)
	assert.NotNil(t, o.RemedyAppliedAt)
	assert.Equal(t, before, e.times(t))

	// 停机期间租约被冻结(买家原因): 停机在 suspended_at 结束, 冻结期不计。
	e.backdate(30 * time.Minute)
	e.nodeDown(20 * time.Minute)
	e.s.OutageRound(ctx)
	rows := e.outages(t)
	require.Len(t, rows, 2)
	assert.False(t, rows[1].StartedAt.Before(*rows[0].EndedAt), "开始时间不早于上一段结束")
	suspendedAt := time.Now().Add(-5 * time.Minute).Truncate(time.Second)
	e.db.MustExec(`UPDATE leases SET state='suspended', suspended_at=? WHERE id=?`, suspendedAt, e.lease.ID)
	e.s.OutageRound(ctx)
	o = e.outages(t)[1]
	require.NotNil(t, o.EndedAt)
	assert.True(t, o.EndedAt.Equal(suspendedAt), "ended_at=%s suspended_at=%s", o.EndedAt, suspendedAt)
	require.NotNil(t, o.Remedy)
	assert.Equal(t, "extend", *o.Remedy, "冻结中的租约仍可顺延")
	assert.Equal(t, before.AccessUntil.Add(time.Duration(*o.Seconds)*time.Second), e.times(t).AccessUntil)
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM lease_outages WHERE open_claim IS NOT NULL`))
}

func TestOutageConcurrentStartAndClose(t *testing.T) {
	e := outageEnv(t)
	ctx := context.Background()
	// 信用分不触发停调度, 只验证单节点停机超限(max_supplier_minutes_30d)。
	e.s.cfg.Credit.StopBelow = 0
	e.s.cfg.Outage.MaxSupplierMinutes30d = 5
	before := e.times(t)
	start := time.Now().Add(-30 * time.Minute)

	var wg sync.WaitGroup
	var mu sync.Mutex
	opened := 0
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := e.s.startOutage(e.lease.ID, OutageGPUFault, "Xid 79", start)
			assert.NoError(t, err)
			if ok {
				mu.Lock()
				opened++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	assert.Equal(t, 1, opened)
	require.Len(t, e.outages(t), 1)

	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			assert.NoError(t, e.s.closeOutage(ctx, e.lease.ID, time.Now(), "concurrent"))
		}()
	}
	wg.Wait()
	o := e.outages(t)[0]
	require.NotNil(t, o.Seconds)
	assert.InDelta(t, 1800, *o.Seconds, 5)
	assert.Equal(t, before.AccessUntil.Add(time.Duration(*o.Seconds)*time.Second), e.times(t).AccessUntil, "只顺延一次")

	var hold struct {
		Hold   bool    `db:"dispatch_hold"`
		Reason *string `db:"dispatch_hold_reason"`
	}
	require.NoError(t, e.db.Get(&hold, `SELECT dispatch_hold, dispatch_hold_reason FROM supplier_nodes WHERE id=?`, e.node))
	assert.True(t, hold.Hold)
	require.NotNil(t, hold.Reason)
	assert.Contains(t, *hold.Reason, "超过上限")
}

func TestOutageGPUAlertAndBareRules(t *testing.T) {
	e := outageEnv(t)
	ctx := context.Background()
	// 探活写的掉卡告警 → gpu_fault; 告警清除后容器租约仍需对账 Up 才结束。
	e.db.MustExec(`UPDATE leases SET resource_alert='GPU 1 掉卡' WHERE id=?`, e.lease.ID)
	e.s.OutageRound(ctx)
	rows := e.outages(t)
	require.Len(t, rows, 1)
	assert.Equal(t, OutageGPUFault, rows[0].Cause)
	// 对账写的告警不算 gpu_fault, 也不被探活告警同步清掉。
	e.db.MustExec(`UPDATE leases SET resource_alert=? WHERE id=?`, reconcileAlertTag+"容器状态 dead", e.lease.ID)
	_, err := e.s.repo.clearProbeAlert(e.db, e.lease.ID)
	require.NoError(t, err)
	require.NotNil(t, e.leaseNow(t).ResourceAlert)
	e.recoverContainer(t)
	require.NotNil(t, e.outages(t)[0].EndedAt)
	assert.Nil(t, e.leaseNow(t).ResourceAlert, "对账 Up 清除对账告警")

	// 完整探活失败 → full_probe_failed。
	e.db.MustExec(`UPDATE supplier_nodes SET verify_status='failed' WHERE id=?`, e.node)
	e.s.OutageRound(ctx)
	rows = e.outages(t)
	require.Len(t, rows, 2)
	assert.Equal(t, OutageFullProbeFailed, rows[1].Cause)
	assert.Nil(t, rows[1].EndedAt)
}

func TestReconcileGatewayDownIsPlatformOutage(t *testing.T) {
	e := outageEnv(t)
	no := e.lease.LeaseNo
	e.fake.Override(cmdLeaseContainers, fakeReply{Stdout: containerNameOf(no) + " exited\n"})
	e.fake.Override(cmdOK(startContainerCommand(no)), fakeReply{Stdout: "@@started\n"})
	e.fake.Override(cmdOK(stopContainerCommand(no)), fakeReply{Stdout: "@@stopped\n"})
	e.gw.mu.Lock()
	e.gw.leaseErr = &gatewayDialError{addr: "gw:22", err: errors.New("connection refused")}
	e.gw.mu.Unlock()
	e.s.ReconcileRound(context.Background())
	rows := e.outages(t)
	require.Len(t, rows, 1)
	assert.Equal(t, OutageGatewayDown, rows[0].Cause)
	assert.Equal(t, "platform", rows[0].Liable)
}

func TestWrongFreezeRecordedOnce(t *testing.T) {
	e := outageEnv(t)
	ctx := context.Background()
	before := e.times(t)
	frozen := time.Now().Add(-40 * time.Minute).Truncate(time.Second)
	unfrozen := frozen.Add(25 * time.Minute)
	rec := outageBook{s: e.s}
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); rec.WrongFreeze(ctx, e.lease.ID, frozen, unfrozen) }()
	}
	wg.Wait()
	rows := e.outages(t)
	require.Len(t, rows, 1)
	o := rows[0]
	assert.Equal(t, OutageWrongFreeze, o.Cause)
	assert.Equal(t, "platform", o.Liable)
	assert.Equal(t, 1500, *o.Seconds)
	assert.Equal(t, before.AccessUntil.Add(25*time.Minute), e.times(t).AccessUntil)
	c, err := e.s.getCredit(e.db, 20)
	require.NoError(t, err)
	if c != nil {
		assert.Equal(t, 0, c.OutageMinutes30d, "平台责任不计入供给方信用")
	}
}

type recRefunder struct {
	mu  sync.Mutex
	ids []int64
}

func (r *recRefunder) RequestOutageRefund(_ context.Context, id int64) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ids = append(r.ids, id)
	return nil
}

func TestOutageRemedyRefundThreshold(t *testing.T) {
	e := outageEnv(t)
	ctx := context.Background()
	buyer, no := e.order.BuyerID, e.order.OrderNo

	_, err := e.s.SetOutageRemedy(buyer, no, "cash")
	assert.Equal(t, errcode.ParamInvalid, codeOf(err))
	_, err = e.s.OrderOutages(buyer+1, no)
	assert.Contains(t, []int{errcode.Forbidden, errcode.NotFound}, codeOf(err))
	_, err = e.s.SetOutageRemedy(buyer+1, no, "extend")
	assert.Contains(t, []int{errcode.Forbidden, errcode.NotFound}, codeOf(err))

	// 未达门槛(6 分钟 < 2h×10%=12 分钟): 退费 40900。
	e.nodeDown(6 * time.Minute)
	e.s.OutageRound(ctx)
	e.recoverContainer(t)
	sum, err := e.s.OrderOutages(buyer, no)
	require.NoError(t, err)
	assert.False(t, sum.RefundEligible)
	assert.Equal(t, 120, sum.TermMinutes)
	_, err = e.s.SetOutageRemedy(buyer, no, "refund")
	assert.Equal(t, errcode.Conflict, codeOf(err))

	// 累计达门槛, 退费通道未就绪: 50000 + 记录退费意向, 补偿方式仍是顺延。
	e.backdate(20 * time.Minute)
	e.nodeDown(8 * time.Minute)
	e.s.OutageRound(ctx)
	sum, err = e.s.OrderOutages(buyer, no)
	require.NoError(t, err)
	assert.True(t, sum.RefundEligible, "进行中的停机也计入门槛")
	_, err = e.s.SetOutageRemedy(buyer, no, "refund")
	assert.Equal(t, errcode.InternalError, codeOf(err))
	sum, err = e.s.OrderOutages(buyer, no)
	require.NoError(t, err)
	assert.Equal(t, "extend", sum.Remedy)
	assert.NotNil(t, sum.RefundRequestedAt)
	assert.False(t, sum.RefundAvailable)
	e.recoverContainer(t)

	// 注入 T-106 退费实现 + 书面确认开关: 选退费生效, 之后的停机进入待退费并交给退费实现。
	ref := &recRefunder{}
	e.s.SetOutageRefunder(ref)
	e.s.cfg.Outage.YeepayPartialRefundConfirmed = true
	sum, err = e.s.SetOutageRemedy(buyer, no, "refund")
	require.NoError(t, err)
	assert.Equal(t, "refund", sum.Remedy)
	untilBefore := e.times(t).AccessUntil
	e.backdate(20 * time.Minute)
	e.nodeDown(7 * time.Minute)
	e.s.OutageRound(ctx)
	e.recoverContainer(t)
	rows := e.outages(t)
	require.Len(t, rows, 3)
	last := rows[2]
	require.NotNil(t, last.Remedy)
	assert.Equal(t, "refund", *last.Remedy)
	assert.Nil(t, last.RemedyAppliedAt)
	require.NotNil(t, last.RemedyAmount)
	assert.Equal(t, *last.LiabilityAmount, *last.RemedyAmount)
	assert.Equal(t, untilBefore, e.times(t).AccessUntil, "选退费不顺延")
	assert.Equal(t, []int64{last.ID}, ref.ids)
	sum, err = e.s.OrderOutages(buyer, no)
	require.NoError(t, err)
	assert.Equal(t, *last.RemedyAmount, sum.PendingRefundTotal)

	// 改回顺延: 清除退费意向。
	sum, err = e.s.SetOutageRemedy(buyer, no, "extend")
	require.NoError(t, err)
	assert.Equal(t, "extend", sum.Remedy)
	assert.Nil(t, sum.RefundRequestedAt)

	// 运营检索待退费。
	list, total, _, _, err := e.s.AdminOutages(OutageFilter{RefundPending: true, SupplierID: 20}, 1, 20)
	require.NoError(t, err)
	assert.EqualValues(t, 1, total)
	require.Len(t, list, 1)
	assert.Equal(t, no, list[0].OrderNo)

	// 租期结束后不能改。
	expireLease(t, e)
	_, err = e.s.SetOutageRemedy(buyer, no, "extend")
	assert.Equal(t, errcode.Conflict, codeOf(err))
}

func TestCreditStopsDispatch(t *testing.T) {
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	s.cfg.Outage.MaxSupplierMinutes30d = 360
	s.cfg.Credit.StopBelow = 90
	nodes := seedLeaseMarket(t, db, s, "container", 2, 2, 1)
	comp := wireLeases(t, s)
	stock, _ := leaseStock(t, db)
	require.Equal(t, 4, stock)

	// 3 次完整探活失败 → 100-15=85 < 90: 全部节点停调度, 库存归零, 不能下单。
	for i := 0; i < 3; i++ {
		db.MustExec(`INSERT INTO audit_logs (action, target_type, target_id) VALUES ('node.verify_failed','supplier_node',?)`, nodes[0])
	}
	require.NoError(t, s.RecomputeCredit(20))
	c, err := s.getCredit(db, 20)
	require.NoError(t, err)
	assert.Equal(t, 85, c.Score)
	assert.Equal(t, 3, c.ProbeFailures30d)
	assert.True(t, c.DispatchStopped)
	assert.Equal(t, 2, countRows(t, db, `SELECT COUNT(*) FROM supplier_nodes WHERE dispatch_hold=1`))
	stock, status := leaseStock(t, db)
	assert.Equal(t, 0, stock)
	assert.Equal(t, "sold_out", status)
	_, err = placeManaged(comp, 21, 2)
	require.Error(t, err)
	assert.Equal(t, 2, countRows(t, db, `SELECT COUNT(*) FROM notifications WHERE user_id=20`))
	s.CreditRound(context.Background()) // 重复重算不重复通知
	assert.Equal(t, 2, countRows(t, db, `SELECT COUNT(*) FROM notifications WHERE user_id=20`))

	// 运营放行。
	_, err = s.AdminOverrideCredit(1, 20, "allow", "", "127.0.0.1")
	assert.Equal(t, errcode.ParamInvalid, codeOf(err), "放行须填理由")
	_, err = s.AdminOverrideCredit(1, 20, "pause", "x", "127.0.0.1")
	assert.Equal(t, errcode.ParamInvalid, codeOf(err))
	_, err = s.AdminSupplierCredit(999)
	assert.Equal(t, errcode.NotFound, codeOf(err))
	d, err := s.AdminOverrideCredit(1, 20, "allow", "已核实为探活镜像问题", "127.0.0.1")
	require.NoError(t, err)
	assert.False(t, d.DispatchStopped)
	assert.Equal(t, "allow", d.OverrideMode)
	require.Len(t, d.Nodes, 2)
	assert.False(t, d.Nodes[0].DispatchHold)
	stock, _ = leaseStock(t, db)
	assert.Equal(t, 4, stock)
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM audit_logs WHERE action='supplier.credit_override'`))
	o, err := placeManaged(comp, 21, 2)
	require.NoError(t, err)
	assert.NotEmpty(t, o.OrderNo)

	// 强制停 / 恢复自动(仍低于阈值 → 停)。
	d, err = s.AdminOverrideCredit(1, 20, "stop", "合规复查", "127.0.0.1")
	require.NoError(t, err)
	assert.True(t, d.DispatchStopped)
	assert.True(t, d.Nodes[1].DispatchHold)
	d, err = s.AdminOverrideCredit(1, 20, "auto", "", "127.0.0.1")
	require.NoError(t, err)
	assert.True(t, d.DispatchStopped)
	list, total, err := s.AdminSupplierCredits(true, 1, 20)
	require.NoError(t, err)
	assert.Equal(t, 1, total)
	assert.Equal(t, int64(20), list[0].SupplierID)

	// 探活失败超出 30 天窗口后恢复。
	db.MustExec(`UPDATE audit_logs SET created_at=NOW()-INTERVAL 31 DAY WHERE action='node.verify_failed'`)
	require.NoError(t, s.RecomputeCredit(20))
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM supplier_nodes WHERE dispatch_hold=1`))
}

func TestCreditScoreFormula(t *testing.T) {
	assert.Equal(t, 100, creditScore(0, 0, 0, 360))
	assert.Equal(t, 99, creditScore(1, 0, 0, 360))
	assert.Equal(t, 50, creditScore(360, 0, 0, 360))
	assert.Equal(t, 40, creditScore(100000, 0, 0, 360), "停机扣分封顶 60")
	assert.Equal(t, 80, creditScore(0, 10, 0, 360), "探活扣分封顶 20")
	assert.Equal(t, 70, creditScore(0, 0, 9, 360), "违约扣分封顶 30")
	assert.Equal(t, 0, creditScore(100000, 10, 9, 360))
}
