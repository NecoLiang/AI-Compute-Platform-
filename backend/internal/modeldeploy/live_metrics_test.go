package modeldeploy

import (
	"context"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// 真机只读: 经运维现有登录(LIVE_SSH_ALIAS)在节点本机抓一次 LIVE_ENGINE_METRICS(如 http://127.0.0.1:8000/metrics),
// 按 v0.11.0 目录 metrics_map 解析, 7 个 L4 指标必须全部取到。只读一次 /metrics, 不发推理请求、不跑监控/自动重启。
func TestLiveEngineMetrics(t *testing.T) {
	alias, url := os.Getenv("LIVE_SSH_ALIAS"), os.Getenv("LIVE_ENGINE_METRICS")
	if alias == "" || url == "" {
		t.Skip("LIVE_SSH_ALIAS / LIVE_ENGINE_METRICS 未设置, 跳过真机指标联调")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	out, err := exec.CommandContext(ctx, "ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=20", alias, "curl -sf -m 10 "+url).Output()
	require.NoError(t, err)
	s, err := ParseMetrics(string(out), v011Map)
	require.NoError(t, err)
	require.NotNil(t, s.RequestsRunning, "requests_running")
	require.NotNil(t, s.RequestsWaiting, "requests_waiting")
	require.NotNil(t, s.KVCacheUsage, "kv_cache_usage")
	require.NotNil(t, s.GenTokensTotal, "generation_tokens_total")
	require.NotNil(t, s.PromptTotal, "prompt_tokens_total")
	require.NotNil(t, s.PreemptTotal, "preemptions_total")
	require.NotEmpty(t, s.TTFT, "ttft_seconds")
	p50, _ := HistogramQuantile(0.5, s.TTFT)
	p95, _ := HistogramQuantile(0.95, s.TTFT)
	t.Logf("running=%.0f waiting=%.0f kv=%.3f gen_total=%.0f prompt_total=%.0f preempt=%.0f ttft(启动以来) p50=%.2fs p95=%.2fs",
		*s.RequestsRunning, *s.RequestsWaiting, *s.KVCacheUsage, *s.GenTokensTotal, *s.PromptTotal, *s.PreemptTotal, p50, p95)
}
