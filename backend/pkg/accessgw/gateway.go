package accessgw

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"time"

	"tokenfactory/pkg/netguard"

	"golang.org/x/crypto/ssh"
)

// 网关核心逻辑(docs/25 §25.6): 鉴权委托主服务、只缓存"允许"且 ≤30s、每 5s 拉撤销游标、会话事件批量上报、
// P11 端口连通任务。不依赖 sshpiper, cmd/accessgw 只做 libplugin 回调到这里的薄适配, 便于在没有 sshpiperd 的
// 环境下用真实 SSH 握手测试。

// ErrDenied 统一的拒绝(不区分登录名不存在/凭证错误/租约到期/主服务不可达)。
var ErrDenied = errors.New("permission denied")

// API 主服务内部接口(*Client 为真实实现)。
type API interface {
	Auth(ctx context.Context, req AuthRequest) (*AuthResponse, error)
	Revocations(ctx context.Context, cursor string) (*RevocationsResponse, error)
	ReportSessions(ctx context.Context, events []SessionEvent) (*SessionsResponse, error)
	ReportProbe(ctx context.Context, r ProbeResult) error
}

// Route 放行后的上游(租约容器 sshd)。
type Route struct {
	Host      string
	Port      int
	HostKeyFP string
	// User 上游账户名; 空 = 网关默认账户(见 AuthResponse.UpstreamUser)。
	User   string
	System bool
}

// ValidUpstreamUser 上游账户名白名单(POSIX 可移植用户名子集, ≤32 位): 主服务下发的账户名不合法即拒绝登录。
func ValidUpstreamUser(u string) bool {
	if len(u) == 0 || len(u) > 32 || !(u[0] >= 'a' && u[0] <= 'z' || u[0] == '_') {
		return false
	}
	for i := 1; i < len(u); i++ {
		c := u[i]
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			return false
		}
	}
	return true
}

const (
	// PollInterval 撤销游标轮询与会话上报周期。
	PollInterval = 5 * time.Second
	// ProbeDialTimeout P11 TCP 连接超时。
	ProbeDialTimeout = 5 * time.Second
	maxQueue         = 20000
	connInfoTTL      = 10 * time.Minute
	authTimeout      = 5 * time.Second
)

type cacheEntry struct {
	route   Route
	login   string
	expires time.Time
}

// connInfo 一条 SSH 连接(sshpiper UniqueID)放行时的身份, 供上游主机密钥校验与会话审计使用。
type connInfo struct {
	login   string
	ip      string
	method  string
	fp      string
	route   Route
	created time.Time
	started *time.Time
}

type Gateway struct {
	api API
	log *slog.Logger
	now func() time.Time
	// ProbeDial P11 的 TCP 连接实现(默认 net.Dialer, 5s 超时, Control 钩子按 netguard 内置拒绝名单拦截)。
	probeDial func(ctx context.Context, addr string) error
	// AllowLoopbackForTest 只允许在 _test.go 中置位(TestAllowLoopbackOnlySetInTests 扫描源码强制): P11 探活放开
	// 127.0.0.0/8、::1, 供跨包集成测试连接本机假节点; 不放开其他任何拒绝段, 也没有对应的配置项/命令行参数。
	AllowLoopbackForTest bool

	pwKey []byte // 进程内随机密钥, 缓存键里的口令只存其 HMAC

	mu      sync.Mutex
	cache   map[string]cacheEntry
	epoch   uint64 // 每次撤销清缓存 +1; 鉴权期间若变化则不缓存本次结果(防止撤销与鉴权交错留下旧放行)
	conns   map[string]*connInfo
	queue   []SessionEvent
	cursor  string
	dropped int
}

func NewGateway(api API, log *slog.Logger) *Gateway {
	if log == nil {
		log = slog.Default()
	}
	k := make([]byte, 32)
	_, _ = rand.Read(k)
	g := &Gateway{api: api, log: log, now: time.Now, pwKey: k, cache: map[string]cacheEntry{}, conns: map[string]*connInfo{}}
	g.probeDial = g.dialProbe
	return g
}

