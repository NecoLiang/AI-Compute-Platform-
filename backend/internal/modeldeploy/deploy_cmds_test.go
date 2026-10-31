package modeldeploy

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	tUUID0 = "GPU-0b6c3a5e-1f2d-4c3b-9a8e-7d6c5b4a3f21"
	tUUID1 = "GPU-1b6c3a5e-1f2d-4c3b-9a8e-7d6c5b4a3f21"
	tImage = "registry.omnisline.com/vllm/vllm-openai:v0.11.0@sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	tRev   = "0123456789abcdef0123456789abcdef01234567"
)

func testEntry() *ModelEntry {
	return &ModelEntry{ID: 7, ModelKey: "qwen3-32b", Repo: "Qwen/Qwen3-32B", Revision: tRev,
		Manifest: json.RawMessage(`[{"path":"config.json","size":10,"sha256":"` + strings.Repeat("1", 64) + `"},` +
			`{"path":"model-00001.safetensors","size":20,"sha256":"` + strings.Repeat("2", 64) + `"}]`)}
}

// shSyntax 用 sh -n 校验命令可被 shell 正确解析(引号配对等)。
func shSyntax(t *testing.T, cmd string) {
	t.Helper()
	if out, err := exec.Command("sh", "-n", "-c", cmd).CombinedOutput(); err != nil {
		t.Fatalf("sh -n failed: %v %s\n%s", err, out, cmd)
	}
}

func TestEngineRunCommandMatchesSpec(t *testing.T) {
	s := engineSpec{DeployNo: "d-abcdefgh", LeaseNo: "l-7f3k4q2a", Network: "omnis-l-7f3k4q2a-net",
		GPUUUIDs: []string{tUUID0, tUUID1}, ModelDir: "/data/omnis-models/qwen3-32b@" + tRev, ModelKey: "qwen3-32b",
		Image: tImage, Port: 41003, CPUs: 8, MemoryGB: 64, TP: 2, DP: 1, MaxModelLen: 32768, GPUMemUtil: 0.9,
		EngineArgs: json.RawMessage(`{"enable-prefix-caching":true,"enforce-eager":false,"max-num-seqs":256,"dtype":"bfloat16"}`)}
	cmd, err := engineRunCommand(s)
	if err != nil {
		t.Fatal(err)
	}
	want := "docker run -d --name omnis-d-abcdefgh --label omnis.lease=l-7f3k4q2a --label omnis.deploy=d-abcdefgh " +
		`--gpus '"device=` + tUUID0 + "," + tUUID1 + `"' ` +
		"--network omnis-l-7f3k4q2a-net --network-alias omnis-engine -v /data/omnis-models/qwen3-32b@" + tRev + ":/model:ro " +
		"-p 127.0.0.1:41003:8000 --shm-size 16g --restart no --cpus 8 --memory 64g --memory-swap 64g " + tImage +
		" --model /model --served-model-name qwen3-32b --tensor-parallel-size 2 --data-parallel-size 1 --max-model-len 32768 " +
		"--gpu-memory-utilization 0.90 --dtype=bfloat16 --enable-prefix-caching --no-enforce-eager --max-num-seqs=256"
	if cmd != want {
		t.Fatalf("got\n%s\nwant\n%s", cmd, want)
	}
	shSyntax(t, cmd)

	bad := []func(*engineSpec){
		func(s *engineSpec) { s.Image = "vllm/vllm-openai:latest" },
		func(s *engineSpec) { s.GPUUUIDs = []string{"GPU-x;rm -rf /"} },
		func(s *engineSpec) { s.GPUUUIDs = nil },
		func(s *engineSpec) { s.EngineArgs = json.RawMessage(`{"trust-remote-code":true}`) },
		func(s *engineSpec) { s.EngineArgs = json.RawMessage(`{"dtype":"--served-model-name=x"}`) },
		func(s *engineSpec) { s.EngineArgs = json.RawMessage(`{"dtype":"a b"}`) },
		func(s *engineSpec) { s.ModelDir = "/data/$(id)" },
		func(s *engineSpec) { s.Network = "omnis-l-x;id" },
		func(s *engineSpec) { s.Port = 80 },
	}
	for i, mut := range bad {
		c := s
		mut(&c)
		if _, err := engineRunCommand(c); err == nil {
			t.Errorf("case %d: expected rejection", i)
		}
	}
}

