package fleet

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// T-084 开通命令模板: 白名单、注入防护、脚本语法与幂等语义。

var b64Re = regexp.MustCompile(`echo ([A-Za-z0-9+/=]+) \| base64 -d \| sh`)

// decodeB64 取出命令中 base64 交给 sh 的脚本。
func decodeB64(t *testing.T, cmd string) string {
	t.Helper()
	m := b64Re.FindStringSubmatch(cmd)
	require.NotNil(t, m, "命令中应有 base64 脚本: %s", cmd)
	raw, err := base64.StdEncoding.DecodeString(m[1])
	require.NoError(t, err)
	return string(raw)
}

func shSyntaxOK(t *testing.T, script string) {
	t.Helper()
	out, err := exec.Command("sh", "-n", "-c", script).CombinedOutput()
	require.NoError(t, err, "sh -n: %s", out)
}

func TestFirewallCommandValidation(t *testing.T) {
	_, err := firewallCommand("ubuntu:latest", []string{"10.0.0.2"}, 40000, 40099)
	assert.Error(t, err, "镜像必须按 digest 引用")
	_, err = firewallCommand(testProbeImage, nil, 40000, 40099)
	assert.ErrorContains(t, err, "egress_ips 未配置")
	_, err = firewallCommand(testProbeImage, []string{"2001:db8::1"}, 40000, 40099)
	assert.ErrorContains(t, err, "只支持 IPv4")
	_, err = firewallCommand(testProbeImage, []string{"10.0.0.2; reboot"}, 40000, 40099)
	assert.Error(t, err)
	_, err = firewallCommand(testProbeImage, []string{"10.0.0.2"}, 40099, 40000)
	assert.Error(t, err)
	_, err = firewallCommand(testProbeImage, []string{"10.0.0.2"}, 80, 40000)
	assert.Error(t, err)

	cmd, err := firewallCommand(testProbeImage, []string{"10.0.0.2", "10.0.0.3"}, 40000, 40099)
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(cmd, "docker run --rm --network host --cap-drop ALL --cap-add NET_ADMIN --cap-add NET_RAW "))
	assert.Contains(t, cmd, " "+testProbeImage+" -c '")
	script := decodeB64(t, cmd)
	assert.Contains(t, script, "--ctorigdstport 40000:40099 --ctdir ORIGINAL")
	assert.Contains(t, script, "IPS='10.0.0.2 10.0.0.3'\nN=3\n")
	shSyntaxOK(t, script)
}

// fakeIptables 用一个状态文件模拟 iptables 的 DOCKER-USER 链(-S/-C/-D/-I), 验证规则脚本的幂等与自愈。
const fakeIptables = `#!/bin/sh
S="$FAKE_IPT_STATE"
[ "$1" = "-w" ] && shift
op=$1; chain=$2; shift 2
[ "$chain" = DOCKER-USER ] || exit 1
[ -f "$S" ] || { echo "iptables: No chain/target/match by that name." >&2; exit 1; }
case $op in
  -S) echo "-N DOCKER-USER"; while IFS= read -r l; do echo "-A DOCKER-USER $l"; done < "$S" ;;
  -C) grep -qxF -- "$*" "$S" ;;
  -D) n=$(grep -nxF -- "$*" "$S" | head -n 1 | cut -d: -f1); [ -n "$n" ] || exit 1
      sed "${n}d" "$S" > "$S.new" && mv "$S.new" "$S" ;;
  -I) shift; { printf '%s\n' "$*"; cat "$S"; } > "$S.new" && mv "$S.new" "$S" ;;
  *) exit 2 ;;
esac
`

