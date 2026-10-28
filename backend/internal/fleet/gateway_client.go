package fleet

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"tokenfactory/pkg/accessgw"
	"tokenfactory/pkg/config"

	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/ssh"
)

// GatewayClient GatewaySelfTester 的真实实现: 主服务以"系统自检身份"(fleet.selftest_key_file)从网关公网入口
// 登录, 走与买家完全相同的路径(docs/25 §25.8 ⑤ 与 P11)。网关主机密钥按 accessgw.host_key_fingerprint 固定。
type GatewayClient struct {
	addr      string
	hostKeyFP string
	signer    ssh.Signer
	rdb       *redis.Client
	// probeWait 网关回报 P11 结果的最长等待时间。
	probeWait time.Duration
	dial      time.Duration
	cmdTO     time.Duration
}

// LoadSelftestSigner 读取系统自检私钥: 必须是 ed25519, 文件不可被组/其他用户读取。
func LoadSelftestSigner(path string) (ssh.Signer, error) {
	st, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("读取 fleet.selftest_key_file 失败: %w", err)
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("fleet.selftest_key_file 权限过宽(%o), 须为 0400 或 0600", st.Mode().Perm())
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 fleet.selftest_key_file 失败: %w", err)
	}
	key, err := ssh.ParseRawPrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("fleet.selftest_key_file 不是可用的 OpenSSH 私钥(不支持口令保护): %w", err)
	}
	var signer ssh.Signer
	switch k := key.(type) {
	case ed25519.PrivateKey:
		signer, err = ssh.NewSignerFromKey(k)
	case *ed25519.PrivateKey:
		signer, err = ssh.NewSignerFromKey(*k)
	default:
		return nil, errors.New("fleet.selftest_key_file 须为 ed25519 私钥")
	}
	if err != nil {
		return nil, err
	}
	return signer, nil
}

// NewGatewayClient gw.HostKeyFingerprint 为网关自身 SSH 主机密钥指纹(SHA256:...), rdb 用于下发 P11 任务与读取结果。
func NewGatewayClient(gw config.AccessGWConfig, signer ssh.Signer, rdb *redis.Client) (*GatewayClient, error) {
	if gw.PublicHost == "" || gw.PublicPort <= 0 {
		return nil, errors.New("accessgw.public_host/public_port 未配置")
	}
	if !validFingerprint(gw.HostKeyFingerprint) {
		return nil, errors.New("accessgw.host_key_fingerprint 未配置或格式非法(应为 SHA256:...)")
	}
	if signer == nil || rdb == nil {
		return nil, errors.New("网关自检需要系统自检私钥与 Redis")
	}
	return &GatewayClient{addr: net.JoinHostPort(gw.PublicHost, strconv.Itoa(gw.PublicPort)), hostKeyFP: gw.HostKeyFingerprint,
		signer: signer, rdb: rdb, probeWait: 10 * time.Second, dial: 10 * time.Second, cmdTO: 60 * time.Second}, nil
}

// SelftestFingerprint 系统自检公钥指纹, 主服务内部接口据此识别系统身份。
func (c *GatewayClient) SelftestFingerprint() string {
	return ssh.FingerprintSHA256(c.signer.PublicKey())
}

func (c *GatewayClient) sshConfig(login string) *ssh.ClientConfig {
	return &ssh.ClientConfig{
		User: login,
		Auth: []ssh.AuthMethod{ssh.PublicKeys(c.signer)},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if got := ssh.FingerprintSHA256(key); got != c.hostKeyFP {
				return &gatewayHostKeyError{want: c.hostKeyFP, got: got}
			}
			return nil
		},
		Timeout: c.dial,
	}
}

func (c *GatewayClient) connect(ctx context.Context, login string) (*ssh.Client, error) {
	d := net.Dialer{Timeout: c.dial}
	conn, err := d.DialContext(ctx, "tcp", c.addr)
	if err != nil {
		return nil, &gatewayDialError{addr: c.addr, err: err}
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	} else {
		_ = conn.SetDeadline(time.Now().Add(c.dial + c.cmdTO))
	}
	sc, chans, reqs, err := ssh.NewClientConn(conn, c.addr, c.sshConfig(login))
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	return ssh.NewClient(sc, chans, reqs), nil
}

