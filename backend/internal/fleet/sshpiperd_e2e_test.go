package fleet

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"tokenfactory/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// 真实 sshpiperd(v1.6.1 官方发布二进制) + failtoban 插件 + 本仓库 cmd/accessgw 插件(go build 产物)的端到端:
// 主服务内部接口(httptest + MySQL + Redis) ← accessgw 插件进程 ← sshpiperd 进程 ← GatewayClient / 买家 ssh 客户端,
// 上游为本机测试 sshd(fakeNode)。插件链与部署一致: failtoban -- accessgw(deploy/accessgw/compose.yml)。
// 需要 SSHPIPERD_DIR(解压后的 sshpiperd_with_plugins 目录, 含 sshpiperd 与 plugins/failtoban)、TEST_MYSQL_DSN、TEST_REDIS_ADDR。
func TestRealSSHPiperdChain(t *testing.T) {
	dir := os.Getenv("SSHPIPERD_DIR")
	if dir == "" {
		t.Skip("set SSHPIPERD_DIR to run the real sshpiperd end-to-end test")
	}
	e := newGWEnv(t, false)
	ctx := context.Background()
	login := e.lease.LeaseNo
	tmp := t.TempDir()

	plugin := filepath.Join(tmp, "accessgw")
	build := exec.Command("go", "build", "-o", plugin, "tokenfactory/cmd/accessgw")
	build.Dir = "../.."
	out, err := build.CombinedOutput()
	require.NoError(t, err, string(out))

	hostKey, hostFile := writeKeyFile(t, tmp, "host_ed25519")
	upKey, upFile := writeKeyFile(t, tmp, "upstream_ed25519")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	const maxFailures = 4
	var logs bytes.Buffer
	cmd := exec.Command(filepath.Join(dir, "sshpiperd"), "--address", "127.0.0.1", "--port", strconv.Itoa(port),
		"--server-key", hostFile, "--log-level", "debug",
		filepath.Join(dir, "plugins", "failtoban"), "--max-failures", strconv.Itoa(maxFailures), "--ban-duration", "1m", "--",
		plugin, "--main-url", e.srv.URL, "--upstream-key-file", upFile)
	cmd.Env = append(os.Environ(), "ACCESSGW_HMAC_SECRET="+gwTestSecret)
	cmd.Stdout, cmd.Stderr = &logs, &logs
	require.NoError(t, cmd.Start())
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		if t.Failed() {
			t.Logf("sshpiperd output:\n%s", logs.String())
		}
	})
	require.Eventually(t, func() bool {
		c, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 200*time.Millisecond)
		if err == nil {
			_ = c.Close()
		}
		return err == nil
	}, 20*time.Second, 100*time.Millisecond, "sshpiperd did not start listening")

	ctr := newFakeNode(t, "") // 租约容器 sshd: 只接受网关上游公钥
	ctr.Authorize(upKey.PublicKey())
	ctr.Override("nvidia-smi -L", fakeReply{Stdout: gpuListL(fakeUUID0, fakeUUID1)})
	ctr.Override("echo hello", fakeReply{Stdout: "hello\n"})

	gc, err := NewGatewayClient(config.AccessGWConfig{PublicHost: "127.0.0.1", PublicPort: port,
		HostKeyFingerprint: fpOf(hostKey)}, e.sys, e.rdb)
	require.NoError(t, err)

	// ① 开通 ⑤: 系统自检身份经真实 sshpiperd 登录 provisioning 租约。
	e.db.MustExec(`UPDATE leases SET state='provisioning', upstream_port=?, upstream_host_key_fp=? WHERE id=?`,
		ctr.Port(), ctr.Fingerprint(), e.lease.ID)
	got, err := gc.SelfTestLease(ctx, login)
	require.NoError(t, err)
	assert.Equal(t, gpuListL(fakeUUID0, fakeUUID1), got)
	// 上游主机密钥与租约记录不符 → 插件拒绝建立管道。
	e.db.MustExec(`UPDATE leases SET upstream_host_key_fp='SHA256:0000000000000000000000000000000000000000000' WHERE id=?`, e.lease.ID)
	_, err = gc.SelfTestLease(ctx, login)
	require.Error(t, err)

	// ② P11: 真实插件进程没有任何放开回环的开关, 探活目标 127.0.0.1 必须被网关侧拒绝名单拦下。
	open, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	defer open.Close()
	err = gc.SelfTestPort(ctx, e.node, "127.0.0.1", open.Addr().(*net.TCPAddr).Port)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "网关无法连接") // 插件确实执行并回报了结果(不是等待超时)
	assert.Contains(t, err.Error(), "不能指向回环")

	// ③ 开通完成后买家登录(公钥 / 访问口令)。
	e.db.MustExec(`UPDATE leases SET state='pending', upstream_port=NULL, upstream_host_key_fp=NULL WHERE id=?`, e.lease.ID)
	e.outcome(t, runOne(t, e.s))
	e.db.MustExec(`UPDATE leases SET upstream_port=?, upstream_host_key_fp=? WHERE id=?`, ctr.Port(), ctr.Fingerprint(), e.lease.ID)
	_, err = gc.SelfTestLease(ctx, login)
	require.Error(t, err, "system identity must not log into an active lease")

	buyer := newSigner(t)
	e.addBuyerKey(t, 21, buyer)
	pw, err := e.s.products.ManagedAccessValue(e.db, e.order.ID)
	require.NoError(t, err)
	exec := func(auth ssh.AuthMethod) (string, error) {
		cl, err := ssh.Dial("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), &ssh.ClientConfig{User: login,
			Auth: []ssh.AuthMethod{auth}, HostKeyCallback: ssh.FixedHostKey(hostKey.PublicKey()), Timeout: 5 * time.Second})
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
	got, err = exec(ssh.PublicKeys(buyer))
	require.NoError(t, err)
	assert.Equal(t, "hello\n", got)
	got, err = exec(ssh.Password(pw))
	require.NoError(t, err)
	assert.Equal(t, "hello\n", got)

	// ④ 会话审计: 插件进程内的 Gateway.Run 每 5s 上报一次, 两条买家会话均应写入且已结束。
	require.Eventually(t, func() bool {
		var n int
		require.NoError(t, e.db.Get(&n, `SELECT COUNT(*) FROM access_sessions WHERE lease_id=? AND ended_at IS NOT NULL`, e.lease.ID))
		return n == 2
	}, 20*time.Second, 250*time.Millisecond)
	var methods []string
	require.NoError(t, e.db.Select(&methods, `SELECT auth_method FROM access_sessions WHERE lease_id=? ORDER BY id`, e.lease.ID))
	assert.Equal(t, []string{"publickey", "password"}, methods)

	// ⑤ failtoban: 同一来源连续认证失败达到上限后, 正确口令也被拒绝(连接在认证前即被断开)。
	_, err = exec(ssh.PublicKeys(newSigner(t)))
	require.Error(t, err)
	banned := false
	for i := 0; i < maxFailures+2 && !banned; i++ {
		_, err = exec(ssh.Password(pw + "x"))
		require.Error(t, err)
		_, err = exec(ssh.Password(pw))
		banned = err != nil
	}
	assert.True(t, banned, "failtoban must reject the source after %d failures", maxFailures)
}

// writeKeyFile 生成 ed25519 私钥并以 OpenSSH 格式写入 0600 文件(与部署时 ssh-keygen 生成的文件同格式)。
func writeKeyFile(t *testing.T, dir, name string) (ssh.Signer, string) {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	block, err := ssh.MarshalPrivateKey(priv, "")
	require.NoError(t, err)
	p := filepath.Join(dir, name)
	require.NoError(t, os.WriteFile(p, pem.EncodeToMemory(block), 0o600))
	s, err := ssh.NewSignerFromKey(priv)
	require.NoError(t, err)
	return s, p
}
