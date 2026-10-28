package compute_test

import (
	"context"
	"fmt"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"

	"tokenfactory/internal/compute"
)

// availabilityRouter 市场路由 + 已装配限流的服务(main 中 SetRateLimiter 的等价)。
func availabilityRouter(t *testing.T, db *sqlx.DB, rdb *redis.Client, id int64) *gin.Engine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	svc := compute.NewService(compute.NewRepository(db), db)
	svc.SetRateLimiter(rdb)
	r := gin.New()
	g := r.Group("/api/v1", userID(id))
	h := compute.NewHandler(svc)
	h.RegisterMarketRoutes(g)
	h.RegisterBuyerRoutes(g)
	return r
}

// T-093: 人工商品 availability —— 报价与 POST /orders 一致(含平台费), 起算规则为买家签收, 库存不足给原因, 参数错误 40001。
func TestManualAvailabilityQuoteMatchesOrder(t *testing.T) {
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR required")
	}
	db := setupTradeDB(t)
	seedTradeUsers(db)
	rdb := redis.NewClient(&redis.Options{Addr: addr, DB: 13})
	ctx := context.Background()
	rdb.Del(ctx, "market:availability:rl:102")
	t.Cleanup(func() { rdb.Del(ctx, "market:availability:rl:102"); rdb.Close() })
	db.MustExec("UPDATE system_config SET config_value='650' WHERE config_key='fee_rate'")

	id := approvedProduct(t, db, multiPriceProductInput()) // 人工 bare_metal, stock 8; hourly 2300 / daily 60000 min2 / monthly 1500000
	buyer := availabilityRouter(t, db, rdb, 102)
	path := func(q string) string { return fmt.Sprintf("/api/v1/products/%d/availability?%s", id, q) }

	a := tradeRequest(t, buyer, "GET", path("pricing_mode=daily&cards=2&duration=2"), nil, 0).Data.(map[string]any)
	quote := a["quote"].(map[string]any)
	lp := a["lease_preview"].(map[string]any)
	if a["available"] != true || a["reason"] != "" || quote["pricing_mode"] != "daily" || quote["unit_price"] != float64(60000) ||
		quote["total_amount"] != float64(240000) || quote["fee_rate"] != float64(650) ||
		lp["start_rule"] != compute.LeaseStartOnBuyerConfirm || lp["start_at"] != nil || lp["end_at"] != nil {
		t.Fatalf("unexpected availability: %v", a)
	}
	order := tradeRequest(t, buyer, "POST", "/api/v1/orders", map[string]any{"product_id": id, "pricing_mode": "daily", "quantity": 2, "duration": 2,
		"compliance_agreed": true, "compliance_version": consentVersion}, 0).Data.(map[string]any)
	if order["total_amount"] != quote["total_amount"] || order["platform_fee"] != quote["platform_fee"] || quote["platform_fee"] != float64(15600) {
		t.Fatalf("quote must equal order amounts: quote=%v order=%v", quote, order)
	}

	// 库存剩 6: 7 张给原因与报价, 不报错。
	short := tradeRequest(t, buyer, "GET", path("cards=7&duration=1"), nil, 0).Data.(map[string]any)
	if short["available"] != false || short["reason"] != "库存不足: 当前可租 6" || short["quote"].(map[string]any)["pricing_mode"] != "hourly" {
		t.Fatalf("stock shortage must be a reason: %v", short)
	}
	tradeRequest(t, buyer, "GET", path("pricing_mode=weekly&cards=1&duration=1"), nil, 40001) // 未开放
	tradeRequest(t, buyer, "GET", path("pricing_mode=daily&cards=1&duration=1"), nil, 40001)  // 日价最短 2 天
	tradeRequest(t, buyer, "GET", path("duration=1"), nil, 40001)                             // cards 必填
	tradeRequest(t, buyer, "GET", "/api/v1/products/999999/availability?cards=1&duration=1", nil, 40400)

	// 交易总开关关闭: available=false, 仍给报价。
	db.MustExec("UPDATE system_config SET config_value='false' WHERE config_key='trading_enabled'")
	off := tradeRequest(t, buyer, "GET", path("cards=1&duration=1"), nil, 0).Data.(map[string]any)
	if off["available"] != false || off["reason"] == "" || off["quote"] == nil {
		t.Fatalf("trading disabled must block: %v", off)
	}
}
