package fleet

import (
	"sync"
	"testing"

	"tokenfactory/internal/compute"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// fireTogether 同时启动全部动作并等待结束, 返回各自的错误。
func fireTogether(actions ...func() error) []error {
	errs := make([]error, len(actions))
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i, act := range actions {
		wg.Add(1)
		go func(i int, act func() error) {
			defer wg.Done()
			<-start
			errs[i] = act()
		}(i, act)
	}
	close(start)
	wg.Wait()
	return errs
}

func leaseStateOf(t *testing.T, db *sqlx.DB, orderID int64) string {
	t.Helper()
	var st string
	require.NoError(t, db.Get(&st, `SELECT state FROM leases WHERE order_id=?`, orderID))
	return st
}

func orderStatusOf(t *testing.T, db *sqlx.DB, orderNo string) string {
	t.Helper()
	var st string
	require.NoError(t, db.Get(&st, `SELECT status FROM orders WHERE order_no=?`, orderNo))
	return st
}

// T-093 / docs/26 §26.5.3: 关单超时、买家取消、运营关单、退款完成同时命中同一笔托管订单,
// 卡槽只释放一次, 派生库存回到真实空闲数, 不出现死锁或重复流转。
func TestManagedReleaseRaceReleasesOnce(t *testing.T) {
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	seedLeaseMarket(t, db, s, "container", 2, 8, 3)
	comp := wireLeases(t, s)
	db.MustExec(`INSERT INTO users (id, phone, password_hash) VALUES (90,'13800000090','x')`)
	db.MustExec(`INSERT INTO user_roles (user_id, role) VALUES (90,'admin')`)

	// 场景 1: 待支付订单已超时 —— 关单定时任务 ×3 + 买家取消 + 运营关单 + 退款完成(不适用, 必须无操作)。
	unpaid, err := placeManaged(comp, 21, 4)
	require.NoError(t, err)
	db.MustExec(`UPDATE orders SET payment_expires_at = NOW() - INTERVAL 1 MINUTE WHERE id=?`, unpaid.ID)
	// 另一单保持占用, 证明重算的是真实空闲而非简单加回。
	keep, err := placeManaged(comp, 22, 4)
	require.NoError(t, err)
	require.Equal(t, 8, realFree(t, db))

	var closedMu sync.Mutex
	closed := 0
	closeJob := func() error {
		n, err := comp.CloseExpiredUnpaidOrders()
		closedMu.Lock()
		closed += n
		closedMu.Unlock()
		return err
	}
	errs := fireTogether(closeJob, closeJob, closeJob,
		func() error { return comp.CancelBuyerOrder(21, unpaid.OrderNo) },
		func() error { return comp.AdminUpdateOrderStatus(90, unpaid.OrderNo, "cancelled", "127.0.0.1") },
		func() error { return comp.CompleteRefund(unpaid.OrderNo) },
	)
	for i, e := range errs[:3] {
		assert.NoError(t, e, "close job %d", i)
	}
	// 输掉竞争的买家取消/运营关单只能得到"已变化"类错误, 不得是死锁或内部错误。
	for _, e := range errs[3:5] {
		if e != nil {
			assert.NotEqual(t, 50000, compute.ErrToCode(e), e.Error())
			assert.NotContains(t, e.Error(), "Deadlock")
		}
	}
	assert.NoError(t, errs[5])
	assert.LessOrEqual(t, closed, 1)
	assert.Equal(t, "cancelled", orderStatusOf(t, db, unpaid.OrderNo))
	assert.Equal(t, LeaseCancelled, leaseStateOf(t, db, unpaid.ID))
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots s JOIN leases l ON l.id=s.lease_id WHERE l.order_id=? AND s.state<>'free'`, unpaid.ID))
	assert.Equal(t, 12, realFree(t, db))
	stock, status := leaseStock(t, db)
	assert.Equal(t, 12, stock)
	assert.Equal(t, "active", status)
	assert.Equal(t, LeaseReserved, leaseStateOf(t, db, keep.ID), "其他订单的占用不受影响")

	// 场景 2: 已付款订单进入退款中 —— 退款完成 ×3 与 关单定时任务 + 运营关单 同时命中。
	paid, err := placeManaged(comp, 23, 4)
	require.NoError(t, err)
	payOrder(t, db, comp, paid.OrderNo)
	db.MustExec(`UPDATE orders SET status='refunding', payment_expires_at = NOW() - INTERVAL 1 MINUTE WHERE id=? AND status='paid'`, paid.ID)
	require.Equal(t, 8, realFree(t, db))

	errs = fireTogether(
		func() error { return comp.CompleteRefund(paid.OrderNo) },
		func() error { return comp.CompleteRefund(paid.OrderNo) },
		func() error { return comp.CompleteRefund(paid.OrderNo) },
		closeJob,
		func() error { return comp.AdminUpdateOrderStatus(90, paid.OrderNo, "cancelled", "127.0.0.1") },
	)
	for _, e := range errs[:4] {
		assert.NoError(t, e)
	}
	require.Error(t, errs[4], "退款中的订单不允许运营关单")
	assert.Equal(t, "refunded", orderStatusOf(t, db, paid.OrderNo))
	assert.Equal(t, LeaseCancelled, leaseStateOf(t, db, paid.ID))
	assert.Equal(t, 12, realFree(t, db))
	stock, _ = leaseStock(t, db)
	assert.Equal(t, 12, stock)
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM node_tasks WHERE state='pending' AND lease_id=(SELECT id FROM leases WHERE order_id=?)`, paid.ID),
		"已入队的开通任务随取消作废")

	// 重复调用全部幂等, 不再改变库存。
	require.NoError(t, comp.CompleteRefund(paid.OrderNo))
	require.NoError(t, comp.CancelBuyerOrder(21, unpaid.OrderNo))
	n, err := comp.CloseExpiredUnpaidOrders()
	require.NoError(t, err)
	assert.Equal(t, 0, n)
	stock, _ = leaseStock(t, db)
	assert.Equal(t, 12, stock)
}
