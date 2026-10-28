package fleet

// 开通流水线(T-084)对节点执行的固定命令模板。与 commands.go 同一原则: 参数只来自平台生成值、配置与数据库,
// 逐个过白名单; 多行脚本以 base64 交给 sh 执行, 避免引号嵌套。

import (
	"encoding/base64"
	"fmt"
	"net"
	"regexp"
	"strings"

	"golang.org/x/crypto/ssh"
)

var leaseResRe = regexp.MustCompile(`^omnis-l-[a-z2-7]{8}(-data|-net)?$`)

func checkLeaseRes(names ...string) error {
	for _, n := range names {
		if !leaseResRe.MatchString(n) {
			return fmt.Errorf("租约资源名非法: %q", truncate(n, 60))
		}
	}
	return nil
}

// shB64 把固定脚本交给 sh 执行。
func shB64(script string) string {
	return "echo " + base64.StdEncoding.EncodeToString([]byte(script)) + " | base64 -d | sh"
}

// gatewayIPv4s DOCKER-USER 规则与容器 from= 用的网关出口 IP。iptables 规则只写 IPv4: IPv6 出口须另行规划,
// 此处明确拒绝而不是静默漏放行。
func gatewayIPv4s(ips []string) ([]string, error) {
	if len(ips) == 0 {
		return nil, fmt.Errorf("accessgw.egress_ips 未配置")
	}
	out := make([]string, 0, len(ips))
	for _, s := range ips {
		ip := net.ParseIP(s)
		if ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("accessgw.egress_ips 只支持 IPv4(开通时写 iptables DOCKER-USER 规则): %q", s)
		}
		out = append(out, ip.To4().String())
	}
	return out, nil
}

// firewallApplyScript 开通 ② 在节点上校验/下发 DOCKER-USER 规则: 端口段 [start,end] 只放行网关出口 IP。
// 对宿主机上每个存在 DOCKER-USER 链的 iptables 后端(nft / legacy)分别处理: 本端口段的规则恰为
// "每个网关 IP 一条 RETURN + 末尾一条 DROP" 时输出 @@present, 否则删掉本端口段的全部旧规则后按序插入到链首,
// 输出 @@applied。宿主机没有 DOCKER-USER 链(Docker 未用 iptables)时输出 @@nochain 并以 3 退出。
func firewallApplyScript(gwIPs []string, start, end int) (string, error) {
	if err := checkPort(start); err != nil {
		return "", err
	}
	if err := checkPort(end); err != nil {
		return "", err
	}
	if start >= end {
		return "", fmt.Errorf("租约端口段非法 %d-%d", start, end)
	}
	ips, err := gatewayIPv4s(gwIPs)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "M='-p tcp -m conntrack --ctorigdstport %d:%d --ctdir ORIGINAL'\n", start, end)
	fmt.Fprintf(&b, "K='--ctorigdstport %d:%d '\n", start, end)
	fmt.Fprintf(&b, "IPS='%s'\nN=%d\n", strings.Join(ips, " "), len(ips)+1)
	b.WriteString(`found=0
for IPT in iptables-nft iptables-legacy; do
  command -v $IPT >/dev/null 2>&1 || continue
  $IPT -w -S DOCKER-USER >/dev/null 2>&1 || continue
  found=1
  cur=$($IPT -w -S DOCKER-USER | grep -F -- "$K" || true)
  ok=1
  [ "$(printf '%s\n' "$cur" | grep -c -- '-A DOCKER-USER')" = "$N" ] || ok=0
  printf '%s\n' "$cur" | tail -n 1 | grep -q -- '-j DROP' || ok=0
  for ip in $IPS; do $IPT -w -C DOCKER-USER $M -s $ip -j RETURN 2>/dev/null || ok=0; done
  $IPT -w -C DOCKER-USER $M -j DROP 2>/dev/null || ok=0
  if [ $ok = 1 ]; then echo "@@present $IPT"; continue; fi
  printf '%s\n' "$cur" | grep -- '-A DOCKER-USER' | sed 's/^-A /-D /' | while read -r r; do $IPT -w $r || exit 1; done || exit 1
  $IPT -w -I DOCKER-USER 1 $M -j DROP || exit 1
  for ip in $IPS; do $IPT -w -I DOCKER-USER 1 $M -s $ip -j RETURN || exit 1; done
  echo "@@applied $IPT"
done
[ $found = 1 ] || { echo @@nochain; exit 3; }
`)
	return b.String(), nil
}

