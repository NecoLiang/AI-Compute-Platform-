package accessgw

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// fakeAPI 测试用的主服务内部接口: 按 (login, fp/password) 决定放行。
type fakeAPI struct {
	mu        sync.Mutex
	allow     map[string]*AuthResponse // key: login+"|"+fp 或 login+"|pw:"+password
	authErr   error
	authCalls int
	onAuth    func()

	revs     []RevocationsResponse
	revErr   error
	cursors  []string
	sessErr  error
	sessions [][]SessionEvent
	probes   []ProbeResult
}

func (f *fakeAPI) Auth(_ context.Context, req AuthRequest) (*AuthResponse, error) {
	f.mu.Lock()
	f.authCalls++
	fn := f.onAuth
	key := req.Login + "|" + req.PubkeyFP
	if req.Method == MethodPassword {
		key = req.Login + "|pw:" + req.Password
	}
	r, err := f.allow[key], f.authErr
	f.mu.Unlock()
	if fn != nil {
		fn()
	}
	if err != nil {
		return nil, err
	}
	if r == nil {
		return &AuthResponse{}, nil
	}
	cp := *r
	return &cp, nil
}

func (f *fakeAPI) Revocations(_ context.Context, cursor string) (*RevocationsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cursors = append(f.cursors, cursor)
	if f.revErr != nil {
		return nil, f.revErr
	}
	if len(f.revs) == 0 {
		return &RevocationsResponse{Cursor: cursor}, nil
	}
	r := f.revs[0]
	f.revs = f.revs[1:]
	return &r, nil
}

func (f *fakeAPI) ReportSessions(_ context.Context, ev []SessionEvent) (*SessionsResponse, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.sessErr != nil {
		return nil, f.sessErr
	}
	f.sessions = append(f.sessions, ev)
	return &SessionsResponse{Accepted: len(ev)}, nil
}

func (f *fakeAPI) ReportProbe(_ context.Context, r ProbeResult) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.probes = append(f.probes, r)
	return nil
}

func (f *fakeAPI) calls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authCalls
}

func newKey(t *testing.T) (ssh.PublicKey, string) {
	t.Helper()
	pub, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return k, ssh.FingerprintSHA256(k)
}

const lease = "L20261028000000000001"

func allowResp(ttl int, fp string) *AuthResponse {
	return &AuthResponse{Allow: true, UpstreamHost: "10.1.1.1", UpstreamPort: 47000, UpstreamHostKeyFP: fp, CacheTTLS: ttl}
}

func TestGatewayCachesOnlyAllow(t *testing.T) {
	k, fp := newKey(t)
	hostK, hostFP := newKey(t)
	api := &fakeAPI{allow: map[string]*AuthResponse{lease + "|" + fp: allowResp(30, hostFP)}}
	g := NewGateway(api, nil)
	now := time.Unix(1_800_000_000, 0)
	g.now = func() time.Time { return now }
	ctx := context.Background()

	r, err := g.AuthPublicKey(ctx, "c1", lease, "1.2.3.4:5555", k.Marshal())
	if err != nil || r.Host != "10.1.1.1" || r.Port != 47000 {
		t.Fatalf("allow: %+v %v", r, err)
	}
	if _, err := g.AuthPublicKey(ctx, "c2", lease, "1.2.3.4:5556", k.Marshal()); err != nil || api.calls() != 1 {
		t.Fatalf("second login must hit cache: calls=%d err=%v", api.calls(), err)
	}
	// 拒绝不缓存。
	other, _ := newKey(t)
	for i := 0; i < 2; i++ {
		if _, err := g.AuthPublicKey(ctx, "c3", lease, "1.2.3.4:1", other.Marshal()); !errors.Is(err, ErrDenied) {
			t.Fatalf("unknown key: %v", err)
		}
	}
	if api.calls() != 3 {
		t.Fatalf("denials must not be cached: calls=%d", api.calls())
	}
	// 30s 后过期回源。
	now = now.Add(30 * time.Second)
	if _, err := g.AuthPublicKey(ctx, "c4", lease, "1.2.3.4:1", k.Marshal()); err != nil || api.calls() != 4 {
		t.Fatalf("expired cache must re-auth: calls=%d %v", api.calls(), err)
	}
	// 上游主机密钥按连接校验。
	if err := g.VerifyHostKey("c1", hostK.Marshal()); err != nil {
		t.Fatalf("host key: %v", err)
	}
	if err := g.VerifyHostKey("c1", other.Marshal()); err == nil {
		t.Fatal("wrong upstream host key accepted")
	}
	if err := g.VerifyHostKey("nope", hostK.Marshal()); err == nil {
		t.Fatal("unknown connection accepted")
	}
	if _, err := g.AuthPublicKey(ctx, "c5", lease, "1.2.3.4:1", []byte("garbage")); !errors.Is(err, ErrDenied) {
		t.Fatal("garbage key must be denied")
	}
}

