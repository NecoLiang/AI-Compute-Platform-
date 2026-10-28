package modeldeploy

import (
	"math"
	"reflect"
	"strings"
	"testing"
)

const kib64 = int64(1) << 10

// spec32B docs/27 §27.3.1 示例: 32B BF16, W=65GiB, K=256KiB/token, O=4GiB。
func spec32B() ModelSpec {
	return ModelSpec{
		ID: 1, ModelKey: "qwen3-32b-bf16", WeightsBytes: 65 * gib64, MinComputeCapability: "8.0",
		ValidTP: []int{1, 2, 4, 8}, KVBytesPerToken: 256 * kib64, RuntimeOverheadBytes: 4 * gib64,
		MaxModelLen: 131072, MinContextTokens: 8192, MinDriverVersion: "535.104.05", CommercialUse: CommercialAllowed,
	}
}

// healthyInput n 张同型空闲健康卡, 所有 F 检查均通过。
func healthyInput(m ModelSpec, n int, vramGiB int64) Input {
	cards := make([]Card, n)
	for i := range cards {
		cards[i] = Card{VRAMTotalBytes: vramGiB * gib64, VRAMUsedBytes: 0}
	}
	return Input{
		Model: m, GPUModel: "H100-80G", ComputeCapability: "9.0", DriverVersion: "550.54.15",
		Cards: cards, NVLink: true, ModelCached: true, GPUMemUtilPct: DefaultGPUMemUtilPct,
		VerifiedGPUModels: []string{"H100-80G"},
	}
}

func mustEval(t *testing.T, in Input) Result {
	t.Helper()
	res, err := Evaluate(in)
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return res
}

func checkByID(t *testing.T, r Result, id string) Check {
	t.Helper()
	for _, c := range r.Checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("缺少检查 %s", id)
	return Check{}
}

type planShape struct {
	TP, DP, Cards, Ctx int
	Tight, Rec         bool
}

func shapes(ps []Plan) []planShape {
	out := []planShape{}
	for _, p := range ps {
		out = append(out, planShape{p.TP, p.DP, p.CardsUsed, p.MaxModelLen, p.ContextTight, p.Recommended})
	}
	return out
}

func TestEvaluateSpecExamples(t *testing.T) {
	// 1×80GB: B = 72−65−4 = 3GiB, T = 3GiB/256KiB = 12288 → 可行但上下文紧张
	r := mustEval(t, healthyInput(spec32B(), 1, 80))
	if !r.Feasible || len(r.Plans) != 1 {
		t.Fatalf("1×80 应可行: %+v", r)
	}
	p := r.Plans[0]
	if p.KVBudgetBytesPerCard != 3*gib64 || p.KVCapacityTokens != 12288 || p.MaxModelLen != 12288 || !p.ContextTight || !p.Recommended {
		t.Fatalf("1×80 计算错误: %+v", p)
	}
	if c := checkByID(t, r, "M2"); c.OK || c.Level != LevelWarning {
		t.Fatalf("1×80 应提示上下文紧张: %+v", c)
	}

	// 2×80GB tp=2: B = 72−32.5−4 = 35.5GiB, T = 35.5GiB×2/256KiB = 290816, 受 max_model_len 截断
	r = mustEval(t, healthyInput(spec32B(), 2, 80))
	want := []planShape{{1, 2, 2, 12288, true, false}, {2, 1, 2, 131072, false, true}}
	if got := shapes(r.Plans); !reflect.DeepEqual(got, want) {
		t.Fatalf("2×80 组合:\n got %+v\nwant %+v", got, want)
	}
	if r.Plans[1].KVBudgetBytesPerCard != 35*gib64+gib64/2 || r.Plans[1].KVCapacityTokens != 290816 {
		t.Fatalf("2×80 tp=2 计算错误: %+v", r.Plans[1])
	}
	if c := checkByID(t, r, "M2"); !c.OK {
		t.Fatalf("推荐 tp=2 时上下文不紧张: %+v", c)
	}

	// 2×24GB: 21.6 − 32.5 − 4 < 0 → 不可行
	r = mustEval(t, healthyInput(spec32B(), 2, 24))
	if r.Feasible || len(r.Plans) != 0 || r.Plans == nil {
		t.Fatalf("2×24 应不可行且 plans 为空数组: %+v", r)
	}
	if c := checkByID(t, r, "M1"); c.OK || c.Level != LevelError {
		t.Fatalf("M1 应为 error: %+v", c)
	}
	for _, m := range r.Memory {
		if m.Feasible || m.Reason == "" {
			t.Fatalf("每个 tp 都应给出不可行原因: %+v", r.Memory)
		}
	}
	if len(r.Memory) != 4 || r.Memory[2].Reason != "需要 4 张卡, 租约只有 2 张" {
		t.Fatalf("tp>k 原因: %+v", r.Memory)
	}
}

