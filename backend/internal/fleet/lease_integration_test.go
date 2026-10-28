package fleet

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"testing"
	"time"

	"tokenfactory/internal/compute"
	"tokenfactory/internal/legal"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 托管下单/租约钩子/派生库存的端到端测试(真实 MySQL, 真实 compute 服务; 节点与卡槽直接按 verified 状态播种)。

const leaseProductID = 2

// seedLeaseMarket 供给方 20 + 买家 21..(21+buyers-1) + 托管容器商品 2(时租 15 元/卡时) + nodes 台 verified 节点,
// 每台 slotsPerNode 张空闲卡(两组拓扑)。返回节点 ID。
func seedLeaseMarket(t *testing.T, db *sqlx.DB, s *Service, delivery string, nodes, slotsPerNode, buyers int) []int64 {
	t.Helper()
	db.MustExec(`INSERT INTO users (id, phone, password_hash) VALUES (20,'13800000020','x')`)
	db.MustExec(`INSERT INTO user_roles (user_id, role) VALUES (20,'supplier')`)
	db.MustExec(`INSERT INTO enterprises (user_id, name, uscc, status) VALUES (20,'供给方','TESTSUP20','verified')`)
	db.MustExec(`INSERT INTO supplier_qualifications (user_id, qual_type, cert_name, status) VALUES (20,'idc','IDC 许可','verified')`)
	for i := 0; i < buyers; i++ {
		id := 21 + i
		db.MustExec(`INSERT INTO users (id, phone, password_hash) VALUES (?,?,'x')`, id, fmt.Sprintf("1390000%04d", id))
		db.MustExec(`INSERT INTO user_roles (user_id, role) VALUES (?,'buyer')`, id)
		db.MustExec(`INSERT INTO enterprises (user_id, name, uscc, status) VALUES (?,?,?,'verified')`, id, fmt.Sprintf("买家%d", id), fmt.Sprintf("TESTBUY%d", id))
	}
	db.MustExec(`INSERT INTO products (id, supplier_id, gpu_model, card_count, delivery_mode, fulfillment_mode, pricing_mode,
		unit_price, stock, status, region) VALUES (?, 20, 'H100-80G', ?, ?, 'managed', 'hourly', 1500, 0, 'sold_out', 'cn-north')`,
		leaseProductID, slotsPerNode, delivery)
	var ids []int64
	for i := 0; i < nodes; i++ {
		res := db.MustExec(`INSERT INTO supplier_nodes (supplier_id, product_id, node_name, status, total_cards, available_cards,
			mgmt_mode, ssh_host, ssh_port, ssh_user, verify_status, schedulable, disk_quota_supported, cpu_cores, mem_gb,
			lease_port_start, lease_port_end)
			VALUES (20, ?, ?, 'online', ?, ?, 'ssh', ?, 22, 'root', 'verified', 1, 1, 128, 1024, 30000, 30099)`,
			leaseProductID, fmt.Sprintf("n%d", i), slotsPerNode, slotsPerNode, fmt.Sprintf("10.9.0.%d", i+1))
		id, err := res.LastInsertId()
		require.NoError(t, err)
		ids = append(ids, id)
		for g := 0; g < slotsPerNode; g++ {
			db.MustExec(`INSERT INTO gpu_slots (node_id, gpu_index, gpu_uuid, pci_bus_id, vram_mb, topo_group) VALUES (?,?,?,?,81920,?)`,
				id, g, fmt.Sprintf("GPU-%d-%d", id, g), fmt.Sprintf("0000:%02x:00.0", g), fmt.Sprintf("g%d", g/4))
		}
	}
	require.NoError(t, s.RefreshManagedStock(leaseProductID))
	return ids
}

// wireLeases 等同 main 的 fleet.enabled=true 接线。
func wireLeases(t *testing.T, s *Service) *compute.Service {
	t.Helper()
	comp, ok := s.products.(*compute.Service)
	require.True(t, ok)
	comp.SetManagedGate(s)
	comp.SetLeaseHooks(s.LeaseHooks())
	return comp
}

func placeManaged(comp *compute.Service, buyerID int64, cards int) (*compute.Order, error) {
	return comp.PlaceOrder(buyerID, compute.PlaceOrderReq{ProductID: leaseProductID, Quantity: cards, Duration: 2,
		ComplianceAgreed: true, ComplianceVersion: legal.Version})
}

func leaseStock(t *testing.T, db *sqlx.DB) (int, string) {
	t.Helper()
	var r struct {
		Stock  int    `db:"stock"`
		Status string `db:"status"`
	}
	require.NoError(t, db.Get(&r, `SELECT stock, status FROM products WHERE id=?`, leaseProductID))
	return r.Stock, r.Status
}

