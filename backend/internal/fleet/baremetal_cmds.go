package fleet

// 整机(bare_metal)托管对宿主机执行的固定命令模板(T-089, docs/25 §25.7.4)。
//
// 管理账户只在 docker 组、无 sudo(docs/25 §25.4.1), 建删宿主机账户需要宿主机 root: 借平台探针镜像起一次性特权容器,
// chroot 到宿主机根目录, 用宿主机自己的 useradd/userdel/pkill/visudo 执行(docker 组本即 root 等价, 不新增权限)。
// 与 provision_cmds.go 同一原则: 参数只来自平台生成值(租约号)与配置(网关上游公钥、出口 IP), 逐个过白名单;
// 脚本以 base64 交给宿主机 sh 执行, 避免引号嵌套。

import (
	"fmt"
	"strings"
)

// bareMetalUser 整机租约在宿主机上的账户名 omnis-<lease_no>(即 docs/25 §25.7.4 的 omnis-l-xxxxxxxx)。
func bareMetalUser(leaseNo string) (string, error) {
	u := containerNameOf(leaseNo)
	if !ValidLeaseNo(leaseNo) || checkLeaseRes(u) != nil {
		return "", fmt.Errorf("租约号非法: %q", truncate(leaseNo, 40))
	}
	return u, nil
}

// hostRootCommand 以探针镜像(须含 chroot)起一次性特权容器, chroot 到宿主机根目录执行脚本。
// --privileged: 解除 AppArmor/SELinux/seccomp 限制, 否则向宿主机进程发信号、写宿主机 /etc 会被拒;
// --pid host: pkill 能看到宿主机进程; --network none: 脚本不需要网络。
func hostRootCommand(image, script string) (string, error) {
	if err := checkImage(image); err != nil {
		return "", err
	}
	return "docker run --rm --privileged --pid host --network none --label omnis.probe=1 -v /:/host " +
		"--entrypoint chroot " + image + " /host /bin/sh -c '" + shB64(script) + "'", nil
}

// bareMetalPrereq 宿主机必备命令检查(缺失输出 @@missing <名> 并以 3 退出, 由调用方转成可读错误)。
const bareMetalPrereq = `for c in useradd userdel usermod pkill pgrep visudo getent; do
  command -v $c >/dev/null 2>&1 || { echo "@@missing $c"; exit 3; }
done
[ -d /etc/sudoers.d ] || { echo "@@missing /etc/sudoers.d"; exit 3; }
`

// bareMetalProvisionScript 开通: 创建账户(已存在则沿用, 幂等)、authorized_keys 只写网关上游公钥并带
// from="<网关出口IP>"、sudoers.d 授予免密 sudo(visudo 校验通过才落盘), 输出 "@@account <账户>"。
// 账户密码字段为 '*'(不可密码登录, 但不是 sshd 视为锁定的 '!'), 登录只能经网关公钥。
func bareMetalProvisionScript(leaseNo, upstreamPub string, gwIPs []string) (string, error) {
	u, err := bareMetalUser(leaseNo)
	if err != nil {
		return "", err
	}
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
	var b strings.Builder
	b.WriteString("set -e\numask 077\n")
	fmt.Fprintf(&b, "U=%s\n", u)
	b.WriteString(bareMetalPrereq)
	b.WriteString(`SH=/bin/sh; [ -x /bin/bash ] && SH=/bin/bash
id -u "$U" >/dev/null 2>&1 || useradd -m -s "$SH" -p '*' -c 'omnis managed lease' "$U"
H=$(getent passwd "$U" | cut -d: -f6)
case "$H" in /*/"$U") ;; *) echo "@@badhome $H"; exit 4;; esac
[ -d "$H" ] || { echo "@@badhome $H"; exit 4; }
G=$(id -g "$U")
mkdir -p "$H/.ssh"
`)
	b.WriteString("printf '%s\\n' '" + auth + "' > \"$H/.ssh/authorized_keys\"\n")
	b.WriteString(`chmod 700 "$H/.ssh"; chmod 600 "$H/.ssh/authorized_keys"; chown -R "$U:$G" "$H/.ssh"
T=/etc/sudoers.d/.omnis-tmp-$U
printf '%s ALL=(ALL:ALL) NOPASSWD: ALL\n' "$U" > "$T"
chmod 440 "$T"
visudo -cf "$T" >/dev/null 2>&1 || { rm -f "$T"; echo @@sudoers_invalid; exit 5; }
mv -f "$T" "/etc/sudoers.d/$U"
echo "@@account $U"
`)
	return b.String(), nil
}

