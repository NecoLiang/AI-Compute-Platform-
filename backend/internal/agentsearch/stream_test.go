package agentsearch

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

const streamedAssessment = `{"relevant":true,"summary":"需要94GB显存，建议2卡A100。","purpose":"推理","compute_estimate":{"total_vram_gb":94,"per_card_vram_gb":80,"min_cards":2,"basis":"72×1×1.3≈94"},"machine_plans":[{"name":"均衡","gpu_model":"A100-80G","cards":2,"nodes":1,"per_card_vram_gb":80}],"gpu_models":["A100-80G"],"pricing_mode":"monthly","analysis_steps":[]}`

func writeDelta(w http.ResponseWriter, content string) {
	b, _ := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": content, "reasoning_content": "PRIVATE_REASONING"}}}})
	fmt.Fprintf(w, "data: %s\r\n\r\n", b)
	w.(http.Flusher).Flush()
}

func streamClient(t *testing.T, handler http.HandlerFunc) *LLMClient {
	t.Helper()
	server := httptest.NewServer(handler)
	t.Cleanup(server.Close)
	return NewLLMClient(LLMConfig{BaseURL: server.URL, APIKey: "fixture", Model: "fixture", TimeoutSeconds: 3})
}

func TestSummaryBoundaries(t *testing.T) {
	for _, raw := range []string{
		`{"relevant":true,"purpose":"嵌入的\"summary\":\"不要泄漏\"","summary":"显存\"94GB\"，GPU 🖥。"}`,
		`{"relevant":true,"summary":"显存\u0039\u0034GB \ud83d\udda5。"}`,
		`{"nested":{"summary":"隐藏字段"},"relevant":true,"summary":"正确结论"}`,
	} {
		var full struct {
			Summary string `json:"summary"`
		}
		json.Unmarshal([]byte(raw), &full)
		for i := 1; i <= len(raw); i++ {
			got, _ := partialSummary(raw[:i])
			if !strings.HasPrefix(full.Summary, got) {
				t.Fatalf("invalid partial %q at byte %d", got, i)
			}
		}
		if got, done := partialSummary(raw); got != full.Summary || !done {
			t.Fatalf("got %q, complete %v", got, done)
		}
	}
	if got, _ := partialSummary(`{"relevant":false,"summary":"不可展示"}`); got != "" {
		t.Fatal("refusal leaked summary")
	}
	if got, _ := partialSummary(`{"summary":"晚判定","relevant":true}`); got != "晚判定" {
		t.Fatal("reordered fields lost summary")
	}
}

func TestSummaryArrivesBeforeGatewayCompletion(t *testing.T) {
	release := make(chan struct{})
	llm := streamClient(t, func(w http.ResponseWriter, r *http.Request) {
		var input map[string]any
		json.NewDecoder(r.Body).Decode(&input)
		if input["stream"] != true {
			t.Error("gateway request is not streaming")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		prefix := `{"relevant":true,"summary":"需要`
		writeDelta(w, prefix)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		writeDelta(w, strings.TrimPrefix(streamedAssessment, prefix))
		fmt.Fprint(w, "data: [DONE]\n\n")
	})
	svc := NewService(llm, fakeLister{sampleProducts()})
	summaries := make(chan string, 10)
	finished := make(chan error, 1)
	go func() {
		result, err := svc.SearchStream(context.Background(), 1, "72B推理", func(text string) error { summaries <- text; return nil })
		if err == nil && (result.Summary != "需要94GB显存，建议2卡A100。" || len(result.Matches) == 0) {
			err = fmt.Errorf("incomplete result")
		}
		finished <- err
	}()
	select {
	case text := <-summaries:
		if text != "需要" {
			t.Errorf("got %q", text)
		}
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("summary buffered until completion")
	}
	close(release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
}

func streamRouter(svc *Service) *gin.Engine {
	r := gin.New()
	r.Use(func(c *gin.Context) { c.Set("user_id", int64(1)) })
	NewHandler(svc).RegisterBuyerRoutes(r.Group("/api/v1"))
	return r
}

func TestStreamRefusalErrorsAndSharedLimit(t *testing.T) {
	for _, test := range []struct {
		name, content string
		done          bool
		want          string
	}{
		{"refusal", `{"relevant":false,"summary":"不可见","reject_reason":"请描述算力需求"}`, true, "event: result"},
		{"truncated", `{"relevant":true,"summary":"尚未完成`, false, "event: error"},
	} {
		t.Run(test.name, func(t *testing.T) {
			llm := streamClient(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				writeDelta(w, test.content)
				if test.done {
					fmt.Fprint(w, "data: [DONE]\n\n")
				}
			})
			svc := NewService(llm, fakeLister{sampleProducts()})
			response := httptest.NewRecorder()
			streamRouter(svc).ServeHTTP(response, httptest.NewRequest("POST", "/api/v1/market/agent-search/stream", strings.NewReader(`{"query":"模型推理"}`)))
			body := response.Body.String()
			if !strings.Contains(body, test.want) || strings.Contains(body, "PRIVATE_REASONING") {
				t.Fatal(body)
			}
			if test.name == "refusal" && (strings.Contains(body, "event: summary") || strings.Contains(body, "不可见")) {
				t.Fatal("refusal streamed summary")
			}
			if test.name == "truncated" && strings.Contains(body, "event: result") {
				t.Fatal("truncated stream accepted")
			}
			for i := 1; i < rateLimitPerMin; i++ {
				svc.allowRate(1)
			}
			if _, err := svc.Search(context.Background(), 1, "A100"); err != ErrRateLimited {
				t.Fatal("JSON and SSE limits are not shared")
			}
		})
	}
}

func TestStreamDisconnectCancelsGateway(t *testing.T) {
	cancelled := make(chan struct{})
	llm := streamClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		writeDelta(w, `{"relevant":true,"summary":"开始`)
		<-r.Context().Done()
		close(cancelled)
	})
	server := httptest.NewServer(streamRouter(NewService(llm, fakeLister{sampleProducts()})))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, "POST", server.URL+"/api/v1/market/agent-search/stream", strings.NewReader(`{"query":"A100推理"}`))
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if line, _ := bufio.NewReader(response.Body).ReadString('\n'); line != "event: summary\n" {
		t.Fatal(line)
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("gateway request survived downstream cancellation")
	}
}
