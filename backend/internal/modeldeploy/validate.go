package modeldeploy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

var (
	modelKeyPattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,63}$`)
	repoPattern        = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,126}/[A-Za-z0-9][A-Za-z0-9._-]{0,126}$`)
	revisionPattern    = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)
	sha256Pattern      = regexp.MustCompile(`^[0-9a-f]{64}$`)
	ccPattern          = regexp.MustCompile(`^([0-9]{1,2})\.([0-9])$`)
	driverPattern      = regexp.MustCompile(`^[0-9]{1,4}(?:\.[0-9]{1,4}){0,2}$`)
	engineImagePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9._/:-]{0,180}@sha256:[0-9a-f]{64}$`)
	// argValuePattern 首字符不得为 "-": 否则 "--served-model-name=x" 这类值会被引擎当作另一个参数解析, 绕过白名单。
	argValuePattern = regexp.MustCompile(`^[A-Za-z0-9._:/=+][A-Za-z0-9._:/=+-]{0,127}$`)
	promNamePattern = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]{0,127}$`)
)

// 目录数值上限(docs/27 §27.2 S5 实现口径)。超出即 40001。
// 上限远高于现实模型(最大的开源权重约 1.5 TiB、单 token KV 数 MiB), 作用是把可行性判定里的
// 乘法(V×u×tp、100×W、100×O×tp、100×tp×K)约束在 int64 安全范围内, 杜绝溢出回绕后误判"可行"。
const (
	MaxWeightsBytes         int64 = 4 << 40   // 4 TiB
	MaxManifestFileBytes    int64 = 1 << 40   // 单文件 1 TiB
	MaxManifestFiles              = 10000     // 清单文件数
	MaxKVBytesPerToken      int64 = 64 << 20  // 64 MiB/token
	MaxRuntimeOverheadBytes int64 = 256 << 30 // 256 GiB/卡
	MaxModelLenLimit              = 1 << 24   // 16,777,216 tokens
	MaxTP                         = 64
)

// 量化方式白名单。新增量化方式需同步核对可行性判定口径, 不接受自由文本。
var validQuant = map[string]bool{
	"bf16": true, "fp16": true, "fp8": true, "int8": true,
	"int4_awq": true, "int4_gptq": true, "mxfp4": true, "nvfp4": true,
}

var validCommercialUse = map[string]bool{
	CommercialAllowed: true, CommercialRestricted: true, CommercialUnknown: true,
}

// EngineArgWhitelist 目录模板允许出现的 vLLM 参数(不带 "--")。
// 由平台按租约/买家选择决定的参数(--model、--served-model-name、--tensor-parallel-size、
// --data-parallel-size、--max-model-len、--gpu-memory-utilization、端口与监听地址)不在名单内,
// 模板里出现即拒绝, 防止目录条目覆盖平台控制的取值(docs/27 §27.4.1)。
// trust-remote-code 不在名单内: 它让引擎执行权重仓库里的 Python 代码, 等于在买家租约的卡上
// 运行目录之外的任意代码(docs/27 §27.4.1 S5 实现口径); 需要自定义代码的模型不进目录。
var EngineArgWhitelist = map[string]bool{
	"dtype": true, "quantization": true, "kv-cache-dtype": true,
	"enable-prefix-caching": true, "enable-chunked-prefill": true,
	"max-num-seqs": true, "max-num-batched-tokens": true, "block-size": true,
	"enforce-eager": true, "tokenizer-mode": true,
	"reasoning-parser": true, "tool-call-parser": true, "enable-auto-tool-choice": true,
	"enable-expert-parallel": true, "swap-space": true, "disable-custom-all-reduce": true,
}

// MetricsMapKeys 健康检测 L4 需要的逻辑指标(docs/27 §27.5.1); 目录必须给全, 值为该 vLLM 版本的实际指标名。
var MetricsMapKeys = []string{
	"requests_running", "requests_waiting", "kv_cache_usage", "ttft_seconds",
	"generation_tokens_total", "prompt_tokens_total", "preemptions_total",
}

