package fleet

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"tokenfactory/internal/compute"
	"tokenfactory/internal/legal"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// T-088 托管续租集成测试: 真实 MySQL + compute 续租事务 + fleet RenewTx + 开通流水线(fakeNode)。

// renewManaged 按报价下续租单(买家 21)。
func renewManaged(t *testing.T, e *provEnv, duration int) (*compute.RenewalQuote, *compute.Order) {
	t.Helper()
	q, err := e.comp.GetRenewalQuote(21, e.order.OrderNo, duration)
	require.NoError(t, err)
	child, err := e.comp.RenewOrder(21, e.order.OrderNo, compute.RenewOrderReq{ExpectedPricingMode: q.PricingMode,
		Duration: duration, RequestID: uuid.NewString(), ComplianceAgreed: true, ComplianceVersion: legal.Version,
		ExpectedLeaseEndAt: q.LeaseEndAt, ExpectedRenewedUntil: q.RenewedUntil, ExpectedTotalAmount: q.TotalAmount,
		ExpectedPlatformFee: q.PlatformFee})
	require.NoError(t, err)
	return q, child
}

// payRenewal 续租单收款的订单侧最小等价(payment.confirmPayment: → paid + Service.ApplyRenewalPaymentTx 同事务)。
func payRenewal(t *testing.T, e *provEnv, childNo string) {
	t.Helper()
	tx := e.db.MustBegin()
	defer tx.Rollback()
	child, parent, r, err := compute.NewRepository(e.db).LockOrderFamilyTx(tx, childNo)
	require.NoError(t, err)
	require.NotNil(t, r)
	tx.MustExec(`UPDATE orders SET status='paid' WHERE order_no=? AND status='pending_payment'`, childNo)
	require.NoError(t, e.comp.ApplyRenewalPaymentTx(tx, child, parent, r))
	require.NoError(t, tx.Commit())
}

// graceProvEnv 在租 → 到期进宽限 → stop 执行完毕(原卡 0/1 释放为 free, 容器停止保留)。
func graceProvEnv(t *testing.T) *provEnv {
	t.Helper()
	e := activeProvEnv(t)
	expireLease(t, e)
	e.s.ExpiryRound(context.Background())
	require.Equal(t, LeaseGrace, e.leaseNow(t).State)
	e.fake.Override(cmdOK(stopContainerCommand(e.lease.LeaseNo)), fakeReply{Stdout: "@@stopped " + fakeUUID0 + "," + fakeUUID1 + ",\n"})
	stop := runOne(t, e.s)
	require.Equal(t, KindStop, stop.Kind)
	require.Equal(t, TaskSucceeded, stop.State, "last_error=%v", stop.LastError)
	require.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE node_id=? AND state='free'`, e.node))
	return e
}

func childStatus(t *testing.T, e *provEnv, no string) string {
	t.Helper()
	var st string
	require.NoError(t, e.db.Get(&st, `SELECT status FROM orders WHERE order_no=?`, no))
	return st
}

func TestManagedExtendRenewal(t *testing.T) {
	e := activeProvEnv(t)
	before := e.leaseNow(t)
	q, child := renewManaged(t, e, 1)
	assert.Equal(t, "extend", q.Mode)
	require.NotNil(t, q.RenewedUntil)
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='reserved'`, e.lease.ID),
		"extend 不预留卡槽")

	time.Sleep(1100 * time.Millisecond) // access_changed_at 精度为秒
	payRenewal(t, e, child.OrderNo)
	l := e.leaseNow(t)
	require.NotNil(t, l.AccessUntil)
	assert.WithinDuration(t, *q.RenewedUntil, *l.AccessUntil, time.Second, "access_until 顺延到续租终点")
	assert.True(t, l.AccessChangedAt.After(before.AccessChangedAt), "刷新网关撤销游标")
	assert.Equal(t, LeaseActive, l.State)
	assert.Equal(t, "active", orderStatus(t, e))
}

func TestManagedExtendRejectedAfterLeaseExpiry(t *testing.T) {
	e := activeProvEnv(t)
	q, child := renewManaged(t, e, 1)
	require.Equal(t, "extend", q.Mode)
	// 下单后、收款前租约已到期(access_until 已过): 收款校验失败, 款项原路退回(由 payment 处理)。
	e.db.MustExec(`UPDATE leases SET access_until=NOW()-INTERVAL 1 MINUTE WHERE id=?`, e.lease.ID)
	tx := e.db.MustBegin()
	defer tx.Rollback()
	c, p, r, err := compute.NewRepository(e.db).LockOrderFamilyTx(tx, child.OrderNo)
	require.NoError(t, err)
	assert.ErrorIs(t, e.comp.ValidateRenewalPaymentTx(tx, c, p, r), compute.ErrRenewalConflict)
}