// firewallCommand 以平台探针镜像(须含 iptables-nft/iptables-legacy、base64、sh)在宿主机网络命名空间内执行规则脚本。
// 管理账户只在 docker 组、无 sudo(docs/25 §25.4.1), 因此借容器的 NET_ADMIN 能力改宿主机 netfilter。
func firewallCommand(image string, gwIPs []string, start, end int) (string, error) {
	if err := checkImage(image); err != nil {
		return "", err
	}
	script, err := firewallApplyScript(gwIPs, start, end)
	if err != nil {
		return "", err
	}
	return "docker run --rm --network host --cap-drop ALL --cap-add NET_ADMIN --cap-add NET_RAW --label omnis.probe=1 " +
		"--entrypoint /bin/sh " + image + " -c '" + shB64(script) + "'", nil
}

// imagePresentCommand 开通 ③: 基础镜像已在本地(按 digest 引用)输出 @@have, 否则 @@missing。
func imagePresentCommand(image string) (string, error) {
	if err := checkImage(image); err != nil {
		return "", err
	}
	return "if docker image inspect --format '{{.Id}}' " + image + " >/dev/null 2>&1; then echo @@have; else echo @@missing; fi", nil
}

// upstreamKeyLine 网关上游公钥规范化为 "类型 base64"(去掉注释与选项), 用于写入容器 authorized_keys。
func upstreamKeyLine(pub string) (string, error) {
	k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(pub))
	if err != nil {
		return "", fmt.Errorf("accessgw.upstream_pubkey 不是合法的 SSH 公钥: %v", err)
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(k))), nil
}

// containerInitScript 租约容器入口(docs/25 §25.5.2 "容器内认证"): authorized_keys 只有网关上游公钥并带
// from="<网关出口IP>"; 只用 ed25519 主机密钥(与记录的 upstream_host_key_fp 一致); 禁用密码与键盘交互认证。
// 基础镜像须含 openssh-server(/usr/sbin/sshd、ssh-keygen)。容器重启(docker start)时重放, 幂等。
func containerInitScript(upstreamPub string, gwIPs []string) (string, error) {
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
		return "", fmt.Errorf("容器 authorized_keys 行含非法字符")
	}
	return "set -e\numask 077\nmkdir -p /root/.ssh /run/sshd\n" +
		"printf '%s\\n' '" + auth + "' > /root/.ssh/authorized_keys\n" +
		"chmod 700 /root/.ssh && chmod 600 /root/.ssh/authorized_keys\n" +
		"[ -s /etc/ssh/ssh_host_ed25519_key ] || ssh-keygen -q -t ed25519 -N '' -f /etc/ssh/ssh_host_ed25519_key\n" +
		"exec /usr/sbin/sshd -D -e -o HostKey=/etc/ssh/ssh_host_ed25519_key -o PasswordAuthentication=no " +
		"-o KbdInteractiveAuthentication=no -o PermitRootLogin=prohibit-password -o PubkeyAuthentication=yes\n", nil
}

// leaseContainerSpec 开通 ④ 的容器参数(全部由平台计算)。
type leaseContainerSpec struct {
	LeaseNo   string
	Image     string
	UUIDs     []string
	CPUs      int
	MemGB     int
	DiskGB    int // 0 = 不设 --storage-opt(仅不支持配额的整机节点)
	Port      int
	InitShell string
}