var probePolicy = netguard.Default()

// dialProbe 主服务下发的探活目标已按完整策略(含 fleet.deny_cidrs)校验并解析为 IP 字面量;
// 网关侧再按内置拒绝名单在拨号钩子里拦一次, 防止主服务被绕过时网关成为内网跳板。
func (g *Gateway) dialProbe(ctx context.Context, addr string) error {
	d := net.Dialer{Timeout: ProbeDialTimeout, Control: func(network, address string, c syscall.RawConn) error {
		if g.AllowLoopbackForTest {
			if h, _, err := net.SplitHostPort(address); err == nil {
				if a, err := netip.ParseAddr(h); err == nil && a.Unmap().IsLoopback() {
					return nil
				}
			}
		}
		return probePolicy.Control(network, address, c)
	}}
	c, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return c.Close()
}

func remoteIP(remoteAddr string) string {
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return h
	}
	return remoteAddr
}

// AuthPublicKey key 为 SSH wire 格式公钥(libplugin PublicKeyCallback 的入参)。
func (g *Gateway) AuthPublicKey(ctx context.Context, connID, login, remoteAddr string, key []byte) (*Route, error) {
	pk, err := ssh.ParsePublicKey(key)
	if err != nil {
		return nil, ErrDenied
	}
	fp := ssh.FingerprintSHA256(pk)
	return g.auth(ctx, connID, "pk\x00"+login+"\x00"+fp, AuthRequest{Login: login, Method: MethodPublicKey, PubkeyFP: fp,
		RemoteIP: remoteIP(remoteAddr)})
}

func (g *Gateway) AuthPassword(ctx context.Context, connID, login, remoteAddr string, password []byte) (*Route, error) {
	m := hmac.New(sha256.New, g.pwKey)
	m.Write(password)
	return g.auth(ctx, connID, "pw\x00"+login+"\x00"+hex.EncodeToString(m.Sum(nil)), AuthRequest{Login: login,
		Method: MethodPassword, Password: string(password), RemoteIP: remoteIP(remoteAddr)})
}

func (g *Gateway) auth(ctx context.Context, connID, cacheKey string, req AuthRequest) (*Route, error) {
	now := g.now()
	g.mu.Lock()
	if e, ok := g.cache[cacheKey]; ok {
		if now.Before(e.expires) {
			g.remember(connID, req, e.route, now)
			g.mu.Unlock()
			return &e.route, nil
		}
		delete(g.cache, cacheKey)
	}
	epoch := g.epoch
	g.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, authTimeout)
	defer cancel()
	resp, err := g.api.Auth(ctx, req)
	if err != nil {
		// fail-closed: 主服务不可达/验签失败 → 拒绝新连接; 已建立的连接不受影响。
		g.log.Warn("鉴权请求失败, 拒绝登录", "login", req.Login, "remote_ip", req.RemoteIP, "error", err)
		return nil, ErrDenied
	}
	if resp.Probe != nil {
		g.probe(ctx, *resp.Probe)
		return nil, ErrDenied
	}
	if !resp.Allow || resp.UpstreamHost == "" || resp.UpstreamPort <= 0 || resp.UpstreamHostKeyFP == "" {
		return nil, ErrDenied
	}
	if resp.UpstreamUser != "" && !ValidUpstreamUser(resp.UpstreamUser) {
		g.log.Warn("主服务下发的上游账户名非法, 拒绝登录", "login", req.Login)
		return nil, ErrDenied
	}
	route := Route{Host: resp.UpstreamHost, Port: resp.UpstreamPort, HostKeyFP: resp.UpstreamHostKeyFP,
		User: resp.UpstreamUser, System: resp.System}
	g.mu.Lock()
	defer g.mu.Unlock()
	ttl := min(time.Duration(resp.CacheTTLS)*time.Second, MaxCacheTTL)
	if !resp.System && ttl > 0 && g.epoch == epoch {
		g.cache[cacheKey] = cacheEntry{route: route, login: req.Login, expires: now.Add(ttl)}
	}
	g.remember(connID, req, route, now)
	return &route, nil
}

