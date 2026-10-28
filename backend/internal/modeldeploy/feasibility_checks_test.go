package modeldeploy

import (
	"strings"
	"testing"
)

// 每个用例只改一项输入, 断言对应检查的 ok/level 与整体 feasible。
func TestEvaluateChecksF1toF8(t *testing.T) {
	cases := []struct {
		name     string
		id       string
		mutate   func(in *Input)
		ok       bool
		level    string
		feasible bool
		msgHas   string
	}{
		{"F1 未登记算力架构", "F1", func(in *Input) { in.ComputeCapability = "" }, false, LevelError, false, "无法判定"},
		{"F1 算力架构格式坏", "F1", func(in *Input) { in.ComputeCapability = "sm90" }, false, LevelError, false, "无法判定"},
		{"F1 恰好等于", "F1", func(in *Input) { in.ComputeCapability = "8.0" }, true, LevelInfo, true, ""},
		{"F1 低于要求", "F1", func(in *Input) { in.ComputeCapability = "7.5" }, false, LevelError, false, "低于"},
		{"F1 10.0 高于 9.0", "F1", func(in *Input) { in.ComputeCapability = "10.0"; in.Model.MinComputeCapability = "9.0" }, true, LevelInfo, true, ""},
		{"F2 驱动未知", "F2", func(in *Input) { in.DriverVersion = "" }, false, LevelError, false, "无法判定"},
		{"F2 驱动过低", "F2", func(in *Input) { in.DriverVersion = "535.86.10" }, false, LevelError, false, "升级驱动"},
		{"F2 驱动恰好等于", "F2", func(in *Input) { in.DriverVersion = "535.104.05" }, true, LevelInfo, true, ""},
		{"F3 显存占用 3%", "F3", func(in *Input) { in.Cards[1].VRAMUsedBytes = in.Cards[1].VRAMTotalBytes * 3 / 100 }, false, LevelError, false, "1 张卡"},
		{"F3 显存占用恰好 2%", "F3", func(in *Input) { in.Cards[1].VRAMUsedBytes = in.Cards[1].VRAMTotalBytes * 2 / 100 }, true, LevelInfo, true, ""},
		{"F4 未缓存且空间恰好 1.1W", "F4", func(in *Input) {
			in.ModelCached, in.CacheFreeBytes, in.CacheEvictableBytes = false, 60*gib64, 11*gib64+gib64/2
		}, true, LevelInfo, true, ""},
		{"F4 未缓存且空间差 1 字节", "F4", func(in *Input) {
			in.ModelCached, in.CacheFreeBytes, in.CacheEvictableBytes = false, 60*gib64, 11*gib64+gib64/2-1
		}, false, LevelError, false, "1.1 倍"},
		{"F4 可清理缓存计入", "F4", func(in *Input) { in.ModelCached, in.CacheFreeBytes, in.CacheEvictableBytes = false, 0, 100*gib64 }, true, LevelInfo, true, ""},
		{"F5 无 NVLink 仅提示", "F5", func(in *Input) { in.NVLink = false }, false, LevelWarning, true, "PCIe"},
		{"F6 卡故障", "F6", func(in *Input) { in.Cards[0].Faulty = true }, false, LevelError, false, "故障"},
		{"F6 致命 Xid", "F6", func(in *Input) { in.Cards[1].FatalXidLastHour = true }, false, LevelError, false, "Xid"},
		{"F7 restricted 需确认", "F7", func(in *Input) { in.Model.CommercialUse = CommercialRestricted }, false, LevelConfirm, true, "许可"},
		{"F7 unknown 需确认", "F7", func(in *Input) { in.Model.CommercialUse = CommercialUnknown }, false, LevelConfirm, true, "尚未核实"},
		{"F8 无验证记录", "F8", func(in *Input) { in.VerifiedGPUModels = nil }, false, LevelConfirm, true, "未在 H100-80G 上实测"},
		{"F8 只验证过别的卡型", "F8", func(in *Input) { in.VerifiedGPUModels = []string{"H800-80G", "A100-80G"} }, false, LevelConfirm, true, "显存估算可能有偏差"},
		{"F8 卡型命中", "F8", func(in *Input) { in.VerifiedGPUModels = []string{"A100-80G", "H100-80G"} }, true, LevelInfo, true, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := healthyInput(spec32B(), 2, 80)
			c.mutate(&in)
			r := mustEval(t, in)
			got := checkByID(t, r, c.id)
			if got.OK != c.ok || got.Level != c.level || r.Feasible != c.feasible || !strings.Contains(got.Message, c.msgHas) {
				t.Fatalf("got check=%+v feasible=%v", got, r.Feasible)
			}
			if !c.feasible && len(r.Plans) != 0 {
				t.Fatal("不可行时 plans 应为空")
			}
			for _, other := range r.Checks {
				if other.ID != c.id && !other.OK {
					t.Fatalf("只应有 %s 不通过, 但 %+v", c.id, other)
				}
			}
		})
	}
}

