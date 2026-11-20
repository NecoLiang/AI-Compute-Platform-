package fleet

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/ssh"
)

// 直连映射访问模式命令模板: 公钥规范化、容器入口、tar 内容、访问同步脚本(以假 docker 实际执行)与端口自检命令。

const testDirectLease = "l-abcdefgh"

func pubLine(t *testing.T, comment string) string {
	t.Helper()
	l := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(newSigner(t).PublicKey())))
	if comment != "" {
		l += " " + comment
	}
	return l
}

func TestCanonicalKeyLine(t *testing.T) {
	raw := pubLine(t, "buyer@laptop")
	line, err := canonicalKeyLine(`command="rm -rf /" ` + raw)
	require.NoError(t, err)
	assert.Equal(t, strings.Join(strings.Fields(raw)[:2], " "), line, "去掉选项与注释")

	_, err = canonicalKeyLine("ssh-ed25519 not-base64")
	assert.Error(t, err)

	lines, err := buyerKeyLines([]string{raw, pubLine(t, "")})
	require.NoError(t, err)
	assert.Len(t, lines, 2)
	_, err = buyerKeyLines([]string{raw, "garbage"})
	assert.ErrorContains(t, err, "买家公钥", "无法解析的一把直接报错, 不静默跳过")
}

func TestContainerInitScriptDirect(t *testing.T) {
	platform, err := authorizedKeysLine(pubLine(t, ""), []string{"203.0.113.7"})
	require.NoError(t, err)
	script, err := containerInitScriptDirect(platform)
	require.NoError(t, err)
	shSyntaxOK(t, script)
	assert.Contains(t, script, "printf '%s\\n' '"+platform+"' > /etc/omnis/platform_keys\n")
	assert.Contains(t, script, "[ -f /etc/omnis/buyer_keys ] || : > /etc/omnis/buyer_keys\n", "已有买家公钥不被入口覆盖")
	assert.Contains(t, script, "'AuthorizedKeysFile=/etc/omnis/platform_keys /etc/omnis/buyer_keys .ssh/authorized_keys'")
	assert.Contains(t, script, "-o PasswordAuthentication=no -o KbdInteractiveAuthentication=no -o PermitRootLogin=prohibit-password")

	_, err = containerInitScriptDirect("ssh-ed25519 AAAA'; reboot; '")
	assert.Error(t, err)
	_, err = containerInitScriptDirect("")
	assert.Error(t, err)
}

// readKeysTar 解开 accessKeysTar, 返回 名字 → (模式, 内容)。
func readKeysTar(t *testing.T, b []byte) map[string][2]string {
	t.Helper()
	out := map[string][2]string{}
	tr := tar.NewReader(bytes.NewReader(b))
	for {
		h, err := tr.Next()
		if err == io.EOF {
			return out
		}
		require.NoError(t, err)
		assert.Equal(t, 0, h.Uid)
		assert.Equal(t, 0, h.Gid)
		body, err := io.ReadAll(tr)
		require.NoError(t, err)
		out[h.Name] = [2]string{os.FileMode(h.Mode).String(), string(body)}
	}
}

func TestAccessKeysTar(t *testing.T) {
	platform, err := authorizedKeysLine(pubLine(t, ""), []string{"203.0.113.7"})
	require.NoError(t, err)
	b1, b2 := pubLine(t, ""), pubLine(t, "")
	raw, err := accessKeysTar(platform, []string{b1, b2})
	require.NoError(t, err)
	files := readKeysTar(t, raw)
	assert.Equal(t, map[string][2]string{
		"omnis/":              {"-rwxr-xr-x", ""},
		"omnis/platform_keys": {"-rw-r--r--", platform + "\n"},
		"omnis/buyer_keys":    {"-rw-r--r--", b1 + "\n" + b2 + "\n"},
	}, files)

	raw, err = accessKeysTar(platform, nil)
	require.NoError(t, err)
	assert.Equal(t, "", readKeysTar(t, raw)["omnis/buyer_keys"][1], "无买家公钥时清空(删光公钥即无人能登录)")

	_, err = accessKeysTar(platform, []string{"ssh-ed25519 AAAA\nssh-rsa BBBB"})
	assert.Error(t, err, "换行注入")
	_, err = accessKeysTar("x'y", nil)
	assert.Error(t, err)
}

func TestAccessSyncCommandValidation(t *testing.T) {
	_, err := accessSyncCommand("l-ABCDEFGH", []byte("x"), syncConnect)
	assert.Error(t, err)
	_, err = accessSyncCommand(testDirectLease, nil, syncConnect)
	assert.ErrorContains(t, err, "公钥归档为空")
	_, err = accessSyncCommand(testDirectLease, nil, syncKeys)
	assert.Error(t, err)
	_, err = accessSyncCommand(testDirectLease, []byte("x"), "open")
	assert.ErrorContains(t, err, "未知访问同步方式")
	for _, m := range []string{syncConnect, syncKeys, syncCut} {
		cmd, err := accessSyncCommand(testDirectLease, []byte("x"), m)
		require.NoError(t, err)
		shSyntaxOK(t, decodeB64(t, cmd))
	}
}