func (g *Gateway) remember(connID string, req AuthRequest, r Route, now time.Time) {
	if connID == "" {
		return
	}
	g.conns[connID] = &connInfo{login: req.Login, ip: req.RemoteIP, method: req.Method, fp: req.PubkeyFP, route: r, created: now}
}

func (g *Gateway) probe(ctx context.Context, t ProbeTask) {
	res := ProbeResult{Token: t.Token}
	if _, perr := netip.ParseAddr(t.Host); perr != nil || t.Port <= 0 || t.Port > 65535 {
		// 主服务只下发 IP 字面量; 域名一律拒绝, 网关不自行解析。
		res.Error = "探活任务地址非法"
	} else if err := g.probeDial(ctx, net.JoinHostPort(t.Host, strconv.Itoa(t.Port))); err != nil {
		res.Error = err.Error()
	} else {
		res.Reachable = true
	}
	rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), authTimeout)
	defer cancel()
	if err := g.api.ReportProbe(rctx, res); err != nil {
		g.log.Warn("回报探活结果失败", "error", err)
	}
}

// Upstream 放行后的上游地址(VerifyHostKey 之前 sshpiperd 已按 Route 连接)。
func (g *Gateway) Upstream(connID string) (Route, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.conns[connID]
	if !ok {
		return Route{}, false
	}
	return c.route, true
}

// VerifyHostKey 上游 sshd 主机公钥必须与租约开通时记录的指纹一致, 否则拒绝建立管道(防止端口被冒名顶替)。
func (g *Gateway) VerifyHostKey(connID string, key []byte) error {
	pk, err := ssh.ParsePublicKey(key)
	if err != nil {
		return errors.New("上游主机公钥无法解析")
	}
	g.mu.Lock()
	c, ok := g.conns[connID]
	g.mu.Unlock()
	if !ok {
		return errors.New("未知连接")
	}
	if got := ssh.FingerprintSHA256(pk); got != c.route.HostKeyFP {
		g.log.Warn("上游主机密钥不符, 拒绝建立管道", "login", c.login, "want", c.route.HostKeyFP, "got", got)
		return errors.New("上游主机密钥不符")
	}
	return nil
}

// PipeStart 会话开始(系统自检身份不上报)。
func (g *Gateway) PipeStart(connID string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.conns[connID]
	if !ok {
		return
	}
	now := g.now()
	c.started = &now
	if !c.route.System {
		g.enqueue(g.event(connID, c, nil, ""))
	}
}

// PipeEnd 会话结束(sshpiper PipeErrorCallback, 正常断开也会触发)。
func (g *Gateway) PipeEnd(connID string, err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	c, ok := g.conns[connID]
	if !ok {
		return
	}
	delete(g.conns, connID)
	if c.route.System || c.started == nil {
		return
	}
	now := g.now()
	reason := "closed"
	if err != nil && err.Error() != "" && err.Error() != "<nil>" && err.Error() != "EOF" {
		reason = err.Error()
	}
	if len(reason) > 64 {
		reason = reason[:64]
	}
	g.enqueue(g.event(connID, c, &now, reason))
}

func (g *Gateway) event(connID string, c *connInfo, ended *time.Time, reason string) SessionEvent {
	// 字节数: sshpiper v1.6.1 插件接口不提供连接字节计数, 统一上报 0(docs/25 S2 实现口径)。
	return SessionEvent{GWSessionID: connID, Login: c.login, RemoteIP: c.ip, AuthMethod: c.method, KeyFingerprint: c.fp,
		StartedAt: *c.started, EndedAt: ended, EndReason: reason}
}

