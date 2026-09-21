package compute

import "tokenfactory/pkg/masking"

// MaskCompanyName 买家侧供给方名称脱敏。
// 实现已下沉到 pkg/masking 供设备市场等业务域共用, 这里保留同名转发以兼容既有调用方与测试。
func MaskCompanyName(name string) string { return masking.MaskCompanyName(name) }
