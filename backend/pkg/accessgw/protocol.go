// Package accessgw 访问网关(cmd/accessgw)与主服务内部接口之间的协议: HMAC 签名、报文结构与客户端
// (docs/25 §25.6.2)。主服务侧的接口实现在 internal/fleet, 网关侧在 cmd/accessgw, 两边只通过本包约定。
package accessgw

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

const (
	HeaderTimestamp = "X-Gw-Timestamp"
	HeaderNonce     = "X-Gw-Nonce"
	HeaderSignature = "X-Gw-Signature"

	// MaxSkew 网关与主服务时钟允许的最大偏差, 超出即拒绝。
	MaxSkew = 60 * time.Second
	// NonceTTL nonce 去重窗口(Redis SET NX EX), 必须大于 2*MaxSkew。
	NonceTTL = 300 * time.Second

	// 内部接口路径(挂在主服务 accessgw.internal_listen 上, 前缀与公网 API 一致, 但不注册到公网路由)。
	PathPrefix      = "/api/v1/internal/accessgw"
	PathAuth        = PathPrefix + "/auth"
	PathRevocations = PathPrefix + "/revocations"
	PathSessions    = PathPrefix + "/sessions"
	PathProbeResult = PathPrefix + "/probe-result"

	MethodPublicKey = "publickey"
	MethodPassword  = "password"

	// MaxCacheTTL 网关只缓存"允许"结果, 且不超过 30s(docs/25 §25.6.2)。
	MaxCacheTTL = 30 * time.Second
)

var (
	ErrBadTimestamp = errors.New("时间戳缺失或超出允许偏差")
	ErrBadNonce     = errors.New("nonce 缺失或格式非法")
	ErrBadSignature = errors.New("签名不匹配")
)

// Sign 签名串 = HMAC-SHA256(secret, method\nuri\nts\nnonce\nhex(sha256(body))), 输出小写 hex。
// uri 为请求行中的路径(含 ?query, 即 http.Request.URL.RequestURI()), 查询参数因此也受签名保护。
func Sign(secret []byte, method, uri string, ts int64, nonce string, body []byte) string {
	sum := sha256.Sum256(body)
	m := hmac.New(sha256.New, secret)
	fmt.Fprintf(m, "%s\n%s\n%d\n%s\n%s", method, uri, ts, nonce, hex.EncodeToString(sum[:]))
	return hex.EncodeToString(m.Sum(nil))
}

// NewNonce 16 字节随机数, hex 编码(32 字符)。
func NewNonce() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// ValidNonce nonce 必须是 32 位小写 hex。
func ValidNonce(n string) bool {
	if len(n) != 32 {
		return false
	}
	for _, c := range n {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

// Verify 校验时间窗与签名(不含 nonce 去重, 去重由调用方用 Redis 完成)。
func Verify(secret []byte, method, uri, ts, nonce, sig string, body []byte, now time.Time) error {
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrBadTimestamp
	}
	d := now.Sub(time.Unix(t, 0))
	if d > MaxSkew || d < -MaxSkew {
		return ErrBadTimestamp
	}
	if !ValidNonce(nonce) {
		return ErrBadNonce
	}
	want := Sign(secret, method, uri, t, nonce, body)
	if !hmac.Equal([]byte(want), []byte(sig)) {
		return ErrBadSignature
	}
	return nil
}

// AuthRequest POST /internal/accessgw/auth。password 只在 method=password 时出现, 网关不保存。
type AuthRequest struct {
	Login    string `json:"login"`
	Method   string `json:"method"`
	PubkeyFP string `json:"pubkey_fp,omitempty"`
	Password string `json:"password,omitempty"`
	RemoteIP string `json:"remote_ip"`
}

// AuthResponse 鉴权结果。拒绝时只有 allow=false(不区分登录名是否存在)。
type AuthResponse struct {
	Allow             bool       `json:"allow"`
	UpstreamHost      string     `json:"upstream_host,omitempty"`
	UpstreamPort      int        `json:"upstream_port,omitempty"`
	UpstreamHostKeyFP string     `json:"upstream_host_key_fp,omitempty"`
	AccessUntil       *time.Time `json:"access_until,omitempty"`
	CacheTTLS         int        `json:"cache_ttl_s"`
	// System 系统自检身份的放行(开通自检): 网关不缓存、不上报会话审计。
	System bool `json:"system,omitempty"`
	// Probe 探活 P11 的端口连通任务: 网关从本机(出口 IP 即节点放行的 IP)对 host:port 做一次 TCP 连接,
	// 结果经 probe-result 回报, 然后拒绝本次 SSH 登录。
	Probe *ProbeTask `json:"probe,omitempty"`
}

type ProbeTask struct {
	Token string `json:"token"`
	Host  string `json:"host"`
	Port  int    `json:"port"`
}

// ProbeResult POST /internal/accessgw/probe-result。
type ProbeResult struct {
	Token     string `json:"token"`
	Reachable bool   `json:"reachable"`
	Error     string `json:"error,omitempty"`
}

// RevocationsResponse GET /internal/accessgw/revocations?cursor=。
// logins 为游标之后访问条件发生变化的登录名(授予或撤销都会出现, 网关一律清缓存); cursor 为下一次请求用的游标。
type RevocationsResponse struct {
	Logins []string `json:"logins"`
	Cursor string   `json:"cursor"`
}

// SessionEvent 会话事件(开始或结束), 以 gw_session_id 幂等; 结束事件同样携带开始信息, 单独到达也能落库。
type SessionEvent struct {
	GWSessionID    string     `json:"gw_session_id"`
	Login          string     `json:"login"`
	RemoteIP       string     `json:"remote_ip"`
	AuthMethod     string     `json:"auth_method"`
	KeyFingerprint string     `json:"key_fingerprint,omitempty"`
	StartedAt      time.Time  `json:"started_at"`
	EndedAt        *time.Time `json:"ended_at,omitempty"`
	BytesUp        int64      `json:"bytes_up"`
	BytesDown      int64      `json:"bytes_down"`
	EndReason      string     `json:"end_reason,omitempty"`
}

type SessionsRequest struct {
	Events []SessionEvent `json:"events"`
}

type SessionsResponse struct {
	Accepted int `json:"accepted"`
	Ignored  int `json:"ignored"`
}

// MaxSessionEvents 单次上报上限。
const MaxSessionEvents = 500
