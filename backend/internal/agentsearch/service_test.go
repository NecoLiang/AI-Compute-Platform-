package agentsearch

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"tokenfactory/internal/compute"
)

type fakeLister struct{ products []compute.Product }

func (f fakeLister) ListProducts(_ compute.ProductFilter) ([]compute.Product, int64, error) {
	return f.products, int64(len(f.products)), nil
}

func sampleProducts() []compute.Product {
	return []compute.Product{
		{ID: 1, GpuModel: "A100-80G", Stock: 16, UnitPrice: 1200000, Region: "华北-廊坊", PricingMode: "monthly"},
		{ID: 2, GpuModel: "RTX 4090", Stock: 64, UnitPrice: 180000, Region: "华东-上海", PricingMode: "monthly"},
		{ID: 3, GpuModel: "H100", Stock: 2, UnitPrice: 3800000, Region: "华北-廊坊", PricingMode: "monthly"},
	}
}

// fakeLLM 返回固定解析结果的 OpenAI 兼容网关。
func fakeLLM(t *testing.T, parsed string) *LLMClient {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-key" {
			w.WriteHeader(401)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{
			"choices": []map[string]any{{"message": map[string]string{"content": parsed}}},
		})
	}))
	t.Cleanup(srv.Close)
	return NewLLMClient(LLMConfig{BaseURL: srv.URL, APIKey: "test-key", Model: "test-model"})
}

func TestSearch_EndToEnd(t *testing.T) {
	parsed := `{"relevant":true,"reject_reason":"","purpose":"7B 模型微调",
		"compute_estimate":{"total_vram_gb":124,"per_card_vram_gb":80,"min_cards":8,
		  "compute_class":"训练-中等规模","basis":"7B×16字节×1.1≈124GB, 按80G卡向上取整并按2的幂对齐为8卡"},
		"gpu_models":["A100-80G"],"card_count":8,"pricing_mode":"monthly","duration_hint":1,
		"budget_fen_max":20000000,"region":"华北",
		"analysis_steps":[{"title":"识别任务类型","detail":"7B 模型微调, 需要大显存"},
		{"title":"算力推定","detail":"约需 124GB 显存, 建议 8 卡 A100-80G"}]}`
	svc := NewService(fakeLLM(t, parsed), fakeLister{sampleProducts()})

	res, err := svc.Search(context.Background(), 1, "我要微调一个7B模型, 预算每月20万")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if !res.Relevant || len(res.AnalysisSteps) != 2 {
		t.Fatalf("分析过程缺失: %+v", res)
	}
	if res.ComputeEstimate == nil || res.ComputeEstimate.MinCards != 8 || res.ComputeEstimate.Basis == "" {
		t.Fatalf("算力推定缺失: %+v", res.ComputeEstimate)
	}
	if len(res.Matches) == 0 || res.Matches[0].Product.ID != 1 {
		t.Fatalf("应匹配到 A100-80G 商品: %+v", res.Matches)
	}
	top := res.Matches[0]
	// 型号40 + 库存20 + 预算(8卡*1月*12000元=9.6万 ≤ 20万)20 + 地域10 + 计费10 = 100
	if top.Score != 100 {
		t.Errorf("满配匹配应得 100 分, got %d (%v)", top.Score, top.Reasons)
	}
	// 用户明确了型号, 不匹配的商品必须出局, 不许凑数
	for _, m := range res.Matches {
		if m.Product.ID == 2 {
			t.Error("4090 不应出现在 A100 需求的结果里")
		}
	}
}

func TestSearch_IrrelevantQueryRejected(t *testing.T) {
	parsed := `{"relevant":false,"reject_reason":"该问题与算力资源采购无关","purpose":"",
		"gpu_models":[],"card_count":0,"pricing_mode":"","duration_hint":0,"budget_fen_max":0,
		"region":"","analysis_steps":[]}`
	svc := NewService(fakeLLM(t, parsed), fakeLister{sampleProducts()})
	res, err := svc.Search(context.Background(), 1, "帮我写一首诗")
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if res.Relevant || res.RejectReason == "" || len(res.Matches) != 0 || res.AnalysisSteps != nil {
		t.Fatalf("无关问题应拒答且不输出分析/商品: %+v", res)
	}
}

