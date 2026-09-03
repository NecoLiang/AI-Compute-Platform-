package admin

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestConfigUpdateRejectsInvalidValueBeforePersistence(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := NewHandler(nil)
	r := gin.New()
	r.PUT("/config", h.UpdateConfig)

	req := httptest.NewRequest(http.MethodPut, "/config", strings.NewReader(`{"key":"fee_rate","value":"10001"}`))
	req.Header.Set("content-type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	if !strings.Contains(w.Body.String(), "费率需为") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}
