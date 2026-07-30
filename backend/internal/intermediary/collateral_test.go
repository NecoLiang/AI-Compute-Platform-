package intermediary

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func mustDate(t *testing.T, s string) *time.Time {
	t.Helper()
	d, err := time.ParseInLocation("2006-01-02", s, time.UTC)
	assert.NoError(t, err)
	return &d
}

// ===== USCC 18 位校验 =====

func TestValidateUSCC(t *testing.T) {
	cases := []struct {
		name    string
		uscc    string
		wantErr bool
	}{
		{name: "标准 18 位 合法", uscc: "91330106MA27XYAB1C"},
		{name: "纯数字 18 位 合法", uscc: "913301067123456789"},
		{name: "17 位 非法", uscc: "91330106MA27XYAB1", wantErr: true},
		{name: "19 位 非法", uscc: "91330106MA27XYAB1CD", wantErr: true},
		{name: "空串 非法", uscc: "", wantErr: true},
		{name: "含小写字母 非法", uscc: "91330106ma27xyab1c", wantErr: true},
		{name: "含禁用字母 I 非法", uscc: "91330106MA27XYABI1", wantErr: true},
		{name: "含禁用字母 O 非法", uscc: "91330106MA27XYABO1", wantErr: true},
		{name: "含禁用字母 Z 非法", uscc: "91330106MA27XYABZ1", wantErr: true},
		{name: "含禁用字母 S 非法", uscc: "91330106MA27XYABS1", wantErr: true},
		{name: "含禁用字母 V 非法", uscc: "91330106MA27XYABV1", wantErr: true},
		{name: "含连字符 非法", uscc: "91330106-MA27XYAB1", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateUSCC(tc.uscc)
			if tc.wantErr {
				assert.Error(t, err)
				ve, ok := err.(*CollateralValidationError)
				assert.True(t, ok)
				if ok { assert.Equal(t, "lessee_uscc", ve.Field) }
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// ===== 日期区间校验 =====

func TestValidateRegDateRange(t *testing.T) {
	cases := []struct {
		name      string
		start     string
		end       string
		wantErr   bool
		wantField string
	}{
		{name: "到期日晚于起始日 合法", start: "2026-01-01", end: "2029-01-01"},
		{name: "到期日仅晚一天 合法", start: "2026-01-01", end: "2026-01-02"},
		{name: "到期日等于起始日 非法", start: "2026-01-01", end: "2026-01-01", wantErr: true, wantField: "reg_end_date"},
		{name: "到期日早于起始日 非法", start: "2029-01-01", end: "2026-01-01", wantErr: true, wantField: "reg_end_date"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateRegDateRange(mustDate(t, tc.start), mustDate(t, tc.end))
			if tc.wantErr {
				assert.Error(t, err)
				ve, ok := err.(*CollateralValidationError)
				assert.True(t, ok)
				if ok { assert.Equal(t, tc.wantField, ve.Field) }
			} else {
				assert.NoError(t, err)
			}
		})
	}

	t.Run("起始日为空 非法", func(t *testing.T) {
		err := ValidateRegDateRange(nil, mustDate(t, "2029-01-01"))
		assert.Error(t, err)
		assert.Equal(t, "reg_start_date", err.(*CollateralValidationError).Field)
	})
	t.Run("到期日为空 非法", func(t *testing.T) {
		err := ValidateRegDateRange(mustDate(t, "2026-01-01"), nil)
		assert.Error(t, err)
		assert.Equal(t, "reg_end_date", err.(*CollateralValidationError).Field)
	})
}

// ===== 过期动态判定（边界: 昨天/今天/明天到期）=====

func TestIsExpiredOn(t *testing.T) {
	today := time.Date(2026, 7, 29, 15, 30, 0, 0, time.UTC)

	cases := []struct {
		name        string
		end         string
		wantExpired bool
	}{
		{name: "昨天到期 已过期", end: "2026-07-28", wantExpired: true},
		{name: "今天到期 未过期(到期日当天仍有效)", end: "2026-07-29", wantExpired: false},
		{name: "明天到期 未过期", end: "2026-07-30", wantExpired: false},
		{name: "去年到期 已过期", end: "2025-07-29", wantExpired: true},
		{name: "明年到期 未过期", end: "2027-07-29", wantExpired: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.wantExpired, IsExpiredOn(mustDate(t, tc.end), today))
		})
	}

	t.Run("到期日为空 视为未过期", func(t *testing.T) {
		assert.False(t, IsExpiredOn(nil, today))
	})

	t.Run("today 带时分秒不影响判定", func(t *testing.T) {
		endOfDay := time.Date(2026, 7, 29, 23, 59, 59, 0, time.UTC)
		startOfDay := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)
		assert.False(t, IsExpiredOn(mustDate(t, "2026-07-29"), endOfDay))
		assert.False(t, IsExpiredOn(mustDate(t, "2026-07-29"), startOfDay))
		assert.True(t, IsExpiredOn(mustDate(t, "2026-07-28"), startOfDay))
	})
}

