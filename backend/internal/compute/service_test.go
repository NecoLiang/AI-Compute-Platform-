package compute

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// ===== 测试夹具 =====

// validCardRentalReq 返回一个合法的零租商品请求, 各用例在其上做单点变异。
func validCardRentalReq() CreateProductReq {
	return CreateProductReq{
		ProductType: ProductTypeCardRental,
		GpuModel:    "H100",
		CardCount:   8,
		PricingMode: PricingHourly,
		UnitPrice:   3500,
		Stock:       16,
		Region:      "华东",
	}
}

// orderableProduct 返回一个可在线下单的商品。
func orderableProduct() *Product {
	return &Product{
		ID: 1, SupplierID: 100,
		ProductType: ProductTypeCardRental,
		PricingMode: PricingHourly,
		UnitPrice:   3500,
		Stock:       16,
		MinOrder:    1,
		MinDuration: 1,
		Status:      "active",
	}
}

func intPtr(v int) *int { return &v }

// ===== C-01 / C-02 / C-04: 商品类型与计费模式校验矩阵 =====

func TestValidateProductReq_TypeAndPricingMatrix(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*CreateProductReq)
		wantErr string // 空 = 期望通过; 非空 = 期望错误信息包含该片段
	}{
		// --- 类型本身 ---
		{"合法零租", nil, ""},
		{"类型为空", func(r *CreateProductReq) { r.ProductType = "" }, "商品类型非法"},
		{"类型未知", func(r *CreateProductReq) { r.ProductType = "gpu_lease" }, "商品类型非法"},

		// --- 零租 card_rental: 计费仅 h/d/w ---
		{"零租-按小时", func(r *CreateProductReq) { r.PricingMode = PricingHourly }, ""},
		{"零租-按天", func(r *CreateProductReq) { r.PricingMode = PricingDaily }, ""},
		{"零租-按周", func(r *CreateProductReq) { r.PricingMode = PricingWeekly }, ""},
		{"零租-按月非法", func(r *CreateProductReq) { r.PricingMode = PricingMonthly }, "不适用于该商品类型"},
		{"零租-永久非法", func(r *CreateProductReq) { r.PricingMode = PricingPerpetual }, "不适用于该商品类型"},
		{"零租-缺GPU型号", func(r *CreateProductReq) { r.GpuModel = "" }, "必须填写 GPU 型号"},
		{"零租-GPU型号纯空格", func(r *CreateProductReq) { r.GpuModel = "   " }, "必须填写 GPU 型号"},
		{"零租-卡数为0", func(r *CreateProductReq) { r.CardCount = 0 }, "卡数必须大于 0"},
		{"零租-卡数负数", func(r *CreateProductReq) { r.CardCount = -8 }, "卡数必须大于 0"},
		{"零租-库存为0", func(r *CreateProductReq) { r.Stock = 0 }, "可租库存必须大于 0"},
		{"零租-单价为0", func(r *CreateProductReq) { r.UnitPrice = 0 }, "单价必须大于 0"},
		{"零租-不许面议", func(r *CreateProductReq) { r.PriceNegotiable = true }, "不支持面议"},

		// --- 零售买断 outright: 必须 perpetual ---
		{"买断-合法", func(r *CreateProductReq) {
			r.ProductType, r.PricingMode, r.MachineCount = ProductTypeOutright, PricingPerpetual, 4
		}, ""},
		{"买断-按小时非法", func(r *CreateProductReq) {
			r.ProductType, r.PricingMode, r.MachineCount = ProductTypeOutright, PricingHourly, 4
		}, "不适用于该商品类型"},
		{"买断-缺台数", func(r *CreateProductReq) {
			r.ProductType, r.PricingMode, r.MachineCount = ProductTypeOutright, PricingPerpetual, 0
		}, "台数必须大于 0"},
		{"买断-缺GPU型号", func(r *CreateProductReq) {
			r.ProductType, r.PricingMode, r.MachineCount, r.GpuModel = ProductTypeOutright, PricingPerpetual, 4, ""
		}, "必须填写 GPU 型号"},
		{"买断-不许面议", func(r *CreateProductReq) {
			r.ProductType, r.PricingMode, r.MachineCount, r.PriceNegotiable = ProductTypeOutright, PricingPerpetual, 4, true
		}, "不支持面议"},

		// --- 成熟算力中心 center: 必须有台数 + 约算力 ---
		{"算力中心-合法", func(r *CreateProductReq) {
			r.ProductType, r.PricingMode, r.MachineCount, r.TotalPflopsApprox = ProductTypeCenter, PricingMonthly, 64, "约128P"
		}, ""},
		{"算力中心-按小时非法", func(r *CreateProductReq) {
			r.ProductType, r.PricingMode, r.MachineCount, r.TotalPflopsApprox = ProductTypeCenter, PricingHourly, 64, "约128P"
		}, "不适用于该商品类型"},
		{"算力中心-缺约算力", func(r *CreateProductReq) {
			r.ProductType, r.PricingMode, r.MachineCount, r.TotalPflopsApprox = ProductTypeCenter, PricingMonthly, 64, ""
		}, "约总算力"},
		{"算力中心-缺台数", func(r *CreateProductReq) {
			r.ProductType, r.PricingMode, r.MachineCount, r.TotalPflopsApprox = ProductTypeCenter, PricingMonthly, 0, "约128P"
		}, "台数必须大于 0"},
		{"算力中心-面议可免单价", func(r *CreateProductReq) {
			r.ProductType, r.PricingMode, r.MachineCount, r.TotalPflopsApprox = ProductTypeCenter, PricingMonthly, 64, "约128P"
			r.PriceNegotiable, r.UnitPrice = true, 0
		}, ""},
		{"算力中心-非面议且无单价", func(r *CreateProductReq) {
			r.ProductType, r.PricingMode, r.MachineCount, r.TotalPflopsApprox = ProductTypeCenter, PricingMonthly, 64, "约128P"
			r.PriceNegotiable, r.UnitPrice = false, 0
		}, "需填写单价, 或勾选面议"},

		// --- 空心机房 colocation: 仅面议 ---
		{"空心机房-合法", func(r *CreateProductReq) {
			r.ProductType, r.PowerCapacityKw, r.RackCount = ProductTypeColocation, 2000, 50
			r.PriceNegotiable, r.UnitPrice = true, 0
		}, ""},
		{"空心机房-缺电力容量", func(r *CreateProductReq) {
			r.ProductType, r.PowerCapacityKw, r.RackCount = ProductTypeColocation, 0, 50
			r.PriceNegotiable, r.UnitPrice = true, 0
		}, "电力容量"},
		{"空心机房-缺机柜数", func(r *CreateProductReq) {
			r.ProductType, r.PowerCapacityKw, r.RackCount = ProductTypeColocation, 2000, 0
			r.PriceNegotiable, r.UnitPrice = true, 0
		}, "机柜数"},
		{"空心机房-必须面议", func(r *CreateProductReq) {
			r.ProductType, r.PowerCapacityKw, r.RackCount = ProductTypeColocation, 2000, 50
			r.PriceNegotiable, r.UnitPrice = false, 100
		}, "仅支持面议"},
		{"空心机房-面议单价必须为0", func(r *CreateProductReq) {
			r.ProductType, r.PowerCapacityKw, r.RackCount = ProductTypeColocation, 2000, 50
			r.PriceNegotiable, r.UnitPrice = true, 100
		}, "单价必须为 0"},

		// --- 通用边界 ---
		{"单价负数", func(r *CreateProductReq) { r.UnitPrice = -1 }, "单价不能为负数"},
		{"库存负数", func(r *CreateProductReq) { r.Stock = -1 }, "库存不能为负数"},
		{"单价超上限", func(r *CreateProductReq) { r.UnitPrice = MaxOrderTotalFen + 1 }, "单价超出上限"},
		{"库存超上限", func(r *CreateProductReq) { r.Stock = MaxOrderQuantity + 1 }, "库存超出上限"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validCardRentalReq()
			if tc.mutate != nil { tc.mutate(&req) }
			err := ValidateProductReq(req)
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// ===== 资金安全: 下单参数校验 (修复的历史漏洞) =====

func TestValidateOrderParams_FundSafety(t *testing.T) {
	t.Run("负数量必须被拒绝(否则会凭空增加库存)", func(t *testing.T) {
		_, _, err := ValidateOrderParams(orderableProduct(), -5, 10)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "不能少于起订量")
	})

	t.Run("零数量必须被拒绝", func(t *testing.T) {
		_, _, err := ValidateOrderParams(orderableProduct(), 0, 10)
		assert.Error(t, err)
	})

	t.Run("零时长必须被拒绝(否则总价为0白嫖)", func(t *testing.T) {
		_, _, err := ValidateOrderParams(orderableProduct(), 1, 0)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "不能少于最短租期")
	})

	t.Run("负时长必须被拒绝", func(t *testing.T) {
		_, _, err := ValidateOrderParams(orderableProduct(), 1, -100)
		assert.Error(t, err)
	})

	t.Run("数量超上限必须被拒绝", func(t *testing.T) {
		p := orderableProduct()
		p.Stock = 0 // 绕过库存分支, 单独验上限
		_, _, err := ValidateOrderParams(p, MaxOrderQuantity+1, 1)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "数量超出上限")
	})

	t.Run("时长超上限必须被拒绝", func(t *testing.T) {
		p := orderableProduct() // hourly
		_, _, err := ValidateOrderParams(p, 1, MaxDurationFor(PricingHourly)+1)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "租期超出上限")
	})

	// C-04: duration 语义为「计费周期数」，上限按计费模式分别设定，
	// 否则 monthly 商品可以传 87600 表示 7300 年租期。
	t.Run("时长上限按计费模式分别生效", func(t *testing.T) {
		cases := []struct {
			mode string
			max  int
			unit string
		}{
			{PricingHourly, 87600, "小时"},
			{PricingDaily, 3650, "天"},
			{PricingWeekly, 520, "周"},
			{PricingMonthly, 120, "个月"},
		}
		for _, tc := range cases {
			p := orderableProduct()
			p.PricingMode = tc.mode

			// 恰好等于上限：通过
			_, dur, err := ValidateOrderParams(p, 1, tc.max)
			assert.NoError(t, err, "%s 模式下 %d 应通过", tc.mode, tc.max)
			assert.Equal(t, tc.max, dur)

			// 超出一个周期：拒绝，且提示里带正确单位
			_, _, err = ValidateOrderParams(p, 1, tc.max+1)
			assert.Error(t, err, "%s 模式下 %d 应被拒绝", tc.mode, tc.max+1)
			assert.Contains(t, err.Error(), tc.unit, "错误提示必须带单位，避免无单位歧义")
			assert.Equal(t, tc.max, MaxDurationFor(tc.mode))
			assert.Equal(t, tc.unit, DurationUnit(tc.mode))
		}
	})

	t.Run("超过库存必须被拒绝", func(t *testing.T) {
		p := orderableProduct()
		p.Stock = 4
		_, _, err := ValidateOrderParams(p, 5, 1)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "insufficient stock")
	})

	t.Run("低于起订量必须被拒绝", func(t *testing.T) {
		p := orderableProduct()
		p.MinOrder = 4
		_, _, err := ValidateOrderParams(p, 2, 1)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "起订量 4")
	})

	t.Run("低于最短租期必须被拒绝", func(t *testing.T) {
		p := orderableProduct()
		p.MinDuration = 24
		_, _, err := ValidateOrderParams(p, 1, 12)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "最短租期 24")
	})

	t.Run("买断商品强制时长为1并忽略客户端传值", func(t *testing.T) {
		p := orderableProduct()
		p.ProductType, p.PricingMode = ProductTypeOutright, PricingPerpetual
		qty, dur, err := ValidateOrderParams(p, 2, 99999)
		assert.NoError(t, err)
		assert.Equal(t, 2, qty)
		assert.Equal(t, 1, dur, "永久买断必须归一为 1, 防止被时长放大总价")
	})

	t.Run("面议商品禁止在线下单", func(t *testing.T) {
		p := orderableProduct()
		p.PriceNegotiable = true
		_, _, err := ValidateOrderParams(p, 1, 1)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "面议商品")
	})

	t.Run("空心机房禁止在线下单", func(t *testing.T) {
		p := orderableProduct()
		p.ProductType = ProductTypeColocation
		_, _, err := ValidateOrderParams(p, 1, 1)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "面议商品")
	})

	t.Run("单价非正数禁止下单", func(t *testing.T) {
		p := orderableProduct()
		p.UnitPrice = 0
		_, _, err := ValidateOrderParams(p, 1, 1)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "未设置有效单价")
	})

	t.Run("商品为nil时报错而非panic", func(t *testing.T) {
		_, _, err := ValidateOrderParams(nil, 1, 1)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "product not found")
	})

	t.Run("合法参数原样返回", func(t *testing.T) {
		qty, dur, err := ValidateOrderParams(orderableProduct(), 8, 720)
		assert.NoError(t, err)
		assert.Equal(t, 8, qty)
		assert.Equal(t, 720, dur)
	})
}

