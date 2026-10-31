package modeldeploy

import (
	"encoding/json"
	"math"
	"testing"
	"time"
)

var t0 = time.Date(2026, 10, 31, 10, 0, 0, 0, time.Local)

func f64(v float64) *float64 { return &v }
func i64(v int64) *int64     { return &v }

func upObs(at time.Time) Observation {
	return Observation{
		At:        at,
		Container: ContainerObs{Exists: true, Running: true, Status: "running"},
		HealthOK:  boolp(true), ModelsOK: boolp(true), CanaryOK: boolp(true), CanaryMS: intp(80),
		Metrics: &MetricsSnapshot{RequestsRunning: f64(2), RequestsWaiting: f64(0), KVCacheUsage: f64(0.3),
			GenTokensTotal: f64(1000), PromptTotal: f64(5000), PreemptTotal: f64(0)},
		Cards: []CardHealth{{UUID: "GPU-a", TempC: intp(60), Throttled: boolp(false), ECCUncorrected: i64(0), ECCCorrected: i64(0)}},
	}
}

func TestJudgeHealthy(t *testing.T) {
	j := Judge(JudgeInput{Obs: upObs(t0), CanaryEnabled: true})
	if j.Health != HealthHealthy || j.Reason != "" || j.Restartable {
		t.Fatalf("%+v", j)
	}
	if !j.Sample.ContainerUp || *j.Sample.KVUsagePct != 30 || *j.Sample.GPUTempMaxC != 60 || j.Sample.GenTPS != nil {
		t.Fatalf("sample %+v", j.Sample)
	}
}

func TestJudgeUnhealthyRules(t *testing.T) {
	cases := []struct {
		name        string
		mut         func(*Observation, *JudgeInput)
		reason      string
		restartable bool
	}{
		{"container exited", func(o *Observation, _ *JudgeInput) {
			o.Container = ContainerObs{Exists: true, Status: "exited", ExitCode: 1}
			o.HealthOK, o.ModelsOK, o.CanaryOK, o.Metrics = nil, nil, nil, nil
		}, ReasonContainerDown, true},
		{"container absent", func(o *Observation, _ *JudgeInput) { o.Container = ContainerObs{} }, ReasonContainerDown, true},
		{"oom", func(o *Observation, _ *JudgeInput) {
			o.Container = ContainerObs{Exists: true, Status: "exited", OOMKilled: true, ExitCode: 137}
		}, ReasonOOMKilled, true},
		{"L1 third failure", func(o *Observation, in *JudgeInput) { o.HealthOK = boolp(false); in.Prev.L1Fails = 2 }, ReasonHealthFailed, true},
		{"L3 third failure", func(o *Observation, in *JudgeInput) { o.CanaryOK = boolp(false); in.Prev.L3Fails = 2 }, ReasonCanaryFailed, false},
		{"fatal xid", func(o *Observation, _ *JudgeInput) { o.Cards[0].FatalXid = true }, ReasonGPUFatal, false},
		{"uncorrectable ecc", func(o *Observation, _ *JudgeInput) { o.Cards[0].ECCUncorrected = i64(1) }, ReasonGPUFatal, false},
		{"fatal xid beats container down: no restart", func(o *Observation, _ *JudgeInput) {
			o.Cards[0].FatalXid = true
			o.Container = ContainerObs{Exists: true, Status: "exited"}
		}, ReasonGPUFatal, false},
	}
	for _, c := range cases {
		in := JudgeInput{Obs: upObs(t0), CanaryEnabled: true}
		c.mut(&in.Obs, &in)
		j := Judge(in)
		if j.Health != HealthUnhealthy || j.Reason != c.reason || j.Restartable != c.restartable {
			t.Errorf("%s: got %s/%s restartable=%v", c.name, j.Health, j.Reason, j.Restartable)
		}
		if j.Sample.Health != HealthUnhealthy {
			t.Errorf("%s: sample health %s", c.name, j.Sample.Health)
		}
	}
}