func TestGatewayTTLCappedAndSystemNotCached(t *testing.T) {
	k, fp := newKey(t)
	_, hostFP := newKey(t)
	sys := allowResp(0, hostFP)
	sys.System = true
	api := &fakeAPI{allow: map[string]*AuthResponse{
		lease + "|" + fp:     allowResp(3600, hostFP),
		lease + "|pw:secret": sys,
	}}
	g := NewGateway(api, nil)
	now := time.Unix(1_800_000_000, 0)
	g.now = func() time.Time { return now }
	ctx := context.Background()
	if _, err := g.AuthPublicKey(ctx, "a", lease, "1.1.1.1:1", k.Marshal()); err != nil {
		t.Fatal(err)
	}
	now = now.Add(MaxCacheTTL)
	if _, err := g.AuthPublicKey(ctx, "b", lease, "1.1.1.1:1", k.Marshal()); err != nil || api.calls() != 2 {
		t.Fatalf("cache TTL must be capped at 30s: calls=%d", api.calls())
	}
	for i := 0; i < 2; i++ {
		r, err := g.AuthPassword(ctx, "s", lease, "1.1.1.1:1", []byte("secret"))
		if err != nil || !r.System {
			t.Fatalf("system: %+v %v", r, err)
		}
	}
	if api.calls() != 4 {
		t.Fatalf("system allow must not be cached: %d", api.calls())
	}
	if _, err := g.AuthPassword(ctx, "s2", lease, "1.1.1.1:1", []byte("Secret")); !errors.Is(err, ErrDenied) {
		t.Fatal("wrong password accepted")
	}
}

func TestGatewayFailClosed(t *testing.T) {
	k, fp := newKey(t)
	_, hostFP := newKey(t)
	api := &fakeAPI{allow: map[string]*AuthResponse{lease + "|" + fp: allowResp(30, hostFP)}}
	g := NewGateway(api, nil)
	ctx := context.Background()
	if _, err := g.AuthPublicKey(ctx, "a", lease, "1.1.1.1:1", k.Marshal()); err != nil {
		t.Fatal(err)
	}
	// 主服务不可达: 新登录拒绝; 撤销轮询失败清空缓存, 缓存也不能再放行。
	api.mu.Lock()
	api.authErr, api.revErr = errors.New("dial tcp: connection refused"), errors.New("down")
	api.mu.Unlock()
	other, _ := newKey(t)
	if _, err := g.AuthPublicKey(ctx, "b", lease, "1.1.1.1:1", other.Marshal()); !errors.Is(err, ErrDenied) {
		t.Fatal("must deny when main service is unreachable")
	}
	g.Tick(ctx)
	if c, _, _, _ := g.Stats(); c != 0 {
		t.Fatalf("cache must be cleared on revocation poll failure: %d", c)
	}
	if _, err := g.AuthPublicKey(ctx, "c", lease, "1.1.1.1:1", k.Marshal()); !errors.Is(err, ErrDenied) {
		t.Fatal("cached allow must not survive a failed revocation poll")
	}
}

func TestGatewayRevocations(t *testing.T) {
	k, fp := newKey(t)
	_, hostFP := newKey(t)
	const lease2 = "L20261028000000000002"
	api := &fakeAPI{allow: map[string]*AuthResponse{lease + "|" + fp: allowResp(30, hostFP), lease2 + "|" + fp: allowResp(30, hostFP)},
		revs: []RevocationsResponse{{Cursor: "100-0"}, {Logins: []string{lease}, Cursor: "101-7"}, {Cursor: "101-7"}}}
	g := NewGateway(api, nil)
	ctx := context.Background()
	g.Tick(ctx) // 初次: 取得游标
	for _, l := range []string{lease, lease2} {
		if _, err := g.AuthPublicKey(ctx, "x", l, "1.1.1.1:1", k.Marshal()); err != nil {
			t.Fatal(err)
		}
	}
	g.Tick(ctx)
	if _, _, _, cur := g.Stats(); cur != "101-7" {
		t.Fatalf("cursor %q", cur)
	}
	if want := []string{"", "100-0", "101-7"}; len(api.cursors) != 3 || api.cursors[0] != want[0] || api.cursors[1] != want[1] || api.cursors[2] != want[2] {
		t.Fatalf("cursors %v", api.cursors)
	}
	n := api.calls()
	if _, err := g.AuthPublicKey(ctx, "y", lease2, "1.1.1.1:1", k.Marshal()); err != nil || api.calls() != n {
		t.Fatal("unrelated login must stay cached")
	}
	api.mu.Lock()
	delete(api.allow, lease+"|"+fp)
	api.mu.Unlock()
	if _, err := g.AuthPublicKey(ctx, "z", lease, "1.1.1.1:1", k.Marshal()); !errors.Is(err, ErrDenied) {
		t.Fatal("revoked login must go back to the main service")
	}
}

