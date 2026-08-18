package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCapVerifierAcceptsOnceAndRejectsReplay(t *testing.T) {
	used := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		require.NoError(t, json.NewDecoder(r.Body).Decode(&body))
		require.Equal(t, "site-secret", body["secret"])
		require.Equal(t, "cap-token", body["response"])
		if used {
			w.WriteHeader(http.StatusNotFound)
		}
		_ = json.NewEncoder(w).Encode(map[string]bool{"success": !used})
		used = true
	}))
	defer server.Close()

	verifier := NewCapVerifier(server.URL, "site-secret")
	require.NoError(t, verifier.Verify(context.Background(), "cap-token"))
	require.ErrorIs(t, verifier.Verify(context.Background(), "cap-token"), ErrCaptchaInvalid)
}
