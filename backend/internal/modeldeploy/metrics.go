package modeldeploy

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

// Bucket 直方图累计桶: le 上界(+Inf 用 math.Inf(1)), Count 为 ≤ le 的累计次数。
type Bucket struct {
	LE    float64 `json:"le"`
	Count float64 `json:"c"`
}

// MetricsSnapshot 一次 /metrics 抓取按目录 metrics_map 解析出的值(L4)。
// 同名指标多条序列(vLLM 数据并行时 engine="0"/"1"…)时: 请求数与计数器求和, KV 占用取最大值
// (任一副本饱和即饱和), 直方图按 le 逐桶求和。缺失的指标为 nil, 不以 0 冒充。
type MetricsSnapshot struct {
	RequestsRunning *float64
	RequestsWaiting *float64
	KVCacheUsage    *float64 // 0–1
	GenTokensTotal  *float64
	PromptTotal     *float64
	PreemptTotal    *float64
	TTFT            []Bucket // 按 le 升序
}

// ParseMetrics 解析 Prometheus 文本格式(0.0.4)。只取 metrics_map 中的 7 个指标, 其余行忽略。
// 格式错误的行忽略(上游输出被截断时最后一行可能不完整), 但一个目标指标都没解析到时报错。
func ParseMetrics(text string, mm map[string]string) (*MetricsSnapshot, error) {
	want := map[string]string{} // 指标名 → 逻辑名
	for k, v := range mm {
		want[v] = k
	}
	ttftName := mm["ttft_seconds"]
	sums := map[string]float64{}
	seen := map[string]bool{}
	var kvMax float64
	buckets := map[float64]float64{}
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line[0] == '#' {
			continue
		}
		name, labels, val, ok := splitSample(line)
		if !ok {
			continue
		}
		if ttftName != "" && name == ttftName+"_bucket" {
			le, ok := labelValue(labels, "le")
			if !ok {
				continue
			}
			bound, err := parseFloat(le)
			if err != nil {
				continue
			}
			buckets[bound] += val
			seen["ttft_seconds"] = true
			continue
		}
		logical, ok := want[name]
		if !ok || logical == "ttft_seconds" {
			continue
		}
		if logical == "kv_cache_usage" {
			if !seen[logical] || val > kvMax {
				kvMax = val
			}
		} else {
			sums[logical] += val
		}
		seen[logical] = true
	}
	if len(seen) == 0 {
		return nil, fmt.Errorf("/metrics 中未找到目录 metrics_map 列出的任何指标")
	}
	s := &MetricsSnapshot{}
	pick := func(k string) *float64 {
		if !seen[k] {
			return nil
		}
		v := sums[k]
		return &v
	}
	s.RequestsRunning = pick("requests_running")
	s.RequestsWaiting = pick("requests_waiting")
	s.GenTokensTotal = pick("generation_tokens_total")
	s.PromptTotal = pick("prompt_tokens_total")
	s.PreemptTotal = pick("preemptions_total")
	if seen["kv_cache_usage"] {
		v := kvMax
		s.KVCacheUsage = &v
	}
	for le, c := range buckets {
		s.TTFT = append(s.TTFT, Bucket{LE: le, Count: c})
	}
	sort.Slice(s.TTFT, func(i, j int) bool { return s.TTFT[i].LE < s.TTFT[j].LE })
	return s, nil
}

// splitSample 拆一行样本: name{labels} value [timestamp]。
func splitSample(line string) (name, labels string, val float64, ok bool) {
	rest := line
	if i := strings.IndexByte(line, '{'); i >= 0 {
		j := strings.LastIndexByte(line, '}')
		if j < i {
			return "", "", 0, false
		}
		name, labels, rest = line[:i], line[i+1:j], strings.TrimSpace(line[j+1:])
	} else {
		f := strings.Fields(line)
		if len(f) < 2 {
			return "", "", 0, false
		}
		name, rest = f[0], strings.Join(f[1:], " ")
	}
	f := strings.Fields(rest)
	if len(f) == 0 {
		return "", "", 0, false
	}
	v, err := parseFloat(f[0])
	if err != nil || math.IsNaN(v) {
		return "", "", 0, false
	}
	return name, labels, v, true
}

func parseFloat(s string) (float64, error) {
	switch s {
	case "+Inf", "Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	}
	return strconv.ParseFloat(s, 64)
}

// labelValue 取标签值; 标签值内的 \" 与 , 按 Prometheus 转义规则处理。
func labelValue(labels, key string) (string, bool) {
	i := 0
	for i < len(labels) {
		for i < len(labels) && (labels[i] == ',' || labels[i] == ' ') {
			i++
		}
		eq := strings.IndexByte(labels[i:], '=')
		if eq < 0 {
			return "", false
		}
		k := strings.TrimSpace(labels[i : i+eq])
		i += eq + 1
		if i >= len(labels) || labels[i] != '"' {
			return "", false
		}
		i++
		var sb strings.Builder
		for i < len(labels) && labels[i] != '"' {
			if labels[i] == '\\' && i+1 < len(labels) {
				i++
			}
			sb.WriteByte(labels[i])
			i++
		}
		i++ // 结束引号
		if k == key {
			return sb.String(), true
		}
	}
	return "", false
}

// BucketDelta 两次抓取之间的桶增量(分位按区间增量计算, 不用进程启动以来的累计值)。
// prev 为空、桶边界不一致或任一桶减少(引擎重启致计数器归零)时, 以 cur 本身作为增量。
func BucketDelta(prev, cur []Bucket) []Bucket {
	if len(prev) != len(cur) || len(prev) == 0 {
		return cur
	}
	out := make([]Bucket, len(cur))
	for i := range cur {
		if prev[i].LE != cur[i].LE || cur[i].Count < prev[i].Count {
			return cur
		}
		out[i] = Bucket{LE: cur[i].LE, Count: cur[i].Count - prev[i].Count}
	}
	return out
}

// HistogramQuantile 与 Prometheus histogram_quantile 相同的线性插值: 返回 q 分位的上界估计(秒)。
// 无样本返回 ok=false; 落入 +Inf 桶时返回最后一个有限上界。
func HistogramQuantile(q float64, b []Bucket) (float64, bool) {
	if len(b) == 0 || q < 0 || q > 1 {
		return 0, false
	}
	total := b[len(b)-1].Count
	if !math.IsInf(b[len(b)-1].LE, 1) || total <= 0 {
		return 0, false
	}
	rank := q * total
	prevLE, prevCount := 0.0, 0.0
	for i, bk := range b {
		if bk.Count >= rank {
			if math.IsInf(bk.LE, 1) {
				if i == 0 {
					return 0, false
				}
				return b[i-1].LE, true
			}
			inBucket := bk.Count - prevCount
			if inBucket <= 0 {
				return bk.LE, true
			}
			return prevLE + (bk.LE-prevLE)*(rank-prevCount)/inBucket, true
		}
		prevLE, prevCount = bk.LE, bk.Count
	}
	return b[len(b)-1].LE, true
}
