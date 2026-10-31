package fleet

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// T-089 整机托管命令模板: 白名单、注入防护、脚本语法与结论解析。

func TestBareMetalUser(t *testing.T) {
	u, err := bareMetalUser(testLeaseNo)
	require.NoError(t, err)
	assert.Equal(t, "omnis-"+testLeaseNo, u)
	for _, bad := range []string{"", "l-ABCDEFGH", "l-abc;rm -", "l-abcdefgh1", "x-abcdefgh", "l-abcdefg$"} {
		_, err := bareMetalUser(bad)
		assert.Error(t, err, bad)
	}
}

func TestHostRootCommand(t *testing.T) {
	cmd, err := hostRootCommand(testProbeImage, "echo hi")
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(cmd, "docker run --rm --privileged --pid host --network none --label omnis.probe=1 -v /:/host --entrypoint chroot "+
		testProbeImage+" /host /bin/sh -c 'echo "), cmd)
	assert.Equal(t, "echo hi", decodeB64(t, cmd))
	assert.Equal(t, 2, strings.Count(cmd, "'"), "只有 sh -c 参数一对单引号")
	_, err = hostRootCommand("busybox:latest", "true")
	assert.Error(t, err, "探针镜像必须按 digest 引用")
	_, err = hostRootCommand("x; rm -rf /", "true")
	assert.Error(t, err)
}

func TestBareMetalProvisionScript(t *testing.T) {
	s, err := bareMetalProvisionScript(testLeaseNo, testUpstreamPub, []string{"203.0.113.7", "198.51.100.2"})
	require.NoError(t, err)
	shSyntaxOK(t, s)
	u := "omnis-" + testLeaseNo
	assert.Contains(t, s, "U="+u+"\n")
	assert.Contains(t, s, `useradd -m -s "$SH" -p '*'`)
	line, err := upstreamKeyLine(testUpstreamPub)
	require.NoError(t, err)
	assert.Contains(t, s, `printf '%s\n' 'from="203.0.113.7,198.51.100.2" `+line+`' > "$H/.ssh/authorized_keys"`)
	assert.Contains(t, s, "NOPASSWD: ALL")
	assert.Contains(t, s, `visudo -cf "$T"`)
	assert.Contains(t, s, `mv -f "$T" "/etc/sudoers.d/$U"`)
	assert.Contains(t, s, `echo "@@account $U"`)
	// sudoers.d 中含 '.' 的文件名会被 sudo 忽略: 临时文件以 '.' 开头, 正式文件名(账户名)不含 '.'。
	assert.NotContains(t, u, ".")

	_, err = bareMetalProvisionScript("l-abc;rm -", testUpstreamPub, []string{"203.0.113.7"})
	assert.Error(t, err)
	_, err = bareMetalProvisionScript(testLeaseNo, "not a key", []string{"203.0.113.7"})
	assert.Error(t, err)
	_, err = bareMetalProvisionScript(testLeaseNo, testUpstreamPub, nil)
	assert.Error(t, err, "未配置网关出口 IP 不能开通(from= 无法限制来源)")
	_, err = bareMetalProvisionScript(testLeaseNo, testUpstreamPub, []string{"2001:db8::1"})
	assert.Error(t, err)
	_, err = bareMetalProvisionScript(testLeaseNo, testUpstreamPub, []string{"203.0.113.7'; id; '"})
	assert.Error(t, err)
}

func TestBareMetalTeardownScript(t *testing.T) {
	rm, err := bareMetalTeardownScript(testLeaseNo, false)
	require.NoError(t, err)
	shSyntaxOK(t, rm)
	keep, err := bareMetalTeardownScript(testLeaseNo, true)
	require.NoError(t, err)
	shSyntaxOK(t, keep)
	for _, s := range []string{rm, keep} {
		assert.Contains(t, s, `rm -f "/etc/sudoers.d/$U"`)
		assert.Contains(t, s, `pkill -KILL -u "$U"`)
		// 先撤 sudo/公钥再杀进程: 防止杀进程期间重新登录或提权。
		assert.Less(t, strings.Index(s, `rm -f "/etc/sudoers.d/$U"`), strings.Index(s, "pkill -KILL"))
		assert.Less(t, strings.Index(s, "authorized_keys"), strings.Index(s, "pkill -KILL"))
	}
	assert.Contains(t, rm, `userdel -r -f "$U"`)
	assert.Contains(t, rm, `echo "@@removed $n"`)
	assert.NotContains(t, keep, "userdel -r", "冻结中到期保留账户与家目录作核查现场")
	assert.Contains(t, keep, `echo "@@locked $n"`)
	// 家目录只在路径以账户名结尾时删除(防 passwd 被篡改后 rm -rf 任意目录)。
	assert.Contains(t, rm, `case "$H" in /*/"$U") [ -e "$H" ] && rm -rf -- "$H";; esac`)
	_, err = bareMetalTeardownScript("../etc", false)
	assert.Error(t, err)
}

func TestBareMetalKillSessionsScript(t *testing.T) {
	s, err := bareMetalKillSessionsScript(testLeaseNo)
	require.NoError(t, err)
	shSyntaxOK(t, s)
	assert.Contains(t, s, `pgrep -u "$U" -x 'sshd|sshd-session'`)
	assert.NotContains(t, s, "pkill -KILL -u", "冻结只断会话, 不结束买家其他进程")
	_, err = bareMetalKillSessionsScript("bad")
	assert.Error(t, err)
}

func TestParseBareMetalOut(t *testing.T) {
	cases := []struct {
		out, tag, val string
		err           string
	}{
		{"noise\n@@account omnis-l-abcdefgh\n", "account", "omnis-l-abcdefgh", ""},
		{"@@absent\n", "absent", "", ""},
		{"@@removed 3\n", "removed", "3", ""},
		{"@@locked 0\n", "locked", "0", ""},
		{"@@killed 2\n", "killed", "2", ""},
		{"@@missing visudo\n", "", "", "宿主机缺少 visudo"},
		{"@@badhome /etc\n", "", "", "家目录异常"},
		{"@@sudoers_invalid\n", "", "", "visudo"},
		{"@@busy omnis-l-abcdefgh\n", "", "", "未能全部结束"},
		{"@@userdel_failed omnis-l-abcdefgh\n", "", "", "删除租约账户失败"},
		{"whatever\n", "", "", "无结论输出"},
	}
	for _, c := range cases {
		tag, val, err := parseBareMetalOut(c.out)
		if c.err != "" {
			require.Error(t, err, c.out)
			assert.Contains(t, err.Error(), c.err)
			continue
		}
		require.NoError(t, err, c.out)
		assert.Equal(t, c.tag, tag)
		assert.Equal(t, c.val, val)
	}
}