func TestSearch_NotConfigured(t *testing.T) {
	svc := NewService(NewLLMClient(LLMConfig{}), fakeLister{sampleProducts()})
	if _, err := svc.Search(context.Background(), 1, "8卡A100"); err != ErrAINotConfigured {
		t.Errorf("未配置应返回 ErrAINotConfigured, got %v", err)
	}
}

func TestSearch_RateLimit(t *testing.T) {
	parsed := `{"relevant":true,"purpose":"x","gpu_models":[],"card_count":0,"pricing_mode":"",
		"duration_hint":0,"budget_fen_max":0,"region":"","analysis_steps":[],"reject_reason":""}`
	svc := NewService(fakeLLM(t, parsed), fakeLister{sampleProducts()})
	for i := 0; i < rateLimitPerMin; i++ {
		if _, err := svc.Search(context.Background(), 7, "A100"); err != nil {
			t.Fatalf("第 %d 次不应被限流: %v", i+1, err)
		}
	}
	if _, err := svc.Search(context.Background(), 7, "A100"); err != ErrRateLimited {
		t.Errorf("超限应返回 ErrRateLimited, got %v", err)
	}
	// 其他用户不受影响
	if _, err := svc.Search(context.Background(), 8, "A100"); err != nil {
		t.Errorf("不同用户不应被连坐: %v", err)
	}
}

// 用户没明确说卡数时, 用算力推定的 min_cards 兜底做库存匹配。
// min_cards 给 7.5 这类小数时向上取整(算力宁多勿少)。
func TestMatchProducts_MinCardsFallback(t *testing.T) {
	req := parsedRequirement{Relevant: true, GPUModels: []string{"H100"}}
	req.RawEstimate.MinCards = 7.5
	if req.needCards() != 8 {
		t.Fatalf("min_cards 7.5 应向上取整为 8, got %d", req.needCards())
	}
	matches := matchProducts(req, sampleProducts())
	if len(matches) != 1 {
		t.Fatalf("只应命中 H100: %+v", matches)
	}
	joined := strings.Join(matches[0].Reasons, ";")
	if !strings.Contains(joined, "不足 8 卡") {
		t.Errorf("应按推定的 8 卡校验库存: %v", matches[0].Reasons)
	}
}

func TestMatchProducts_BudgetAndStock(t *testing.T) {
	req := parsedRequirement{Relevant: true, GPUModels: []string{"H100"}, CardCount: 8,
		DurationHint: 1, BudgetFenMax: 10000000}
	matches := matchProducts(req, sampleProducts())
	if len(matches) != 1 {
		t.Fatalf("只应命中 H100: %+v", matches)
	}
	m := matches[0]
	// 型号40 + 库存不足(2<8)部分满足5 + 预算超(8*380万) 0 = 45, 且理由里要说清楚
	if m.Score != 45 {
		t.Errorf("score=%d want 45 (%v)", m.Score, m.Reasons)
	}
	joined := strings.Join(m.Reasons, ";")
	if !strings.Contains(joined, "不足") || !strings.Contains(joined, "预算") {
		t.Errorf("理由应如实说明库存不足与预算风险: %v", m.Reasons)
	}
}

func TestStripCodeFence(t *testing.T) {
	if got := stripCodeFence("```json\n{\"a\":1}\n```"); got != `{"a":1}` {
		t.Errorf("fence 剥离失败: %q", got)
	}
	if got := stripCodeFence(`{"a":1}`); got != `{"a":1}` {
		t.Errorf("原样输入被改变: %q", got)
	}
}
