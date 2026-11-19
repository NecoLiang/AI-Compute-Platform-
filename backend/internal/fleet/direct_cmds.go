package fleet

// 直连映射访问模式(fleet.access_mode=direct, 缺省)对节点执行的固定命令模板。
//
// 买家只拿到租约容器内的 root, 用自己登记的公钥直连 节点地址:映射端口(容器 22); 平台不做中转。平台的控制点全在
// 宿主机 Docker 层(买家在容器内无法绕过):
//   - 到期: 停容器(stop 任务, 与网关模式相同);
//   - 冻结: 把容器从全部网络断开(端口映射随之撤销, 容器内也无法出网) + 结束容器内会话;
//   - 解冻/续租恢复: 重新接入租约网络, Docker 按容器的端口绑定重建映射。
//
// 公钥: 容器 sshd 读 /etc/omnis/platform_keys(平台管理公钥, from=控制面出口 IP, 开通自检/重启复检用)、
// /etc/omnis/buyer_keys(买家登记的公钥)与 ~/.ssh/authorized_keys(买家自己维护)。两个平台文件由主服务以 tar 经
// `docker cp` 整体写入: 不依赖容器内的工具, 容器停止时也能写, 买家在容器内改动后下一次同步即恢复。
//
// 与 provision_cmds.go 同一原则: 参数只来自平台生成值、配置与数据库(公钥逐把重新序列化), 脚本以 base64 交给 sh。

import (
	"archive/tar"
	"bytes"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"

	"golang.org/x/crypto/ssh"
)

const (
	directKeysDir      = "/etc/omnis"
	directPlatformKeys = directKeysDir + "/platform_keys"
	directBuyerKeys    = directKeysDir + "/buyer_keys"
)

// canonicalKeyLine 公钥规范化为 "类型 base64"(去掉选项与注释); 买家公钥入库时已规范化, 这里防御性重做一次。
func canonicalKeyLine(pub string) (string, error) {
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	if err != nil {
		return "", fmt.Errorf("公钥无法解析: %v", err)
	}
	line := strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k)))
	if !authorizedLineRe.MatchString(line) {
		return "", fmt.Errorf("公钥行含非法字符")
	}
	return line, nil
}

// buyerKeyLines 买家公钥逐把规范化; 无法解析的一把直接报错(数据库里不应出现), 不静默跳过。
func buyerKeyLines(keys []string) ([]string, error) {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		line, err := canonicalKeyLine(k)
		if err != nil {
			return nil, fmt.Errorf("买家公钥: %w", err)
		}
		out = append(out, line)
	}
	return out, nil
}

// containerInitScriptDirect 直连模式的租约容器入口: 每次启动重写 platform_keys(平台管理公钥行, 已带 from=控制面出口),
// buyer_keys 不存在时建空文件(内容由 sync_access 写入); 只用 ed25519 主机密钥; 禁用密码与键盘交互认证。
// 基础镜像须含 openssh-server(/usr/sbin/sshd、ssh-keygen)。容器重启(docker start)时重放, 幂等。
func containerInitScriptDirect(platformLine string) (string, error) {
	if platformLine == "" || !authorizedLineRe.MatchString(platformLine) {
		return "", fmt.Errorf("平台公钥行含非法字符")
	}
	return "set -e\numask 022\nmkdir -p " + directKeysDir + " /root/.ssh /run/sshd\n" +
		"chown root:root " + directKeysDir + " && chmod 755 " + directKeysDir + "\n" +
		"printf '%s\\n' '" + platformLine + "' > " + directPlatformKeys + "\n" +
		"[ -f " + directBuyerKeys + " ] || : > " + directBuyerKeys + "\n" +
		"chmod 644 " + directPlatformKeys + " " + directBuyerKeys + "\n" +
		"chmod 700 /root/.ssh\n" +
		"[ -s /etc/ssh/ssh_host_ed25519_key ] || ssh-keygen -q -t ed25519 -N '' -f /etc/ssh/ssh_host_ed25519_key\n" +
		"exec /usr/sbin/sshd -D -e -o HostKey=/etc/ssh/ssh_host_ed25519_key " +
		"-o 'AuthorizedKeysFile=" + directPlatformKeys + " " + directBuyerKeys + " .ssh/authorized_keys' " +
		"-o PasswordAuthentication=no -o KbdInteractiveAuthentication=no -o PermitRootLogin=prohibit-password " +
		"-o PubkeyAuthentication=yes\n", nil
}

