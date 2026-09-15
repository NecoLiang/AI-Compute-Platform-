package blockchain

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestVerifyRequiresBusinessSourceEvenWhenChainHashMatches(t *testing.T) {
	db := setupChainDB(t)
	chain := newFakeChain(t)
	svc := newTestService(t, db, chain.URL())
	if err := svc.Attest("order", "ORD_SOURCE", OrderPayload{OrderNo: "ORD_SOURCE"}); err != nil {
		t.Fatal(err)
	}
	svc.processPendingOnce(context.Background())
	result, err := svc.Verify(context.Background(), "order", "ORD_SOURCE")
	if err != nil || result.Verified || result.DBHashMatch != nil || result.Note == "" {
		t.Fatalf("missing business source must not verify: result=%+v err=%v", result, err)
	}
}

func TestAttestationHTTPErrorsAreNotEmptySuccesses(t *testing.T) {
	db := setupChainDB(t)
	svc := newTestService(t, db, "")
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handler := NewHandler(svc)
	handler.RegisterRoutes(router.Group("/api/v1"))
	handler.RegisterAdminRoutes(router.Group("/api/v1"))
	request := func(method, path string, want int) {
		t.Helper()
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(method, path, nil))
		var body struct {
			Code int             `json:"code"`
			Data json.RawMessage `json:"data"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil || body.Code != want {
			t.Fatalf("%s: want code %d, got %s, err=%v", path, want, w.Body, err)
		}
		if want != 0 && len(body.Data) != 0 {
			t.Fatalf("failed operation returned success data: %s", w.Body)
		}
	}
	request("GET", "/api/v1/blockchain/attestations/order/MISSING", 0)
	request("POST", "/api/v1/admin/blockchain/requeue-failed", 0)
	db.MustExec("RENAME TABLE blockchain_attestations TO temporarily_unavailable")
	request("GET", "/api/v1/blockchain/attestations/order/MISSING", 50000)
	request("GET", "/api/v1/blockchain/verify?type=order&id=MISSING", 50000)
	request("POST", "/api/v1/admin/blockchain/requeue-failed", 50000)
}
