package compute

import (
	"errors"
	"fmt"
	"time"

	"tokenfactory/pkg/errcode"
)

// ===== v1.2 灵活计费 (docs/26 §26.2-26.4, T-090) =====

const PricingNightly = "nightly"

const (
	AvailabilityFulltime    = "fulltime"
	AvailabilityNightWindow = "night_window"
)

// FulfillmentManual / FulfillmentManaged 定义在 managed.go(S1), 此处不重复。

// 夜间窗口默认 22:00-08:00, 长度 4-14 小时, 30 分钟对齐 (§26.6.1)。
const (
	DefaultNightStartMin = 22 * 60
	DefaultNightEndMin   = 8 * 60
	nightWindowMinLen    = 4 * 60
	nightWindowMaxLen    = 14 * 60
	nightWindowAlignMin  = 30
)

var (
	// ErrPricingModeDisabled 该计费方式未开放或已停用。续租时映射为 40900(提示重新下单),
	// 下单时由调用方改写为 40001 参数错误。
	ErrPricingModeDisabled = errors.New("该计费方式已停用，请重新下单")
	// errNightlyNotWired 包夜续租(在同一组卡上追加后续晚, docs/26 §26.6.3)尚未实现, 明确 50000,
	// 不落成按小时或全天交付的续租单。
	errNightlyNotWired = errcode.New(errcode.InternalError, "包夜订单暂不支持续租(追加晚数尚未接入, docs/26 §26.6.3), 请重新下单")
	// errNightlyHooksNotWired 包夜预订/日历依赖 fleet 注入的夜间钩子(T-094), 未注入时明确 50000。
	errNightlyHooksNotWired = errcode.New(errcode.InternalError, "包夜预订模块未接入(fleet 未注入夜间预订/日历钩子), 暂不可预订包夜")
	// ErrOrderPricingModeMissing 订单缺计费方式快照(orders.pricing_mode 为空)。028 回填后不应出现,
	// 出现即数据异常, 返回 50000 由平台核对, 不猜测按商品当前方式计算租期或报价。
	ErrOrderPricingModeMissing = errors.New("订单缺少计费方式快照, 无法计算租期或续租报价, 请联系平台核对")
)

// ProductPrice 零租商品的一种计费方式及其价格(product_prices 一行)。unit_price 为 分/卡/周期。
type ProductPrice struct {
	ProductID   int64  `db:"product_id" json:"-"`
	PricingMode string `db:"pricing_mode" json:"pricing_mode"`
	UnitPrice   int64  `db:"unit_price" json:"unit_price"`
	MinDuration int    `db:"min_duration" json:"min_duration"`
	Enabled     bool   `db:"enabled" json:"enabled"`
}

// PriceInput 发布/重提商品时提交的一种计费方式。min_duration 省略视为 1, enabled 省略视为 true。
type PriceInput struct {
	PricingMode string `json:"pricing_mode"`
	UnitPrice   int64  `json:"unit_price"`
	MinDuration *int   `json:"min_duration"`
	Enabled     *bool  `json:"enabled"`
}

// PriceQuote PriceFor 的结果: 某商品按某计费方式下单/续租时使用的价格。
type PriceQuote struct {
	PricingMode string
	UnitPrice   int64
	MinDuration int
}

// PriceWarning 价格倒挂提醒, 只提醒不阻断 (§26.3.2)。
type PriceWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// pricingPeriodOrder 周期从短到长, 用于选展示主价与稳定输出顺序。
var pricingPeriodOrder = []string{PricingHourly, PricingNightly, PricingDaily, PricingWeekly, PricingMonthly, PricingPerpetual}

func periodRank(mode string) int {
	for i, m := range pricingPeriodOrder {
		if m == mode {
			return i
		}
	}
	return len(pricingPeriodOrder)
}

// PriceFor 返回商品按 mode 计价的单价与最短租期。mode 为空取展示主价方式(兼容旧前端/存量订单)。
// 零租商品有价格行时以价格行为准, 该方式不存在或已停用返回 ErrPricingModeDisabled;
// 无价格行(非零租商品, 或未经 028 回填的行)回退到商品上的单价, 但仅限展示主价方式。
func PriceFor(p *Product, mode string) (PriceQuote, error) {
	if p == nil {
		return PriceQuote{}, fmt.Errorf("product not found")
	}
	if mode == "" {
		mode = p.PricingMode
	}
	if p.ProductType == ProductTypeCardRental && len(p.Prices) > 0 {
		for _, pr := range p.Prices {
			if pr.PricingMode == mode {
				if !pr.Enabled {
					return PriceQuote{}, ErrPricingModeDisabled
				}
				return PriceQuote{PricingMode: mode, UnitPrice: pr.UnitPrice, MinDuration: max(1, pr.MinDuration)}, nil
			}
		}
		return PriceQuote{}, ErrPricingModeDisabled
	}
	if mode != p.PricingMode {
		return PriceQuote{}, ErrPricingModeDisabled
	}
	return PriceQuote{PricingMode: mode, UnitPrice: p.UnitPrice, MinDuration: max(1, p.MinDuration)}, nil
}

