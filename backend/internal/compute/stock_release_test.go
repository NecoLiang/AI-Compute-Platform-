package compute

// 库存归还与到期处理的集成测试 (REQ-A-012 / REQ-A-023 / REQ-A-043)。
//
// 这些逻辑的正确性几乎全在 SQL 里 —— 条件 UPDATE 的 RowsAffected 判定、
// 事务隔离下的并发串行化、NULL 租期的排除。纯函数测试覆盖不到, 必须打真库。
//
// 默认跳过。需要真实 MySQL 时设置 DSN 后运行:
//   TEST_MYSQL_DSN='root:pw@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true' go test ./internal/compute/ -run Stock -v
//
// 测试会自行建立独立库 tokenfactory_stock_test 并在结束时删除, 不触碰业务库。

import (
	"database/sql"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"
	_ "github.com/go-sql-driver/mysql"
)

const stockTestDB = "tokenfactory_stock_test"

func setupStockDB(t *testing.T) (*sqlx.DB, *Service) {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 TEST_MYSQL_DSN, 跳过库存集成测试")
	}

	root, err := sqlx.Connect("mysql", dsn)
	if err != nil { t.Fatalf("连接 MySQL 失败: %v", err) }
	defer root.Close()

	root.MustExec("DROP DATABASE IF EXISTS " + stockTestDB)
	root.MustExec("CREATE DATABASE " + stockTestDB + " CHARACTER SET utf8mb4")

	// loc 必须与库的会话时区一致, 否则时间戳会有 8 小时偏移, 测出来的是假结果。
	// DSN 统一为文件头注释的 "/?" 格式, 与 blockchain/scheduler 集成测试共用一个环境变量。
	testDSN := strings.Replace(dsn, "/?", "/"+stockTestDB+"?", 1)
	if !strings.Contains(testDSN, "loc=") {
		testDSN += "&loc=Asia%2FShanghai"
	}
	db, err := sqlx.Connect("mysql", testDSN)
	if err != nil { t.Fatalf("连接测试库失败: %v", err) }

	db.MustExec(`CREATE TABLE products (
		id BIGINT PRIMARY KEY AUTO_INCREMENT,
		supplier_id BIGINT NOT NULL DEFAULT 1,
		stock INT NOT NULL DEFAULT 0,
		status ENUM('draft','pending','active','sold_out','offline','frozen') DEFAULT 'active'
	)`)
	db.MustExec(`CREATE TABLE orders (
		id BIGINT PRIMARY KEY AUTO_INCREMENT,
		order_no VARCHAR(64) NOT NULL UNIQUE,
		buyer_id BIGINT NOT NULL DEFAULT 1,
		product_id BIGINT NOT NULL,
		quantity INT NOT NULL,
		duration INT NOT NULL DEFAULT 1,
		unit_price BIGINT NOT NULL DEFAULT 0,
		total_amount BIGINT NOT NULL DEFAULT 0,
		platform_fee BIGINT NOT NULL DEFAULT 0,
		status ENUM('pending_payment','paid','provisioning','active','completed','cancelled','refunding','refunded','frozen') DEFAULT 'pending_payment',
		payment_expires_at TIMESTAMP NULL,
		lease_start_at TIMESTAMP NULL,
		lease_end_at TIMESTAMP NULL,
		compliance_agreed TINYINT DEFAULT 0,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
	)`)
	db.MustExec(`CREATE TABLE order_deliveries (
		id BIGINT PRIMARY KEY AUTO_INCREMENT,
		order_id BIGINT NOT NULL,
		credential_encrypted TEXT,
		ip_address VARCHAR(64),
		access_key VARCHAR(64),
		access_value_encrypted TEXT,
		access_status ENUM('none','generated','delivered','revoked') DEFAULT 'none',
		access_expires_at TIMESTAMP NULL,
		revoked_at TIMESTAMP NULL,
		delivered_at TIMESTAMP NULL,
		confirmed_at TIMESTAMP NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`)

	t.Cleanup(func() {
		db.Exec("DROP DATABASE IF EXISTS " + stockTestDB)
		db.Close()
	})

	svc := NewService(NewRepository(db), db)
	return db, svc
}

