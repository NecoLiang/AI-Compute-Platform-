package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSMSCodeConsumesCaptchaBeforeSending(t *testing.T) {
	used := map[string]bool{}
	capServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		token := body["response"]
		success := token == "once" && !used[token]
		used[token] = true
		require.NoError(t, json.NewEncoder(w).Encode(map[string]bool{"success": success}))
	}))
	defer capServer.Close()

	repo := newFakeUserRepository()
	_, err := repo.CreateUser("13800138000", "", "unused-password-hash")
	require.NoError(t, err)
	sender := &countingSMSSender{}
	svc := newSMSService(repo, sender, &fakeSMSCodeStore{})

	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(svc, NewCapVerifier(capServer.URL, "site-secret", "")).RegisterPublicRoutes(router.Group("/api/v1"))

	require.Equal(t, 0, postSMSCode(t, router, "once"))
	require.Equal(t, 1, sender.calls)
	for _, token := range []string{"invalid", "expired", "once"} {
		require.NotEqual(t, 0, postSMSCode(t, router, token))
		require.Equal(t, 1, sender.calls)
	}
}

func postSMSCode(t *testing.T, handler http.Handler, captchaToken string) int {
	t.Helper()
	body, err := json.Marshal(map[string]string{
		"phone":         "13800138000",
		"purpose":       "login",
		"captcha_token": captchaToken,
	})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/auth/sms/code", bytes.NewReader(body)))
	var envelope struct {
		Code int `json:"code"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &envelope))
	return envelope.Code
}

type countingSMSSender struct {
	calls int
}

func (s *countingSMSSender) SendCode(context.Context, string, string, string) error {
	s.calls++
	return nil
}
