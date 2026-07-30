package equipment

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func yearPtr(y int) *int { return &y }

// 校验矩阵: 一手/二手 × 面议/定价 × 年份边界 × 数量边界
func TestValidateCreateProduct(t *testing.T) {
	const currentYear = 2026

	base := func() CreateProductReq {
		return CreateProductReq{
			Title:         "H100 SXM 整机 8卡",
			EquipmentType: "gpu_server",
			ConditionType: "new",
			Quantity:      10,
			UnitPrice:     int64(128000000), // ¥1,280,000.00 = 128000000 分
			Region:        "杭州",
		}
	}

	cases := []struct {
		name      string
		mutate    func(*CreateProductReq)
		wantErr   bool
		wantField string
	}{
		// ---- 一手 ----
		{name: "一手定价 合法", mutate: func(r *CreateProductReq) {}, wantErr: false},
		{
			name:   "一手不填出厂年份 合法",
			mutate: func(r *CreateProductReq) { r.ManufactureYear = nil },
		},
		{
			name:      "一手填未来年份 非法",
			mutate:    func(r *CreateProductReq) { r.ManufactureYear = yearPtr(currentYear + 1) },
			wantErr:   true,
			wantField: "manufacture_year",
		},

		// ---- 二手 ----
		{
			name: "二手带年份 合法",
			mutate: func(r *CreateProductReq) {
				r.ConditionType = "used"
				r.ManufactureYear = yearPtr(2022)
				r.UsageDesc = "机房恒温运行 2 年，无水冷改装"
			},
		},
		{
			name:      "二手缺出厂年份 非法",
			mutate:    func(r *CreateProductReq) { r.ConditionType = "used"; r.ManufactureYear = nil },
			wantErr:   true,
			wantField: "manufacture_year",
		},
		{
			name:      "二手年份低于下限 2009 非法",
			mutate:    func(r *CreateProductReq) { r.ConditionType = "used"; r.ManufactureYear = yearPtr(2009) },
			wantErr:   true,
			wantField: "manufacture_year",
		},
		{
			name:   "二手年份等于下限 2010 合法",
			mutate: func(r *CreateProductReq) { r.ConditionType = "used"; r.ManufactureYear = yearPtr(MinManufactureYear) },
		},
		{
			name:   "二手年份等于当前年 合法",
			mutate: func(r *CreateProductReq) { r.ConditionType = "used"; r.ManufactureYear = yearPtr(currentYear) },
		},
		{
			name:      "二手年份超过当前年 非法",
			mutate:    func(r *CreateProductReq) { r.ConditionType = "used"; r.ManufactureYear = yearPtr(currentYear + 1) },
			wantErr:   true,
			wantField: "manufacture_year",
		},

		// ---- 面议 / 定价 ----
		{
			name:   "面议且单价为 0 合法",
			mutate: func(r *CreateProductReq) { r.PriceNegotiable = true; r.UnitPrice = 0 },
		},
		{
			name:      "面议但带了单价 非法",
			mutate:    func(r *CreateProductReq) { r.PriceNegotiable = true; r.UnitPrice = 100 },
			wantErr:   true,
			wantField: "unit_price",
		},
		{
			name:      "非面议单价为 0 非法",
			mutate:    func(r *CreateProductReq) { r.PriceNegotiable = false; r.UnitPrice = 0 },
			wantErr:   true,
			wantField: "unit_price",
		},
		{
			name:      "非面议单价为负 非法",
			mutate:    func(r *CreateProductReq) { r.UnitPrice = -1 },
			wantErr:   true,
			wantField: "unit_price",
		},
		{
			name:   "非面议单价 1 分 合法",
			mutate: func(r *CreateProductReq) { r.UnitPrice = 1 },
		},

		// ---- 数量边界 ----
		{name: "数量为 0 非法", mutate: func(r *CreateProductReq) { r.Quantity = 0 }, wantErr: true, wantField: "quantity"},
		{name: "数量为负 非法", mutate: func(r *CreateProductReq) { r.Quantity = -5 }, wantErr: true, wantField: "quantity"},
		{name: "数量为 1 合法", mutate: func(r *CreateProductReq) { r.Quantity = 1 }},
		{name: "数量等于上限 合法", mutate: func(r *CreateProductReq) { r.Quantity = MaxQuantity }},
		{
			name:      "数量超过上限 非法",
			mutate:    func(r *CreateProductReq) { r.Quantity = MaxQuantity + 1 },
			wantErr:   true,
			wantField: "quantity",
		},

		// ---- 其他字段 ----
		{name: "标题为空 非法", mutate: func(r *CreateProductReq) { r.Title = "   " }, wantErr: true, wantField: "title"},
		{
			name:      "设备类型非法",
			mutate:    func(r *CreateProductReq) { r.EquipmentType = "spaceship" },
			wantErr:   true,
			wantField: "equipment_type",
		},
		{
			name:      "新旧程度非法",
			mutate:    func(r *CreateProductReq) { r.ConditionType = "refurbished" },
			wantErr:   true,
			wantField: "condition_type",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := base()
			tc.mutate(&req)
			err := ValidateCreateProduct(req, currentYear)
			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			assert.Error(t, err)
			ve, ok := err.(*ValidationError)
			assert.True(t, ok, "错误类型应为 *ValidationError, 实际 %T", err)
			if ok && tc.wantField != "" {
				assert.Equal(t, tc.wantField, ve.Field)
			}
		})
	}
}

