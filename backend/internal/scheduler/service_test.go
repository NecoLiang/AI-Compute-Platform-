package scheduler

import (
	"strings"
	"testing"
)

func intp(v int) *int { return &v }

func TestScoreNode_OfflineUnavailable(t *testing.T) {
	a := scoreNode(Node{NodeName: "n1", Status: "offline", AvailableCards: 8, TotalCards: 8}, 4, expectedBeats24h)
	if a.Verdict != "unavailable" || a.Score != 0 {
		t.Fatalf("离线节点应不可调度: %+v", a)
	}
}

func TestScoreNode_InsufficientCards(t *testing.T) {
	a := scoreNode(Node{NodeName: "n1", Status: "online", AvailableCards: 2, TotalCards: 8}, 4, expectedBeats24h)
	if a.Verdict != "unavailable" {
		t.Fatalf("容量不足应不可调度: %+v", a)
	}
	if !strings.Contains(strings.Join(a.Reasons, ";"), "不足") {
		t.Errorf("理由应说明容量不足: %v", a.Reasons)
	}
}

func TestScoreNode_BestFitPrefersTighterNode(t *testing.T) {
	// 需求 4 卡: 可用 4 卡的节点应比可用 16 卡的得分高(best-fit 减少碎片)
	tight := scoreNode(Node{NodeName: "tight", Status: "online", AvailableCards: 4, TotalCards: 8, GPUUtilPct: intp(50)}, 4, expectedBeats24h)
	loose := scoreNode(Node{NodeName: "loose", Status: "online", AvailableCards: 16, TotalCards: 16, GPUUtilPct: intp(50)}, 4, expectedBeats24h)
	if tight.Score <= loose.Score {
		t.Errorf("best-fit 应偏好贴合容量: tight=%d loose=%d", tight.Score, loose.Score)
	}
}

func TestScoreNode_LoadAndStability(t *testing.T) {
	idle := scoreNode(Node{Status: "online", AvailableCards: 8, TotalCards: 8, GPUUtilPct: intp(0)}, 4, expectedBeats24h)
	busy := scoreNode(Node{Status: "online", AvailableCards: 8, TotalCards: 8, GPUUtilPct: intp(90)}, 4, expectedBeats24h)
	if idle.Score <= busy.Score {
		t.Errorf("低负载应得分更高: idle=%d busy=%d", idle.Score, busy.Score)
	}
	flaky := scoreNode(Node{Status: "online", AvailableCards: 8, TotalCards: 8, GPUUtilPct: intp(0)}, 4, expectedBeats24h/10)
	if flaky.Score >= idle.Score {
		t.Errorf("心跳在线率低应扣稳定性分: flaky=%d idle=%d", flaky.Score, idle.Score)
	}
	// 心跳数异常超量(重复上报)稳定性分也不能超过 10
	over := scoreNode(Node{Status: "online", AvailableCards: 8, TotalCards: 8, GPUUtilPct: intp(0)}, 4, expectedBeats24h*3)
	if over.Score != idle.Score {
		t.Errorf("稳定性分应封顶: over=%d idle=%d", over.Score, idle.Score)
	}
}

func TestSummarize_PicksBestAndMarksRecommended(t *testing.T) {
	nodes := []NodeAdvice{
		{NodeName: "a", Verdict: "unavailable"},
		{NodeName: "b", Verdict: "alternative", Score: 88},
		{NodeName: "c", Verdict: "alternative", Score: 70},
	}
	s := summarize(nodes, 4)
	if !strings.Contains(s, "b") || nodes[1].Verdict != "recommended" {
		t.Errorf("应推荐得分最高的可用节点: %s %+v", s, nodes)
	}
	if nodes[2].Verdict != "alternative" {
		t.Errorf("次优节点应保持 alternative: %+v", nodes[2])
	}

	none := []NodeAdvice{{Verdict: "unavailable"}}
	if !strings.Contains(summarize(none, 4), "无可调度") {
		t.Error("全部不可用时应如实说明")
	}
}

func TestHashKey_Deterministic(t *testing.T) {
	if hashKey("nk-abc") != hashKey("nk-abc") || hashKey("nk-abc") == hashKey("nk-abd") {
		t.Error("hashKey 不稳定或碰撞")
	}
	if len(hashKey("x")) != 64 {
		t.Error("应为 sha256 hex")
	}
}

func managedNode(name string) Node {
	// available_cards 故意填与卡槽不一致的值: 托管口径不得读它。
	return Node{NodeName: name, Status: "online", MgmtMode: "ssh", VerifyStatus: "verified", Schedulable: true,
		AvailableCards: 99, TotalCards: 99}
}

func slotsOf(quota bool, groups ...SlotGroup) nodeSlots {
	for i := range groups {
		groups[i].Quota = quota
	}
	return groupSlots(groups)[0]
}