func realFree(t *testing.T, db *sqlx.DB) int {
	return countRows(t, db, `SELECT COUNT(*) FROM gpu_slots s JOIN supplier_nodes n ON n.id=s.node_id
		WHERE n.product_id=? AND n.verify_status='verified' AND n.schedulable=1 AND n.status='online' AND s.state='free'`, leaseProductID)
}

func TestManagedPlaceOrderConcurrentReservation(t *testing.T) {
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	nodes := seedLeaseMarket(t, db, s, "container", 2, 8, 20)
	comp := wireLeases(t, s)
	stock, status := leaseStock(t, db)
	require.Equal(t, 16, stock)
	require.Equal(t, "active", status)

	// 20 个买家同时各下 4 卡: 2 台 × 8 卡只够 4 单, 其余必须失败且不留任何占用。
	var wg sync.WaitGroup
	start := make(chan struct{})
	var mu sync.Mutex
	var okOrders []*compute.Order
	var fails []error
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(buyer int64) {
			defer wg.Done()
			<-start
			o, err := placeManaged(comp, buyer, 4)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				fails = append(fails, err)
				return
			}
			okOrders = append(okOrders, o)
		}(int64(21 + i))
	}
	close(start)
	wg.Wait()
	require.Len(t, okOrders, 4, "fails=%v", fails)
	for _, err := range fails {
		// 无整机余量(40900)或商品已售罄下架(下单前读到 sold_out); 不得出现死锁等其他错误。
		if codeOf(err) != 40900 {
			assert.Contains(t, err.Error(), "product not available")
		}
	}

	// 每张卡只被一个租约预留; 每个租约 4 张卡同机且挂在自己的节点上。
	assert.Equal(t, 16, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE state='reserved' AND lease_id IS NOT NULL`))
	assert.Equal(t, 4, countRows(t, db, `SELECT COUNT(*) FROM leases WHERE state='reserved'`))
	assert.Equal(t, 4, countRows(t, db, `SELECT COUNT(*) FROM orders WHERE fulfillment_mode='managed'`))
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM orders WHERE fulfillment_mode='managed' AND stock_reserved<>0`))
	for _, o := range okOrders {
		l, err := s.repo.getLeaseByOrder(db, o.ID)
		require.NoError(t, err)
		require.NotNil(t, l)
		assert.Contains(t, nodes, l.NodeID)
		assert.Equal(t, 4, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND node_id=?`, l.ID, l.NodeID))
		assert.True(t, ValidLeaseNo(l.LeaseNo), l.LeaseNo)
	}
	stock, status = leaseStock(t, db)
	assert.Equal(t, 0, stock)
	assert.Equal(t, "sold_out", status)
}

// CR-S1 M3: 同商品多节点并发改卡槽 + 并发下单, 派生库存不得丢更新(最终 = 真实空闲数)。
func TestManagedStockNoLostUpdate(t *testing.T) {
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	nodes := seedLeaseMarket(t, db, s, "container", 4, 8, 8)
	comp := wireLeases(t, s)

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make(chan error, 256)
	// 每台节点一个 goroutine 反复停用/启用自己的 6 号、7 号卡(供给方单卡停用路径, 各自事务内重算库存)。
	for _, nodeID := range nodes {
		wg.Add(1)
		go func(nodeID int64) {
			defer wg.Done()
			<-start
			for round := 0; round < 6; round++ {
				for _, g := range []int{6, 7} {
					st := SlotWithheld
					if round%2 == 1 {
						st = SlotFree
					}
					// 卡被并发下单预留走时 40900 是正确结果(仅 free↔withheld 可切换)。
					if err := s.SetSlotState(20, nodeID, g, st, "127.0.0.1"); err != nil && codeOf(err) != 40900 {
						errs <- fmt.Errorf("node %d slot %d → %s: %w", nodeID, g, st, err)
					}
				}
			}
			// 最后停用 7 号卡, 留下与初始不同的终态。
			if err := s.SetSlotState(20, nodeID, 7, SlotWithheld, "127.0.0.1"); err != nil && codeOf(err) != 40900 {
				errs <- err
			}
		}(nodeID)
	}
	// 同时 8 个买家各下 2 卡。
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(buyer int64) {
			defer wg.Done()
			<-start
			if _, err := placeManaged(comp, buyer, 2); err != nil {
				errs <- fmt.Errorf("buyer %d: %w", buyer, err)
			}
		}(int64(21 + i))
	}
	close(start)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("并发操作失败: %v", err)
	}
	// 8 单 × 2 卡全部成功; 其余卡 free 或 withheld(取决于交错), 派生库存必须恰为真实空闲数。
	assert.Equal(t, 16, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE state='reserved'`))
	free := realFree(t, db)
	assert.Equal(t, 32-16, free+countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE state='withheld'`))
	require.Greater(t, free, 0)
	stock, status := leaseStock(t, db)
	assert.Equal(t, free, stock, "派生库存必须等于真实空闲卡数")
	assert.Equal(t, "active", status)
}

func TestManagedLeaseHooksLifecycle(t *testing.T) {
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	s.cfg.Grace.Short, s.cfg.Grace.Long = 24*time.Hour, 72*time.Hour
	seedLeaseMarket(t, db, s, "container", 1, 8, 3)
	comp, ok := s.products.(*compute.Service)
	require.True(t, ok)

	// fleet 已启用但租约钩子未接入: 50000 明确拒绝, 不落任何数据。
	comp.SetManagedGate(s)
	_, err := placeManaged(comp, 21, 2)
	require.Error(t, err)
	assert.Equal(t, 50000, codeOf(err))
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM orders`))
	comp.SetLeaseHooks(s.LeaseHooks())

	// ① 下单 → 租约 reserved, 2 卡 reserved, 库存 8→6; 订单不占 stock_reserved。
	o1, err := placeManaged(comp, 21, 2)
	require.NoError(t, err)
	assert.Equal(t, compute.FulfillmentManaged, o1.FulfillmentMode)
	l1, err := s.repo.getLeaseByOrder(db, o1.ID)
	require.NoError(t, err)
	require.NotNil(t, l1)
	assert.Equal(t, LeaseReserved, l1.State)
	assert.Equal(t, "container", l1.DeliveryKind)
	stock, _ := leaseStock(t, db)
	assert.Equal(t, 6, stock)
	// 同组优先: 两张卡来自同一拓扑组。
	slots, err := s.repo.leaseSlots(db, l1.ID)
	require.NoError(t, err)
	require.Len(t, slots, 2)
	assert.Equal(t, slots[0].TopoGroup, slots[1].TopoGroup)

	// ② 收款 → 租约 pending + provision 任务(优先级 3, 绑定 lease_id)。
	payOrder(t, db, comp, o1.OrderNo)
	l1, _ = s.repo.getLease(db, l1.ID)
	assert.Equal(t, LeasePending, l1.State)
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='provision' AND state='pending' AND priority=3`, l1.ID))
	// 重复收款回调: 租约已非 reserved → 钩子报错, 整个事务回滚。
	tx := db.MustBegin()
	err = comp.OnOrderPaidTx(tx, o1)
	require.Error(t, err)
	tx.Rollback()

	// ③ 已支付未开通即取消 → 租约 cancelled, 卡槽回到 free, 未领取的开通任务作废, 库存恢复。
	require.NoError(t, comp.CancelOrder(o1.OrderNo))
	l1, _ = s.repo.getLease(db, l1.ID)
	assert.Equal(t, LeaseCancelled, l1.State)
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=?`, l1.ID))
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='provision' AND state='failed'`, l1.ID))
	stock, _ = leaseStock(t, db)
	assert.Equal(t, 8, stock)
	// 再次取消: 订单守卫式迁移失败, 卡槽不会被重复释放。
	_ = comp.CancelOrder(o1.OrderNo)
	assert.Equal(t, 8, realFree(t, db))

	// ④ 运行中的租约到期完成 → grace(时租 = grace.short), 卡槽 draining, stop 任务优先级 1。
	o2, err := placeManaged(comp, 22, 4)
	require.NoError(t, err)
	payOrder(t, db, comp, o2.OrderNo)
	l2, _ := s.repo.getLeaseByOrder(db, o2.ID)
	activate(t, db, o2.ID, l2.ID)
	before := time.Now()
	require.NoError(t, comp.CompleteOrder(o2.OrderNo))
	l2, _ = s.repo.getLease(db, l2.ID)
	assert.Equal(t, LeaseGrace, l2.State)
	require.NotNil(t, l2.GraceUntil)
	assert.WithinDuration(t, before.Add(24*time.Hour), *l2.GraceUntil, time.Minute)
	assert.Equal(t, 4, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='draining'`, l2.ID))
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='stop' AND priority=1`, l2.ID))
	stock, _ = leaseStock(t, db)
	assert.Equal(t, 4, stock, "draining 卡不计入库存, 清理完成(T-087)后才回到 free")

	// ⑤ 运行中被运营取消 → purging(宽限 0), purge 任务优先级 8。
	o3, err := placeManaged(comp, 23, 2)
	require.NoError(t, err)
	payOrder(t, db, comp, o3.OrderNo)
	l3, _ := s.repo.getLeaseByOrder(db, o3.ID)
	activate(t, db, o3.ID, l3.ID)
	require.NoError(t, comp.AdminUpdateOrderStatus(1, o3.OrderNo, "cancelled", "127.0.0.1"))
	l3, _ = s.repo.getLease(db, l3.ID)
	assert.Equal(t, LeasePurging, l3.State)
	assert.Equal(t, 2, countRows(t, db, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND state='draining'`, l3.ID))
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND kind='purge' AND priority=8`, l3.ID))
	stock, _ = leaseStock(t, db)
	assert.Equal(t, 2, stock)

	// ⑥ 托管订单续租: T-088 前 50000 明确拒绝。
	o4, err := placeManaged(comp, 23, 1)
	require.NoError(t, err)
	payOrder(t, db, comp, o4.OrderNo)
	l4, _ := s.repo.getLeaseByOrder(db, o4.ID)
	activate(t, db, o4.ID, l4.ID)
	_, err = comp.GetRenewalQuote(23, o4.OrderNo, 1)
	require.Error(t, err)
	assert.Equal(t, 50000, codeOf(err))
	assert.Contains(t, err.Error(), "T-088")
}

// payOrder 收款确认的订单侧最小等价(payment.confirmPayment: 守卫式 → paid + OnOrderPaidTx 同事务)。
func payOrder(t *testing.T, db *sqlx.DB, comp *compute.Service, orderNo string) {
	t.Helper()
	tx := db.MustBegin()
	defer tx.Rollback()
	res := tx.MustExec(`UPDATE orders SET status='paid' WHERE order_no=? AND status='pending_payment'`, orderNo)
	n, _ := res.RowsAffected()
	require.EqualValues(t, 1, n)
	o, err := comp.GetOrder(orderNo)
	require.NoError(t, err)
	o.Status = "paid"
	require.NoError(t, comp.OnOrderPaidTx(tx, o))
	require.NoError(t, tx.Commit())
}

// activate 开通完成的数据面效果(开通流水线属 T-084): 订单 active, 租约 active, 卡槽 leased。
func activate(t *testing.T, db *sqlx.DB, orderID, leaseID int64) {
	t.Helper()
	db.MustExec(`UPDATE orders SET status='active', lease_start_at=NOW(), lease_end_at=NOW() + INTERVAL 2 HOUR WHERE id=?`, orderID)
	db.MustExec(`UPDATE leases SET state='active' WHERE id=?`, leaseID)
	db.MustExec(`UPDATE gpu_slots SET state='leased' WHERE lease_id=?`, leaseID)
}

// CR-S1 M3 确定性复现: 两台节点各自改卡槽后重算库存, tx2 的重算在 tx1 持有商品行锁期间发起。
// 修复前 tx2 先做快照计数(看不到 tx1 未提交的变更)再在 UPDATE products 上排队, tx1 提交后写入陈旧计数 → 丢更新;
// 修复后 tx2 在计数之前就阻塞在商品行锁上, 拿到锁后在 READ COMMITTED 下计数能看到 tx1 的提交。
func TestRecomputeStockSerializedByProductLock(t *testing.T) {
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	nodes := seedLeaseMarket(t, db, s, "container", 2, 8, 0)
	stock, _ := leaseStock(t, db)
	require.Equal(t, 16, stock)

	begin := func() *sqlx.Tx {
		tx, err := db.BeginTxx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
		require.NoError(t, err)
		return tx
	}
	tx1, tx2 := begin(), begin()
	defer tx1.Rollback()
	defer tx2.Rollback()
	tx1.MustExec(`UPDATE gpu_slots SET state='withheld' WHERE node_id=? AND gpu_index=0`, nodes[0])
	tx2.MustExec(`UPDATE gpu_slots SET state='withheld' WHERE node_id=? AND gpu_index=0`, nodes[1])
	require.NoError(t, s.recomputeStockTx(tx1, leaseProductID)) // tx1 持有商品行锁, 写入 15

	done := make(chan error, 1)
	go func() { done <- s.recomputeStockTx(tx2, leaseProductID) }()
	// 等 tx2 进入锁等待(修复后等在 SELECT … FOR UPDATE, 修复前等在 UPDATE products)。
	deadline := time.Now().Add(5 * time.Second)
	for countRows(t, db, `SELECT COUNT(*) FROM information_schema.INNODB_TRX WHERE trx_state='LOCK WAIT'`) == 0 {
		require.True(t, time.Now().Before(deadline), "tx2 应在商品行锁上等待")
		time.Sleep(20 * time.Millisecond)
	}
	require.NoError(t, tx1.Commit())
	require.NoError(t, <-done)
	require.NoError(t, tx2.Commit())

	stock, _ = leaseStock(t, db)
	assert.Equal(t, 14, stock, "两台节点各停用 1 张卡, 库存必须为 14(15 即丢更新)")
	assert.Equal(t, realFree(t, db), stock)
}
