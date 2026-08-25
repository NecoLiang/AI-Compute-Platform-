package blockchain

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

// 存证落库 → worker 上链/重试/死信/补推 → 验证重算 的集成测试 (T-057/T-058, REQ-H-021/H-030)。
// 与 compute 的库存测试同一模式: 默认跳过, 设置 TEST_MYSQL_DSN 后打真库:
//   TEST_MYSQL_DSN='root:pw@tcp(127.0.0.1:3306)/?parseTime=true&multiStatements=true' go test ./internal/blockchain/ -v
// 测试自建独立库 tokenfactory_chain_test, 结束时删除, 不触碰业务库。

const chainTestDB = "tokenfactory_chain_test"

// 与 migrations/001 + 007 保持一致的表结构。
const attestationDDL = `CREATE TABLE blockchain_attestations (
    id BIGINT AUTO_INCREMENT PRIMARY KEY,
    target_type VARCHAR(32) NOT NULL,
    target_id VARCHAR(64) NOT NULL,
    data_hash VARCHAR(128) NOT NULL,
    signers JSON,
    chain_tx_id VARCHAR(128),
    chain_status ENUM('pending','confirmed','failed') DEFAULT 'pending',
    attempts INT NOT NULL DEFAULT 0,
    last_error VARCHAR(512) NULL,
    created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
    confirmed_at TIMESTAMP NULL,
    INDEX idx_target (target_type, target_id),
    INDEX idx_chain_status (chain_status)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4`

func setupChainDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 TEST_MYSQL_DSN, 跳过存证集成测试")
	}
	root, err := sqlx.Connect("mysql", dsn)
	if err != nil {
		t.Fatalf("连接 MySQL 失败: %v", err)
	}
	defer root.Close()
	root.MustExec("DROP DATABASE IF EXISTS " + chainTestDB)
	root.MustExec("CREATE DATABASE " + chainTestDB + " CHARACTER SET utf8mb4")

	testDSN := strings.Replace(dsn, "/?", "/"+chainTestDB+"?", 1)
	db, err := sqlx.Connect("mysql", testDSN)
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db.MustExec(attestationDDL)
	t.Cleanup(func() {
		db.MustExec("DROP DATABASE IF EXISTS " + chainTestDB)
		db.Close()
	})
	return db
}

