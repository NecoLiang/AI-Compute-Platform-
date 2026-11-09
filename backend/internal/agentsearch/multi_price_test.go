package agentsearch

import (
	"strings"
	"testing"

	"tokenfactory/internal/compute"
)

// 零租多价格商品(展示主价 hourly, 另开放 daily): 需求 daily 时按日价匹配、估算与展示, 不只看展示主价。
func TestMatchProductsUsesEnabledPriceForRequestedMode(t *testing.T) {
	p := compute.Product{ID: 1, ProductType: compute.ProductTypeCardRental, GpuModel: "H100", CardCount: 8, Stock: 8,
		PricingMode: "hourly", UnitPrice: 2000, MinOrder: 1, MinDuration: 1,
		Prices: []compute.ProductPrice{
			{PricingMode: "hourly", UnitPrice: 2000, MinDuration: 1, Enabled: true},
			{PricingMode: "daily", UnitPrice: 40000, MinDuration: 2, Enabled: true},
		}}
	req := parsedRequirement{Relevant: true, GPUModels: []string{"H100"}, CardCount: 2, PricingMode: "daily", DurationHint: 1, BudgetFenMax: 160000}

	matches := matchProducts(req, []compute.Product{p})
	if len(matches) != 1 {
		t.Fatalf("daily demand must match a product offering a daily price: %+v", matches)
	}
	m := matches[0]
	reasons := strings.Join(m.Reasons, ";")
	// 型号40 + 库存20 + 预算20 + 计费10; 估算按日价 400 元 × 2 卡 × 最短 2 天 = 1600 元。
	if m.Score != 90 || !strings.Contains(reasons, "按 2卡、2天及商品最低采购条件估算") ||
		!strings.Contains(reasons, "预估费用 1600.00 元在预算内") || !strings.Contains(reasons, "计费模式匹配(daily, 单价 400.00 元/卡/天)") {
		t.Fatalf("daily price must drive score, estimate and reasons: %d %s", m.Score, reasons)
	}
	if m.Quote == nil || *m.Quote != (MatchQuote{PricingMode: "daily", UnitPrice: 40000, MinDuration: 2}) {
		t.Fatalf("quote must be the requested mode's price: %+v", m.Quote)
	}

	// 预算 1599 元: 按日价超预算(若误用小时价 20 元×2卡×2 会误判在预算内)。
	req.BudgetFenMax = 159900
	if r := strings.Join(matchProducts(req, []compute.Product{p})[0].Reasons, ";"); !strings.Contains(r, "可能超出预算") {
		t.Fatalf("budget must be compared at the daily price: %s", r)
	}

	// 未开放(或已停用)的方式不加计费分, 不做预算估算, quote 为空。
	req.PricingMode, req.BudgetFenMax = "weekly", 100000000
	p.Prices = append(p.Prices, compute.ProductPrice{PricingMode: "weekly", UnitPrice: 200000, MinDuration: 1, Enabled: false})
	m = matchProducts(req, []compute.Product{p})[0]
	if m.Score != 60 || m.Quote != nil || !strings.Contains(strings.Join(m.Reasons, ";"), "商品未开放需求的计费方式") {
		t.Fatalf("disabled/absent mode must not match: %d %+v %v", m.Score, m.Quote, m.Reasons)
	}

	// 未指定方式: 取展示主价。
	req.PricingMode, req.BudgetFenMax = "", 0
	m = matchProducts(req, []compute.Product{p})[0]
	if m.Quote == nil || m.Quote.PricingMode != "hourly" || m.Quote.UnitPrice != 2000 {
		t.Fatalf("no requested mode must quote the display price: %+v", m.Quote)
	}
}

// 非零租商品没有价格行, 回退商品主价方式, 与 compute.PriceFor 一致。
func TestMatchProductsNonCardRentalUsesProductMode(t *testing.T) {
	p := compute.Product{ID: 2, ProductType: "center", GpuModel: "H100", CardCount: 8, Stock: 4,
		PricingMode: "monthly", UnitPrice: 5000000, MinOrder: 1, MinDuration: 1}
	req := parsedRequirement{Relevant: true, GPUModels: []string{"H100"}, PricingMode: "monthly"}
	m := matchProducts(req, []compute.Product{p})[0]
	if m.Quote == nil || m.Quote.UnitPrice != 5000000 || !strings.Contains(strings.Join(m.Reasons, ";"), "计费模式匹配(monthly") {
		t.Fatalf("non card_rental must match on its own mode: %+v %v", m.Quote, m.Reasons)
	}
	req.PricingMode = "daily"
	m = matchProducts(req, []compute.Product{p})[0]
	if m.Quote != nil || strings.Contains(strings.Join(m.Reasons, ";"), "计费模式匹配") {
		t.Fatalf("non card_rental must not match another mode: %+v %v", m.Quote, m.Reasons)
	}
}