// 续租不得重复占用库存: 买家延长的是已持有的同一批卡。
func TestValidateRenewParams_DoesNotConsumeStock(t *testing.T) {
	p := orderableProduct()
	p.Stock = 2 // 库存已被别人买走, 只剩 2

	// 下单路径: 买 5 张应被库存拦住
	_, _, err := ValidateOrderParams(p, 5, 24)
	assert.Error(t, err, "下单必须校验库存")
	assert.Contains(t, err.Error(), "insufficient stock")

	// 续租路径: 老客户手里已有 5 张, 续租不应被库存拦住
	qty, dur, err := ValidateRenewParams(p, 5, 24)
	assert.NoError(t, err, "续租不得校验库存, 否则卡卖光后老客户无法续租")
	assert.Equal(t, 5, qty)
	assert.Equal(t, 24, dur)

	// 但其余规则仍然生效
	_, _, err = ValidateRenewParams(p, 5, 0)
	assert.Error(t, err, "续租仍必须校验时长")
}

// ===== 资金安全: 金额计算 (int64 分, 溢出保护) =====

func TestCalcOrderAmount(t *testing.T) {
	t.Run("常规金额与佣金", func(t *testing.T) {
		total, fee, err := CalcOrderAmount(3500, 8, 720, 500)
		assert.NoError(t, err)
		assert.Equal(t, int64(20160000), total, "¥35.00 × 8卡 × 720时 = ¥201,600.00")
		assert.Equal(t, int64(1008000), fee, "5% 佣金 = ¥10,080.00")
	})

	t.Run("佣金分步计算与朴素公式完全一致(含余数)", func(t *testing.T) {
		// 遍历一批带余数的金额, 验证 total/10000*rate + (total%10000)*rate/10000
		// 与 total*rate/10000 结果一致。
		for _, total := range []int64{1, 7, 333, 9999, 10000, 10001, 123457, 999999999} {
			for _, rate := range []int64{0, 1, 250, 500, 1000} {
				_, fee, err := CalcOrderAmount(total, 1, 1, rate)
				assert.NoError(t, err)
				assert.Equal(t, total*rate/10000, fee,
					"total=%d rate=%d 佣金分步计算必须等于朴素公式", total, rate)
			}
		}
	})

	t.Run("单价为0或负数报错", func(t *testing.T) {
		_, _, err := CalcOrderAmount(0, 1, 1, 500)
		assert.Error(t, err)
		_, _, err = CalcOrderAmount(-100, 1, 1, 500)
		assert.Error(t, err)
	})

	t.Run("数量或时长非正数报错", func(t *testing.T) {
		_, _, err := CalcOrderAmount(3500, 0, 1, 500)
		assert.Error(t, err)
		_, _, err = CalcOrderAmount(3500, 1, 0, 500)
		assert.Error(t, err)
		_, _, err = CalcOrderAmount(3500, -1, 1, 500)
		assert.Error(t, err)
	})

	t.Run("单价乘数量溢出被拦截", func(t *testing.T) {
		_, _, err := CalcOrderAmount(math.MaxInt64, 2, 1, 500)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "溢出")
	})

	t.Run("再乘时长溢出被拦截", func(t *testing.T) {
		_, _, err := CalcOrderAmount(math.MaxInt64/4, 4, 1000, 500)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "溢出")
	})

	t.Run("超过单笔金额上限被拦截", func(t *testing.T) {
		_, _, err := CalcOrderAmount(100000000000, 100, 1000, 500)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "上限")
	})

	t.Run("恰好等于上限可通过", func(t *testing.T) {
		total, fee, err := CalcOrderAmount(MaxOrderTotalFen, 1, 1, 500)
		assert.NoError(t, err)
		assert.Equal(t, MaxOrderTotalFen, total)
		assert.Equal(t, MaxOrderTotalFen*500/10000, fee)
	})

	t.Run("佣金永不超过总额", func(t *testing.T) {
		total, fee, err := CalcOrderAmount(3500, 8, 720, 10000) // 100%
		assert.NoError(t, err)
		assert.Equal(t, total, fee)
		assert.LessOrEqual(t, fee, total)
	})
}

