package modeldeploy

import (
	"encoding/json"
	"math"
	"strconv"
	"time"
)

// 健康判定与自动处置的常量(docs/27 §27.5.2 / §27.5.3)。
const (
	consecutiveFailLimit   = 3                // L1/L3 连续失败次数 → unhealthy
	degradedPersist        = 5 * time.Minute  // degraded 条件须持续
	kvSaturatedPct         = 95.0             // KV 占用 ≥95% 且排队 > 0 → capacity_saturated
	ttftBaselineFactor     = 3                // TTFT P95 > 基线 × 3
	gpuHotTempC            = 85               // 卡温 ≥85°C
	MaxAutoRestartsPerHour = 3                // 滚动 1 小时内自动重启上限, 超过 → failed
	degradedNotifyEvery    = 30 * time.Minute // 同一原因通知间隔
	restartWindow          = time.Hour
)

// restartBackoff 第 n 次(滚动 1 小时内已发生 n 次)自动重启前的等待。
var restartBackoff = []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}

// PlanRestart restartsInLastHour 为滚动 1 小时内已下发的自动重启次数。达到上限 → giveUp(转 failed);
// 否则返回本次重启前的退避时长 1/5/15 分钟。
func PlanRestart(restartsInLastHour int) (delay time.Duration, giveUp bool) {
	if restartsInLastHour < 0 {
		restartsInLastHour = 0
	}
	if restartsInLastHour >= MaxAutoRestartsPerHour {
		return 0, true
	}
	return restartBackoff[restartsInLastHour], false
}

// MarshalJSON le 可能是 +Inf, encoding/json 不支持, 以字符串存。
func (b Bucket) MarshalJSON() ([]byte, error) {
	return json.Marshal([2]string{strconv.FormatFloat(b.LE, 'g', -1, 64), strconv.FormatFloat(b.Count, 'g', -1, 64)})
}

func (b *Bucket) UnmarshalJSON(data []byte) error {
	var a [2]string
	if err := json.Unmarshal(data, &a); err != nil {
		return err
	}
	le, err := parseFloat(a[0])
	if err != nil {
		return err
	}
	c, err := strconv.ParseFloat(a[1], 64)
	if err != nil {
		return err
	}
	b.LE, b.Count = le, c
	return nil
}

// MonitorState 跨轮判定状态(model_deployments.monitor_state)。
type MonitorState struct {
	L1Fails          int                  `json:"l1_fails"`
	L3Fails          int                  `json:"l3_fails"`
	DegradedSince    map[string]time.Time `json:"degraded_since,omitempty"`
	PrevAt           *time.Time           `json:"prev_at,omitempty"`
	PrevTTFT         []Bucket             `json:"prev_ttft,omitempty"`
	PrevGen          *float64             `json:"prev_gen,omitempty"`
	PrevPrompt       *float64             `json:"prev_prompt,omitempty"`
	PrevPreempt      *float64             `json:"prev_preempt,omitempty"`
	PrevECCCorrected map[string]int64     `json:"prev_ecc_corrected,omitempty"`
}

// ContainerObs L0 docker inspect。
type ContainerObs struct {
	Exists    bool
	Running   bool
	OOMKilled bool
	ExitCode  int
	Status    string
}

// Observation 一轮检测的观测值。HealthOK/ModelsOK/CanaryOK 为 nil 表示该层本轮未执行(容器未运行/金丝雀关闭)。
type Observation struct {
	At        time.Time
	Container ContainerObs
	HealthOK  *bool
	ModelsOK  *bool
	CanaryOK  *bool
	CanaryMS  *int
	Metrics   *MetricsSnapshot // nil = 抓取或解析失败
	Cards     []CardHealth
}

type JudgeInput struct {
	Prev                  MonitorState
	Obs                   Observation
	CanaryEnabled         bool
	BaselineCanaryMS      *int
	AutoRestartWithinHour bool
}