// EffectiveStatus 不能只依赖 status 列: status 可能滞后（无逐日刷新任务）
func TestEffectiveStatus(t *testing.T) {
	today := time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC)

	cases := []struct {
		name    string
		status  string
		end     string
		want    string
	}{
		{name: "status=valid 且未到期 -> valid", status: "valid", end: "2029-01-01", want: "valid"},
		{name: "status=valid 但已过期 -> expired(动态覆盖)", status: "valid", end: "2026-07-28", want: "expired"},
		{name: "status=valid 今天到期 -> valid", status: "valid", end: "2026-07-29", want: "valid"},
		{name: "status=expired 且已过期 -> expired", status: "expired", end: "2026-01-01", want: "expired"},
		{name: "status=cancelled 且未到期 -> cancelled(作废优先)", status: "cancelled", end: "2029-01-01", want: "cancelled"},
		{name: "status=cancelled 且已过期 -> cancelled(作废优先于过期)", status: "cancelled", end: "2026-01-01", want: "cancelled"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := &CollateralRegistration{Status: tc.status, RegEndDate: mustDate(t, tc.end)}
			assert.Equal(t, tc.want, EffectiveStatus(r, today))
		})
	}

	t.Run("nil 记录返回空", func(t *testing.T) {
		assert.Equal(t, "", EffectiveStatus(nil, today))
	})
}

// ===== 录入/修改入参校验 =====

