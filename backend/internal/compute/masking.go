package compute

import "strings"

// companySuffixes 需要保留的公司类后缀, 按长度从长到短匹配。
var companySuffixes = []string{"股份有限公司", "有限责任公司", "有限公司", "集团", "公司"}

// MaskCompanyName 买家侧供给方名称脱敏 (信息隔离):
// 保留前 2 字(通常是地域)与公司类后缀, 中间打码 —— "北京万象硅芯科技有限公司" → "北京***有限公司"。
// "平台自营"与空串原样返回; 名称过短无从打码时只保留首字。
// 管理端与供给方本人视角不适用本函数, 展示全名。
func MaskCompanyName(name string) string {
	if name == "" || name == "平台自营" {
		return name
	}
	runes := []rune(name)
	suffix := ""
	for _, s := range companySuffixes {
		if strings.HasSuffix(name, s) {
			suffix = s
			break
		}
	}
	const prefixLen = 2
	suffixLen := len([]rune(suffix))
	// 前缀+后缀已覆盖全名(或更多)时, 常规规则会原样泄露, 退化为只留首字。
	if len(runes) <= prefixLen+suffixLen {
		if len(runes) <= 1 {
			return "***"
		}
		return string(runes[:1]) + "***"
	}
	return string(runes[:prefixLen]) + "***" + suffix
}