// docs/26 §26.5.5: 托管商品容量与碎片按卡槽计算, 权重不变。
func TestScoreManagedNode_SlotsCapacityAndFragmentation(t *testing.T) {
	// 两台 8 卡机: tight 剩 4 张且同组, loose 8 张全空闲。需求 4 → tight 贴合得分更高, 同权重下与 scoreNode 一致。
	tight := scoreManagedNode(managedNode("tight"), slotsOf(true,
		SlotGroup{TopoGroup: "g0", Total: 4, Free: 0}, SlotGroup{TopoGroup: "g1", Total: 4, Free: 4}), 4, 0, false)
	loose := scoreManagedNode(managedNode("loose"), slotsOf(true,
		SlotGroup{TopoGroup: "g0", Total: 4, Free: 4}, SlotGroup{TopoGroup: "g1", Total: 4, Free: 4}), 4, 0, false)
	if tight.AvailableCards != 4 || tight.TotalCards != 8 || tight.CapacitySource != "gpu_slots" || *tight.MaxGroupFree != 4 {
		t.Fatalf("容量应按卡槽计算: %+v", tight)
	}
	same := scoreNode(Node{Status: "online", AvailableCards: 4, TotalCards: 8}, 4, 0)
	if tight.Score != same.Score || tight.Score <= loose.Score {
		t.Fatalf("权重须不变且 best-fit: tight=%d same=%d loose=%d", tight.Score, same.Score, loose.Score)
	}
	if !strings.Contains(strings.Join(tight.Reasons, ";"), "同一拓扑组内分配") {
		t.Errorf("应说明拓扑: %v", tight.Reasons)
	}

	// 碎片: 2+2 空闲分在两组, 需求 4 → 可调度但提示跨组拼接; 需求 6 → 容量不足。
	frag := slotsOf(true, SlotGroup{TopoGroup: "g0", Total: 4, Free: 2}, SlotGroup{TopoGroup: "g1", Total: 4, Free: 2})
	a := scoreManagedNode(managedNode("frag"), frag, 4, 0, false)
	if a.Verdict == "unavailable" || *a.MaxGroupFree != 2 || !strings.Contains(strings.Join(a.Reasons, ";"), "跨拓扑组拼接") {
		t.Fatalf("碎片节点: %+v", a)
	}
	if a := scoreManagedNode(managedNode("frag"), frag, 6, 0, false); a.Verdict != "unavailable" {
		t.Fatalf("容量不足应不可调度: %+v", a)
	}

	// 本订单租约已占的卡计入可用。
	own := slotsOf(true, SlotGroup{TopoGroup: "g0", Total: 4, Free: 0, Own: 4}, SlotGroup{TopoGroup: "g1", Total: 4, Free: 0})
	if a := scoreManagedNode(managedNode("own"), own, 4, 0, false); a.Verdict == "unavailable" || a.AvailableCards != 4 ||
		!strings.Contains(strings.Join(a.Reasons, ";"), "已在本节点预留 4 张卡") {
		t.Fatalf("自有预留应计入: %+v", a)
	}
}

// 可调度条件与卡槽分配候选一致。
func TestScoreManagedNode_Eligibility(t *testing.T) {
	full := func(quota bool) nodeSlots {
		return slotsOf(quota, SlotGroup{TopoGroup: "g0", Total: 4, Free: 4}, SlotGroup{TopoGroup: "g1", Total: 4, Free: 4})
	}
	half := slotsOf(true, SlotGroup{TopoGroup: "g0", Total: 4, Free: 4}, SlotGroup{TopoGroup: "g1", Total: 4, Free: 0})
	cases := []struct {
		name  string
		node  func(Node) Node
		slots nodeSlots
		need  int
		whole bool
		want  string // 空=可调度
	}{
		{"ok", nil, full(true), 4, false, ""},
		{"unverified", func(n Node) Node { n.VerifyStatus = "verifying"; return n }, full(true), 4, false, "未通过验收"},
		{"paused", func(n Node) Node { n.Schedulable = false; return n }, full(true), 4, false, "暂停调度"},
		{"degraded", func(n Node) Node { n.Status = "degraded"; return n }, full(true), 4, false, "非在线"},
		{"offline", func(n Node) Node { n.Status = "offline"; return n }, full(true), 4, false, "离线"},
		{"no quota partial", nil, full(false), 4, false, "只能整机"},
		{"no quota whole", nil, full(false), 8, false, ""},
		{"insufficient", nil, half, 8, false, "不足"},
		{"bare metal partial", nil, full(true), 4, true, "整机交付"},
		{"bare metal busy", nil, half, 8, true, "整机交付"},
		{"bare metal ok", nil, full(true), 8, true, ""},
		{"no slots", nil, nodeSlots{}, 4, false, "尚无卡槽"},
	}
	for _, c := range cases {
		n := managedNode(c.name)
		if c.node != nil {
			n = c.node(n)
		}
		a := scoreManagedNode(n, c.slots, c.need, 0, c.whole)
		reasons := strings.Join(a.Reasons, ";")
		if c.want == "" {
			if a.Verdict == "unavailable" {
				t.Errorf("%s: 应可调度: %v", c.name, reasons)
			}
			continue
		}
		if a.Verdict != "unavailable" || !strings.Contains(reasons, c.want) {
			t.Errorf("%s: want unavailable(%s), got %s %v", c.name, c.want, a.Verdict, reasons)
		}
	}
}
