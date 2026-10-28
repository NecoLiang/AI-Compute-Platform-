package fleet

import (
	"context"
	"encoding/hex"
	"io"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"tokenfactory/pkg/accessgw"
	"tokenfactory/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// 全链路(T-085): GatewayClient(主服务系统自检) / 买家 ssh 客户端 → miniPiper → accessgw.Gateway → 主服务内部接口
// (httptest + MySQL + Redis) → 上游"容器 sshd"(fakeNode)。
//
// miniPiper 只在测试中使用: 用 x/crypto/ssh 按 sshpiperd 的回调时序驱动 accessgw.Gateway(认证回调 → 连接上游并校验
// 上游主机密钥 → PipeStart → 双向转发 → PipeEnd)。真实 sshpiperd(v1.6.1)+cmd/accessgw 插件的端到端联调未在本机验收。
type miniPiper struct {
	t       *testing.T
	ln      net.Listener
	gw      *accessgw.Gateway
	hostKey ssh.Signer
	upKey   ssh.Signer
}

func newMiniPiper(t *testing.T, gw *accessgw.Gateway, upKey ssh.Signer) *miniPiper {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	p := &miniPiper{t: t, ln: ln, gw: gw, hostKey: newSigner(t), upKey: upKey}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go p.handle(c)
		}
	}()
	t.Cleanup(func() { _ = ln.Close() })
	return p
}

func (p *miniPiper) Port() int { return p.ln.Addr().(*net.TCPAddr).Port }

func (p *miniPiper) handle(c net.Conn) {
	defer c.Close()
	ctx := context.Background()
	cfg := &ssh.ServerConfig{
		PublicKeyCallback: func(m ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			_, err := p.gw.AuthPublicKey(ctx, hex.EncodeToString(m.SessionID()), m.User(), m.RemoteAddr().String(), key.Marshal())
			return nil, err
		},
		PasswordCallback: func(m ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			_, err := p.gw.AuthPassword(ctx, hex.EncodeToString(m.SessionID()), m.User(), m.RemoteAddr().String(), pw)
			return nil, err
		},
	}
	cfg.AddHostKey(p.hostKey)
	sc, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	defer sc.Close()
	id := hex.EncodeToString(sc.SessionID())
	route, ok := p.gw.Upstream(id)
	if !ok {
		p.t.Logf("miniPiper: no upstream route for %s", id)
		return
	}
	up, err := ssh.Dial("tcp", net.JoinHostPort(route.Host, strconv.Itoa(route.Port)), &ssh.ClientConfig{
		User: "root", Auth: []ssh.AuthMethod{ssh.PublicKeys(p.upKey)}, Timeout: 5 * time.Second,
		HostKeyCallback: func(_ string, _ net.Addr, k ssh.PublicKey) error { return p.gw.VerifyHostKey(id, k.Marshal()) },
	})
	if err != nil {
		p.t.Logf("miniPiper: upstream dial %s:%d: %v", route.Host, route.Port, err) // sshpiperd: PipeCreateErrorCallback
		return
	}
	defer up.Close()
	p.gw.PipeStart(id)
	go ssh.DiscardRequests(reqs)
	go func() {
		for nc := range chans {
			uch, ureqs, err := up.OpenChannel(nc.ChannelType(), nc.ExtraData())
			if err != nil {
				_ = nc.Reject(ssh.ConnectionFailed, err.Error())
				continue
			}
			ch, creqs, err := nc.Accept()
			if err != nil {
				_ = uch.Close()
				continue
			}
			go relayChannel(ch, creqs, uch, ureqs)
		}
	}()
	werr := sc.Wait()
	p.gw.PipeEnd(id, werr)
}

