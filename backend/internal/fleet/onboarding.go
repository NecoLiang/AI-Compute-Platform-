package fleet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"strings"
	"time"

	"tokenfactory/pkg/errcode"

	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/ssh"
)

// 节点接入(docs/25 §25.4.2 ③-⑦): 上架说明、SSH 连接信息与一次性引导、主机指纹 TOFU 确认。

const fingerprintCheckCommand = "ssh-keygen -lf /etc/ssh/ssh_host_ed25519_key.pub"

// HostKeyState 指纹状态(供页面展示与比对)。
type HostKeyState struct {
	Fingerprint  *string    `json:"fingerprint"`
	Confirmed    bool       `json:"confirmed"`
	ConfirmedAt  *time.Time `json:"confirmed_at"`
	CheckCommand string     `json:"check_command"`
}

func hostKeyState(n *Node) HostKeyState {
	return HostKeyState{Fingerprint: n.HostKeyFingerprint, Confirmed: n.hostKeyConfirmed(),
		ConfirmedAt: n.HostKeyConfirmedAt, CheckCommand: fingerprintCheckCommand}
}

// Onboarding GET /supplier/nodes/:id/onboarding 返回内容。
type Onboarding struct {
	NodeID              int64             `json:"node_id"`
	VerifyStatus        string            `json:"verify_status"`
	AuthorizedKeysLine  string            `json:"authorized_keys_line"`
	ControllerEgressIPs []string          `json:"controller_egress_ips"`
	GatewayEgressIPs    []string          `json:"gateway_egress_ips"`
	LeasePortStart      int               `json:"lease_port_start"`
	LeasePortEnd        int               `json:"lease_port_end"`
	SSHHost             *string           `json:"ssh_host"`
	SSHPort             *int              `json:"ssh_port"`
	SSHUser             *string           `json:"ssh_user"`
	HostKey             HostKeyState      `json:"host_key"`
	FirewallScripts     map[string]string `json:"firewall_scripts"`
	Requirements        []string          `json:"requirements"`
}

// Onboarding ③: 管理公钥行(带 from= 限制)、控制面/网关出口 IP、按发行版生成的租约端口放行脚本。
func (s *Service) Onboarding(supplierID, nodeID int64) (*Onboarding, error) {
	n, err := s.getOwned(supplierID, nodeID)
	if err != nil {
		return nil, err
	}
	if n.MgmtPubkey == nil || *n.MgmtPubkey == "" {
		return nil, errcode.New(errcode.InternalError, "节点缺少管理公钥, 请删除节点后重新添加")
	}
	if len(s.gwEgress) == 0 {
		return nil, errcode.New(errcode.InternalError, "accessgw.egress_ips 未配置: 无法生成租约端口放行规则")
	}
	line, err := authorizedKeysLine(*n.MgmtPubkey, s.cfg.ControllerEgressIPs)
	if err != nil {
		return nil, errcode.Newf(errcode.InternalError, "生成管理公钥行失败: %v", err)
	}
	start, end := portRange(n)
	scripts, err := firewallScripts(s.gwEgress, start, end)
	if err != nil {
		return nil, errcode.Newf(errcode.InternalError, "生成端口放行脚本失败: %v", err)
	}
	return &Onboarding{
		NodeID: n.ID, VerifyStatus: n.VerifyStatus, AuthorizedKeysLine: line,
		ControllerEgressIPs: s.cfg.ControllerEgressIPs, GatewayEgressIPs: s.gwEgress,
		LeasePortStart: start, LeasePortEnd: end,
		SSHHost: n.SSHHost, SSHPort: n.SSHPort, SSHUser: n.SSHUser,
		HostKey: hostKeyState(n), FirewallScripts: scripts,
		Requirements: []string{
			"Linux x86_64 + systemd; NVIDIA 驱动 ≥ " + s.cfg.MinDriverVersion,
			"Docker ≥ 24 + nvidia-container-toolkit, docker run --gpus 可用",
			"管理账户须能执行 docker(docker 组或 root), 不要求 sudo; 注意 docker 组权限等同 root",
			"管理 SSH 端口对控制面出口 IP 可达; 租约端口段只对网关出口 IP 开放(见放行脚本)",
			"分卡出租要求 Docker 数据目录位于 xfs+pquota(--storage-opt size= 可用), 否则只能整机出租",
		},
	}, nil
}