func TestJudgeConsecutiveCounters(t *testing.T) {
	in := JudgeInput{Obs: upObs(t0), CanaryEnabled: true}
	in.Obs.HealthOK = boolp(false)
	j := Judge(in)
	if j.Health != HealthHealthy || j.Next.L1Fails != 1 {
		t.Fatalf("first L1 failure must not be unhealthy: %+v", j)
	}
	in.Prev = j.Next
	j = Judge(in)
	if j.Next.L1Fails != 2 || j.Health != HealthHealthy {
		t.Fatalf("second: %+v", j)
	}
	// 一次成功清零。
	in.Prev = j.Next
	in.Obs.HealthOK = boolp(true)
	if j = Judge(in); j.Next.L1Fails != 0 {
		t.Fatal("success must reset")
	}
	// 金丝雀关闭时 L3 不参与判定。
	in = JudgeInput{Obs: upObs(t0), CanaryEnabled: false, Prev: MonitorState{L3Fails: 5}}
	in.Obs.CanaryOK = nil
	if j = Judge(in); j.Health != HealthHealthy {
		t.Fatalf("canary disabled: %+v", j)
	}
}

// degradedRun 连续 n 分钟每分钟一轮相同观测, 返回最后一轮判定。
func degradedRun(t *testing.T, minutes int, mut func(i int, o *Observation, in *JudgeInput)) Judgement {
	t.Helper()
	var prev MonitorState
	var j Judgement
	for i := 0; i <= minutes; i++ {
		in := JudgeInput{Obs: upObs(t0.Add(time.Duration(i) * time.Minute)), CanaryEnabled: true, Prev: prev}
		mut(i, &in.Obs, &in)
		j = Judge(in)
		prev = j.Next
	}
	return j
}

func TestJudgeDegradedPersistsFiveMinutes(t *testing.T) {
	saturate := func(_ int, o *Observation, _ *JudgeInput) {
		o.Metrics.RequestsWaiting = f64(3)
		o.Metrics.KVCacheUsage = f64(0.96)
	}
	if j := degradedRun(t, 4, saturate); j.Health != HealthHealthy {
		t.Fatalf("4 min must still be healthy: %s", j.Health)
	}
	j := degradedRun(t, 5, saturate)
	if j.Health != HealthDegraded || j.Reason != ReasonCapacitySaturated {
		t.Fatalf("5 min: %s/%s", j.Health, j.Reason)
	}
	// 中途一轮不满足 → 重新计时。
	j = degradedRun(t, 6, func(i int, o *Observation, in *JudgeInput) {
		if i != 3 {
			saturate(i, o, in)
		}
	})
	if j.Health != HealthHealthy {
		t.Fatalf("interrupted condition must restart timer: %s", j.Health)
	}
	// 排队但 KV 未满 / KV 满但不排队 都不算饱和。
	if j := degradedRun(t, 6, func(_ int, o *Observation, _ *JudgeInput) { o.Metrics.RequestsWaiting = f64(3) }); j.Health != HealthHealthy {
		t.Fatal("waiting alone is not saturation")
	}
	if j := degradedRun(t, 6, func(_ int, o *Observation, _ *JudgeInput) { o.Metrics.KVCacheUsage = f64(0.99) }); j.Health != HealthHealthy {
		t.Fatal("kv alone is not saturation")
	}
}

