package compute_test

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"

	"tokenfactory/internal/compute"
)

type priceRow struct {
	Mode        string `db:"pricing_mode"`
	UnitPrice   int64  `db:"unit_price"`
	MinDuration int    `db:"min_duration"`
	Enabled     bool   `db:"enabled"`
}

func productPriceRows(t *testing.T, db *sqlx.DB, id int64) map[string]priceRow {
	t.Helper()
	var rows []priceRow
	if err := db.Select(&rows, "SELECT pricing_mode,unit_price,min_duration,enabled FROM product_prices WHERE product_id=?", id); err != nil {
		t.Fatal(err)
	}
	out := map[string]priceRow{}
	for _, r := range rows {
		out[r.Mode] = r
	}
	return out
}

func multiPriceProductInput() map[string]any {
	input := tradeProductInput()
	delete(input, "pricing_mode")
	delete(input, "unit_price")
	input["prices"] = []map[string]any{
		{"pricing_mode": "monthly", "unit_price": 1500000, "min_duration": 1},
		{"pricing_mode": "hourly", "unit_price": 2300},
		{"pricing_mode": "daily", "unit_price": 60000, "min_duration": 2}, // > 24×2300=55200, 倒挂提醒
	}
	return input
}

// T-090: 多价格发布、倒挂提醒不阻断、价格行持久化; 重提时未提交的方式置停用而非删除; 市场只展示已启用价格。
func TestPricingMultiPriceProductLifecycle(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	supplier, admin := tradeRouter(db, 101), tradeRouter(db, 104)
	public := gin.New()
	compute.NewHandler(compute.NewService(compute.NewRepository(db), db)).RegisterMarketRoutes(public.Group("/api/v1"))

	created := tradeRequest(t, supplier, "POST", "/api/v1/supplier/products", multiPriceProductInput(), 0)
	data := created.Data.(map[string]any)
	id := int64(data["id"].(float64))
	warnings := data["warnings"].([]any)
	if len(warnings) != 1 || warnings[0].(map[string]any)["code"] != "daily_above_24_hourly" {
		t.Fatalf("want one inversion warning, got %v", warnings)
	}
	rows := productPriceRows(t, db, id)
	if len(rows) != 3 || rows["daily"].MinDuration != 2 || rows["hourly"].MinDuration != 1 || !rows["monthly"].Enabled {
		t.Fatalf("prices not persisted: %+v", rows)
	}
	var display struct {
		Mode string `db:"pricing_mode"`
		Unit int64  `db:"unit_price"`
		FM   string `db:"fulfillment_mode"`
		Av   string `db:"availability"`
	}
	if err := db.Get(&display, "SELECT pricing_mode,unit_price,fulfillment_mode,availability FROM products WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	if display.Mode != "hourly" || display.Unit != 2300 || display.FM != "manual" || display.Av != "fulltime" {
		t.Fatalf("display price not synced to shortest enabled period: %+v", display)
	}

	// 重提: 去掉 hourly → hourly 行保留但停用; 展示主价改为 daily。
	tradeRequest(t, admin, "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/reject", id), map[string]string{"reason": "改价"}, 0)
	input := multiPriceProductInput()
	input["prices"] = []map[string]any{
		{"pricing_mode": "daily", "unit_price": 50000},
		{"pricing_mode": "monthly", "unit_price": 1400000},
	}
	resub := tradeRequest(t, supplier, "PUT", fmt.Sprintf("/api/v1/supplier/products/%d", id), input, 0)
	if w := resub.Data.(map[string]any)["warnings"].([]any); len(w) != 0 {
		t.Fatalf("unexpected warnings: %v", w)
	}
	rows = productPriceRows(t, db, id)
	if len(rows) != 3 || rows["hourly"].Enabled || !rows["daily"].Enabled || rows["daily"].UnitPrice != 50000 || rows["daily"].MinDuration != 1 {
		t.Fatalf("resubmit did not upsert/disable: %+v", rows)
	}
	tradeRequest(t, admin, "POST", fmt.Sprintf("/api/v1/admin/audits/products/%d/approve", id), nil, 0)
	got := tradeRequest(t, public, "GET", fmt.Sprintf("/api/v1/products/%d", id), nil, 0)
	product := got.Data.(map[string]any)["product"].(map[string]any)
	prices := product["prices"].([]any)
	if product["pricing_mode"] != "daily" || len(prices) != 2 || prices[0].(map[string]any)["pricing_mode"] != "daily" || prices[1].(map[string]any)["pricing_mode"] != "monthly" {
		t.Fatalf("market must show enabled prices ordered by period: %v", product)
	}
	market := tradeRequest(t, public, "GET", "/api/v1/products", nil, 0)
	item := market.Data.(map[string]any)["list"].([]any)[0].(map[string]any)
	if len(item["prices"].([]any)) != 2 {
		t.Fatalf("market list leaked disabled price: %v", item["prices"])
	}
	own := tradeRequest(t, supplier, "GET", "/api/v1/supplier/products", nil, 0)
	if len(own.Data.([]any)[0].(map[string]any)["prices"].([]any)) != 3 {
		t.Fatalf("supplier should see disabled prices too: %v", own.Data)
	}
}

