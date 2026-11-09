package compute_test

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jmoiron/sqlx"

	"tokenfactory/internal/compute"
	"tokenfactory/internal/payment"
)

const consentVersion = "2026-09-06.1"

// approvedProduct 供给方发布 + 运营审核通过, 返回商品 ID。
func approvedProduct(t *testing.T, db *sqlx.DB, input map[string]any) int64 {
	t.Helper()
	created := tradeRequest(t, tradeRouter(db, 101), "POST", "/api/v1/supplier/products", input, 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	tradeRequest(t, tradeRouter(db, 104), "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	return id
}

// payDeliverConfirm 走真实支付回调、交付、买家确认, 让订单进入 active 租期。
func payDeliverConfirm(t *testing.T, db *sqlx.DB, no string, amount int64) {
	t.Helper()
	payments := payment.NewService(payment.NewRepository(db), db, &testPaymentGateway{})
	pay, err := payments.Pay(102, payment.PayReq{OrderNo: no, Channel: "wechat"})
	if err != nil {
		t.Fatal(err)
	}
	if err := sendPayNotify(t, payments, testCallback{OrderNo: no, TxID: pay.TxID, Status: "success", Amount: amount}); err != nil {
		t.Fatal(err)
	}
	service := compute.NewService(compute.NewRepository(db), db, strings.Repeat("12", 32))
	if _, err := service.DeliverWithAccess(101, no, compute.DeliverInfo{IpAddress: "192.0.2.10"}, false); err != nil {
		t.Fatal(err)
	}
	if err := service.ConfirmDelivery(102, no); err != nil {
		t.Fatal(err)
	}
}

func orderSnapshot(t *testing.T, db *sqlx.DB, no string) (mode sql.NullString, unit int64) {
	t.Helper()
	var row struct {
		Mode sql.NullString `db:"pricing_mode"`
		Unit int64          `db:"unit_price"`
	}
	if err := db.Get(&row, "SELECT pricing_mode,unit_price FROM orders WHERE order_no=?", no); err != nil {
		t.Fatal(err)
	}
	return row.Mode, row.Unit
}

// resubmitPrices 供给方下架 → 改价重提 → 运营审核通过, 即真实的改价流程。
func resubmitPrices(t *testing.T, db *sqlx.DB, id int64, prices []map[string]any) {
	t.Helper()
	supplier := tradeRouter(db, 101)
	tradeRequest(t, supplier, "PATCH", fmt.Sprintf("/api/v1/supplier/products/%d/offline", id), nil, 0)
	input := multiPriceProductInput()
	input["prices"] = prices
	tradeRequest(t, supplier, "PUT", fmt.Sprintf("/api/v1/supplier/products/%d", id), input, 0)
	tradeRequest(t, tradeRouter(db, 104), "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
}

// T-091: 下单按所选计费方式报价并快照; 商品改价/换展示主价后, 续租仍按订单快照的方式报价;
// 该方式被停用后续租返回 40900。
func TestPricingOrderSnapshotDrivesRenewal(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	id := approvedProduct(t, db, multiPriceProductInput()) // hourly 2300 / daily 60000 min2 / monthly 1500000
	buyer := tradeRouter(db, 102)
	order := func(mode string, qty, dur int, want int) map[string]any {
		t.Helper()
		body := map[string]any{"product_id": id, "quantity": qty, "duration": dur, "compliance_agreed": true, "compliance_version": consentVersion}
		if mode != "" {
			body["pricing_mode"] = mode
		}
		res := tradeRequest(t, buyer, "POST", "/api/v1/orders", body, want)
		if want != 0 {
			return nil
		}
		return res.Data.(map[string]any)
	}
	order("daily", 1, 1, 40001)   // 日价最短 2 天
	order("weekly", 1, 1, 40001)  // 未开放
	order("nightly", 1, 1, 40001) // 未开放
	order("monthly", 1, 121, 40001)

	legacy := order("", 1, 3, 0) // 不传 pricing_mode = 展示主价(hourly)
	if mode, unit := orderSnapshot(t, db, legacy["order_no"].(string)); mode.String != "hourly" || unit != 2300 || legacy["total_amount"] != float64(6900) {
		t.Fatalf("default mode snapshot wrong: %v %d %v", mode, unit, legacy)
	}
	daily := order("daily", 2, 2, 0)
	no := daily["order_no"].(string)
	if mode, unit := orderSnapshot(t, db, no); mode.String != "daily" || unit != 60000 || daily["total_amount"] != float64(240000) {
		t.Fatalf("daily snapshot wrong: %v %d %v", mode, unit, daily)
	}
	payDeliverConfirm(t, db, no, 240000)
	detail := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+no, nil, 0).Data.(map[string]any)
	o, p := detail["order"].(map[string]any), detail["product"].(map[string]any)
	start, _ := time.Parse(time.RFC3339, o["lease_start_at"].(string))
	end, _ := time.Parse(time.RFC3339, o["lease_end_at"].(string))
	if end.Sub(start) != 48*time.Hour || p["pricing_mode"] != "daily" || p["min_duration"] != float64(2) || detail["actions"].(map[string]any)["can_renew"] != true {
		t.Fatalf("daily lease/detail wrong: %v", detail)
	}

	// 改价并让小时价成为展示主价; 日价最短租期改为 1。续租仍按 daily 快照、取新的日价。
	resubmitPrices(t, db, id, []map[string]any{
		{"pricing_mode": "hourly", "unit_price": 2000},
		{"pricing_mode": "daily", "unit_price": 45000},
	})
	quote := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+no+"/renewal-quote?duration=1", nil, 0).Data.(map[string]any)
	renewedUntil := end.Add(24 * time.Hour).Format(time.RFC3339)
	if quote["pricing_mode"] != "daily" || quote["unit_price"] != float64(45000) || quote["total_amount"] != float64(90000) ||
		quote["min_duration"] != float64(1) || quote["max_duration"] != float64(3650) || quote["renewed_until"] != renewedUntil {
		t.Fatalf("renewal must quote the order's pricing mode: %v", quote)
	}
	child := tradeRequest(t, buyer, "POST", "/api/v1/orders/"+no+"/renew", renewalInput(quote, "4f1f2c55-7c0f-4f4e-9a52-2a3c4e8d1b11"), 0).Data.(map[string]any)
	if mode, unit := orderSnapshot(t, db, child["order_no"].(string)); mode.String != "daily" || unit != 45000 {
		t.Fatalf("renewal order must carry the pricing snapshot: %v %d", mode, unit)
	}
	tradeRequest(t, buyer, "POST", "/api/v1/orders/"+child["order_no"].(string)+"/cancel", nil, 0)

	// 供给方停用日价: 续租不换算成其他方式, 返回 40900 提示重新下单; 详情页不再显示可续租。
	resubmitPrices(t, db, id, []map[string]any{{"pricing_mode": "hourly", "unit_price": 2000}})
	res := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+no+"/renewal-quote?duration=1", nil, 40900)
	if !strings.Contains(res.Message, "该计费方式已停用") {
		t.Fatalf("unexpected message: %s", res.Message)
	}
	tradeRequest(t, buyer, "POST", "/api/v1/orders/"+no+"/renew", renewalInput(quote, "5b9e1d0a-2f36-4c43-8a1e-9d3f6b7c2a01"), 40900)
	detail = tradeRequest(t, buyer, "GET", "/api/v1/orders/"+no, nil, 0).Data.(map[string]any)
	if detail["actions"].(map[string]any)["can_renew"] != false {
		t.Fatalf("disabled mode must not be renewable: %v", detail["actions"])
	}
	order("daily", 1, 2, 40001) // 新下单同样不能再选日价
}