// EnabledPrices 返回已启用的价格行(市场展示用), 永不为 nil。
func EnabledPrices(prices []ProductPrice) []ProductPrice {
	out := make([]ProductPrice, 0, len(prices))
	for _, pr := range prices {
		if pr.Enabled {
			out = append(out, pr)
		}
	}
	return out
}

// PriceWarnings 价格倒挂提醒(只看已启用价格), 永不为 nil 以便 JSON 输出 []。
func PriceWarnings(prices []ProductPrice) []PriceWarning {
	unit := map[string]int64{}
	for _, pr := range prices {
		if pr.Enabled {
			unit[pr.PricingMode] = pr.UnitPrice
		}
	}
	h, hasH := unit[PricingHourly]
	d, hasD := unit[PricingDaily]
	w, hasW := unit[PricingWeekly]
	m, hasM := unit[PricingMonthly]
	out := []PriceWarning{}
	if hasH && hasD && d > 24*h {
		out = append(out, PriceWarning{Code: "daily_above_24_hourly", Message: "日价高于 24 小时价"})
	}
	if hasD && hasW && w > 7*d {
		out = append(out, PriceWarning{Code: "weekly_above_7_daily", Message: "周价高于 7 天价"})
	}
	if hasM && hasD && m > 30*d {
		out = append(out, PriceWarning{Code: "monthly_above_30_daily", Message: "月价高于 30 天价"})
	} else if hasM && !hasD && hasW && m > 4*w {
		out = append(out, PriceWarning{Code: "monthly_above_4_weekly", Message: "月价高于 4 周价"})
	}
	return out
}

// ===== 夜间窗口与包夜租期 (§26.6.1-26.6.2) =====

// NightWindow 夜间窗口, 分钟数取 Asia/Shanghai 当日 0-1439。EndMin <= StartMin 表示次日结束。
type NightWindow struct {
	StartMin int
	EndMin   int
}

func (w NightWindow) crossesMidnight() bool { return w.EndMin <= w.StartMin }

func (w NightWindow) lengthMin() int {
	if w.crossesMidnight() {
		return 1440 - w.StartMin + w.EndMin
	}
	return w.EndMin - w.StartMin
}

// Validate 校验窗口: 0-1439, 30 分钟对齐, 长度 4-14 小时。
func (w NightWindow) Validate() error {
	if w.StartMin < 0 || w.StartMin > 1439 || w.EndMin < 0 || w.EndMin > 1439 {
		return fmt.Errorf("夜间窗口时间必须在 0-1439 分钟之间")
	}
	if w.StartMin%nightWindowAlignMin != 0 || w.EndMin%nightWindowAlignMin != 0 {
		return fmt.Errorf("夜间窗口必须按 30 分钟对齐")
	}
	if l := w.lengthMin(); l < nightWindowMinLen || l > nightWindowMaxLen {
		return fmt.Errorf("夜间窗口长度必须在 4-14 小时之间")
	}
	return nil
}

// Text 窗口展示文本, 如 "22:00-08:00"; 用于自动生成 available_hours。
func (w NightWindow) Text() string {
	return fmt.Sprintf("%02d:%02d-%02d:%02d", w.StartMin/60, w.StartMin%60, w.EndMin/60, w.EndMin%60)
}

func shanghaiLoc() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*3600)
}