// fakeAccessDocker 模拟 accessSyncCommand 用到的 docker 子命令; 状态目录 $FD:
// exists/running(存在即真)、nets(空格分隔网络)、stuck(断开无效)、tar(docker cp 收到的归档)、netcreated。
// docker port 在接入任意网络时返回映射(模拟 Docker 按网络撤销/恢复端口绑定)。
const fakeAccessDocker = `#!/bin/sh
D="$FD"
nets() { cat "$D/nets" 2>/dev/null; }
case "$1" in
inspect)
  [ -f "$D/exists" ] || exit 1
  if [ "$4" = "-f" ]; then
    case "$5" in
      *Networks*) for n in $(nets); do printf '%s ' "$n"; done; echo ;;
      *Running*) if [ -f "$D/running" ]; then echo true; else echo false; fi ;;
      *) exit 2 ;;
    esac
  fi
  exit 0 ;;
network)
  case "$2" in
    disconnect) [ -f "$D/stuck" ] && exit 1
      n="$4"; out=""; for x in $(nets); do [ "$x" = "$n" ] || out="$out $x"; done; echo "$out" > "$D/nets" ;;
    connect) echo "$(nets) $3" > "$D/nets" ;;
    inspect) [ -f "$D/netcreated" ] ;;
    create) touch "$D/netcreated" ;;
    *) exit 2 ;;
  esac ;;
cp) [ "$2" = "-a" ] && [ "$3" = "-" ] || exit 2; cat > "$D/tar" ;;
port) [ -n "$(nets | tr -d ' ')" ] && [ -f "$D/running" ] && { echo "0.0.0.0:40001"; echo "[::]:40001"; }; exit 0 ;;
*) exit 2 ;;
esac
`

type dockerSim struct {
	t   *testing.T
	dir string
	bin string
}

func newDockerSim(t *testing.T) *dockerSim {
	bin := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(bin, "docker"), []byte(fakeAccessDocker), 0o755))
	return &dockerSim{t: t, dir: t.TempDir(), bin: bin}
}

func (d *dockerSim) flag(name string) {
	require.NoError(d.t, os.WriteFile(filepath.Join(d.dir, name), nil, 0o644))
}

func (d *dockerSim) setNets(nets string) {
	require.NoError(d.t, os.WriteFile(filepath.Join(d.dir, "nets"), []byte(nets+"\n"), 0o644))
}

func (d *dockerSim) nets() string {
	b, _ := os.ReadFile(filepath.Join(d.dir, "nets"))
	return strings.Join(strings.Fields(string(b)), " ")
}

func (d *dockerSim) run(cmd string) (string, int) {
	c := exec.Command("sh", "-c", cmd)
	c.Env = append(os.Environ(), "PATH="+d.bin+":"+os.Getenv("PATH"), "FD="+d.dir)
	out, err := c.CombinedOutput()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	} else {
		require.NoError(d.t, err)
	}
	return string(out), code
}

func (d *dockerSim) sync(keys []byte, mode string) (AccessSyncResult, error, int) {
	cmd, err := accessSyncCommand(testDirectLease, keys, mode)
	require.NoError(d.t, err)
	out, code := d.run(cmd)
	res, perr := parseAccessSync(out)
	return res, perr, code
}