// legacyPaidOrder 在只执行到 026 的库里按 v1.2 前代码的写法写入一个零租商品和一笔已付款订单
// (无 028 列, 库存已扣), 再执行真实的 028 迁移文件。返回库、商品 ID、订单号。
func legacyPaidOrder(t *testing.T, mode string, unit int64, duration int) (*sqlx.DB, int64, string) {
	t.Helper()
	db := setupTradeDBBefore(t, "028")
	seedTradeUsers(db)
	res := db.MustExec(`INSERT INTO products (supplier_id,product_type,gpu_model,card_count,delivery_mode,pricing_mode,unit_price,
		stock,min_order,min_duration,region,status,compliance_agreed) VALUES (101,'card_rental','H100',8,'bare_metal',?,?,7,1,1,'北京','active',1)`, mode, unit)
	id, _ := res.LastInsertId()
	no := "ORDLEGACY" + mode
	db.MustExec(`INSERT INTO orders (order_no,buyer_id,product_id,quantity,duration,unit_price,total_amount,platform_fee,status,stock_reserved,compliance_agreed)
		VALUES (?,102,?,1,?,?,?,0,'paid',1,1)`, no, id, duration, unit, unit*int64(duration))
	applyMigrationFile(t, db, "../../migrations/028_flexible_pricing.up.sql")
	return db, id, no
}