// seedOrder 建一个商品 + 一笔订单, 返回订单号与商品 ID。
func seedOrder(t *testing.T, db *sqlx.DB, stock, qty int, status string, payExpire, leaseEnd *time.Time) (string, int64) {
	t.Helper()
	res := db.MustExec("INSERT INTO products (stock, status) VALUES (?, 'active')", stock)
	pid, _ := res.LastInsertId()
	no := fmt.Sprintf("ORD%d%s", time.Now().UnixNano(), status)
	db.MustExec(`INSERT INTO orders (order_no, product_id, quantity, status, payment_expires_at, lease_end_at)
		VALUES (?,?,?,?,?,?)`, no, pid, qty, status, payExpire, leaseEnd)
	return no, pid
}

func stockOf(t *testing.T, db *sqlx.DB, pid int64) int {
	t.Helper()
	var s int
	if err := db.Get(&s, "SELECT stock FROM products WHERE id=?", pid); err != nil { t.Fatal(err) }
	return s
}

func statusOf(t *testing.T, db *sqlx.DB, no string) string {
	t.Helper()
	var s string
	if err := db.Get(&s, "SELECT status FROM orders WHERE order_no=?", no); err != nil { t.Fatal(err) }
	return s
}

// 超时未支付 -> 关单并归还余量。这是「库存被永久锁死」缺陷的核心回归测试。
func TestStock_CloseExpiredUnpaidOrders(t *testing.T) {
	db, svc := setupStockDB(t)
	past := time.Now().Add(-20 * time.Minute)
	no, pid := seedOrder(t, db, 5, 3, "pending_payment", &past, nil)

	n, err := svc.CloseExpiredUnpaidOrders()
	if err != nil { t.Fatalf("关单失败: %v", err) }
	if n != 1 { t.Fatalf("应关闭 1 笔, 实际 %d", n) }
	if got := statusOf(t, db, no); got != "cancelled" { t.Errorf("状态应为 cancelled, 实际 %s", got) }
	if got := stockOf(t, db, pid); got != 8 { t.Errorf("余量应归还为 8, 实际 %d", got) }
}

// 未到期的待支付订单不得被关闭。
func TestStock_UnexpiredOrderNotClosed(t *testing.T) {
	db, svc := setupStockDB(t)
	future := time.Now().Add(10 * time.Minute)
	no, pid := seedOrder(t, db, 5, 3, "pending_payment", &future, nil)

	if n, err := svc.CloseExpiredUnpaidOrders(); err != nil || n != 0 {
		t.Fatalf("不应关闭任何订单, n=%d err=%v", n, err)
	}
	if got := statusOf(t, db, no); got != "pending_payment" { t.Errorf("状态不应变化, 实际 %s", got) }
	if got := stockOf(t, db, pid); got != 5 { t.Errorf("余量不应变化, 实际 %d", got) }
}

// 重复执行不得重复归还 —— 幂等性。
func TestStock_ReleaseIsIdempotent(t *testing.T) {
	db, svc := setupStockDB(t)
	past := time.Now().Add(-20 * time.Minute)
	_, pid := seedOrder(t, db, 5, 3, "pending_payment", &past, nil)

	for i := 0; i < 3; i++ {
		if _, err := svc.CloseExpiredUnpaidOrders(); err != nil { t.Fatal(err) }
	}
	if got := stockOf(t, db, pid); got != 8 {
		t.Errorf("重复关单后余量应仍为 8(只归还一次), 实际 %d", got)
	}
}

// 并发释放同一笔订单, 余量只能被归还一次。
func TestStock_ConcurrentReleaseOnlyOnce(t *testing.T) {
	db, svc := setupStockDB(t)
	past := time.Now().Add(-20 * time.Minute)
	no, pid := seedOrder(t, db, 5, 3, "pending_payment", &past, nil)

	var wg sync.WaitGroup
	released := make([]bool, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ok, err := svc.releaseStock(no, []string{"pending_payment"}, "cancelled")
			if err == nil { released[idx] = ok }
		}(i)
	}
	wg.Wait()

	cnt := 0
	for _, r := range released { if r { cnt++ } }
	if cnt != 1 { t.Errorf("并发下应只有 1 次真正归还, 实际 %d", cnt) }
	if got := stockOf(t, db, pid); got != 8 { t.Errorf("余量应为 8, 实际 %d", got) }
}

