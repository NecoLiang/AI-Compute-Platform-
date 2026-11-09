package modeldeploy

import (
	"fmt"
	"math"
	"sort"
	"strings"
)

// 可行性判定(docs/27 §27.3): 纯函数、确定性, 只依赖 Input, 不读库、不取当前时间、不调用大模型。
// 显存计算全部用 int64 整数运算, 同一输入在任何机器上得出同一结果, 便于事后复核"当时为什么放行"。

// 检查级别。ok=true 时恒为 info; ok=false 时 error 阻断、warning 仅提示、confirm 需买家勾选确认。
const (
	LevelInfo    = "info"
	LevelError   = "error"
	LevelWarning = "warning"
	LevelConfirm = "confirm"
)

// 需要买家确认的提交字段(F7/F8 分开记录, docs/27 §27.3.2)。
const (
	AckLicense  = "license_ack"
	AckUntested = "untested_ack"
)

const (
	DefaultGPUMemUtilPct = 90
	MinGPUMemUtilPct     = 80
	MaxGPUMemUtilPct     = 95
	// maxIdleVRAMUsedPct F3: 租约内各卡当前显存占用上限(百分比)。
	maxIdleVRAMUsedPct = 2
	// contextTightFactor 建议上下文 < Lmin×该倍数时提示"上下文紧张"。
	contextTightFactor = 2
	// MaxCardVRAMBytes 单卡显存的合理上限。租约快照里超出该值(或 ≤0)的显存视为数据异常, 判"无法判定"。
	MaxCardVRAMBytes int64 = 1 << 40 // 1 TiB
)

// ModelSpec 判定所需的目录参数。
type ModelSpec struct {
	ID                   int64
	ModelKey             string
	WeightsBytes         int64
	MinComputeCapability string
	ValidTP              []int
	KVBytesPerToken      int64
	KVReplicatedAcrossTP bool
	RuntimeOverheadBytes int64
	MaxModelLen          int
	MinContextTokens     int
	MinDriverVersion     string
	CommercialUse        string
}

// Alternative 目录中其他 active 条目, F1 不满足时据此列出"其他可用目录条目"。
// 目录没有标识基础模型的字段(display_name 为自由文本、量化变体的 repo 各不相同), 因此不限定为同一模型的
// 量化变体, 而是列出所有架构要求满足该卡型的 active 条目(docs/27 §27.3.2 S5 实现口径)。
type Alternative struct {
	ModelKey             string
	MinComputeCapability string
}

// Card 租约内单卡的实时状态。
type Card struct {
	VRAMTotalBytes   int64
	VRAMUsedBytes    int64
	Faulty           bool
	FatalXidLastHour bool
}

// Input 判定的全部输入。所有字段都是实时数据或目录参数, 不是商品声明值。
type Input struct {
	Model               ModelSpec
	GPUModel            string
	ComputeCapability   string // gpu_catalog.compute_capability, 空串=未登记
	DriverVersion       string
	Cards               []Card
	NVLink              bool
	ModelCached         bool
	CacheFreeBytes      int64
	CacheEvictableBytes int64
	GPUMemUtilPct       int      // u×100, 80–95
	VerifiedGPUModels   []string // 该条目 result=pass 的卡型
	Alternatives        []Alternative
	// Unknown 尚未采集的实时输入(键见 Input* 常量, 值为原因), 对应检查判"无法判定"。
	Unknown map[string]string
}

// Check 单项检查结果。
type Check struct {
	ID      string `json:"id"`
	OK      bool   `json:"ok"`
	Level   string `json:"level"`
	Message string `json:"message"`
	Ack     string `json:"ack,omitempty"`
}

// Plan 一个可行的 (tp, dp) 组合。
type Plan struct {
	TP                   int   `json:"tp"`
	DP                   int   `json:"dp"`
	CardsUsed            int   `json:"cards_used"`
	MaxModelLen          int   `json:"max_model_len"`
	KVBudgetBytesPerCard int64 `json:"kv_budget_bytes_per_card"`
	KVCapacityTokens     int64 `json:"kv_capacity_tokens"`
	ContextTight         bool  `json:"context_tight"`
	Recommended          bool  `json:"recommended"`
}

