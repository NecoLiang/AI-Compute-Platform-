package fleet

import (
	"context"
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/netguard"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFirewallScripts(t *testing.T) {
	s, err := firewallScripts([]string{"47.236.1.10", "2408:4000::1"}, 40000, 40099)
	require.NoError(t, err)
	for _, distro := range []string{"debian", "rhel"} {
		sc := s[distro]
		require.NotEmpty(t, sc)
		assert.Contains(t, sc, "--ctorigdstport 40000:40099 --ctdir ORIGINAL")
		assert.Contains(t, sc, "iptables -I DOCKER-USER -p tcp -m conntrack --ctorigdstport 40000:40099 --ctdir ORIGINAL -s 47.236.1.10 -j RETURN")
		// DROP 先插入, RETURN 后插入(-I 插在链首), 因此 RETURN 位于 DROP 之前生效。
		assert.Less(t, strings.Index(sc, "-j DROP\n"), strings.Index(sc, "-s 47.236.1.10 -j RETURN\n"))
		assert.Contains(t, sc, "-D DOCKER-USER", "先删后加, 重复执行幂等")
	}
	assert.Contains(t, s["debian"], "netfilter-persistent save")
	assert.Contains(t, s["rhel"], "service iptables save")

	_, err = firewallScripts([]string{"1.2.3.4; reboot"}, 40000, 40099)
	assert.Error(t, err)
	_, err = firewallScripts([]string{"1.2.3.4"}, 80, 40099)
	assert.Error(t, err)
	_, err = firewallScripts([]string{"1.2.3.4"}, 40100, 40099)
	assert.Error(t, err)
}

// fakeLookup 测试用解析表; 未登记的域名按解析失败处理(测试不依赖外部 DNS)。
func fakeLookup(m map[string][]string) netguard.Resolver {
	return func(_ context.Context, _, host string) ([]netip.Addr, error) {
		ips, ok := m[host]
		if !ok {
			return nil, &net.DNSError{Err: "no such host", Name: host, IsNotFound: true}
		}
		var out []netip.Addr
		for _, ip := range ips {
			out = append(out, netip.MustParseAddr(ip))
		}
		return out, nil
	}
}

func TestValidateSSHTarget(t *testing.T) {
	ctx := context.Background()
	d, err := newDialer([]string{"47.236.0.0/16", "8.8.4.4"})
	require.NoError(t, err)
	d.lookup = fakeLookup(map[string][]string{
		"gpu-01.example.com":    {"8.8.8.8", "2400:3200::1"},
		"evil.example.com":      {"100.100.100.200"},
		"half-evil.example.com": {"8.8.8.8", "10.0.0.5"},
		"vpc.example.com":       {"47.236.165.75"},
		"mapped.example.com":    {"::ffff:192.168.1.1"},
	})
	ok := []struct {
		host string
		port int
		user string
	}{
		{"8.8.8.8", 22, "root"},
		{"gpu-01.example.com", 2222, "omnis"},
		{"2400:3200::1", 22, "_svc"},
		{"100.128.0.1", 22, "ubuntu"}, // 100.64.0.0/10 之外
	}
	for _, c := range ok {
		assert.NoError(t, validateSSHTarget(ctx, d, c.host, c.port, c.user), c.host)
	}
	bad := []struct {
		host string
		port int
		user string
	}{
		{"127.0.0.1", 22, "root"},
		{"::1", 22, "root"},
		{"169.254.169.254", 80, "root"},
		{"100.100.100.200", 80, "root"}, // 阿里云元数据(CGNAT 段)
		{"100.64.0.1", 22, "root"},
		{"10.8.0.12", 22, "root"},
		{"172.16.5.4", 22, "root"},
		{"192.168.1.1", 22, "root"},
		{"::ffff:10.0.0.1", 22, "root"}, // IPv4-mapped 也按 IPv4 判
		{"fc00::1", 22, "root"},
		{"fd12:3456::1", 22, "root"},
		{"fe80::1", 22, "root"},
		{"2001:db8::7", 22, "root"},
		{"0.0.0.0", 22, "root"},
		{"224.0.0.1", 22, "root"},
		{"47.236.165.75", 22, "root"}, // fleet.deny_cidrs 追加段
		{"8.8.4.4", 22, "root"},       // fleet.deny_cidrs 单 IP
		{"evil.example.com", 22, "root"},
		{"half-evil.example.com", 22, "root"}, // 任一解析结果被拒即整体拒绝
		{"vpc.example.com", 22, "root"},
		{"mapped.example.com", 22, "root"},
		{"nx.example.com", 22, "root"}, // 无法解析
		{"localhost", 22, "root"},
		{"evil.com;rm -rf /", 22, "root"},
		{"$(id)", 22, "root"},
		{"8.8.8.8", 0, "root"},
		{"8.8.8.8", 70000, "root"},
		{"8.8.8.8", 22, "Root"},
		{"8.8.8.8", 22, "a b"},
		{"8.8.8.8", 22, "root'"},
		{"8.8.8.8", 22, ""},
	}
	for _, c := range bad {
		err := validateSSHTarget(ctx, d, c.host, c.port, c.user)
		require.Error(t, err, "%s:%d %s", c.host, c.port, c.user)
		assert.Equal(t, errcode.ParamInvalid, errcode.CodeOf(err, 0))
		// 拒绝提示不回显解析出的地址。
		for _, leak := range []string{"100.100.100.200", "10.0.0.5", "47.236", "192.168"} {
			assert.NotContains(t, err.Error(), leak, c.host)
		}
	}

	// 测试开关只放开回环, 不放开其他任何拒绝段。
	d.allowLoopback = true
	assert.NoError(t, validateSSHTarget(ctx, d, "127.0.0.1", 2222, "omnis"))
	for _, h := range []string{"169.254.169.254", "100.100.100.200", "10.0.0.1", "fc00::1", "47.236.1.1"} {
		assert.Error(t, validateSSHTarget(ctx, d, h, 22, "omnis"), h)
	}
}

