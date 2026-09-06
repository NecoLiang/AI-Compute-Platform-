package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
	"tokenfactory/pkg/config"
	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/response"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
)

const wechatTTL = 10 * time.Minute

var wechatNoncePattern = regexp.MustCompile(`^[a-f0-9]{64}$`)
var wechatIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var errWeChatExpired = errors.New("微信授权已失效，请重新扫码")
var errWeChatConflict = errors.New("微信或手机号已绑定其他账户，请使用原账户登录")

func (s *Service) ConfigureWeChat(cfg config.WeChatConfig, repo *Repository) {
	s.wechatConfig, s.wechatRepo = cfg, repo
	s.wechatHTTP = &http.Client{Timeout: 10 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
}

func (s *Service) wechatEnabled() bool {
	return s.wechatConfig.AppID != "" && s.wechatConfig.AppSecret != "" && s.wechatConfig.CallbackURL != "" && s.wechatRepo != nil && s.rdb != nil
}

func (h *Handler) WeChatStatus(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	response.Success(c, gin.H{"enabled": h.svc.wechatEnabled()})
}

func (h *Handler) requireWeChat(c *gin.Context) bool {
	c.Header("Cache-Control", "no-store")
	if !h.svc.wechatEnabled() {
		response.Error(c, errcode.Forbidden, "微信登录暂未开放，请使用手机号登录")
		return false
	}
	return true
}

func wechatNonce() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

func wechatHash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func (h *Handler) WeChatStart(c *gin.Context) {
	if !h.requireWeChat(c) {
		return
	}
	var req struct {
		BrowserVerifier string `json:"browser_verifier"`
	}
	if c.ShouldBindJSON(&req) != nil || !wechatNoncePattern.MatchString(req.BrowserVerifier) {
		response.Error(c, errcode.ParamInvalid, "微信登录请求无效")
		return
	}
	state, err := wechatNonce()
	if err != nil {
		writeAuthError(c, err)
		return
	}
	if err := h.svc.rdb.Set(c.Request.Context(), "auth:wechat:state:"+state, wechatHash(req.BrowserVerifier), wechatTTL).Err(); err != nil {
		writeAuthError(c, err)
		return
	}
	query := url.Values{"appid": {h.svc.wechatConfig.AppID}, "redirect_uri": {h.svc.wechatConfig.CallbackURL}, "response_type": {"code"}, "scope": {"snsapi_login"}, "state": {state}}
	response.Success(c, gin.H{"authorize_url": "https://open.weixin.qq.com/connect/qrconnect?" + query.Encode() + "#wechat_redirect"})
}

type wechatIdentity struct {
	AppID   string `json:"app_id"`
	OpenID  string `json:"openid"`
	UnionID string `json:"unionid"`
}

// Compare and consume in one operation: an invalid browser cannot consume another login's state.
var consumeWeChatState = redis.NewScript(`
if redis.call('GET', KEYS[1]) == ARGV[1] then
 return redis.call('DEL', KEYS[1])
end
return 0`)

func (h *Handler) WeChatExchange(c *gin.Context) {
	if !h.requireWeChat(c) {
		return
	}
	var req struct {
		Code            string `json:"code"`
		State           string `json:"state"`
		BrowserVerifier string `json:"browser_verifier"`
	}
	if c.ShouldBindJSON(&req) != nil || req.Code == "" || len(req.Code) > 512 || !wechatNoncePattern.MatchString(req.State) || !wechatNoncePattern.MatchString(req.BrowserVerifier) {
		response.Error(c, errcode.Unauthorized, errWeChatExpired.Error())
		return
	}
	consumed, err := consumeWeChatState.Run(c.Request.Context(), h.svc.rdb, []string{"auth:wechat:state:" + req.State}, wechatHash(req.BrowserVerifier)).Int()
	if err != nil {
		writeAuthError(c, err)
		return
	}
	if consumed != 1 {
		response.Error(c, errcode.Unauthorized, errWeChatExpired.Error())
		return
	}
	identity, err := h.svc.exchangeWeChatCode(c.Request, req.Code)
	if err != nil {
		writeAuthError(c, err)
		return
	}
	user, err := h.svc.wechatRepo.FindByWeChat(identity.AppID, identity.OpenID)
	if errors.Is(err, sql.ErrNoRows) {
		ticket, err := wechatNonce()
		if err != nil {
			writeAuthError(c, err)
			return
		}
		encoded, err := json.Marshal(identity)
		if err != nil {
			writeAuthError(c, err)
			return
		}
		if err := h.svc.rdb.Set(c.Request.Context(), "auth:wechat:bind:"+wechatHash(ticket), encoded, wechatTTL).Err(); err != nil {
			writeAuthError(c, err)
			return
		}
		response.Success(c, gin.H{"binding_required": true, "binding_ticket": ticket})
		return
	}
	if err != nil {
		writeAuthError(c, err)
		return
	}
	if user.Status != "active" {
		writeAuthError(c, ErrUserFrozen)
		return
	}
	tokens, account, err := h.svc.issueTokens(user)
	if err != nil {
		writeAuthError(c, err)
		return
	}
	response.Success(c, gin.H{"access_token": tokens.AccessToken, "refresh_token": tokens.RefreshToken, "expires_in": tokens.ExpiresIn, "user": account})
}

func (s *Service) exchangeWeChatCode(request *http.Request, code string) (*wechatIdentity, error) {
	query := url.Values{"appid": {s.wechatConfig.AppID}, "secret": {s.wechatConfig.AppSecret}, "code": {code}, "grant_type": {"authorization_code"}}
	req, err := http.NewRequestWithContext(request.Context(), http.MethodGet, "https://api.weixin.qq.com/sns/oauth2/access_token?"+query.Encode(), nil)
	if err != nil {
		return nil, errors.New("wechat request could not be created")
	}
	result, err := s.wechatHTTP.Do(req)
	// URL errors contain AppSecret; do not log or return them.
	if err != nil {
		return nil, errors.New("wechat service unavailable")
	}
	defer result.Body.Close()
	if result.StatusCode != http.StatusOK {
		return nil, errors.New("wechat service unavailable")
	}
	var payload struct {
		AccessToken string `json:"access_token"`
		OpenID      string `json:"openid"`
		UnionID     string `json:"unionid"`
		Scope       string `json:"scope"`
		ErrCode     int    `json:"errcode"`
	}
	if json.NewDecoder(io.LimitReader(result.Body, 16384)).Decode(&payload) != nil {
		return nil, errors.New("invalid wechat response")
	}
	if payload.ErrCode != 0 {
		return nil, errWeChatExpired
	}
	if payload.AccessToken == "" || !wechatIdentityPattern.MatchString(payload.OpenID) || (payload.UnionID != "" && !wechatIdentityPattern.MatchString(payload.UnionID)) || !containsWeChatLoginScope(payload.Scope) {
		return nil, errors.New("incomplete wechat response")
	}
	return &wechatIdentity{AppID: s.wechatConfig.AppID, OpenID: payload.OpenID, UnionID: payload.UnionID}, nil
}

func containsWeChatLoginScope(scope string) bool {
	for _, item := range strings.Split(scope, ",") {
		if item == "snsapi_login" {
			return true
		}
	}
	return false
}

func (h *Handler) WeChatBind(c *gin.Context) {
	if !h.requireWeChat(c) {
		return
	}
	var req struct {
		BindingTicket string `json:"binding_ticket"`
	}
	if c.ShouldBindJSON(&req) != nil || !wechatNoncePattern.MatchString(req.BindingTicket) {
		response.Error(c, errcode.Unauthorized, errWeChatExpired.Error())
		return
	}
	user, err := h.svc.repo.FindByID(c.GetInt64("user_id"))
	if err != nil {
		writeAuthError(c, err)
		return
	}
	if user.Status != "active" {
		writeAuthError(c, ErrUserFrozen)
		return
	}
	encoded, err := h.svc.rdb.GetDel(c.Request.Context(), "auth:wechat:bind:"+wechatHash(req.BindingTicket)).Bytes()
	if errors.Is(err, redis.Nil) {
		writeAuthError(c, errWeChatExpired)
		return
	}
	if err != nil {
		writeAuthError(c, err)
		return
	}
	var identity wechatIdentity
	if json.Unmarshal(encoded, &identity) != nil || identity.AppID != h.svc.wechatConfig.AppID {
		writeAuthError(c, errWeChatExpired)
		return
	}
	if err := h.svc.wechatRepo.BindWeChat(user.ID, identity); err != nil {
		writeAuthError(c, err)
		return
	}
	response.Success(c, nil)
}