// MemoryEval valid_tp 中每个并行度的显存计算明细(含不可行的)。
type MemoryEval struct {
	TP                   int    `json:"tp"`
	Feasible             bool   `json:"feasible"`
	KVBudgetBytesPerCard int64  `json:"kv_budget_bytes_per_card"`
	KVCapacityTokens     int64  `json:"kv_capacity_tokens"`
	Reason               string `json:"reason,omitempty"`
}

// Basis 判定所用的输入摘要, 随结果写入部署记录 feasibility 字段。
type Basis struct {
	ModelID              int64  `json:"model_id"`
	ModelKey             string `json:"model_key"`
	GPUModel             string `json:"gpu_model"`
	Cards                int    `json:"cards"`
	VRAMBytesPerCard     int64  `json:"vram_bytes_per_card"`
	GPUMemUtilPct        int    `json:"gpu_mem_util_pct"`
	ComputeCapability    string `json:"compute_capability"`
	DriverVersion        string `json:"driver_version"`
	WeightsBytes         int64  `json:"weights_bytes"`
	KVBytesPerToken      int64  `json:"kv_bytes_per_token"`
	KVReplicatedAcrossTP bool   `json:"kv_replicated_across_tp"`
	RuntimeOverheadBytes int64  `json:"runtime_overhead_bytes"`
	MinContextTokens     int    `json:"min_context_tokens"`
	MaxModelLen          int    `json:"max_model_len"`
	NVLink               bool   `json:"nvlink"`
	ModelCached          bool   `json:"model_cached"`
	// Undetermined 判定时尚未采集的输入及原因(随判定明细留档, 便于事后复核)。
	Undetermined map[string]string `json:"undetermined,omitempty"`
}

// Result 判定结果。Feasible=false 时 Plans 为空, Memory/Checks 仍完整给出原因。
type Result struct {
	Feasible bool         `json:"feasible"`
	Plans    []Plan       `json:"plans"`
	Checks   []Check      `json:"checks"`
	Memory   []MemoryEval `json:"memory"`
	Basis    Basis        `json:"basis"`
}

// MissingAcks 返回需要确认但买家未勾选的检查(F7/F8)。提交部署时非空即 40001。
func (r Result) MissingAcks(licenseAck, untestedAck bool) []Check {
	var out []Check
	for _, c := range r.Checks {
		if c.OK || c.Level != LevelConfirm {
			continue
		}
		if (c.Ack == AckLicense && !licenseAck) || (c.Ack == AckUntested && !untestedAck) {
			out = append(out, c)
		}
	}
	return out
}