func TestFirewallScriptIdempotent(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "iptables-nft"), []byte(fakeIptables), 0o755))
	state := filepath.Join(dir, "state")
	script, err := firewallApplyScript([]string{"10.0.0.2", "10.0.0.3"}, 40000, 40099)
	require.NoError(t, err)
	run := func() (string, int) {
		c := exec.Command("sh", "-c", script)
		c.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "FAKE_IPT_STATE=" + state}
		out, err := c.CombinedOutput()
		code := 0
		if ee, ok := err.(*exec.ExitError); ok {
			code = ee.ExitCode()
		} else {
			require.NoError(t, err)
		}
		return string(out), code
	}
	rules := func() []string {
		raw, err := os.ReadFile(state)
		require.NoError(t, err)
		return strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	}
	m := "-p tcp -m conntrack --ctorigdstport 40000:40099 --ctdir ORIGINAL"
	// 逐条插到链首, 放行规则顺序与配置相反(互不重叠, 顺序无关), DROP 必须在它们之后。
	want := []string{m + " -s 10.0.0.3 -j RETURN", m + " -s 10.0.0.2 -j RETURN", m + " -j DROP",
		"-p tcp --dport 9999 -j ACCEPT", "-j RETURN"}

	// 没有 DOCKER-USER 链: 明确失败。
	out, code := run()
	assert.Equal(t, 3, code)
	assert.Contains(t, out, "@@nochain")

	// 首次下发: 供给方已有的其他规则与 Docker 默认 RETURN 保持在后面。
	require.NoError(t, os.WriteFile(state, []byte("-p tcp --dport 9999 -j ACCEPT\n-j RETURN\n"), 0o644))
	out, code = run()
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "@@applied iptables-nft")
	assert.Equal(t, want, rules())

	// 再次执行: 已就位, 不改动。
	out, code = run()
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "@@present iptables-nft")
	assert.Equal(t, want, rules())

	// 被人删掉 DROP、残留旧网关 IP 的放行: 清掉本端口段全部旧规则后重建。
	require.NoError(t, os.WriteFile(state, []byte(m+" -s 9.9.9.9 -j RETURN\n"+m+" -s 10.0.0.2 -j RETURN\n"+
		m+" -s 10.0.0.3 -j RETURN\n-p tcp --dport 9999 -j ACCEPT\n-j RETURN\n"), 0o644))
	out, code = run()
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "@@applied iptables-nft")
	assert.Equal(t, want, rules())

	// DROP 跑到放行规则前面(顺序错误)也会被纠正。
	require.NoError(t, os.WriteFile(state, []byte(m+" -j DROP\n"+m+" -s 10.0.0.2 -j RETURN\n"+
		m+" -s 10.0.0.3 -j RETURN\n-p tcp --dport 9999 -j ACCEPT\n-j RETURN\n"), 0o644))
	out, code = run()
	require.Equal(t, 0, code, out)
	assert.Contains(t, out, "@@applied")
	assert.Equal(t, want, rules())
}

func TestContainerInitScript(t *testing.T) {
	_, err := containerInitScript("not a key", []string{"10.0.0.2"})
	assert.ErrorContains(t, err, "upstream_pubkey")
	_, err = containerInitScript(testUpstreamPub, []string{"2001:db8::1"})
	assert.Error(t, err)
	// 公钥注释里的 shell 元字符被丢弃(重新 marshal), 不会进入脚本。
	evil := strings.Fields(testUpstreamPub)[0] + " " + strings.Fields(testUpstreamPub)[1] + " x';reboot;'"
	script, err := containerInitScript(evil, []string{"10.0.0.2", "10.0.0.3"})
	require.NoError(t, err)
	assert.NotContains(t, script, "reboot")
	keyLine := strings.Join(strings.Fields(testUpstreamPub)[:2], " ")
	assert.Contains(t, script, `printf '%s\n' 'from="10.0.0.2,10.0.0.3" `+keyLine+`' > /root/.ssh/authorized_keys`)
	assert.Contains(t, script, "HostKey=/etc/ssh/ssh_host_ed25519_key")
	assert.Contains(t, script, "PasswordAuthentication=no")
	assert.NotContains(t, script, "no-port-forwarding", "买家需要 ssh -L 端口转发")
	shSyntaxOK(t, script)
}

