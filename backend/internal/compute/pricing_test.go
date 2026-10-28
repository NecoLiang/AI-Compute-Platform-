package compute

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"tokenfactory/pkg/errcode"
)

func boolPtr(v bool) *bool { return &v }

func price(mode string, unit int64) PriceInput { return PriceInput{PricingMode: mode, UnitPrice: unit} }

func enabledRow(mode string, unit int64) ProductPrice {
	return ProductPrice{PricingMode: mode, UnitPrice: unit, MinDuration: 1, Enabled: true}
}

// ===== PriceFor =====

func TestPriceFor(t *testing.T) {
	t.Run("非零租商品无价格行回退商品单价", func(t *testing.T) {
		p := &Product{ProductType: ProductTypeCenter, PricingMode: PricingMonthly, UnitPrice: 900, MinDuration: 3}
		q, err := PriceFor(p, "")
		require.NoError(t, err)
		assert.Equal(t, PriceQuote{PricingMode: PricingMonthly, UnitPrice: 900, MinDuration: 3}, q)
		_, err = PriceFor(p, PricingDaily)
		assert.ErrorIs(t, err, ErrPricingModeDisabled, "回退只覆盖展示主价方式")
	})
	t.Run("零租无价格行(未回填)回退展示主价, min_duration 至少 1", func(t *testing.T) {
		p := &Product{ProductType: ProductTypeCardRental, PricingMode: PricingHourly, UnitPrice: 3500, MinDuration: 0}
		q, err := PriceFor(p, PricingHourly)
		require.NoError(t, err)
		assert.Equal(t, PriceQuote{PricingMode: PricingHourly, UnitPrice: 3500, MinDuration: 1}, q)
	})
	t.Run("零租有价格行时以价格行为准, 不看商品顶层单价", func(t *testing.T) {
		p := &Product{ProductType: ProductTypeCardRental, PricingMode: PricingHourly, UnitPrice: 1,
			Prices: []ProductPrice{enabledRow(PricingHourly, 3500), {PricingMode: PricingMonthly, UnitPrice: 2000000, MinDuration: 2, Enabled: true}}}
		q, err := PriceFor(p, "")
		require.NoError(t, err)
		assert.Equal(t, int64(3500), q.UnitPrice)
		q, err = PriceFor(p, PricingMonthly)
		require.NoError(t, err)
		assert.Equal(t, PriceQuote{PricingMode: PricingMonthly, UnitPrice: 2000000, MinDuration: 2}, q)
	})
	t.Run("已停用或未开放的方式报 ErrPricingModeDisabled", func(t *testing.T) {
		p := &Product{ProductType: ProductTypeCardRental, PricingMode: PricingDaily, UnitPrice: 80000,
			Prices: []ProductPrice{enabledRow(PricingDaily, 80000), {PricingMode: PricingWeekly, UnitPrice: 500000, MinDuration: 1}}}
		_, err := PriceFor(p, PricingWeekly)
		assert.ErrorIs(t, err, ErrPricingModeDisabled)
		_, err = PriceFor(p, PricingHourly)
		assert.ErrorIs(t, err, ErrPricingModeDisabled)
		assert.Equal(t, errcode.Conflict, ErrToCode(err), "续租遇停用方式应为 40900")
	})
	t.Run("nil 商品", func(t *testing.T) {
		_, err := PriceFor(nil, "")
		assert.Error(t, err)
	})
}

// ===== 价格倒挂提醒全组合 =====