// createContainerCommand 幂等建网络/数据卷, 删除同名旧容器(上次失败的半成品)后按 §25.5.2 规格启动容器。
func createContainerCommand(sp leaseContainerSpec) (string, error) {
	if !ValidLeaseNo(sp.LeaseNo) {
		return "", fmt.Errorf("租约号非法: %q", sp.LeaseNo)
	}
	c, v, nw := containerNameOf(sp.LeaseNo), volumeNameOf(sp.LeaseNo), networkNameOf(sp.LeaseNo)
	if err := checkLeaseRes(c, v, nw); err != nil {
		return "", err
	}
	if err := checkImage(sp.Image); err != nil {
		return "", err
	}
	if len(sp.UUIDs) == 0 {
		return "", fmt.Errorf("租约无卡")
	}
	g, err := gpusFlag(sp.UUIDs)
	if err != nil {
		return "", err
	}
	if err := checkPort(sp.Port); err != nil {
		return "", err
	}
	if sp.CPUs < 1 || sp.CPUs > 4096 || sp.MemGB < 1 || sp.MemGB > 65536 || sp.DiskGB < 0 || sp.DiskGB > 10000000 {
		return "", fmt.Errorf("容器资源参数非法: cpus=%d mem=%dG disk=%dG", sp.CPUs, sp.MemGB, sp.DiskGB)
	}
	if sp.InitShell == "" {
		return "", fmt.Errorf("容器入口脚本为空")
	}
	storage := ""
	if sp.DiskGB > 0 {
		storage = fmt.Sprintf(" --storage-opt size=%dG", sp.DiskGB)
	}
	label := "--label omnis.lease=" + sp.LeaseNo
	run := fmt.Sprintf("docker run -d --name %s %s --gpus %s --cpus %d --memory %dg --memory-swap %dg --pids-limit 8192 "+
		"--shm-size %dg%s -v %s:/root/workspace --network %s -p %d:22 --restart no --entrypoint /bin/sh %s -c '%s'",
		c, label, g, sp.CPUs, sp.MemGB, sp.MemGB, shmSizeGB(sp.MemGB), storage, v, nw, sp.Port, sp.Image, shB64(sp.InitShell))
	return "set -e; (docker network inspect " + nw + " >/dev/null 2>&1 || docker network create " + label + " " + nw + " >/dev/null); " +
		"docker volume create " + label + " " + v + " >/dev/null; docker rm -f " + c + " >/dev/null 2>&1 || true; " +
		run + " >/dev/null; echo @@created", nil
}

// hostKeyCommand 等容器 sshd 生成主机密钥后读出公钥(最多 30s); 失败时附容器状态与日志尾部便于排查。
func hostKeyCommand(leaseNo string) (string, error) {
	if !ValidLeaseNo(leaseNo) {
		return "", fmt.Errorf("租约号非法: %q", leaseNo)
	}
	c := containerNameOf(leaseNo)
	return "for i in $(seq 1 30); do " +
		"if [ \"$(docker inspect -f '{{.State.Running}}' " + c + " 2>/dev/null)\" = true ] && " +
		"docker exec " + c + " cat /etc/ssh/ssh_host_ed25519_key.pub 2>/dev/null; then exit 0; fi; sleep 1; done; " +
		"echo \"容器状态: $(docker inspect -f '{{.State.Status}} exit={{.State.ExitCode}}' " + c + " 2>&1)\" >&2; " +
		"docker logs --tail 20 " + c + " >&2 2>&1; exit 1", nil
}

// parseHostKeyFP 取 hostKeyCommand 输出中的 ed25519 公钥, 返回 SHA256 指纹。
func parseHostKeyFP(out string) (string, error) {
	for _, l := range nonEmptyLines(out) {
		k, _, _, _, err := ssh.ParseAuthorizedKey([]byte(l))
		if err == nil && k.Type() == ssh.KeyAlgoED25519 {
			return ssh.FingerprintSHA256(k), nil
		}
	}
	return "", fmt.Errorf("未读到容器 sshd 的 ed25519 主机公钥: %q", truncate(strings.TrimSpace(out), 120))
}

// sameUUIDSet 自检: 容器内可见 GPU 必须恰好是分配给租约的卡(不多不少)。
func sameUUIDSet(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	m := map[string]int{}
	for _, u := range want {
		m[u]++
	}
	for _, u := range got {
		if m[u] == 0 {
			return false
		}
		m[u]--
	}
	return true
}