func TestEvaluateCardCounts(t *testing.T) {
	// 120GiB 权重: tp=1 装不下, tp=2 起可行(B=72−60−4=8GiB → T=65536)。
	big := spec32B()
	big.ModelKey, big.WeightsBytes = "big-120g", 120*gib64
	cases := []struct {
		name string
		m    ModelSpec
		k    int
		want []planShape
	}{
		{"32B×1", spec32B(), 1, []planShape{{1, 1, 1, 12288, true, true}}},
		{"32B×4 推荐 tp2×dp2", spec32B(), 4, []planShape{
			{1, 4, 4, 12288, true, false}, {2, 2, 4, 131072, false, true}, {4, 1, 4, 131072, false, false}}},
		{"32B×6 tp4 只用 4 卡", spec32B(), 6, []planShape{
			{1, 6, 6, 12288, true, false}, {2, 3, 6, 131072, false, true}, {4, 1, 4, 131072, false, false}}},
		{"32B×8", spec32B(), 8, []planShape{
			{1, 8, 8, 12288, true, false}, {2, 4, 8, 131072, false, true},
			{4, 2, 8, 131072, false, false}, {8, 1, 8, 131072, false, false}}},
		{"120G×1 不可行", big, 1, []planShape{}},
		{"120G×2", big, 2, []planShape{{2, 1, 2, 65536, false, true}}},
		// §27.3.1 示例: 6 卡 → tp=2×dp=3(6 卡全用) 优于 tp=4×dp=1(剩 2 卡)
		{"120G×6 推荐 tp2×dp3", big, 6, []planShape{{2, 3, 6, 65536, false, true}, {4, 1, 4, 131072, false, false}}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := mustEval(t, healthyInput(c.m, c.k, 80))
			if got := shapes(r.Plans); !reflect.DeepEqual(got, c.want) {
				t.Fatalf("\n got %+v\nwant %+v", got, c.want)
			}
			if r.Feasible != (len(c.want) > 0) {
				t.Fatalf("feasible=%v", r.Feasible)
			}
		})
	}
}

func TestEvaluateKVReplicated(t *testing.T) {
	m := spec32B()
	m.MaxModelLen = 1 << 20 // 不截断, 直接观察 T
	r := mustEval(t, healthyInput(m, 2, 80))
	if r.Plans[1].KVCapacityTokens != 290816 || r.Plans[1].MaxModelLen != 290816 {
		t.Fatalf("KV 按 TP 切分: %+v", r.Plans[1])
	}
	m.KVReplicatedAcrossTP = true
	r = mustEval(t, healthyInput(m, 2, 80))
	if r.Plans[1].KVCapacityTokens != 145408 || r.Plans[1].MaxModelLen != 145408 {
		t.Fatalf("KV 跨 TP 复制时 T=B/K: %+v", r.Plans[1])
	}
	if r.Plans[0].KVCapacityTokens != 12288 {
		t.Fatalf("tp=1 不受复制影响: %+v", r.Plans[0])
	}
}

func TestEvaluateBoundaries(t *testing.T) {
	base := spec32B()
	base.ValidTP = []int{1}
	// V×u = 90GiB。
	cases := []struct {
		name     string
		weights  int64
		feasible bool
		tokens   int64
	}{
		{"B=0 不可行", 86 * gib64, false, 0},
		{"B 恰好为正但 T<Lmin", 86*gib64 - 256*kib64, false, 1},
		{"T=Lmin 可行", 84 * gib64, true, 8192},
		{"T=Lmin−1 不可行", 84*gib64 + 1, false, 8191},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m := base
			m.WeightsBytes = c.weights
			r := mustEval(t, healthyInput(m, 1, 100))
			if r.Feasible != c.feasible || r.Memory[0].Feasible != c.feasible || r.Memory[0].KVCapacityTokens != c.tokens {
				t.Fatalf("%+v %+v", r.Feasible, r.Memory)
			}
			if c.feasible && (r.Plans[0].MaxModelLen != 8192 || !r.Plans[0].ContextTight) {
				t.Fatalf("T=Lmin 时建议上下文=Lmin 且紧张: %+v", r.Plans[0])
			}
		})
	}
}

