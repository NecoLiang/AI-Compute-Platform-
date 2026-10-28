package fleet

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func intp(v int) *int { return &v }

func TestModelMatches(t *testing.T) {
	cases := []struct {
		product, smi string
		want         bool
	}{
		{"H100-80G", "NVIDIA H100 80GB HBM3", true},
		{"H100-80G", "NVIDIA H100 PCIe", true},
		{"H800-80G", "NVIDIA H100 80GB HBM3", false},
		{"A100-80G", "NVIDIA A100-SXM4-80GB", true},
		{"A100-40G", "NVIDIA A100-PCIE-40GB", true},
		{"RTX 4090-24G", "NVIDIA GeForce RTX 4090", true},
		{"RTX 4090-24G", "NVIDIA GeForce RTX 4090 D", true},
		{"RTX 4090-24G", "NVIDIA GeForce RTX 3090", false},
		{"L40S-48G", "NVIDIA L40S", true},
		{"L40S-48G", "NVIDIA L40", false},
		{"H20-96G", "NVIDIA H20", true},
		{"", "NVIDIA H20", false},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, ModelMatches(c.product, c.smi), "%s vs %s", c.product, c.smi)
	}
}

func TestCheckConsistency(t *testing.T) {
	gpus, err := ParseGPUList(h100GPUList)
	require.NoError(t, err)
	cat := &CatalogEntry{ModelName: "H100-80G", Vendor: "NVIDIA", VRAMGB: intp(80)}
	assert.Empty(t, CheckConsistency("H100-80G", cat, 8, gpus))

	d := CheckConsistency("H100-80G", cat, 4, gpus)
	require.Len(t, d, 1)
	assert.Contains(t, d[0], "声明 4 张, 实测 8 张")

	h800 := &CatalogEntry{ModelName: "H800-80G", Vendor: "NVIDIA", VRAMGB: intp(80)}
	d = CheckConsistency("H800-80G", h800, 8, gpus)
	assert.Len(t, d, 8, "每张卡逐项列出差异")

	a100_40 := &CatalogEntry{ModelName: "A100-40G", Vendor: "NVIDIA", VRAMGB: intp(40)}
	a100 := []GPU{{Index: 0, Name: "NVIDIA A100-SXM4-80GB", MemoryMiB: 81920}}
	d = CheckConsistency("A100-40G", a100_40, 1, a100)
	require.Len(t, d, 1)
	assert.Contains(t, d[0], "显存不一致")

	// GDDR + ECC: L40S 48G 可见 46068 MiB
	l40s := &CatalogEntry{ModelName: "L40S-48G", Vendor: "NVIDIA", VRAMGB: intp(48)}
	assert.Empty(t, CheckConsistency("L40S-48G", l40s, 1, []GPU{{Index: 0, Name: "NVIDIA L40S", MemoryMiB: 46068}}))

	// catalog 未填 vram_gb 时用型号后缀
	noVram := &CatalogEntry{ModelName: "H100-80G", Vendor: "NVIDIA"}
	assert.Empty(t, CheckConsistency("H100-80G", noVram, 8, gpus))

	assert.NotEmpty(t, CheckConsistency("X", nil, 8, gpus))
	ascend := &CatalogEntry{ModelName: "昇腾910B", Vendor: "华为昇腾"}
	assert.Contains(t, strings.Join(CheckConsistency("昇腾910B", ascend, 8, gpus), ""), "仅支持 NVIDIA")
}

func TestCheckResources(t *testing.T) {
	h := HostInfo{Arch: "x86_64", CPUCores: 224, MemGB: 2015, DiskFreeGB: 12946, DiskPath: "/data"}
	assert.Empty(t, CheckResources(h, ResourceReq{Cards: 8, ReserveCPU: 2, ReserveMemGB: 8, DiskPerCardGB: 500}))

	small := HostInfo{Arch: "x86_64", CPUCores: 8, MemGB: 32, DiskFreeGB: 900, DiskPath: "/"}
	d := CheckResources(small, ResourceReq{Cards: 8, ReserveCPU: 2, ReserveMemGB: 8, DiskPerCardGB: 200})
	assert.Len(t, d, 3)

	arm := HostInfo{Arch: "aarch64", CPUCores: 72, MemGB: 480, DiskFreeGB: 3000}
	assert.Len(t, CheckResources(arm, ResourceReq{Cards: 1, ReserveCPU: 2, ReserveMemGB: 8}), 1)
}

func TestParseLight(t *testing.T) {
	out := "0, GPU-5b1d6c0e-7c3a-3f0e-9a39-0b6a6f1d2c11, 97, 61, 71, 0\n1, GPU-0c3e1b2a-4d5f-6a7b-8c9d-0e1f2a3b4c5d, 0, 0, 33, [N/A]\n"
	g, err := ParseLight(out)
	require.NoError(t, err)
	require.Len(t, g, 2)
	assert.Equal(t, 97, g[0].UtilGPU)
	assert.Equal(t, -1, g[1].UncorrectedECC)
	_, err = ParseLight("No devices were found\n")
	assert.Error(t, err)
}
