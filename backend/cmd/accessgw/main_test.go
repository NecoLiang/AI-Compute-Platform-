package main

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"tokenfactory/pkg/accessgw"

	"github.com/tg123/sshpiper/libplugin"
	"golang.org/x/crypto/ssh"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// 插件适配层测试: 通过真实 gRPC(libplugin.NewFromGrpc)按 sshpiperd 的调用方式驱动回调。
// testAPI 是测试替身, 只按 (login, fp/password) 返回预设结果。

type testAPI struct {
	mu       sync.Mutex
	allow    map[string]*accessgw.AuthResponse
	sessions []accessgw.SessionEvent
}

func (a *testAPI) Auth(_ context.Context, r accessgw.AuthRequest) (*accessgw.AuthResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	k := r.Login + "|" + r.PubkeyFP
	if r.Method == accessgw.MethodPassword {
		k = r.Login + "|pw:" + r.Password
	}
	if v, ok := a.allow[k]; ok {
		cp := *v
		return &cp, nil
	}
	return &accessgw.AuthResponse{}, nil
}

func (a *testAPI) Revocations(_ context.Context, c string) (*accessgw.RevocationsResponse, error) {
	return &accessgw.RevocationsResponse{Cursor: c}, nil
}

func (a *testAPI) ReportSessions(_ context.Context, ev []accessgw.SessionEvent) (*accessgw.SessionsResponse, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.sessions = append(a.sessions, ev...)
	return &accessgw.SessionsResponse{Accepted: len(ev)}, nil
}

func (a *testAPI) ReportProbe(context.Context, accessgw.ProbeResult) error { return nil }

func genKey(t *testing.T) (ssh.PublicKey, []byte) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	k, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	blk, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatal(err)
	}
	return k, pem.EncodeToMemory(blk)
}

