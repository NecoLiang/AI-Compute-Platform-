package fleet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"tokenfactory/internal/compute"

	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/ssh"
)

// 直连映射访问模式(fleet.access_mode=direct)的服务端逻辑, 命令模板见 direct_cmds.go。
//
// sync_access 任务(express, 同一租约与其他任务互斥)在执行时读取租约当前状态决定动作, 因此入队方只需"有变化就入队":
//   - active: 写公钥 + 接入租约网络(端口映射恢复);
//   - pending/provisioning: 订单仍在开通中只写公钥(网络由开通流程负责); 订单已冻结/取消/退款 → 断开并结束会话;
//   - parked: 只写公钥(网络由包夜开窗负责);
//   - suspended: 从全部网络断开(冻结);
//   - purging: 断开并结束会话(purge 走普通车道, 可能排在同节点长任务之后, 入口先由快车道切断);
//   - 其他(到期/已清除/失败): 不需要(容器已停或已删), errTaskAborted。
//
// 入队时机: 买家添加/删除公钥(名下 pending/provisioning/active/parked/suspended 租约)、解冻回到 active、
// 开通中冻结(freezeLeaseTx)、进入清除(purgeTx)。
// 冻结由 kill_sessions 先断网再结束会话; 到期由 stop 停容器。

func (s *Service) directMode() bool { return !s.cfg.GatewayMode() }

// SyncAccessOutcome sync_access 任务结果(node_tasks.result)。
type SyncAccessOutcome struct {
	LeaseNo string           `json:"lease_no"`
	Mode    string           `json:"mode"`
	Result  AccessSyncResult `json:"result"`
	Keys    int              `json:"keys,omitempty"` // 整机: 写入 authorized_keys 的行数
}

const syncAccessExecTimeout = 3 * time.Minute

// platformAuthLine 写入租约(容器 platform_keys / 整机账户 authorized_keys)的平台管理公钥行: 节点管理公钥,
// from=控制面出口 IP, 禁止转发与 pty(平台只执行非交互命令)。
func (s *Service) platformAuthLine(n *Node) (string, error) {
	if n.MgmtPubkey == nil || *n.MgmtPubkey == "" {
		return "", fmt.Errorf("节点 #%d 缺少管理公钥", n.ID)
	}
	line, err := canonicalKeyLine(*n.MgmtPubkey)
	if err != nil {
		return "", fmt.Errorf("节点 #%d 管理公钥: %w", n.ID, err)
	}
	return authorizedKeysLine(line, s.cfg.ControllerEgressIPs)
}

// buyerKeyLinesOf 买家当前未吊销的公钥(按添加顺序)。
func (s *Service) buyerKeyLinesOf(buyerID int64) ([]string, error) {
	var keys []string
	if err := s.repo.db.Select(&keys, `SELECT public_key FROM buyer_ssh_keys WHERE user_id=? AND revoked_at IS NULL ORDER BY id`,
		buyerID); err != nil {
		return nil, err
	}
	return buyerKeyLines(keys)
}

// leaseInitScript 租约容器入口脚本(按访问模式)。
func (s *Service) leaseInitScript(n *Node) (string, error) {
	if !s.directMode() {
		return containerInitScript(s.gw.UpstreamPubkey, s.gwEgress)
	}
	platform, err := s.platformAuthLine(n)
	if err != nil {
		return "", err
	}
	return containerInitScriptDirect(platform)
}

// bareMetalAuthLines 整机租约账户 authorized_keys 的内容(按访问模式)。
func (s *Service) bareMetalAuthLines(n *Node, l *Lease) ([]string, error) {
	if !s.directMode() {
		line, err := gatewayAuthLine(s.gw.UpstreamPubkey, s.gwEgress)
		if err != nil {
			return nil, err
		}
		return []string{line}, nil
	}
	platform, err := s.platformAuthLine(n)
	if err != nil {
		return nil, err
	}
	buyer, err := s.buyerKeyLinesOf(l.BuyerID)
	if err != nil {
		return nil, err
	}
	return append([]string{platform}, buyer...), nil
}

