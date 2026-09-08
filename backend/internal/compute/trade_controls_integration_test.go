package compute_test

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
	"tokenfactory/internal/admin"
	"tokenfactory/internal/compute"
	"tokenfactory/internal/payment"
	"tokenfactory/internal/user"
	"tokenfactory/pkg/middleware"
)

func TestTradeConfigControlsNewOrdersAndPreservesFees(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	supplier, buyer, operator := tradeRouter(db, 101), tradeRouter(db, 102), tradeRouter(db, 104)
	admin.NewHandler(admin.NewService(admin.NewRepository(db))).RegisterRoutes(operator.Group("/api/v1", userID(104)))
	created := tradeRequest(t, supplier, "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	tradeRequest(t, operator, "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	input := map[string]any{"product_id": id, "quantity": 2, "duration": 3, "compliance_agreed": true, "compliance_version": "2026-09-06.1"}
	first := tradeRequest(t, buyer, "POST", "/api/v1/orders", input, 0).Data.(map[string]any)
	tradeRequest(t, operator, "PUT", "/api/v1/admin/config", map[string]string{"key": "fee_rate", "value": "650"}, 0)
	second := tradeRequest(t, buyer, "POST", "/api/v1/orders", input, 0).Data.(map[string]any)
	if second["total_amount"] != float64(13800) || second["platform_fee"] != float64(897) {
		t.Fatalf("new fee must be 897 fen within 13800 total: %v", second)
	}
	old := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+first["order_no"].(string), nil, 0).Data.(map[string]any)["order"].(map[string]any)
	if old["platform_fee"] != float64(690) {
		t.Fatalf("old fee changed: %v", old)
	}
	tradeRequest(t, operator, "PUT", "/api/v1/admin/config", map[string]string{"key": "trading_enabled", "value": "false"}, 0)
	tradeRequest(t, buyer, "POST", "/api/v1/orders", input, 40900)
	// Pausing new trades must not stop payment of an existing order or reprice its split.
	gateway := &testPaymentGateway{}
	payments := payment.NewService(payment.NewRepository(db), db, gateway)
	for i, placed := range []map[string]any{first, second} {
		no := placed["order_no"].(string)
		pay, err := payments.Pay(102, payment.PayReq{OrderNo: no, Channel: "wechat"})
		if err != nil {
			t.Fatal(err)
		}
		if err := payments.HandleCallback(payment.CallbackReq{OrderNo: no, TxID: pay.TxID, Amount: 13800, Status: "success"}); err != nil {
			t.Fatal(err)
		}
		settlements, err := payments.GetOrderSettlements(101, no)
		if err != nil || len(settlements) != 2 {
			t.Fatalf("settlements=%v err=%v", settlements, err)
		}
		wantFee := int64(690)
		if i == 1 {
			wantFee = 897
		}
		for _, item := range settlements {
			if item.PayeeType == "platform" && item.Amount != wantFee {
				t.Fatalf("repriced platform fee: %v", item)
			}
			if item.PayeeType == "supplier" && item.Amount != 13800-wantFee {
				t.Fatalf("repriced supplier split: %v", item)
			}
		}
	}

	product := tradeRequest(t, buyer, "GET", fmt.Sprintf("/api/v1/products/%d", id), nil, 0).Data.(map[string]any)["product"].(map[string]any)
	if product["stock"] != float64(4) {
		t.Fatalf("rejected order changed stock: %v", product)
	}
	policy := tradeRequest(t, buyer, "GET", "/api/v1/trading-config", nil, 0).Data.(map[string]any)
	if policy["trading_enabled"] != false || policy["fee_rate"] != float64(650) {
		t.Fatalf("policy=%v", policy)
	}
	tradeRequest(t, operator, "PUT", "/api/v1/admin/config", map[string]string{"key": "trading_enabled", "value": "true"}, 0)
	tradeRequest(t, buyer, "POST", "/api/v1/orders", input, 0)
	db.MustExec("DELETE FROM system_config WHERE config_key='fee_rate'")
	tradeRequest(t, buyer, "POST", "/api/v1/orders", input, 50000)
	w := httptest.NewRecorder()
	buyer.ServeHTTP(w, httptest.NewRequest("GET", "/api/v1/trading-config", nil))
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing config must report HTTP 503: %d", w.Code)
	}
}

