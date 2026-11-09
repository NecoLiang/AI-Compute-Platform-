package compute

import (
	"testing"

	"tokenfactory/pkg/errcode"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestManagedFulfillmentValidation(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(r *CreateProductReq)
		wantErr string
	}{
		{"人工默认", nil, ""},
		{"托管-容器零租", func(r *CreateProductReq) { r.FulfillmentMode, r.DeliveryMode = FulfillmentManaged, "container" }, ""},
		{"托管-整机零租", func(r *CreateProductReq) { r.FulfillmentMode, r.DeliveryMode = FulfillmentManaged, "bare_metal" }, ""},
		{"托管-非零租", func(r *CreateProductReq) {
			r.FulfillmentMode, r.DeliveryMode = FulfillmentManaged, "container"
			r.ProductType = ProductTypeOutright
		}, "仅适用于零租"},
		{"托管-交付方式不支持", func(r *CreateProductReq) { r.FulfillmentMode, r.DeliveryMode = FulfillmentManaged, "vm" }, "container"},
		{"非法模式", func(r *CreateProductReq) { r.FulfillmentMode = "auto" }, "履约模式非法"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := validCardRentalReq()
			if tc.mutate != nil {
				tc.mutate(&req)
			}
			err := ValidateProductReq(NormalizeProductReq(req))
			if tc.wantErr == "" {
				assert.NoError(t, err)
				return
			}
			if assert.Error(t, err) {
				assert.Contains(t, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestManagedStockIgnoredAndDraftStatus(t *testing.T) {
	req := validCardRentalReq()
	req.FulfillmentMode, req.DeliveryMode, req.Stock = FulfillmentManaged, "container", 50
	n := NormalizeProductReq(req)
	assert.Equal(t, 0, n.Stock, "托管商品忽略人工填写的库存")
	assert.NoError(t, ValidateProductReq(n), "托管商品库存由卡槽派生, stock=0 不应报错")
	assert.Equal(t, "draft", initialProductStatus(FulfillmentManaged))
	assert.Equal(t, "pending", initialProductStatus(FulfillmentManual))
}

func TestManagedGateDisabled(t *testing.T) {
	s := &Service{}
	err := s.checkManagedSubmit(1, "H100")
	assert.ErrorIs(t, err, ErrManagedDisabled)
	assert.Equal(t, errcode.ParamInvalid, errcode.CodeOf(err, 0))
	assert.Equal(t, errcode.InternalError, errcode.CodeOf(errManagedLeaseNotWired, 0))
}

// 托管订单误入人工库存归还路径必须报错, 不得把人工增量叠加到派生库存上(在任何写库之前拦截)。
func TestReturnReservedStockRejectsManagedOrder(t *testing.T) {
	zero := 0
	err := (&Service{}).returnReservedStockTx(nil, &Order{OrderNo: "ORD-M", FulfillmentMode: FulfillmentManaged, StockReserved: &zero})
	require.Error(t, err)
	assert.Equal(t, errcode.InternalError, errcode.CodeOf(err, 0))
}