func TestPluginOverGRPC(t *testing.T) {
	const login = "l-abcdefgh"
	buyer, _ := genKey(t)
	host, _ := genKey(t)
	other, _ := genKey(t)
	_, upKey := genKey(t)
	api := &testAPI{allow: map[string]*accessgw.AuthResponse{
		login + "|" + ssh.FingerprintSHA256(buyer): {Allow: true, UpstreamHost: "10.1.2.3", UpstreamPort: 47001,
			UpstreamHostKeyFP: ssh.FingerprintSHA256(host), CacheTTLS: 30},
		login + "|pw:s3cret": {Allow: true, UpstreamHost: "2001:db8::5", UpstreamPort: 47002,
			UpstreamHostKeyFP: ssh.FingerprintSHA256(host), CacheTTLS: 30},
		// 整机交付(T-089): 主服务指定上游账户; 非法账户名网关拒绝。
		login + "|pw:bare": {Allow: true, UpstreamHost: "10.1.2.4", UpstreamPort: 22,
			UpstreamHostKeyFP: ssh.FingerprintSHA256(host), UpstreamUser: "omnis-l-abcdefgh", CacheTTLS: 30},
		login + "|pw:evil": {Allow: true, UpstreamHost: "10.1.2.4", UpstreamPort: 22,
			UpstreamHostKeyFP: ssh.FingerprintSHA256(host), UpstreamUser: "root;id", CacheTTLS: 30},
	}}
	gw := accessgw.NewGateway(api, nil)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := grpc.NewServer()
	p, err := libplugin.NewFromGrpc(NewPluginConfig(gw, upKey, "root"), srv, ln)
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = p.Serve() }()
	t.Cleanup(srv.Stop)
	conn, err := grpc.NewClient("passthrough:///"+ln.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	c := libplugin.NewSshPiperPluginClient(conn)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cbs, err := c.ListCallbacks(ctx, &libplugin.ListCallbackRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"NextAuthMethods", "PasswordAuth", "PublicKeyAuth", "VerifyHostKey", "PipeStart", "PipeError", "PipeCreateError"} {
		if !slices.Contains(cbs.Callbacks, want) {
			t.Errorf("callback %s not registered: %v", want, cbs.Callbacks)
		}
	}
	if slices.Contains(cbs.Callbacks, "NoneAuth") || slices.Contains(cbs.Callbacks, "KeyboardInteractiveAuth") {
		t.Errorf("only publickey/password may be offered: %v", cbs.Callbacks)
	}

	meta := &libplugin.ConnMeta{UserName: login, FromAddr: "198.51.100.9:50000", UniqId: "conn-1"}
	pk, err := c.PublicKeyAuth(ctx, &libplugin.PublicKeyAuthRequest{Meta: meta, PublicKey: buyer.Marshal()})
	if err != nil {
		t.Fatal(err)
	}
	up := pk.GetUpstream()
	if up.GetUri() != "tcp://10.1.2.3:47001" || up.GetUserName() != "root" || !bytes.Equal(up.GetPrivateKey().GetPrivateKey(), upKey) {
		t.Fatalf("upstream %v", up)
	}
	if _, err := c.PublicKeyAuth(ctx, &libplugin.PublicKeyAuthRequest{Meta: meta, PublicKey: other.Marshal()}); err == nil {
		t.Fatal("unknown key must be rejected")
	}
	pw, err := c.PasswordAuth(ctx, &libplugin.PasswordAuthRequest{Meta: &libplugin.ConnMeta{UserName: login, FromAddr: "[2001:db8::9]:1",
		UniqId: "conn-2"}, Password: []byte("s3cret")})
	if err != nil {
		t.Fatal(err)
	}
	if pw.GetUpstream().GetUri() != "tcp://[2001:db8::5]:47002" {
		t.Fatalf("ipv6 uri %q", pw.GetUpstream().GetUri())
	}
	if _, err := c.PasswordAuth(ctx, &libplugin.PasswordAuthRequest{Meta: meta, Password: []byte("nope")}); err == nil {
		t.Fatal("wrong password must be rejected")
	}
	bm, err := c.PasswordAuth(ctx, &libplugin.PasswordAuthRequest{Meta: &libplugin.ConnMeta{UserName: login, FromAddr: "198.51.100.9:2",
		UniqId: "conn-3"}, Password: []byte("bare")})
	if err != nil {
		t.Fatal(err)
	}
	if bm.GetUpstream().GetUserName() != "omnis-l-abcdefgh" || bm.GetUpstream().GetUri() != "tcp://10.1.2.4:22" {
		t.Fatalf("bare-metal upstream %v", bm.GetUpstream())
	}
	if _, err := c.PasswordAuth(ctx, &libplugin.PasswordAuthRequest{Meta: &libplugin.ConnMeta{UserName: login, FromAddr: "198.51.100.9:3",
		UniqId: "conn-4"}, Password: []byte("evil")}); err == nil {
		t.Fatal("invalid upstream user must be rejected")
	}

	if r, err := c.VerifyHostKey(ctx, &libplugin.VerifyHostKeyRequest{Meta: meta, Key: host.Marshal(), Hostname: "10.1.2.3:47001"}); err != nil || !r.GetVerified() {
		t.Fatalf("host key: %v %v", r, err)
	}
	if _, err := c.VerifyHostKey(ctx, &libplugin.VerifyHostKeyRequest{Meta: meta, Key: other.Marshal()}); err == nil {
		t.Fatal("wrong upstream host key must be rejected")
	}

	if _, err := c.PipeStartNotice(ctx, &libplugin.PipeStartNoticeRequest{Meta: meta}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.PipeErrorNotice(ctx, &libplugin.PipeErrorNoticeRequest{Meta: meta, Error: "EOF"}); err != nil {
		t.Fatal(err)
	}
	gw.Tick(ctx)
	api.mu.Lock()
	defer api.mu.Unlock()
	if len(api.sessions) != 2 || api.sessions[0].GWSessionID != "conn-1" || api.sessions[1].EndedAt == nil ||
		api.sessions[0].RemoteIP != "198.51.100.9" {
		t.Fatalf("sessions %+v", api.sessions)
	}
}

func TestLoadUpstreamKey(t *testing.T) {
	_, key := genKey(t)
	dir := t.TempDir()
	p := filepath.Join(dir, "k")
	if err := os.WriteFile(p, key, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := loadUpstreamKey(p); err == nil {
		t.Fatal("world-readable key accepted")
	}
	if err := os.Chmod(p, 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := loadUpstreamKey(p); err != nil || !bytes.Equal(got, key) {
		t.Fatalf("load: %v", err)
	}
	if err := os.WriteFile(p, []byte("garbage"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadUpstreamKey(p); err == nil {
		t.Fatal("garbage key accepted")
	}
}
