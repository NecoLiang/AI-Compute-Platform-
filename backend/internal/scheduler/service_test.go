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
