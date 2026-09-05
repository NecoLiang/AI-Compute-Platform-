package compute_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tokenfactory/internal/compute"
	"tokenfactory/internal/intermediary"
	"tokenfactory/internal/payment"

	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

// Run against a disposable MySQL instance with TEST_MYSQL_DSN. Every run uses
// a fresh database and the real migrations, without changing application data.
func setupTradeDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN is required for trade flow integration tests")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = ""
	root, err := sqlx.Connect("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("omnis_trade_test_%d", time.Now().UnixNano())
	root.MustExec("CREATE DATABASE " + name + " CHARACTER SET utf8mb4")
	cfg.DBName, cfg.ParseTime, cfg.MultiStatements = name, true, true
	db, err := sqlx.Connect("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); root.Exec("DROP DATABASE " + name); root.Close() })
	paths, err := filepath.Glob("../../migrations/*.up.sql")
	if err != nil || len(paths) == 0 {
		t.Fatalf("migrations: %v", err)
	}
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(data)); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
	}
	return db
}

func tradeRouter(db *sqlx.DB, id int64) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := compute.NewHandler(compute.NewService(compute.NewRepository(db), db))
	g := r.Group("/api/v1", userID(id))
	h.RegisterSupplierRoutes(g)
	h.RegisterBuyerRoutes(g)
	h.RegisterPublicRoutes(g)
	h.RegisterAdminRoutes(g)
	intermediary.NewHandler(intermediary.NewService(intermediary.NewRepository(db))).RegisterAdminRoutes(g)
	intermediary.NewHandler(intermediary.NewService(intermediary.NewRepository(db))).RegisterPublicRoutes(g)
	payment.NewHandler(payment.NewService(payment.NewRepository(db), db)).RegisterBuyerRoutes(g)
	return r
}

