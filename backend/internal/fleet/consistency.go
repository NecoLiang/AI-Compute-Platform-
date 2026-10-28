package fleet

import (
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
)

// CatalogEntry P5/P13 用到的 gpu_catalog 字段。
type CatalogEntry struct {
	ModelName           string   `db:"model_name"`
	Vendor              string   `db:"vendor"`
	VRAMGB              *int     `db:"vram_gb"`
	ProbeBaselineTFLOPS *float64 `db:"probe_baseline_tflops"`
}

var memSuffixRe = regexp.MustCompile(`(?i)[-\s]?(\d+)\s*G(B)?$`)
var tokenSplitRe = regexp.MustCompile(`[^A-Z0-9]+`)

// modelTokens 归一型号: 去掉显存后缀(-80G / 80GB)、厂商前缀, 大写后按非字母数字切分。
func modelTokens(model string) []string {
	m := strings.ToUpper(strings.TrimSpace(model))
	m = memSuffixRe.ReplaceAllString(m, "")
	var out []string
	for _, t := range tokenSplitRe.Split(m, -1) {
		switch t {
		case "", "NVIDIA", "TESLA", "GEFORCE":
			continue
		}
		out = append(out, t)
	}
	return out
}

// memFromModel 从 "H100-80G" 取 80, 作为 catalog 未填 vram_gb 时的期望显存。
func memFromModel(model string) int {
	m := memSuffixRe.FindStringSubmatch(strings.TrimSpace(model))
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// ModelMatches 商品型号的每个归一 token 都必须出现在 nvidia-smi 名称的 token 中:
// "H100-80G" 匹配 "NVIDIA H100 80GB HBM3"; "RTX 4090-24G" 匹配 "NVIDIA GeForce RTX 4090",
// 不匹配 "RTX 4090 D"(多出的 D 不影响), 但 "4090" 不匹配 "4090D"(token 不同)。
func ModelMatches(productModel, smiName string) bool {
	want := modelTokens(productModel)
	if len(want) == 0 {
		return false
	}
	have := map[string]bool{}
	for _, t := range tokenSplitRe.Split(strings.ToUpper(smiName), -1) {
		have[t] = true
	}
	for _, t := range want {
		if !have[t] {
			return false
		}
	}
	return true
}

// vramMatches 显存误差 ≤ 3%。GDDR 卡开启 ECC 时可见显存少 1/16(如 A40/L40S 48G → 46068 MiB),
// 同样视为一致(docs/25 P5 的实现细化)。
func vramMatches(gotMiB, wantGB int) bool {
	want := float64(wantGB) * 1024
	if math.Abs(float64(gotMiB)-want)/want <= 0.03 {
		return true
	}
	eccWant := want * 15 / 16
	return math.Abs(float64(gotMiB)-eccWant)/eccWant <= 0.03
}

// CheckConsistency P5: 型号、卡数、显存逐项比对, 返回全部差异(空=一致)。
func CheckConsistency(productModel string, cat *CatalogEntry, declaredCards int, gpus []GPU) []string {
	var diffs []string
	if cat == nil {
		return []string{fmt.Sprintf("gpu_catalog 未收录商品型号 %q, 无法核对", productModel)}
	}
	if !strings.EqualFold(cat.Vendor, "NVIDIA") {
		return []string{fmt.Sprintf("型号 %q 厂商为 %s, 托管探活当前仅支持 NVIDIA", productModel, cat.Vendor)}
	}
	if len(gpus) != declaredCards {
		diffs = append(diffs, fmt.Sprintf("卡数不一致: 声明 %d 张, 实测 %d 张", declaredCards, len(gpus)))
	}
	wantGB := 0
	if cat.VRAMGB != nil {
		wantGB = *cat.VRAMGB
	} else {
		wantGB = memFromModel(cat.ModelName)
	}
	for _, g := range gpus {
		if !ModelMatches(cat.ModelName, g.Name) {
			diffs = append(diffs, fmt.Sprintf("GPU%d 型号不一致: 商品 %s, 实测 %s", g.Index, cat.ModelName, g.Name))
			continue
		}
		if wantGB > 0 && !vramMatches(g.MemoryMiB, wantGB) {
			diffs = append(diffs, fmt.Sprintf("GPU%d 显存不一致: 期望 %dGB, 实测 %d MiB(误差超过 3%%)", g.Index, wantGB, g.MemoryMiB))
		}
	}
	if wantGB == 0 {
		diffs = append(diffs, fmt.Sprintf("gpu_catalog 型号 %s 未填写显存, 无法核对显存", cat.ModelName))
	}
	return diffs
}

// ResourceReq P8 按卡切分的最低要求。
type ResourceReq struct {
	Cards         int
	ReserveCPU    int
	ReserveMemGB  int
	DiskPerCardGB int // 0 = 未声明(整机交付)
}

// 每卡最低切分额度: 1 核 / 4 GB 内存。低于此无法给租约容器分配可用资源。
const (
	minCPUPerCard   = 1
	minMemGBPerCard = 4
)

// CheckResources P8: 扣除主机保留后按卡切分是否足够, 返回不足项(空=满足)。
func CheckResources(h HostInfo, r ResourceReq) []string {
	var diffs []string
	if h.Arch != "x86_64" {
		diffs = append(diffs, fmt.Sprintf("仅支持 x86_64, 实测 %s", h.Arch))
	}
	if r.Cards <= 0 {
		return append(diffs, "节点声明卡数为 0")
	}
	if cpu := h.CPUCores - r.ReserveCPU; cpu < r.Cards*minCPUPerCard {
		diffs = append(diffs, fmt.Sprintf("CPU 不足: %d 核扣除保留 %d 核后不足每卡 %d 核", h.CPUCores, r.ReserveCPU, minCPUPerCard))
	}
	if mem := h.MemGB - r.ReserveMemGB; mem < r.Cards*minMemGBPerCard {
		diffs = append(diffs, fmt.Sprintf("内存不足: %dGB 扣除保留 %dGB 后不足每卡 %dGB", h.MemGB, r.ReserveMemGB, minMemGBPerCard))
	}
	if r.DiskPerCardGB > 0 && h.DiskFreeGB < r.DiskPerCardGB*r.Cards {
		diffs = append(diffs, fmt.Sprintf("数据盘不足: %s 可用 %dGB, 需要 %d 卡 × %dGB", h.DiskPath, h.DiskFreeGB, r.Cards, r.DiskPerCardGB))
	}
	return diffs
}

// LightGPU 周期轻量探活的单卡读数。
type LightGPU struct {
	Index          int
	UUID           string
	UtilGPU        int
	UtilMem        int
	TempC          int
	UncorrectedECC int // -1 = 不支持
}

// ParseLight 解析 cmdLight 输出。
func ParseLight(out string) ([]LightGPU, error) {
	var res []LightGPU
	for _, l := range nonEmptyLines(out) {
		f := splitCSV(l)
		if len(f) != 6 || !uuidRe.MatchString(f[1]) {
			return nil, fmt.Errorf("轻量探活输出格式异常: %q", truncate(l, 120))
		}
		g := LightGPU{UncorrectedECC: -1}
		var err error
		if g.Index, err = strconv.Atoi(f[0]); err != nil {
			return nil, fmt.Errorf("GPU index 非数字: %q", f[0])
		}
		g.UUID = f[1]
		g.UtilGPU, _ = strconv.Atoi(f[2])
		g.UtilMem, _ = strconv.Atoi(f[3])
		g.TempC, _ = strconv.Atoi(f[4])
		if !naValue(f[5]) {
			if g.UncorrectedECC, err = strconv.Atoi(f[5]); err != nil {
				return nil, fmt.Errorf("ECC 计数无法解析: %q", f[5])
			}
		}
		res = append(res, g)
	}
	if len(res) == 0 {
		return nil, fmt.Errorf("未发现 GPU")
	}
	return res, nil
}
