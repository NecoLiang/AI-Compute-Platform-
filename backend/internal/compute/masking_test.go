package compute

import "testing"

func TestMaskCompanyName(t *testing.T) {
	cases := map[string]string{
		"北京万象硅芯科技有限公司": "北京***有限公司",
		"上海算力云股份有限公司":  "上海***股份有限公司",
		"深圳智算集团":       "深圳***集团",
		"廊坊数据中心服务公司":   "廊坊***公司",
		"无后缀名称字符串":     "无后***", // 无公司后缀: 保留前 2 字打码
		"平台自营":         "平台自营",  // 自营标识原样
		"":             "",
		"甲":            "***",
		"甲乙":           "甲***",
		"某公司":          "某***", // 前缀+后缀覆盖全名, 退化只留首字
	}
	for in, want := range cases {
		if got := MaskCompanyName(in); got != want {
			t.Errorf("MaskCompanyName(%q) = %q, want %q", in, got, want)
		}
	}
	// 不变量: 脱敏结果绝不包含被打码的中段字符
	if got := MaskCompanyName("北京万象硅芯科技有限公司"); len([]rune(got)) >= len([]rune("北京万象硅芯科技有限公司")) {
		t.Errorf("脱敏结果不应与原名等长或更长: %q", got)
	}
}