func TestTradePaymentRejectsWrongOwnerBeforeContactingProvider(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	created := tradeRequest(t, tradeRouter(db, 101), "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	tradeRequest(t, tradeRouter(db, 104), "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	placed := tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/orders", map[string]any{"product_id": id, "quantity": 1, "duration": 1, "compliance_agreed": true}, 0)
	no := placed.Data.(map[string]any)["order_no"].(string)
	pay := map[string]string{"order_no": no, "channel": "wechat"}
	tradeRequest(t, tradeRouter(db, 101), "POST", "/api/v1/payment/pay", pay, 40300)
	tradeRequest(t, tradeRouter(db, 101), "GET", "/api/v1/payment/status/"+no, nil, 40300)
	pay["channel"] = "unknown"
	tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/payment/pay", pay, 40001)
	pay["channel"] = "wechat"
	tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/payment/pay", pay, 50000)
	db.MustExec("UPDATE orders SET payment_expires_at=DATE_SUB(NOW(), INTERVAL 1 MINUTE) WHERE order_no=?", no)
	tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/payment/pay", pay, 40900)
}

// Only the external provider is a test double; handlers, transactions,
// migrations, stock and delivery use the real implementation and MySQL.
type testPaymentGateway struct {
	amount int64
	calls  int
	splits int
}

func (g *testPaymentGateway) CreatePayment(no string, amount int64, channel string) (string, string, error) {
	g.amount, g.calls = amount, g.calls+1
	return "https://cashier.example.test/" + no, "TX" + no, nil
}
func (g *testPaymentGateway) VerifyCallback(req payment.CallbackReq) error {
	if req.TxID != "TX"+req.OrderNo {
		return fmt.Errorf("invalid test signature")
	}
	return nil
}
func (g *testPaymentGateway) CreateSplit(no string, items []payment.SplitItem) error {
	g.splits++
	return nil
}

func TestTradePaymentCallbackUnlocksDeliveryAndIsIdempotent(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	created := tradeRequest(t, tradeRouter(db, 101), "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	tradeRequest(t, tradeRouter(db, 104), "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	placed := tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/orders", map[string]any{"product_id": id, "quantity": 2, "duration": 3, "compliance_agreed": true}, 0)
	no := placed.Data.(map[string]any)["order_no"].(string)
	gateway := &testPaymentGateway{}
	service := payment.NewService(payment.NewRepository(db), db, gateway)
	checkout, err := service.Pay(102, payment.PayReq{OrderNo: no, Channel: "wechat"})
	if err != nil || gateway.amount != 13800 {
		t.Fatalf("checkout: %+v amount=%d err=%v", checkout, gateway.amount, err)
	}
	repeated, err := service.Pay(102, payment.PayReq{OrderNo: no, Channel: "wechat"})
	if err != nil || repeated == nil || repeated.TxID != checkout.TxID || gateway.calls != 1 {
		t.Fatalf("repeated pay created another payment: %+v calls=%d err=%v", repeated, gateway.calls, err)
	}
	req := payment.CallbackReq{TxID: checkout.TxID, OrderNo: no, Status: "fail", Amount: 13800}
	if err := service.HandleCallback(req); err == nil {
		t.Fatal("failed payment must not be accepted as paid")
	}
	req.Status, req.Amount = "success", 1
	if err := service.HandleCallback(req); err == nil {
		t.Fatal("amount mismatch must be rejected")
	}
	req.Amount = 13800
	if err := service.HandleCallback(req); err != nil {
		t.Fatal(err)
	}
	if err := service.HandleCallback(req); err != nil {
		t.Fatal(err)
	}
	computeService := compute.NewService(compute.NewRepository(db), db, strings.Repeat("12", 32))
	order, err := computeService.GetOrder(no)
	if err != nil || order.Status != "paid" {
		t.Fatalf("business order not paid: %+v %v", order, err)
	}
	if _, err := computeService.DeliverWithAccess(101, no, compute.DeliverInfo{IpAddress: "192.0.2.10"}, false); err != nil {
		t.Fatal(err)
	}
	if err := computeService.ConfirmDelivery(102, no); err != nil {
		t.Fatal(err)
	}
	order, err = computeService.GetOrder(no)
	if err != nil || order.Status != "active" {
		t.Fatalf("delivery not active: %+v %v", order, err)
	}
	if err := service.HandleCallback(req); err != nil {
		t.Fatal(err)
	}
	order, _ = computeService.GetOrder(no)
	if order.Status != "active" || gateway.splits != 1 {
		t.Fatalf("callback replay regressed delivery or repeated split: status=%s splits=%d", order.Status, gateway.splits)
	}
	settlements, err := service.GetOrderSettlements(no)
	if err != nil || len(settlements) != 2 {
		t.Fatalf("duplicate/missing settlement: %v %v", settlements, err)
	}
	for _, item := range settlements {
		if item.PayeeType == "supplier" && (item.PayeeID != 101 || item.Amount != 13110) {
			t.Fatalf("wrong supplier settlement: %+v", item)
		}
	}
}

func TestTradeLatePaymentCannotReviveCancelledOrderOrConsumeReleasedStock(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	created := tradeRequest(t, tradeRouter(db, 101), "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	tradeRequest(t, tradeRouter(db, 104), "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	placed := tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/orders", map[string]any{"product_id": id, "quantity": 2, "duration": 3, "compliance_agreed": true}, 0)
	no := placed.Data.(map[string]any)["order_no"].(string)
	service := payment.NewService(payment.NewRepository(db), db, &testPaymentGateway{})
	checkout, err := service.Pay(102, payment.PayReq{OrderNo: no, Channel: "wechat"})
	if err != nil {
		t.Fatal(err)
	}
	db.MustExec("UPDATE orders SET payment_expires_at=DATE_SUB(NOW(), INTERVAL 1 MINUTE) WHERE order_no=?", no)
	computeService := compute.NewService(compute.NewRepository(db), db)
	if n, err := computeService.CloseExpiredUnpaidOrders(); err != nil || n != 1 {
		t.Fatalf("expiration: %d %v", n, err)
	}
	if err := service.HandleCallback(payment.CallbackReq{OrderNo: no, TxID: checkout.TxID, Status: "success", Amount: 13800}); err == nil {
		t.Fatal("late callback revived cancelled order")
	}
	order, _ := computeService.GetOrder(no)
	if order.Status != "cancelled" {
		t.Fatalf("late callback: %s", order.Status)
	}
	products := tradeRequest(t, tradeRouter(db, 101), "GET", "/api/v1/supplier/products", nil, 0)
	if products.Data.([]any)[0].(map[string]any)["stock"] != float64(8) {
		t.Fatalf("stock was not released: %v", products.Data)
	}
}

func TestTradeAdminClosesNumericOrderIDAndReleasesStockOnce(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	created := tradeRequest(t, tradeRouter(db, 101), "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	admin := tradeRouter(db, 104)
	tradeRequest(t, admin, "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	placed := tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/orders", map[string]any{"product_id": id, "quantity": 2, "duration": 3, "compliance_agreed": true}, 0)
	no := placed.Data.(map[string]any)["order_no"].(string)
	service := compute.NewService(compute.NewRepository(db), db)
	order, _ := service.GetOrder(no)
	for _, identifier := range []string{fmt.Sprint(order.ID), no} {
		tradeRequest(t, admin, "PATCH", "/api/v1/admin/orders/"+identifier+"/status", map[string]string{"status": "cancelled"}, 0)
		current, _ := service.GetOrder(no)
		if current.Status != "cancelled" {
			t.Fatalf("order was not cancelled: %s", current.Status)
		}
	}
	products := tradeRequest(t, tradeRouter(db, 101), "GET", "/api/v1/supplier/products", nil, 0)
	if products.Data.([]any)[0].(map[string]any)["stock"] != float64(8) {
		t.Fatalf("stock not released exactly once: %v", products.Data)
	}
	tradeRequest(t, admin, "PATCH", "/api/v1/admin/orders/999999/status", map[string]string{"status": "cancelled"}, 40400)
}

func TestTradeInquiryReachesCRMWithProductContext(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	tradeRequest(t, tradeRouter(db, 103), "POST", "/api/v1/leads", map[string]string{"type": "compute", "description": "forged product inquiry"}, 40001)
	input := map[string]any{"product_type": "colocation", "power_capacity_kw": 200, "rack_count": 20,
		"price_negotiable": true, "pricing_mode": "monthly", "stock": 20, "region": "北京", "compliance_agreed": true}
	created := tradeRequest(t, tradeRouter(db, 101), "POST", "/api/v1/supplier/products", input, 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	path := fmt.Sprintf("/api/v1/products/%d/inquiries", id)
	request := map[string]string{"contact_name": "测试采购", "contact_phone": "18800001102", "message": "需要 5 个机柜，计划下个月交付"}
	tradeRequest(t, tradeRouter(db, 102), "POST", path, request, 40900)
	tradeRequest(t, tradeRouter(db, 104), "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	tradeRequest(t, tradeRouter(db, 102), "POST", path, request, 0)
	response := tradeRequest(t, tradeRouter(db, 104), "GET", "/api/v1/admin/leads", nil, 0)
	leads := response.Data.(map[string]any)["list"].([]any)
	lead := leads[0].(map[string]any)
	if lead["type"] != "compute" || lead["contact_name"] != "测试采购" || !strings.Contains(lead["description"].(string), fmt.Sprintf("商品 #%d", id)) {
		t.Fatalf("inquiry missing from CRM: %v", lead)
	}
	leadID := int64(lead["id"].(float64))
	tradeRequest(t, tradeRouter(db, 104), "POST", fmt.Sprintf("/api/v1/admin/leads/%d/assign", leadID), map[string]int64{"assignee_id": 104}, 0)
	response = tradeRequest(t, tradeRouter(db, 104), "GET", "/api/v1/admin/leads", nil, 0)
	lead = response.Data.(map[string]any)["list"].([]any)[0].(map[string]any)
	if lead["status"] != "assigned" || lead["assignee_id"] != float64(104) {
		t.Fatalf("inquiry assignment not persisted: %v", lead)
	}
}

func tradeRequest(t *testing.T, r http.Handler, method, path string, body any, wantCode int) envelope {
	t.Helper()
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(data))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var result envelope
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Code != wantCode {
		t.Fatalf("%s %s: want code %d, status=%d body=%s err=%v", method, path, wantCode, w.Code, w.Body.String(), err)
	}
	return result
}

func seedTradeUsers(db *sqlx.DB) {
	db.MustExec(`INSERT INTO users (id,phone,password_hash) VALUES
		(101,'18800001101',''),(102,'18800001102',''),(103,'18800001103',''),(104,'18800001104','')`)
	db.MustExec(`INSERT INTO user_roles (user_id,role) VALUES (101,'supplier'),(101,'buyer'),(102,'buyer'),(103,'buyer'),(104,'admin')`)
	db.MustExec(`INSERT INTO enterprises (user_id,name,uscc,status) VALUES (101,'Test supplier','TEST101','verified'),(102,'Test buyer','TEST102','verified')`)
	db.MustExec(`INSERT INTO supplier_qualifications (user_id,qual_type,cert_name,status) VALUES (101,'idc','Test license','verified')`)
}

func tradeProductInput() map[string]any {
	return map[string]any{"product_type": "card_rental", "gpu_model": "H100", "card_count": 8,
		"delivery_mode": "bare_metal", "pricing_mode": "hourly", "unit_price": 2300,
		"stock": 8, "min_order": 1, "min_duration": 1, "region": "北京", "compliance_agreed": true}
}

func TestTradeAdmissionRejectsWrongRoleUnverifiedAndMissingConsent(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	created := tradeRequest(t, tradeRouter(db, 101), "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	tradeRequest(t, tradeRouter(db, 104), "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	order := map[string]any{"product_id": id, "quantity": 2, "duration": 3, "compliance_agreed": true}
	for _, user := range []int64{103, 104} {
		tradeRequest(t, tradeRouter(db, user), "POST", "/api/v1/orders", order, 40300)
	}
	order["compliance_agreed"] = false
	tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/orders", order, 40001)
	order["compliance_agreed"] = true
	placed := tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/orders", order, 0)
	if placed.Data.(map[string]any)["total_amount"] != float64(13800) {
		t.Fatalf("unexpected amount: %v", placed.Data)
	}
	tradeRequest(t, tradeRouter(db, 104), "POST", "/api/v1/supplier/products", tradeProductInput(), 40300)
	db.MustExec("UPDATE enterprises SET status='rejected' WHERE user_id=101")
	tradeRequest(t, tradeRouter(db, 101), "POST", "/api/v1/supplier/products", tradeProductInput(), 40300)
}

func TestTradeRejectedProductCanBeCorrectedAndReviewedAgain(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	supplier, admin := tradeRouter(db, 101), tradeRouter(db, 104)
	input := tradeProductInput()
	created := tradeRequest(t, supplier, "POST", "/api/v1/supplier/products", input, 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	path := fmt.Sprintf("/api/v1/admin/audits/products/%d", id)
	tradeRequest(t, admin, "POST", path+"/reject", map[string]string{"reason": ""}, 40001)
	tradeRequest(t, admin, "POST", path+"/reject", map[string]string{"reason": "请更正资源规格"}, 0)
	list := tradeRequest(t, supplier, "GET", "/api/v1/supplier/products", nil, 0)
	product := list.Data.([]any)[0].(map[string]any)
	if product["status"] != "draft" || product["rejected_reason"] != "请更正资源规格" {
		t.Fatalf("missing rejection: %v", product)
	}
	tradeRequest(t, admin, "POST", path+"/approve", nil, 40900)
	input["gpu_model"] = "H200"
	editPath := fmt.Sprintf("/api/v1/supplier/products/%d", id)
	tradeRequest(t, tradeRouter(db, 102), "PUT", editPath, input, 40300)
	tradeRequest(t, supplier, "PUT", editPath, input, 0)
	list = tradeRequest(t, supplier, "GET", "/api/v1/supplier/products", nil, 0)
	product = list.Data.([]any)[0].(map[string]any)
	if product["status"] != "pending" || product["gpu_model"] != "H200" || product["rejected_reason"] != "" {
		t.Fatalf("resubmission: %v", product)
	}
	tradeRequest(t, admin, "POST", path+"/approve", nil, 0)
	tradeRequest(t, supplier, "PUT", editPath, input, 40900)
	tradeRequest(t, admin, "POST", path+"/reject", map[string]string{"reason": "stale review"}, 40900)
}