// fakeBSN 返回可控的 BSN 网关: fail 非零时返回 500, 否则按序发 txId。
func fakeBSN(t *testing.T, fail *atomic.Bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if fail.Load() {
			w.WriteHeader(500)
			w.Write([]byte("gateway down"))
			return
		}
		switch r.URL.Path {
		case "/api/v1/evidence":
			json.NewEncoder(w).Encode(map[string]any{"txId": "0xTX-ok"})
		case "/api/v1/evidence/query":
			json.NewEncoder(w).Encode(map[string]any{"exists": true, "txId": "0xTX-ok"})
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestService(t *testing.T, db *sqlx.DB, gatewayURL string) *Service {
	t.Helper()
	bsn := NewBSNClient(BSNConfig{GatewayURL: gatewayURL, APIKey: "k", ContractKey: "c", ExplorerURL: "https://scan.test/tx/"})
	svc, err := NewService(NewRepository(db), bsn, strings.Repeat("cd", 32))
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestAttest_ThenWorkerConfirms(t *testing.T) {
	db := setupChainDB(t)
	var fail atomic.Bool
	srv := fakeBSN(t, &fail)
	svc := newTestService(t, db, srv.URL)

	payload := ViolationPayload{TargetNo: "ORD1", Violation: "order_frozen", Conclusion: "risk_freeze"}
	if err := svc.Attest("violation", "ORD1", payload); err != nil {
		t.Fatalf("Attest: %v", err)
	}

	att, err := svc.repo.GetLatestAttestation("violation", "ORD1")
	if err != nil || att == nil {
		t.Fatalf("存证行未落库: %v", err)
	}
	if att.ChainStatus != "pending" || att.DataHash != ComputeHash(payload) {
		t.Fatalf("落库状态/hash 不符: %+v", att)
	}
	var signers []Signer
	json.Unmarshal([]byte(att.Signers), &signers)
	if len(signers) != 1 || signers[0].Role != "platform" || signers[0].Signature == "" {
		t.Fatalf("平台见证签名缺失: %s", att.Signers)
	}

	svc.processPendingOnce(context.Background())

	att, _ = svc.repo.GetLatestAttestation("violation", "ORD1")
	if att.ChainStatus != "confirmed" || att.ChainTxID == nil || *att.ChainTxID != "0xTX-ok" {
		t.Fatalf("上链后应 confirmed 并回写 TX ID (REQ-H-022): %+v", att)
	}
	if att.ConfirmedAt == nil {
		t.Fatal("confirmed_at 未回写")
	}
}

func TestWorker_RetryThenDeadLetterThenRequeue(t *testing.T) {
	db := setupChainDB(t)
	var fail atomic.Bool
	fail.Store(true)
	srv := fakeBSN(t, &fail)
	svc := newTestService(t, db, srv.URL)

	if err := svc.Attest("order", "ORD2", OrderPayload{OrderNo: "ORD2"}); err != nil {
		t.Fatalf("Attest: %v", err)
	}

	// 失败 workerMaxAttempts 次后进死信, 且不再被捞起。
	for i := 0; i < workerMaxAttempts; i++ {
		svc.processPendingOnce(context.Background())
	}
	att, _ := svc.repo.GetLatestAttestation("order", "ORD2")
	if att.ChainStatus != "failed" || att.Attempts != workerMaxAttempts {
		t.Fatalf("重试耗尽应进死信: status=%s attempts=%d", att.ChainStatus, att.Attempts)
	}
	if att.LastError == nil || *att.LastError == "" {
		t.Fatal("死信应记录 last_error")
	}
	svc.processPendingOnce(context.Background())
	if att, _ = svc.repo.GetLatestAttestation("order", "ORD2"); att.Attempts != workerMaxAttempts {
		t.Fatal("死信不应再被 worker 捞起")
	}

	// 故障恢复 → 补推 (REQ-H-021)。
	fail.Store(false)
	n, err := svc.RequeueFailed()
	if err != nil || n != 1 {
		t.Fatalf("补推重置失败: n=%d err=%v", n, err)
	}
	svc.processPendingOnce(context.Background())
	if att, _ = svc.repo.GetLatestAttestation("order", "ORD2"); att.ChainStatus != "confirmed" {
		t.Fatalf("补推后应上链成功: %+v", att)
	}
}

func TestVerify_RecomputeAndTamperDetection(t *testing.T) {
	db := setupChainDB(t)
	var fail atomic.Bool
	srv := fakeBSN(t, &fail)
	svc := newTestService(t, db, srv.URL)

	payload := OrderPayload{OrderNo: "ORD3", BuyerIDHash: HashID(1), SupplierIDHash: HashID(2), Spec: "s", TotalAmountFen: 100, PlacedAt: "2026-08-25T04:00:00Z"}
	source := payload // 模拟业务库当前数据
	svc.RegisterSource("order", func(id string) (any, error) { return source, nil })

	if err := svc.Attest("order", "ORD3", payload); err != nil {
		t.Fatalf("Attest: %v", err)
	}

	// 尚未上链: 如实返回 false
	res, err := svc.Verify(context.Background(), "order", "ORD3")
	if err != nil || res.Verified {
		t.Fatalf("未上链不得声称已验证: %+v err=%v", res, err)
	}

	svc.processPendingOnce(context.Background())
	res, err = svc.Verify(context.Background(), "order", "ORD3")
	if err != nil || !res.Verified {
		t.Fatalf("上链后验证应通过: %+v err=%v", res, err)
	}
	if res.DBHashMatch == nil || !*res.DBHashMatch {
		t.Fatalf("重算 hash 应一致: %+v", res)
	}
	if res.VerifyURL == "" {
		t.Error("应返回区块链浏览器链接")
	}

	// 业务数据被改动 → 重算不一致 → 判 false (REQ-H-030 的意义所在)
	source.TotalAmountFen = 999
	res, _ = svc.Verify(context.Background(), "order", "ORD3")
	if res.Verified || res.DBHashMatch == nil || *res.DBHashMatch {
		t.Fatalf("数据被改动应判不通过: %+v", res)
	}

	// 无存证记录
	res, _ = svc.Verify(context.Background(), "order", "NOPE")
	if res.Verified {
		t.Fatal("无存证记录应返回 false")
	}
}