// Sample 写入 deployment_health_samples 的一行。
type Sample struct {
	ContainerUp     bool
	HealthOK        *bool
	ModelsOK        *bool
	CanaryOK        *bool
	CanaryMS        *int
	RequestsRunning *int
	RequestsWaiting *int
	KVUsagePct      *float64
	TTFTP50MS       *int
	TTFTP95MS       *int
	GenTPS          *float64
	PromptTPS       *float64
	Preemptions     *int
	GPUTempMaxC     *int
	Throttled       *bool
	Health          string
}

type Judgement struct {
	Health string
	Reason string // healthy 时为空
	// Restartable 属于 §27.5.3 自动重启范围(容器退出/OOM/L1 连续失败), 且不是卡故障。
	Restartable bool
	// Degraded 当前已持续满 5 分钟的 degraded 原因(按固定优先级)。
	Degraded []string
	Sample   Sample
	Next     MonitorState
}

// degradedOrder degraded 原因优先级(health_reason 取第一个)。
var degradedOrder = []string{ReasonCapacitySaturated, ReasonPreempting, ReasonTTFTSlow, ReasonGPUHot, ReasonECCRising, ReasonRecentlyRestarted}

// Judge 纯函数: 由上轮状态与本轮观测给出健康判定(docs/27 §27.5.2)。
func Judge(in JudgeInput) Judgement {
	o, prev := in.Obs, in.Prev
	next := MonitorState{
		DegradedSince:    map[string]time.Time{},
		PrevAt:           prev.PrevAt,
		PrevTTFT:         prev.PrevTTFT,
		PrevGen:          prev.PrevGen,
		PrevPrompt:       prev.PrevPrompt,
		PrevPreempt:      prev.PrevPreempt,
		PrevECCCorrected: map[string]int64{},
	}
	s := Sample{ContainerUp: o.Container.Running, HealthOK: o.HealthOK, ModelsOK: o.ModelsOK, CanaryOK: o.CanaryOK, CanaryMS: o.CanaryMS}

	if o.Container.Running {
		if o.HealthOK != nil {
			if *o.HealthOK {
				next.L1Fails = 0
			} else {
				next.L1Fails = prev.L1Fails + 1
			}
		} else {
			next.L1Fails = prev.L1Fails
		}
		if in.CanaryEnabled && o.CanaryOK != nil {
			if *o.CanaryOK {
				next.L3Fails = 0
			} else {
				next.L3Fails = prev.L3Fails + 1
			}
		} else if in.CanaryEnabled {
			next.L3Fails = prev.L3Fails
		}
	}

	var preemptDelta *float64
	if m := o.Metrics; m != nil {
		s.RequestsRunning = floatToIntPtr(m.RequestsRunning)
		s.RequestsWaiting = floatToIntPtr(m.RequestsWaiting)
		if m.KVCacheUsage != nil {
			v := math.Round(*m.KVCacheUsage*10000) / 100
			s.KVUsagePct = &v
		}
		if len(m.TTFT) > 0 {
			d := BucketDelta(prev.PrevTTFT, m.TTFT)
			if p, ok := HistogramQuantile(0.5, d); ok {
				s.TTFTP50MS = intp(int(math.Round(p * 1000)))
			}
			if p, ok := HistogramQuantile(0.95, d); ok {
				s.TTFTP95MS = intp(int(math.Round(p * 1000)))
			}
			next.PrevTTFT = m.TTFT
		}
		var elapsed float64
		if prev.PrevAt != nil {
			elapsed = o.At.Sub(*prev.PrevAt).Seconds()
		}
		s.GenTPS = rate(prev.PrevGen, m.GenTokensTotal, elapsed)
		s.PromptTPS = rate(prev.PrevPrompt, m.PromptTotal, elapsed)
		if m.PreemptTotal != nil && prev.PrevPreempt != nil {
			d := *m.PreemptTotal - *prev.PrevPreempt
			if d < 0 { // 计数器归零(引擎重启)
				d = *m.PreemptTotal
			}
			preemptDelta = &d
			s.Preemptions = intp(int(d))
		}
		next.PrevGen, next.PrevPrompt, next.PrevPreempt = m.GenTokensTotal, m.PromptTotal, m.PreemptTotal
		at := o.At
		next.PrevAt = &at
	}

	fatal, hot, eccRising := false, false, false
	for _, c := range o.Cards {
		if c.FatalXid || (c.ECCUncorrected != nil && *c.ECCUncorrected > 0) {
			fatal = true
		}
		if c.TempC != nil {
			if s.GPUTempMaxC == nil || *c.TempC > *s.GPUTempMaxC {
				s.GPUTempMaxC = intp(*c.TempC)
			}
			if *c.TempC >= gpuHotTempC {
				hot = true
			}
		}
		if c.Throttled != nil {
			if s.Throttled == nil {
				s.Throttled = boolp(false)
			}
			if *c.Throttled {
				s.Throttled = boolp(true)
				hot = true
			}
		}
		if c.ECCCorrected != nil {
			next.PrevECCCorrected[c.UUID] = *c.ECCCorrected
			if p, ok := prev.PrevECCCorrected[c.UUID]; ok && *c.ECCCorrected > p {
				eccRising = true
			}
		}
	}

	j := Judgement{Next: next}
	switch {
	case fatal:
		j.Reason = ReasonGPUFatal
	case !o.Container.Running && o.Container.OOMKilled:
		j.Reason, j.Restartable = ReasonOOMKilled, true
	case !o.Container.Running:
		j.Reason, j.Restartable = ReasonContainerDown, true
	case next.L1Fails >= consecutiveFailLimit:
		j.Reason, j.Restartable = ReasonHealthFailed, true
	case in.CanaryEnabled && next.L3Fails >= consecutiveFailLimit:
		j.Reason = ReasonCanaryFailed
	}
	if j.Reason != "" {
		j.Health = HealthUnhealthy
		s.Health = j.Health
		j.Sample = s
		return j
	}

	conds := map[string]bool{
		ReasonCapacitySaturated: s.RequestsWaiting != nil && *s.RequestsWaiting > 0 && s.KVUsagePct != nil && *s.KVUsagePct >= kvSaturatedPct,
		ReasonPreempting:        preemptDelta != nil && *preemptDelta > 0,
		ReasonTTFTSlow:          in.BaselineCanaryMS != nil && *in.BaselineCanaryMS > 0 && s.TTFTP95MS != nil && *s.TTFTP95MS > ttftBaselineFactor**in.BaselineCanaryMS,
		ReasonGPUHot:            hot,
		ReasonECCRising:         eccRising,
		ReasonRecentlyRestarted: in.AutoRestartWithinHour,
	}
	for _, r := range degradedOrder {
		if !conds[r] {
			continue
		}
		since, ok := prev.DegradedSince[r]
		if !ok || since.After(o.At) {
			since = o.At
		}
		next.DegradedSince[r] = since
		if o.At.Sub(since) >= degradedPersist {
			j.Degraded = append(j.Degraded, r)
		}
	}
	j.Next = next
	if len(j.Degraded) > 0 {
		j.Health, j.Reason = HealthDegraded, j.Degraded[0]
	} else {
		j.Health = HealthHealthy
	}
	s.Health = j.Health
	j.Sample = s
	return j
}

func rate(prev, cur *float64, elapsed float64) *float64 {
	if prev == nil || cur == nil || elapsed <= 0 {
		return nil
	}
	d := *cur - *prev
	if d < 0 {
		d = *cur
	}
	v := math.Round(d/elapsed*100) / 100
	return &v
}

func floatToIntPtr(f *float64) *int {
	if f == nil {
		return nil
	}
	return intp(int(math.Round(*f)))
}

func intp(v int) *int    { return &v }
func boolp(v bool) *bool { return &v }

// medianInt 基线 = running 后首小时金丝雀耗时中位数。
func medianInt(v []int) (int, bool) {
	if len(v) == 0 {
		return 0, false
	}
	c := append([]int(nil), v...)
	for i := 1; i < len(c); i++ { // 插入排序, 首小时最多 60 个点
		for k := i; k > 0 && c[k] < c[k-1]; k-- {
			c[k], c[k-1] = c[k-1], c[k]
		}
	}
	n := len(c)
	if n%2 == 1 {
		return c[n/2], true
	}
	return (c[n/2-1] + c[n/2]) / 2, true
}