// Evaluate 执行 §27.3.1 显存/上下文计算与 §27.3.2 F1–F8 检查。任何输入都不会 panic。
// 返回 error 仅表示 gpu_mem_util 越界(调用方已校验, 属调用方缺陷)。
// 目录参数缺失或超出合理范围(库里可能有校验收紧前写入的旧行)、租约显存/缓存数据异常时
// 不返回 error, 而是判为不可行并在检查项中写明"无法判定"及原因。
func Evaluate(in Input) (Result, error) {
	m := in.Model
	if in.GPUMemUtilPct < MinGPUMemUtilPct || in.GPUMemUtilPct > MaxGPUMemUtilPct {
		return Result{}, fmt.Errorf("gpu_mem_util 须在 0.%d–0.%d 之间", MinGPUMemUtilPct, MaxGPUMemUtilPct)
	}

	k := len(in.Cards)
	var vram int64
	for i, c := range in.Cards {
		if i == 0 || c.VRAMTotalBytes < vram {
			vram = c.VRAMTotalBytes // 卡间显存不一致时按最小者计, 保守
		}
	}
	res := Result{
		Basis: Basis{
			ModelID: m.ID, ModelKey: m.ModelKey, GPUModel: in.GPUModel, Cards: k,
			VRAMBytesPerCard: vram, GPUMemUtilPct: in.GPUMemUtilPct,
			ComputeCapability: in.ComputeCapability, DriverVersion: in.DriverVersion,
			WeightsBytes: m.WeightsBytes, KVBytesPerToken: m.KVBytesPerToken,
			KVReplicatedAcrossTP: m.KVReplicatedAcrossTP, RuntimeOverheadBytes: m.RuntimeOverheadBytes,
			MinContextTokens: m.MinContextTokens, MaxModelLen: m.MaxModelLen,
			NVLink: in.NVLink, ModelCached: in.ModelCached, Undetermined: in.Unknown,
		},
	}

	var plans []Plan
	var m1 Check
	if problem := modelSpecProblem(m); problem != "" {
		// 目录参数不可信时不做显存计算, 避免在越界数值上得出任何结论。
		plans, res.Memory = []Plan{}, []MemoryEval{}
		m1 = fail("M1", LevelError, fmt.Sprintf("目录条目 %s 参数超出合理范围(%s), 无法判定, 请联系平台运营核对目录", m.ModelKey, problem))
	} else {
		plans, res.Memory = evaluateMemory(in, k, vram)
		recommend(plans)
		m1 = memoryCheck(in, k, vram, plans)
	}

	checks := []Check{m1, contextCheck(in, plans)}
	checks = append(checks,
		checkF1(in), checkF2(in), checkF3(in), checkF4(in), checkF5(in, plans),
		checkF6(in), checkF7(in), checkF8(in))
	res.Checks = checks

	res.Feasible = len(plans) > 0
	for _, c := range checks {
		if !c.OK && c.Level == LevelError {
			res.Feasible = false
		}
	}
	if res.Feasible {
		res.Plans = plans
	} else {
		res.Plans = []Plan{}
	}
	return res, nil
}

// modelSpecProblem 目录参数是否在可判定范围内(与 normalizeEntry 的上限一致); 返回空串表示正常。
func modelSpecProblem(m ModelSpec) string {
	switch {
	case m.WeightsBytes <= 0 || m.WeightsBytes > MaxWeightsBytes:
		return fmt.Sprintf("weights_bytes=%d, 应在 1–%d", m.WeightsBytes, MaxWeightsBytes)
	case m.KVBytesPerToken <= 0 || m.KVBytesPerToken > MaxKVBytesPerToken:
		return fmt.Sprintf("kv_bytes_per_token=%d, 应在 1–%d", m.KVBytesPerToken, MaxKVBytesPerToken)
	case m.RuntimeOverheadBytes <= 0 || m.RuntimeOverheadBytes > MaxRuntimeOverheadBytes:
		return fmt.Sprintf("runtime_overhead_bytes=%d, 应在 1–%d", m.RuntimeOverheadBytes, MaxRuntimeOverheadBytes)
	case m.MinContextTokens <= 0 || m.MaxModelLen < m.MinContextTokens || m.MaxModelLen > MaxModelLenLimit:
		return fmt.Sprintf("min_context_tokens=%d, max_model_len=%d, 应满足 0 < min_context_tokens ≤ max_model_len ≤ %d",
			m.MinContextTokens, m.MaxModelLen, MaxModelLenLimit)
	case len(m.ValidTP) == 0:
		return "valid_tp 为空"
	}
	for _, tp := range m.ValidTP {
		if tp < 1 || tp > MaxTP {
			return fmt.Sprintf("valid_tp 含 %d, 应在 1–%d", tp, MaxTP)
		}
	}
	return ""
}

func vramInRange(v int64) bool { return v > 0 && v <= MaxCardVRAMBytes }

// addInt64 / subInt64 / mulInt64 带溢出检测的 int64 运算; ok=false 表示结果溢出。
func addInt64(a, b int64) (int64, bool) {
	c := a + b
	if (b > 0 && c < a) || (b < 0 && c > a) {
		return 0, false
	}
	return c, true
}

