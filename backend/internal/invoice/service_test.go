package invoice

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidateTaxNo(t *testing.T) {
	cases := []struct {
		name  string
		taxNo string
		want  bool
	}{
		{"统一社会信用代码18位", "91110108MA01C8Y35X", true},
		{"15位老税号", "110108123456789", true},
		{"20位", "12345678901234567890", true},
		{"18位全数字", "123456789012345678", true},
		{"长度14", "91110108MA01C8Y35", false},
		{"长度19", "91110108MA01C8Y35X1", false},
		{"含小写字母", "91110108ma01c8y35x", false},
		{"含特殊字符", "91110108MA01C8Y35-", false},
		{"空串", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ValidateTaxNo(tc.taxNo))
		})
	}
}

func TestCanInvoiceOrder(t *testing.T) {
	cases := []struct {
		status string
		want   bool
	}{
		{"paid", true},
		{"provisioning", true},
		{"active", true},
		{"completed", true},
		{"pending_payment", false},
		{"cancelled", false},
		{"refunding", false},
		{"refunded", false},
		{"frozen", false},
		{"unknown", false},
	}
	for _, tc := range cases {
		t.Run(tc.status, func(t *testing.T) {
			assert.Equal(t, tc.want, CanInvoiceOrder(tc.status))
		})
	}
}

func TestSumInvoiceAmountFen(t *testing.T) {
	orders := []BillableOrder{
		{OrderNo: "ORD-1", TotalAmount: 8920000},
		{OrderNo: "ORD-2", TotalAmount: 15600000},
	}
	assert.Equal(t, int64(24520000), SumInvoiceAmountFen(orders))
	assert.Equal(t, int64(0), SumInvoiceAmountFen(nil))
}

func TestNextInvoiceNoFromMax(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		maxNo  string
		want   string
	}{
		{"无记录从0001起", "INV-2026-", "", "INV-2026-0001"},
		{"递增", "INV-2026-", "INV-2026-0012", "INV-2026-0013"},
		{"进位", "INV-2026-", "INV-2026-0099", "INV-2026-0100"},
		{"跨年后旧编号不影响", "INV-2027-", "INV-2026-0012", "INV-2027-0001"},
		{"异常后缀安全回落", "INV-2026-", "INV-2026-ABCD", "INV-2026-0001"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, NextInvoiceNoFromMax(tc.prefix, tc.maxNo))
		})
	}
}

func TestIsPDF(t *testing.T) {
	assert.True(t, IsPDF([]byte("%PDF-1.7 rest of file")))
	assert.False(t, IsPDF([]byte("%PD")))
	assert.False(t, IsPDF([]byte("not a pdf at all")))
	assert.False(t, IsPDF(nil))
}

func TestSaveTitleReqValidate(t *testing.T) {
	valid := SaveTitleReq{
		CompanyName: "XX 科技有限公司", TaxNo: "91110108MA01C8Y35X",
		BankName: "招商银行北京分行", BankAccount: "110908877665",
	}
	assert.NoError(t, valid.validate())

	cases := []struct {
		name    string
		mutate  func(*SaveTitleReq)
		wantErr string
	}{
		{"缺企业名称", func(r *SaveTitleReq) { r.CompanyName = "" }, "企业名称"},
		{"企业名称纯空格", func(r *SaveTitleReq) { r.CompanyName = "   " }, "企业名称"},
		{"税号格式错", func(r *SaveTitleReq) { r.TaxNo = "abc" }, "纳税人识别号"},
		{"缺开户行", func(r *SaveTitleReq) { r.BankName = "" }, "开户行"},
		{"缺银行账号", func(r *SaveTitleReq) { r.BankAccount = "" }, "银行账号"},
		{"银行账号非数字", func(r *SaveTitleReq) { r.BankAccount = "asdfasdf" }, "银行账号"},
		{"银行账号太短", func(r *SaveTitleReq) { r.BankAccount = "1234567" }, "银行账号"},
		{"银行账号超长", func(r *SaveTitleReq) { r.BankAccount = "123456789012345678901234567890123" }, "银行账号"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := valid
			tc.mutate(&req)
			err := req.validate()
			assert.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}

	// 税号小写输入归一化为大写后合法
	lowered := valid
	lowered.TaxNo = "91110108ma01c8y35x"
	assert.NoError(t, lowered.validate())
	assert.Equal(t, "91110108MA01C8Y35X", lowered.TaxNo)
}

func TestRejectRequiresReason(t *testing.T) {
	s := &Service{}
	assert.Error(t, s.Reject(1, "  "))
}
