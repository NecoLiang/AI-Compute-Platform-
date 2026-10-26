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
	"sync"
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
	// 会话时区与驱动解析时区都钉死 +08:00: 订单过期等时间比较依赖两者一致,
	// 否则在 +08:00 的库上会出现 8 小时偏移, "已过期"的订单看起来还没过期。
	// 注意必须用可被 LoadLocation 解析的时区名, FixedZone 的名字进不了 DSN。
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	cfg.Loc = loc
	if cfg.Params == nil {
		cfg.Params = map[string]string{}
	}
	cfg.Params["time_zone"] = "'+08:00'"
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
	h.RegisterMarketRoutes(g)
	h.RegisterAdminRoutes(g)
	intermediary.NewHandler(intermediary.NewService(intermediary.NewRepository(db))).RegisterAdminRoutes(g)
	intermediary.NewHandler(intermediary.NewService(intermediary.NewRepository(db))).RegisterAuthenticatedRoutes(g)
	payment.NewHandler(payment.NewService(payment.NewRepository(db), db)).RegisterBuyerRoutes(g)
	return r
}

func TestTradePublicProductVisibilityPreservesPrivateAccess(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	supplier, buyer, admin := tradeRouter(db, 101), tradeRouter(db, 102), tradeRouter(db, 104)
	public := gin.New()
	compute.NewHandler(compute.NewService(compute.NewRepository(db), db)).RegisterMarketRoutes(public.Group("/api/v1"))
	created := tradeRequest(t, supplier, "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	path := fmt.Sprintf("/api/v1/products/%d", id)
	missing := tradeRequest(t, public, "GET", "/api/v1/products/999999", nil, 40400)
	assertHidden := func(t *testing.T) {
		t.Helper()
		hidden := tradeRequest(t, public, "GET", path, nil, 40400)
		if hidden.Message != missing.Message || hidden.Data != nil {
			t.Fatalf("hidden product exposed its existence or data: %+v", hidden)
		}
	}
	assertHidden(t)
	tradeRequest(t, admin, "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	visible := tradeRequest(t, public, "GET", path, nil, 0)
	if visible.Data.(map[string]any)["product"].(map[string]any)["gpu_model"] != "H100" {
		t.Fatalf("approved product not visible: %v", visible.Data)
	}
	placed := tradeRequest(t, buyer, "POST", "/api/v1/orders", map[string]any{"product_id": id, "quantity": 2, "duration": 3, "compliance_agreed": true, "compliance_version": "2026-09-06.1"}, 0)
	no := placed.Data.(map[string]any)["order_no"].(string)
	for _, status := range []string{"pending", "draft", "sold_out", "offline", "frozen"} {
		t.Run(status, func(t *testing.T) {
			// Seed each visibility state; all observations use the HTTP interfaces.
			db.MustExec("UPDATE products SET status=? WHERE id=?", status, id)
			assertHidden(t)
			market := tradeRequest(t, public, "GET", "/api/v1/products", nil, 0)
			if market.Data.(map[string]any)["total"] != float64(0) {
				t.Fatalf("hidden product leaked into market: %v", market.Data)
			}
			own := tradeRequest(t, supplier, "GET", "/api/v1/supplier/products", nil, 0)
			if own.Data.([]any)[0].(map[string]any)["status"] != status {
				t.Fatalf("supplier lost access to own product: %v", own.Data)
			}
			audit := tradeRequest(t, admin, "GET", "/api/v1/admin/products?status="+status, nil, 0)
			if audit.Data.(map[string]any)["total"] != float64(1) {
				t.Fatalf("admin lost access to product: %v", audit.Data)
			}
			order := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+no, nil, 0)
			if order.Data.(map[string]any)["product"].(map[string]any)["id"] != float64(id) {
				t.Fatalf("buyer lost historical order product: %v", order.Data)
			}
		})
	}
}

func TestTradePaymentRejectsWrongOwnerBeforeContactingProvider(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	created := tradeRequest(t, tradeRouter(db, 101), "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	tradeRequest(t, tradeRouter(db, 104), "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	placed := tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/orders", map[string]any{"product_id": id, "quantity": 1, "duration": 1, "compliance_agreed": true, "compliance_version": "2026-09-06.1"}, 0)
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
	mu         sync.Mutex
	amount     int64
	calls      int
	splits     []payment.DivideOrder
	refunds    []payment.RefundOrder
	refundFail bool
	channels   []string // nil 表示全部开通
}

func (g *testPaymentGateway) Configured() bool { return true }
func (g *testPaymentGateway) PayChannels() []string {
	if g.channels != nil {
		return g.channels
	}
	return []string{"wechat", "alipay", "bank"}
}
func (g *testPaymentGateway) CreatePayment(o payment.PaymentOrder) (*payment.Checkout, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.amount, g.calls = o.Amount, g.calls+1
	return &payment.Checkout{PayURL: "https://cashier.example.test/" + o.OrderNo, UniqueOrderNo: "TX" + o.OrderNo}, nil
}
func (g *testPaymentGateway) QueryPayment(no string) (*payment.PaymentResult, error) {
	return &payment.PaymentResult{OrderNo: no, UniqueOrderNo: "TX" + no, Status: "PROCESSING"}, nil
}
func (g *testPaymentGateway) Divide(o payment.DivideOrder) (*payment.DivideResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.splits = append(g.splits, o)
	return &payment.DivideResult{Status: "SUCCESS", Details: []payment.DivideDetail{{DetailNo: "DD" + o.RequestID, LedgerNo: o.Items[0].LedgerNo, Amount: o.Items[0].Amount}}}, nil
}
func (g *testPaymentGateway) QueryDivide(no, requestID string) (*payment.DivideResult, error) {
	return &payment.DivideResult{Status: "SUCCESS"}, nil
}
func (g *testPaymentGateway) Refund(o payment.RefundOrder) (*payment.RefundResult, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.refunds = append(g.refunds, o)
	if g.refundFail {
		return nil, &payment.YeepayBizError{Op: "退款", Code: "OPR13001", Message: "余额不足"}
	}
	// 退款受理后异步完成, 终态由退款通知送达。
	return &payment.RefundResult{OrderNo: o.OrderNo, RequestID: o.RequestID, Status: "PROCESSING", Amount: o.Amount}, nil
}
func (g *testPaymentGateway) QueryRefund(no, requestID string) (*payment.RefundResult, error) {
	return &payment.RefundResult{OrderNo: no, RequestID: requestID, Status: "PROCESSING"}, nil
}
func (g *testPaymentGateway) NetInURL(requestNo string) (string, error) {
	return "https://netin.example.test/" + requestNo, nil
}
func (g *testPaymentGateway) QueryNetIn(requestNo string) (*payment.NetInResult, error) {
	return &payment.NetInResult{RequestNo: requestNo, ApplicationStatus: "REVIEWING"}, nil
}

// DecryptNotify 测试替身的"验签": 只有 signed: 前缀的报文视为易宝签发。
func (g *testPaymentGateway) DecryptNotify(raw string) ([]byte, error) {
	if !strings.HasPrefix(raw, "signed:") {
		return nil, payment.ErrYeepayCallbackUnverifiable
	}
	return []byte(strings.TrimPrefix(raw, "signed:")), nil
}

func (g *testPaymentGateway) splitCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.splits)
}