func subInt64(a, b int64) (int64, bool) {
	c := a - b
	if (b > 0 && c > a) || (b < 0 && c < a) {
		return 0, false
	}
	return c, true
}

func mulInt64(a, b int64) (int64, bool) {
	if a == 0 || b == 0 {
		return 0, true
	}
	if (a == -1 && b == math.MinInt64) || (b == -1 && a == math.MinInt64) {
		return 0, false
	}
	c := a * b
	if c/b != a {
		return 0, false
	}
	return c, true
}

// mulAll 连乘, 任一步溢出即 ok=false。
func mulAll(xs ...int64) (int64, bool) {
	r := int64(1)
	for _, x := range xs {
		var ok bool
		if r, ok = mulInt64(r, x); !ok {
			return 0, false
		}
	}
	return r, true
}

// kvBudgetNum Bnum = V×U×tp − 100×W − 100×O×tp, 全程溢出检测。
func kvBudgetNum(vram, u, w, o, tp int64) (int64, bool) {
	a, ok1 := mulAll(vram, u, tp)
	b, ok2 := mulAll(100, w)
	c, ok3 := mulAll(100, o, tp)
	if !ok1 || !ok2 || !ok3 {
		return 0, false
	}
	d, ok4 := subInt64(a, b)
	if !ok4 {
		return 0, false
	}
	return subInt64(d, c)
}

// evaluateMemory §27.3.1。精确有理数运算:
//
//	B = V×u − W/tp − O        ⇒ Bnum = V×U×tp − 100×W − 100×O×tp,  B = Bnum / (100×tp)   (U = u×100)
//	T = B×tp/K                ⇒ T = Bnum / (100×K)
//	T = B/K (KV 跨 TP 复制)   ⇒ T = Bnum / (100×tp×K)
//
// 可行: B > 0 且 T ≥ Lmin。
// 调用前 modelSpecProblem 已确认目录参数在范围内; 显存数据异常或任一步溢出均记为该 tp "无法判定"。
func evaluateMemory(in Input, k int, vram int64) ([]Plan, []MemoryEval) {
	m := in.Model
	u := int64(in.GPUMemUtilPct)
	lmin := int64(m.MinContextTokens)
	plans := []Plan{}
	mem := make([]MemoryEval, 0, len(m.ValidTP))
	for _, tp := range m.ValidTP {
		ev := MemoryEval{TP: tp}
		if tp > k {
			ev.Reason = fmt.Sprintf("需要 %d 张卡, 租约只有 %d 张", tp, k)
			mem = append(mem, ev)
			continue
		}
		if !vramInRange(vram) {
			ev.Reason = fmt.Sprintf("卡显存数据缺失或超出合理范围(%d 字节), 无法判定", vram)
			mem = append(mem, ev)
			continue
		}
		t64 := int64(tp)
		bnum, okB := kvBudgetNum(vram, u, m.WeightsBytes, m.RuntimeOverheadBytes, t64)
		den := []int64{100, m.KVBytesPerToken}
		if m.KVReplicatedAcrossTP {
			den = append(den, t64)
		}
		kvDen, okK := mulAll(den...)
		if !okB || !okK || kvDen <= 0 {
			ev.Reason = "参数超出可计算范围, 无法判定"
			mem = append(mem, ev)
			continue
		}
		ev.KVBudgetBytesPerCard = bnum / (100 * t64)
		if bnum <= 0 {
			ev.Reason = "每卡显存不足以容纳权重分片与运行开销"
			mem = append(mem, ev)
			continue
		}
		tokens := bnum / kvDen
		ev.KVCapacityTokens = tokens
		if tokens < lmin {
			ev.Reason = fmt.Sprintf("KV 可容纳约 %d tokens, 低于最低上下文 %d", tokens, lmin)
			mem = append(mem, ev)
			continue
		}
		ev.Feasible = true
		mem = append(mem, ev)
		ctx := tokens / 1024 * 1024
		if ctx > int64(m.MaxModelLen) {
			ctx = int64(m.MaxModelLen)
		}
		dp := k / tp
		plans = append(plans, Plan{
			TP: tp, DP: dp, CardsUsed: tp * dp, MaxModelLen: int(ctx),
			KVBudgetBytesPerCard: ev.KVBudgetBytesPerCard, KVCapacityTokens: tokens,
			ContextTight: ctx < lmin*contextTightFactor,
		})
	}
	return plans, mem
}