func midnight(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

// NightDateOf 返回时刻 t 所属或其后第一个"晚"的 night_date(窗口开始那天, Asia/Shanghai 零点)。
// 跨零点窗口中, 结束前的凌晨时段属于前一天的晚; 不跨零点窗口中, 当天窗口已结束则顺延到次日的晚。
func NightDateOf(t time.Time, w NightWindow) time.Time {
	local := t.In(shanghaiLoc())
	day := midnight(local)
	tod := local.Hour()*60 + local.Minute()
	switch {
	case w.crossesMidnight() && tod < w.EndMin:
		return day.AddDate(0, 0, -1)
	case !w.crossesMidnight() && tod >= w.EndMin:
		return day.AddDate(0, 0, 1)
	}
	return day
}

// NightlyLeaseEnd lease_end = (night_start_date + nights - 1 天) + night_end_min, 跨零点再 +1 天。
// 日期加法走日历 AddDate, 月底/跨年无需特判。
func NightlyLeaseEnd(nightStartDate time.Time, nights int, w NightWindow) time.Time {
	d := midnight(nightStartDate.In(shanghaiLoc())).AddDate(0, 0, nights-1)
	if w.crossesMidnight() {
		d = d.AddDate(0, 0, 1)
	}
	return d.Add(time.Duration(w.EndMin) * time.Minute)
}

// NightWindowOf 返回商品的夜间窗口(非夜间窗口商品返回 nil)。
func NightWindowOf(p *Product) *NightWindow {
	if p == nil || p.Availability != AvailabilityNightWindow || p.NightStartMin == nil || p.NightEndMin == nil {
		return nil
	}
	return &NightWindow{StartMin: *p.NightStartMin, EndMin: *p.NightEndMin}
}

// OrderNightWindow 返回订单快照的夜间窗口(非包夜订单返回 nil)。
func OrderNightWindow(o *Order) *NightWindow {
	if o == nil || o.NightStartMin == nil || o.NightEndMin == nil {
		return nil
	}
	return &NightWindow{StartMin: *o.NightStartMin, EndMin: *o.NightEndMin}
}

// OrderPricingMode 订单计费方式, 只认订单快照(CR M1)。
// 028 已把存量订单回填为迁移时商品的计费方式, 此后每笔订单下单即写快照, 所以 NULL/空串是数据异常:
// 返回 ErrOrderPricingModeMissing(50000), 绝不回退商品当前展示方式——商品重提后展示主价会变成
// 最短周期, 回退会让 3 天的订单按 3 小时交付、按小时价续租。
func OrderPricingMode(o *Order) (string, error) {
	if o == nil {
		return "", fmt.Errorf("order not found")
	}
	if o.PricingMode == nil || *o.PricingMode == "" {
		return "", fmt.Errorf("%w: 订单 %s", ErrOrderPricingModeMissing, o.OrderNo)
	}
	return *o.PricingMode, nil
}

// ===== 商品发布: 多价格归一化与组合校验 (§26.3.2) =====

func intOr(v *int, def int) int {
	if v == nil {
		return def
	}
	return *v
}

// normalizeCardRentalPricing 零租商品: 夜间窗口补默认时段并生成展示文本; 未传 prices 时按顶层字段生成一行;
// 传了 prices 时把已启用价格中周期最短的一项同步为展示主价(顶层 pricing_mode/unit_price/min_duration)。
func normalizeCardRentalPricing(req CreateProductReq) CreateProductReq {
	if req.Availability == AvailabilityNightWindow {
		if req.NightStartMin == nil {
			v := DefaultNightStartMin
			req.NightStartMin = &v
		}
		if req.NightEndMin == nil {
			v := DefaultNightEndMin
			req.NightEndMin = &v
		}
		req.AvailableHours = NightWindow{StartMin: *req.NightStartMin, EndMin: *req.NightEndMin}.Text()
		if req.PricingMode == "" {
			req.PricingMode = PricingNightly
		}
	}
	if req.PricingMode == "" {
		req.PricingMode = PricingHourly
	}
	if len(req.Prices) == 0 {
		md := req.MinDuration
		req.Prices = []PriceInput{{PricingMode: req.PricingMode, UnitPrice: req.UnitPrice, MinDuration: &md}}
		return req
	}
	best := -1
	for i, pr := range req.Prices {
		if pr.Enabled != nil && !*pr.Enabled {
			continue
		}
		if best < 0 || periodRank(pr.PricingMode) < periodRank(req.Prices[best].PricingMode) {
			best = i
		}
	}
	if best >= 0 {
		pr := req.Prices[best]
		req.PricingMode, req.UnitPrice, req.MinDuration = pr.PricingMode, pr.UnitPrice, max(1, intOr(pr.MinDuration, 1))
	}
	return req
}

// effectivePriceInputs 未归一化的请求(仅顶层字段)视为一行价格, 让新旧两种入参走同一套校验。
func effectivePriceInputs(req CreateProductReq) []PriceInput {
	if len(req.Prices) > 0 {
		return req.Prices
	}
	md := max(1, req.MinDuration)
	return []PriceInput{{PricingMode: req.PricingMode, UnitPrice: req.UnitPrice, MinDuration: &md}}
}

func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// validateFulfillmentAndAvailability 履约模式与可用时段的通用规则(合并 S1 validateFulfillment 与 S3 规则):
//   - 履约模式/可用时段取值合法;
//   - 托管、夜间窗口、多价格仅限零租;
//   - 托管只适用于 container(分卡容器)/bare_metal(整机)交付(docs/25), 库存由卡槽派生(NormalizeProductReq 置 0)。
//
// 托管商品创建即为草稿、审核通过前按节点状态复核(initialProductStatus / ApproveProduct), 不在此处校验节点。
func validateFulfillmentAndAvailability(req CreateProductReq) error {
	fm := orDefault(req.FulfillmentMode, FulfillmentManual)
	av := orDefault(req.Availability, AvailabilityFulltime)
	if fm != FulfillmentManual && fm != FulfillmentManaged {
		return fmt.Errorf("履约模式非法: 仅支持 manual(人工交付)/managed(平台托管)")
	}
	if av != AvailabilityFulltime && av != AvailabilityNightWindow {
		return fmt.Errorf("可用时段类型非法: 仅支持 fulltime(全天)/night_window(夜间窗口)")
	}
	if req.ProductType != ProductTypeCardRental {
		switch {
		case len(req.Prices) > 0:
			return fmt.Errorf("仅零租商品支持多计费方式价格")
		case fm == FulfillmentManaged:
			return fmt.Errorf("平台托管仅适用于零租商品")
		case av == AvailabilityNightWindow || req.NightStartMin != nil || req.NightEndMin != nil:
			return fmt.Errorf("夜间窗口仅适用于零租商品")
		}
		return nil
	}
	if fm == FulfillmentManaged && req.DeliveryMode != "container" && req.DeliveryMode != "bare_metal" {
		return fmt.Errorf("平台托管仅支持 container(分卡容器)或 bare_metal(整机)交付")
	}
	return nil
}

// validateCardRentalPricing 零租价格组合约束 (§26.2, §26.3.2), 全部为 40001 参数错误。
func validateCardRentalPricing(req CreateProductReq) error {
	fm := orDefault(req.FulfillmentMode, FulfillmentManual)
	av := orDefault(req.Availability, AvailabilityFulltime)
	prices := effectivePriceInputs(req)
	cardCap := int64(max(req.Stock, req.CardCount, 1))
	seen := map[string]bool{}
	enabled, hasNightly := 0, false
	for _, pr := range prices {
		label := pricingModeLabels[pr.PricingMode]
		if !allowedPricingModes[ProductTypeCardRental][pr.PricingMode] {
			return fmt.Errorf("计费模式 %q 不适用于该商品类型, 允许: %s", pr.PricingMode, allowedModesText(ProductTypeCardRental))
		}
		if seen[pr.PricingMode] {
			return fmt.Errorf("计费方式 %s 重复提交", label)
		}
		seen[pr.PricingMode] = true
		if pr.UnitPrice <= 0 {
			return fmt.Errorf("零租商品%s的单价必须大于 0", label)
		}
		maxDur := MaxDurationFor(pr.PricingMode)
		md := intOr(pr.MinDuration, 1)
		if md < 1 {
			return fmt.Errorf("%s的最短租期必须大于等于 1", label)
		}
		if md > maxDur {
			return fmt.Errorf("%s的最短租期超出上限 %d%s", label, maxDur, DurationUnit(pr.PricingMode))
		}
		// 单价 × 卡数上限 × 周期上限 不得超过单笔订单金额上限, 否则最大订单必然溢出。
		if pr.UnitPrice > MaxOrderTotalFen/cardCap/int64(maxDur) {
			return fmt.Errorf("%s单价过高: 单价×可租卡数×最长租期超出单笔订单上限(100 亿元)", label)
		}
		if pr.Enabled == nil || *pr.Enabled {
			enabled++
		}
		hasNightly = hasNightly || pr.PricingMode == PricingNightly
	}
	if enabled == 0 {
		return fmt.Errorf("至少需要启用一种计费方式")
	}
	if hasNightly && len(prices) > 1 {
		return fmt.Errorf("包夜计费不能与其他计费方式同时开放")
	}
	if av == AvailabilityNightWindow {
		if !hasNightly {
			return fmt.Errorf("夜间窗口商品仅支持包夜计费")
		}
		if fm != FulfillmentManaged {
			return fmt.Errorf("夜间窗口(包夜)商品仅限平台托管模式")
		}
		w := NightWindow{StartMin: intOr(req.NightStartMin, DefaultNightStartMin), EndMin: intOr(req.NightEndMin, DefaultNightEndMin)}
		return w.Validate()
	}
	if hasNightly {
		return fmt.Errorf("包夜计费要求商品可用时段为夜间窗口(availability=night_window)")
	}
	if req.NightStartMin != nil || req.NightEndMin != nil {
		return fmt.Errorf("仅夜间窗口商品可设置夜间时段")
	}
	return nil
}

// priceRowsOf 把已校验的输入转成待保存的价格行。
func priceRowsOf(inputs []PriceInput) []ProductPrice {
	rows := make([]ProductPrice, 0, len(inputs))
	for _, in := range inputs {
		rows = append(rows, ProductPrice{PricingMode: in.PricingMode, UnitPrice: in.UnitPrice,
			MinDuration: max(1, intOr(in.MinDuration, 1)), Enabled: in.Enabled == nil || *in.Enabled})
	}
	sortPrices(rows)
	return rows
}
