package compute_test

import (
	"database/sql"
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

// T-091: v1.2 前的存量包月零租订单(pricing_mode=NULL)在 028 回填后仍可续租, 续租单写入快照。
func TestPricingLegacyMonthlyOrderRenewsAfterBackfill(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	input := tradeProductInput()
	input["pricing_mode"], input["unit_price"] = "monthly", 1000000
	id := approvedProduct(t, db, input)
	// 模拟 028 之前的数据: 订单无快照; 价格行与 028 回填结果一致(由迁移测试单独覆盖)。
	placed := tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/orders", map[string]any{
		"product_id": id, "quantity": 1, "duration": 1, "compliance_agreed": true, "compliance_version": consentVersion}, 0)
	no := placed.Data.(map[string]any)["order_no"].(string)
	db.MustExec("UPDATE orders SET pricing_mode=NULL WHERE order_no=?", no)
	var rows int
	db.Get(&rows, "SELECT COUNT(*) FROM product_prices WHERE product_id=? AND pricing_mode='monthly' AND enabled=1", id)
	if rows != 1 {
		t.Fatalf("monthly card_rental must have a price row, got %d", rows)
	}
	payDeliverConfirm(t, db, no, 1000000)
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
			t.Fatalf("order list must expose pricing mode (legacy falls back to product): %v", item)
		}
	}
}

// 托管商品下单: 托管履约钩子未接入时明确 50000, 不静默落成人工交付订单。
func TestPricingManagedProductOrderFailsLoudly(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	input := tradeProductInput()
	input["fulfillment_mode"] = "managed"
	id := approvedProduct(t, db, input)
	tradeRequest(t, tradeRouter(db, 102), "POST", "/api/v1/orders", map[string]any{
		"product_id": id, "quantity": 1, "duration": 1, "compliance_agreed": true, "compliance_version": consentVersion}, 50000)
	var n int
	db.Get(&n, "SELECT COUNT(*) FROM orders")
	if n != 0 {
		t.Fatalf("managed order must not be created: %d", n)
	}
}