func TestEvaluateGPUMemUtil(t *testing.T) {
	m := spec32B()
	m.WeightsBytes, m.ValidTP = 68*gib64, []int{1}
	in := healthyInput(m, 1, 80)
	if r := mustEval(t, in); r.Feasible {
		t.Fatal("u=0.90: 72−68−4=0 应不可行")
	}
	in.GPUMemUtilPct = 95 // 76−68−4 = 4GiB → T=16384
	r := mustEval(t, in)
	if !r.Feasible || r.Plans[0].KVCapacityTokens != 16384 || r.Basis.GPUMemUtilPct != 95 {
		t.Fatalf("u=0.95 应可行: %+v", r)
	}
	for _, u := range []int{0, 79, 96, 100} {
		in.GPUMemUtilPct = u
		if _, err := Evaluate(in); err == nil {
			t.Errorf("u=%d 越界应报错", u)
		}
	}
}

func TestEvaluateMixedVRAMUsesMinimum(t *testing.T) {
	in := healthyInput(spec32B(), 2, 80)
	in.Cards[1].VRAMTotalBytes = 24 * gib64
	r := mustEval(t, in)
	if r.Feasible || r.Basis.VRAMBytesPerCard != 24*gib64 {
		t.Fatalf("卡间显存不一致按最小者计: %+v", r.Basis)
	}
}

// 目录参数缺失或越界(库中可能有旧行)不报错、不 panic, 判为不可行且 M1 写明"无法判定"。
func TestEvaluateBrokenCatalogInputIsUndecidable(t *testing.T) {
	mut := map[string]func(m *ModelSpec){
		"weights=0":          func(m *ModelSpec) { m.WeightsBytes = 0 },
		"kv=0":               func(m *ModelSpec) { m.KVBytesPerToken = 0 },
		"overhead=0":         func(m *ModelSpec) { m.RuntimeOverheadBytes = 0 },
		"valid_tp 空":         func(m *ModelSpec) { m.ValidTP = nil },
		"valid_tp=0":         func(m *ModelSpec) { m.ValidTP = []int{0} },
		"max_model_len<Lmin": func(m *ModelSpec) { m.MaxModelLen = 1024 },
	}
	for name, f := range mut {
		t.Run(name, func(t *testing.T) {
			m := spec32B()
			f(&m)
			r := mustEval(t, healthyInput(m, 1, 80))
			assertUndecidable(t, r)
		})
	}
}

func assertUndecidable(t *testing.T, r Result) {
	t.Helper()
	m1 := checkByID(t, r, "M1")
	if r.Feasible || len(r.Plans) != 0 || m1.OK || m1.Level != LevelError || !strings.Contains(m1.Message, "无法判定") {
		t.Fatalf("应判为不可行且 M1 写明无法判定: feasible=%v plans=%v M1=%+v", r.Feasible, r.Plans, m1)
	}
}

// 溢出回归: 修复前 kv=2^62 使 100×K 回绕为 0 触发除零 panic; weights≈1.8e17 回绕后 184 PB 模型被判"单卡 80GB 可行"。
func TestEvaluateOverflowInputs(t *testing.T) {
	const maxI = math.MaxInt64
	cases := []struct {
		name string
		mut  func(in *Input)
	}{
		{"kv=2^62 (曾除零 panic)", func(in *Input) { in.Model.KVBytesPerToken = 1 << 62 }},
		{"kv=2^62 且 KV 跨 TP 复制", func(in *Input) {
			in.Model.KVBytesPerToken, in.Model.KVReplicatedAcrossTP = 1<<62, true
		}},
		{"kv 刚超上限", func(in *Input) { in.Model.KVBytesPerToken = MaxKVBytesPerToken + 1 }},
		{"kv=MaxInt64", func(in *Input) { in.Model.KVBytesPerToken = maxI }},
		{"weights≈1.8e17 (184 PB, 曾误判可行)", func(in *Input) { in.Model.WeightsBytes = 184467440737095516 }},
		{"weights 刚超 4 TiB", func(in *Input) { in.Model.WeightsBytes = MaxWeightsBytes + 1 }},
		{"weights=MaxInt64", func(in *Input) { in.Model.WeightsBytes = maxI }},
		{"weights 为负", func(in *Input) { in.Model.WeightsBytes = -1 }},
		{"overhead=2^61", func(in *Input) { in.Model.RuntimeOverheadBytes = 1 << 61 }},
		{"overhead 刚超 256 GiB", func(in *Input) { in.Model.RuntimeOverheadBytes = MaxRuntimeOverheadBytes + 1 }},
		{"max_model_len 超上限", func(in *Input) { in.Model.MaxModelLen = MaxModelLenLimit + 1 }},
		{"valid_tp 超 64", func(in *Input) { in.Model.ValidTP = []int{1, 1 << 40} }},
		{"valid_tp 为负", func(in *Input) { in.Model.ValidTP = []int{-8} }},
		{"卡显存=2^62", func(in *Input) {
			for i := range in.Cards {
				in.Cards[i].VRAMTotalBytes = 1 << 62
			}
		}},
		{"卡显存=MaxInt64", func(in *Input) {
			for i := range in.Cards {
				in.Cards[i].VRAMTotalBytes = maxI
			}
		}},
		{"卡显存为负", func(in *Input) { in.Cards[0].VRAMTotalBytes = -80 * gib64 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := healthyInput(spec32B(), 8, 80)
			c.mut(&in)
			r, err := Evaluate(in)
			if err != nil {
				t.Fatalf("越界输入不应返回 error: %v", err)
			}
			assertUndecidable(t, r)
		})
	}
}

