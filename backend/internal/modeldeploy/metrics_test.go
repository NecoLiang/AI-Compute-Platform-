package modeldeploy

import (
	"math"
	"os"
	"testing"
)

// vLLM v0.11.0(V1 引擎)/metrics 指标名; KV 占用是 vllm:kv_cache_usage_perc(旧名 gpu_cache_usage_perc 已移除),
// 指标名随版本变化正是 metrics_map 存在的原因。
var v011Map = map[string]string{
	"requests_running":        "vllm:num_requests_running",
	"requests_waiting":        "vllm:num_requests_waiting",
	"kv_cache_usage":          "vllm:kv_cache_usage_perc",
	"ttft_seconds":            "vllm:time_to_first_token_seconds",
	"generation_tokens_total": "vllm:generation_tokens_total",
	"prompt_tokens_total":     "vllm:prompt_tokens_total",
	"preemptions_total":       "vllm:num_preemptions_total",
}

func readFixture(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func TestParseMetricsVLLM011(t *testing.T) {
	s, err := ParseMetrics(readFixture(t, "vllm_v0.11.0_metrics_t1.txt"), v011Map)
	if err != nil {
		t.Fatal(err)
	}
	// 数据并行 2 副本(engine="0"/"1"): 请求数与计数器求和, KV 取最大值。
	if *s.RequestsRunning != 15 || *s.RequestsWaiting != 4 {
		t.Fatalf("running/waiting = %v/%v", *s.RequestsRunning, *s.RequestsWaiting)
	}
	if !approx(*s.KVCacheUsage, 0.9712) {
		t.Fatalf("kv = %v", *s.KVCacheUsage)
	}
	if *s.PreemptTotal != 6 || *s.GenTokensTotal != 249347 || *s.PromptTotal != 1843520 {
		t.Fatalf("counters = %v %v %v", *s.PreemptTotal, *s.GenTokensTotal, *s.PromptTotal)
	}
	if len(s.TTFT) != 23 || !math.IsInf(s.TTFT[22].LE, 1) || s.TTFT[22].Count != 348 {
		t.Fatalf("ttft buckets = %d last=%+v", len(s.TTFT), s.TTFT[len(s.TTFT)-1])
	}
	// e2e 直方图、request_success、python_gc 等非目录指标不得混入。
	if s.TTFT[0].LE != 0.001 {
		t.Fatalf("first bucket %+v", s.TTFT[0])
	}
}

func TestParseMetricsOldNameMissing(t *testing.T) {
	// 目录写了旧版指标名: 对应值缺失为 nil, 不以 0 冒充。
	m := map[string]string{}
	for k, v := range v011Map {
		m[k] = v
	}
	m["kv_cache_usage"] = "vllm:gpu_cache_usage_perc"
	s, err := ParseMetrics(readFixture(t, "vllm_v0.11.0_metrics_t0.txt"), m)
	if err != nil {
		t.Fatal(err)
	}
	if s.KVCacheUsage != nil {
		t.Fatalf("kv should be nil, got %v", *s.KVCacheUsage)
	}
	if _, err := ParseMetrics("# nothing\nfoo 1\n", v011Map); err == nil {
		t.Fatal("expected error when no mapped metric present")
	}
}

func TestParseMetricsTruncatedAndEscapes(t *testing.T) {
	text := `vllm:num_requests_waiting{model_name="a\"b,c",engine="0"} 2
vllm:time_to_first_token_seconds_bucket{model_name="a\"b,c",le="0.5"} 1
vllm:time_to_first_token_seconds_bucket{model_name="a\"b,c",le="+Inf"} 3
vllm:num_requests_running{engine="0"} 1.0 1730000000000
vllm:num_requests_runn`
	s, err := ParseMetrics(text, v011Map)
	if err != nil {
		t.Fatal(err)
	}
	if *s.RequestsWaiting != 2 || *s.RequestsRunning != 1 || len(s.TTFT) != 2 || s.TTFT[1].Count != 3 {
		t.Fatalf("got %+v", s)
	}
}

func TestHistogramPercentilesFromDeltas(t *testing.T) {
	m0, _ := ParseMetrics(readFixture(t, "vllm_v0.11.0_metrics_t0.txt"), v011Map)
	m1, _ := ParseMetrics(readFixture(t, "vllm_v0.11.0_metrics_t1.txt"), v011Map)
	d := BucketDelta(m0.TTFT, m1.TTFT)
	if d[len(d)-1].Count != 232 {
		t.Fatalf("delta total = %v", d[len(d)-1].Count)
	}
	p50, ok := HistogramQuantile(0.5, d)
	if !ok || !approx(p50, 0.06+0.02*8.0/36.0) {
		t.Fatalf("p50 = %v", p50)
	}
	p95, ok := HistogramQuantile(0.95, d)
	if !ok || !approx(p95, 0.5+0.25*4.4/8.0) {
		t.Fatalf("p95 = %v", p95)
	}
	// 区间内无新请求 → 无分位。
	if _, ok := HistogramQuantile(0.95, BucketDelta(m1.TTFT, m1.TTFT)); ok {
		t.Fatal("expected no quantile for empty delta")
	}
	// 计数器归零(引擎重启): 以当前值为增量。
	if r := BucketDelta(m1.TTFT, m0.TTFT); r[len(r)-1].Count != 116 {
		t.Fatalf("reset delta total = %v", r[len(r)-1].Count)
	}
}

func TestHistogramQuantileEdges(t *testing.T) {
	inf := math.Inf(1)
	// 全部落入 +Inf 桶 → 返回最后一个有限上界。
	b := []Bucket{{0.1, 0}, {1, 0}, {inf, 5}}
	if v, ok := HistogramQuantile(0.95, b); !ok || v != 1 {
		t.Fatalf("got %v %v", v, ok)
	}
	// 缺 +Inf 桶(输出被截断)→ 不计算。
	if _, ok := HistogramQuantile(0.5, []Bucket{{0.1, 3}}); ok {
		t.Fatal("expected not ok")
	}
	// 第一个桶即命中: 从 0 线性插值。
	if v, _ := HistogramQuantile(0.5, []Bucket{{0.2, 10}, {inf, 10}}); !approx(v, 0.1) {
		t.Fatalf("got %v", v)
	}
}