func TestF1SuggestsAlternatives(t *testing.T) {
	in := healthyInput(spec32B(), 2, 80)
	in.Model.ModelKey, in.Model.MinComputeCapability = "qwen3-32b-fp8", "8.9"
	in.ComputeCapability = "8.0" // A100
	in.Alternatives = []Alternative{
		{ModelKey: "qwen3-32b-fp8", MinComputeCapability: "8.9"}, // 自身, 不列
		{ModelKey: "qwen3-32b-int4-awq", MinComputeCapability: "7.5"},
		{ModelKey: "qwen3-32b-bf16", MinComputeCapability: "8.0"},
		{ModelKey: "llama-fp8", MinComputeCapability: "8.9"},
	}
	c := checkByID(t, mustEval(t, in), "F1")
	if c.OK || !strings.HasSuffix(c.Message, "qwen3-32b-bf16, qwen3-32b-int4-awq") {
		t.Fatalf("应按字典序列出可在 8.0 上运行的其他条目: %q", c.Message)
	}
	in.Alternatives = in.Alternatives[:1]
	if c := checkByID(t, mustEval(t, in), "F1"); !strings.Contains(c.Message, "暂无") {
		t.Fatalf("无替代时应明确说明: %q", c.Message)
	}
}

func TestF3UnknownVRAMIsBusy(t *testing.T) {
	in := healthyInput(spec32B(), 2, 80)
	in.Cards[0].VRAMTotalBytes = 0
	r := mustEval(t, in)
	if r.Feasible || checkByID(t, r, "F3").OK {
		t.Fatalf("显存总量未知的卡不能视为空闲: %+v", r.Checks)
	}
}

func TestF5OnlyWhenMultiCardTP(t *testing.T) {
	in := healthyInput(spec32B(), 1, 80)
	in.NVLink = false
	if c := checkByID(t, mustEval(t, in), "F5"); !c.OK {
		t.Fatalf("单卡不涉及卡间通信: %+v", c)
	}
}

func TestMissingAcks(t *testing.T) {
	in := healthyInput(spec32B(), 2, 80)
	in.Model.CommercialUse, in.VerifiedGPUModels = CommercialRestricted, nil
	r := mustEval(t, in)
	if !r.Feasible {
		t.Fatal("F7/F8 只需确认, 不影响可行")
	}
	ids := func(cs []Check) string {
		var s []string
		for _, c := range cs {
			s = append(s, c.ID+":"+c.Ack)
		}
		return strings.Join(s, ",")
	}
	cases := []struct {
		license, untested bool
		want              string
	}{
		{false, false, "F7:license_ack,F8:untested_ack"},
		{true, false, "F8:untested_ack"},
		{false, true, "F7:license_ack"},
		{true, true, ""},
	}
	for _, c := range cases {
		if got := ids(r.MissingAcks(c.license, c.untested)); got != c.want {
			t.Errorf("MissingAcks(%v,%v)=%q want %q", c.license, c.untested, got, c.want)
		}
	}
	if got := mustEval(t, healthyInput(spec32B(), 2, 80)).MissingAcks(false, false); len(got) != 0 {
		t.Fatalf("allowed+已验证 不需要任何确认: %+v", got)
	}
}

func TestBuildInputMapsSnapshot(t *testing.T) {
	e := validEntry()
	if err := normalizeEntry(e); err != nil {
		t.Fatal(err)
	}
	e.ID = 7
	snap := &LeaseSnapshot{
		GPUModel: "H100-80G", DriverVersion: "550.54.15", NVLink: true, ModelCached: false,
		CacheFreeBytes: 1, CacheEvictableBytes: 2,
		Cards: []CardSnapshot{{UUID: "GPU-a", VRAMTotalBytes: 80 * gib64, VRAMUsedBytes: 5, Faulty: true}},
	}
	in, err := buildInput(e, snap, "9.0", 85, []string{"H100-80G"}, []ModelEntry{*e})
	if err != nil {
		t.Fatal(err)
	}
	if in.Model.ID != 7 || len(in.Model.ValidTP) != 4 || in.Model.ValidTP[3] != 8 || in.ComputeCapability != "9.0" ||
		in.GPUMemUtilPct != 85 || len(in.Cards) != 1 || !in.Cards[0].Faulty || in.Cards[0].VRAMUsedBytes != 5 ||
		in.CacheEvictableBytes != 2 || len(in.Alternatives) != 1 || in.VerifiedGPUModels[0] != "H100-80G" {
		t.Fatalf("映射错误: %+v", in)
	}
}

func TestParseGPUMemUtil(t *testing.T) {
	for s, want := range map[string]int{"": 0, "0.9": 90, "0.90": 90, "0.85": 85, "0.8": 80, "0.95": 95, "0.99": 99} {
		if got, ok := parseGPUMemUtil(s); !ok || got != want {
			t.Errorf("parseGPUMemUtil(%q)=%d,%v", s, got, ok)
		}
	}
	for _, s := range []string{"90", "0.955", "1.0", ".9", "0.9a", "-0.9"} {
		if _, ok := parseGPUMemUtil(s); ok {
			t.Errorf("parseGPUMemUtil(%q) 应拒绝", s)
		}
	}
}
