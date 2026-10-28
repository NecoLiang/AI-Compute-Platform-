package fleet

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"tokenfactory/pkg/accessgw"
	"tokenfactory/pkg/errcode"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// 访问网关内部接口集成测试(T-085): MySQL(TEST_MYSQL_DSN) + Redis(TEST_REDIS_ADDR), 任一缺失即跳过。

var gwTestSecret = strings.Repeat("g", 32)

func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("set TEST_REDIS_ADDR to run the access gateway integration tests")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	ctx := context.Background()
	require.NoError(t, rdb.Ping(ctx).Err())
	clean := func() {
		keys, err := rdb.Keys(ctx, "accessgw:*").Result()
		require.NoError(t, err)
		if len(keys) > 0 {
			require.NoError(t, rdb.Del(ctx, keys...).Err())
		}
	}
	clean()
	t.Cleanup(func() { clean(); _ = rdb.Close() })
	return rdb
}

func newSigner(t *testing.T) ssh.Signer {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	s, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	return s
}

func fpOf(s ssh.Signer) string { return ssh.FingerprintSHA256(s.PublicKey()) }

type gwEnv struct {
	*provEnv
	rdb    *redis.Client
	api    *GatewayAPI
	srv    *httptest.Server
	client *accessgw.Client
	sys    ssh.Signer
}

// newGWEnv activate=true 时跑完开通任务(网关自检用桩), 租约进入 active。
func newGWEnv(t *testing.T, activate bool) *gwEnv {
	t.Helper()
	rdb := testRedis(t)
	e := &gwEnv{provEnv: newProvEnv(t, true), rdb: rdb, sys: newSigner(t)}
	if activate {
		e.outcome(t, runOne(t, e.s))
	}
	var err error
	e.api, err = NewGatewayAPI(e.s, rdb, gwTestSecret, fpOf(e.sys))
	require.NoError(t, err)
	e.srv = httptest.NewServer(e.api.Router())
	t.Cleanup(e.srv.Close)
	e.client, err = accessgw.NewClient(e.srv.URL, []byte(gwTestSecret), 5*time.Second)
	require.NoError(t, err)
	return e
}

func (e *gwEnv) addBuyerKey(t *testing.T, userID int64, s ssh.Signer) {
	t.Helper()
	e.db.MustExec(`INSERT INTO buyer_ssh_keys (user_id, name, public_key, fingerprint) VALUES (?,?,?,?)`,
		userID, "k", strings.TrimSpace(string(ssh.MarshalAuthorizedKey(s.PublicKey()))), fpOf(s))
}

func pkReq(login, ip string, s ssh.Signer) accessgw.AuthRequest {
	return accessgw.AuthRequest{Login: login, Method: accessgw.MethodPublicKey, PubkeyFP: fpOf(s), RemoteIP: ip}
}

func pwReq(login, ip, pw string) accessgw.AuthRequest {
	return accessgw.AuthRequest{Login: login, Method: accessgw.MethodPassword, Password: pw, RemoteIP: ip}
}