// 撤销与鉴权交错: 鉴权请求在途时发生了撤销, 返回的放行不得进入缓存。
func TestGatewayRevocationDuringAuthNotCached(t *testing.T) {
	k, fp := newKey(t)
	_, hostFP := newKey(t)
	api := &fakeAPI{allow: map[string]*AuthResponse{lease + "|" + fp: allowResp(30, hostFP)},
		revs: []RevocationsResponse{{Cursor: "1-0"}, {Logins: []string{lease}, Cursor: "2-1"}}}
	g := NewGateway(api, nil)
	ctx := context.Background()
	g.Tick(ctx)
	once := sync.Once{}
	api.onAuth = func() { once.Do(func() { g.Tick(ctx) }) }
	if _, err := g.AuthPublicKey(ctx, "a", lease, "1.1.1.1:1", k.Marshal()); err != nil {
		t.Fatal(err)
	}
	if c, _, _, _ := g.Stats(); c != 0 {
		t.Fatal("allow obtained across a revocation must not be cached")
	}
}

func TestGatewayProbeTask(t *testing.T) {
	k, fp := newKey(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	api := &fakeAPI{allow: map[string]*AuthResponse{
		"p-open|" + fp:   {Probe: &ProbeTask{Token: "tok1", Host: "127.0.0.1", Port: port}},
		"p-closed|" + fp: {Probe: &ProbeTask{Token: "tok2", Host: "127.0.0.1", Port: port}},
	}}
	g := NewGateway(api, nil)
	g.AllowLoopbackForTest = true
	ctx := context.Background()
	if _, err := g.AuthPublicKey(ctx, "a", "p-open", "1.1.1.1:1", k.Marshal()); !errors.Is(err, ErrDenied) {
		t.Fatal("probe login must always be denied")
	}
	_ = ln.Close()
	if _, err := g.AuthPublicKey(ctx, "b", "p-closed", "1.1.1.1:1", k.Marshal()); !errors.Is(err, ErrDenied) {
		t.Fatal("probe login must always be denied")
	}
	if len(api.probes) != 2 || !api.probes[0].Reachable || api.probes[0].Token != "tok1" || api.probes[1].Reachable || api.probes[1].Error == "" {
		t.Fatalf("probe results %+v", api.probes)
	}
	if _, conns, _, _ := g.Stats(); conns != 0 {
		t.Fatal("probe must not register a connection")
	}
}

func TestGatewaySessionEvents(t *testing.T) {
	k, fp := newKey(t)
	_, hostFP := newKey(t)
	sys := allowResp(0, hostFP)
	sys.System = true
	api := &fakeAPI{allow: map[string]*AuthResponse{lease + "|" + fp: allowResp(30, hostFP), lease + "|pw:sys": sys},
		sessErr: errors.New("down")}
	g := NewGateway(api, nil)
	t0 := time.Unix(1_800_000_000, 0)
	now := t0
	g.now = func() time.Time { return now }
	ctx := context.Background()
	if _, err := g.AuthPublicKey(ctx, "conn-1", lease, "[2001:db8::1]:50000", k.Marshal()); err != nil {
		t.Fatal(err)
	}
	g.PipeStart("conn-1")
	now = now.Add(time.Minute)
	g.PipeEnd("conn-1", errors.New("EOF"))
	// 系统自检会话不上报。
	if _, err := g.AuthPassword(ctx, "conn-2", lease, "1.1.1.1:1", []byte("sys")); err != nil {
		t.Fatal(err)
	}
	g.PipeStart("conn-2")
	g.PipeEnd("conn-2", nil)
	// 认证通过但没建立管道: 不上报, 10 分钟后清理。
	if _, err := g.AuthPublicKey(ctx, "conn-3", lease, "1.1.1.1:1", k.Marshal()); err != nil {
		t.Fatal(err)
	}

	g.Tick(ctx) // 上报失败: 事件保留
	if _, _, q, _ := g.Stats(); q != 2 {
		t.Fatalf("events must be kept for retry, queued=%d", q)
	}
	api.mu.Lock()
	api.sessErr = nil
	api.mu.Unlock()
	now = now.Add(11 * time.Minute)
	g.Tick(ctx)
	_, conns, q, _ := g.Stats()
	if q != 0 || conns != 0 {
		t.Fatalf("queued=%d conns=%d", q, conns)
	}
	if len(api.sessions) != 1 || len(api.sessions[0]) != 2 {
		t.Fatalf("sessions %+v", api.sessions)
	}
	start, end := api.sessions[0][0], api.sessions[0][1]
	if start.GWSessionID != "conn-1" || start.EndedAt != nil || start.RemoteIP != "2001:db8::1" || start.KeyFingerprint != fp ||
		start.AuthMethod != MethodPublicKey || !start.StartedAt.Equal(t0) {
		t.Fatalf("start %+v", start)
	}
	if end.EndedAt == nil || !end.EndedAt.Equal(t0.Add(time.Minute)) || end.EndReason != "closed" || !end.StartedAt.Equal(t0) {
		t.Fatalf("end %+v", end)
	}
}

func TestGatewayQueueBounded(t *testing.T) {
	g := NewGateway(&fakeAPI{}, nil)
	for i := 0; i < maxQueue+5; i++ {
		g.enqueue(SessionEvent{GWSessionID: strconv.Itoa(i)})
	}
	if len(g.queue) != maxQueue || g.queue[0].GWSessionID != "5" || g.dropped != 5 {
		t.Fatalf("len=%d first=%s dropped=%d", len(g.queue), g.queue[0].GWSessionID, g.dropped)
	}
}

func TestGatewayProbeDeniesInternalTargets(t *testing.T) {
	k, fp := newKey(t)
	api := &fakeAPI{allow: map[string]*AuthResponse{
		"p-meta|" + fp: {Probe: &ProbeTask{Token: "t1", Host: "100.100.100.200", Port: 80}},
		"p-lo|" + fp:   {Probe: &ProbeTask{Token: "t2", Host: "127.0.0.1", Port: 22}},
		"p-dns|" + fp:  {Probe: &ProbeTask{Token: "t3", Host: "localhost", Port: 22}},
		"p-ula|" + fp:  {Probe: &ProbeTask{Token: "t4", Host: "fd00::1", Port: 22}},
	}}
	g := NewGateway(api, nil) // 生产默认: 不放开回环
	ctx := context.Background()
	for _, login := range []string{"p-meta", "p-lo", "p-dns", "p-ula"} {
		if _, err := g.AuthPublicKey(ctx, login, login, "1.1.1.1:1", k.Marshal()); !errors.Is(err, ErrDenied) {
			t.Fatal("probe login must always be denied")
		}
	}
	if len(api.probes) != 4 {
		t.Fatalf("probe results %+v", api.probes)
	}
	for _, r := range api.probes {
		if r.Reachable || r.Error == "" {
			t.Fatalf("internal target must not be probed: %+v", r)
		}
	}
	if api.probes[2].Error != "探活任务地址非法" {
		t.Fatalf("domain names must be rejected without resolving: %+v", api.probes[2])
	}
}

// AllowLoopbackForTest 不得在生产代码中置位。
func TestAllowLoopbackOnlySetInTests(t *testing.T) {
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if n := d.Name(); n == "vendor" || n == "node_modules" || strings.HasPrefix(n, ".") && n != "." && n != ".." {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range strings.Split(string(b), "\n") {
			if strings.Contains(line, "AllowLoopbackForTest") && !strings.HasPrefix(strings.TrimSpace(line), "//") &&
				!strings.Contains(line, "AllowLoopbackForTest bool") && !strings.Contains(line, "g.AllowLoopbackForTest {") {
				t.Errorf("%s: AllowLoopbackForTest 只能在 _test.go 中使用: %s", path, strings.TrimSpace(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestValidUpstreamUser(t *testing.T) {
	for _, u := range []string{"omnis-l-abcdefgh", "root", "_svc", "a"} {
		if !ValidUpstreamUser(u) {
			t.Errorf("%q should be valid", u)
		}
	}
	for _, u := range []string{"", "Root", "-x", "1abc", "a b", "a;id", "a$(id)", "a/b", strings.Repeat("a", 33), "omnis-l-ä"} {
		if ValidUpstreamUser(u) {
			t.Errorf("%q should be invalid", u)
		}
	}
}