// directEndpoint 直连模式买家(与平台自检)的登录目标: 容器租约 root@节点地址:租约端口(容器 sshd 指纹);
// 整机租约 omnis-<lease_no>@节点地址:管理端口(节点指纹)。端口与指纹取开通时写入租约的 upstream_port/upstream_host_key_fp。
func directEndpoint(l *Lease) (user string, port int, fp string, err error) {
	if l.UpstreamPort == nil || l.UpstreamHostKeyFP == nil || *l.UpstreamHostKeyFP == "" {
		return "", 0, "", fmt.Errorf("租约 %s 缺少登录端口或 sshd 主机指纹", l.LeaseNo)
	}
	user = "root"
	if l.DeliveryKind == "bare_metal" {
		if user, err = bareMetalUser(l.LeaseNo); err != nil {
			return "", 0, "", err
		}
	}
	return user, *l.UpstreamPort, *l.UpstreamHostKeyFP, nil
}

// managedAccess 交付记录里给买家的连接信息(按访问模式)。
func (s *Service) managedAccess(n *Node, l *Lease) compute.ManagedAccess {
	if !s.directMode() {
		return compute.ManagedAccess{Host: s.gw.PublicHost, Port: s.gw.PublicPort, Login: l.LeaseNo}
	}
	user, port, fp, err := directEndpoint(l)
	if err != nil {
		// 开通流程走到激活时端口与指纹必然已写入; 这里只防御, 交付记录仍写节点地址。
		slog.Error("直连交付信息不完整", "lease_no", l.LeaseNo, "error", err)
	}
	return compute.ManagedAccess{Host: deref(n.SSHHost), Port: port, Login: user,
		Note: fmt.Sprintf("平台托管租约: 直连 节点地址:端口, 仅支持账户中登记的 SSH 公钥登录(不支持密码); 主机指纹 %s。"+
			"到期、冻结时平台会切断连接。", fp)}
}

// syncContainerAccess 对容器租约执行一次访问同步(mode 见 direct_cmds.go)。connect 且容器运行中时, 映射端口必须等于
// 租约占用的端口。
func (s *Service) syncContainerAccess(ctx context.Context, client Runner, n *Node, l *Lease, mode string) (AccessSyncResult, error) {
	var keys []byte
	if mode != syncCut {
		platform, err := s.platformAuthLine(n)
		if err != nil {
			return AccessSyncResult{}, err
		}
		buyer, err := s.buyerKeyLinesOf(l.BuyerID)
		if err != nil {
			return AccessSyncResult{}, err
		}
		if keys, err = accessKeysTar(platform, buyer); err != nil {
			return AccessSyncResult{}, err
		}
	}
	cmd, err := accessSyncCommand(l.LeaseNo, keys, mode)
	if err != nil {
		return AccessSyncResult{}, errTaskAborted{err.Error()}
	}
	out, rerr := runRemote(ctx, client, cmd, cmdTimeout)
	res, perr := parseAccessSync(out)
	switch {
	case perr != nil && rerr != nil && out != "":
		return res, fmt.Errorf("同步租约访问失败: %v(%w)", perr, rerr)
	case rerr != nil:
		return res, fmt.Errorf("同步租约访问失败: %w", rerr)
	case perr != nil:
		return res, fmt.Errorf("同步租约访问失败: %w", perr)
	}
	if mode == syncConnect && res.Running && l.PortClaim != nil && res.Port != *l.PortClaim {
		return res, fmt.Errorf("容器 22 端口映射到 %d, 与租约端口 %d 不一致", res.Port, *l.PortClaim)
	}
	return res, nil
}

// syncBareMetalKeys 整机租约: 重写账户 authorized_keys(平台管理公钥 + 买家公钥; cut=true 时只留平台公钥)。
func (s *Service) syncBareMetalKeys(ctx context.Context, client Runner, n *Node, l *Lease, cut bool) (string, int, error) {
	lines, err := s.bareMetalAuthLines(n, l)
	if err != nil {
		return "", 0, err
	}
	if cut {
		lines = lines[:1]
	}
	script, err := bareMetalKeysScript(l.LeaseNo, lines)
	if err != nil {
		return "", 0, errTaskAborted{err.Error()}
	}
	tag, _, err := runHostRoot(ctx, client, s.cfg.ProbeImage, script, "同步整机租约公钥")
	if err != nil {
		return "", 0, err
	}
	if tag != "keys" && tag != "absent" {
		return "", 0, fmt.Errorf("同步整机租约公钥: 意外结论 %q", tag)
	}
	return tag, len(lines), nil
}