func TestGatewayAPIAuthenticate(t *testing.T) {
	e := newGWEnv(t, true)
	ctx := context.Background()
	_, hostFP := fakeContainerHostKey(t)
	login := e.lease.LeaseNo
	buyer, other, stranger := newSigner(t), newSigner(t), newSigner(t)
	e.addBuyerKey(t, 21, buyer)
	e.db.MustExec(`INSERT INTO users (id, phone, password_hash) VALUES (99,'13900000099','x')`)
	e.addBuyerKey(t, 99, other)

	// 买家公钥: 经 HTTP 签名链路放行, 路由与租约一致。
	r, err := e.client.Auth(ctx, pkReq(login, "198.51.100.1", buyer))
	require.NoError(t, err)
	require.True(t, r.Allow)
	assert.Equal(t, "127.0.0.1", r.UpstreamHost)
	assert.Equal(t, provPortStart, r.UpstreamPort)
	assert.Equal(t, hostFP, r.UpstreamHostKeyFP)
	assert.False(t, r.System)
	assert.Equal(t, 30, r.CacheTTLS)
	require.NotNil(t, r.AccessUntil)
	assert.True(t, r.AccessUntil.After(time.Now()))

	// 他人的公钥 / 未登记的公钥 / 不存在的租约号 / 非法租约号: 一律 allow=false。
	for _, req := range []accessgw.AuthRequest{pkReq(login, "198.51.100.1", other), pkReq(login, "198.51.100.1", stranger),
		pkReq("l-zzzzzzzz", "198.51.100.1", buyer), pkReq("root", "198.51.100.1", buyer)} {
		r, err := e.api.Authenticate(ctx, req)
		require.NoError(t, err)
		assert.False(t, r.Allow, "%+v", req)
	}

	// 访问凭证(密码)。
	pw, err := e.s.products.ManagedAccessValue(e.db, e.order.ID)
	require.NoError(t, err)
	require.NotEmpty(t, pw)
	r, err = e.api.Authenticate(ctx, pwReq(login, "198.51.100.2", pw))
	require.NoError(t, err)
	assert.True(t, r.Allow)
	r, err = e.api.Authenticate(ctx, pwReq(login, "198.51.100.2", pw+"x"))
	require.NoError(t, err)
	assert.False(t, r.Allow)

	// 吊销的公钥拒绝。
	e.db.MustExec(`UPDATE buyer_ssh_keys SET revoked_at=NOW() WHERE fingerprint=?`, fpOf(buyer))
	r, err = e.api.Authenticate(ctx, pkReq(login, "198.51.100.1", buyer))
	require.NoError(t, err)
	assert.False(t, r.Allow)
	e.db.MustExec(`UPDATE buyer_ssh_keys SET revoked_at=NULL WHERE fingerprint=?`, fpOf(buyer))

	// access_until 已过 / 非 active 状态: 拒绝(即使凭证正确)。
	e.db.MustExec(`UPDATE leases SET access_until=NOW() - INTERVAL 1 SECOND WHERE id=?`, e.lease.ID)
	r, err = e.api.Authenticate(ctx, pkReq(login, "198.51.100.1", buyer))
	require.NoError(t, err)
	assert.False(t, r.Allow)
	e.db.MustExec(`UPDATE leases SET access_until=NOW() + INTERVAL 10 SECOND WHERE id=?`, e.lease.ID)
	r, err = e.api.Authenticate(ctx, pkReq(login, "198.51.100.1", buyer))
	require.NoError(t, err)
	require.True(t, r.Allow)
	assert.LessOrEqual(t, r.CacheTTLS, 10, "cache TTL must not outlive access_until")
	e.db.MustExec(`UPDATE leases SET access_until=NOW() + INTERVAL 1 HOUR, state='suspended' WHERE id=?`, e.lease.ID)
	r, err = e.api.Authenticate(ctx, pkReq(login, "198.51.100.1", buyer))
	require.NoError(t, err)
	assert.False(t, r.Allow)
	e.db.MustExec(`UPDATE leases SET state='active' WHERE id=?`, e.lease.ID)

	// 参数非法 → 40001(经 HTTP 也一样)。
	_, err = e.api.Authenticate(ctx, accessgw.AuthRequest{Login: login, Method: "keyboard-interactive", RemoteIP: "1.1.1.1"})
	assert.Equal(t, errcode.ParamInvalid, codeOf(err))
	_, err = e.api.Authenticate(ctx, accessgw.AuthRequest{Login: login, Method: accessgw.MethodPublicKey, PubkeyFP: "md5:x", RemoteIP: "1.1.1.1"})
	assert.Equal(t, errcode.ParamInvalid, codeOf(err))
	_, err = e.client.Auth(ctx, pkReq(login, "not-an-ip", buyer))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "40001")

	// 公钥失败不计数: 30 次未登记公钥后同 IP 仍可登录, 且没有计数键。
	for range 30 {
		r, err := e.api.Authenticate(ctx, pkReq(login, "198.51.100.3", stranger))
		require.NoError(t, err)
		require.False(t, r.Allow)
	}
	n, err := e.rdb.Exists(ctx, ipFailKey("198.51.100.3")).Result()
	require.NoError(t, err)
	assert.Zero(t, n)
	r, err = e.api.Authenticate(ctx, pkReq(login, "198.51.100.3", buyer))
	require.NoError(t, err)
	assert.True(t, r.Allow)

	// 登录名+IP: 10 次错误密码后锁 15 分钟, 正确密码也拒绝; 公钥不受影响; 其他 IP 不受影响。
	const ipA = "203.0.113.10"
	for range gwLoginFailLimit {
		r, err := e.api.Authenticate(ctx, pwReq(login, ipA, "wrong"))
		require.NoError(t, err)
		require.False(t, r.Allow)
	}
	r, err = e.api.Authenticate(ctx, pwReq(login, ipA, pw))
	require.NoError(t, err)
	assert.False(t, r.Allow, "login+IP must be locked after 10 failures")
	ttl, err := e.rdb.PTTL(ctx, loginFailKey(login, ipA)).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, 14*time.Minute)
	r, err = e.api.Authenticate(ctx, pkReq(login, ipA, buyer))
	require.NoError(t, err)
	assert.True(t, r.Allow, "login lock only applies to password")
	r, err = e.api.Authenticate(ctx, pwReq(login, "203.0.113.11", pw))
	require.NoError(t, err)
	assert.True(t, r.Allow)

	// IP: 20 次密码失败(跨登录名)后封 30 分钟, 所有认证方式都拒绝。
	const ipB = "2001:db8::7"
	for i := range gwIPFailLimit {
		r, err := e.api.Authenticate(ctx, pwReq(fmt.Sprintf("guess%d", i), ipB, "x"))
		require.NoError(t, err)
		require.False(t, r.Allow)
	}
	ttl, err = e.rdb.PTTL(ctx, ipFailKey(ipB)).Result()
	require.NoError(t, err)
	assert.Greater(t, ttl, 29*time.Minute)
	for _, req := range []accessgw.AuthRequest{pkReq(login, ipB, buyer), pwReq(login, ipB, pw)} {
		r, err := e.api.Authenticate(ctx, req)
		require.NoError(t, err)
		assert.False(t, r.Allow, "blocked IP must be denied for %s", req.Method)
	}
	// IP 规范化: 同一 IPv6 的不同写法共用计数键。
	r, err = e.api.Authenticate(ctx, pkReq(login, "2001:0db8:0:0:0:0:0:7", buyer))
	require.NoError(t, err)
	assert.False(t, r.Allow)

	// 系统自检身份不受封禁影响(但对 active 租约无权登录)。
	r, err = e.api.Authenticate(ctx, pkReq(login, ipB, e.sys))
	require.NoError(t, err)
	assert.False(t, r.Allow)
}

