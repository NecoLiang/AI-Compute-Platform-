package modeldeploy

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 健康检测 L0–L4 合并为一条节点命令(一次 SSH 往返), 各层输出以 @@ 段分隔。
// 引擎端口只绑 127.0.0.1, 由节点本机 curl 访问(节点须装有 curl)。

const (
	probeHTTPTimeoutS   = 5
	probeCanaryTimeoutS = 20
	probeMetricsMaxB    = 256 * 1024
)

// probeSpec 一次检测的参数。
type probeSpec struct {
	DeployNo   string
	Port       int
	ModelKey   string
	Canary     bool
	MetricsMap map[string]string
}

func probeCommand(s probeSpec) (string, error) {
	if !ValidDeployNo(s.DeployNo) || s.Port < 1024 || s.Port > 65535 || !modelKeyPattern.MatchString(s.ModelKey) {
		return "", fmt.Errorf("检测参数非法")
	}
	names := make([]string, 0, len(s.MetricsMap))
	for k, v := range s.MetricsMap {
		if !promNamePattern.MatchString(v) {
			return "", fmt.Errorf("metrics_map.%s 指标名非法", k)
		}
		if k == "ttft_seconds" {
			v += "_bucket"
		}
		names = append(names, v)
	}
	sort.Strings(names)
	base := fmt.Sprintf("http://127.0.0.1:%d", s.Port)
	c := EngineContainer(s.DeployNo)
	var b strings.Builder
	b.WriteString("echo @@inspect; docker inspect --type container -f '{{.State.Status}}|{{.State.OOMKilled}}|{{.State.ExitCode}}' " + c + " 2>/dev/null || echo absent; ")
	fmt.Fprintf(&b, "echo @@health; curl -s -o /dev/null -m %d -w '%%{http_code}' %s/health; echo; ", probeHTTPTimeoutS, base)
	fmt.Fprintf(&b, "echo @@models; if m=$(curl -sf -m %d %s/v1/models) && printf %%s \"$m\" | tr -d ' \\n' | grep -qF '\"id\":\"%s\"'; then echo ok; else echo fail; fi; ",
		probeHTTPTimeoutS, base, s.ModelKey)
	if s.Canary {
		fmt.Fprintf(&b, "echo @@canary; curl -s -o /dev/null -m %d -w '%%{http_code} %%{time_total}' -H 'Content-Type: application/json' "+
			"-d '{\"model\":\"%s\",\"prompt\":\"ping\",\"max_tokens\":1,\"temperature\":0}' %s/v1/completions; echo; ",
			probeCanaryTimeoutS, s.ModelKey, base)
	}
	if len(names) > 0 {
		fmt.Fprintf(&b, "echo @@metrics; curl -sf -m %d %s/metrics | grep -E '^(%s)[{ ]' | head -c %d; echo; ",
			probeHTTPTimeoutS, base, strings.Join(names, "|"), probeMetricsMaxB)
	}
	b.WriteString("echo @@end")
	return b.String(), nil
}

// parseProbe 把检测输出解析为 Observation(卡遥测由调用方另行填入)。
// 容器未运行时 L1–L4 一律记为未执行(nil), 不当作失败计数。输出不完整(无 @@end)返回错误 → 本轮视为不可达。
func parseProbe(out string, at time.Time, canary bool, mm map[string]string) (Observation, error) {
	o := Observation{At: at}
	secs := map[string][]string{}
	sec := ""
	ended := false
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "@@") {
			if t == "@@end" {
				ended = true
				break
			}
			sec = t[2:]
			secs[sec] = secs[sec][:0:0]
			continue
		}
		if sec != "" && t != "" {
			secs[sec] = append(secs[sec], t)
		}
	}
	if !ended {
		return o, fmt.Errorf("检测输出不完整")
	}
	if l := secs["inspect"]; len(l) > 0 && l[0] != "absent" {
		f := strings.Split(l[0], "|")
		if len(f) == 3 {
			o.Container.Exists = true
			o.Container.Status = f[0]
			o.Container.Running = f[0] == "running"
			o.Container.OOMKilled = f[1] == "true"
			o.Container.ExitCode, _ = strconv.Atoi(f[2])
		}
	}
	if !o.Container.Running {
		return o, nil
	}
	code := firstField(secs["health"])
	o.HealthOK = boolp(code == "200")
	o.ModelsOK = boolp(firstField(secs["models"]) == "ok")
	if canary {
		ok, ms := false, (*int)(nil)
		if l := secs["canary"]; len(l) > 0 {
			f := strings.Fields(l[0])
			if len(f) == 2 && f[0] == "200" {
				if sec, err := strconv.ParseFloat(f[1], 64); err == nil && sec >= 0 {
					ok, ms = true, intp(int(math.Round(sec*1000)))
				}
			}
		}
		o.CanaryOK, o.CanaryMS = boolp(ok), ms
	}
	if l := secs["metrics"]; len(l) > 0 {
		if m, err := ParseMetrics(strings.Join(l, "\n"), mm); err == nil {
			o.Metrics = m
		}
	}
	return o, nil
}

func firstField(l []string) string {
	if len(l) == 0 {
		return ""
	}
	return l[0]
}