func TestJudgeDegradedRules(t *testing.T) {
	cases := []struct {
		name   string
		mut    func(i int, o *Observation, in *JudgeInput)
		reason string
	}{
		{"preemptions increasing", func(i int, o *Observation, _ *JudgeInput) { o.Metrics.PreemptTotal = f64(float64(i * 2)) }, ReasonPreempting},
		{"ttft p95 > 3x baseline", func(i int, o *Observation, in *JudgeInput) {
			in.BaselineCanaryMS = intp(100)
			// 每分钟 10 个请求全部落在 0.5–1s 桶 → P95 ≈ 975ms > 300ms。
			n := float64(i * 10)
			o.Metrics.TTFT = []Bucket{{0.1, 0}, {0.5, 0}, {1, n}, {math.Inf(1), n}}
		}, ReasonTTFTSlow},
		{"gpu hot", func(_ int, o *Observation, _ *JudgeInput) { o.Cards[0].TempC = intp(85) }, ReasonGPUHot},
		{"throttled", func(_ int, o *Observation, _ *JudgeInput) { o.Cards[0].Throttled = boolp(true) }, ReasonGPUHot},
		{"corrected ecc rising", func(i int, o *Observation, _ *JudgeInput) { o.Cards[0].ECCCorrected = i64(int64(i)) }, ReasonECCRising},
		{"recent auto restart", func(_ int, _ *Observation, in *JudgeInput) { in.AutoRestartWithinHour = true }, ReasonRecentlyRestarted},
	}
	for _, c := range cases {
		j := degradedRun(t, 6, c.mut)
		if j.Health != HealthDegraded || j.Reason != c.reason {
			t.Errorf("%s: got %s/%s", c.name, j.Health, j.Reason)
		}
	}
	// 无基线时 TTFT 规则不生效; 增量为 0 的抢占不算增长。
	j := degradedRun(t, 6, func(i int, o *Observation, _ *JudgeInput) {
		n := float64(i * 10)
		o.Metrics.TTFT = []Bucket{{1, n}, {math.Inf(1), n}}
		o.Metrics.PreemptTotal = f64(7)
	})
	if j.Health != HealthHealthy {
		t.Fatalf("no baseline / flat preemptions: %s/%s", j.Health, j.Reason)
	}
	// 多个原因同时满足: 取优先级最高者, 全部列出。
	j = degradedRun(t, 6, func(i int, o *Observation, in *JudgeInput) {
		in.AutoRestartWithinHour = true
		o.Cards[0].TempC = intp(90)
	})
	if j.Reason != ReasonGPUHot || len(j.Degraded) != 2 {
		t.Fatalf("priority: %s %v", j.Reason, j.Degraded)
	}
}

func TestJudgeRatesAndPercentiles(t *testing.T) {
	prevAt := t0
	prev := MonitorState{PrevAt: &prevAt, PrevGen: f64(1000), PrevPrompt: f64(5000), PrevPreempt: f64(1),
		PrevTTFT: []Bucket{{0.1, 10}, {1, 10}, {math.Inf(1), 10}}}
	o := upObs(t0.Add(time.Minute))
	o.Metrics.GenTokensTotal, o.Metrics.PromptTotal, o.Metrics.PreemptTotal = f64(7000), f64(17000), f64(3)
	o.Metrics.TTFT = []Bucket{{0.1, 10}, {1, 30}, {math.Inf(1), 30}}
	j := Judge(JudgeInput{Obs: o, Prev: prev, CanaryEnabled: true})
	s := j.Sample
	if *s.GenTPS != 100 || *s.PromptTPS != 200 || *s.Preemptions != 2 {
		t.Fatalf("rates %v %v %v", *s.GenTPS, *s.PromptTPS, *s.Preemptions)
	}
	// 增量 20 个请求全在 (0.1,1]: P50 = 0.1+0.9*0.5 = 0.55s, P95 = 0.1+0.9*0.95 = 0.955s。
	if *s.TTFTP50MS != 550 || *s.TTFTP95MS != 955 {
		t.Fatalf("ttft %v %v", *s.TTFTP50MS, *s.TTFTP95MS)
	}
	// MonitorState 可序列化(含 +Inf 桶)并还原。
	raw, err := json.Marshal(j.Next)
	if err != nil {
		t.Fatal(err)
	}
	var back MonitorState
	if err := json.Unmarshal(raw, &back); err != nil || !math.IsInf(back.PrevTTFT[2].LE, 1) || back.PrevTTFT[1].Count != 30 {
		t.Fatalf("roundtrip %s %v %+v", raw, err, back.PrevTTFT)
	}
}

func TestPlanRestartBackoffAndHourlyCap(t *testing.T) {
	want := []time.Duration{time.Minute, 5 * time.Minute, 15 * time.Minute}
	for n, w := range want {
		d, giveUp := PlanRestart(n)
		if giveUp || d != w {
			t.Fatalf("n=%d: %v %v", n, d, giveUp)
		}
	}
	for _, n := range []int{3, 4, 10} {
		if _, giveUp := PlanRestart(n); !giveUp {
			t.Fatalf("n=%d must give up", n)
		}
	}
}

func TestMedian(t *testing.T) {
	if m, _ := medianInt([]int{30, 10, 20}); m != 20 {
		t.Fatal(m)
	}
	if m, _ := medianInt([]int{40, 10, 20, 30}); m != 25 {
		t.Fatal(m)
	}
	if _, ok := medianInt(nil); ok {
		t.Fatal("empty")
	}
}