func TestGatewayAPISystemIdentityAndProbe(t *testing.T) {
	e := newGWEnv(t, false)
	ctx := context.Background()
	login := e.lease.LeaseNo
	buyer := newSigner(t)
	e.addBuyerKey(t, 21, buyer)

	// pending(尚未开始开通) / provisioning 但未分配端口与指纹: 系统身份也拒绝。
	r, err := e.api.Authenticate(ctx, pkReq(login, "10.0.0.1", e.sys))
	require.NoError(t, err)
	assert.False(t, r.Allow)
	e.db.MustExec(`UPDATE leases SET state='provisioning' WHERE id=?`, e.lease.ID)
	r, err = e.api.Authenticate(ctx, pkReq(login, "10.0.0.1", e.sys))
	require.NoError(t, err)
	assert.False(t, r.Allow)

	e.db.MustExec(`UPDATE leases SET upstream_port=47299, upstream_host_key_fp='SHA256:abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG' WHERE id=?`, e.lease.ID)
	r, err = e.client.Auth(ctx, pkReq(login, "10.0.0.1", e.sys))
	require.NoError(t, err)
	assert.True(t, r.Allow)
	assert.True(t, r.System)
	assert.Zero(t, r.CacheTTLS)
	assert.Equal(t, 47299, r.UpstreamPort)
	// 买家公钥不能登录 provisioning 租约。
	r, err = e.api.Authenticate(ctx, pkReq(login, "10.0.0.1", buyer))
	require.NoError(t, err)
	assert.False(t, r.Allow)

	// P11 探活: 只有系统身份 + 已登记的 token 才下发任务, 且 allow 恒为 false。
	token, err := accessgw.NewNonce()
	require.NoError(t, err)
	task, _ := json.Marshal(accessgw.ProbeTask{Host: "10.9.0.1", Port: 30001})
	require.NoError(t, e.rdb.Set(ctx, probeKey(token), task, time.Minute).Err())
	r, err = e.client.Auth(ctx, pkReq("p-"+token, "10.0.0.1", e.sys))
	require.NoError(t, err)
	assert.False(t, r.Allow)
	require.NotNil(t, r.Probe)
	assert.Equal(t, accessgw.ProbeTask{Token: token, Host: "10.9.0.1", Port: 30001}, *r.Probe)
	r, err = e.api.Authenticate(ctx, pkReq("p-"+token, "10.0.0.1", buyer))
	require.NoError(t, err)
	assert.Nil(t, r.Probe, "probe tasks are only handed to the system identity")
	other, _ := accessgw.NewNonce()
	r, err = e.api.Authenticate(ctx, pkReq("p-"+other, "10.0.0.1", e.sys))
	require.NoError(t, err)
	assert.False(t, r.Allow)
	assert.Nil(t, r.Probe)

	// 回报: 合法 token 写入结果; 未登记 → 40400; 非法 → 40001。
	require.NoError(t, e.client.ReportProbe(ctx, accessgw.ProbeResult{Token: token, Error: "connection refused"}))
	raw, err := e.rdb.Get(ctx, probeResultKey(token)).Bytes()
	require.NoError(t, err)
	var got accessgw.ProbeResult
	require.NoError(t, json.Unmarshal(raw, &got))
	assert.Equal(t, accessgw.ProbeResult{Token: token, Error: "connection refused"}, got)
	assert.Equal(t, errcode.NotFound, codeOf(e.api.ProbeResult(ctx, accessgw.ProbeResult{Token: other})))
	assert.Equal(t, errcode.ParamInvalid, codeOf(e.api.ProbeResult(ctx, accessgw.ProbeResult{Token: "../x"})))

	// 开通完成后系统身份不能再登录(active 租约只给买家)。
	e.db.MustExec(`UPDATE leases SET state='pending', upstream_port=NULL, upstream_host_key_fp=NULL WHERE id=?`, e.lease.ID)
	e.outcome(t, runOne(t, e.s))
	r, err = e.api.Authenticate(ctx, pkReq(login, "10.0.0.1", e.sys))
	require.NoError(t, err)
	assert.False(t, r.Allow)
}

