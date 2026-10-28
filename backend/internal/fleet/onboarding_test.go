package fleet

import (
	"strings"
	"testing"

	"tokenfactory/pkg/errcode"

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

func TestValidateSSHTarget(t *testing.T) {
	ok := []struct {
		host string
		port int
		user string
	}{
		{"203.0.113.7", 22, "root"},
		{"gpu-01.example.com", 2222, "omnis"},
		{"10.8.0.12", 22, "ubuntu"},
		{"2001:db8::7", 22, "_svc"},
	}
	for _, c := range ok {
		assert.NoError(t, validateSSHTarget(c.host, c.port, c.user, false), c.host)
	}
	bad := []struct {
		host string
		port int
		user string
	}{
		{"127.0.0.1", 22, "root"},
		{"::1", 22, "root"},
		{"169.254.169.254", 80, "root"},
		{"0.0.0.0", 22, "root"},
		{"224.0.0.1", 22, "root"},
		{"localhost", 22, "root"},
		{"evil.com;rm -rf /", 22, "root"},
		{"$(id)", 22, "root"},
		{"203.0.113.7", 0, "root"},
		{"203.0.113.7", 70000, "root"},
		{"203.0.113.7", 22, "Root"},
		{"203.0.113.7", 22, "a b"},
		{"203.0.113.7", 22, "root'"},
		{"203.0.113.7", 22, ""},
	}
	for _, c := range bad {
		err := validateSSHTarget(c.host, c.port, c.user, false)
		require.Error(t, err, "%s:%d %s", c.host, c.port, c.user)
		assert.Equal(t, errcode.ParamInvalid, errcode.CodeOf(err, 0))
	}
	assert.NoError(t, validateSSHTarget("127.0.0.1", 2222, "omnis", true), "仅测试放开回环")
	assert.Error(t, validateSSHTarget("169.254.169.254", 22, "omnis", true), "测试开关也不放开链路本地")
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