// directLogin 以节点管理密钥直连租约(节点地址:port, 用户 user, 钉死租约 sshd 主机指纹)执行 nvidia-smi -L。
// 走的是买家同一条入口(节点公网地址 + 映射端口), 用于开通自检与重启复检。
func (s *Service) directLogin(ctx context.Context, n *Node, user string, port int, hostFP string) (string, error) {
	if n.SSHHost == nil || n.MgmtPrivkeyEnc == nil {
		return "", fmt.Errorf("节点 #%d 缺少地址或管理私钥", n.ID)
	}
	if hostFP == "" {
		return "", fmt.Errorf("租约 sshd 主机指纹未记录")
	}
	conn, err := s.dialer.TCPCheck(ctx, *n.SSHHost, port)
	if err != nil {
		return "", fmt.Errorf("连接 %s:%d 失败(检查节点租约端口段是否对公网放行): %w", *n.SSHHost, port, err)
	}
	signer, err := loadNodeSigner(*n.MgmtPrivkeyEnc, s.credKey)
	if err != nil {
		_ = conn.Close()
		return "", fmt.Errorf("节点管理私钥不可用: %w", err)
	}
	client, info, err := s.dialer.Connect(conn, user, []ssh.AuthMethod{ssh.PublicKeys(signer)}, hostFP)
	if errors.Is(err, ErrHostKeyMismatch) {
		return "", fmt.Errorf("%s:%d 的主机指纹 %s 与租约记录 %s 不一致(端口上不是本租约的 sshd)", *n.SSHHost, port, info.Fingerprint, hostFP)
	}
	if err != nil {
		return "", fmt.Errorf("以平台密钥登录 %s@%s:%d 失败: %w", user, *n.SSHHost, port, err)
	}
	defer client.Close()
	out, err := runRemote(ctx, client, "nvidia-smi -L", cmdTimeout)
	if err != nil {
		return "", fmt.Errorf("直连执行 nvidia-smi -L 失败: %w", err)
	}
	return out, nil
}

// cutContainerAccess 冻结: 断开容器全部网络, 再结束容器内会话。断网失败返回错误(冻结必须生效, 由任务重试)。
func (s *Service) cutContainerAccess(ctx context.Context, client Runner, n *Node, l *Lease) (KillSessionsOutcome, error) {
	res := KillSessionsOutcome{LeaseNo: l.LeaseNo}
	sync, err := s.syncContainerAccess(ctx, client, n, l, syncCut)
	if err != nil {
		return res, err
	}
	if sync.Absent || !sync.Running {
		return res, nil
	}
	return s.killContainerSessions(ctx, client, l)
}

// ===== sync_access 任务 =====

