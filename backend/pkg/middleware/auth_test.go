package middleware

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
)

func TestAuthRequiredFailsClosedWhenRevocationStateIsUnavailable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	const secret = "test-access-secret-at-least-32-characters"
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		UserID: 1,
		Roles:  []string{"buyer"},
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Minute)),
		},
	})
	signed, err := token.SignedString([]byte(secret))
	if err != nil {
		t.Fatal(err)
	}

	rdb := redis.NewClient(&redis.Options{
		Network:    "unix",
		Addr:       filepath.Join(t.TempDir(), "missing.sock"),
		MaxRetries: -1,
	})
	defer rdb.Close()

	reached := false
	router := gin.New()
	router.GET("/protected", AuthRequired(secret, rdb, nil), func(c *gin.Context) {
		reached = true
		c.Status(http.StatusNoContent)
	})

	request := httptest.NewRequest(http.MethodGet, "/protected", nil)
	request.Header.Set("Authorization", "Bearer "+signed)
	responseRecorder := httptest.NewRecorder()
	router.ServeHTTP(responseRecorder, request)
	if responseRecorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", responseRecorder.Code)
	}
	if reached {
		t.Fatal("protected handler ran without a verified revocation state")
	}

	request = httptest.NewRequest(http.MethodGet, "/protected", nil)
	request.Header.Set("Authorization", "Bearer invalid")
	responseRecorder = httptest.NewRecorder()
	router.ServeHTTP(responseRecorder, request)
	if responseRecorder.Code != http.StatusUnauthorized {
		t.Fatalf("expected invalid JWT to fail before Redis with 401, got %d", responseRecorder.Code)
	}
}