func relayChannel(ch ssh.Channel, creqs <-chan *ssh.Request, uch ssh.Channel, ureqs <-chan *ssh.Request) {
	// reqMu: 上游可能在 exec 应答转发给客户端之前就发完 exit-status 并关闭通道; 关闭下游通道前须等在途请求的
	// 应答转发完, 否则客户端收到 EOF(负载高时偶发)。真实 sshpiperd 按报文顺序转发, 无此问题。
	var reqMu sync.Mutex
	defer func() {
		reqMu.Lock()
		_ = ch.Close()
		reqMu.Unlock()
	}()
	defer uch.Close()
	go func() {
		_, _ = io.Copy(uch, ch)
		_ = uch.CloseWrite()
	}()
	go func() {
		for r := range creqs {
			reqMu.Lock()
			ok, _ := uch.SendRequest(r.Type, r.WantReply, r.Payload)
			if r.WantReply {
				_ = r.Reply(ok, nil)
			}
			reqMu.Unlock()
		}
	}()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.Copy(ch, uch) }()
	go func() { defer wg.Done(); _, _ = io.Copy(ch.Stderr(), uch.Stderr()) }()
	for r := range ureqs {
		ok, _ := ch.SendRequest(r.Type, r.WantReply, r.Payload)
		if r.WantReply {
			_ = r.Reply(ok, nil)
		}
	}
	wg.Wait()
}