func TestGatewayAPIRevocations(t *testing.T) {
	e := newGWEnv(t, true)
	ctx := context.Background()
	login := e.lease.LeaseNo

	head, err := e.client.Revocations(ctx, "")
	require.NoError(t, err)
	assert.Empty(t, head.Logins)
	hts, hid, ok := parseCursor(head.Cursor)
	require.True(t, ok)
	assert.Zero(t, hid)
	assert.InDelta(t, time.Now().Add(-2*time.Second).Unix(), hts, 2)

	// 固定一个过去的时间点, 验证 (ts, id) 严格递增与同秒 id 决胜。
	e.db.MustExec(`UPDATE leases SET access_changed_at=NOW() - INTERVAL 30 SECOND WHERE id=?`, e.lease.ID)
	var ts int64
	require.NoError(t, e.db.Get(&ts, `SELECT UNIX_TIMESTAMP(access_changed_at) FROM leases WHERE id=?`, e.lease.ID))
	r, err := e.client.Revocations(ctx, fmt.Sprintf("%d-0", ts-1))
	require.NoError(t, err)
	assert.Equal(t, []string{login}, r.Logins)
	assert.Equal(t, fmt.Sprintf("%d-%d", ts, e.lease.ID), r.Cursor)
	r, err = e.api.Revocations(fmt.Sprintf("%d-%d", ts, e.lease.ID-1))
	require.NoError(t, err)
	assert.Equal(t, []string{login}, r.Logins, "same second, larger id must be returned")
	r, err = e.api.Revocations(r.Cursor)
	require.NoError(t, err)
	assert.Empty(t, r.Logins)
	assert.Equal(t, fmt.Sprintf("%d-%d", ts, e.lease.ID), r.Cursor, "cursor must not move without rows")

	// 2 秒滞后: 刚变化的行暂不返回。
	require.NoError(t, e.s.repo.touchBuyerLeases(e.db, 21))
	r, err = e.api.Revocations(fmt.Sprintf("%d-%d", ts, e.lease.ID))
	require.NoError(t, err)
	assert.Empty(t, r.Logins)

	_, err = e.api.Revocations("abc")
	assert.Equal(t, errcode.ParamInvalid, codeOf(err))
	_, err = e.client.Revocations(ctx, "-1-2")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "40001")
}

