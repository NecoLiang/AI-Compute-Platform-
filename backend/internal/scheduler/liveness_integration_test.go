package scheduler

// 探活与健康联动的集成测试。判定逻辑全在 SQL 里(条件 UPDATE/聚合), 必须打真库。
// 默认跳过, 与 compute/blockchain 的集成测试同一模式:
//   TEST_MYSQL_DSN='root:pw@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true' go test ./internal/scheduler/ -v

import (
	"os"
	"strings"
	"testing"

	"tokenfactory/pkg/errcode"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

const schedTestDB = "tokenfactory_sched_test"

func setupSchedDB(t *testing.T) (*sqlx.DB, *Service) {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 TEST_MYSQL_DSN, 跳过探活集成测试")
	}
	root, err := sqlx.Connect("mysql", dsn)
	if err != nil {
		t.Fatalf("连接 MySQL 失败: %v", err)
	}
	defer root.Close()
	root.MustExec("DROP DATABASE IF EXISTS " + schedTestDB)
	root.MustExec("CREATE DATABASE " + schedTestDB + " CHARACTER SET utf8mb4")

	db, err := sqlx.Connect("mysql", strings.Replace(dsn, "/?", "/"+schedTestDB+"?", 1))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	// 与 migrations/008 一致的最小表结构
	db.MustExec(`CREATE TABLE products (
		id BIGINT PRIMARY KEY AUTO_INCREMENT,
		supplier_id BIGINT NOT NULL,
        product_type VARCHAR(32) NOT NULL DEFAULT 'card_rental',
        card_count INT NULL DEFAULT 0, machine_count INT NULL,
		fulfillment_mode ENUM('manual','managed') NOT NULL DEFAULT 'manual',
		health ENUM('unknown','healthy','degraded','offline') NOT NULL DEFAULT 'unknown'
	)`)
	db.MustExec(`CREATE TABLE supplier_nodes (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		supplier_id BIGINT NOT NULL, product_id BIGINT NOT NULL,
		node_name VARCHAR(64) NOT NULL, node_key_hash CHAR(64) NULL,
		status ENUM('online','degraded','offline') NOT NULL DEFAULT 'offline',
		total_cards INT NOT NULL DEFAULT 0, available_cards INT NOT NULL DEFAULT 0,
		gpu_util_pct TINYINT UNSIGNED NULL, vram_util_pct TINYINT UNSIGNED NULL,
		last_heartbeat_at TIMESTAMP NULL,
		mgmt_mode ENUM('push','ssh') NOT NULL DEFAULT 'push',
		verify_status ENUM('unverified','verifying','verified','failed','key_mismatch','needs_reimage','suspended') NOT NULL DEFAULT 'unverified',
		schedulable TINYINT NOT NULL DEFAULT 1,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		UNIQUE KEY uk_product_node (product_id, node_name)
	)`)
	db.MustExec(`CREATE TABLE node_heartbeats (
		id BIGINT AUTO_INCREMENT PRIMARY KEY, node_id BIGINT NOT NULL,
		available_cards INT NOT NULL, gpu_util_pct TINYINT UNSIGNED NULL, vram_util_pct TINYINT UNSIGNED NULL,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, INDEX idx_node_time (node_id, created_at)
	)`)
	db.MustExec(`CREATE TABLE orders (
		id BIGINT PRIMARY KEY AUTO_INCREMENT, order_no VARCHAR(64) NOT NULL UNIQUE,
		product_id BIGINT NOT NULL, quantity INT NOT NULL, status VARCHAR(32) NOT NULL
	)`)
	renewalMigration, err := os.ReadFile("../../migrations/021_order_renewals.up.sql")
	if err != nil {
		t.Fatal(err)
	}
	db.MustExec(string(renewalMigration))

	t.Cleanup(func() {
		db.MustExec("DROP DATABASE IF EXISTS " + schedTestDB)
		db.Close()
	})
	return db, NewService(NewRepository(db))
}