func TestGatewayFullChain(t *testing.T) {
	e := newGWEnv(t, false)
	ctx := context.Background()
	login := e.lease.LeaseNo

	upKey := newSigner(t)
	ctr := newFakeNode(t, "") // 租约容器 sshd: 只接受网关上游公钥
	ctr.Authorize(upKey.PublicKey())
	ctr.Override("nvidia-smi -L", fakeReply{Stdout: gpuListL(fakeUUID0, fakeUUID1)})
	ctr.Override("echo hello", fakeReply{Stdout: "hello\n"})

	gw := accessgw.NewGateway(e.client, nil)
	gw.AllowLoopbackForTest = true // 假节点在 127.0.0.1
	pp := newMiniPiper(t, gw, upKey)
	gwCfg := config.AccessGWConfig{PublicHost: "127.0.0.1", PublicPort: pp.Port(), HostKeyFingerprint: fpOf(pp.hostKey)}
	gc, err := NewGatewayClient(gwCfg, e.sys, e.rdb)
	require.NoError(t, err)
	assert.Equal(t, fpOf(e.sys), gc.SelftestFingerprint())

	// ① 开通 ⑤: provisioning 租约, 系统自检身份经网关执行 nvidia-smi -L。
	e.db.MustExec(`UPDATE leases SET state='provisioning', upstream_port=?, upstream_host_key_fp=? WHERE id=?`,
		ctr.Port(), ctr.Fingerprint(), e.lease.ID)
	out, err := gc.SelfTestLease(ctx, login)
	require.NoError(t, err)
	assert.Equal(t, gpuListL(fakeUUID0, fakeUUID1), out)

	// 上游主机密钥与租约记录不符 → 网关拒绝建立管道。
	e.db.MustExec(`UPDATE leases SET upstream_host_key_fp='SHA256:0000000000000000000000000000000000000000000' WHERE id=?`, e.lease.ID)
	_, err = gc.SelfTestLease(ctx, login)
	require.Error(t, err)

	// ② P11: 网关从自身出口对节点临时端口做 TCP 连接。
	open, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer open.Close()
	require.NoError(t, gc.SelfTestPort(ctx, e.node, "127.0.0.1", open.Addr().(*net.TCPAddr).Port))
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	closedPort := closed.Addr().(*net.TCPAddr).Port
	require.NoError(t, closed.Close())
	err = gc.SelfTestPort(ctx, e.node, "127.0.0.1", closedPort)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "网关无法连接")
	keys, err := e.rdb.Keys(ctx, "accessgw:probe:*").Result()
	require.NoError(t, err)
	assert.Empty(t, keys, "probe task and result must be cleaned up")

	// 网关主机密钥固定: 指纹不符直接失败(不会把探活/自检结果当真)。
	bad, err := NewGatewayClient(config.AccessGWConfig{PublicHost: "127.0.0.1", PublicPort: pp.Port(),
		HostKeyFingerprint: "SHA256:0000000000000000000000000000000000000000000"}, e.sys, e.rdb)
	require.NoError(t, err)
	err = bad.SelfTestPort(ctx, e.node, "127.0.0.1", open.Addr().(*net.TCPAddr).Port)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "网关主机密钥指纹不符")
	// 网关入口不可达。
	down, err := NewGatewayClient(config.AccessGWConfig{PublicHost: "127.0.0.1", PublicPort: closedPort,
		HostKeyFingerprint: gwCfg.HostKeyFingerprint}, e.sys, e.rdb)
	require.NoError(t, err)
	_, err = down.SelfTestLease(ctx, login)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "连接网关入口")

	// ③ 开通完成(网关自检用桩), 把上游指向 ctr; 系统身份不能再登录 active 租约。
	e.db.MustExec(`UPDATE leases SET state='pending', upstream_port=NULL, upstream_host_key_fp=NULL WHERE id=?`, e.lease.ID)
	e.outcome(t, runOne(t, e.s))
	e.db.MustExec(`UPDATE leases SET upstream_port=?, upstream_host_key_fp=? WHERE id=?`, ctr.Port(), ctr.Fingerprint(), e.lease.ID)
	_, err = gc.SelfTestLease(ctx, login)
	require.Error(t, err)
	gw.Tick(ctx) // 取得撤销游标起点

	buyer := newSigner(t)
	e.addBuyerKey(t, 21, buyer)
	pw, err := e.s.products.ManagedAccessValue(e.db, e.order.ID)
	require.NoError(t, err)
	exec := func(auth ssh.AuthMethod) (string, error) {
		cl, err := ssh.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(pp.Port())), &ssh.ClientConfig{User: login,
			Auth: []ssh.AuthMethod{auth}, HostKeyCallback: ssh.FixedHostKey(pp.hostKey.PublicKey()), Timeout: 5 * time.Second})
		if err != nil {
			return "", err
		}
		defer cl.Close()
		s, err := cl.NewSession()
		if err != nil {
			return "", err
		}
		defer s.Close()
		b, err := s.Output("echo hello")
		return string(b), err
	}
	out, err = exec(ssh.PublicKeys(buyer))
	require.NoError(t, err)
	assert.Equal(t, "hello\n", out)
	out, err = exec(ssh.Password(pw))
	require.NoError(t, err)
	assert.Equal(t, "hello\n", out)
	_, err = exec(ssh.Password(pw + "x"))
	require.Error(t, err)
	_, err = exec(ssh.PublicKeys(newSigner(t)))
	require.Error(t, err)

	// ④ 会话审计: 两条买家会话(系统自检不记), 结束信息已回写。
	// 两条会话各有开始+结束事件; 上游主机密钥不符那次连接已认证但没有建立管道, 不产生事件(10 分钟后由 gc 清理)。
	require.Eventually(t, func() bool { _, _, q, _ := gw.Stats(); return q == 4 }, 5*time.Second, 50*time.Millisecond)
	gw.Tick(ctx)
	var rows []struct {
		Method string  `db:"auth_method"`
		IP     string  `db:"remote_ip"`
		FP     *string `db:"key_fingerprint"`
		Ended  bool    `db:"ended"`
		Reason *string `db:"end_reason"`
	}
	require.NoError(t, e.db.Select(&rows, `SELECT auth_method, remote_ip, key_fingerprint, ended_at IS NOT NULL ended, end_reason
		FROM access_sessions WHERE lease_id=? ORDER BY id`, e.lease.ID))
	require.Len(t, rows, 2)
	assert.Equal(t, "publickey", rows[0].Method)
	require.NotNil(t, rows[0].FP)
	assert.Equal(t, fpOf(buyer), *rows[0].FP)
	assert.Equal(t, "password", rows[1].Method)
	assert.Nil(t, rows[1].FP)
	for _, r := range rows {
		assert.Equal(t, "127.0.0.1", r.IP)
		assert.True(t, r.Ended)
		require.NotNil(t, r.Reason)
	}

	// ⑤ 撤销: 删除公钥并刷新游标; 缓存的放行最多保留到撤销被拉取(≤ 轮询周期 + 2s 滞后), 之后拒绝。
	e.db.MustExec(`UPDATE buyer_ssh_keys SET revoked_at=NOW() WHERE fingerprint=?`, fpOf(buyer))
	require.NoError(t, e.s.repo.touchBuyerLeases(e.db, 21))
	_, err = exec(ssh.PublicKeys(buyer))
	require.NoError(t, err, "allow is still cached until the revocation poll")
	time.Sleep(3200 * time.Millisecond)
	gw.Tick(ctx)
	_, err = exec(ssh.PublicKeys(buyer))
	require.Error(t, err)
	assert.True(t, strings.Contains(err.Error(), "unable to authenticate"), err.Error())
	out, err = exec(ssh.Password(pw))
	require.NoError(t, err, "password access is unaffected by key deletion")
	assert.Equal(t, "hello\n", out)
}