func yuanString(fen int64) string { return fmt.Sprintf("%d.%02d", fen/100, fen%100) }

func signedNotify(t testing.TB, payload map[string]any) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return "signed:" + string(raw)
}

// testCallback 一笔易宝支付结果通知。
type testCallback struct {
	OrderNo, TxID, Status string
	Amount                int64
}

func sendPayNotify(t testing.TB, svc *payment.Service, cb testCallback) error {
	status := map[string]string{"success": "SUCCESS", "fail": "FAIL"}[cb.Status]
	return svc.HandleNotify(payment.NotifyPay, signedNotify(t, map[string]any{
		"orderId": cb.OrderNo, "uniqueOrderNo": cb.TxID, "status": status, "orderAmount": yuanString(cb.Amount),
		"paySuccessDate": time.Now().Format("2006-01-02 15:04:05"),
	}))
}

func sendClearNotify(t testing.TB, svc *payment.Service, no string, amount int64) error {
	return svc.HandleNotify(payment.NotifyClear, signedNotify(t, map[string]any{
		"orderId": no, "uniqueOrderNo": "TX" + no, "status": "SUCCESS", "orderAmount": yuanString(amount),
		"csSuccessDate": time.Now().Format("2006-01-02 15:04:05"),
	}))
}

func sendRefundNotify(t testing.TB, svc *payment.Service, no, requestID, status string, amount int64) error {
	return svc.HandleNotify(payment.NotifyRefund, signedNotify(t, map[string]any{
		"orderId": no, "refundRequestId": requestID, "status": status, "refundAmount": yuanString(amount),
	}))
}

