package compute

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func splitReq(fulfillment, delivery string, cardCount int, opts []int) CreateProductReq {
	return CreateProductReq{ProductType: ProductTypeCardRental, FulfillmentMode: fulfillment, DeliveryMode: delivery,
		CardCount: cardCount, CardSplitOptions: opts}
}

// T-092: 可选卡数只适用于托管容器零租商品, 取值限平台集合且不超过单机卡数。
func TestValidateSplitOptions(t *testing.T) {
	assert.NoError(t, validateSplitOptions(splitReq(FulfillmentManaged, "container", 8, []int{1, 2, 4, 8})))
	assert.NoError(t, validateSplitOptions(splitReq(FulfillmentManual, "bare_metal", 8, nil)))
	assert.NoError(t, validateSplitOptions(splitReq(FulfillmentManaged, "bare_metal", 8, nil)))
	for _, req := range []CreateProductReq{
		splitReq(FulfillmentManual, "container", 8, []int{1}),   // 人工不可分卡
		splitReq(FulfillmentManaged, "bare_metal", 8, []int{8}), // 整机不可分卡
		splitReq(FulfillmentManaged, "container", 8, nil),       // 至少一个
		splitReq(FulfillmentManaged, "container", 8, []int{3}),  // 不在平台集合
		splitReq(FulfillmentManaged, "container", 4, []int{8}),  // 超过单机卡数
	} {
		err := validateSplitOptions(req)
		require.Error(t, err, "%+v", req)
		assert.Equal(t, 40001, ErrToCode(err), err.Error())
	}
}

func TestProductSplitOptions(t *testing.T) {
	raw := "8,2,2,4"
	p := &Product{ProductType: ProductTypeCardRental, FulfillmentMode: FulfillmentManaged, DeliveryMode: "container", CardCount: 8,
		CardSplitOptionsRaw: &raw}
	assert.Equal(t, []int{2, 4, 8}, p.SplitOptions())
	p.CardSplitOptionsRaw = nil
	assert.Equal(t, []int{1, 2, 4, 6, 8}, p.SplitOptions(), "NULL 取默认集合")
	p.CardCount = 4
	assert.Equal(t, []int{1, 2, 4}, p.SplitOptions())
	p.DeliveryMode = "bare_metal"
	assert.Nil(t, p.SplitOptions())
	assert.Equal(t, "1,2,4", FormatCardSplitOptions([]int{1, 2, 4}))
}

// 托管分卡商品下单: 卡数须在可选集合内, 不受 min_order 与派生库存约束; 其他商品仍按起订量/库存。
func TestValidateOrderParamsSplitOptions(t *testing.T) {
	raw := "2,4"
	managed := &Product{ProductType: ProductTypeCardRental, FulfillmentMode: FulfillmentManaged, DeliveryMode: "container",
		CardCount: 8, MinOrder: 3, Stock: 1, UnitPrice: 100, CardSplitOptionsRaw: &raw}
	q := PriceQuote{PricingMode: PricingHourly, UnitPrice: 100, MinDuration: 1}
	qty, _, err := validateOrderParams(managed, q, 2, 1, true)
	require.NoError(t, err)
	assert.Equal(t, 2, qty)
	_, _, err = validateOrderParams(managed, q, 3, 1, true)
	require.Error(t, err)
	assert.Equal(t, 40001, ErrToCode(err))

	manual := &Product{ProductType: ProductTypeCardRental, FulfillmentMode: FulfillmentManual, DeliveryMode: "bare_metal",
		CardCount: 8, MinOrder: 2, Stock: 3, UnitPrice: 100}
	_, _, err = validateOrderParams(manual, q, 1, 1, true)
	assert.Equal(t, 40001, ErrToCode(err))
	_, _, err = validateOrderParams(manual, q, 4, 1, true)
	assert.Equal(t, 40900, ErrToCode(err))
	_, _, err = validateOrderParams(manual, q, 4, 1, false)
	assert.NoError(t, err)
}