// SelfTestLease 开通 ⑤: 以系统自检身份经网关登录 provisioning 租约, 执行 nvidia-smi -L 并返回输出。
func (c *GatewayClient) SelfTestLease(ctx context.Context, leaseNo string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, c.dial+c.cmdTO)
	defer cancel()
	cl, err := c.connect(ctx, leaseNo)
	if err != nil {
		return "", err
	}
	defer cl.Close()
	sess, err := cl.NewSession()
	if err != nil {
		return "", fmt.Errorf("经网关打开会话失败: %w", err)
	}
	defer sess.Close()
	var out, errb bytes.Buffer
	sess.Stdout, sess.Stderr = &out, &errb
	if err := sess.Run("nvidia-smi -L"); err != nil {
		return "", fmt.Errorf("经网关执行 nvidia-smi -L 失败: %v: %s", err, truncate(errb.String(), 300))
	}
	return out.String(), nil
}

// RecheckLease 节点重启对账(T-087): 在 Redis 登记复检许可后以系统自检身份经网关登录 active 租约,
// 执行 nvidia-smi -L 并返回输出; 结束即删除许可。
func (c *GatewayClient) RecheckLease(ctx context.Context, leaseNo string) (string, error) {
	if !ValidLeaseNo(leaseNo) {
		return "", fmt.Errorf("租约号非法: %q", leaseNo)
	}
	if err := c.rdb.Set(ctx, recheckKey(leaseNo), "1", gwRecheckTTL).Err(); err != nil {
		return "", fmt.Errorf("登记复检许可失败(Redis): %w", err)
	}
	defer c.rdb.Del(context.WithoutCancel(ctx), recheckKey(leaseNo))
	return c.SelfTestLease(ctx, leaseNo)
}

// SelfTestPort P11: 在 Redis 登记一次性探活任务, 以系统身份登录临时用户名 p-<token>; 网关识别后从其出口对
// host:port 做 TCP 连接并回报结果, 然后拒绝该登录。这里等待并读取回报。
func (c *GatewayClient) SelfTestPort(ctx context.Context, nodeID int64, host string, port int) error {
	token, err := accessgw.NewNonce()
	if err != nil {
		return err
	}
	task, _ := json.Marshal(accessgw.ProbeTask{Host: host, Port: port})
	if err := c.rdb.Set(ctx, probeKey(token), task, gwProbeTTL).Err(); err != nil {
		return fmt.Errorf("登记探活任务失败(Redis): %w", err)
	}
	defer c.rdb.Del(context.WithoutCancel(ctx), probeKey(token), probeResultKey(token))

	cl, err := c.connect(ctx, "p-"+token)
	if err == nil {
		// 网关必须拒绝探活登录; 放行说明网关版本与主服务不一致。
		_ = cl.Close()
		return errors.New("网关放行了探活登录(网关版本与主服务内部接口不一致)")
	}
	var de *gatewayDialError
	var he *gatewayHostKeyError
	if errors.As(err, &de) || errors.As(err, &he) {
		return err
	}
	// 其余情况(预期是认证失败; 也可能是网关在拒绝后直接断开、主机密钥不符)都以回报结果为准。
	sshErr := err
	deadline := time.Now().Add(c.probeWait)
	for {
		raw, err := c.rdb.Get(ctx, probeResultKey(token)).Bytes()
		if err == nil {
			var r accessgw.ProbeResult
			if err := json.Unmarshal(raw, &r); err != nil {
				return fmt.Errorf("探活结果无法解析: %w", err)
			}
			if !r.Reachable {
				return fmt.Errorf("网关无法连接 %s:%d: %s", host, port, r.Error)
			}
			return nil
		}
		if !errors.Is(err, redis.Nil) {
			return fmt.Errorf("读取探活结果失败(Redis): %w", err)
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("网关未在时限内回报探活结果(登录结果: %v)", sshErr)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}

type gatewayDialError struct {
	addr string
	err  error
}

func (e *gatewayDialError) Error() string {
	return fmt.Sprintf("连接网关入口 %s 失败: %v", e.addr, e.err)
}
func (e *gatewayDialError) Unwrap() error { return e.err }

type gatewayHostKeyError struct{ want, got string }

func (e *gatewayHostKeyError) Error() string {
	return fmt.Sprintf("网关主机密钥指纹不符(期望 %s, 实际 %s)", e.want, e.got)
}