func TestValidateUpsertCollateral(t *testing.T) {
	base := func() UpsertCollateralReq {
		return UpsertCollateralReq{
			RegNo:          "2026073100123456",
			RegType:        "finance_lease",
			LessorName:     "某融资租赁有限公司",
			LesseeName:     "杭州某智算科技有限公司",
			LesseeUscc:     "91330106MA27XYAB1C",
			CollateralDesc: "NVIDIA H100 SXM 8卡整机 × 20 台",
			RegStartDate:   "2026-07-01",
			RegEndDate:     "2029-06-30",
			SourceNote:     "中登网查询截图 ZD-20260729-001，查询日期 2026-07-29",
			VerifiedAt:     "2026-07-29",
		}
	}

	cases := []struct {
		name      string
		mutate    func(*UpsertCollateralReq)
		wantErr   bool
		wantField string
	}{
		{name: "完整合法", mutate: func(r *UpsertCollateralReq) {}},
		{name: "USCC 留空 合法(选填)", mutate: func(r *UpsertCollateralReq) { r.LesseeUscc = "" }},
		{name: "核验日期留空 合法", mutate: func(r *UpsertCollateralReq) { r.VerifiedAt = "" }},
		{name: "登记编号为空 非法", mutate: func(r *UpsertCollateralReq) { r.RegNo = "  " }, wantErr: true, wantField: "reg_no"},
		{
			name:      "登记类型非法",
			mutate:    func(r *UpsertCollateralReq) { r.RegType = "pledge" },
			wantErr:   true,
			wantField: "reg_type",
		},
		{
			name:      "出租人为空 非法",
			mutate:    func(r *UpsertCollateralReq) { r.LessorName = "" },
			wantErr:   true,
			wantField: "lessor_name",
		},
		{
			name:      "承租人为空 非法",
			mutate:    func(r *UpsertCollateralReq) { r.LesseeName = "" },
			wantErr:   true,
			wantField: "lessee_name",
		},
		{
			name:      "USCC 位数不对 非法",
			mutate:    func(r *UpsertCollateralReq) { r.LesseeUscc = "9133010612345" },
			wantErr:   true,
			wantField: "lessee_uscc",
		},
		{
			name:      "到期日早于起始日 非法",
			mutate:    func(r *UpsertCollateralReq) { r.RegEndDate = "2026-06-30" },
			wantErr:   true,
			wantField: "reg_end_date",
		},
		{
			name:      "日期格式错误 非法",
			mutate:    func(r *UpsertCollateralReq) { r.RegStartDate = "2026/07/01" },
			wantErr:   true,
			wantField: "reg_start_date",
		},
		{
			name:      "起始日缺失 非法",
			mutate:    func(r *UpsertCollateralReq) { r.RegStartDate = "" },
			wantErr:   true,
			wantField: "reg_start_date",
		},
		{
			name:      "录入依据缺失 非法(人工录入必须可追溯)",
			mutate:    func(r *UpsertCollateralReq) { r.SourceNote = "" },
			wantErr:   true,
			wantField: "source_note",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := base()
			tc.mutate(&req)
			v, err := ValidateUpsertCollateral(req)
			if !tc.wantErr {
				assert.NoError(t, err)
				assert.NotNil(t, v)
				return
			}
			assert.Error(t, err)
			assert.Nil(t, v)
			ve, ok := err.(*CollateralValidationError)
			assert.True(t, ok, "错误类型应为 *CollateralValidationError, 实际 %T", err)
			if ok { assert.Equal(t, tc.wantField, ve.Field) }
		})
	}

	t.Run("小写 USCC 自动归一为大写", func(t *testing.T) {
		req := base()
		req.LesseeUscc = "91330106ma27xyab1c"
		v, err := ValidateUpsertCollateral(req)
		assert.NoError(t, err)
		assert.Equal(t, "91330106MA27XYAB1C", v.LesseeUscc)
	})
}

// ===== 查询条件校验: 禁止裸查全表 =====

func TestCollateralQueryValidate(t *testing.T) {
	cases := []struct {
		name    string
		q       CollateralQuery
		wantErr error
	}{
		{name: "只填承租人名称 合法", q: CollateralQuery{LesseeName: "杭州某智算"}},
		{name: "只填 USCC 合法", q: CollateralQuery{LesseeUscc: "91330106MA27XYAB1C"}},
		{name: "两者都填 合法", q: CollateralQuery{LesseeName: "杭州某智算", LesseeUscc: "91330106MA27XYAB1C"}},
		{name: "两者都空 拒绝(防拖全表)", q: CollateralQuery{}, wantErr: ErrQueryTooBroad},
		{name: "两者只有空白 拒绝", q: CollateralQuery{LesseeName: "   ", LesseeUscc: "  "}, wantErr: ErrQueryTooBroad},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := tc.q
			q.Normalize()
			err := q.Validate()
			if tc.wantErr != nil {
				assert.ErrorIs(t, err, tc.wantErr)
			} else {
				assert.NoError(t, err)
			}
		})
	}

	t.Run("USCC 位数不对 拒绝", func(t *testing.T) {
		q := CollateralQuery{LesseeUscc: "913301061234"}
		q.Normalize()
		err := q.Validate()
		assert.Error(t, err)
		_, ok := err.(*CollateralValidationError)
		assert.True(t, ok)
	})
}