// ParseValidTP 解析 "1,2,4,8" 为升序去重的并行度列表。
func ParseValidTP(s string) ([]int, error) {
	parts := strings.Split(s, ",")
	seen := map[int]bool{}
	out := make([]int, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		n, err := strconv.Atoi(p)
		if err != nil || n < 1 || n > MaxTP {
			return nil, fmt.Errorf("valid_tp 须为 1–64 的整数列表, 如 1,2,4,8")
		}
		if seen[n] {
			return nil, fmt.Errorf("valid_tp 存在重复值 %d", n)
		}
		seen[n] = true
		out = append(out, n)
	}
	sort.Ints(out)
	return out, nil
}

func joinTP(tps []int) string {
	parts := make([]string, len(tps))
	for i, n := range tps {
		parts[i] = strconv.Itoa(n)
	}
	return strings.Join(parts, ",")
}

// parseCC 解析 CUDA 算力架构 "主.次" 为可比较整数(主*10+次)。
func parseCC(s string) (int, bool) {
	m := ccPattern.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return 0, false
	}
	major, _ := strconv.Atoi(m[1])
	minor, _ := strconv.Atoi(m[2])
	return major*10 + minor, true
}

// compareVersion 按点分数字逐段比较驱动版本, 缺失段视为 0。ok=false 表示任一方格式无法识别。
func compareVersion(a, b string) (cmp int, ok bool) {
	a, b = strings.TrimSpace(a), strings.TrimSpace(b)
	if !driverPattern.MatchString(a) || !driverPattern.MatchString(b) {
		return 0, false
	}
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x < y {
				return -1, true
			}
			return 1, true
		}
	}
	return 0, true
}

