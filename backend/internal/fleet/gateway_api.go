package fleet

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"tokenfactory/pkg/accessgw"
	"tokenfactory/pkg/errcode"

	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
)

// 访问网关内部接口的主服务侧实现(docs/25 §25.6.2, T-085)。只挂在 accessgw.internal_listen 上(见 gateway_http.go),
// 不注册到公网路由组。网关不持有业务数据: 每次鉴权都在这里按租约/公钥/访问凭证判定。

const (
	gwFailWindow     = 10 * time.Minute
	gwIPFailLimit    = 20
	gwIPBlock        = 30 * time.Minute
	gwLoginFailLimit = 10
	gwLoginLock      = 15 * time.Minute

	gwProbeTTL = 2 * time.Minute
	// gwCursorLag 撤销游标只推进到"2 秒前"为止: access_changed_at 为秒精度, 当前秒内(及未提交事务)的变化
	// 留到下一次轮询, 避免同秒内较小 id 的行被跳过。
	gwCursorLag       = 2
	gwRevocationBatch = 1000
	sessionRetention  = 180 * 24 * time.Hour
)

// GatewayAPI 内部接口。rdb 用于 nonce 去重、爆破计数与 P11 探活任务; 任何 Redis 错误都按失败处理(fail-closed)。
type GatewayAPI struct {
	s          *Service
	rdb        *redis.Client
	secret     []byte
	selftestFP string
	now        func() time.Time
}

// NewGatewayAPI selftestFP 为 fleet.selftest_key_file 公钥的 SHA256 指纹(系统自检身份)。
func NewGatewayAPI(s *Service, rdb *redis.Client, hmacSecret, selftestFP string) (*GatewayAPI, error) {
	if rdb == nil {
		return nil, errors.New("访问网关内部接口需要 Redis(nonce 去重与爆破计数)")
	}
	if len(hmacSecret) < 32 {
		return nil, errors.New("accessgw.hmac_secret 至少 32 字节")
	}
	if !strings.HasPrefix(selftestFP, "SHA256:") {
		return nil, errors.New("系统自检身份指纹缺失")
	}
	return &GatewayAPI{s: s, rdb: rdb, secret: []byte(hmacSecret), selftestFP: selftestFP, now: time.Now}, nil
}

func ipFailKey(ip string) string           { return "accessgw:fail:ip:" + ip }
func loginFailKey(login, ip string) string { return "accessgw:fail:login:" + login + ":" + ip }
func probeKey(token string) string         { return "accessgw:probe:" + token }
func probeResultKey(token string) string   { return "accessgw:probe:" + token + ":result" }
func nonceKey(nonce string) string         { return "accessgw:nonce:" + nonce }

// failIncr 固定窗口计数: 首次失败开 10 分钟窗口; 达到上限时把同一个键的有效期改为封禁时长,
// 封禁期间值 ≥ 上限即视为封禁(不再递增, 等待自然过期)。只用设计文档约定的两个键。
var failIncr = redis.NewScript(`
local n = redis.call('INCR', KEYS[1])
if n == 1 then redis.call('PEXPIRE', KEYS[1], ARGV[1]) end
if n == tonumber(ARGV[2]) then redis.call('PEXPIRE', KEYS[1], ARGV[3]) end
return n`)

func (g *GatewayAPI) blocked(ctx context.Context, key string, limit int) (bool, error) {
	n, err := g.rdb.Get(ctx, key).Int()
	if errors.Is(err, redis.Nil) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return n >= limit, nil
}

func (g *GatewayAPI) recordFail(ctx context.Context, key string, limit int, block time.Duration) error {
	return failIncr.Run(ctx, g.rdb, []string{key}, gwFailWindow.Milliseconds(), limit, block.Milliseconds()).Err()
}

func validFingerprint(fp string) bool {
	return strings.HasPrefix(fp, "SHA256:") && len(fp) >= 20 && len(fp) <= 80
}

// probeLoginToken P11 的临时登录名 p-<32 hex>。
func probeLoginToken(login string) (string, bool) {
	t, ok := strings.CutPrefix(login, "p-")
	return t, ok && accessgw.ValidNonce(t)
}

// gwLeaseRoute 鉴权所需的租约与节点地址。
type gwLeaseRoute struct {
	ID           int64      `db:"id"`
	OrderID      int64      `db:"order_id"`
	BuyerID      int64      `db:"buyer_id"`
	State        string     `db:"state"`
	DeliveryKind string     `db:"delivery_kind"`
	AccessUntil  *time.Time `db:"access_until"`
	Port         *int       `db:"upstream_port"`
	HostKeyFP    *string    `db:"upstream_host_key_fp"`
	Host         *string    `db:"ssh_host"`
}