func TestGatewayAPISessions(t *testing.T) {
	e := newGWEnv(t, true)
	ctx := context.Background()
	login := e.lease.LeaseNo
	t0 := time.Date(2026, 10, 28, 10, 0, 0, 0, time.Local)
	t1 := t0.Add(5 * time.Minute)
	start := accessgw.SessionEvent{GWSessionID: "s1", Login: login, RemoteIP: "2001:0db8::1", AuthMethod: accessgw.MethodPublicKey,
		KeyFingerprint: "SHA256:abcdefghijklmnopqrstuvwxyz0123456789ABCDEFG", StartedAt: t0}
	end := start
	end.EndedAt, end.EndReason, end.BytesUp = &t1, "closed", 10
	again := end
	t2 := t1.Add(time.Hour)
	again.EndedAt, again.EndReason, again.BytesUp = &t2, "other", 5

	bad := []accessgw.SessionEvent{
		{GWSessionID: "u1", Login: "l-zzzzzzzz", RemoteIP: "1.1.1.1", AuthMethod: "password", StartedAt: t0}, // 未知租约
		{GWSessionID: "u2", Login: login, RemoteIP: "1.1.1.1", AuthMethod: "hostbased", StartedAt: t0},
		{GWSessionID: "u3", Login: login, RemoteIP: "nope", AuthMethod: "password", StartedAt: t0},
		{GWSessionID: "u4", Login: login, RemoteIP: "1.1.1.1", AuthMethod: "password", StartedAt: t1, EndedAt: &t0},
		{GWSessionID: "u5", Login: login, RemoteIP: "1.1.1.1", AuthMethod: "publickey", KeyFingerprint: fpOf(e.sys), StartedAt: t0},
		{GWSessionID: "", Login: login, RemoteIP: "1.1.1.1", AuthMethod: "password", StartedAt: t0},
	}
	r, err := e.client.ReportSessions(ctx, append([]accessgw.SessionEvent{start}, bad...))
	require.NoError(t, err)
	assert.Equal(t, accessgw.SessionsResponse{Accepted: 1, Ignored: len(bad)}, *r)
	r, err = e.client.ReportSessions(ctx, []accessgw.SessionEvent{end, again, start})
	require.NoError(t, err)
	assert.Equal(t, 3, r.Accepted)

	var row struct {
		LeaseID   int64      `db:"lease_id"`
		IP        string     `db:"remote_ip"`
		Method    string     `db:"auth_method"`
		FP        *string    `db:"key_fingerprint"`
		Started   time.Time  `db:"started_at"`
		Ended     *time.Time `db:"ended_at"`
		Up        int64      `db:"bytes_up"`
		EndReason *string    `db:"end_reason"`
	}
	require.NoError(t, e.db.Get(&row, `SELECT lease_id, remote_ip, auth_method, key_fingerprint, started_at, ended_at, bytes_up, end_reason
		FROM access_sessions WHERE gw_session_id='s1'`))
	assert.Equal(t, e.lease.ID, row.LeaseID)
	assert.Equal(t, "2001:db8::1", row.IP)
	assert.Equal(t, "publickey", row.Method)
	require.NotNil(t, row.FP)
	assert.True(t, row.Started.Equal(t0))
	require.NotNil(t, row.Ended)
	assert.True(t, row.Ended.Equal(t1), "end is written once")
	require.NotNil(t, row.EndReason)
	assert.Equal(t, "closed", *row.EndReason)
	assert.Equal(t, int64(10), row.Up)
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM access_sessions`))

	_, err = e.api.RecordSessions(make([]accessgw.SessionEvent, accessgw.MaxSessionEvents+1))
	assert.Equal(t, errcode.ParamInvalid, codeOf(err))

	// 保留 180 天。
	e.db.MustExec(`INSERT INTO access_sessions (lease_id, gw_session_id, remote_ip, auth_method, started_at)
		VALUES (?, 'old', '1.1.1.1', 'password', NOW() - INTERVAL 181 DAY)`, e.lease.ID)
	n, err := e.s.repo.PurgeOldSessions(time.Now().Add(-sessionRetention))
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM access_sessions WHERE gw_session_id='s1'`))
}