func portRange(n *Node) (int, int) {
	start, end := defaultLeasePortStart, defaultLeasePortEnd
	if n.LeasePortStart != nil && n.LeasePortEnd != nil {
		start, end = *n.LeasePortStart, *n.LeasePortEnd
	}
	return start, end
}

// firewallScripts 租约端口段只放行网关出口。Docker 发布端口的流量走 FORWARD 而非 INPUT,
// 因此规则写在 DOCKER-USER 链, 并按 DNAT 前的原始目的端口(conntrack --ctorigdstport)匹配。
// 参数全部来自配置与数据库并经校验, 不含用户自由文本。
func firewallScripts(gwIPs []string, start, end int) (map[string]string, error) {
	if start < 1024 || end > 65535 || start > end {
		return nil, fmt.Errorf("端口段非法 %d-%d", start, end)
	}
	for _, ip := range gwIPs {
		if net.ParseIP(ip) == nil {
			return nil, fmt.Errorf("网关出口 IP 非法: %q", ip)
		}
	}
	var b strings.Builder
	b.WriteString("#!/bin/sh\nset -e\n")
	fmt.Fprintf(&b, "# 租约端口段 %d-%d 仅允许平台访问网关出口访问(幂等: 先删后加)\n", start, end)
	match := fmt.Sprintf("-p tcp -m conntrack --ctorigdstport %d:%d --ctdir ORIGINAL", start, end)
	fmt.Fprintf(&b, "iptables -D DOCKER-USER %s -j DROP 2>/dev/null || true\n", match)
	for _, ip := range gwIPs {
		fmt.Fprintf(&b, "iptables -D DOCKER-USER %s -s %s -j RETURN 2>/dev/null || true\n", match, ip)
	}
	fmt.Fprintf(&b, "iptables -I DOCKER-USER %s -j DROP\n", match)
	for _, ip := range gwIPs {
		fmt.Fprintf(&b, "iptables -I DOCKER-USER %s -s %s -j RETURN\n", match, ip)
	}
	rules := b.String()
	return map[string]string{
		"debian": rules + "# 持久化(Debian/Ubuntu)\napt-get install -y iptables-persistent && netfilter-persistent save\n",
		"rhel": rules + "# 持久化(RHEL/Rocky/Alma; 若使用 firewalld 请改用 firewall-cmd --direct 等价规则)\n" +
			"dnf install -y iptables-services && service iptables save\n",
	}, nil
}