func TestTradeProductHealthMatchesPublicContract(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	supplier, buyer, operator := tradeRouter(db, 101), tradeRouter(db, 102), tradeRouter(db, 104)
	created := tradeRequest(t, supplier, "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	tradeRequest(t, operator, "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	for _, health := range []string{"unknown", "healthy", "degraded", "offline"} {
		db.MustExec("UPDATE products SET health=? WHERE id=?", health, id)
		detail := tradeRequest(t, buyer, "GET", fmt.Sprintf("/api/v1/products/%d", id), nil, 0).Data.(map[string]any)["product"].(map[string]any)
		list := tradeRequest(t, buyer, "GET", "/api/v1/products", nil, 0).Data.(map[string]any)["list"].([]any)
		if detail["health"] != health || list[0].(map[string]any)["health"] != health {
			t.Fatalf("health omitted: detail=%v list=%v", detail, list)
		}
	}
	tradeRequest(t, buyer, "POST", "/api/v1/orders", map[string]any{"product_id": id, "quantity": 1, "duration": 1, "compliance_agreed": true, "compliance_version": "2026-09-06.1"}, 40001)
}

func TestTradeProfileIsCurrentAndUpdatesAreNotPretended(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	r := tradeRouter(db, 102)
	user.NewHandler(user.NewService(user.NewRepository(db))).RegisterRoutes(r.Group("/api/v1", userID(102)))
	profile := tradeRequest(t, r, "GET", "/api/v1/user/profile", nil, 0).Data.(map[string]any)
	if profile["phone"] != "18800001102" {
		t.Fatalf("profile must read current account: %v", profile)
	}
	w := httptest.NewRecorder()
	req := httptest.NewRequest("PUT", "/api/v1/user/profile", nil)
	r.ServeHTTP(w, req)
	if w.Code != http.StatusNotImplemented {
		t.Fatalf("profile update must report HTTP 501: %d", w.Code)
	}
	var update envelope
	if err := json.Unmarshal(w.Body.Bytes(), &update); err != nil || update.Code != 50000 {
		t.Fatalf("profile update=%s", w.Body.String())
	}
	profile = tradeRequest(t, r, "GET", "/api/v1/user/profile", nil, 0).Data.(map[string]any)
	if profile["phone"] != "18800001102" {
		t.Fatalf("unsupported update changed account: %v", profile)
	}
}

func TestTradeRenewalDoesNotCreateIncompleteOrders(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	supplier, buyer, operator := tradeRouter(db, 101), tradeRouter(db, 102), tradeRouter(db, 104)
	created := tradeRequest(t, supplier, "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	tradeRequest(t, operator, "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	placed := tradeRequest(t, buyer, "POST", "/api/v1/orders", map[string]any{"product_id": id, "quantity": 2, "duration": 3, "compliance_agreed": true, "compliance_version": "2026-09-06.1"}, 0)
	no := placed.Data.(map[string]any)["order_no"].(string)
	db.MustExec("UPDATE orders SET status='active' WHERE order_no=?", no)
	tradeRequest(t, buyer, "POST", "/api/v1/orders/"+no+"/renew", map[string]int{"duration": 3}, 40900)
	detail := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+no, nil, 0).Data.(map[string]any)
	if detail["actions"].(map[string]any)["can_renew"] != false {
		t.Fatal("incomplete renewal must not be offered")
	}
	list := tradeRequest(t, buyer, "GET", "/api/v1/orders", nil, 0).Data.(map[string]any)
	if list["total"] != float64(1) {
		t.Fatalf("renewal created another order: %v", list)
	}
}

func TestTradeRiskFreezeChangesTheTargetAndIsIdempotent(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	supplier, buyer, operator := tradeRouter(db, 101), tradeRouter(db, 102), tradeRouter(db, 104)
	admin.NewHandler(admin.NewService(admin.NewRepository(db))).RegisterRoutes(operator.Group("/api/v1", userID(104)))
	created := tradeRequest(t, supplier, "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	tradeRequest(t, operator, "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	placed := tradeRequest(t, buyer, "POST", "/api/v1/orders", map[string]any{"product_id": id, "quantity": 2, "duration": 3, "compliance_agreed": true, "compliance_version": "2026-09-06.1"}, 0)
	no := placed.Data.(map[string]any)["order_no"].(string)
	detail := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+no, nil, 0).Data.(map[string]any)
	orders := tradeRequest(t, operator, "GET", "/api/v1/admin/orders", nil, 0).Data.(map[string]any)["list"].([]any)
	oid := int64(orders[0].(map[string]any)["id"].(float64))
	db.MustExec("INSERT INTO risk_alerts (id,level,alert_type,target_type,target_id) VALUES (1,'high','manual','order',?),(2,'high','manual','product',?),(3,'high','manual','order',999999)", oid, id)
	db.MustExec("UPDATE orders SET status='paid' WHERE id=?", oid)
	accessRouter := gin.New()
	accessHandler := compute.NewHandler(compute.NewService(compute.NewRepository(db), db, "1111111111111111111111111111111111111111111111111111111111111111"))
	accessHandler.RegisterSupplierRoutes(accessRouter.Group("/supplier", userID(101)))
	accessHandler.RegisterBuyerRoutes(accessRouter.Group("/buyer", userID(102)))
	tradeRequest(t, accessRouter, "POST", "/supplier/orders/"+no+"/deliver", map[string]any{"ip_address": "10.0.0.1", "ssh_port": 22, "username": "test", "password": "disposable-fixture"}, 0)
	credentialPath := "/buyer/orders/" + no + "/access-credential"
	tradeRequest(t, accessRouter, "POST", credentialPath+"/reveal", nil, 0)
	db.MustExec("CREATE TRIGGER reject_audit BEFORE INSERT ON audit_logs FOR EACH ROW SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT='test audit failure'")
	tradeRequest(t, operator, "POST", "/api/v1/admin/risk/alerts/1/freeze", nil, 50000)
	db.MustExec("DROP TRIGGER reject_audit")
	failed := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+no, nil, 0).Data.(map[string]any)
	if failed["order"].(map[string]any)["status"] != "provisioning" {
		t.Fatal("audit failure did not roll back order freeze")
	}
	tradeRequest(t, accessRouter, "POST", credentialPath+"/reveal", nil, 0)

	for i := 0; i < 2; i++ {
		tradeRequest(t, operator, "POST", "/api/v1/admin/risk/alerts/1/freeze", nil, 0)
	}
	detail = tradeRequest(t, buyer, "GET", "/api/v1/orders/"+no, nil, 0).Data.(map[string]any)
	credential := tradeRequest(t, accessRouter, "GET", credentialPath, nil, 0).Data.(map[string]any)
	if credential["access_status"] != "revoked" {
		t.Fatalf("freeze did not revoke credentials: %v", credential)
	}
	tradeRequest(t, accessRouter, "POST", credentialPath+"/reveal", nil, 40001)
	if detail["order"].(map[string]any)["status"] != "frozen" {
		t.Fatalf("alert was resolved without freezing order: %v", detail)
	}
	tradeRequest(t, operator, "POST", "/api/v1/admin/risk/alerts/2/freeze", nil, 40001)
	tradeRequest(t, operator, "POST", "/api/v1/admin/risk/alerts/3/freeze", nil, 40400)
	tradeRequest(t, operator, "POST", "/api/v1/admin/risk/alerts/999/freeze", nil, 40400)
	tradeRequest(t, operator, "POST", "/api/v1/admin/risk/alerts/1/dismiss", nil, 40900)
	alerts := tradeRequest(t, operator, "GET", "/api/v1/admin/risk/alerts", nil, 0).Data.(map[string]any)["list"].([]any)
	for _, raw := range alerts {
		alert := raw.(map[string]any)
		want := "pending"
		if alert["id"] == float64(1) {
			want = "resolved"
		}
		if alert["status"] != want {
			t.Fatalf("alert state=%v", alert)
		}
	}
	audits := tradeRequest(t, operator, "GET", "/api/v1/admin/audit-logs", nil, 0).Data.(map[string]any)["list"].([]any)
	count := 0
	for _, raw := range audits {
		if raw.(map[string]any)["action"] == "freeze_alert" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("freeze audit count=%d", count)
	}
	for i := 0; i < 2; i++ {
		tradeRequest(t, operator, "PATCH", fmt.Sprintf("/api/v1/admin/orders/%d/status", oid), map[string]string{"status": "cancelled"}, 0)
	}
	product := tradeRequest(t, buyer, "GET", fmt.Sprintf("/api/v1/products/%d", id), nil, 0).Data.(map[string]any)["product"].(map[string]any)
	if product["stock"] != float64(8) {
		t.Fatalf("closing frozen order must release its reservation once: %v", product)
	}

}