// accessKeysTar 构造写入容器 /etc/ 的 tar: omnis/(0755) + omnis/platform_keys、omnis/buyer_keys(0644), 属主 0:0。
func accessKeysTar(platformLine string, buyerLines []string) ([]byte, error) {
	if !authorizedLineRe.MatchString(platformLine) {
		return nil, fmt.Errorf("平台公钥行含非法字符")
	}
	var buyer strings.Builder
	for _, l := range buyerLines {
		if !authorizedLineRe.MatchString(l) {
			return nil, fmt.Errorf("买家公钥行含非法字符")
		}
		buyer.WriteString(l + "\n")
	}
	mt := time.Unix(0, 0)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	hdr := func(name string, typ byte, mode int64, size int) error {
		return tw.WriteHeader(&tar.Header{Name: name, Typeflag: typ, Mode: mode, Size: int64(size), Uid: 0, Gid: 0,
			Uname: "root", Gname: "root", ModTime: mt, Format: tar.FormatPAX})
	}
	if err := hdr("omnis/", tar.TypeDir, 0o755, 0); err != nil {
		return nil, err
	}
	for _, f := range []struct{ name, body string }{
		{"omnis/platform_keys", platformLine + "\n"},
		{"omnis/buyer_keys", buyer.String()},
	} {
		if err := hdr(f.name, tar.TypeReg, 0o644, len(f.body)); err != nil {
			return nil, err
		}
		if _, err := tw.Write([]byte(f.body)); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// 访问同步方式。
const (
	syncConnect = "connect" // 写公钥 + 接入租约网络(端口映射恢复); 运行中时校验映射端口存在
	syncKeys    = "keys"    // 只写公钥, 不动网络(开通中: 网络由开通流程负责)
	syncCut     = "cut"     // 从全部网络断开(冻结), 不动公钥
)

// accessSyncCommand 在宿主机上同步租约容器的访问状态。输出:
//
//	容器不存在 @@absent; 接入网络 @@connected; 断开 @@disconnected(断开后仍有网络 @@cutfail 并以 5 退出);
//	写公钥 @@keys; 未运行 @@notrunning; 运行中且 connect: 映射端口 @@port <p>(缺失 @@noport 并以 4 退出); 结尾 @@done。
func accessSyncCommand(leaseNo string, keysTar []byte, mode string) (string, error) {
	if !ValidLeaseNo(leaseNo) {
		return "", fmt.Errorf("租约号非法: %q", leaseNo)
	}
	c, nw := containerNameOf(leaseNo), networkNameOf(leaseNo)
	if err := checkLeaseRes(c, nw); err != nil {
		return "", err
	}
	switch mode {
	case syncConnect, syncKeys:
		if len(keysTar) == 0 {
			return "", fmt.Errorf("公钥归档为空")
		}
	case syncCut:
	default:
		return "", fmt.Errorf("未知访问同步方式 %q", mode)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "C=%s\nNW=%s\nL=%s\n", c, nw, leaseNo)
	b.WriteString(`docker inspect --type container "$C" >/dev/null 2>&1 || { echo @@absent; exit 0; }
nets() { docker inspect --type container -f '{{range $k, $v := .NetworkSettings.Networks}}{{$k}} {{end}}' "$C"; }
NETS=$(nets) || exit 1
`)
	switch mode {
	case syncCut:
		b.WriteString(`for n in $NETS; do docker network disconnect -f "$n" "$C" >/dev/null 2>&1 || true; done
NETS=$(nets) || exit 1
[ -z "$(printf '%s' "$NETS" | tr -d ' ')" ] || { echo "@@cutfail $NETS"; exit 5; }
echo @@disconnected
`)
	default:
		b.WriteString("echo " + base64.StdEncoding.EncodeToString(keysTar) + ` | base64 -d | docker cp -a - "$C":/etc/ >/dev/null || exit 6
echo @@keys
`)
		if mode == syncConnect {
			b.WriteString(`case " $NETS " in
  *" $NW "*) ;;
  *) docker network inspect "$NW" >/dev/null 2>&1 || docker network create --label omnis.lease="$L" "$NW" >/dev/null || exit 1
     docker network connect "$NW" "$C" || exit 1
     echo @@connected ;;
esac
`)
		}
	}
	b.WriteString(`if [ "$(docker inspect --type container -f '{{.State.Running}}' "$C")" != true ]; then echo @@notrunning; echo @@done; exit 0; fi
`)
	if mode == syncConnect {
		b.WriteString(`P=$(docker port "$C" 22/tcp 2>/dev/null | head -n 1)
[ -n "$P" ] || { echo @@noport; exit 4; }
echo "@@port ${P##*:}"
`)
	}
	b.WriteString("echo @@done\n")
	return shB64(b.String()), nil
}

// AccessSyncResult accessSyncCommand 的结论。
type AccessSyncResult struct {
	Absent       bool `json:"absent,omitempty"`
	Keys         bool `json:"keys,omitempty"`
	Connected    bool `json:"connected,omitempty"`
	Disconnected bool `json:"disconnected,omitempty"`
	Running      bool `json:"running"`
	Port         int  `json:"port,omitempty"`
}

// parseAccessSync 解析结论; 异常标记转成可读错误。
func parseAccessSync(out string) (AccessSyncResult, error) {
	var r AccessSyncResult
	done, notRunning := false, false
	for _, l := range nonEmptyLines(out) {
		rest, ok := strings.CutPrefix(l, "@@")
		if !ok {
			continue
		}
		t, v, _ := strings.Cut(rest, " ")
		switch t {
		case "absent":
			r.Absent = true
			return r, nil
		case "keys":
			r.Keys = true
		case "connected":
			r.Connected = true
		case "disconnected":
			r.Disconnected = true
		case "notrunning":
			notRunning = true
		case "port":
			p, err := strconv.Atoi(v)
			if err != nil || checkPort(p) != nil {
				return r, fmt.Errorf("容器映射端口无法解析: %q", truncate(v, 40))
			}
			r.Port = p
		case "noport":
			return r, fmt.Errorf("容器已接入租约网络但没有 22 端口映射(Docker 未恢复端口绑定)")
		case "cutfail":
			return r, fmt.Errorf("从网络断开容器后仍接入: %s", truncate(v, 120))
		case "done":
			done = true
		}
	}
	if !done {
		return r, fmt.Errorf("访问同步无结论输出: %q", truncate(strings.TrimSpace(out), 160))
	}
	r.Running = !notRunning
	return r, nil
}

// containerGPUListCommand 经管理连接在租约容器内执行 nvidia-smi -L(冻结中的租约公网入口已断开, 重启复检用)。
// 输出来自买家可写的容器, 只作一致性核对, 不作为安全依据。
func containerGPUListCommand(leaseNo string) (string, error) {
	if !ValidLeaseNo(leaseNo) {
		return "", fmt.Errorf("租约号非法: %q", leaseNo)
	}
	return "docker exec " + containerNameOf(leaseNo) + " nvidia-smi -L", nil
}

// gatewayAuthLine 网关模式写入租约 authorized_keys 的唯一一行: 网关上游公钥, from=网关出口 IP。
func gatewayAuthLine(upstreamPub string, gwIPs []string) (string, error) {
	line, err := upstreamKeyLine(upstreamPub)
	if err != nil {
		return "", err
	}
	ips, err := gatewayIPv4s(gwIPs)
	if err != nil {
		return "", err
	}
	auth := fmt.Sprintf(`from="%s" %s`, strings.Join(ips, ","), line)
	if !authorizedLineRe.MatchString(auth) {
		return "", fmt.Errorf("authorized_keys 行含非法字符")
	}
	return auth, nil
}

// authorizedKeysWrite 整机脚本片段: 把 lines 原子写入 "$H/.ssh/authorized_keys"(先写临时文件再 mv)。
func authorizedKeysWrite(lines []string) (string, error) {
	if len(lines) == 0 {
		return "", fmt.Errorf("authorized_keys 至少要有一行")
	}
	var b strings.Builder
	b.WriteString(`mkdir -p "$H/.ssh"
T="$H/.ssh/.authorized_keys.omnis"
: > "$T"
`)
	for _, l := range lines {
		if !authorizedLineRe.MatchString(l) {
			return "", fmt.Errorf("authorized_keys 行含非法字符")
		}
		b.WriteString("printf '%s\\n' '" + l + "' >> \"$T\"\n")
	}
	b.WriteString(`chmod 600 "$T"; mv -f "$T" "$H/.ssh/authorized_keys"
chmod 700 "$H/.ssh"; chown -R "$U:$G" "$H/.ssh"
`)
	return b.String(), nil
}

// bareMetalKeysScript 直连模式整机: 重写租约账户的 authorized_keys(平台管理公钥 + 买家公钥; 冻结时只留平台公钥),
// 输出 "@@keys <行数>"; 账户不存在 @@absent。
func bareMetalKeysScript(leaseNo string, lines []string) (string, error) {
	u, err := bareMetalUser(leaseNo)
	if err != nil {
		return "", err
	}
	w, err := authorizedKeysWrite(lines)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("set -e\numask 077\n")
	fmt.Fprintf(&b, "U=%s\n", u)
	b.WriteString(bareMetalPrereq)
	b.WriteString(`if ! id -u "$U" >/dev/null 2>&1; then echo @@absent; exit 0; fi
H=$(getent passwd "$U" | cut -d: -f6)
case "$H" in /*/"$U") ;; *) echo "@@badhome $H"; exit 4;; esac
[ -d "$H" ] || { echo "@@badhome $H"; exit 4; }
G=$(id -g "$U")
`)
	b.WriteString(w)
	fmt.Fprintf(&b, "echo \"@@keys %d\"\n", len(lines))
	return b.String(), nil
}
