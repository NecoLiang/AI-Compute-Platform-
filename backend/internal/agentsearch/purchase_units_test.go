package agentsearch

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/gin-gonic/gin"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"tokenfactory/internal/compute"
)

func TestMatchProductsUsesPurchaseUnitsAndMinimums(t *testing.T) {
	machines := 5
	p := compute.Product{ID: 1, ProductType: "outright", GpuModel: "H100", CardCount: 40, MachineCount: &machines, Stock: 5, UnitPrice: 1000, PricingMode: "perpetual", MinOrder: 1, MinDuration: 1}
	req := parsedRequirement{Relevant: true, GPUModels: []string{"H100"}, CardCount: 16, DurationHint: 10, PricingMode: "perpetual", BudgetFenMax: 2000}
	matches := matchProducts(req, []compute.Product{p})
	if len(matches) != 1 || !strings.Contains(strings.Join(matches[0].Reasons, ";"), "预估费用 20.00 元在预算内") {
		t.Fatal("two machines for16cards must cost20yuan once, not16units times10periods")
	}
	p.ProductType = "card_rental"
	p.PricingMode = "daily"
	p.MinOrder = 8
	p.MinDuration = 3
	p.Stock = 16
	p.UnitPrice = 100
	req.CardCount = 2
	req.DurationHint = 1
	req.PricingMode = "daily"
	req.BudgetFenMax = 1000
	matches = matchProducts(req, []compute.Product{p})
	if len(matches) != 1 || strings.Contains(strings.Join(matches[0].Reasons, ";"), "在预算内") {
		t.Fatal("minimum8units times3days costs24yuan and exceeds10yuan budget")
	}
	p.ProductType = "center"
	p.CardCount = 0
	p.MachineCount = nil
	matches = matchProducts(req, []compute.Product{p})
	if len(matches) != 1 || strings.Contains(strings.Join(matches[0].Reasons, ";"), "在预算内") {
		t.Fatal("unknown per-machine capacity must not produce a within-budget claim")
	}
	req.CardCount = 0
	req.BudgetFenMax = 10000
	matches = matchProducts(req, []compute.Product{p})
	if len(matches) != 1 || strings.Contains(strings.Join(matches[0].Reasons, ";"), "在预算内") {
		t.Fatal("unknown demand and unknown machine specification must not imply an affordable order")
	}
	p.ProductType = "card_rental"
	p.PriceNegotiable = true
	matches = matchProducts(req, []compute.Product{p})
	if len(matches) != 1 || strings.Contains(strings.Join(matches[0].Reasons, ";"), "在预算内") {
		t.Fatal("negotiable pricing must not produce a within-budget claim")
	}
	p.PriceNegotiable = false
	p.PricingMode = "hourly"
	matches = matchProducts(req, []compute.Product{p})
	if len(matches) != 1 || strings.Contains(strings.Join(matches[0].Reasons, ";"), "在预算内") {
		t.Fatal("different billing units must not be compared as the same budget")
	}
}

func TestSearchRejectsUnboundedModelNumbers(t *testing.T) {
	for _, field := range []string{"card_count", "duration_hint", "budget_fen_max", "min_cards"} {
		parsed := map[string]any{"relevant": true, "compute_estimate": map[string]any{"min_cards": 1}}
		if field == "min_cards" {
			parsed["compute_estimate"].(map[string]any)[field] = 1e30
		} else {
			parsed[field] = 1e30
		}
		raw, err := json.Marshal(parsed)
		if err != nil {
			t.Fatal(err)
		}
		service := NewService(fakeLLM(t, string(raw)), fakeLister{sampleProducts()})
		if _, err = service.Search(context.Background(), 1, "H100"); err == nil {
			t.Fatalf("unbounded %s must be rejected before integer conversion", field)
		}
	}
}

func TestSearchFailureDoesNotLogPrivateContent(t *testing.T) {
	const marker = "PRIVATE_QUERY_MUST_NOT_BE_LOGGED"
	var logs bytes.Buffer
	original := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(original) })
	for _, status := range []int{200, 503} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
			if status == 200 {
				json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]string{"content": marker}}}})
			} else {
				w.Write([]byte(marker))
			}
		}))
		service := NewService(NewLLMClient(LLMConfig{BaseURL: server.URL, APIKey: "fixture-key", Model: "fixture"}), fakeLister{sampleProducts()})
		router := gin.New()
		router.Use(func(c *gin.Context) { c.Set("user_id", int64(1)); c.Set("request_id", "fixture-request") })
		NewHandler(service).RegisterBuyerRoutes(router.Group("/api/v1"))
		response := httptest.NewRecorder()
		request := httptest.NewRequest("POST", "/api/v1/market/agent-search", strings.NewReader(`{"query":"`+marker+`"}`))
		request.Header.Set("Content-Type", "application/json")
		router.ServeHTTP(response, request)
		server.Close()
		if strings.Contains(logs.String(), marker) || strings.Contains(response.Body.String(), marker) {
			t.Fatalf("private content escaped through error path for status%d", status)
		}
		if !strings.Contains(logs.String(), "fixture-request") {
			t.Fatal("error must retain request correlation")
		}
	}
}
