package fleet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"syscall"
	"time"

	"tokenfactory/pkg/netguard"

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
// 目标地址策略(policy)为 nil 时使用 netguard 内置拒绝名单; 生产由 NewService 按 fleet.deny_cidrs 构造。
// 复制 Dialer(d := *s.dialer)会连同策略一起复制, 改超时/次数时请用复制而不是新建零值 Dialer。
type Dialer struct {
	DialTimeout time.Duration
	Attempts    int
	CmdTimeout  time.Duration
	MaxOutput   int

	policy *netguard.Policy
	lookup netguard.Resolver // nil = 系统解析器
	// allowLoopback 仅测试(_test.go)置位: 允许连接 127.0.0.0/8、::1 上的测试 SSH 服务器; 不放开其他任何拒绝段。
	// 刻意不导出、无配置项, 生产代码无法打开。
	allowLoopback bool
}

// newDialer 生产构造: 内置拒绝名单 + extraDeny(fleet.deny_cidrs)。
func newDialer(extraDeny []string) (*Dialer, error) {
	p, err := netguard.New(extraDeny)
	if err != nil {
		return nil, err
	}
	return &Dialer{policy: p}, nil
}

// ErrAddrNotAllowed 目标(含域名解析结果)落在拒绝名单内: 拒绝连接, 防止借探活访问平台自身、内网或云元数据服务。
var ErrAddrNotAllowed = netguard.ErrDenied

func (d *Dialer) pol() *netguard.Policy {
	if d.policy != nil {
		return d.policy
	}
	return defaultPolicy
}

var defaultPolicy = netguard.Default()

func (d *Dialer) checkAddr(a netip.Addr) error {
	a = a.Unmap()
	if d.allowLoopback && a.IsLoopback() {
		return nil
	}
	return d.pol().CheckAddr(a)
}

func (d *Dialer) control(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return ErrAddrNotAllowed
	}
	a, err := netip.ParseAddr(host)
	if err != nil {
		return ErrAddrNotAllowed
	}
	return d.checkAddr(a)
}

// resolveAllowed 提交时校验: IP 字面量直接按策略判断, 域名解析后任一地址被拒即整体拒绝。
// 返回第一个地址(交给网关作为上游/探活目标的 IP 字面量, 网关侧不再自行解析)。
func (d *Dialer) resolveAllowed(ctx context.Context, host string) (netip.Addr, error) {
	if a, err := netip.ParseAddr(host); err == nil {
		if err := d.checkAddr(a); err != nil {
			return netip.Addr{}, err
		}
		return a.Unmap(), nil
	}
	lookup := d.lookup
	if lookup == nil {
		lookup = net.DefaultResolver.LookupNetIP
	}
	rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := lookup(rctx, "ip", host)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("%w: %v", netguard.ErrUnresolvable, err)
	}
	if len(addrs) == 0 {
		return netip.Addr{}, netguard.ErrUnresolvable
	}
	for _, a := range addrs {
		if err := d.checkAddr(a); err != nil {
			return netip.Addr{}, err
		}
	}
	return addrs[0].Unmap(), nil
}

// with 复制一份(保留目标地址策略与测试开关)并改拨号超时/次数。
func (d *Dialer) with(timeout time.Duration, attempts int) Dialer {
	c := *d
	c.DialTimeout, c.Attempts = timeout, attempts
	return c
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