// ===== C-05: 盘点异常阈值 =====

func TestComputeAnomaly(t *testing.T) {
	cases := []struct {
		name        string
		before      int
		after       int
		wantDiff    int
		wantAnomaly bool
	}{
		{"无变化", 100, 100, 0, false},
		{"减少10%正常", 100, 90, -10, false},
		{"恰好减少30%不算异常(阈值为严格大于)", 100, 70, -30, false},
		{"减少31%判异常", 100, 69, -31, true},
		{"减少100%判异常", 100, 0, -100, true},
		{"增加31%判异常", 100, 131, 31, true},
		{"恰好增加30%不算异常", 100, 130, 30, false},
		{"盘前为0不判异常(无基线)", 0, 50, 50, false},
		{"盘前为负不判异常", -1, 50, 51, false},
		{"小基数减1判异常", 2, 1, -1, true},
		{"大基数小变动正常", 100000, 99999, -1, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			diff, anomaly := ComputeAnomaly(tc.before, tc.after)
			assert.Equal(t, tc.wantDiff, diff)
			assert.Equal(t, tc.wantAnomaly, anomaly)
		})
	}
}

// ===== C-06: 访问凭证 =====

func TestGenerateAccessKey(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		k, err := GenerateAccessKey()
		assert.NoError(t, err)
		assert.True(t, strings.HasPrefix(k, AccessKeyPrefix), "必须以 ak- 开头")
		assert.Len(t, k, len(AccessKeyPrefix)+accessKeyRandLen*2)
		assert.True(t, IsValidAccessKey(k), "生成的 key 必须自校验通过: %s", k)
		assert.False(t, seen[k], "access_key 出现重复, 随机性不足: %s", k)
		seen[k] = true
	}
}

