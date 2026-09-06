package auth

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
	"tokenfactory/internal/user"
	"tokenfactory/pkg/config"
	"tokenfactory/pkg/middleware"

	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestWeChatLoginRemainsUnavailableWithoutConfiguration(t *testing.T) {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	NewHandler(&Service{}, nil).RegisterPublicRoutes(router.Group("/api/v1"))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/auth/wechat/status", nil))
	require.Equal(t, http.StatusOK, response.Code)
	require.Contains(t, response.Body.String(), `"enabled":false`)
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/auth/wechat/start", strings.NewReader(`{"browser_verifier":"`+strings.Repeat("a", 64)+`"}`)))
	require.Contains(t, response.Body.String(), `"code":40300`)
}

// This exercises the HTTP contract against MySQL and Redis; only WeChat is simulated.
func TestWeChatLoginBindsExistingSupplierAndRejectsReplay(t *testing.T) {
	dsn, redisAddr := os.Getenv("TEST_MYSQL_DSN"), os.Getenv("TEST_REDIS_ADDR")
	if dsn == "" || redisAddr == "" {
		t.Skip("set TEST_MYSQL_DSN and TEST_REDIS_ADDR for the WeChat integration check")
	}
	cfg, err := mysql.ParseDSN(dsn)
	require.NoError(t, err)
	cfg.DBName = ""
	root, err := sqlx.Connect("mysql", cfg.FormatDSN())
	require.NoError(t, err)
	dbName := "omnis_wechat_test_" + time.Now().Format("20060102150405")
	_, err = root.Exec("CREATE DATABASE " + dbName)
	require.NoError(t, err)
	t.Cleanup(func() { root.Exec("DROP DATABASE " + dbName); root.Close() })
	cfg.DBName, cfg.MultiStatements, cfg.ParseTime = dbName, true, true
	db, err := sqlx.Connect("mysql", cfg.FormatDSN())
	require.NoError(t, err)
	t.Cleanup(func() { db.Close() })
	for _, name := range []string{"001_initial_schema.up.sql", "018_wechat_login.up.sql"} {
		schema, err := os.ReadFile("../../migrations/" + name)
		require.NoError(t, err)
		_, err = db.Exec(string(schema))
		require.NoError(t, err)
	}
	rdb := redis.NewClient(&redis.Options{Addr: redisAddr})
	t.Cleanup(func() { rdb.Close() })
	repo, roles := NewRepository(db), user.NewRepository(db)
	userID, err := repo.CreateUser("18800002991", "", "")
	require.NoError(t, err)
	_, err = db.Exec("INSERT INTO user_roles (user_id,role) VALUES (?, 'supplier')", userID)
	require.NoError(t, err)
	svc := NewService(repo, roles, rdb, &fakeSMSSender{}, time.Minute, "test-access-secret-at-least-32-characters", "test-refresh-secret-at-least-32-characters", 900, 604800)
	svc.ConfigureWeChat(config.WeChatConfig{AppID: "wx-test", AppSecret: "test-secret", CallbackURL: "https://omnis.example/api/auth/wechat/callback"}, repo)
	svc.wechatHTTP = &http.Client{Transport: wechatTransport(func(r *http.Request) (*http.Response, error) {
		require.Equal(t, "https://api.weixin.qq.com/sns/oauth2/access_token", r.URL.Scheme+"://"+r.URL.Host+r.URL.Path)
		require.Equal(t, "authorization_code", r.URL.Query().Get("grant_type"))
		if r.URL.Query().Get("code") == "invalid-provider-code" {
			return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"errcode":40029,"errmsg":"invalid code"}`))}, nil
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{"access_token":"provider-token","openid":"wechat-person-a","unionid":"union-a","scope":"snsapi_login"}`))}, nil
	})}
	router := gin.New()
	handler := NewHandler(svc, nil)
	handler.RegisterPublicRoutes(router.Group("/api/v1"))
	protected := router.Group("/api/v1", middleware.AuthRequired(svc.jwtAccessSec, rdb, roles))
	handler.RegisterProtectedRoutes(protected)
	request := func(path string, body any, token string) map[string]any {
		t.Helper()
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		r := httptest.NewRequest(http.MethodPost, "/api/v1/auth/wechat/"+path, bytes.NewReader(encoded))
		r.Header.Set("Content-Type", "application/json")
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, r)
		var result map[string]any
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
		return result
	}
	verifier := strings.Repeat("a", 64)
	start := func() string {
		t.Helper()
		result := request("start", map[string]string{"browser_verifier": verifier}, "")
		require.EqualValues(t, 0, result["code"])
		u, err := url.Parse(result["data"].(map[string]any)["authorize_url"].(string))
		require.NoError(t, err)
		require.Equal(t, "open.weixin.qq.com", u.Host)
		require.Equal(t, "snsapi_login", u.Query().Get("scope"))
		require.Equal(t, "https://omnis.example/api/auth/wechat/callback", u.Query().Get("redirect_uri"))
		return u.Query().Get("state")
	}
	state := start()
	callback := map[string]string{"code": "one-time-code", "state": state, "browser_verifier": strings.Repeat("b", 64)}
	require.EqualValues(t, 40100, request("exchange", callback, "")["code"])
	callback["browser_verifier"] = verifier
	result := request("exchange", callback, "")
	require.EqualValues(t, 0, result["code"])
	data := result["data"].(map[string]any)
	require.Equal(t, true, data["binding_required"])
	require.Nil(t, data["access_token"])
	ticket := data["binding_ticket"].(string)
	require.EqualValues(t, 40100, request("exchange", callback, "")["code"])
	callback["state"] = start()
	competing := request("exchange", callback, "")["data"].(map[string]any)["binding_ticket"].(string)
	tokens, _, err := svc.issueTokens(&User{ID: userID, Phone: "18800002991", Status: "active"})
	require.NoError(t, err)
	bind := map[string]string{"binding_ticket": ticket}
	require.EqualValues(t, 40100, request("bind", bind, "")["code"])
	require.EqualValues(t, 0, request("bind", bind, tokens.AccessToken)["code"])
	otherID, err := repo.CreateUser("18800002992", "", "")
	require.NoError(t, err)
	otherTokens, _, err := svc.issueTokens(&User{ID: otherID, Phone: "18800002992", Status: "active"})
	require.NoError(t, err)
	require.EqualValues(t, 40900, request("bind", map[string]string{"binding_ticket": competing}, otherTokens.AccessToken)["code"])
	require.EqualValues(t, 40100, request("bind", bind, tokens.AccessToken)["code"])
	callback["state"] = start()
	result = request("exchange", callback, "")
	require.EqualValues(t, 0, result["code"])
	data = result["data"].(map[string]any)
	require.NotEmpty(t, data["access_token"])
	account := data["user"].(map[string]any)
	require.EqualValues(t, userID, account["id"])
	require.Contains(t, account["roles"], "supplier")
	callback["state"] = start()
	callback["code"] = "invalid-provider-code"
	require.EqualValues(t, 40100, request("exchange", callback, "")["code"])
	callback["code"] = "one-time-code"
	require.NoError(t, repo.UpdateStatus(userID, "frozen"))
	callback["state"] = start()
	require.EqualValues(t, 40300, request("exchange", callback, "")["code"])
}

type wechatTransport func(*http.Request) (*http.Response, error)

func (f wechatTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