func TestTradePaymentCallbackUnlocksDeliveryAndIsIdempotent(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	created := tradeRequest(t, tradeRouter(db, 101), "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	tradeRequest(t, tradeRouter(db, 104), "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	placed := tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/orders", map[string]any{"product_id": id, "quantity": 2, "duration": 3, "compliance_agreed": true, "compliance_version": "2026-09-06.1"}, 0)
	no := placed.Data.(map[string]any)["order_no"].(string)
	gateway := &testPaymentGateway{channels: []string{"bank"}}
	service := payment.NewService(payment.NewRepository(db), db, gateway)
	if _, err := service.Pay(102, payment.PayReq{OrderNo: no, Channel: "alipay"}); err == nil || gateway.calls != 0 {
		t.Fatalf("未开通的支付方式不得向易宝下单: calls=%d err=%v", gateway.calls, err)
	}
	gateway.channels = nil
	checkout, err := service.Pay(102, payment.PayReq{OrderNo: no, Channel: "wechat"})
	if err != nil || gateway.amount != 13800 {
		t.Fatalf("checkout: %+v amount=%d err=%v", checkout, gateway.amount, err)
	}
	repeated, err := service.Pay(102, payment.PayReq{OrderNo: no, Channel: "wechat"})
	if err != nil || repeated == nil || repeated.TxID != checkout.TxID || gateway.calls != 1 {
		t.Fatalf("repeated pay created another payment: %+v calls=%d err=%v", repeated, gateway.calls, err)
	}
	req := testCallback{TxID: checkout.TxID, OrderNo: no, Status: "fail", Amount: 13800}
	if err := sendPayNotify(t, service, req); err != nil {
		t.Fatal(err)
	}
	if payments, _ := service.GetOrderPayments(102, no); len(payments) != 1 || payments[0].Status != "pending" {
		t.Fatalf("failed payment must not be accepted as paid: %+v", payments)
	}
	req.Status, req.Amount = "success", 1
	if err := sendPayNotify(t, service, req); err == nil {
		t.Fatal("amount mismatch must be rejected")
	}
	req.Amount = 13800
	if err := service.HandleNotify(payment.NotifyPay, `{"orderId":"`+no+`","uniqueOrderNo":"`+checkout.TxID+`","status":"SUCCESS","orderAmount":"138.00"}`); err == nil {
		t.Fatal("unsigned notify must be rejected")
	}
	if err := sendPayNotify(t, service, req); err != nil {
		t.Fatal(err)
	}
	if err := sendPayNotify(t, service, req); err != nil {
		t.Fatal(err)
	}
	computeService := compute.NewService(compute.NewRepository(db), db, strings.Repeat("12", 32))
	order, err := computeService.GetOrder(no)
	if err != nil || order.Status != "paid" {
		t.Fatalf("business order not paid: %+v %v", order, err)
	}
	// 已清算但未交付: 资金留在易宝待分账, 不得分账。供给方开通商编前同样不分。
	if err := sendClearNotify(t, service, no, 13800); err != nil {
		t.Fatal(err)
	}
	if gateway.splitCount() != 0 {
		t.Fatal("split before delivery confirmation")
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
	if err := service.RunJobs(); err != nil || gateway.splitCount() != 0 {
		t.Fatalf("split without supplier merchant: splits=%d err=%v", gateway.splitCount(), err)
	}
	if err := service.BindMerchant(104, 101, "10089000101"); err != nil {
		t.Fatal(err)
	}
	if err := service.RunJobs(); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := sendPayNotify(t, service, req); err != nil {
			t.Fatal(err)
		}
		if err := sendClearNotify(t, service, no, 13800); err != nil {
			t.Fatal(err)
		}
		if err := service.RunJobs(); err != nil {
			t.Fatal(err)
		}
	}
	order, _ = computeService.GetOrder(no)
	if order.Status != "active" || gateway.splitCount() != 1 {
		t.Fatalf("callback replay regressed delivery or repeated split: status=%s splits=%d", order.Status, gateway.splitCount())
	}
	split := gateway.splits[0]
	if split.OrderNo != no || len(split.Items) != 1 || split.Items[0].LedgerNo != "10089000101" || split.Items[0].Amount != 13110 {
		t.Fatalf("divide must send only the supplier share to its merchant: %+v", split)
	}
	settlements, err := service.GetOrderSettlements(101, no)
	if err != nil || len(settlements) != 2 {
		t.Fatalf("duplicate/missing settlement: %v %v", settlements, err)
	}
	for _, item := range settlements {
		if item.Status != "success" {
			t.Fatalf("settlement not divided: %+v", item)
		}
		if item.PayeeType == "supplier" && (item.PayeeID != 101 || item.Amount != 13110) {
			t.Fatalf("wrong supplier settlement: %+v", item)
		}
	}

	// 交付后退款: 运营执行, 已分给供给方的部分经 divideBackDetail 原路归还。
	if err := computeService.RequestRefund(102, no); err != nil {
		t.Fatal(err)
	}
	service.SetRefundHook(computeService.CompleteRefund)
	if _, err := service.ExecuteRefund(104, no, "买家申请退款"); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ExecuteRefund(104, no, "买家申请退款"); err == nil {
		t.Fatal("refund submitted twice")
	}
	if len(gateway.refunds) != 1 {
		t.Fatalf("refunds=%d", len(gateway.refunds))
	}
	refund := gateway.refunds[0]
	if refund.Amount != 13800 || len(refund.DivideBack) != 1 || refund.DivideBack[0].LedgerNo != "10089000101" ||
		refund.DivideBack[0].Amount != 13110 || refund.DivideBack[0].DivideDetailNo != "DD"+split.RequestID {
		t.Fatalf("refund must claw back the supplier share: %+v", refund)
	}
	if err := sendRefundNotify(t, service, no, refund.RequestID+"x", "SUCCESS", 13800); err == nil {
		t.Fatal("unknown refund request accepted")
	}
	for range 2 {
		if err := sendRefundNotify(t, service, no, refund.RequestID, "SUCCESS", 13800); err != nil {
			t.Fatal(err)
		}
	}
	order, _ = computeService.GetOrder(no)
	if order.Status != "refunded" {
		t.Fatalf("order not refunded: %s", order.Status)
	}
	settlements, _ = service.GetOrderSettlements(101, no)
	for _, item := range settlements {
		if item.Status != "reversed" {
			t.Fatalf("divided settlement not reversed: %+v", item)
		}
	}
	if total, succeeded, pending, err := service.SumSupplierSettlements(101); err != nil || total != 0 || succeeded != 0 || pending != 0 {
		t.Fatalf("refunded order still counted: %d %d %d %v", total, succeeded, pending, err)
	}
}

// 交付前退款: 资金仍在待分账账户, 原路退回且不归还任何分账; 退款失败可由运营重试。
func TestTradeRefundBeforeDeliveryUsesUndividedFunds(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	created := tradeRequest(t, tradeRouter(db, 101), "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	tradeRequest(t, tradeRouter(db, 104), "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	placed := tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/orders", map[string]any{"product_id": id, "quantity": 2, "duration": 3, "compliance_agreed": true, "compliance_version": "2026-09-06.1"}, 0)
	no := placed.Data.(map[string]any)["order_no"].(string)
	gateway := &testPaymentGateway{refundFail: true}
	service := payment.NewService(payment.NewRepository(db), db, gateway)
	computeService := compute.NewService(compute.NewRepository(db), db)
	service.SetRefundHook(computeService.CompleteRefund)
	if err := service.BindMerchant(104, 101, "10089000101"); err != nil {
		t.Fatal(err)
	}
	checkout, err := service.Pay(102, payment.PayReq{OrderNo: no, Channel: "alipay"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sendPayNotify(t, service, testCallback{OrderNo: no, TxID: checkout.TxID, Status: "success", Amount: 13800}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ExecuteRefund(104, no, ""); err == nil {
		t.Fatal("paid order refunded without buyer request or cancellation")
	}
	if err := computeService.RequestRefund(102, no); err != nil {
		t.Fatal(err)
	}
	// 未清算: 先排队, 清算入账后由定时任务提交。
	if status, err := service.ExecuteRefund(104, no, ""); err != nil || status != "pending" || len(gateway.refunds) != 0 {
		t.Fatalf("uncleared refund: %s %v refunds=%d", status, err, len(gateway.refunds))
	}
	if err := sendClearNotify(t, service, no, 13800); err != nil {
		t.Fatal(err)
	}
	if gateway.splitCount() != 0 {
		t.Fatal("refunding order was divided")
	}
	if err := service.RunJobs(); err != nil {
		t.Fatal(err)
	}
	if len(gateway.refunds) != 1 || len(gateway.refunds[0].DivideBack) != 0 {
		t.Fatalf("undivided refund must not claw back: %+v", gateway.refunds)
	}
	refunds, err := service.ListRefunds()
	if err != nil || len(refunds) != 1 || refunds[0].RefundStatus == nil || *refunds[0].RefundStatus != "failed" {
		t.Fatalf("failed refund not visible to operators: %+v %v", refunds, err)
	}
	gateway.refundFail = false
	if status, err := service.ExecuteRefund(104, no, "重试"); err != nil || status != "processing" {
		t.Fatalf("retry: %s %v", status, err)
	}
	if err := sendRefundNotify(t, service, no, gateway.refunds[1].RequestID, "SUCCESS", 13800); err != nil {
		t.Fatal(err)
	}
	order, _ := computeService.GetOrder(no)
	if order.Status != "refunded" {
		t.Fatalf("order not refunded: %s", order.Status)
	}
	settlements, _ := service.GetOrderSettlements(101, no)
	for _, item := range settlements {
		if item.Status != "cancelled" {
			t.Fatalf("undivided settlement not cancelled: %+v", item)
		}
	}
	products := tradeRequest(t, tradeRouter(db, 101), "GET", "/api/v1/supplier/products", nil, 0)
	if products.Data.([]any)[0].(map[string]any)["stock"] != float64(8) {
		t.Fatalf("stock was not released: %v", products.Data)
	}
}

func TestTradeLatePaymentCannotReviveCancelledOrderOrConsumeReleasedStock(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	created := tradeRequest(t, tradeRouter(db, 101), "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	tradeRequest(t, tradeRouter(db, 104), "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	placed := tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/orders", map[string]any{"product_id": id, "quantity": 2, "duration": 3, "compliance_agreed": true, "compliance_version": "2026-09-06.1"}, 0)
	no := placed.Data.(map[string]any)["order_no"].(string)
	gateway := &testPaymentGateway{}
	service := payment.NewService(payment.NewRepository(db), db, gateway)
	checkout, err := service.Pay(102, payment.PayReq{OrderNo: no, Channel: "wechat"})
	if err != nil {
		t.Fatal(err)
	}
	db.MustExec("UPDATE orders SET payment_expires_at=DATE_SUB(NOW(), INTERVAL 1 MINUTE) WHERE order_no=?", no)
	computeService := compute.NewService(compute.NewRepository(db), db)
	if n, err := computeService.CloseExpiredUnpaidOrders(); err != nil || n != 1 {
		t.Fatalf("expiration: %d %v", n, err)
	}
	// 迟到的支付: 钱已进易宝, 订单不复活, 收款记为待原路退回。
	if err := sendPayNotify(t, service, testCallback{OrderNo: no, TxID: checkout.TxID, Status: "success", Amount: 13800}); err != nil {
		t.Fatal(err)
	}
	order, _ := computeService.GetOrder(no)
	if order.Status != "cancelled" {
		t.Fatalf("late callback: %s", order.Status)
	}
	assertOrphanRefund(t, db, service, gateway, no, 13800)
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
	placed := tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/orders", map[string]any{"product_id": id, "quantity": 2, "duration": 3, "compliance_agreed": true, "compliance_version": "2026-09-06.1"}, 0)
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

// 运营关闭订单: 租期未开始的自动原路退款; 交付后关闭的不自动退, 进入退款待办由运营决定。
func TestTradeAdminCloseRefundsOnlyUndeliveredOrdersAutomatically(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	created := tradeRequest(t, tradeRouter(db, 101), "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	admin := tradeRouter(db, 104)
	tradeRequest(t, admin, "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	gateway := &testPaymentGateway{}
	service := payment.NewService(payment.NewRepository(db), db, gateway)
	computeService := compute.NewService(compute.NewRepository(db), db, strings.Repeat("12", 32))
	service.SetRefundHook(computeService.CompleteRefund)
	if err := service.BindMerchant(104, 101, "10089000101"); err != nil {
		t.Fatal(err)
	}
	paidOrder := func() string {
		placed := tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/orders", map[string]any{"product_id": id, "quantity": 1, "duration": 3, "compliance_agreed": true, "compliance_version": "2026-09-06.1"}, 0)
		no := placed.Data.(map[string]any)["order_no"].(string)
		order, _ := computeService.GetOrder(no)
		checkout, err := service.Pay(102, payment.PayReq{OrderNo: no, Channel: "alipay"})
		if err != nil {
			t.Fatal(err)
		}
		if err := sendPayNotify(t, service, testCallback{OrderNo: no, TxID: checkout.TxID, Status: "success", Amount: order.TotalAmount}); err != nil {
			t.Fatal(err)
		}
		if err := sendClearNotify(t, service, no, order.TotalAmount); err != nil {
			t.Fatal(err)
		}
		return no
	}
	undelivered, delivered := paidOrder(), paidOrder()
	if _, err := computeService.DeliverWithAccess(101, delivered, compute.DeliverInfo{IpAddress: "192.0.2.10"}, false); err != nil {
		t.Fatal(err)
	}
	if err := computeService.ConfirmDelivery(102, delivered); err != nil {
		t.Fatal(err)
	}
	if err := service.RunJobs(); err != nil || gateway.splitCount() != 1 {
		t.Fatalf("delivered order not divided: %d %v", gateway.splitCount(), err)
	}
	for _, no := range []string{undelivered, delivered} {
		tradeRequest(t, admin, "PATCH", "/api/v1/admin/orders/"+no+"/status", map[string]string{"status": "cancelled"}, 0)
	}
	if err := service.RunJobs(); err != nil {
		t.Fatal(err)
	}
	if len(gateway.refunds) != 1 || gateway.refunds[0].OrderNo != undelivered || len(gateway.refunds[0].DivideBack) != 0 {
		t.Fatalf("only the undelivered order may be refunded automatically: %+v", gateway.refunds)
	}
	refunds, err := service.ListRefunds()
	if err != nil {
		t.Fatal(err)
	}
	var queued bool
	for _, item := range refunds {
		queued = queued || item.OrderNo == delivered && item.RefundStatus == nil
	}
	if !queued {
		t.Fatalf("closed delivered order missing from operator refund queue: %+v", refunds)
	}
	if _, err := service.ExecuteRefund(104, delivered, "运营核实后全额退款"); err != nil {
		t.Fatal(err)
	}
	if len(gateway.refunds) != 2 || len(gateway.refunds[1].DivideBack) != 1 {
		t.Fatalf("operator refund must claw back the divided share: %+v", gateway.refunds)
	}
}

func TestTradeInquiryReachesCRMWithProductContext(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	tradeRequest(t, tradeRouter(db, 103), "POST", "/api/v1/leads", map[string]string{"type": "compute", "description": "forged product inquiry"}, 40001)
	input := map[string]any{"product_type": "colocation", "power_capacity_kw": 200, "rack_count": 20,
		"price_negotiable": true, "pricing_mode": "monthly", "stock": 20, "region": "北京", "compliance_agreed": true, "compliance_version": "2026-09-06.1"}
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

type paymentState struct {
	Status       string  `db:"status"`
	RefundStatus *string `db:"refund_status"`
}

// assertQueuedForRefund 已关闭订单收到的付款: 订单不复活, 收款排队原路退回, 不产生任何应结。
func assertQueuedForRefund(t *testing.T, db *sqlx.DB, no string) {
	t.Helper()
	var state paymentState
	if err := db.Get(&state, "SELECT status,refund_status FROM payments WHERE order_no=?", no); err != nil {
		t.Fatal(err)
	}
	if state.Status != "paid" || state.RefundStatus == nil || *state.RefundStatus != "pending" {
		t.Fatalf("late payment not queued for refund: %+v", state)
	}
	var orderStatus string
	if err := db.Get(&orderStatus, "SELECT status FROM orders WHERE order_no=?", no); err != nil || orderStatus != "cancelled" {
		t.Fatalf("closed order revived by payment: %s %v", orderStatus, err)
	}
	var settlements int
	if err := db.Get(&settlements, "SELECT COUNT(*) FROM settlements WHERE order_no=?", no); err != nil || settlements != 0 {
		t.Fatalf("late payment created settlements: %d %v", settlements, err)
	}
}

// assertOrphanRefund 已关闭订单收到的付款必须在清算后自动原路退回。
func assertOrphanRefund(t *testing.T, db *sqlx.DB, service *payment.Service, gateway *testPaymentGateway, no string, amount int64) {
	t.Helper()
	assertQueuedForRefund(t, db, no)
	var state paymentState
	before := len(gateway.refunds)
	if err := sendClearNotify(t, service, no, amount); err != nil {
		t.Fatal(err)
	}
	if err := service.RunJobs(); err != nil {
		t.Fatal(err)
	}
	if len(gateway.refunds) != before+1 || gateway.refunds[before].Amount != amount || len(gateway.refunds[before].DivideBack) != 0 {
		t.Fatalf("orphan refund not submitted: %+v", gateway.refunds)
	}
	if err := sendRefundNotify(t, service, no, gateway.refunds[before].RequestID, "SUCCESS", amount); err != nil {
		t.Fatal(err)
	}
	db.Get(&state, "SELECT status,refund_status FROM payments WHERE order_no=?", no)
	if state.Status != "refunded" {
		t.Fatalf("orphan refund not completed: %+v", state)
	}
}

func seedTradeUsers(db *sqlx.DB) {
	db.MustExec(`INSERT INTO users (id,phone,password_hash) VALUES
		(101,'18800001101',''),(102,'18800001102',''),(103,'18800001103',''),(104,'18800001104','')`)
	db.MustExec(`INSERT INTO user_roles (user_id,role) VALUES (101,'supplier'),(101,'buyer'),(102,'buyer'),(103,'buyer'),(104,'admin')`)
	db.MustExec(`INSERT INTO enterprises (user_id,name,uscc,status) VALUES (101,'Test supplier','TEST101','verified'),(102,'Test buyer','TEST102','verified')`)
	db.MustExec(`INSERT INTO supplier_qualifications (user_id,qual_type,cert_name,status) VALUES (101,'idc','Test license','verified')`)
	// 105: 供给方角色 + 个人实名 + 资质, 但无企业认证 —— 供给方准入要求企业主体, 应被拒。
	db.MustExec(`INSERT INTO users (id,phone,password_hash) VALUES (105,'18800001105','')`)
	db.MustExec(`INSERT INTO user_roles (user_id,role) VALUES (105,'supplier'),(105,'buyer')`)
	db.MustExec(`INSERT INTO user_kyc (user_id,real_name,id_card,status) VALUES (105,'个人供给','110101199001010011','verified')`)
	db.MustExec(`INSERT INTO supplier_qualifications (user_id,qual_type,cert_name,status) VALUES (105,'idc','Personal license','verified')`)
}

func tradeProductInput() map[string]any {
	return map[string]any{"product_type": "card_rental", "gpu_model": "H100", "card_count": 8,
		"delivery_mode": "bare_metal", "pricing_mode": "hourly", "unit_price": 2300,
		"stock": 8, "min_order": 1, "min_duration": 1, "region": "北京", "compliance_agreed": true, "compliance_version": "2026-09-06.1"}
}

func TestTradeAdmissionRejectsWrongRoleUnverifiedAndMissingConsent(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	// 供给方必须企业认证: 仅个人实名(即使有资质)不得上架商品。
	tradeRequest(t, tradeRouter(db, 105), "POST", "/api/v1/supplier/products", tradeProductInput(), 40300)
	created := tradeRequest(t, tradeRouter(db, 101), "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	tradeRequest(t, tradeRouter(db, 104), "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	order := map[string]any{"product_id": id, "quantity": 2, "duration": 3, "compliance_agreed": true, "compliance_version": "2026-09-06.1"}
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