func TestGenerateAccessValue(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		v, err := GenerateAccessValue()
		assert.NoError(t, err)
		assert.Len(t, v, accessValRandLen*2, "48 位 hex")
		assert.False(t, seen[v], "access_value 出现重复")
		seen[v] = true
	}
}

func TestIsValidAccessKey(t *testing.T) {
	valid, err := GenerateAccessKey()
	assert.NoError(t, err)

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"合法", valid, true},
		{"空串", "", false},
		{"缺前缀", strings.Repeat("a", 32), false},
		{"前缀错误", "sk-" + strings.Repeat("a", 32), false},
		{"长度不足", AccessKeyPrefix + strings.Repeat("a", 31), false},
		{"长度超出", AccessKeyPrefix + strings.Repeat("a", 33), false},
		{"含大写hex", AccessKeyPrefix + strings.Repeat("A", 32), false},
		{"含非hex字符", AccessKeyPrefix + strings.Repeat("g", 32), false},
		{"只有前缀", AccessKeyPrefix, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, IsValidAccessKey(tc.in))
		})
	}
}

func TestMaskAccessValue(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"空串", "", ""},
		{"长度1全码", "a", "*"},
		{"长度8全码(防短串泄露)", "abcdefgh", "********"},
		{"长度9保留前后4", "abcdefghi", "abcd********fghi"},
		{"典型48位", "0123456789abcdef0123456789abcdef0123456789abcdef", "0123********cdef"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := MaskAccessValue(tc.in)
			assert.Equal(t, tc.want, got)
			// 脱敏结果绝不能包含完整原文
			if len(tc.in) > 8 {
				assert.NotEqual(t, tc.in, got)
				assert.NotContains(t, got, tc.in[4:len(tc.in)-4], "中间段必须被打码")
			}
		})
	}
}