func TestTradeRiskAccountFreezeRevokesExistingSessionsAndCanRetry(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR required")
	}
	db := setupTradeDB(t)
	seedTradeUsers(db)
	rdb := redis.NewClient(&redis.Options{Addr: addr, DB: 14})
	ctx := context.Background()
	rdb.Del(ctx, "auth:frozen:102")
	t.Cleanup(func() { rdb.Del(ctx, "auth:frozen:102"); rdb.Close() })
	operator := tradeRouter(db, 104)
	service := admin.NewService(admin.NewRepository(db))
	admin.NewHandler(service).RegisterRoutes(operator.Group("/api/v1", userID(104)))
	db.MustExec("INSERT INTO risk_alerts (id,level,alert_type,target_type,target_id) VALUES (1,'high','manual','user',102),(2,'high','manual','user',104)")
	secret := "integration-test-only"
	claims := middleware.Claims{UserID: 102, Phone: "18800001102", Roles: []string{"buyer"}, RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour))}}
	token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}
	protected := gin.New()
	user.NewHandler(user.NewService(user.NewRepository(db))).RegisterRoutes(protected.Group("/api/v1", middleware.AuthRequired(secret, rdb, user.NewRepository(db))))
	checkAccess := func(want int) {
		t.Helper()
		req := httptest.NewRequest("GET", "/api/v1/user/profile", nil)
		req.Header.Set("Authorization", "Bearer "+token)
		w := httptest.NewRecorder()
		protected.ServeHTTP(w, req)
		if w.Code != want {
			t.Fatalf("session HTTP=%d body=%s", w.Code, w.Body.String())
		}
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
	}
	checkAccess(http.StatusOK)
	// Missing Redis must not be reported as a completed freeze; DB status still protects access.
	tradeRequest(t, operator, "POST", "/api/v1/admin/risk/alerts/1/freeze", nil, 50000)
	checkAccess(http.StatusUnauthorized)
	service.SetSessionRevoker(rdb)
	for i := 0; i < 2; i++ {
		tradeRequest(t, operator, "POST", "/api/v1/admin/risk/alerts/1/freeze", nil, 0)
	}
	checkAccess(http.StatusUnauthorized)
	tradeRequest(t, operator, "POST", "/api/v1/admin/risk/alerts/2/freeze", nil, 40001)
}