func TestAccessSyncScript(t *testing.T) {
	platform, err := authorizedKeysLine(pubLine(t, ""), []string{"203.0.113.7"})
	require.NoError(t, err)
	keys, err := accessKeysTar(platform, []string{pubLine(t, "")})
	require.NoError(t, err)
	nw := networkNameOf(testDirectLease)

	t.Run("容器不存在", func(t *testing.T) {
		d := newDockerSim(t)
		for _, m := range []string{syncConnect, syncKeys, syncCut} {
			res, err, code := d.sync(keys, m)
			require.NoError(t, err)
			assert.Equal(t, 0, code)
			assert.True(t, res.Absent)
		}
	})

	t.Run("冻结断开全部网络, 再解冻接入租约网络恢复端口", func(t *testing.T) {
		d := newDockerSim(t)
		d.flag("exists")
		d.flag("running")
		d.flag("netcreated")
		d.setNets("bridge " + nw)

		res, err, _ := d.sync(nil, syncCut)
		require.NoError(t, err)
		assert.Equal(t, AccessSyncResult{Disconnected: true, Running: true}, res)
		assert.Equal(t, "", d.nets())
		_, statErr := os.Stat(filepath.Join(d.dir, "tar"))
		assert.True(t, os.IsNotExist(statErr), "cut 不动公钥")

		res, err, _ = d.sync(keys, syncConnect)
		require.NoError(t, err)
		assert.Equal(t, AccessSyncResult{Keys: true, Connected: true, Running: true, Port: 40001}, res)
		assert.Equal(t, nw, d.nets())
		got, err := os.ReadFile(filepath.Join(d.dir, "tar"))
		require.NoError(t, err)
		assert.Equal(t, keys, got, "公钥归档原样交给 docker cp")

		res, err, _ = d.sync(keys, syncConnect)
		require.NoError(t, err)
		assert.False(t, res.Connected, "已接入时不重复 connect")
		assert.Equal(t, 40001, res.Port)
	})

	t.Run("租约网络不存在时先创建", func(t *testing.T) {
		d := newDockerSim(t)
		d.flag("exists")
		d.flag("running")
		res, err, _ := d.sync(keys, syncConnect)
		require.NoError(t, err)
		assert.True(t, res.Connected)
		assert.FileExists(t, filepath.Join(d.dir, "netcreated"))
	})

	t.Run("只写公钥不动网络", func(t *testing.T) {
		d := newDockerSim(t)
		d.flag("exists")
		res, err, _ := d.sync(keys, syncKeys)
		require.NoError(t, err)
		assert.Equal(t, AccessSyncResult{Keys: true}, res, "未运行")
		assert.Equal(t, "", d.nets())
	})

	t.Run("容器停止时 connect 只接网络不校验端口", func(t *testing.T) {
		d := newDockerSim(t)
		d.flag("exists")
		d.flag("netcreated")
		res, err, code := d.sync(keys, syncConnect)
		require.NoError(t, err)
		assert.Equal(t, 0, code)
		assert.Equal(t, AccessSyncResult{Keys: true, Connected: true}, res)
	})

	t.Run("断开无效即失败", func(t *testing.T) {
		d := newDockerSim(t)
		d.flag("exists")
		d.flag("running")
		d.flag("stuck")
		d.setNets(nw)
		_, err, code := d.sync(nil, syncCut)
		assert.Equal(t, 5, code)
		assert.ErrorContains(t, err, "仍接入")
	})
}

func TestParseAccessSync(t *testing.T) {
	r, err := parseAccessSync("@@keys\n@@connected\n@@port 40007\n@@done\n")
	require.NoError(t, err)
	assert.Equal(t, AccessSyncResult{Keys: true, Connected: true, Running: true, Port: 40007}, r)

	r, err = parseAccessSync("noise\n@@absent\n")
	require.NoError(t, err)
	assert.True(t, r.Absent)

	r, err = parseAccessSync("@@disconnected\n@@notrunning\n@@done\n")
	require.NoError(t, err)
	assert.Equal(t, AccessSyncResult{Disconnected: true}, r)

	_, err = parseAccessSync("@@keys\n@@connected\n@@noport\n")
	assert.ErrorContains(t, err, "没有 22 端口映射")
	_, err = parseAccessSync("@@cutfail bridge\n")
	assert.ErrorContains(t, err, "仍接入")
	_, err = parseAccessSync("@@port 22\n@@done\n")
	assert.ErrorContains(t, err, "无法解析")
	_, err = parseAccessSync("@@port abc\n@@done\n")
	assert.Error(t, err)
	_, err = parseAccessSync("@@keys\n")
	assert.ErrorContains(t, err, "无结论")
	_, err = parseAccessSync("")
	assert.Error(t, err)
}

func TestContainerGPUListCommand(t *testing.T) {
	cmd, err := containerGPUListCommand(testDirectLease)
	require.NoError(t, err)
	assert.Equal(t, "docker exec omnis-l-abcdefgh nvidia-smi -L", cmd)
	_, err = containerGPUListCommand("l-abc; rm")
	assert.Error(t, err)
}

func TestGatewayAuthLine(t *testing.T) {
	up := pubLine(t, "")
	line, err := gatewayAuthLine(up, []string{"10.0.0.2", "10.0.0.3"})
	require.NoError(t, err)
	assert.True(t, strings.HasPrefix(line, `from="10.0.0.2,10.0.0.3" ssh-ed25519 `), line)
	_, err = gatewayAuthLine(up, nil)
	assert.Error(t, err)
}

func TestBareMetalKeysScript(t *testing.T) {
	platform, err := authorizedKeysLine(pubLine(t, ""), []string{"203.0.113.7"})
	require.NoError(t, err)
	buyer := pubLine(t, "")
	script, err := bareMetalKeysScript(testDirectLease, []string{platform, buyer})
	require.NoError(t, err)
	shSyntaxOK(t, script)
	assert.Contains(t, script, "U=omnis-l-abcdefgh\n")
	assert.Contains(t, script, "printf '%s\\n' '"+platform+"' >> \"$T\"\n")
	assert.Contains(t, script, "printf '%s\\n' '"+buyer+"' >> \"$T\"\n")
	assert.Contains(t, script, `mv -f "$T" "$H/.ssh/authorized_keys"`, "原子替换")
	assert.Contains(t, script, "echo \"@@keys 2\"\n")

	_, err = bareMetalKeysScript(testDirectLease, nil)
	assert.Error(t, err, "至少保留平台公钥")
	_, err = bareMetalKeysScript(testDirectLease, []string{"a'b"})
	assert.Error(t, err)
	_, err = bareMetalKeysScript("bad", []string{platform})
	assert.Error(t, err)
}