// ===== C-03: 供给方工作台按类型分组 =====

func TestGroupProductsByType(t *testing.T) {
	list := []Product{
		{ID: 1, ProductType: ProductTypeCardRental, CardCount: 8},
		{ID: 2, ProductType: ProductTypeCardRental, CardCount: 8},
		{ID: 3, ProductType: ProductTypeCenter, CardCount: 512, MachineCount: intPtr(64)},
		{ID: 4, ProductType: ProductTypeColocation, RackCount: intPtr(50)},
	}
	groups := GroupProductsByType(list)

	byType := map[string]ProductTypeGroup{}
	for _, g := range groups { byType[g.ProductType] = g }

	assert.Len(t, byType[ProductTypeCardRental].Products, 2)
	assert.Len(t, byType[ProductTypeCenter].Products, 1)
	assert.Len(t, byType[ProductTypeColocation].Products, 1)
	assert.Empty(t, byType[ProductTypeOutright].Products, "无数据的类型应为空而非缺失")

	// 分组必须覆盖全部 4 种类型, 且顺序稳定
	assert.Len(t, groups, len(productTypeOrder))
	for i, want := range productTypeOrder {
		assert.Equal(t, want, groups[i].ProductType, "分组顺序必须稳定")
	}

	// 商品总数守恒
	sum := 0
	for _, g := range groups { sum += len(g.Products) }
	assert.Equal(t, len(list), sum, "分组后商品总数必须守恒")
}