func TestPriceWarnings(t *testing.T) {
	codes := func(rows ...ProductPrice) []string {
		out := []string{}
		for _, w := range PriceWarnings(rows) {
			out = append(out, w.Code)
		}
		return out
	}
	h, d, w, m := PricingHourly, PricingDaily, PricingWeekly, PricingMonthly
	assert.NotNil(t, PriceWarnings(nil), "无提醒时也必须是空数组, 便于 JSON 输出 []")
	assert.Empty(t, codes(enabledRow(h, 100), enabledRow(d, 2400), enabledRow(w, 16800), enabledRow(m, 72000)), "恰好等于不算倒挂")
	assert.Equal(t, []string{"daily_above_24_hourly"}, codes(enabledRow(h, 100), enabledRow(d, 2401)))
	assert.Equal(t, []string{"weekly_above_7_daily"}, codes(enabledRow(d, 100), enabledRow(w, 701)))
	assert.Equal(t, []string{"monthly_above_30_daily"}, codes(enabledRow(d, 100), enabledRow(m, 3001)))
	assert.Equal(t, []string{"monthly_above_4_weekly"}, codes(enabledRow(w, 100), enabledRow(m, 401)), "未设日价时月价对比 4 周")
	assert.Empty(t, codes(enabledRow(d, 100), enabledRow(w, 700), enabledRow(m, 3000)), "有日价时不再用周价比较月价")
	assert.Empty(t, codes(enabledRow(h, 100), enabledRow(w, 999999)), "缺日价时不比较周价")
	assert.Equal(t, []string{"daily_above_24_hourly", "weekly_above_7_daily", "monthly_above_30_daily"},
		codes(enabledRow(h, 1), enabledRow(d, 25), enabledRow(w, 176), enabledRow(m, 751)))
	disabledDaily := ProductPrice{PricingMode: d, UnitPrice: 999999, MinDuration: 1}
	assert.Empty(t, codes(enabledRow(h, 1), disabledDaily), "停用价格不参与提醒")
	assert.Empty(t, codes(enabledRow(PricingNightly, 1)))
}

// ===== 归一化: 展示主价同步、旧字段兼容、夜间窗口默认值 =====

func TestNormalizeProductReq_Prices(t *testing.T) {
	t.Run("旧前端只传顶层字段: 生成一行价格", func(t *testing.T) {
		req := NormalizeProductReq(validCardRentalReq())
		require.Len(t, req.Prices, 1)
		assert.Equal(t, PricingHourly, req.Prices[0].PricingMode)
		assert.Equal(t, int64(3500), req.Prices[0].UnitPrice)
		assert.Equal(t, 1, *req.Prices[0].MinDuration)
		assert.Equal(t, FulfillmentManual, req.FulfillmentMode)
		assert.Equal(t, AvailabilityFulltime, req.Availability)
	})
	t.Run("传 prices: 顶层字段被忽略并同步为已启用价格中周期最短的一项", func(t *testing.T) {
		r := validCardRentalReq()
		r.PricingMode, r.UnitPrice, r.MinDuration = PricingMonthly, 1, 9
		md := 3
		r.Prices = []PriceInput{price(PricingMonthly, 2000000), {PricingMode: PricingHourly, UnitPrice: 3500, Enabled: boolPtr(false)},
			{PricingMode: PricingDaily, UnitPrice: 80000, MinDuration: &md}}
		req := NormalizeProductReq(r)
		assert.Equal(t, PricingDaily, req.PricingMode, "停用的小时价不能成为展示主价")
		assert.Equal(t, int64(80000), req.UnitPrice)
		assert.Equal(t, 3, req.MinDuration)
		assert.NoError(t, ValidateProductReq(req))
		rows := priceRowsOf(req.Prices)
		assert.Equal(t, []string{PricingHourly, PricingDaily, PricingMonthly}, []string{rows[0].PricingMode, rows[1].PricingMode, rows[2].PricingMode})
		assert.False(t, rows[0].Enabled)
	})
	t.Run("夜间窗口默认 22:00-08:00 且 available_hours 由系统生成", func(t *testing.T) {
		r := validCardRentalReq()
		r.PricingMode, r.Availability, r.FulfillmentMode, r.AvailableHours = "", AvailabilityNightWindow, FulfillmentManaged, "随便写"
		r.DeliveryMode = "container" // 托管仅支持 container/bare_metal 交付(S1)
		req := NormalizeProductReq(r)
		assert.Equal(t, DefaultNightStartMin, *req.NightStartMin)
		assert.Equal(t, DefaultNightEndMin, *req.NightEndMin)
		assert.Equal(t, "22:00-08:00", req.AvailableHours)
		assert.Equal(t, PricingNightly, req.PricingMode)
		assert.NoError(t, ValidateProductReq(req))
	})
}