// normalizeEntry 校验并规范化运营提交的目录条目。所有字段均为整体提交, 不做部分更新。
func normalizeEntry(e *ModelEntry) error {
	e.ModelKey = strings.TrimSpace(e.ModelKey)
	e.DisplayName = strings.TrimSpace(e.DisplayName)
	e.Repo = strings.TrimSpace(e.Repo)
	e.Revision = strings.TrimSpace(e.Revision)
	e.License = strings.TrimSpace(e.License)
	if !modelKeyPattern.MatchString(e.ModelKey) {
		return fmt.Errorf("model_key 须为 2–64 位小写字母、数字、点、下划线或连字符")
	}
	if e.DisplayName == "" || len([]rune(e.DisplayName)) > 128 {
		return fmt.Errorf("display_name 必填且不超过 128 字")
	}
	if e.Source == "" {
		e.Source = "modelscope"
	}
	if e.Source != "modelscope" {
		return fmt.Errorf("source 目前只支持 modelscope")
	}
	if !repoPattern.MatchString(e.Repo) {
		return fmt.Errorf("repo 须为 组织/模型 格式")
	}
	if !revisionPattern.MatchString(e.Revision) {
		return fmt.Errorf("revision 须为完整的固定提交号(40 或 64 位小写十六进制), 不接受分支名或标签")
	}
	manifestTotal, err := validateManifest(e.Manifest)
	if err != nil {
		return err
	}
	if e.WeightsBytes <= 0 || e.WeightsBytes > MaxWeightsBytes {
		return fmt.Errorf("weights_bytes 须在 1–%d(4 TiB)之间", MaxWeightsBytes)
	}
	if e.WeightsBytes > manifestTotal {
		return fmt.Errorf("weights_bytes 不得超过清单文件总大小 %d", manifestTotal)
	}
	if !validQuant[e.Quant] {
		return fmt.Errorf("quant 须为 bf16/fp16/fp8/int8/int4_awq/int4_gptq/mxfp4/nvfp4 之一")
	}
	if _, ok := parseCC(e.MinComputeCapability); !ok {
		return fmt.Errorf("min_compute_capability 须为 主.次 格式, 如 8.0 / 8.9 / 9.0")
	}
	e.MinComputeCapability = strings.TrimSpace(e.MinComputeCapability)
	tps, err := ParseValidTP(e.ValidTP)
	if err != nil {
		return err
	}
	e.ValidTP = joinTP(tps)
	if len(e.ValidTP) > 32 {
		return fmt.Errorf("valid_tp 过长")
	}
	if e.KVBytesPerToken <= 0 || e.KVBytesPerToken > MaxKVBytesPerToken {
		return fmt.Errorf("kv_bytes_per_token 须在 1–%d(64 MiB)之间", MaxKVBytesPerToken)
	}
	if e.RuntimeOverheadBytes <= 0 || e.RuntimeOverheadBytes > MaxRuntimeOverheadBytes {
		return fmt.Errorf("runtime_overhead_bytes 须在 1–%d(256 GiB)之间, 取真机实测值", MaxRuntimeOverheadBytes)
	}
	if e.MinContextTokens == 0 {
		e.MinContextTokens = 8192
	}
	// 最低上下文须为 1024 的整数倍: 建议上下文按 1024 向下取整, 否则可能出现 "T ≥ Lmin 但建议值 < Lmin"。
	if e.MinContextTokens < 1024 || e.MinContextTokens%1024 != 0 {
		return fmt.Errorf("min_context_tokens 须为 1024 的正整数倍")
	}
	if e.MaxModelLen < e.MinContextTokens || e.MaxModelLen > MaxModelLenLimit {
		return fmt.Errorf("max_model_len 须在 min_context_tokens–%d 之间", MaxModelLenLimit)
	}
	if e.Engine == "" {
		e.Engine = "vllm"
	}
	if e.Engine != "vllm" {
		return fmt.Errorf("engine 目前只支持 vllm")
	}
	e.EngineImage = strings.TrimSpace(e.EngineImage)
	if !engineImagePattern.MatchString(e.EngineImage) {
		return fmt.Errorf("engine_image 须为 镜像引用@sha256:<64位摘要>, 按 digest 固定")
	}
	e.MinDriverVersion = strings.TrimSpace(e.MinDriverVersion)
	if !driverPattern.MatchString(e.MinDriverVersion) {
		return fmt.Errorf("min_driver_version 须为点分数字, 如 535.104.05")
	}
	args, err := normalizeEngineArgs(e.EngineArgs)
	if err != nil {
		return err
	}
	e.EngineArgs = args
	metrics, err := normalizeMetricsMap(e.MetricsMap)
	if err != nil {
		return err
	}
	e.MetricsMap = metrics
	if e.StartupTimeoutS == 0 {
		e.StartupTimeoutS = 1200
	}
	if e.StartupTimeoutS < 60 || e.StartupTimeoutS > 7200 {
		return fmt.Errorf("startup_timeout_s 须在 60–7200 秒之间")
	}
	if e.License == "" || len(e.License) > 64 {
		return fmt.Errorf("license 必填且不超过 64 字符")
	}
	if !validCommercialUse[e.CommercialUse] {
		return fmt.Errorf("commercial_use 须为 allowed/restricted/unknown")
	}
	return nil
}

func validateManifest(raw json.RawMessage) (int64, error) {
	var files []ManifestFile
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if len(raw) == 0 || dec.Decode(&files) != nil || len(files) == 0 {
		return 0, fmt.Errorf("manifest 须为非空数组 [{path,size,sha256}]")
	}
	if len(files) > MaxManifestFiles {
		return 0, fmt.Errorf("manifest 文件数不得超过 %d", MaxManifestFiles)
	}
	seen := map[string]bool{}
	var total int64
	for _, f := range files {
		if !validManifestPath(f.Path) {
			return 0, fmt.Errorf("manifest 路径非法: %q(须为相对路径, 不得包含 .. 或以 / 开头)", f.Path)
		}
		if seen[f.Path] {
			return 0, fmt.Errorf("manifest 路径重复: %s", f.Path)
		}
		seen[f.Path] = true
		if f.Size <= 0 || f.Size > MaxManifestFileBytes {
			return 0, fmt.Errorf("manifest 文件 %s 的 size 须在 1–%d(1 TiB)之间", f.Path, MaxManifestFileBytes)
		}
		if !sha256Pattern.MatchString(f.SHA256) {
			return 0, fmt.Errorf("manifest 文件 %s 的 sha256 须为 64 位小写十六进制", f.Path)
		}
		var okSum bool
		if total, okSum = addInt64(total, f.Size); !okSum {
			return 0, fmt.Errorf("manifest 文件总大小溢出")
		}
	}
	return total, nil
}