func (g *Gateway) enqueue(e SessionEvent) {
	if len(g.queue) >= maxQueue {
		g.queue = g.queue[1:]
		g.dropped++
	}
	g.queue = append(g.queue, e)
}

// Run 撤销轮询 + 会话上报 + 连接表清理, 直到 ctx 结束(结束前尽力上报一次)。
func (g *Gateway) Run(ctx context.Context) {
	t := time.NewTicker(PollInterval)
	defer t.Stop()
	g.Tick(ctx)
	for {
		select {
		case <-ctx.Done():
			fctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			g.flush(fctx)
			cancel()
			return
		case <-t.C:
			g.Tick(ctx)
		}
	}
}

// Tick 一轮: 拉撤销、上报会话、清理过期连接记录。
func (g *Gateway) Tick(ctx context.Context) {
	g.pollRevocations(ctx)
	g.flush(ctx)
	g.gc()
}

func (g *Gateway) pollRevocations(ctx context.Context) {
	g.mu.Lock()
	cursor := g.cursor
	g.mu.Unlock()
	for range 20 {
		rctx, cancel := context.WithTimeout(ctx, authTimeout)
		resp, err := g.api.Revocations(rctx, cursor)
		cancel()
		if err != nil {
			// 无法确认撤销 → 清空全部缓存(fail-closed), 后续每次登录都回主服务鉴权。
			g.mu.Lock()
			if len(g.cache) > 0 {
				g.cache = map[string]cacheEntry{}
			}
			g.epoch++
			g.mu.Unlock()
			g.log.Warn("拉取撤销列表失败, 已清空鉴权缓存", "error", err)
			return
		}
		g.mu.Lock()
		if len(resp.Logins) > 0 {
			gone := make(map[string]bool, len(resp.Logins))
			for _, l := range resp.Logins {
				gone[l] = true
			}
			for k, e := range g.cache {
				if gone[e.login] {
					delete(g.cache, k)
				}
			}
			g.epoch++
		}
		g.cursor = resp.Cursor
		g.mu.Unlock()
		if len(resp.Logins) == 0 || resp.Cursor == cursor {
			return
		}
		cursor = resp.Cursor
	}
}

func (g *Gateway) flush(ctx context.Context) {
	for {
		g.mu.Lock()
		n := min(len(g.queue), MaxSessionEvents)
		if n == 0 {
			g.mu.Unlock()
			return
		}
		batch := append([]SessionEvent(nil), g.queue[:n]...)
		dropped := g.dropped
		g.dropped = 0
		g.mu.Unlock()
		if dropped > 0 {
			g.log.Error("会话事件队列已满, 丢弃最旧事件", "dropped", dropped)
		}
		rctx, cancel := context.WithTimeout(ctx, authTimeout)
		_, err := g.api.ReportSessions(rctx, batch)
		cancel()
		if err != nil {
			g.log.Warn("上报会话事件失败, 下轮重试", "pending", n, "error", err)
			return
		}
		g.mu.Lock()
		// 上报期间可能有队首被丢弃(队列满), 只移除仍在队首的同一批事件。
		i := 0
		for i < n && i < len(g.queue) && g.queue[i].GWSessionID == batch[i].GWSessionID && g.queue[i].EndedAt == batch[i].EndedAt {
			i++
		}
		g.queue = g.queue[i:]
		g.mu.Unlock()
	}
}

// gc 认证通过但始终没有建立管道的连接记录(上游连接失败等)在 10 分钟后清理。
func (g *Gateway) gc() {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := g.now()
	for id, c := range g.conns {
		if c.started == nil && now.Sub(c.created) > connInfoTTL {
			delete(g.conns, id)
		}
	}
	for k, e := range g.cache {
		if !now.Before(e.expires) {
			delete(g.cache, k)
		}
	}
}

// Stats 运维/测试观测。
func (g *Gateway) Stats() (cached, conns, queued int, cursor string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.cache), len(g.conns), len(g.queue), g.cursor
}