// recommend 默认推荐用卡最多的组合, 同等用卡下取 tp 最小者(副本多、吞吐高)。
// 补充规则: 同等用卡时上下文不紧张的组合优先于紧张的组合——否则 2×80GB 跑 32B BF16 会推荐
// tp=1×dp=2(每副本仅约 1.2 万上下文)而不是 §27.3.1 示例表中推荐的 tp=2。
func recommend(plans []Plan) {
	better := func(a, b Plan) bool {
		if a.CardsUsed != b.CardsUsed {
			return a.CardsUsed > b.CardsUsed
		}
		if a.ContextTight != b.ContextTight {
			return !a.ContextTight
		}
		return a.TP < b.TP
	}
	best := -1
	for i, p := range plans {
		if best < 0 || better(p, plans[best]) {
			best = i
		}
	}
	if best >= 0 {
		plans[best].Recommended = true
	}
}

func recommended(plans []Plan) *Plan {
	for i := range plans {
		if plans[i].Recommended {
			return &plans[i]
		}
	}
	return nil
}

func ok(id, msg string) Check { return Check{ID: id, OK: true, Level: LevelInfo, Message: msg} }

func fail(id, level, msg string) Check { return Check{ID: id, Level: level, Message: msg} }

func gib(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

// memoryCheck M1: 至少一个 (tp, dp) 组合满足显存与最低上下文。
func memoryCheck(in Input, k int, vram int64, plans []Plan) Check {
	if k == 0 {
		return fail("M1", LevelError, "租约内没有可用的卡")
	}
	if !vramInRange(vram) {
		return fail("M1", LevelError, fmt.Sprintf("租约卡显存数据缺失或超出合理范围(%d 字节), 无法判定, 请稍后重试或联系平台运营", vram))
	}
	if len(plans) == 0 {
		return fail("M1", LevelError, fmt.Sprintf(
			"%d 张卡在显存利用率 0.%02d 下装不下该模型(权重 %s, 每卡开销 %s, 最低上下文 %d tokens), 请选择更小的量化变体或更多卡",
			k, in.GPUMemUtilPct, gib(in.Model.WeightsBytes), gib(in.Model.RuntimeOverheadBytes), in.Model.MinContextTokens))
	}
	return ok("M1", fmt.Sprintf("共 %d 个可行组合", len(plans)))
}

// contextCheck M2: 推荐组合的可用上下文是否紧张(仅提示)。
func contextCheck(in Input, plans []Plan) Check {
	rec := recommended(plans)
	if rec == nil {
		return ok("M2", "无可行组合, 不评估上下文余量")
	}
	if rec.ContextTight {
		return fail("M2", LevelWarning, fmt.Sprintf(
			"推荐组合可用上下文约 %d tokens, 接近最低要求 %d, 长上下文请求可能排队或被拒绝", rec.MaxModelLen, in.Model.MinContextTokens))
	}
	return ok("M2", fmt.Sprintf("推荐组合可用上下文 %d tokens", rec.MaxModelLen))
}

// checkF1 GPU 架构满足目录最低 compute capability; 型号未登记即"无法判定"。
func checkF1(in Input) Check {
	cc, okCC := parseCC(in.ComputeCapability)
	if strings.TrimSpace(in.ComputeCapability) == "" || !okCC {
		return fail("F1", LevelError, fmt.Sprintf("GPU 型号 %s 未登记算力架构(compute_capability), 无法判定, 请联系平台运营补充", in.GPUModel))
	}
	need, okNeed := parseCC(in.Model.MinComputeCapability)
	if !okNeed {
		return fail("F1", LevelError, fmt.Sprintf("目录条目 %s 的 min_compute_capability 非法, 无法判定", in.Model.ModelKey))
	}
	if cc >= need {
		return ok("F1", fmt.Sprintf("GPU 架构 %s 满足要求 ≥ %s", in.ComputeCapability, in.Model.MinComputeCapability))
	}
	var alts []string
	for _, a := range in.Alternatives {
		if a.ModelKey == in.Model.ModelKey {
			continue
		}
		if v, okA := parseCC(a.MinComputeCapability); okA && v <= cc {
			alts = append(alts, a.ModelKey)
		}
	}
	sort.Strings(alts)
	msg := fmt.Sprintf("GPU 架构 %s 低于该模型要求的 %s(如 FP8 权重需 ≥ 8.9)", in.ComputeCapability, in.Model.MinComputeCapability)
	if len(alts) > 0 {
		msg += ", 其他可用目录条目: " + strings.Join(alts, ", ")
	} else {
		msg += ", 暂无其他可用目录条目"
	}
	return fail("F1", LevelError, msg)
}

// checkF2 驱动版本 ≥ min_driver_version。
func checkF2(in Input) Check {
	cmp, okV := compareVersion(in.DriverVersion, in.Model.MinDriverVersion)
	if !okV {
		return fail("F2", LevelError, fmt.Sprintf("节点驱动版本未知或无法识别(%q), 无法判定", in.DriverVersion))
	}
	if cmp < 0 {
		return fail("F2", LevelError, fmt.Sprintf("节点驱动 %s 低于要求 %s, 请联系供给方升级驱动", in.DriverVersion, in.Model.MinDriverVersion))
	}
	return ok("F2", fmt.Sprintf("驱动 %s 满足要求 ≥ %s", in.DriverVersion, in.Model.MinDriverVersion))
}

// checkF3 租约内各卡当前显存占用 ≤ 2%。
func checkF3(in Input) Check {
	if len(in.Cards) == 0 {
		return fail("F3", LevelError, "租约内没有可用的卡")
	}
	if why, unknown := in.Unknown[InputVRAMUsage]; unknown {
		return fail("F3", LevelError, "各卡当前显存占用无法判定: "+why)
	}
	busy := 0
	for _, c := range in.Cards {
		// 显存总量/占用缺失或越界的卡不能视为空闲; 先限定 0 ≤ used ≤ total ≤ 1 TiB, 再做乘法比较, 不会溢出。
		if !vramInRange(c.VRAMTotalBytes) || c.VRAMUsedBytes < 0 || c.VRAMUsedBytes > c.VRAMTotalBytes ||
			c.VRAMUsedBytes*100 > c.VRAMTotalBytes*maxIdleVRAMUsedPct {
			busy++
		}
	}
	if busy > 0 {
		return fail("F3", LevelError, fmt.Sprintf("%d 张卡当前显存占用超过 %d%% 或显存数据异常, 请先停止占用显存的进程", busy, maxIdleVRAMUsedPct))
	}
	return ok("F3", "各卡显存空闲")
}

// checkF4 节点已缓存该模型, 或缓存区剩余(含可按 LRU 清理部分) ≥ 权重×1.1。
func checkF4(in Input) Check {
	if in.ModelCached {
		return ok("F4", "节点已缓存该模型权重")
	}
	if why, unknown := in.Unknown[InputModelCache]; unknown {
		return fail("F4", LevelError, "节点未缓存该模型, 且模型缓存区剩余空间无法判定: "+why)
	}
	if in.CacheFreeBytes < 0 || in.CacheEvictableBytes < 0 {
		return fail("F4", LevelError, "节点模型缓存区容量数据异常, 无法判定, 请稍后重试或联系平台运营")
	}
	avail, okA := addInt64(in.CacheFreeBytes, in.CacheEvictableBytes)
	avail10, okB := mulInt64(avail, 10)
	need, okC := mulInt64(in.Model.WeightsBytes, 11)
	if !okA || !okB || !okC {
		return fail("F4", LevelError, "节点模型缓存区容量或权重大小超出可计算范围, 无法判定")
	}
	if avail10 >= need {
		return ok("F4", fmt.Sprintf("模型缓存区可用 %s(含可清理 %s), 满足权重 1.1 倍", gib(avail), gib(in.CacheEvictableBytes)))
	}
	return fail("F4", LevelError, fmt.Sprintf("模型缓存区可用 %s(含可清理 %s), 不足权重 %s 的 1.1 倍",
		gib(avail), gib(in.CacheEvictableBytes), gib(in.Model.WeightsBytes)))
}

// checkF5 多卡张量并行且卡间无 NVLink: 可行, 仅提示性能受 PCIe 带宽限制。
func checkF5(in Input, plans []Plan) Check {
	multi := false
	for _, p := range plans {
		if p.TP > 1 {
			multi = true
		}
	}
	if !multi {
		return ok("F5", "可行组合均不涉及卡间张量并行")
	}
	if why, unknown := in.Unknown[InputNVLink]; unknown {
		return fail("F5", LevelWarning, "卡间是否 NVLink 互联无法判定("+why+"), 多卡张量并行(tp>1)的性能可能受 PCIe 带宽限制")
	}
	if in.NVLink {
		return ok("F5", "卡间 NVLink 互联")
	}
	return fail("F5", LevelWarning, "卡间无 NVLink, 多卡张量并行(tp>1)的性能会受 PCIe 带宽限制")
}

// checkF6 卡健康: 无 faulty、近 1 小时无致命 Xid。
func checkF6(in Input) Check {
	faulty, xid := 0, 0
	for _, c := range in.Cards {
		if c.Faulty {
			faulty++
		}
		if c.FatalXidLastHour {
			xid++
		}
	}
	if faulty > 0 || xid > 0 {
		return fail("F6", LevelError, fmt.Sprintf("租约内 %d 张卡被标记故障、%d 张卡近 1 小时出现致命 Xid, 暂不能部署", faulty, xid))
	}
	if why, unknown := in.Unknown[InputFatalXid]; unknown {
		return fail("F6", LevelError, "无故障标记, 但近 1 小时是否出现致命 Xid 无法判定: "+why)
	}
	return ok("F6", "各卡健康")
}

// checkF7 许可证非 allowed: 可行, 但须买家勾选 license_ack。
// docs/12 §12.5.1 口径为"commercial_use 非 allowed 的模型须买家部署前单独确认", 因此 unknown 同样要求确认。
func checkF7(in Input) Check {
	switch in.Model.CommercialUse {
	case CommercialAllowed:
		return ok("F7", "许可证允许商用")
	case CommercialRestricted:
		c := fail("F7", LevelConfirm, "该模型许可证对商用有限制, 部署前请阅读并确认许可条款")
		c.Ack = AckLicense
		return c
	default:
		c := fail("F7", LevelConfirm, "该模型许可证的商用条件尚未核实, 部署前请自行阅读并确认许可条款")
		c.Ack = AckLicense
		return c
	}
}

// checkF8 租约卡型在该条目下没有 result=pass 的真机验证记录: 可行, 但须买家勾选 untested_ack。
func checkF8(in Input) Check {
	for _, g := range in.VerifiedGPUModels {
		if g == in.GPUModel {
			return ok("F8", fmt.Sprintf("已在 %s 上真机验证", in.GPUModel))
		}
	}
	c := fail("F8", LevelConfirm, fmt.Sprintf("该模型未在 %s 上实测, 显存估算可能有偏差", in.GPUModel))
	c.Ack = AckUntested
	return c
}