// ===== 组合约束 =====

func TestValidateCardRentalPricing(t *testing.T) {
	night := func(r *CreateProductReq) {
		r.Availability, r.FulfillmentMode, r.DeliveryMode = AvailabilityNightWindow, FulfillmentManaged, "container"
		r.Prices = []PriceInput{price(PricingNightly, 20000)}
	}
	cases := []struct {
		name    string
		mutate  func(*CreateProductReq)
		wantErr string
	}{
		{"全天四种方式同时开放", func(r *CreateProductReq) {
			r.Prices = []PriceInput{price(PricingHourly, 3500), price(PricingDaily, 80000), price(PricingWeekly, 500000), price(PricingMonthly, 2000000)}
		}, ""},
		{"至少启用一种", func(r *CreateProductReq) {
			r.Prices = []PriceInput{{PricingMode: PricingHourly, UnitPrice: 3500, Enabled: boolPtr(false)}}
		}, "至少需要启用一种"},
		{"方式重复", func(r *CreateProductReq) { r.Prices = []PriceInput{price(PricingDaily, 1), price(PricingDaily, 2)} }, "重复"},
		{"价格行单价为 0", func(r *CreateProductReq) { r.Prices = []PriceInput{price(PricingDaily, 0)} }, "单价必须大于 0"},
		{"停用的价格行也要合法单价", func(r *CreateProductReq) {
			r.Prices = []PriceInput{price(PricingHourly, 1), {PricingMode: PricingDaily, UnitPrice: -5, Enabled: boolPtr(false)}}
		}, "单价必须大于 0"},
		{"min_duration 显式 0", func(r *CreateProductReq) {
			r.Prices = []PriceInput{{PricingMode: PricingDaily, UnitPrice: 1, MinDuration: intPtr(0)}}
		}, "最短租期必须大于等于 1"},
		{"min_duration 超上限", func(r *CreateProductReq) {
			r.Prices = []PriceInput{{PricingMode: PricingMonthly, UnitPrice: 1, MinDuration: intPtr(121)}}
		}, "最短租期超出上限"},
		{"永久不适用零租", func(r *CreateProductReq) { r.Prices = []PriceInput{price(PricingPerpetual, 1)} }, "不适用于该商品类型"},
		{"单价×卡数×周期上限溢出", func(r *CreateProductReq) {
			// stock=16, hourly 上限 87600: 1e12/16/87600 = 713470
			r.Prices = []PriceInput{price(PricingHourly, 713471)}
		}, "单价过高"},
		{"溢出边界值本身合法", func(r *CreateProductReq) { r.Prices = []PriceInput{price(PricingHourly, 713470)} }, ""},
		{"包夜合法: 夜间窗口 + 托管", night, ""},
		{"包夜不能与其他方式并存", func(r *CreateProductReq) {
			night(r)
			r.Prices = append(r.Prices, price(PricingDaily, 1))
		}, "不能与其他计费方式同时开放"},
		{"包夜要求夜间窗口", func(r *CreateProductReq) {
			night(r)
			r.Availability = AvailabilityFulltime
		}, "night_window"},
		{"夜间窗口要求托管", func(r *CreateProductReq) {
			night(r)
			r.FulfillmentMode = FulfillmentManual
		}, "仅限平台托管"},
		{"夜间窗口只能卖包夜", func(r *CreateProductReq) {
			night(r)
			r.Prices = []PriceInput{price(PricingDaily, 1)}
		}, "仅支持包夜"},
		{"夜间窗口 30 分钟对齐", func(r *CreateProductReq) {
			night(r)
			r.NightStartMin = intPtr(1325)
		}, "30 分钟对齐"},
		{"夜间窗口过短(3h)", func(r *CreateProductReq) {
			night(r)
			r.NightStartMin, r.NightEndMin = intPtr(0), intPtr(180)
		}, "4-14 小时"},
		{"夜间窗口过长(15h)", func(r *CreateProductReq) {
			night(r)
			r.NightStartMin, r.NightEndMin = intPtr(1080), intPtr(540)
		}, "4-14 小时"},
		{"夜间窗口起止相同=24h", func(r *CreateProductReq) {
			night(r)
			r.NightStartMin, r.NightEndMin = intPtr(600), intPtr(600)
		}, "4-14 小时"},
		{"夜间窗口越界", func(r *CreateProductReq) {
			night(r)
			r.NightStartMin = intPtr(1440)
		}, "0-1439"},
		{"不跨零点窗口 01:00-07:00 合法", func(r *CreateProductReq) {
			night(r)
			r.NightStartMin, r.NightEndMin = intPtr(60), intPtr(420)
		}, ""},
		{"全天商品不能带夜间时段", func(r *CreateProductReq) { r.NightStartMin = intPtr(1320) }, "仅夜间窗口商品"},
		{"履约模式非法", func(r *CreateProductReq) { r.FulfillmentMode = "auto" }, "履约模式非法"},
		{"可用时段非法", func(r *CreateProductReq) { r.Availability = "weekend" }, "可用时段类型非法"},
		{"零租托管全天商品合法", func(r *CreateProductReq) { r.FulfillmentMode, r.DeliveryMode = FulfillmentManaged, "container" }, ""},
		{"零租托管交付方式不支持", func(r *CreateProductReq) { r.FulfillmentMode, r.DeliveryMode = FulfillmentManaged, "vm" }, "container"},
		{"非零租不能传 prices", func(r *CreateProductReq) {
			r.ProductType, r.PricingMode, r.MachineCount, r.TotalPflopsApprox, r.CardCount = ProductTypeCenter, PricingMonthly, 64, "约128P", 512
			r.Prices = []PriceInput{price(PricingMonthly, 1)}
		}, "仅零租商品支持多计费方式"},
		{"非零租不能托管", func(r *CreateProductReq) {
			r.ProductType, r.PricingMode, r.MachineCount, r.TotalPflopsApprox, r.CardCount = ProductTypeCenter, PricingMonthly, 64, "约128P", 512
			r.FulfillmentMode = FulfillmentManaged
		}, "平台托管仅适用于零租"},
		{"非零租不能夜间窗口", func(r *CreateProductReq) {
			r.ProductType, r.PricingMode, r.MachineCount, r.TotalPflopsApprox, r.CardCount = ProductTypeCenter, PricingMonthly, 64, "约128P", 512
			r.Availability = AvailabilityNightWindow
		}, "夜间窗口仅适用于零租"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validCardRentalReq()
			tc.mutate(&req)
			err := ValidateProductReq(NormalizeProductReq(req))
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
			assert.Equal(t, errcode.ParamInvalid, ErrToCode(err), "组合约束一律 40001")
		})
	}
}

