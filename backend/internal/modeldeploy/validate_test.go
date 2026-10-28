package modeldeploy

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
	"time"
)

const (
	testSHA      = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	testRevision = "0123456789abcdef0123456789abcdef01234567"
	gib64        = int64(1) << 30
)

func testMetricsMap() json.RawMessage {
	return json.RawMessage(`{"requests_running":"vllm:num_requests_running","requests_waiting":"vllm:num_requests_waiting",
		"kv_cache_usage":"vllm:gpu_cache_usage_perc","ttft_seconds":"vllm:time_to_first_token_seconds",
		"generation_tokens_total":"vllm:generation_tokens_total","prompt_tokens_total":"vllm:prompt_tokens_total",
		"preemptions_total":"vllm:num_preemptions_total"}`)
}

// validEntry 一个合法的 32B BF16 条目(数值同 docs/27 §27.3.1 示例)。
func validEntry() *ModelEntry {
	return &ModelEntry{
		ModelKey: "qwen3-32b-bf16", DisplayName: "Qwen3 32B", Repo: "Qwen/Qwen3-32B", Revision: testRevision,
		Manifest: json.RawMessage(`[{"path":"model-00001.safetensors","size":` + itoa(65*gib64) + `,"sha256":"` + testSHA + `"},
			{"path":"config.json","size":1024,"sha256":"` + testSHA + `"}]`),
		WeightsBytes: 65 * gib64, Quant: "bf16", MinComputeCapability: "8.0", ValidTP: "8, 1,2,4",
		KVBytesPerToken: 256 << 10, RuntimeOverheadBytes: 4 * gib64, MaxModelLen: 131072,
		EngineImage:      "registry.omnisline.com/vllm/vllm-openai:v0.11.0@sha256:" + testSHA,
		MinDriverVersion: "535.104.05", EngineArgs: json.RawMessage(`{"enable-prefix-caching":true,"max-num-seqs":256}`),
		MetricsMap: testMetricsMap(), License: "Apache-2.0", CommercialUse: CommercialAllowed,
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestNormalizeEntryAcceptsAndNormalizes(t *testing.T) {
	e := validEntry()
	if err := normalizeEntry(e); err != nil {
		t.Fatalf("合法条目报错: %v", err)
	}
	if e.ValidTP != "1,2,4,8" || e.Source != "modelscope" || e.Engine != "vllm" ||
		e.MinContextTokens != 8192 || e.StartupTimeoutS != 1200 {
		t.Fatalf("未规范化默认值: %+v", e)
	}
}

func TestNormalizeEntryRejects(t *testing.T) {
	cases := map[string]func(e *ModelEntry){
		"model_key 大写":        func(e *ModelEntry) { e.ModelKey = "Qwen3" },
		"repo 无组织":            func(e *ModelEntry) { e.Repo = "Qwen3-32B" },
		"revision 分支名":        func(e *ModelEntry) { e.Revision = "master" },
		"revision 短哈希":        func(e *ModelEntry) { e.Revision = "0123456" },
		"source 非 modelscope": func(e *ModelEntry) { e.Source = "huggingface" },
		"manifest 空":          func(e *ModelEntry) { e.Manifest = json.RawMessage(`[]`) },
		"manifest 越界路径": func(e *ModelEntry) {
			e.Manifest = json.RawMessage(`[{"path":"../x","size":1,"sha256":"` + testSHA + `"}]`)
		},
		"manifest 绝对路径": func(e *ModelEntry) {
			e.Manifest = json.RawMessage(`[{"path":"/x","size":1,"sha256":"` + testSHA + `"}]`)
		},
		"manifest sha 非法": func(e *ModelEntry) { e.Manifest = json.RawMessage(`[{"path":"x","size":1,"sha256":"abc"}]`) },
		"manifest 未知字段": func(e *ModelEntry) {
			e.Manifest = json.RawMessage(`[{"path":"x","size":1,"sha256":"` + testSHA + `","url":"http://a"}]`)
		},
		"weights 超过清单":         func(e *ModelEntry) { e.WeightsBytes = 70 * gib64 },
		"quant 非白名单":           func(e *ModelEntry) { e.Quant = "q4_k_m" },
		"cc 格式":                func(e *ModelEntry) { e.MinComputeCapability = "sm90" },
		"valid_tp 重复":          func(e *ModelEntry) { e.ValidTP = "1,1" },
		"valid_tp 非数字":         func(e *ModelEntry) { e.ValidTP = "1,x" },
		"kv 为 0":               func(e *ModelEntry) { e.KVBytesPerToken = 0 },
		"overhead 为 0":         func(e *ModelEntry) { e.RuntimeOverheadBytes = 0 },
		"Lmin 非 1024 倍数":       func(e *ModelEntry) { e.MinContextTokens = 9000 },
		"max_model_len < Lmin": func(e *ModelEntry) { e.MaxModelLen = 4096 },
		"镜像无 digest":           func(e *ModelEntry) { e.EngineImage = "vllm/vllm-openai:latest" },
		"驱动格式":                 func(e *ModelEntry) { e.MinDriverVersion = "r535" },
		"参数白名单外":               func(e *ModelEntry) { e.EngineArgs = json.RawMessage(`{"tensor-parallel-size":2}`) },
		"参数注入":                 func(e *ModelEntry) { e.EngineArgs = json.RawMessage(`{"dtype":"auto; rm -rf /"}`) },
		"参数嵌套对象":               func(e *ModelEntry) { e.EngineArgs = json.RawMessage(`{"dtype":{"a":1}}`) },
		"metrics 缺键": func(e *ModelEntry) {
			e.MetricsMap = json.RawMessage(`{"requests_running":"vllm:num_requests_running"}`)
		},
		"metrics 多余键": func(e *ModelEntry) {
			e.MetricsMap = json.RawMessage(strings.Replace(string(testMetricsMap()), "{", `{"x":"y",`, 1))
		},
		"startup 超限":        func(e *ModelEntry) { e.StartupTimeoutS = 10 },
		"license 空":         func(e *ModelEntry) { e.License = " " },
		"commercial_use 非法": func(e *ModelEntry) { e.CommercialUse = "yes" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			e := validEntry()
			mutate(e)
			if err := normalizeEntry(e); err == nil {
				t.Fatalf("应校验失败")
			}
		})
	}
}

func TestValidateVerification(t *testing.T) {
	entry := validEntry()
	if err := normalizeEntry(entry); err != nil {
		t.Fatal(err)
	}
	base := func() *Verification {
		return &Verification{GPUModel: "H100-80G", Cards: 2, TP: 2, MaxModelLen: 32768, Result: "pass", TestedAt: time.Now()}
	}
	if err := validateVerification(base(), entry); err != nil {
		t.Fatalf("合法记录报错: %v", err)
	}
	bad := map[string]func(v *Verification){
		"tp 不在 valid_tp": func(v *Verification) { v.TP = 3; v.Cards = 4 },
		"tp 大于卡数":        func(v *Verification) { v.TP = 4 },
		"result 非法":      func(v *Verification) { v.Result = "ok" },
		"上下文超上限":         func(v *Verification) { v.MaxModelLen = 262144 },
		"缺 tested_at":    func(v *Verification) { v.TestedAt = time.Time{} },
		"缺 gpu_model":    func(v *Verification) { v.GPUModel = "" },
	}
	for name, mutate := range bad {
		t.Run(name, func(t *testing.T) {
			v := base()
			mutate(v)
			if err := validateVerification(v, entry); err == nil {
				t.Fatal("应校验失败")
			}
		})
	}
}

func TestCompareVersionAndCC(t *testing.T) {
	vs := []struct {
		a, b string
		cmp  int
		ok   bool
	}{
		{"535.104.05", "535.104.05", 0, true},
		{"550.54", "535.104.05", 1, true},
		{"535", "535.104.05", -1, true},
		{"570.86.15", "570.86.9", 1, true},
		{"", "535", 0, false},
		{"535.x", "535", 0, false},
	}
	for _, c := range vs {
		cmp, ok := compareVersion(c.a, c.b)
		if cmp != c.cmp || ok != c.ok {
			t.Errorf("compareVersion(%q,%q)=%d,%v want %d,%v", c.a, c.b, cmp, ok, c.cmp, c.ok)
		}
	}
	for s, want := range map[string]int{"8.0": 80, "8.9": 89, "9.0": 90, "10.0": 100, "12.0": 120} {
		if got, ok := parseCC(s); !ok || got != want {
			t.Errorf("parseCC(%q)=%d,%v", s, got, ok)
		}
	}
	if _, ok := parseCC("9"); ok {
		t.Error("parseCC 应拒绝缺次版本号")
	}
}