func TestCreateContainerCommand(t *testing.T) {
	init, err := containerInitScript(testUpstreamPub, []string{"10.0.0.2"})
	require.NoError(t, err)
	sp := leaseContainerSpec{LeaseNo: "l-abcdefgh", Image: testBaseImage, UUIDs: []string{fakeUUID0, fakeUUID1},
		CPUs: 126, MemGB: 1016, DiskGB: 1000, Port: 40000, InitShell: init}
	cmd, err := createContainerCommand(sp)
	require.NoError(t, err)
	for _, frag := range []string{
		"docker network create --label omnis.lease=l-abcdefgh omnis-l-abcdefgh-net",
		"docker volume create --label omnis.lease=l-abcdefgh omnis-l-abcdefgh-data",
		"docker rm -f omnis-l-abcdefgh ",
		`docker run -d --name omnis-l-abcdefgh --label omnis.lease=l-abcdefgh --gpus '"device=` + fakeUUID0 + "," + fakeUUID1 + `"'`,
		"--cpus 126 --memory 1016g --memory-swap 1016g --pids-limit 8192 --shm-size 64g --storage-opt size=1000G",
		"-v omnis-l-abcdefgh-data:/root/workspace --network omnis-l-abcdefgh-net -p 40000:22 --restart no",
		"--entrypoint /bin/sh " + testBaseImage + " -c '",
		"echo @@created",
	} {
		assert.Contains(t, cmd, frag)
	}
	assert.Equal(t, init, decodeB64(t, cmd))
	shSyntaxOK(t, cmd)

	whole := sp
	whole.DiskGB = 0
	cmd, err = createContainerCommand(whole)
	require.NoError(t, err)
	assert.NotContains(t, cmd, "--storage-opt")

	bad := []func(*leaseContainerSpec){
		func(s *leaseContainerSpec) { s.LeaseNo = "l-ABCDEFGH" },
		func(s *leaseContainerSpec) { s.LeaseNo = "l-abc;rm -" },
		func(s *leaseContainerSpec) { s.Image = "ubuntu:22.04" },
		func(s *leaseContainerSpec) { s.UUIDs = []string{"GPU-1-0"} },
		func(s *leaseContainerSpec) { s.UUIDs = nil },
		func(s *leaseContainerSpec) { s.Port = 22 },
		func(s *leaseContainerSpec) { s.CPUs = 0 },
		func(s *leaseContainerSpec) { s.MemGB = 0 },
		func(s *leaseContainerSpec) { s.DiskGB = -1 },
		func(s *leaseContainerSpec) { s.InitShell = "" },
	}
	for i, f := range bad {
		c := sp
		f(&c)
		_, err := createContainerCommand(c)
		assert.Error(t, err, "case %d", i)
	}
}

func TestHostKeyParsing(t *testing.T) {
	cmd, err := hostKeyCommand("l-abcdefgh")
	require.NoError(t, err)
	assert.Contains(t, cmd, "docker exec omnis-l-abcdefgh cat /etc/ssh/ssh_host_ed25519_key.pub")
	shSyntaxOK(t, cmd)
	_, err = hostKeyCommand("omnis-x")
	assert.Error(t, err)

	pub, _, err := ed25519.GenerateKey(rand.Reader)
	require.NoError(t, err)
	k, err := ssh.NewPublicKey(pub)
	require.NoError(t, err)
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))) + " root@container"
	fp, err := parseHostKeyFP("some noise\n" + line + "\n")
	require.NoError(t, err)
	assert.Equal(t, ssh.FingerprintSHA256(k), fp)

	rk, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	rpub, err := ssh.NewPublicKey(&rk.PublicKey)
	require.NoError(t, err)
	_, err = parseHostKeyFP(string(ssh.MarshalAuthorizedKey(rpub)))
	assert.Error(t, err, "只接受 ed25519 主机密钥")
	_, err = parseHostKeyFP("")
	assert.Error(t, err)
}

func TestSameUUIDSet(t *testing.T) {
	assert.True(t, sameUUIDSet([]string{"b", "a"}, []string{"a", "b"}))
	assert.False(t, sameUUIDSet([]string{"a"}, []string{"a", "b"}))
	assert.False(t, sameUUIDSet([]string{"a", "a"}, []string{"a", "b"}))
	assert.False(t, sameUUIDSet([]string{"a", "c"}, []string{"a", "b"}))
	assert.False(t, sameUUIDSet(nil, []string{"a"}))
}
