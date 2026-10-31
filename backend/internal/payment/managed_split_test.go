package payment

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestComputeManagedSplit(t *testing.T) {
	// 实付 10000, 平台费 1000(10%), 供给方 9000。
	m, err := computeManagedSplit(10000, 1000, 9000, 0, 0)
	require.NoError(t, err)
	assert.Equal(t, managedSplit{Platform: 1000, Supplier: 9000}, m)

	// 供给方责任 333: 平台按比例扣 floor(333×1000/10000)=33, 供给方扣 300。
	m, err = computeManagedSplit(10000, 1000, 9000, 333, 0)
	require.NoError(t, err)
	assert.Equal(t, managedSplit{Platform: 967, Supplier: 8700, PlatformDeduct: 33, SupplierDeduct: 300}, m)

	// 平台责任 500: 只扣平台费。
	m, err = computeManagedSplit(10000, 1000, 9000, 0, 500)
	require.NoError(t, err)
	assert.Equal(t, managedSplit{Platform: 500, Supplier: 9000, PlatformDeduct: 500}, m)

	// 平台责任 1500 > 平台费 1000: 超出 500 扣供给方应结, 记平台另行补偿 500。
	m, err = computeManagedSplit(10000, 1000, 9000, 0, 1500)
	require.NoError(t, err)
	assert.Equal(t, managedSplit{Platform: 0, Supplier: 8500, PlatformDeduct: 1000, SupplierDeduct: 500, Compensation: 500}, m)

	// 两种责任叠加: 供给方 2000(平台扣 200, 供给方扣 1800) + 平台 900(平台剩 800, 全扣平台)。
	m, err = computeManagedSplit(10000, 1000, 9000, 2000, 900)
	require.NoError(t, err)
	assert.Equal(t, managedSplit{Platform: 0, Supplier: 7100, PlatformDeduct: 1000, SupplierDeduct: 1900, Compensation: 100}, m)
	assert.Equal(t, int64(10000), m.Platform+m.Supplier+2000+900)

	// 全额退回: 两行归零。
	m, err = computeManagedSplit(10000, 1000, 9000, 10000, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(0), m.Platform+m.Supplier)

	// 大额不溢出 int64。
	m, err = computeManagedSplit(9_000_000_000_000, 900_000_000_000, 8_100_000_000_000, 3_000_000_000_000, 0)
	require.NoError(t, err)
	assert.Equal(t, int64(300_000_000_000), m.PlatformDeduct)

	_, err = computeManagedSplit(10000, 1000, 9000, 6000, 5000)
	assert.Error(t, err, "退款超实付")
	_, err = computeManagedSplit(10000, 1000, 8000, 0, 0)
	assert.Error(t, err, "基数不平")

	// 守恒: 任意组合 退款+应结=实付, 且各项非负。
	for s := int64(0); s <= 10000; s += 137 {
		for p := int64(0); s+p <= 10000; p += 211 {
			m, err := computeManagedSplit(10000, 1234, 8766, s, p)
			require.NoError(t, err)
			require.Equal(t, int64(10000), m.Platform+m.Supplier+s+p)
			require.True(t, m.Platform >= 0 && m.Supplier >= 0 && m.Compensation >= 0)
			require.LessOrEqual(t, m.PlatformDeduct-min(p, 1234), (s*1234)/10000, "平台对供给方责任只按比例扣")
		}
	}
}