// ===== 包夜租期换算与 night_date 归属 =====

func TestLeaseEndAtNightly(t *testing.T) {
	sh := shanghaiLoc()
	at := func(y int, m time.Month, d, hh, mm int) time.Time { return time.Date(y, m, d, hh, mm, 0, 0, sh) }
	cross := &NightWindow{StartMin: 1320, EndMin: 480} // 22:00-08:00
	early := &NightWindow{StartMin: 60, EndMin: 420}   // 01:00-07:00 不跨零点

	cases := []struct {
		name  string
		start time.Time
		win   *NightWindow
		n     int
		want  time.Time
	}{
		{"跨零点 1 晚, 窗口开始时起算", at(2026, 10, 28, 22, 0), cross, 1, at(2026, 10, 29, 8, 0)},
		{"跨零点 3 晚", at(2026, 10, 28, 22, 0), cross, 3, at(2026, 10, 31, 8, 0)},
		{"跨零点凌晨开通属于前一晚", at(2026, 10, 29, 3, 0), cross, 1, at(2026, 10, 29, 8, 0)},
		{"跨零点白天起算属于当天晚", at(2026, 10, 28, 15, 0), cross, 1, at(2026, 10, 29, 8, 0)},
		{"跨零点 extend 从上晚结束时刻接续", at(2026, 10, 29, 8, 0), cross, 2, at(2026, 10, 31, 8, 0)},
		{"不跨零点 1 晚", at(2026, 10, 28, 1, 0), early, 1, at(2026, 10, 28, 7, 0)},
		{"不跨零点当天窗口已过顺延到次日", at(2026, 10, 28, 9, 0), early, 2, at(2026, 10, 30, 7, 0)},
		{"月底跨月", at(2026, 2, 27, 22, 0), cross, 3, at(2026, 3, 2, 8, 0)},
		{"跨年", at(2026, 12, 31, 22, 0), cross, 1, at(2027, 1, 1, 8, 0)},
		{"跨年多晚", at(2026, 12, 30, 23, 30), cross, 4, at(2027, 1, 3, 8, 0)},
		{"UTC 输入按上海时区归属", time.Date(2026, 10, 28, 14, 0, 0, 0, time.UTC), cross, 1, at(2026, 10, 29, 8, 0)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := LeaseEndAt(tc.start, PricingNightly, tc.n, tc.win)
			require.NoError(t, err)
			assert.True(t, tc.want.Equal(got), "want %s got %s", tc.want, got)
			hourly, _ := LeaseEndAt(tc.start, PricingHourly, tc.n, nil)
			assert.False(t, hourly.Equal(got), "包夜不得落入默认的按小时分支")
		})
	}

	t.Run("缺窗口快照直接报错, 不回退按小时", func(t *testing.T) {
		_, err := LeaseEndAt(at(2026, 10, 28, 22, 0), PricingNightly, 1, nil)
		assert.Error(t, err)
	})
	t.Run("night_date 归属", func(t *testing.T) {
		assert.True(t, at(2026, 10, 28, 0, 0).Equal(NightDateOf(at(2026, 10, 29, 7, 59), *cross)))
		assert.True(t, at(2026, 10, 29, 0, 0).Equal(NightDateOf(at(2026, 10, 29, 8, 0), *cross)))
		assert.True(t, at(2026, 10, 28, 0, 0).Equal(NightDateOf(at(2026, 10, 28, 6, 59), *early)))
		assert.True(t, at(2026, 10, 29, 0, 0).Equal(NightDateOf(at(2026, 10, 28, 7, 0), *early)))
	})
	t.Run("nightly 单笔上限 31 晚", func(t *testing.T) {
		assert.Equal(t, 31, MaxDurationFor(PricingNightly))
		assert.Equal(t, "晚", DurationUnit(PricingNightly))
	})
}