// bareMetalTeardownScript 回收(docs/25 §25.7.4 到期): 先撤 sudo 与 authorized_keys(阻止新登录与提权),
// 再结束该账户全部进程(反复 pkill 直到没有, 最多 10 轮), keep=false 时删除账户与家目录。
// 输出: 账户不存在 @@absent; keep=true @@locked <结束进程数>; keep=false @@removed <结束进程数>。
// 买家经 sudo 以 root 身份留下的进程/文件不在账户名下, 不保证清除——这正是节点必须重装(needs_reimage)的原因。
func bareMetalTeardownScript(leaseNo string, keep bool) (string, error) {
	u, err := bareMetalUser(leaseNo)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("set -e\n")
	fmt.Fprintf(&b, "U=%s\n", u)
	b.WriteString(bareMetalPrereq)
	b.WriteString(`rm -f "/etc/sudoers.d/$U" "/etc/sudoers.d/.omnis-tmp-$U"
if ! id -u "$U" >/dev/null 2>&1; then echo @@absent; exit 0; fi
H=$(getent passwd "$U" | cut -d: -f6)
case "$H" in /*/"$U") rm -f "$H/.ssh/authorized_keys" "$H/.ssh/authorized_keys2";; esac
usermod -e 1 "$U" >/dev/null 2>&1 || true
n=0; i=0
while pgrep -u "$U" >/dev/null 2>&1; do
  i=$((i+1)); [ $i -le 10 ] || { echo "@@busy $U"; exit 6; }
  c=$(pgrep -u "$U" | wc -l); n=$((n+c))
  pkill -KILL -u "$U" || true
  sleep 1
done
`)
	if keep {
		b.WriteString("echo \"@@locked $n\"\n")
		return b.String(), nil
	}
	b.WriteString(`userdel -r -f "$U" >/dev/null 2>&1 || true
if id -u "$U" >/dev/null 2>&1; then echo "@@userdel_failed $U"; exit 7; fi
case "$H" in /*/"$U") [ -e "$H" ] && rm -rf -- "$H";; esac
echo "@@removed $n"
`)
	return b.String(), nil
}

// bareMetalKillSessionsScript 冻结(docs/25 §25.7.3): 只结束该账户的 sshd 会话进程(断开现有登录), 不动其他进程
// (保全现场), 输出 "@@killed <n>"; 账户不存在输出 @@absent。新登录由网关拒绝。
func bareMetalKillSessionsScript(leaseNo string) (string, error) {
	u, err := bareMetalUser(leaseNo)
	if err != nil {
		return "", err
	}
	return "U=" + u + "\n" + bareMetalPrereq + `if ! id -u "$U" >/dev/null 2>&1; then echo @@absent; exit 0; fi
n=0
for p in $(pgrep -u "$U" -x 'sshd|sshd-session' || true); do kill -KILL "$p" 2>/dev/null && n=$((n+1)); done
echo "@@killed $n"
`, nil
}

// parseBareMetalOut 解析脚本结论行: 返回标记(account/absent/locked/removed/killed)与其后的值;
// @@missing/@@badhome/@@sudoers_invalid/@@busy/@@userdel_failed 转为可读错误(可重试, 由运营处理节点环境)。
func parseBareMetalOut(out string) (tag, val string, err error) {
	for _, l := range nonEmptyLines(out) {
		rest, ok := strings.CutPrefix(l, "@@")
		if !ok {
			continue
		}
		t, v, _ := strings.Cut(rest, " ")
		switch t {
		case "account", "absent", "locked", "removed", "killed":
			return t, v, nil
		case "missing":
			return "", "", fmt.Errorf("宿主机缺少 %s(整机托管需要 useradd/userdel/usermod/pkill/pgrep/visudo/getent 与 /etc/sudoers.d)", truncate(v, 60))
		case "badhome":
			return "", "", fmt.Errorf("租约账户家目录异常: %q", truncate(v, 80))
		case "sudoers_invalid":
			return "", "", fmt.Errorf("宿主机 visudo 校验租约 sudoers 失败")
		case "busy":
			return "", "", fmt.Errorf("租约账户进程反复出现, 10 轮内未能全部结束")
		case "userdel_failed":
			return "", "", fmt.Errorf("删除租约账户失败")
		}
	}
	return "", "", fmt.Errorf("整机托管脚本无结论输出: %q", truncate(strings.TrimSpace(out), 160))
}