// 恰在上限上的目录参数照常计算, 不被误判为"无法判定"。
func TestEvaluateAtBoundsStillComputes(t *testing.T) {
	m := spec32B()
	m.WeightsBytes, m.ValidTP = MaxWeightsBytes, []int{8, 64}
	m.KVBytesPerToken, m.RuntimeOverheadBytes, m.MaxModelLen = MaxKVBytesPerToken, MaxRuntimeOverheadBytes, MaxModelLenLimit
	in := healthyInput(m, 64, 1024) // 64 张 1 TiB 卡: 单卡显存恰在上限
	r := mustEval(t, in)
	if c := checkByID(t, r, "M1"); strings.Contains(c.Message, "无法判定") || len(r.Memory) != 2 {
		t.Fatalf("上限值应正常计算: %+v %+v", c, r.Memory)
	}
	// tp=8: Bnum = 1TiB×90×8 − 100×4TiB − 100×256GiB×8 > 0; T = Bnum/(100×64MiB)
	ev := r.Memory[0]
	wantNum := int64(1<<40)*90*8 - 100*MaxWeightsBytes - 100*MaxRuntimeOverheadBytes*8
	if ev.TP != 8 || ev.KVCapacityTokens != wantNum/(100*MaxKVBytesPerToken) || ev.KVBudgetBytesPerCard != wantNum/800 {
		t.Fatalf("tp=8 计算错误: %+v", ev)
	}
}

// 租约侧的越界数值(显存占用、缓存容量)不 panic, 对应检查判为不通过。
func TestEvaluateLeaseDataOverflow(t *testing.T) {
	const maxI = math.MaxInt64
	cases := []struct {
		name  string
		check string
		mut   func(in *Input)
	}{
		{"显存占用=MaxInt64", "F3", func(in *Input) { in.Cards[0].VRAMUsedBytes = maxI }},
		{"显存占用为负", "F3", func(in *Input) { in.Cards[0].VRAMUsedBytes = -1 }},
		{"显存占用超过总量", "F3", func(in *Input) { in.Cards[0].VRAMUsedBytes = 81 * gib64 }},
		{"缓存容量相加溢出", "F4", func(in *Input) {
			in.ModelCached, in.CacheFreeBytes, in.CacheEvictableBytes = false, maxI, maxI
		}},
		{"缓存容量×10 溢出", "F4", func(in *Input) { in.ModelCached, in.CacheFreeBytes = false, maxI/2 }},
		{"缓存容量为负", "F4", func(in *Input) { in.ModelCached, in.CacheFreeBytes, in.CacheEvictableBytes = false, -1, 200*gib64 }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := healthyInput(spec32B(), 2, 80)
			c.mut(&in)
			r := mustEval(t, in)
			if ch := checkByID(t, r, c.check); r.Feasible || ch.OK || ch.Level != LevelError {
				t.Fatalf("%s 应不通过: %+v", c.check, ch)
			}
		})
	}
}