// 包夜下单/续租(T-094)未接入: 50000, 包装后仍为 50000。托管订单本身走 S2 租约钩子, 门禁见 managed_test.go。
func TestNightlyNotWiredIs50000(t *testing.T) {
	assert.Equal(t, errcode.InternalError, ErrToCode(errNightlyNotWired))
	assert.Equal(t, errcode.InternalError, ErrToCode(errors.Join(errNightlyNotWired)))
}

// CR M1: 订单计费方式只认快照; 缺失时 50000, 不回退商品当前展示方式。
func TestOrderPricingModeRequiresSnapshot(t *testing.T) {
	daily, empty := "daily", ""
	mode, err := OrderPricingMode(&Order{OrderNo: "ORD1", PricingMode: &daily})
	require.NoError(t, err)
	assert.Equal(t, "daily", mode)
	for _, o := range []*Order{{OrderNo: "ORD2"}, {OrderNo: "ORD3", PricingMode: &empty}} {
		_, err := OrderPricingMode(o)
		assert.ErrorIs(t, err, ErrOrderPricingModeMissing)
		assert.Contains(t, err.Error(), o.OrderNo)
		assert.Equal(t, errcode.InternalError, ErrToCode(err))
	}
	_, err = OrderPricingMode(nil)
	assert.Equal(t, errcode.NotFound, ErrToCode(err))
}