func deliverAndConfirm(t *testing.T, db *sqlx.DB, no string) (time.Time, time.Time) {
	t.Helper()
	service := compute.NewService(compute.NewRepository(db), db, strings.Repeat("12", 32))
	if _, err := service.DeliverWithAccess(101, no, compute.DeliverInfo{IpAddress: "192.0.2.10"}, false); err != nil {
		t.Fatal(err)
	}
	if err := service.ConfirmDelivery(102, no); err != nil {
		t.Fatal(err)
	}
	var lease struct {
		Start time.Time `db:"lease_start_at"`
		End   time.Time `db:"lease_end_at"`
	}
	if err := db.Get(&lease, "SELECT lease_start_at,lease_end_at FROM orders WHERE order_no=?", no); err != nil {
		t.Fatal(err)
	}
	return lease.Start, lease.End
}

// CR M1: 日租商品有一笔已付款待交付的 3 天存量订单; 供给方下架、加上小时价重提并审核通过后
// (展示主价变成 hourly), 交付租期仍是 3 天, 续租仍按日价报价——028 已把订单方式冻结为 daily。
func TestPricingLegacyOrderSurvivesProductRepricing(t *testing.T) {
	db, id, no := legacyPaidOrder(t, "daily", 60000, 3)
	if mode, _ := orderSnapshot(t, db, no); mode.String != "daily" {
		t.Fatalf("028 must freeze the legacy order's pricing mode: %v", mode)
	}
	resubmitPrices(t, db, id, []map[string]any{
		{"pricing_mode": "hourly", "unit_price": 2000},
		{"pricing_mode": "daily", "unit_price": 45000},
	})
	var display string
	db.Get(&display, "SELECT pricing_mode FROM products WHERE id=?", id)
	if display != "hourly" {
		t.Fatalf("scenario precondition: display mode should now be hourly, got %q", display)
	}

	start, end := deliverAndConfirm(t, db, no)
	if end.Sub(start) != 72*time.Hour {
		t.Fatalf("3-day legacy order must lease 72h after repricing, got %v", end.Sub(start))
	}
	var expires time.Time
	db.Get(&expires, "SELECT d.access_expires_at FROM order_deliveries d JOIN orders o ON o.id=d.order_id WHERE o.order_no=?", no)
	if !expires.Equal(end) {
		t.Fatalf("credential expiry must follow the lease: %v vs %v", expires, end)
	}
	buyer := tradeRouter(db, 102)
	quote := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+no+"/renewal-quote?duration=1", nil, 0).Data.(map[string]any)
	if quote["pricing_mode"] != "daily" || quote["unit_price"] != float64(45000) || quote["renewed_until"] != end.Add(24*time.Hour).Format(time.RFC3339) {
		t.Fatalf("renewal must be quoted at the daily price: %v", quote)
	}
	detail := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+no, nil, 0).Data.(map[string]any)
	if detail["product"].(map[string]any)["pricing_mode"] != "daily" || detail["actions"].(map[string]any)["can_renew"] != true {
		t.Fatalf("detail must show the order's own mode: %v", detail)
	}
	list := tradeRequest(t, buyer, "GET", "/api/v1/orders", nil, 0).Data.(map[string]any)["list"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["pricing_mode"] != "daily" {
		t.Fatalf("order list must expose the snapshot: %v", list)
	}
}

// CR M1: 快照缺失(028 后不应出现)时交付、签收、续租报价一律 50000, 不回退商品当前展示方式。
func TestPricingMissingOrderSnapshotFailsLoudly(t *testing.T) {
	db, _, no := legacyPaidOrder(t, "daily", 60000, 3)
	db.MustExec("UPDATE orders SET pricing_mode=NULL WHERE order_no=?", no)
	service := compute.NewService(compute.NewRepository(db), db, strings.Repeat("12", 32))
	_, err := service.DeliverWithAccess(101, no, compute.DeliverInfo{IpAddress: "192.0.2.10"}, false)
	if !errors.Is(err, compute.ErrOrderPricingModeMissing) || compute.ErrToCode(err) != 50000 {
		t.Fatalf("deliver must refuse a missing snapshot with 50000, got %v", err)
	}
	var status string
	db.Get(&status, "SELECT status FROM orders WHERE order_no=?", no)
	if status != "paid" {
		t.Fatalf("failed delivery must not change the order: %s", status)
	}

	// 交付/签收后再抹掉快照: 签收与续租报价同样拒绝。
	db.MustExec("UPDATE orders SET pricing_mode='daily' WHERE order_no=?", no)
	if _, err := service.DeliverWithAccess(101, no, compute.DeliverInfo{IpAddress: "192.0.2.10"}, false); err != nil {
		t.Fatal(err)
	}
	db.MustExec("UPDATE orders SET pricing_mode='' WHERE order_no=?", no)
	if err := service.ConfirmDelivery(102, no); !errors.Is(err, compute.ErrOrderPricingModeMissing) {
		t.Fatalf("confirm must refuse a missing snapshot, got %v", err)
	}
	db.MustExec("UPDATE orders SET pricing_mode='daily' WHERE order_no=?", no)
	if err := service.ConfirmDelivery(102, no); err != nil {
		t.Fatal(err)
	}
	db.MustExec("UPDATE orders SET pricing_mode=NULL WHERE order_no=?", no)
	buyer := tradeRouter(db, 102)
	res := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+no+"/renewal-quote?duration=1", nil, 50000)
	if !strings.Contains(res.Message, "计费方式快照") {
		t.Fatalf("unexpected message: %s", res.Message)
	}
	detail := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+no, nil, 0).Data.(map[string]any)
	if detail["product"].(map[string]any)["pricing_mode"] != "" || detail["actions"].(map[string]any)["can_renew"] != false {
		t.Fatalf("detail must not invent a pricing mode: %v", detail)
	}
}