// 极值组合扫描: 任何输入都不 panic, 且越界组合不会被判为可行。
func TestEvaluateNeverPanics(t *testing.T) {
	vals := []int64{math.MinInt64, -1, 0, 1, 1 << 30, 1 << 40, 1 << 62, math.MaxInt64}
	for _, w := range vals {
		for _, k := range vals {
			for _, o := range vals {
				for _, v := range vals {
					for _, rep := range []bool{false, true} {
						m := spec32B()
						m.WeightsBytes, m.KVBytesPerToken, m.RuntimeOverheadBytes, m.KVReplicatedAcrossTP = w, k, o, rep
						in := healthyInput(m, 8, 80)
						for i := range in.Cards {
							in.Cards[i].VRAMTotalBytes = v
						}
						func() {
							defer func() {
								if p := recover(); p != nil {
									t.Fatalf("panic w=%d k=%d o=%d v=%d rep=%v: %v", w, k, o, v, rep, p)
								}
							}()
							r := mustEval(t, in)
							inRange := w > 0 && w <= MaxWeightsBytes && k > 0 && k <= MaxKVBytesPerToken &&
								o > 0 && o <= MaxRuntimeOverheadBytes && v > 0 && v <= MaxCardVRAMBytes
							if r.Feasible && !inRange {
								t.Fatalf("越界输入被判可行 w=%d k=%d o=%d v=%d", w, k, o, v)
							}
						}()
					}
				}
			}
		}
	}
}

func TestCheckedArithmetic(t *testing.T) {
	const maxI, minI = math.MaxInt64, math.MinInt64
	add := []struct {
		a, b int64
		ok   bool
	}{{1, 2, true}, {maxI, 1, false}, {minI, -1, false}, {maxI, minI, true}, {maxI - 1, 1, true}}
	for _, c := range add {
		if r, ok := addInt64(c.a, c.b); ok != c.ok || (ok && r != c.a+c.b) {
			t.Errorf("addInt64(%d,%d)=%d,%v", c.a, c.b, r, ok)
		}
	}
	sub := []struct {
		a, b int64
		ok   bool
	}{{1, 2, true}, {minI, 1, false}, {maxI, -1, false}, {0, minI, false}, {-1, minI, true}}
	for _, c := range sub {
		if r, ok := subInt64(c.a, c.b); ok != c.ok || (ok && r != c.a-c.b) {
			t.Errorf("subInt64(%d,%d)=%d,%v", c.a, c.b, r, ok)
		}
	}
	mul := []struct {
		a, b int64
		ok   bool
	}{{0, maxI, true}, {3, 4, true}, {100, 1 << 62, false}, {1 << 32, 1 << 31, false}, {1 << 31, 1 << 31, true},
		{-1, minI, false}, {minI, -1, false}, {-1, maxI, true}, {maxI, 2, false}}
	for _, c := range mul {
		if r, ok := mulInt64(c.a, c.b); ok != c.ok || (ok && r != c.a*c.b) {
			t.Errorf("mulInt64(%d,%d)=%d,%v", c.a, c.b, r, ok)
		}
	}
	if _, ok := mulAll(100, 1<<40, 90, 64); !ok {
		t.Error("mulAll 安全范围内不应报溢出")
	}
	if _, ok := mulAll(100, 1<<62); ok {
		t.Error("mulAll 应检测溢出")
	}
}

func TestEvaluateDeterministicAndOrdered(t *testing.T) {
	in := healthyInput(spec32B(), 8, 80)
	a, b := mustEval(t, in), mustEval(t, in)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("同一输入应得出同一结果")
	}
	var ids []string
	for _, c := range a.Checks {
		ids = append(ids, c.ID)
	}
	if want := []string{"M1", "M2", "F1", "F2", "F3", "F4", "F5", "F6", "F7", "F8"}; !reflect.DeepEqual(ids, want) {
		t.Fatalf("检查顺序: %v", ids)
	}
	for _, c := range a.Checks {
		if !c.OK || c.Level != LevelInfo {
			t.Fatalf("健康输入应全部通过: %+v", c)
		}
	}
	if a.Basis.Cards != 8 || a.Basis.VRAMBytesPerCard != 80*gib64 || a.Basis.ModelKey != "qwen3-32b-bf16" {
		t.Fatalf("basis: %+v", a.Basis)
	}
}

func TestEvaluateNoCards(t *testing.T) {
	r := mustEval(t, healthyInput(spec32B(), 0, 80))
	if r.Feasible || checkByID(t, r, "M1").OK || checkByID(t, r, "F3").OK {
		t.Fatalf("0 张卡应不可行: %+v", r)
	}
}