func TestManagedRestartInGraceResumesOnOriginalNode(t *testing.T) {
	e := graceProvEnv(t)
	q, child := renewManaged(t, e, 2)
	assert.Equal(t, "restart", q.Mode)
	assert.Equal(t, LeaseGrace, e.leaseNow(t).State)
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND node_id=? AND state='reserved'`,
		e.lease.ID, e.node), "原节点原卡预留")
	stock, _ := leaseStock(t, e.db)
	assert.Equal(t, 0, stock, "预留卡不计入派生库存")

	// 支付窗口内宽限期满: 暂缓清除。
	e.db.MustExec(`UPDATE leases SET grace_until=NOW()-INTERVAL 1 MINUTE WHERE id=?`, e.lease.ID)
	e.s.ExpiryRound(context.Background())
	require.Equal(t, LeaseGrace, e.leaseNow(t).State)

	payRenewal(t, e, child.OrderNo)
	assert.Equal(t, "paid", orderStatus(t, e))
	assert.Equal(t, "completed", childStatus(t, e, child.OrderNo))
	l := e.leaseNow(t)
	require.Equal(t, LeasePending, l.State)
	assert.Nil(t, l.AccessUntil)
	task := leaseTaskOf(t, e, KindProvision)
	var pl LeaseProvisionPayload
	require.NoError(t, json.Unmarshal(task.Payload, &pl))
	assert.True(t, pl.Renewal)
	assert.True(t, pl.Resume)

	// 原容器在、卡集合一致 → docker start, 不重建。
	no := e.lease.LeaseNo
	e.fake.Override(cmdOK(containerStateCommand(no)), fakeReply{Stdout: "@@state false " + fakeUUID0 + "," + fakeUUID1 + ",\n"})
	e.fake.Override(cmdOK(startContainerCommand(no)), fakeReply{Stdout: "@@started\n"})
	out := e.outcome(t, runOne(t, e.s))
	assert.Equal(t, "started", out.Resume)
	assert.Len(t, e.cmdsWith("set -e; (docker network inspect omnis-l-"), 1, "只有首次开通建过容器")

	l = e.leaseNow(t)
	assert.Equal(t, LeaseActive, l.State)
	assert.Nil(t, l.GraceUntil)
	assert.Equal(t, "active", orderStatus(t, e))
	var c struct {
		Start   *time.Time `db:"lease_start_at"`
		End     *time.Time `db:"lease_end_at"`
		Confirm *time.Time `db:"confirmed_at"`
	}
	require.NoError(t, e.db.Get(&c, `SELECT o.lease_start_at, o.lease_end_at, r.confirmed_at FROM orders o
		JOIN order_renewals r ON r.order_id=o.id WHERE o.order_no=?`, child.OrderNo))
	require.NotNil(t, c.Start)
	require.NotNil(t, c.End)
	require.NotNil(t, c.Confirm)
	assert.Equal(t, 2*time.Hour, c.End.Sub(*c.Start), "新租期按续租单时长自自检通过起算")
	assert.WithinDuration(t, *c.End, *l.AccessUntil, time.Second)
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='leased'`, e.lease.ID))
}

func TestManagedRestartInGraceRecreatesWhenGPUsChanged(t *testing.T) {
	e := graceProvEnv(t)
	_, child := renewManaged(t, e, 1)
	payRenewal(t, e, child.OrderNo)

	no := e.lease.LeaseNo
	// 原容器挂的卡与本次预留不同(例: 宽限期内原卡被转租后换卡) → commit 后以新卡重建, 挂原数据卷。
	e.fake.Override(cmdOK(containerStateCommand(no)), fakeReply{Stdout: "@@state false " + fakeUUID1 + ",\n"})
	e.fake.Override("docker commit "+containerNameOf(no)+" omnis-commit/"+no+":", fakeReply{Stdout: "@@committed\n"})
	out := e.outcome(t, runOne(t, e.s))
	assert.Equal(t, "recreated", out.Resume)
	created := e.cmdsWith("set -e; (docker network inspect omnis-l-")
	require.Len(t, created, 2)
	l := e.leaseNow(t)
	require.NotNil(t, l.CommitImage)
	assert.True(t, isLeaseCommitRef(no, *l.CommitImage))
	assert.Contains(t, created[1], "--entrypoint /bin/sh "+*l.CommitImage+" -c '")
	assert.Equal(t, LeaseActive, l.State)
}