func TestDialerControlDeniesAtDialTime(t *testing.T) {
	d, err := newDialer([]string{"203.0.113.0/24"})
	require.NoError(t, err)
	for _, addr := range []string{"127.0.0.1:22", "[::1]:22", "10.0.0.1:22", "100.100.100.200:80", "169.254.169.254:80",
		"[fc00::1]:22", "[::ffff:172.16.0.1]:22", "203.0.113.9:22"} {
		assert.ErrorIs(t, d.control("tcp", addr, nil), ErrAddrNotAllowed, addr)
	}
	assert.NoError(t, d.control("tcp", "8.8.8.8:22", nil))

	// 真实拨号: "localhost" 在拨号时才解析为回环, 由 Control 钩子拦截(DNS rebinding 同理: 以实际连接地址为准)。
	d.Attempts, d.DialTimeout = 1, time.Second
	_, err = d.TCPCheck(context.Background(), "localhost", 22)
	assert.ErrorIs(t, err, ErrAddrNotAllowed)

	// with 复制保留策略。
	c := d.with(time.Second, 1)
	assert.ErrorIs(t, c.control("tcp", "203.0.113.9:22", nil), ErrAddrNotAllowed)

	// 零值 Dialer 也使用内置拒绝名单。
	var z Dialer
	assert.ErrorIs(t, z.control("tcp", "100.100.100.200:80", nil), ErrAddrNotAllowed)

	_, err = newDialer([]string{"not-a-cidr"})
	assert.Error(t, err)
}

func TestAuthorizedKeysLine(t *testing.T) {
	line, err := authorizedKeysLine("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOx omnis-node-7", []string{"47.236.165.75", "8.211.182.0/24"})
	require.NoError(t, err)
	assert.Equal(t, `from="47.236.165.75,8.211.182.0/24",no-agent-forwarding,no-port-forwarding,no-X11-forwarding,no-pty ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOx omnis-node-7`, line)
	_, err = authorizedKeysLine("ssh-ed25519 AAAA x", nil)
	assert.Error(t, err)
	_, err = authorizedKeysLine("ssh-ed25519 AAAA x", []string{`1.2.3.4",command="sh`})
	assert.Error(t, err)
}