// T-091: v1.2 前的存量包月零租订单经 028 回填(价格行 + 订单快照)后仍可交付、续租, 续租单写入快照。
func TestPricingLegacyMonthlyOrderRenewsAfterBackfill(t *testing.T) {
	db, id, no := legacyPaidOrder(t, "monthly", 1000000, 1)
	var rows int
	db.Get(&rows, "SELECT COUNT(*) FROM product_prices WHERE product_id=? AND pricing_mode='monthly' AND enabled=1", id)
	if rows != 1 {
		t.Fatalf("monthly card_rental must have a price row, got %d", rows)
	}
	start, end := deliverAndConfirm(t, db, no)
	if !end.Equal(start.AddDate(0, 1, 0)) {
		t.Fatalf("monthly lease wrong: %v - %v", start, end)
	}
	buyer := tradeRouter(db, 102)
	quote := tradeRequest(t, buyer, "GET", "/api/v1/orders/"+no+"/renewal-quote?duration=2", nil, 0).Data.(map[string]any)
	if quote["pricing_mode"] != "monthly" || quote["total_amount"] != float64(2000000) {
		t.Fatalf("legacy monthly renewal quote wrong: %v", quote)
	}
	child := tradeRequest(t, buyer, "POST", "/api/v1/orders/"+no+"/renew", renewalInput(quote, "0c7c3f4e-1d2b-4a5c-9e8f-7a6b5c4d3e2f"), 0).Data.(map[string]any)
	if mode, _ := orderSnapshot(t, db, child["order_no"].(string)); mode.String != "monthly" {
		t.Fatalf("renewal order snapshot missing: %v", mode)
	}
	list := tradeRequest(t, buyer, "GET", "/api/v1/orders", nil, 0).Data.(map[string]any)["list"].([]any)
	for _, item := range list {
		if item.(map[string]any)["pricing_mode"] != "monthly" {
			t.Fatalf("order list must expose the pricing snapshot: %v", item)
		}
	}
}

// 托管商品(fleet.enabled=false: 未注入 ManagedGate/LeaseHooks): 创建即草稿, 审核通过被 40001 拦下;
// 即使商品行被置为在售, 下单也明确 40001(S1/S2 门禁, 取代 S3 早期的 50000 占位), 不静默落成人工交付订单。
// fleet 启用后的托管下单(租约钩子 + 计费方式快照)见 fleet 包的租约集成测试。
func TestPricingManagedProductOrderFailsLoudly(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	input := tradeProductInput()
	input["fulfillment_mode"] = "managed"
	created := tradeRequest(t, tradeRouter(db, 101), "POST", "/api/v1/supplier/products", input, 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	var status string
	if err := db.Get(&status, "SELECT status FROM products WHERE id=?", id); err != nil || status != "draft" {
		t.Fatalf("managed product must start as draft, got %q (%v)", status, err)
	}
	tradeRequest(t, tradeRouter(db, 104), "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 40001)
	db.MustExec("UPDATE products SET status='active', stock=8 WHERE id=?", id)
	tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/orders", map[string]any{
		"product_id": id, "quantity": 1, "duration": 1, "compliance_agreed": true, "compliance_version": consentVersion}, 40001)
	var n int
	db.Get(&n, "SELECT COUNT(*) FROM orders")
	if n != 0 {
		t.Fatalf("managed order must not be created: %d", n)
	}
}