// 退款完成 -> 归还余量。
func TestStock_RefundRestoresStock(t *testing.T) {
	db, svc := setupStockDB(t)
	no, pid := seedOrder(t, db, 2, 4, "refunding", nil, nil)

	if err := svc.CompleteRefund(no); err != nil { t.Fatalf("退款失败: %v", err) }
	if got := statusOf(t, db, no); got != "refunded" { t.Errorf("状态应为 refunded, 实际 %s", got) }
	if got := stockOf(t, db, pid); got != 6 { t.Errorf("余量应为 6, 实际 %d", got) }
}

// 租期到期 -> 置完成并归还余量 (REQ-A-043)。
func TestStock_CompleteExpiredLeases(t *testing.T) {
	db, svc := setupStockDB(t)
	past := time.Now().Add(-1 * time.Hour)
	no, pid := seedOrder(t, db, 0, 2, "active", nil, &past)

	n, err := svc.CompleteExpiredLeases()
	if err != nil { t.Fatalf("处理到期租约失败: %v", err) }
	if n != 1 { t.Fatalf("应完成 1 笔, 实际 %d", n) }
	if got := statusOf(t, db, no); got != "completed" { t.Errorf("状态应为 completed, 实际 %s", got) }
	if got := stockOf(t, db, pid); got != 2 { t.Errorf("余量应归还为 2, 实际 %d", got) }
}

// 买断订单 lease_end_at 为 NULL, 使用权永久, 绝不能被到期任务关掉。
func TestStock_PerpetualLeaseNeverExpires(t *testing.T) {
	db, svc := setupStockDB(t)
	no, pid := seedOrder(t, db, 1, 2, "active", nil, nil)

	if n, err := svc.CompleteExpiredLeases(); err != nil || n != 0 {
		t.Fatalf("买断订单不应被完成, n=%d err=%v", n, err)
	}
	if got := statusOf(t, db, no); got != "active" { t.Errorf("买断订单状态不应变化, 实际 %s", got) }
	if got := stockOf(t, db, pid); got != 1 { t.Errorf("余量不应变化, 实际 %d", got) }
}

// 售罄商品归还余量后应自动恢复在售。
func TestStock_SoldOutRecoversToActive(t *testing.T) {
	db, svc := setupStockDB(t)
	past := time.Now().Add(-20 * time.Minute)
	no, pid := seedOrder(t, db, 0, 2, "pending_payment", &past, nil)
	db.MustExec("UPDATE products SET status='sold_out' WHERE id=?", pid)

	if _, err := svc.releaseStock(no, []string{"pending_payment"}, "cancelled"); err != nil { t.Fatal(err) }

	var st string
	db.Get(&st, "SELECT status FROM products WHERE id=?", pid)
	if st != "active" { t.Errorf("售罄商品归还余量后应恢复 active, 实际 %s", st) }
}

// 被运营强制下架/风控冻结的商品, 不得因归还余量而自动重新上架。
func TestStock_OfflineProductNotResurrected(t *testing.T) {
	db, svc := setupStockDB(t)
	for _, blocked := range []string{"offline", "frozen"} {
		past := time.Now().Add(-20 * time.Minute)
		no, pid := seedOrder(t, db, 0, 2, "pending_payment", &past, nil)
		db.MustExec("UPDATE products SET status=? WHERE id=?", blocked, pid)

		if _, err := svc.releaseStock(no, []string{"pending_payment"}, "cancelled"); err != nil { t.Fatal(err) }

		var st string
		db.Get(&st, "SELECT status FROM products WHERE id=?", pid)
		if st != blocked {
			t.Errorf("%s 商品不应被自动上架, 实际变为 %s", blocked, st)
		}
		if got := stockOf(t, db, pid); got != 2 {
			t.Errorf("%s 商品的余量仍应归还, 实际 %d", blocked, got)
		}
	}
}

// 已是终态的订单不得再归还余量。
func TestStock_TerminalStatusNotReleased(t *testing.T) {
	db, svc := setupStockDB(t)
	for _, st := range []string{"cancelled", "refunded", "completed"} {
		_, pid := seedOrder(t, db, 5, 3, st, nil, nil)
		var no string
		db.Get(&no, "SELECT order_no FROM orders WHERE product_id=?", pid)

		ok, err := svc.releaseStock(no, stockHoldingStatuses, "cancelled")
		if err != nil { t.Fatal(err) }
		if ok { t.Errorf("终态 %s 不应发生归还", st) }
		if got := stockOf(t, db, pid); got != 5 {
			t.Errorf("终态 %s 的余量不应变化, 实际 %d", st, got)
		}
	}
}

var _ = sql.ErrNoRows