func TestManagedRestartRejectedWhenOriginalNodeFull(t *testing.T) {
	e := graceProvEnv(t)
	// 原卡宽限期内被别人租走(2 卡节点占满)。
	e.db.MustExec(`INSERT INTO users (id, phone, password_hash) VALUES (22,'13900000022','x')`)
	e.db.MustExec(`INSERT INTO user_roles (user_id, role) VALUES (22,'buyer')`)
	e.db.MustExec(`INSERT INTO enterprises (user_id, name, uscc, status) VALUES (22,'买家22','TESTBUY22','verified')`)
	other, err := placeManaged(e.comp, 22, 2)
	require.NoError(t, err)
	require.NotNil(t, other)
	_, err = e.comp.GetRenewalQuote(21, e.order.OrderNo, 1)
	require.Error(t, err)
	assert.Equal(t, 40900, codeOf(err))
	assert.Contains(t, err.Error(), "原节点")
}

func TestManagedRestartUnpaidCloseReleasesReservation(t *testing.T) {
	e := graceProvEnv(t)
	_, child := renewManaged(t, e, 1)
	require.NoError(t, e.comp.CancelOrder(child.OrderNo))
	assert.Equal(t, "cancelled", childStatus(t, e, child.OrderNo))
	assert.Equal(t, LeaseGrace, e.leaseNow(t).State)
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='reserved'`, e.lease.ID))
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='draining'`, e.lease.ID))
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='stop' AND state='pending'`, e.lease.ID),
		"预留退回 draining, 由 stop 释放")
}

func TestManagedRestartFailureRefundsRenewalAndReturnsToGrace(t *testing.T) {
	e := graceProvEnv(t)
	graceUntil := e.leaseNow(t).GraceUntil
	require.NotNil(t, graceUntil)
	var prevEnd time.Time
	require.NoError(t, e.db.Get(&prevEnd, `SELECT lease_end_at FROM orders WHERE id=?`, e.order.ID))
	_, child := renewManaged(t, e, 1)
	payRenewal(t, e, child.OrderNo)

	task := leaseTaskOf(t, e, KindProvision)
	e.s.onProvisionDead(task, "原容器启动失败")

	assert.Equal(t, "cancelled", childStatus(t, e, child.OrderNo), "续租单取消 → 自动原路退款")
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM orders WHERE order_no=? AND lease_start_at IS NULL AND lease_end_at IS NULL`, child.OrderNo))
	assert.Equal(t, "completed", orderStatus(t, e), "原订单回到 completed, 不退原订单")
	var end time.Time
	require.NoError(t, e.db.Get(&end, `SELECT lease_end_at FROM orders WHERE id=?`, e.order.ID))
	assert.WithinDuration(t, prevEnd, end, time.Second)
	l := e.leaseNow(t)
	assert.Equal(t, LeaseGrace, l.State, "数据在原节点: 不换节点, 回到宽限期")
	require.NotNil(t, l.GraceUntil)
	assert.WithinDuration(t, *graceUntil, *l.GraceUntil, time.Second, "保留期沿用续租前的 grace_until")
	assert.Equal(t, 2, countRows(t, e.db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='draining'`, e.lease.ID))
	assert.Equal(t, 0, countRows(t, e.db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='purge'`, e.lease.ID), "不清理现场")
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM notifications WHERE user_id=21 AND title='续租重新开通失败, 续租款将全额退回'`))
}

func TestManagedRestartAfterPurgeProvisionsFresh(t *testing.T) {
	e := graceProvEnv(t)
	e.db.MustExec(`UPDATE leases SET grace_until=NOW()-INTERVAL 1 MINUTE WHERE id=?`, e.lease.ID)
	e.s.ExpiryRound(context.Background())
	require.Equal(t, LeasePurging, e.leaseNow(t).State)
	e.overridePurge(t, fakeReply{Stdout: "@@removed\n"})
	purge := runOne(t, e.s)
	require.Equal(t, TaskSucceeded, purge.State, "last_error=%v", purge.LastError)
	require.Equal(t, LeasePurged, e.leaseNow(t).State)

	q, child := renewManaged(t, e, 1)
	assert.Equal(t, "restart", q.Mode)
	payRenewal(t, e, child.OrderNo)
	l := e.leaseNow(t)
	require.Equal(t, LeasePending, l.State)
	assert.Nil(t, l.PurgedAt)
	assert.Nil(t, l.ContainerName)
	var pl LeaseProvisionPayload
	require.NoError(t, json.Unmarshal(leaseTaskOf(t, e, KindProvision).Payload, &pl))
	assert.True(t, pl.Renewal)
	assert.False(t, pl.Resume)

	out := e.outcome(t, runOne(t, e.s))
	assert.Empty(t, out.Resume)
	assert.Len(t, e.cmdsWith("set -e; (docker network inspect omnis-l-"), 2, "全新开通")
	assert.Equal(t, LeaseActive, e.leaseNow(t).State)
	assert.Equal(t, "active", orderStatus(t, e))
}