func TestManifestChecklistAndPullCommands(t *testing.T) {
	e := testEntry()
	list, sum, err := manifestChecklist(e)
	if err != nil {
		t.Fatal(err)
	}
	if list != strings.Repeat("1", 64)+"  config.json\n"+strings.Repeat("2", 64)+"  model-00001.safetensors\n" || len(sum) != 64 {
		t.Fatalf("checklist %q %s", list, sum)
	}
	e2 := testEntry()
	e2.Manifest = json.RawMessage(`[{"path":"a\nb","size":1,"sha256":"` + strings.Repeat("1", 64) + `"}]`)
	if _, _, err := manifestChecklist(e2); err == nil {
		t.Fatal("newline in path must be rejected")
	}

	// 分块写入后在本机 shell 真实执行, 结果摘要应与期望一致。
	dir := t.TempDir()
	big := strings.Repeat(strings.Repeat("3", 64)+"  shard.safetensors\n", 3000) // ≈ 250KB → 多块
	cmds, err := writeChecklistCommands(dir, e, big)
	if err != nil || len(cmds) < 4 {
		t.Fatalf("%v %d", err, len(cmds))
	}
	var last string
	for _, c := range cmds {
		out, err := exec.Command("sh", "-c", c).CombinedOutput()
		if err != nil {
			t.Fatalf("%v %s", err, out)
		}
		last = strings.TrimSpace(string(out))
	}
	got, _ := os.ReadFile(pullChecklistPath(dir, e))
	if string(got) != big || len(last) != 64 {
		t.Fatalf("written %d bytes, digest %q", len(got), last)
	}

	pull, err := pullRunCommand("/data/omnis-models", tImage, 3, e)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"--name omnis-pull-m7", "--label omnis.pull=3", "-v /data/omnis-models:/data/omnis-models",
		"modelscope download --model Qwen/Qwen3-32B --revision " + tRev, "sha256sum -c --strict --quiet", "mv \"$P\" \"$D\""} {
		if !strings.Contains(pull, want) {
			t.Errorf("pull command missing %q", want)
		}
	}
	shSyntax(t, pull)
	if _, err := pullRunCommand("/data/omnis-models", "busybox:latest", 3, e); err == nil {
		t.Fatal("pull image without digest must be rejected")
	}
	st, _ := pullStatusCommand("/data/omnis-models", e)
	ev, _ := evictCommand("/data/omnis-models", e)
	lc, _ := liveCheckCommand("/data/omnis-models")
	q, _ := leaseQuotaCommand("omnis-l-7f3k4q2a", 8, 64)
	rm, _ := removeEngineCommand("d-abcdefgh")
	lg, _ := engineLogsCommand("d-abcdefgh", 200)
	img, _ := ensureImageCommand(tImage)
	for _, c := range []string{st, ev, lc, q, rm, lg, img, pullFinishCommand(7), markerCheckCommand("/data/x")} {
		shSyntax(t, c)
	}
	if _, err := checkCacheDirOK("/data/../etc"); err == nil {
		t.Fatal("dotdot")
	}
}

func checkCacheDirOK(d string) (string, error) { return d, checkCacheDir(d) }

