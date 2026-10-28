package compute

import (
	"errors"
	"fmt"
)

// errInsufficientStock 人工商品库存不足(40900, 文案沿用历史 "insufficient stock")。
var errInsufficientStock = errors.New("insufficient stock")

// OrderQuote 下单报价结果(单位: 分)。
type OrderQuote struct {
	Price       PriceQuote // 实际采用的计费方式/单价/最短租期
	Quantity    int        // 归一化后的数量(零租=卡数)
	Duration    int        // 归一化后的计费周期数(买断恒为 1)
	TotalAmount int64
	PlatformFee int64
	FeeRateBp   int64
}

// QuoteOrder 下单报价纯函数(docs/26 §26.9): POST /orders 与 GET /products/:id/availability 共用,
// 保证"看到的价"与"实付的价"出自同一段代码。不读库、不校验库存与卡槽余量(那是成交可行性, 不是价格)。
//
//   - 计费方式: PriceFor(省略取展示主价方式); 未开放/已停用 → 40001「该商品未开放X计费」
//   - 包夜: 夜间预订防重与每晚开停机(T-094/T-095)未接入 → errNightlyNotWired(50000)
//   - 托管商品不支持买断
//   - 数量/租期: validateOrderParams(托管分卡商品卡数须在可选卡数内; 其他商品起订量/上限; 租期上下限)
//   - 金额: CalcOrderAmount(int64 分, 溢出检查), 费率 feeRateBp 为万分比
func QuoteOrder(p *Product, pricingMode string, quantity, duration int, feeRateBp int64) (OrderQuote, error) {
	if p == nil {
		return OrderQuote{}, fmt.Errorf("product not found")
	}
	price, err := PriceFor(p, pricingMode)
	if errors.Is(err, ErrPricingModeDisabled) {
		return OrderQuote{}, fmt.Errorf("该商品未开放%s计费", orDefault(pricingModeLabels[pricingMode], pricingMode))
	}
	if err != nil {
		return OrderQuote{}, err
	}
	if price.PricingMode == PricingNightly {
		return OrderQuote{}, errNightlyNotWired
	}
	if p.FulfillmentMode == FulfillmentManaged && price.PricingMode == PricingPerpetual {
		return OrderQuote{}, fmt.Errorf("托管商品不支持买断计费")
	}
	qty, dur, err := validateOrderParams(p, price, quantity, duration, false)
	if err != nil {
		return OrderQuote{}, err
	}
	total, fee, err := CalcOrderAmount(price.UnitPrice, qty, dur, feeRateBp)
	if err != nil {
		return OrderQuote{}, err
	}
	return OrderQuote{Price: price, Quantity: qty, Duration: dur, TotalAmount: total, PlatformFee: fee, FeeRateBp: feeRateBp}, nil
}
