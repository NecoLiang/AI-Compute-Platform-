package compute_test

import (
	"sort"
	"testing"
)

// T-093: 市场列表 pricing_mode 按已启用价格行(任一行)匹配, 非零租/无价格行的存量零租回落 products.pricing_mode;
// min_cards 托管按 max_free_per_node、人工零租按 stock、买断/算力中心按 card_count。列表带出 v1.2 字段。
func TestMarketListPricingModeAndMinCardsFilters(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	db.MustExec(`INSERT INTO products (id, supplier_id, product_type, gpu_model, card_count, delivery_mode, fulfillment_mode,
		pricing_mode, unit_price, stock, max_free_per_node, card_split_options, status, region) VALUES
		(501, 101, 'card_rental', 'H100', 8, 'bare_metal', 'manual',  'hourly',    2300,    8, NULL, NULL,    'active', '北京'),
		(502, 101, 'card_rental', 'H100', 8, 'bare_metal', 'manual',  'daily',     50000,   2, NULL, NULL,    'active', '北京'),
		(503, 101, 'outright',    'H100', 8, 'bare_metal', 'manual',  'perpetual', 9000000, 1, NULL, NULL,    'active', '北京'),
		(504, 101, 'card_rental', 'H100', 8, 'container',  'managed', 'hourly',    1500,   12, 4,    '1,2,4', 'active', '北京'),
		(505, 101, 'center',      'H100', 2, 'bare_metal', 'manual',  'monthly',   800000,  1, NULL, NULL,    'active', '北京'),
		(506, 101, 'card_rental', 'H100', 8, 'bare_metal', 'manual',  'hourly',    2300,    8, NULL, NULL,    'offline', '北京')`)
	// 501: hourly 主价 + daily 启用 + monthly 已停用; 504: 仅 hourly; 506 下架不出现。
	db.MustExec(`INSERT INTO product_prices (product_id, pricing_mode, unit_price, min_duration, enabled) VALUES
		(501, 'hourly', 2300, 1, 1), (501, 'daily', 50000, 1, 1), (501, 'monthly', 1500000, 1, 0),
		(504, 'hourly', 1500, 1, 1), (506, 'daily', 50000, 1, 1)`)

	buyer := tradeRouter(db, 102)
	ids := func(query string) []int {
		t.Helper()
		res := tradeRequest(t, buyer, "GET", "/api/v1/products?page_size=100&"+query, nil, 0)
		list, _ := res.Data.(map[string]any)["list"].([]any)
		out := []int{}
		for _, it := range list {
			out = append(out, int(it.(map[string]any)["id"].(float64)))
		}
		sort.Ints(out)
		return out
	}
	eq := func(name string, got, want []int) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %v want %v", name, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: got %v want %v", name, got, want)
			}
		}
	}
	eq("all", ids(""), []int{501, 502, 503, 504, 505})
	eq("daily", ids("pricing_mode=daily"), []int{501, 502}) // 501 非主价方式也匹配; 502 存量无价格行回落主价
	eq("hourly", ids("pricing_mode=hourly"), []int{501, 504})
	eq("monthly", ids("pricing_mode=monthly"), []int{505}) // 501 的 monthly 已停用
	eq("perpetual", ids("pricing_mode=perpetual"), []int{503})
	eq("min_cards=4", ids("min_cards=4"), []int{501, 503, 504})
	eq("min_cards=5", ids("min_cards=5"), []int{501, 503}) // 托管 504 单机最多 4 张; 502 库存 2
	eq("min_cards=2", ids("min_cards=2"), []int{501, 502, 503, 504, 505})
	eq("combined", ids("pricing_mode=hourly&min_cards=5"), []int{501})
	tradeRequest(t, buyer, "GET", "/api/v1/products?min_cards=-1", nil, 40001)

	res := tradeRequest(t, buyer, "GET", "/api/v1/products?pricing_mode=hourly&min_cards=4&page_size=100", nil, 0)
	for _, it := range res.Data.(map[string]any)["list"].([]any) {
		p := it.(map[string]any)
		for _, k := range []string{"prices", "fulfillment_mode", "availability", "card_split_options", "max_free_per_node"} {
			if _, ok := p[k]; !ok {
				t.Fatalf("list item missing %s: %v", k, p)
			}
		}
		if p["id"] == float64(504) {
			opts := p["card_split_options"].([]any)
			if p["fulfillment_mode"] != "managed" || p["max_free_per_node"] != float64(4) || len(opts) != 3 || opts[2] != float64(4) {
				t.Fatalf("managed list item wrong: %v", p)
			}
		}
		if p["id"] == float64(501) && (p["card_split_options"] != nil || p["max_free_per_node"] != nil || len(p["prices"].([]any)) != 2) {
			t.Fatalf("manual list item wrong: %v", p)
		}
	}
	detail := tradeRequest(t, buyer, "GET", "/api/v1/products/504", nil, 0).Data.(map[string]any)["product"].(map[string]any)
	if detail["max_free_per_node"] != float64(4) || len(detail["card_split_options"].([]any)) != 3 {
		t.Fatalf("detail must carry split fields: %v", detail)
	}
}