func (r *gwLeaseRoute) routable() bool {
	return r.Port != nil && r.HostKeyFP != nil && *r.HostKeyFP != "" && r.Host != nil && *r.Host != ""
}

func (g *GatewayAPI) leaseRoute(login string) (*gwLeaseRoute, error) {
	var r gwLeaseRoute
	err := g.s.repo.db.Get(&r, `SELECT l.id, l.order_id, l.buyer_id, l.state, l.delivery_kind, l.access_until, l.upstream_port,
		l.upstream_host_key_fp, n.ssh_host FROM leases l JOIN supplier_nodes n ON n.id=l.node_id WHERE l.lease_no=?`, login)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

var deny = &accessgw.AuthResponse{Allow: false}

// Authenticate POST /internal/accessgw/auth。拒绝统一返回 allow=false, 不区分登录名是否存在、租约是否到期。
//
// 爆破计数(S2 实现口径): 只有密码失败计数(按 IP 与 登录名+IP 两个键); 公钥失败不计数——SSH 客户端会逐把尝试
// agent 中的公钥, 每把不匹配都是一次失败, 计入会把正常用户的 IP 封掉, 而公钥本身不可被暴力猜测。
// IP 一旦被封, 该 IP 的所有认证方式都拒绝; 登录名+IP 锁只拒绝密码方式。系统自检身份不受计数与封禁影响。
func (g *GatewayAPI) Authenticate(ctx context.Context, req accessgw.AuthRequest) (*accessgw.AuthResponse, error) {
	ip := net.ParseIP(strings.TrimSpace(req.RemoteIP))
	if ip == nil {
		return nil, errcode.New(errcode.ParamInvalid, "remote_ip 非法")
	}
	ipStr := ip.String()
	switch req.Method {
	case accessgw.MethodPublicKey:
		if !validFingerprint(req.PubkeyFP) {
			return nil, errcode.New(errcode.ParamInvalid, "pubkey_fp 非法")
		}
		if subtle.ConstantTimeCompare([]byte(req.PubkeyFP), []byte(g.selftestFP)) == 1 {
			return g.systemAuth(ctx, req.Login)
		}
	case accessgw.MethodPassword:
	default:
		return nil, errcode.New(errcode.ParamInvalid, "method 只能是 publickey 或 password")
	}
	if len(req.Login) > 64 {
		return deny, nil
	}
	if b, err := g.blocked(ctx, ipFailKey(ipStr), gwIPFailLimit); err != nil || b {
		return deny, err
	}
	pw := req.Method == accessgw.MethodPassword
	if pw && ValidLeaseNo(req.Login) {
		if b, err := g.blocked(ctx, loginFailKey(req.Login, ipStr), gwLoginFailLimit); err != nil || b {
			return deny, err
		}
	}
	resp, err := g.check(req)
	if err != nil {
		return nil, err
	}
	if resp.Allow || !pw {
		return resp, nil
	}
	if err := g.recordFail(ctx, ipFailKey(ipStr), gwIPFailLimit, gwIPBlock); err != nil {
		return nil, err
	}
	if ValidLeaseNo(req.Login) {
		if err := g.recordFail(ctx, loginFailKey(req.Login, ipStr), gwLoginFailLimit, gwLoginLock); err != nil {
			return nil, err
		}
	}
	return deny, nil
}

// check 买家登录: 租约 active、now < access_until、路由齐全; 公钥属于租约买家且未吊销, 或密码等于访问凭证。
func (g *GatewayAPI) check(req accessgw.AuthRequest) (*accessgw.AuthResponse, error) {
	if !ValidLeaseNo(req.Login) {
		return deny, nil
	}
	r, err := g.leaseRoute(req.Login)
	if err != nil || r == nil {
		return deny, err
	}
	now := g.now()
	if r.State != LeaseActive || r.DeliveryKind != "container" || r.AccessUntil == nil || !now.Before(*r.AccessUntil) || !r.routable() {
		return deny, nil
	}
	switch req.Method {
	case accessgw.MethodPublicKey:
		var n int
		if err := g.s.repo.db.Get(&n, `SELECT COUNT(*) FROM buyer_ssh_keys WHERE user_id=? AND fingerprint=? AND revoked_at IS NULL`,
			r.BuyerID, req.PubkeyFP); err != nil {
			return nil, err
		}
		if n == 0 {
			return deny, nil
		}
	case accessgw.MethodPassword:
		v, err := g.s.products.ManagedAccessValue(g.s.repo.db, r.OrderID)
		if err != nil {
			return nil, wrapKeyErr(err)
		}
		if v == "" || req.Password == "" || subtle.ConstantTimeCompare([]byte(v), []byte(req.Password)) != 1 {
			return deny, nil
		}
	}
	ttl := min(accessgw.MaxCacheTTL, r.AccessUntil.Sub(now))
	until := *r.AccessUntil
	return &accessgw.AuthResponse{Allow: true, UpstreamHost: *r.Host, UpstreamPort: *r.Port, UpstreamHostKeyFP: *r.HostKeyFP,
		AccessUntil: &until, CacheTTLS: int(ttl / time.Second)}, nil
}

// systemAuth 系统自检身份(fleet.selftest_key_file): 只能登录 provisioning 租约(开通自检), 或领取 P11 端口连通任务。
func (g *GatewayAPI) systemAuth(ctx context.Context, login string) (*accessgw.AuthResponse, error) {
	if token, ok := probeLoginToken(login); ok {
		raw, err := g.rdb.Get(ctx, probeKey(token)).Bytes()
		if errors.Is(err, redis.Nil) {
			return deny, nil
		}
		if err != nil {
			return nil, err
		}
		var t accessgw.ProbeTask
		if err := json.Unmarshal(raw, &t); err != nil {
			return nil, err
		}
		t.Token = token
		return &accessgw.AuthResponse{Allow: false, Probe: &t}, nil
	}
	if !ValidLeaseNo(login) {
		return deny, nil
	}
	r, err := g.leaseRoute(login)
	if err != nil || r == nil {
		return deny, err
	}
	if r.State != LeaseProvisioning || r.DeliveryKind != "container" || !r.routable() {
		return deny, nil
	}
	return &accessgw.AuthResponse{Allow: true, System: true, UpstreamHost: *r.Host, UpstreamPort: *r.Port,
		UpstreamHostKeyFP: *r.HostKeyFP, CacheTTLS: 0}, nil
}

// ===== 撤销游标 =====

// parseCursor 游标 "<unix 秒>-<id>"; 空串表示网关刚启动(本地无缓存), 从当前位置开始。
func parseCursor(c string) (ts, id int64, ok bool) {
	a, b, found := strings.Cut(c, "-")
	if !found {
		return 0, 0, false
	}
	ts, err1 := strconv.ParseInt(a, 10, 64)
	id, err2 := strconv.ParseInt(b, 10, 64)
	return ts, id, err1 == nil && err2 == nil && ts >= 0 && id >= 0
}

// Revocations GET /internal/accessgw/revocations?cursor=。返回游标之后访问条件变化的登录名(按 access_changed_at, id 递增,
// 不重不漏), 只推进到 NOW()-2s 为止。
func (g *GatewayAPI) Revocations(cursor string) (*accessgw.RevocationsResponse, error) {
	db := g.s.repo.db
	if cursor == "" {
		var head int64
		if err := db.Get(&head, `SELECT CAST(UNIX_TIMESTAMP(NOW() - INTERVAL ? SECOND) AS SIGNED)`, gwCursorLag); err != nil {
			return nil, err
		}
		return &accessgw.RevocationsResponse{Logins: []string{}, Cursor: fmt.Sprintf("%d-0", head)}, nil
	}
	ts, id, ok := parseCursor(cursor)
	if !ok {
		return nil, errcode.New(errcode.ParamInvalid, "cursor 非法")
	}
	var rows []struct {
		ID      int64  `db:"id"`
		LeaseNo string `db:"lease_no"`
		TS      int64  `db:"ts"`
	}
	if err := db.Select(&rows, `SELECT id, lease_no, CAST(UNIX_TIMESTAMP(access_changed_at) AS SIGNED) ts FROM leases
		WHERE (access_changed_at > FROM_UNIXTIME(?) OR (access_changed_at = FROM_UNIXTIME(?) AND id > ?))
		  AND access_changed_at <= NOW() - INTERVAL ? SECOND
		ORDER BY access_changed_at, id LIMIT ?`, ts, ts, id, gwCursorLag, gwRevocationBatch); err != nil {
		return nil, err
	}
	out := &accessgw.RevocationsResponse{Logins: make([]string, 0, len(rows)), Cursor: cursor}
	for _, r := range rows {
		out.Logins = append(out.Logins, r.LeaseNo)
		out.Cursor = fmt.Sprintf("%d-%d", r.TS, r.ID)
	}
	return out, nil
}

// ===== 会话审计 =====

// RecordSessions POST /internal/accessgw/sessions: 以 gw_session_id 幂等 upsert; 结束信息只写一次, 字节数取最大值。
// 未知登录名、系统自检身份、字段非法的事件计入 ignored(不报错, 避免一条坏事件卡住整批重试)。
func (g *GatewayAPI) RecordSessions(events []accessgw.SessionEvent) (*accessgw.SessionsResponse, error) {
	if len(events) > accessgw.MaxSessionEvents {
		return nil, errcode.Newf(errcode.ParamInvalid, "单次最多上报 %d 条会话事件", accessgw.MaxSessionEvents)
	}
	out := &accessgw.SessionsResponse{}
	leaseIDs := map[string]int64{}
	for _, e := range events {
		if !g.validSessionEvent(e) {
			out.Ignored++
			continue
		}
		lid, seen := leaseIDs[e.Login]
		if !seen {
			if err := g.s.repo.db.Get(&lid, `SELECT id FROM leases WHERE lease_no=?`, e.Login); err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			leaseIDs[e.Login] = lid
		}
		if lid == 0 {
			out.Ignored++
			continue
		}
		var fp, reason *string
		if e.KeyFingerprint != "" {
			fp = &e.KeyFingerprint
		}
		if e.EndReason != "" {
			r := truncate(e.EndReason, 64)
			reason = &r
		}
		if _, err := g.s.repo.db.Exec(`INSERT INTO access_sessions (lease_id, gw_session_id, remote_ip, auth_method, key_fingerprint,
				started_at, ended_at, bytes_up, bytes_down, end_reason) VALUES (?,?,?,?,?,?,?,?,?,?)
			ON DUPLICATE KEY UPDATE ended_at=COALESCE(ended_at, VALUES(ended_at)), end_reason=COALESCE(end_reason, VALUES(end_reason)),
				bytes_up=GREATEST(bytes_up, VALUES(bytes_up)), bytes_down=GREATEST(bytes_down, VALUES(bytes_down))`,
			lid, e.GWSessionID, net.ParseIP(e.RemoteIP).String(), e.AuthMethod, fp, e.StartedAt, e.EndedAt,
			max(e.BytesUp, 0), max(e.BytesDown, 0), reason); err != nil {
			return nil, err
		}
		out.Accepted++
	}
	return out, nil
}

func (g *GatewayAPI) validSessionEvent(e accessgw.SessionEvent) bool {
	switch {
	case e.GWSessionID == "" || len(e.GWSessionID) > 64:
		return false
	case !ValidLeaseNo(e.Login):
		return false
	case e.AuthMethod != accessgw.MethodPublicKey && e.AuthMethod != accessgw.MethodPassword:
		return false
	case net.ParseIP(e.RemoteIP) == nil:
		return false
	case e.StartedAt.IsZero() || (e.EndedAt != nil && e.EndedAt.Before(e.StartedAt)):
		return false
	case len(e.KeyFingerprint) > 80:
		return false
	case e.KeyFingerprint != "" && subtle.ConstantTimeCompare([]byte(e.KeyFingerprint), []byte(g.selftestFP)) == 1:
		return false // 系统自检不进买家会话记录
	}
	return true
}

// PurgeOldSessions 会话记录保留 180 天(周期任务调用, 分批删除)。
func (r *Repository) PurgeOldSessions(before time.Time) (int64, error) {
	var total int64
	for {
		res, err := r.db.Exec(`DELETE FROM access_sessions WHERE started_at < ? LIMIT 5000`, before)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < 5000 {
			return total, nil
		}
	}
}

// ===== P11 端口连通任务 =====

// ProbeResult POST /internal/accessgw/probe-result: 网关回报对临时端口的 TCP 连接结果。
func (g *GatewayAPI) ProbeResult(ctx context.Context, r accessgw.ProbeResult) error {
	if !accessgw.ValidNonce(r.Token) {
		return errcode.New(errcode.ParamInvalid, "token 非法")
	}
	n, err := g.rdb.Exists(ctx, probeKey(r.Token)).Result()
	if err != nil {
		return err
	}
	if n == 0 {
		return errcode.New(errcode.NotFound, "探活任务不存在或已过期")
	}
	r.Error = truncate(r.Error, 300)
	raw, err := json.Marshal(r)
	if err != nil {
		return err
	}
	return g.rdb.Set(ctx, probeResultKey(r.Token), raw, gwProbeTTL).Err()
}

// gatewayTouchBuyerLeases 买家公钥变化(删除)时刷新其在租租约的撤销游标, 让网关清掉按旧公钥缓存的放行。
func (r *Repository) touchBuyerLeases(q sqlx.Execer, buyerID int64) error {
	_, err := q.Exec(`UPDATE leases SET access_changed_at=NOW() WHERE buyer_id=? AND state IN ('active','parked','suspended','grace')`, buyerID)
	return err
}