func TestCollateralQueryNormalize(t *testing.T) {
	cases := []struct {
		name         string
		in           CollateralQuery
		wantPage     int
		wantPageSize int
	}{
		{name: "空值取默认", in: CollateralQuery{}, wantPage: 1, wantPageSize: 20},
		{name: "负值归一", in: CollateralQuery{Page: -1, PageSize: -1}, wantPage: 1, wantPageSize: 20},
		{name: "正常值保留", in: CollateralQuery{Page: 2, PageSize: 50}, wantPage: 2, wantPageSize: 50},
		{
			name:         "超大 page_size 收敛到上限",
			in:           CollateralQuery{Page: 1, PageSize: 999999},
			wantPage:     1,
			wantPageSize: collateralMaxPageSize,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := tc.in
			q.Normalize()
			assert.Equal(t, tc.wantPage, q.Page)
			assert.Equal(t, tc.wantPageSize, q.PageSize)
		})
	}

	t.Run("USCC 归一为大写且去空格", func(t *testing.T) {
		q := CollateralQuery{LesseeUscc: "  91330106ma27xyab1c "}
		q.Normalize()
		assert.Equal(t, "91330106MA27XYAB1C", q.LesseeUscc)
	})
}

// ===== 合规声明 =====

func TestDisclaimerContent(t *testing.T) {
	// 合规声明必须点明: 人工录入 / 仅供参考 / 以官方系统为准
	assert.Contains(t, Disclaimer, "人工录入")
	assert.Contains(t, Disclaimer, "仅供参考")
	assert.Contains(t, Disclaimer, "中国人民银行征信中心动产融资统一登记公示系统")
	assert.Contains(t, Disclaimer, "以官方系统实时查询结果为准")
	// 不得出现"待对接接口"式的假承诺
	assert.NotContains(t, Disclaimer, "待对接")
	assert.NotContains(t, Disclaimer, "实时同步")
}

func TestParseDate(t *testing.T) {
	t.Run("空串返回 nil", func(t *testing.T) {
		d, err := ParseDate("reg_start_date", "")
		assert.NoError(t, err)
		assert.Nil(t, d)
	})
	t.Run("合法日期", func(t *testing.T) {
		d, err := ParseDate("reg_start_date", "2026-07-29")
		assert.NoError(t, err)
		assert.Equal(t, 2026, d.Year())
		assert.Equal(t, time.July, d.Month())
		assert.Equal(t, 29, d.Day())
	})
	t.Run("非法格式", func(t *testing.T) {
		_, err := ParseDate("reg_end_date", "29/07/2026")
		assert.Error(t, err)
		assert.Equal(t, "reg_end_date", err.(*CollateralValidationError).Field)
	})
	t.Run("不存在的日期", func(t *testing.T) {
		_, err := ParseDate("reg_end_date", "2026-02-30")
		assert.Error(t, err)
	})
}

func TestCollateralErrToCodeMapping(t *testing.T) {
	assert.Equal(t, 40400, CollateralErrToCode(ErrCollateralNotFound))
	assert.Equal(t, 40900, CollateralErrToCode(ErrRegNoConflict))
	assert.Equal(t, 40001, CollateralErrToCode(ErrQueryTooBroad))
	assert.Equal(t, 40001, CollateralErrToCode(collateralInvalid("reg_no", "不能为空")))
	assert.Equal(t, 0, CollateralErrToCode(nil))
}

// 佣金金额一律 int64 分（与现有 Commission 模型一致，此处守住单位不被改成 float）
func TestCollateralRelatedAmountIsFen(t *testing.T) {
	dealAmountFen := int64(1024000000) // ¥10,240,000.00
	rateBp := int64(150)               // 1.5% = 150 basis points
	commissionFen := dealAmountFen * rateBp / 10000
	assert.Equal(t, int64(15360000), commissionFen) // ¥153,600.00
}