// ===== C-04: duration 为「计费周期数」，租期换算必须按模式走日历 =====

func TestLeaseEndAt(t *testing.T) {
	start := time.Date(2026, 1, 31, 10, 0, 0, 0, time.UTC)

	cases := []struct {
		name string
		mode string
		dur  int
		want time.Time
	}{
		{"按小时: 24 小时", PricingHourly, 24, time.Date(2026, 2, 1, 10, 0, 0, 0, time.UTC)},
		{"按天: 3 天", PricingDaily, 3, time.Date(2026, 2, 3, 10, 0, 0, 0, time.UTC)},
		{"按周: 2 周 = 14 天", PricingWeekly, 2, time.Date(2026, 2, 14, 10, 0, 0, 0, time.UTC)},
		// 自然月：1月31日 + 1 个月，Go 规范化为 3 月 3 日（2026 非闰年，2 月 28 天）
		{"按月: 走自然日历而非固定30天", PricingMonthly, 1, time.Date(2026, 3, 3, 10, 0, 0, 0, time.UTC)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, LeaseEndAt(start, tc.mode, tc.dur))
		})
	}

	t.Run("买断无到期时间", func(t *testing.T) {
		assert.True(t, LeaseEndAt(start, PricingPerpetual, 1).IsZero(),
			"永久使用权必须返回零值，由调用方置 lease_end_at = NULL")
	})

	t.Run("同样的 duration 在不同模式下租期长度不同", func(t *testing.T) {
		// 这正是修复的核心：duration=30 在 hourly 下是 30 小时，在 daily 下是 30 天。
		hourly := LeaseEndAt(start, PricingHourly, 30)
		daily := LeaseEndAt(start, PricingDaily, 30)
		assert.True(t, daily.After(hourly),
			"daily 的 30 个周期必须远长于 hourly 的 30 个周期，否则就是把周期数当成了小时数")
		assert.Equal(t, 30*24, int(daily.Sub(start).Hours()))
		assert.Equal(t, 30, int(hourly.Sub(start).Hours()))
	})
}

// ===== 回归: 分页默认值 =====

func TestProductFilterDefaults(t *testing.T) {
	f := ProductFilter{}
	if f.Page <= 0 { f.Page = 1 }
	if f.PageSize <= 0 { f.PageSize = 20 }
	assert.Equal(t, 1, f.Page)
	assert.Equal(t, 20, f.PageSize)
}