// 验签: 错误密钥 / 重放 / 时钟偏差 / 篡改查询串 全部 40100; 未知路径 40400。
func TestGatewayAPISignature(t *testing.T) {
	e := newGWEnv(t, false)
	ctx := context.Background()

	bad, err := accessgw.NewClient(e.srv.URL, []byte(strings.Repeat("x", 32)), 5*time.Second)
	require.NoError(t, err)
	_, err = bad.Revocations(ctx, "")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "40100")

	send := func(method, uri string, ts int64, nonce, signedURI string, body []byte) int {
		req, err := http.NewRequest(method, e.srv.URL+uri, bytes.NewReader(body))
		require.NoError(t, err)
		req.Header.Set(accessgw.HeaderTimestamp, strconv.FormatInt(ts, 10))
		req.Header.Set(accessgw.HeaderNonce, nonce)
		req.Header.Set(accessgw.HeaderSignature, accessgw.Sign([]byte(gwTestSecret), method, signedURI, ts, nonce, body))
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		defer resp.Body.Close()
		raw, _ := io.ReadAll(resp.Body)
		var env struct {
			Code int `json:"code"`
		}
		require.NoError(t, json.Unmarshal(raw, &env), string(raw))
		return env.Code
	}
	now := time.Now().Unix()
	n1, _ := accessgw.NewNonce()
	uri := accessgw.PathRevocations + "?cursor=1-0"
	assert.Equal(t, 0, send("GET", uri, now, n1, uri, nil))
	assert.Equal(t, errcode.Unauthorized, send("GET", uri, now, n1, uri, nil), "replayed nonce")
	n2, _ := accessgw.NewNonce()
	assert.Equal(t, errcode.Unauthorized, send("GET", uri, now-120, n2, uri, nil), "clock skew")
	n3, _ := accessgw.NewNonce()
	assert.Equal(t, errcode.Unauthorized, send("GET", accessgw.PathRevocations+"?cursor=2-0", now, n3, uri, nil), "query tampering")
	n4, _ := accessgw.NewNonce()
	assert.Equal(t, errcode.NotFound, send("GET", accessgw.PathPrefix+"/nope", now, n4, accessgw.PathPrefix+"/nope", nil))
	n5, _ := accessgw.NewNonce()
	assert.Equal(t, errcode.Unauthorized, send("GET", "/api/v1/health", now, n5, "/api/v1/health/x", nil))
}
