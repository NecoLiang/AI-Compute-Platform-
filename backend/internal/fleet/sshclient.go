package fleet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"
)

// 平台到节点的管理 SSH 通道(golang.org/x/crypto/ssh, 无 agent)。

// ErrHostKeyMismatch 主机指纹与钉死值不一致: 握手阶段即中止, 不发送任何凭证。
var ErrHostKeyMismatch = errors.New("主机指纹与已确认指纹不一致")

// Result 一条远程命令的结果。Err 仅表示传输层失败/超时; 非 0 退出码在 ExitCode。
type Result struct {
	Stdout    string
	Stderr    string
	ExitCode  int
	Truncated bool
}

// Runner 在节点上执行固定模板命令。
type Runner interface {
	Run(ctx context.Context, cmd string) (Result, error)
	Close() error
}

// Dialer 建立管理连接。字段零值使用 docs/25 的默认: 3 次 × 5s 拨号, 单命令 60s, 输出上限 1 MiB。
type Dialer struct {
	DialTimeout time.Duration
	Attempts    int
	CmdTimeout  time.Duration
	MaxOutput   int
	// AllowLoopback 仅测试使用: 允许连接 127.0.0.0/8 上的测试 SSH 服务器。
	AllowLoopback bool
}

// ErrAddrNotAllowed 目标解析到回环/链路本地/组播/未指定地址: 拒绝连接, 防止借探活访问平台自身或云元数据服务。
var ErrAddrNotAllowed = errors.New("目标地址不允许: 不能是回环、链路本地(含云元数据 169.254.0.0/16)、组播或未指定地址")

func (d *Dialer) control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return err
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return ErrAddrNotAllowed
	}
	if ip.IsLoopback() && d.AllowLoopback {
		return nil
	}
	if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() ||
		ip.IsUnspecified() || ip.IsInterfaceLocalMulticast() {
		return ErrAddrNotAllowed
	}
	return nil
}

func (d *Dialer) dialTimeout() time.Duration {
	if d.DialTimeout > 0 {
		return d.DialTimeout
	}
	return 5 * time.Second
}

func (d *Dialer) attempts() int {
	if d.Attempts > 0 {
		return d.Attempts
	}
	return 3
}

// TCPCheck P1: 连接管理端口, 最多 attempts 次, 每次超时 dialTimeout。
func (d *Dialer) TCPCheck(ctx context.Context, host string, port int) (net.Conn, error) {
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	var last error
	for i := 0; i < d.attempts(); i++ {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		dctx, cancel := context.WithTimeout(ctx, d.dialTimeout())
		c, err := (&net.Dialer{Control: d.control}).DialContext(dctx, "tcp", addr)
		cancel()
		if err == nil {
			return c, nil
		}
		last = err
		if errors.Is(err, ErrAddrNotAllowed) {
			return nil, fmt.Errorf("%s: %w", addr, ErrAddrNotAllowed)
		}
	}
	return nil, fmt.Errorf("%d 次连接 %s 均失败: %v", d.attempts(), addr, last)
}

// HandshakeInfo 握手观测值。
type HandshakeInfo struct {
	Fingerprint string // SHA256:…
	KeyType     string // ssh-ed25519 / ecdsa-sha2-nistp256 / rsa-sha2-512 …
}

// Connect 在已建立的 TCP 连接上做 SSH 握手与认证。pinnedFP 非空时指纹不一致即中止(返回 ErrHostKeyMismatch),
// 为空时为首次连接(TOFU): 记录观测指纹交由供给方确认。info 在握手阶段即填充, 认证失败时同样可用。
func (d *Dialer) Connect(conn net.Conn, user string, auth []ssh.AuthMethod, pinnedFP string) (*Client, HandshakeInfo, error) {
	var info HandshakeInfo
	cfg := &ssh.ClientConfig{
		User: user,
		Auth: auth,
		// 优先 ed25519, 与上架说明中 ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub 对应。
		HostKeyAlgorithms: []string{ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384,
			ssh.KeyAlgoECDSA521, ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256},
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			info.Fingerprint, info.KeyType = ssh.FingerprintSHA256(key), key.Type()
			if pinnedFP != "" && info.Fingerprint != pinnedFP {
				return ErrHostKeyMismatch
			}
			return nil
		},
		Timeout: d.dialTimeout(),
	}
	_ = conn.SetDeadline(time.Now().Add(3 * d.dialTimeout()))
	c, chans, reqs, err := ssh.NewClientConn(conn, conn.RemoteAddr().String(), cfg)
	if err != nil {
		_ = conn.Close()
		if errors.Is(err, ErrHostKeyMismatch) {
			return nil, info, ErrHostKeyMismatch
		}
		return nil, info, err
	}
	_ = conn.SetDeadline(time.Time{})
	return &Client{c: ssh.NewClient(c, chans, reqs), d: d}, info, nil
}

// Client 一条管理连接, 可并发开多个会话(P13 压测时并行采样降频)。
type Client struct {
	c *ssh.Client
	d *Dialer
}

func (c *Client) Close() error { return c.c.Close() }

// limitedBuffer 超过上限的输出丢弃并标记截断, 防止节点用超大输出拖垮主服务。
type limitedBuffer struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if room := b.max - b.buf.Len(); room < len(p) {
		if room > 0 {
			b.buf.Write(p[:room])
		}
		b.truncated = true
		return len(p), nil
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// Run 执行一条命令; ctx 无截止时间时套用 CmdTimeout(默认 60s)。
func (c *Client) Run(ctx context.Context, cmd string) (Result, error) {
	if _, ok := ctx.Deadline(); !ok {
		t := c.d.CmdTimeout
		if t <= 0 {
			t = 60 * time.Second
		}
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, t)
		defer cancel()
	}
	max := c.d.MaxOutput
	if max <= 0 {
		max = 1 << 20
	}
	s, err := c.c.NewSession()
	if err != nil {
		return Result{}, err
	}
	defer s.Close()
	stdout, stderr := &limitedBuffer{max: max}, &limitedBuffer{max: 64 << 10}
	s.Stdout, s.Stderr = stdout, stderr
	done := make(chan error, 1)
	go func() { done <- s.Run(cmd) }()
	select {
	case <-ctx.Done():
		_ = s.Signal(ssh.SIGKILL)
		_ = s.Close()
		return Result{Stdout: stdout.String(), Stderr: stderr.String()}, fmt.Errorf("命令超时: %w", ctx.Err())
	case err = <-done:
	}
	res := Result{Stdout: stdout.String(), Stderr: stderr.String(), Truncated: stdout.truncated}
	var exit *ssh.ExitError
	switch {
	case err == nil:
	case errors.As(err, &exit):
		res.ExitCode = exit.ExitStatus()
	default:
		var missing *ssh.ExitMissingError
		if errors.As(err, &missing) {
			res.ExitCode = -1
			return res, nil
		}
		return res, err
	}
	return res, nil
}