func TestParseLiveCheck(t *testing.T) {
	out := "@@smi\n" + tUUID0 + ", 3, 81559\n" + tUUID1 + ", 40960, 81559\ngarbage\n@@df\n1234567890\n@@ports\n" +
		"LISTEN 0 4096 127.0.0.1:41000 0.0.0.0:*\n@@end\n"
	lc, err := parseLiveCheck(out)
	if err != nil {
		t.Fatal(err)
	}
	if lc.VRAM[tUUID0] != [2]int64{3, 81559} || lc.VRAM[tUUID1][0] != 40960 || lc.CacheAvail != 1234567890 ||
		!strings.Contains(lc.Listening, "127.0.0.1:41000") {
		t.Fatalf("%+v", lc)
	}
	if _, err := parseLiveCheck("@@smi\n"); err == nil {
		t.Fatal("truncated output must error")
	}
}

// TestProbeScriptEndToEnd 用 PATH 中的替身 docker/curl 真实执行检测脚本, 校验引号/分段与解析。
func TestProbeScriptEndToEnd(t *testing.T) {
	bin := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\n"+body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	metrics := readFixture(t, "vllm_v0.11.0_metrics_t1.txt")
	if err := os.WriteFile(filepath.Join(bin, "metrics.txt"), []byte(metrics), 0o644); err != nil {
		t.Fatal(err)
	}
	write("docker", `echo 'running|false|0'`)
	write("curl", `for a; do last="$a"; done
case "$last" in
*/health) printf 200 ;;
*/v1/models) printf '{"object": "list", "data": [{"id": "qwen3-32b", "object": "model"}]}' ;;
*/v1/completions) case "$*" in *'"max_tokens":1'*) printf '200 0.084' ;; *) printf '400 0.001' ;; esac ;;
*/metrics) cat "`+bin+`/metrics.txt" ;;
esac`)
	cmd, err := probeCommand(probeSpec{DeployNo: "d-abcdefgh", Port: 41003, ModelKey: "qwen3-32b", Canary: true, MetricsMap: v011Map})
	if err != nil {
		t.Fatal(err)
	}
	c := exec.Command("sh", "-c", cmd)
	c.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("%v %s", err, out)
	}
	o, err := parseProbe(string(out), t0, true, v011Map)
	if err != nil {
		t.Fatal(err)
	}
	if !o.Container.Running || !*o.HealthOK || !*o.ModelsOK || !*o.CanaryOK || *o.CanaryMS != 84 {
		t.Fatalf("%+v\n%s", o, out)
	}
	if o.Metrics == nil || *o.Metrics.RequestsWaiting != 4 || len(o.Metrics.TTFT) != 23 {
		t.Fatalf("metrics %+v", o.Metrics)
	}
	// 非目标指标被 grep 过滤掉(e2e 直方图不应出现)。
	if strings.Contains(string(out), "e2e_request_latency") {
		t.Fatal("metrics not filtered")
	}

	// 容器退出: L1–L4 不执行; OOM 标记解析。
	o, err = parseProbe("@@inspect\nexited|true|137\n@@health\n000\n@@models\nfail\n@@end\n", t0, true, v011Map)
	if err != nil || o.Container.Running || !o.Container.OOMKilled || o.Container.ExitCode != 137 || o.HealthOK != nil {
		t.Fatalf("%+v %v", o, err)
	}
	o, _ = parseProbe("@@inspect\nabsent\n@@end\n", t0, true, v011Map)
	if o.Container.Exists {
		t.Fatal("absent")
	}
	// 金丝雀非 200 / 超时。
	o, _ = parseProbe("@@inspect\nrunning|false|0\n@@health\n200\n@@models\nok\n@@canary\n000 20.001\n@@end\n", t0, true, v011Map)
	if *o.CanaryOK || o.CanaryMS != nil || o.Metrics != nil {
		t.Fatalf("%+v", o)
	}
	if _, err := parseProbe("@@inspect\nrunning|false|0\n@@health\n", t0, true, v011Map); err == nil {
		t.Fatal("truncated output must error")
	}
	if _, err := probeCommand(probeSpec{DeployNo: "d-abcdefgh", Port: 41003, ModelKey: "qwen3-32b",
		MetricsMap: map[string]string{"x": "a'b"}}); err == nil {
		t.Fatal("bad metric name")
	}
}