var (
	sshUserRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	hostRe    = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,62})(\.[A-Za-z0-9]([A-Za-z0-9-]{0,62}))*$`)
)

// validateSSHTarget 校验连接信息(白名单字符), IP 字面量不得指向回环/链路本地/组播/未指定地址。
// allowLoopback 仅测试(Dialer.AllowLoopback)使用。域名在拨号时由 Dialer 再按解析结果拦截一次。
func validateSSHTarget(host string, port int, user string, allowLoopback bool) error {
	if port < 1 || port > 65535 {
		return errcode.New(errcode.ParamInvalid, "port 须在 1-65535 之间")
	}
	if !sshUserRe.MatchString(user) {
		return errcode.New(errcode.ParamInvalid, "user 非法: 须为小写字母/数字/下划线/短横线, 以字母或下划线开头, ≤32 位")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() && allowLoopback {
			return nil
		}
		if ip.IsLoopback() || ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() || ip.IsUnspecified() {
			return errcode.New(errcode.ParamInvalid, "host 不能是回环、链路本地、组播或未指定地址")
		}
		return nil
	}
	if len(host) > 253 || !hostRe.MatchString(host) {
		return errcode.New(errcode.ParamInvalid, "host 须为 IP 或合法域名")
	}
	if strings.EqualFold(host, "localhost") {
		return errcode.New(errcode.ParamInvalid, "host 不能是 localhost")
	}
	return nil
}

// SSHInput PUT /supplier/nodes/:id/ssh。BootstrapPassword 只在本次请求内存中使用一次。
type SSHInput struct {
	Host              string
	Port              int
	User              string
	BootstrapPassword string
}

// SSHUpdateResult PUT /ssh 返回。
type SSHUpdateResult struct {
	Node      *Node        `json:"node"`
	Bootstrap string       `json:"bootstrap"` // skipped / installed
	HostKey   HostKeyState `json:"host_key"`
}

// UpdateSSH ⑤ 保存连接信息, 节点回到 unverified; host/port 变化时清除指纹钉死。
// 带 bootstrap_password 时在保存后用它登录一次写入管理公钥, 然后丢弃; 登录失败直接返回错误, 不重试。
func (s *Service) UpdateSSH(ctx context.Context, supplierID, nodeID int64, in SSHInput, ip string) (*SSHUpdateResult, error) {
	in.Host, in.User = strings.TrimSpace(in.Host), strings.TrimSpace(in.User)
	if err := validateSSHTarget(in.Host, in.Port, in.User, s.dialer.AllowLoopback); err != nil {
		return nil, err
	}
	var pinned string
	err := s.inTx(func(tx *sqlx.Tx) error {
		n, err := s.ownedNode(tx, supplierID, nodeID)
		if err != nil {
			return err
		}
		if n.VerifyStatus == VerifySuspended {
			return errcode.New(errcode.Conflict, "节点已被运营停用, 不能修改连接信息")
		}
		old := guardOf(n)
		resetFP := old.Host != in.Host || old.Port != in.Port
		ok, err := s.repo.updateSSH(tx, nodeID, in.Host, in.Port, in.User, resetFP)
		if err != nil {
			return err
		}
		if !ok {
			return errcode.New(errcode.Conflict, "节点状态已变化, 请刷新后重试")
		}
		// 进行中的探活针对的是旧连接信息: 作废, 由供给方重新发起。
		if _, err := tx.Exec(`UPDATE node_tasks SET state='failed', last_error='连接信息已修改', locked_until=NULL
			WHERE node_id=? AND kind='probe' AND state='pending'`, nodeID); err != nil {
			return err
		}
		if err := s.repo.audit(tx, supplierID, "node.ssh_updated", "supplier_node", nodeID,
			map[string]any{"host": old.Host, "port": old.Port, "user": old.User},
			map[string]any{"host": in.Host, "port": in.Port, "user": in.User}, ip); err != nil {
			return err
		}
		if resetFP && n.HostKeyFingerprint != nil {
			if err := s.repo.audit(tx, supplierID, "node.host_key_reset", "supplier_node", nodeID,
				map[string]any{"fingerprint": *n.HostKeyFingerprint}, nil, ip); err != nil {
				return err
			}
		}
		if !resetFP && n.hostKeyConfirmed() {
			pinned = *n.HostKeyFingerprint
		}
		if n.VerifyStatus == VerifyVerified {
			return s.recomputeStockTx(tx, n.ProductID)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	n, _ := s.repo.GetNode(nodeID)
	if n != nil {
		s.refreshHealth(n)
	}
	res := &SSHUpdateResult{Bootstrap: "skipped"}
	if in.BootstrapPassword != "" {
		if err := s.bootstrap(ctx, supplierID, nodeID, in, pinned, ip); err != nil {
			return nil, err
		}
		res.Bootstrap = "installed"
	}
	if res.Node, err = s.repo.GetNode(nodeID); err != nil {
		return nil, err
	}
	if res.Node == nil {
		return nil, errNodeNotFound
	}
	res.HostKey = hostKeyState(res.Node)
	return res, nil
}

// bootstrap 用一次性密码登录, 幂等追加管理公钥行。密码不落库、不写日志、不进审计; 失败不重试。
func (s *Service) bootstrap(ctx context.Context, supplierID, nodeID int64, in SSHInput, pinned, ip string) error {
	n, err := s.repo.GetNode(nodeID)
	if err != nil {
		return err
	}
	if n == nil || n.MgmtPubkey == nil {
		return errNodeNotFound
	}
	line, err := authorizedKeysLine(*n.MgmtPubkey, s.cfg.ControllerEgressIPs)
	if err != nil {
		return errcode.Newf(errcode.InternalError, "生成管理公钥行失败: %v", err)
	}
	cmd, err := installKeyCommand(line, pubKeyBody(*n.MgmtPubkey))
	if err != nil {
		return errcode.Newf(errcode.InternalError, "生成公钥安装命令失败: %v", err)
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	fail := func(reason string) error {
		if aerr := s.repo.audit(s.repo.db, supplierID, "node.bootstrap_password_used", "supplier_node", nodeID,
			nil, map[string]any{"result": "failed", "reason": reason}, ip); aerr != nil {
			slog.Error("写引导审计失败", "node_id", nodeID, "error", aerr)
		}
		return errcode.New(errcode.ParamInvalid, "连接信息已保存, 但一次性密码登录安装公钥失败: "+reason+
			"。可改为手动把公钥行加入 authorized_keys 后直接发起探活")
	}
	d := *s.dialer
	d.Attempts = 1 // 一次性密码: 不重试
	conn, err := d.TCPCheck(ctx, in.Host, in.Port)
	if err != nil {
		return fail("管理端口不可达(" + err.Error() + ")")
	}
	pw := in.BootstrapPassword
	auth := []ssh.AuthMethod{
		ssh.Password(pw),
		ssh.KeyboardInteractive(func(_, _ string, questions []string, echos []bool) ([]string, error) {
			ans := make([]string, len(questions))
			for i := range questions {
				if !echos[i] {
					ans[i] = pw
				}
			}
			return ans, nil
		}),
	}
	client, info, err := d.Connect(conn, in.User, auth, pinned)
	if errors.Is(err, ErrHostKeyMismatch) {
		s.handleKeyMismatch(nodeID, pinned, info.Fingerprint)
		return errcode.New(errcode.Conflict, "主机指纹与已确认指纹不一致, 已停止调度; 若机器重装过系统, 请核对新指纹后确认")
	}
	if err != nil {
		return fail("SSH 登录失败(用户名或密码错误, 或服务器禁用了密码登录)")
	}
	defer client.Close()
	r, err := client.Run(ctx, cmd)
	if err != nil || r.ExitCode != 0 || !strings.Contains(r.Stdout, "@@installed") {
		return fail("写入 ~/.ssh/authorized_keys 失败(" + truncate(strings.TrimSpace(r.Stderr), 200) + ")")
	}
	if pinned == "" && info.Fingerprint != "" {
		if err := s.repo.recordObservedFingerprint(s.repo.db, nodeID, info.Fingerprint); err != nil {
			return err
		}
	}
	return s.repo.audit(s.repo.db, supplierID, "node.bootstrap_password_used", "supplier_node", nodeID,
		nil, map[string]any{"result": "installed", "host_key_fingerprint": info.Fingerprint}, ip)
}

// handleKeyMismatch P2 指纹与钉死值不一致: key_mismatch、停止新分配、通知供给方、写系统审计。
func (s *Service) handleKeyMismatch(nodeID int64, pinned, observed string) {
	var n *Node
	err := s.inTx(func(tx *sqlx.Tx) error {
		var err error
		if n, err = s.repo.lockNode(tx, nodeID); err != nil || n == nil {
			return err
		}
		ok, err := s.repo.markKeyMismatch(tx, nodeID, pinned, observed)
		if err != nil || !ok {
			n = nil
			return err
		}
		if err := s.repo.audit(tx, 0, "node.key_mismatch", "supplier_node", nodeID,
			map[string]any{"fingerprint": pinned}, map[string]any{"observed": observed}, ""); err != nil {
			return err
		}
		return s.recomputeStockTx(tx, n.ProductID)
	})
	if err != nil {
		slog.Error("处理主机指纹变化失败", "node_id", nodeID, "error", err)
		return
	}
	if n == nil {
		return
	}
	slog.Error("运营告警: 托管节点主机指纹变化, 已停止新分配", "node_id", nodeID, "pinned", pinned, "observed", observed)
	s.notify(n.SupplierID, "节点主机指纹发生变化",
		fmt.Sprintf("节点「%s」的 SSH 主机指纹与已确认值不一致(已确认 %s, 本次 %s), 平台已停止向其分配新租约。"+
			"如机器重装过系统, 请在机器上执行 %s 核对后在节点页确认新指纹。", n.NodeName, pinned, observed, fingerprintCheckCommand), nodeID)
	s.refreshHealth(n)
}

// ConfirmHostKey ⑦ 供给方核对后确认指纹: 与观测值一致才钉死, 然后入队完整探活。
func (s *Service) ConfirmHostKey(supplierID, nodeID int64, fingerprint, ip string) (int64, error) {
	fp := strings.TrimSpace(fingerprint)
	if fp == "" {
		return 0, errcode.New(errcode.ParamInvalid, "fingerprint 必填")
	}
	var taskID int64
	err := s.inTx(func(tx *sqlx.Tx) error {
		n, err := s.ownedNode(tx, supplierID, nodeID)
		if err != nil {
			return err
		}
		if n.VerifyStatus == VerifySuspended {
			return errcode.New(errcode.Conflict, "节点已被运营停用")
		}
		if n.HostKeyFingerprint == nil || *n.HostKeyFingerprint == "" {
			return errcode.New(errcode.Conflict, "平台尚未观测到该节点的主机指纹, 请先发起探活")
		}
		observed := *n.HostKeyFingerprint
		if fp != observed {
			return errcode.Newf(errcode.ParamInvalid, "指纹与平台观测值不一致(平台观测 %s), 请在机器上执行 %s 重新核对", observed, fingerprintCheckCommand)
		}
		if n.hostKeyConfirmed() {
			if t, err := s.repo.activeProbe(tx, nodeID); err == nil && t != nil {
				taskID = t.ID
			}
			return err
		}
		ok, err := s.repo.confirmHostKey(tx, nodeID, fp)
		if err != nil {
			return err
		}
		if !ok {
			return errcode.New(errcode.Conflict, "指纹状态已变化, 请刷新后重试")
		}
		action := "node.host_key_confirmed"
		if n.VerifyStatus == VerifyKeyMismatch {
			action = "node.host_key_reconfirmed"
		}
		if err := s.repo.audit(tx, supplierID, action, "supplier_node", nodeID,
			map[string]any{"verify_status": n.VerifyStatus}, map[string]any{"fingerprint": fp}, ip); err != nil {
			return err
		}
		if !n.sshReady() {
			return nil
		}
		taskID, err = s.enqueueProbeTx(tx, n, ProbeFull, "host_key_confirm", 5)
		return err
	})
	return taskID, err
}

// enqueueProbeTx 入队探活(同一节点已有未结束的 probe 则复用), 完整探活时 unverified/failed/key_mismatch → verifying。
func (s *Service) enqueueProbeTx(tx *sqlx.Tx, n *Node, mode, trigger string, priority int) (int64, error) {
	if mode == ProbeFull {
		if _, err := s.repo.setVerifyStatus(tx, n.ID,
			[]string{VerifyUnverified, VerifyFailed, VerifyKeyMismatch, VerifyNeedsReimage}, VerifyVerifying); err != nil {
			return 0, err
		}
	}
	if t, err := s.repo.activeProbe(tx, n.ID); err != nil || t != nil {
		if t != nil {
			return t.ID, nil
		}
		return 0, err
	}
	return s.repo.insertTask(tx, n.ID, KindProbe, priority, ProbePayload{Mode: mode, Trigger: trigger})
}