func productHealth(t *testing.T, db *sqlx.DB, id int64) string {
	t.Helper()
	var h string
	if err := db.Get(&h, "SELECT health FROM products WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	return h
}

func TestLiveness_FullLifecycle(t *testing.T) {
	db, svc := setupSchedDB(t)
	db.MustExec("INSERT INTO products (id, supplier_id) VALUES (1, 10)")

	// 注册: 归属校验 + key 只给一次
	node, key, err := svc.RegisterNode(10, 1, "gpu-node-1", 8)
	if err != nil {
		t.Fatalf("注册: %v", err)
	}
	if !strings.HasPrefix(key, "nk-") {
		t.Fatalf("node_key 格式: %s", key)
	}
	if _, _, err := svc.RegisterNode(99, 1, "x", 8); err == nil {
		t.Fatal("非归属供应方注册应被拒")
	}

	// 错误密钥心跳必须拒绝
	if err := svc.Heartbeat(node.ID, "nk-wrong", 8, nil, nil); err == nil {
		t.Fatal("错误 node_key 应被拒")
	}
	// 上报超过总卡数应被拒
	if err := svc.Heartbeat(node.ID, key, 9, nil, nil); err == nil {
		t.Fatal("available>total 应被拒")
	}

	// 正常心跳 → online, 商品 healthy
	if err := svc.Heartbeat(node.ID, key, 8, intp(30), intp(40)); err != nil {
		t.Fatalf("心跳: %v", err)
	}
	n, _ := svc.repo.GetNode(node.ID)
	if n.Status != "online" || n.AvailableCards != 8 {
		t.Fatalf("心跳后应 online: %+v", n)
	}
	if h := productHealth(t, db, 1); h != "healthy" {
		t.Fatalf("商品应 healthy, got %s", h)
	}

	// 无余量心跳 → degraded
	if err := svc.Heartbeat(node.ID, key, 0, intp(99), nil); err != nil {
		t.Fatal(err)
	}
	n, _ = svc.repo.GetNode(node.ID)
	if n.Status != "degraded" {
		t.Fatalf("无余量应 degraded: %+v", n)
	}
	if h := productHealth(t, db, 1); h != "degraded" {
		t.Fatalf("商品应 degraded, got %s", h)
	}

	// 心跳过期 → sweep 判离线, 商品 offline
	db.MustExec("UPDATE supplier_nodes SET last_heartbeat_at = NOW() - INTERVAL 300 SECOND WHERE id=?", node.ID)
	svc.sweepOnce()
	n, _ = svc.repo.GetNode(node.ID)
	if n.Status != "offline" {
		t.Fatalf("超时应 offline: %+v", n)
	}
	if h := productHealth(t, db, 1); h != "offline" {
		t.Fatalf("商品应 offline, got %s", h)
	}

	// 心跳恢复 → 立即解除
	if err := svc.Heartbeat(node.ID, key, 4, intp(10), nil); err != nil {
		t.Fatal(err)
	}
	if h := productHealth(t, db, 1); h != "healthy" {
		t.Fatalf("恢复后应 healthy, got %s", h)
	}
}

func TestAdvise_RanksNodes(t *testing.T) {
	db, svc := setupSchedDB(t)
	db.MustExec("INSERT INTO products (id, supplier_id) VALUES (1, 10)")
	db.MustExec("INSERT INTO orders (order_no, product_id, quantity, status) VALUES ('ORD-1', 1, 4, 'paid')")

	nA, keyA, _ := svc.RegisterNode(10, 1, "node-a", 16)
	nB, keyB, _ := svc.RegisterNode(10, 1, "node-b", 8)
	nC, _, _ := svc.RegisterNode(10, 1, "node-c", 8) // 不发心跳 → 保持 offline
	_ = nC
	svc.Heartbeat(nA.ID, keyA, 16, intp(80), nil) // 大而忙
	svc.Heartbeat(nB.ID, keyB, 4, intp(10), nil)  // 恰好容纳且空闲

	adv, err := svc.Advise("ORD-1", 10)
	if err != nil {
		t.Fatalf("Advise: %v", err)
	}
	if len(adv.Nodes) != 3 {
		t.Fatalf("应包含全部节点: %+v", adv.Nodes)
	}
	if adv.Nodes[0].NodeName != "node-b" || adv.Nodes[0].Verdict != "recommended" {
		t.Fatalf("best-fit 空闲节点应被推荐: %+v", adv.Nodes)
	}
	if !strings.Contains(adv.Summary, "node-b") {
		t.Errorf("summary 应指向推荐节点: %s", adv.Summary)
	}
	// 供应方越权
	if _, err := svc.Advise("ORD-1", 99); err == nil {
		t.Fatal("非归属供应方应被拒")
	}
	// 运营视角(0)放行
	if _, err := svc.Advise("ORD-1", 0); err != nil {
		t.Fatalf("运营视角: %v", err)
	}
}

// docs/25 §25.4.4: 托管节点(mgmt_mode=ssh)不推心跳, 在线状态只由 fleet 判定;
// scheduler 的心跳超时清扫不得把它们打成离线, push 节点行为不变。
func TestLiveness_SweepSkipsSSHNodes(t *testing.T) {
	db, svc := setupSchedDB(t)
	db.MustExec("INSERT INTO products (id, supplier_id) VALUES (1, 10), (2, 10)")
	db.MustExec(`INSERT INTO supplier_nodes (supplier_id, product_id, node_name, node_key_hash, status, total_cards, last_heartbeat_at, mgmt_mode)
		VALUES (10, 1, 'ssh-node', NULL, 'online', 8, NOW() - INTERVAL 600 SECOND, 'ssh'),
		       (10, 2, 'push-node', REPEAT('a',64), 'online', 8, NOW() - INTERVAL 600 SECOND, 'push')`)
	svc.sweepOnce()
	var ssh, push string
	if err := db.Get(&ssh, "SELECT status FROM supplier_nodes WHERE node_name='ssh-node'"); err != nil {
		t.Fatal(err)
	}
	if err := db.Get(&push, "SELECT status FROM supplier_nodes WHERE node_name='push-node'"); err != nil {
		t.Fatal(err)
	}
	if ssh != "online" {
		t.Fatalf("ssh 节点不应被心跳清扫置离线, got %s", ssh)
	}
	if push != "offline" {
		t.Fatalf("push 节点心跳超时应离线, got %s", push)
	}
	if h := productHealth(t, db, 1); h != "unknown" {
		t.Fatalf("ssh 节点商品不应被清扫重算, got %s", h)
	}
	// ssh 节点 node_key_hash 为 NULL: 读取不报错, 且任何 node_key 都不能冒充它发心跳。
	var id int64
	db.Get(&id, "SELECT id FROM supplier_nodes WHERE node_name='ssh-node'")
	n, err := svc.repo.GetNode(id)
	if err != nil || n == nil || n.MgmtMode != "ssh" || n.NodeKeyHash != "" {
		t.Fatalf("读取 ssh 节点: %+v %v", n, err)
	}
	if err := svc.Heartbeat(id, "nk-anything", 1, nil, nil); err == nil {
		t.Fatal("ssh 节点不接受推送心跳")
	}
	// fleet 未注入时托管节点不可注册/删除, 且错误码明确。
	if _, err := svc.RegisterManagedNode(10, ManagedNodeReq{ProductID: 1, NodeName: "x", TotalCards: 1}); errcode.CodeOf(err, 0) != errcode.ParamInvalid {
		t.Fatalf("fleet 未启用注册托管节点应 40001: %v", err)
	}
	if err := svc.DeleteNode(id, 10); errcode.CodeOf(err, 0) != errcode.InternalError {
		t.Fatalf("fleet 未启用删除托管节点应 50000: %v", err)
	}
	// push 节点不能挂到托管商品下
	db.MustExec("UPDATE products SET fulfillment_mode='managed' WHERE id=1")
	if _, _, err := svc.RegisterNode(10, 1, "push-on-managed", 8); err == nil {
		t.Fatal("托管商品不应接受 push 节点")
	}
}