func (s *Service) runSyncAccessTask(ctx context.Context, t *Task) (any, error) {
	if !s.directMode() {
		return nil, errTaskAborted{"当前为网关访问模式, 不使用 sync_access"}
	}
	pl, err := parseLeaseTask(t)
	if err != nil {
		return nil, err
	}
	l, err := s.repo.getLease(s.repo.db, pl.LeaseID)
	if err != nil {
		return nil, err
	}
	if l == nil {
		return nil, errTaskAborted{"租约不存在"}
	}
	if l.NodeID != t.NodeID {
		return nil, errTaskAborted{fmt.Sprintf("租约 %s 已改由节点 #%d 承载", l.LeaseNo, l.NodeID)}
	}
	mode, kill := "", false
	switch l.State {
	case LeaseActive:
		mode = syncConnect
	case LeasePending, LeaseProvisioning:
		// 开通中: 订单仍在开通(paid/provisioning)时只写公钥; 订单已冻结/取消/退款而容器可能已建好 → 断开。
		provisioning, err := s.orderProvisioning(l.OrderID)
		if err != nil {
			return nil, err
		}
		if mode = syncKeys; !provisioning {
			mode, kill = syncCut, true
		}
	case LeaseParked:
		mode = syncKeys
	case LeaseSuspended:
		mode = syncCut
	case LeasePurging:
		// 退款/取消/到期清除: purge 走普通车道, 可能排在同节点长任务之后; 先由快车道断开入口并结束会话。
		mode, kill = syncCut, true
	default:
		return nil, errTaskAborted{fmt.Sprintf("租约 %s 当前状态 %s, 无需同步访问", l.LeaseNo, l.State)}
	}
	n, err := s.repo.GetNode(t.NodeID)
	if err != nil {
		return nil, err
	}
	if n == nil {
		return nil, errTaskAborted{fmt.Sprintf("节点 #%d 已不存在", t.NodeID)}
	}
	client, err := s.dialLeaseNode(ctx, t.NodeID)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	out := SyncAccessOutcome{LeaseNo: l.LeaseNo, Mode: mode}
	if l.DeliveryKind == "bare_metal" {
		tag, keys, err := s.syncBareMetalKeys(ctx, client, n, l, mode == syncCut)
		if err != nil {
			return nil, err
		}
		out.Result.Absent, out.Keys = tag == "absent", keys
		if kill && tag != "absent" {
			if _, err := s.bareMetalKillSessions(ctx, client, l); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
	if out.Result, err = s.syncContainerAccess(ctx, client, n, l, mode); err != nil {
		return nil, err
	}
	if kill && out.Result.Running {
		if _, err := s.killContainerSessions(ctx, client, l); err != nil {
			return nil, err
		}
	}
	slog.Info("租约访问已同步", "lease_no", l.LeaseNo, "mode", mode, "running", out.Result.Running, "port", out.Result.Port)
	return out, nil
}

// orderProvisioning 租约的订单是否仍在开通中(paid/provisioning)。订单不存在按"不在开通中"处理。
func (s *Service) orderProvisioning(orderID int64) (bool, error) {
	var status string
	err := s.repo.db.Get(&status, `SELECT status FROM orders WHERE id=?`, orderID)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return status == "paid" || status == "provisioning", nil
}

// syncAccessStates 买家公钥变化时需要同步的租约状态。
var syncAccessStates = []string{LeasePending, LeaseProvisioning, LeaseActive, LeaseParked, LeaseSuspended}

// enqueueSyncAccessTx 入队一次 sync_access(直连模式; 网关模式无操作)。已有未开始的同类任务时不重复入队——
// 任务执行时才读取状态与公钥, 未开始的那一条会带上本次变化。
func (s *Service) enqueueSyncAccessTx(tx *sqlx.Tx, l *Lease, reason string) error {
	if !s.directMode() || l.NodeID == 0 {
		return nil
	}
	var n int
	if err := tx.Get(&n, `SELECT COUNT(*) FROM node_tasks WHERE lease_id=? AND node_id=? AND kind=? AND state='pending'`,
		l.ID, l.NodeID, KindSyncAccess); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := s.repo.insertLeaseTaskMax(tx, l.NodeID, l.ID, KindSyncAccess, prioKillSessions, cleanupMaxAttempts,
		LeaseTaskPayload{LeaseID: l.ID, Reason: reason})
	return err
}

// enqueueBuyerSyncTx 买家公钥变化: 名下需要同步的每个租约各入队一次。
func (s *Service) enqueueBuyerSyncTx(tx *sqlx.Tx, buyerID int64, reason string) error {
	if !s.directMode() {
		return nil
	}
	q, args, err := sqlx.In(`SELECT `+leaseColumns+` FROM leases WHERE buyer_id=? AND node_id<>0 AND state IN (?) ORDER BY id`,
		buyerID, syncAccessStates)
	if err != nil {
		return err
	}
	var list []Lease
	if err := tx.Select(&list, q, args...); err != nil {
		return err
	}
	for i := range list {
		if err := s.enqueueSyncAccessTx(tx, &list[i], reason); err != nil {
			return err
		}
	}
	return nil
}