// T-090: 旧前端只传顶层字段仍可发布, 自动生成一行价格; 组合约束一律 40001。
func TestPricingLegacyInputAndCombinationRules(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	supplier := tradeRouter(db, 101)
	created := tradeRequest(t, supplier, "POST", "/api/v1/supplier/products", tradeProductInput(), 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	if w := created.Data.(map[string]any)["warnings"].([]any); len(w) != 0 {
		t.Fatalf("legacy input should not warn: %v", w)
	}
	rows := productPriceRows(t, db, id)
	if len(rows) != 1 || rows["hourly"].UnitPrice != 2300 || !rows["hourly"].Enabled {
		t.Fatalf("legacy input must create one price row: %+v", rows)
	}

	bad := func(mutate func(map[string]any)) {
		t.Helper()
		in := tradeProductInput()
		mutate(in)
		tradeRequest(t, supplier, "POST", "/api/v1/supplier/products", in, 40001)
	}
	bad(func(in map[string]any) { in["pricing_mode"] = "nightly" })                                      // 全天商品不能包夜
	bad(func(in map[string]any) { in["availability"] = "night_window"; in["pricing_mode"] = "nightly" }) // 夜间窗口须托管
	bad(func(in map[string]any) {
		in["prices"] = []map[string]any{{"pricing_mode": "nightly", "unit_price": 1}, {"pricing_mode": "daily", "unit_price": 1}}
	})
	bad(func(in map[string]any) {
		in["prices"] = []map[string]any{{"pricing_mode": "daily", "unit_price": 1, "enabled": false}}
	})
	bad(func(in map[string]any) {
		in["product_type"], in["pricing_mode"] = "center", "monthly"
		in["prices"] = []map[string]any{{"pricing_mode": "monthly", "unit_price": 1}}
	})
	var count int
	db.Get(&count, "SELECT COUNT(*) FROM products")
	if count != 1 {
		t.Fatalf("rejected products must not be written, got %d", count)
	}
}

// T-090: 028 回填前核对——零租商品出现不支持的计费方式时迁移在任何 DDL 之前失败;
// 正常数据回填全部零租(含 monthly); 028.down 无条件拒绝。
func TestMigration028BackfillGuardAndDownRefuses(t *testing.T) {
	db := setupTradeDB(t) // 已执行到 028
	// setupTradeDB 跑完全链; 另起一个只执行 028 之前迁移的库验证核对与回填。
	rebuild := func(t *testing.T) *sqlx.DB {
		t.Helper()
		var name string
		db.Get(&name, "SELECT DATABASE()")
		fresh := name + "_m028"
		db.MustExec("CREATE DATABASE " + fresh)
		t.Cleanup(func() { db.Exec("DROP DATABASE " + fresh) })
		dsn := strings.Replace(os.Getenv("TEST_MYSQL_DSN"), "/?", "/"+fresh+"?", 1)
		other, err := sqlx.Connect("mysql", dsn)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { other.Close() })
		paths, _ := filepath.Glob("../../migrations/*.up.sql")
		for _, p := range paths {
			if filepath.Base(p) >= "028" {
				continue
			}
			data, _ := os.ReadFile(p)
			if _, err := other.Exec(string(data)); err != nil {
				t.Fatalf("%s: %v", p, err)
			}
		}
		return other
	}
	up, _ := os.ReadFile("../../migrations/028_flexible_pricing.up.sql")
	down, _ := os.ReadFile("../../migrations/028_flexible_pricing.down.sql")

	t.Run("bad card_rental pricing aborts before DDL", func(t *testing.T) {
		other := rebuild(t)
		other.MustExec(`INSERT INTO users (id,phone,password_hash) VALUES (1,'18800000001','')`)
		other.MustExec(`INSERT INTO products (supplier_id,product_type,gpu_model,pricing_mode,unit_price,stock,region) VALUES
			(1,'card_rental','H100','perpetual',100,1,'北京')`)
		_, err := other.Exec(string(up))
		if err == nil || !strings.Contains(err.Error(), "m028_card_rental_unsupported_pricing_mode") {
			t.Fatalf("want guard violation, got %v", err)
		}
		var n int
		other.Get(&n, "SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='product_prices'")
		if n != 0 {
			t.Fatal("DDL ran despite guard failure")
		}
	})
	t.Run("backfill covers every card_rental product including monthly", func(t *testing.T) {
		other := rebuild(t)
		other.MustExec(`INSERT INTO users (id,phone,password_hash) VALUES (1,'18800000001','')`)
		other.MustExec(`INSERT INTO products (supplier_id,product_type,gpu_model,pricing_mode,unit_price,stock,region,min_duration) VALUES
			(1,'card_rental','H100','hourly',100,1,'北京',NULL),(1,'card_rental','H100','daily',2000,1,'北京',0),
			(1,'card_rental','H100','weekly',9000,1,'北京',2),(1,'card_rental','H100','monthly',30000,1,'北京',3),
			(1,'center','H100','monthly',30000,1,'北京',1),(1,'outright','H100','perpetual',30000,1,'北京',1)`)
		if _, err := other.Exec(string(up)); err != nil {
			t.Fatal(err)
		}
		var rows []struct {
			Mode string `db:"pricing_mode"`
			Unit int64  `db:"unit_price"`
			Min  int    `db:"min_duration"`
		}
		if err := other.Select(&rows, "SELECT pricing_mode,unit_price,min_duration FROM product_prices ORDER BY product_id"); err != nil {
			t.Fatal(err)
		}
		want := fmt.Sprint([]any{"hourly", 100, 1, "daily", 2000, 1, "weekly", 9000, 2, "monthly", 30000, 3})
		var got []any
		for _, r := range rows {
			got = append(got, r.Mode, r.Unit, r.Min)
		}
		if fmt.Sprint(got) != want {
			t.Fatalf("backfill mismatch: %v", got)
		}
	})
	t.Run("down refuses", func(t *testing.T) {
		_, err := db.Exec(string(down))
		if err == nil || !strings.Contains(err.Error(), "45000") {
			t.Fatalf("028 down must refuse, got %v", err)
		}
		var n int
		db.Get(&n, "SELECT COUNT(*) FROM information_schema.columns WHERE table_schema=DATABASE() AND table_name='orders' AND column_name='pricing_mode'")
		if n != 1 {
			t.Fatal("down must not drop columns")
		}
	})
}