func validManifestPath(p string) bool {
	if p == "" || len(p) > 255 || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

// normalizeEngineArgs 白名单键 + 标量值(字符串限安全字符集, 杜绝拼接进 docker 命令时的注入)。
func normalizeEngineArgs(raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 {
		return json.RawMessage(`{}`), nil
	}
	var args map[string]any
	if err := json.Unmarshal(raw, &args); err != nil || args == nil {
		return nil, fmt.Errorf("engine_args 须为 JSON 对象")
	}
	for k, v := range args {
		if !EngineArgWhitelist[k] {
			return nil, fmt.Errorf("engine_args 不允许参数 %q(白名单外, 或由平台按部署选择决定)", k)
		}
		switch val := v.(type) {
		case bool:
		case float64:
			if val < 0 {
				return nil, fmt.Errorf("engine_args.%s 不能为负数", k)
			}
		case string:
			if strings.HasPrefix(val, "-") {
				return nil, fmt.Errorf("engine_args.%s 的值不能以 - 开头(会被当作另一个参数)", k)
			}
			if !argValuePattern.MatchString(val) {
				return nil, fmt.Errorf("engine_args.%s 含非法字符", k)
			}
		default:
			return nil, fmt.Errorf("engine_args.%s 只能是字符串、数字或布尔值", k)
		}
	}
	out, _ := json.Marshal(args)
	return out, nil
}

func normalizeMetricsMap(raw json.RawMessage) (json.RawMessage, error) {
	var m map[string]string
	if len(raw) == 0 || json.Unmarshal(raw, &m) != nil || m == nil {
		return nil, fmt.Errorf("metrics_map 须为 JSON 对象 {逻辑名: 指标名}")
	}
	allowed := map[string]bool{}
	for _, k := range MetricsMapKeys {
		allowed[k] = true
		if !promNamePattern.MatchString(m[k]) {
			return nil, fmt.Errorf("metrics_map 缺少或非法: %s", k)
		}
	}
	for k := range m {
		if !allowed[k] {
			return nil, fmt.Errorf("metrics_map 不支持的逻辑名: %s", k)
		}
	}
	out, _ := json.Marshal(m)
	return out, nil
}

var verificationResults = map[string]bool{"pass": true, "fail": true}

// validateVerification 真机验证记录校验; tp 必须属于条目 valid_tp 且不超过卡数。
func validateVerification(v *Verification, entry *ModelEntry) error {
	v.GPUModel = strings.TrimSpace(v.GPUModel)
	if v.GPUModel == "" || len(v.GPUModel) > 64 {
		return fmt.Errorf("gpu_model 必填, 须为 GPU 型号库中的标准型号名")
	}
	if v.Cards < 1 {
		return fmt.Errorf("cards 须为正整数")
	}
	tps, err := ParseValidTP(entry.ValidTP)
	if err != nil {
		return err
	}
	okTP := false
	for _, n := range tps {
		if n == v.TP {
			okTP = true
		}
	}
	if !okTP {
		return fmt.Errorf("tp=%d 不在条目 valid_tp(%s) 内", v.TP, entry.ValidTP)
	}
	if v.TP > v.Cards {
		return fmt.Errorf("tp 不能大于 cards")
	}
	if v.MaxModelLen < 1 || v.MaxModelLen > entry.MaxModelLen {
		return fmt.Errorf("max_model_len 须在 1–%d 之间", entry.MaxModelLen)
	}
	if v.MeasuredOverheadBytes != nil && *v.MeasuredOverheadBytes <= 0 {
		return fmt.Errorf("measured_overhead_bytes 须为正数")
	}
	if !verificationResults[v.Result] {
		return fmt.Errorf("result 须为 pass/fail")
	}
	if v.Notes != nil {
		n := strings.TrimSpace(*v.Notes)
		if len([]rune(n)) > 512 {
			return fmt.Errorf("notes 不超过 512 字")
		}
		if n == "" {
			v.Notes = nil
		} else {
			v.Notes = &n
		}
	}
	if v.TestedAt.IsZero() {
		return fmt.Errorf("tested_at 必填")
	}
	return nil
}