func TestValidateInquiryQuantity(t *testing.T) {
	cases := []struct {
		name      string
		qty       int
		available int
		wantErr   bool
	}{
		{name: "数量为 1 库存 1 合法", qty: 1, available: 1},
		{name: "数量等于库存 合法", qty: 10, available: 10},
		{name: "数量小于库存 合法", qty: 3, available: 10},
		{name: "数量为 0 非法", qty: 0, available: 10, wantErr: true},
		{name: "数量为负 非法", qty: -1, available: 10, wantErr: true},
		{name: "数量超过库存 非法", qty: 11, available: 10, wantErr: true},
		{name: "库存为 0 非法", qty: 1, available: 0, wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateInquiryQuantity(tc.qty, tc.available)
			if tc.wantErr {
				assert.Error(t, err)
				ve, ok := err.(*ValidationError)
				assert.True(t, ok)
				if ok { assert.Equal(t, "quantity", ve.Field) }
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateCreateInquiry(t *testing.T) {
	base := func() CreateInquiryReq {
		return CreateInquiryReq{Quantity: 2, ContactName: "张工", ContactPhone: "13800138000"}
	}
	cases := []struct {
		name      string
		mutate    func(*CreateInquiryReq)
		wantErr   bool
		wantField string
	}{
		{name: "合法", mutate: func(r *CreateInquiryReq) {}},
		{name: "联系人为空", mutate: func(r *CreateInquiryReq) { r.ContactName = "  " }, wantErr: true, wantField: "contact_name"},
		{name: "电话为空", mutate: func(r *CreateInquiryReq) { r.ContactPhone = "" }, wantErr: true, wantField: "contact_phone"},
		{name: "数量为 0", mutate: func(r *CreateInquiryReq) { r.Quantity = 0 }, wantErr: true, wantField: "quantity"},
		{
			name:      "数量超上限",
			mutate:    func(r *CreateInquiryReq) { r.Quantity = MaxQuantity + 1 },
			wantErr:   true,
			wantField: "quantity",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := base()
			tc.mutate(&req)
			err := ValidateCreateInquiry(req)
			if !tc.wantErr {
				assert.NoError(t, err)
				return
			}
			assert.Error(t, err)
			ve, ok := err.(*ValidationError)
			assert.True(t, ok)
			if ok { assert.Equal(t, tc.wantField, ve.Field) }
		})
	}
}

func TestProductFilterNormalize(t *testing.T) {
	cases := []struct {
		name         string
		in           ProductFilter
		wantPage     int
		wantPageSize int
	}{
		{name: "空值取默认", in: ProductFilter{}, wantPage: 1, wantPageSize: 20},
		{name: "负页码归一", in: ProductFilter{Page: -3, PageSize: -1}, wantPage: 1, wantPageSize: 20},
		{name: "正常值保留", in: ProductFilter{Page: 3, PageSize: 50}, wantPage: 3, wantPageSize: 50},
		{name: "超大 page_size 收敛到上限", in: ProductFilter{Page: 1, PageSize: 100000}, wantPage: 1, wantPageSize: maxPageSize},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := tc.in
			f.Normalize()
			assert.Equal(t, tc.wantPage, f.Page)
			assert.Equal(t, tc.wantPageSize, f.PageSize)
		})
	}
}

func TestNormalizeSort(t *testing.T) {
	assert.Equal(t, "price_asc", NormalizeSort("price_asc"))
	assert.Equal(t, "price_desc", NormalizeSort("price_desc"))
	assert.Equal(t, "created_at_desc", NormalizeSort("created_at_desc"))
	// 非白名单值必须收敛，防 SQL 注入
	assert.Equal(t, "created_at_desc", NormalizeSort("unit_price; DROP TABLE equipment_products"))
	assert.Equal(t, "created_at_desc", NormalizeSort(""))
}

func TestNormalizeStatusFilter(t *testing.T) {
	for _, s := range []string{"draft", "pending", "active", "sold_out", "offline"} {
		assert.Equal(t, s, NormalizeStatusFilter(s))
	}
	assert.Equal(t, "", NormalizeStatusFilter("deleted"))
	assert.Equal(t, "", NormalizeStatusFilter("' OR 1=1--"))
}

// 金额一律 int64 分，验证大额设备不会溢出也不引入浮点误差
func TestEquipmentAmountIsFenInt64(t *testing.T) {
	unitPrice := int64(128000000) // ¥1,280,000.00
	qty := int64(8)
	total := unitPrice * qty
	assert.Equal(t, int64(1024000000), total) // ¥10,240,000.00
	// 1000 台千万级设备也远未触及 int64 上限
	assert.Greater(t, int64(1)<<62, unitPrice*1000)
}

func TestErrToCodeMapping(t *testing.T) {
	assert.Equal(t, 40400, ErrToCode(ErrProductNotFound))
	assert.Equal(t, 40900, ErrToCode(ErrProductNotActive))
	assert.Equal(t, 40001, ErrToCode(invalid("quantity", "数量必须大于 0")))
	assert.Equal(t, 0, ErrToCode(nil))
}
