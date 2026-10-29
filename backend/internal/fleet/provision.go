package fleet

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"tokenfactory/internal/compute"

	"github.com/jmoiron/sqlx"
	"golang.org/x/crypto/ssh"
)

// 开通流水线(docs/25 §25.5.1, T-084)。provision 任务执行器, 每一步幂等, 失败按 worker 的指数退避重试,
// 重试耗尽由 onProvisionDead 换节点/退款(T-083)。
//
//	⓪ 订单 paid→provisioning, 租约 pending→provisioning(同一事务, 锁顺序 订单→租约)
//	① 校验节点与卡槽, 以钉死指纹建立管理连接
//	② 校验/下发 DOCKER-USER 规则(租约端口段只放行网关出口)
//	③ 基础镜像(按 digest)不在本地则拉取
//	④ 占用端口(uk_node_port) → 建网络/数据卷/容器 → 读取容器 sshd 主机指纹
//	⑤ 经网关公网入口以系统自检身份登录, nvidia-smi -L 的 UUID 集合必须恰为分配的卡; 主服务出口直连端口必须失败
//	⑥ 订单 active(写交付记录与 selftest_passed_at)、租约 active、卡槽 reserved→leased, 通知买家
//
// 不变式: 节点/网络问题一律返回可重试错误(最终走换节点/退款), 只有订单/租约已不需要开通时才 errTaskAborted。

// ProvisionOutcome 写入 node_tasks.result。
type ProvisionOutcome struct {
	LeaseNo           string   `json:"lease_no"`
	UpstreamPort      int      `json:"upstream_port,omitempty"`
	UpstreamHostKeyFP string   `json:"upstream_host_key_fp,omitempty"`
	Firewall          []string `json:"firewall,omitempty"`
	ImagePulled       bool     `json:"image_pulled,omitempty"`
	Abandoned         bool     `json:"abandoned,omitempty"` // 激活前订单已被取消/冻结, 容器由 purge 清理
}

// provisionRun 一次开通的上下文。
type provisionRun struct {
	s      *Service
	t      *Task
	l      *Lease
	n      *Node
	slots  []Slot
	client Runner
	out    ProvisionOutcome
}

func (s *Service) runProvisionTask(ctx context.Context, t *Task) (any, error) {
	var pl LeaseProvisionPayload
	if err := jsonUnmarshal(t.Payload, &pl); err != nil || pl.LeaseID == 0 {
		return nil, errTaskAborted{fmt.Sprintf("payload 无法解析: %v", err)}
	}
	p := &provisionRun{s: s, t: t}
	if err := p.begin(pl.LeaseID); err != nil {
		return nil, err
	}
	p.out.LeaseNo = p.l.LeaseNo
	if err := p.prepare(ctx); err != nil {
		return nil, err
	}
	defer p.client.Close()
	steps := []func(context.Context) error{p.firewall, p.image, p.container, p.selftest}
	for _, step := range steps {
		if err := step(ctx); err != nil {
			if ctx.Err() != nil {
				return nil, fmt.Errorf("开通被中断: %w(%v)", ctx.Err(), err)
			}
			return nil, err
		}
	}
	if err := p.activate(); err != nil {
		return nil, err
	}
	return p.out, nil
}

// begin ⓪。
func (p *provisionRun) begin(leaseID int64) error {
	s := p.s
	return s.inTx(func(tx *sqlx.Tx) error {
		var orderID int64
		if err := tx.Get(&orderID, `SELECT order_id FROM leases WHERE id=?`, leaseID); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return errTaskAborted{"租约不存在"}
			}
			return err
		}
		o, err := s.products.BeginManagedProvisioningTx(tx, orderID)
		if err != nil {
			return err
		}
		if o == nil {
			return errTaskAborted{"租约的订单不存在"}
		}
		if o.Status != "paid" && o.Status != "provisioning" {
			// 已取消/退款由 ReleaseTx 处理租约; 冻结(frozen)解冻后须重新入队开通(T-088)。
			return errTaskAborted{fmt.Sprintf("订单 %s 当前状态 %s, 不再开通", o.OrderNo, o.Status)}
		}
		l, err := s.repo.lockLease(tx, leaseID)
		if err != nil {
			return err
		}
		switch {
		case l == nil:
			return errTaskAborted{"租约不存在"}
		case l.NodeID != p.t.NodeID:
			return errTaskAborted{fmt.Sprintf("租约 %s 已改由节点 #%d 开通", l.LeaseNo, l.NodeID)}
		case l.DeliveryKind != "container":
			return errTaskAborted{"整机托管开通尚未接入(T-089)"}
		case l.State == LeasePending:
			if err := s.mustTransition(tx, l, LeaseProvisioning, ""); err != nil {
				return err
			}
		case l.State != LeaseProvisioning:
			return errTaskAborted{fmt.Sprintf("租约 %s 当前状态 %s, 不再开通", l.LeaseNo, l.State)}
		}
		p.l = l
		return nil
	})
}

// prepare ①: 节点状态、卡槽、管理连接。
func (p *provisionRun) prepare(ctx context.Context) error {
	s := p.s
	n, err := s.repo.GetNode(p.l.NodeID)
	if err != nil {
		return err
	}
	switch {
	case n == nil:
		return fmt.Errorf("节点 #%d 已不存在", p.l.NodeID)
	case n.VerifyStatus != VerifyVerified:
		return fmt.Errorf("节点 #%d 核验状态为 %s, 不能开通", n.ID, n.VerifyStatus)
	case n.Status != "online":
		return fmt.Errorf("节点 #%d 当前离线", n.ID)
	case !n.sshReady() || !n.hostKeyConfirmed() || n.MgmtPrivkeyEnc == nil:
		return fmt.Errorf("节点 #%d 缺少管理连接信息或主机指纹未确认", n.ID)
	case len(s.credKey) == 0:
		return errKeyUnavailable
	}
	p.n = n
	if p.slots, err = s.repo.leaseSlotsOnNode(s.repo.db, p.l.ID, n.ID); err != nil {
		return err
	}
	if len(p.slots) != p.l.Cards {
		return fmt.Errorf("租约 %s 在节点 #%d 上的卡槽数 %d 与租约卡数 %d 不一致", p.l.LeaseNo, n.ID, len(p.slots), p.l.Cards)
	}
	for _, sl := range p.slots {
		if sl.State != SlotReserved {
			return fmt.Errorf("租约 %s 的卡槽 GPU%d 状态为 %s(期望 reserved)", p.l.LeaseNo, sl.GPUIndex, sl.State)
		}
	}
	p.client, err = s.dialManaged(ctx, n)
	return err
}

// dialManaged 以钉死指纹建立管理连接; 指纹变化按 P2 处理(key_mismatch + 告警)并返回可重试错误。
func (s *Service) dialManaged(ctx context.Context, n *Node) (Runner, error) {
	conn, err := s.dialer.TCPCheck(ctx, *n.SSHHost, *n.SSHPort)
	if err != nil {
		return nil, fmt.Errorf("连接节点管理端口失败: %w", err)
	}
	signer, err := loadNodeSigner(*n.MgmtPrivkeyEnc, s.credKey)
	if err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("节点管理私钥不可用: %w", err)
	}
	pinned := *n.HostKeyFingerprint
	client, info, err := s.dialer.Connect(conn, *n.SSHUser, []ssh.AuthMethod{ssh.PublicKeys(signer)}, pinned)
	if errors.Is(err, ErrHostKeyMismatch) {
		s.handleKeyMismatch(n.ID, n.Generation, pinned, info.Fingerprint)
		return nil, fmt.Errorf("节点 #%d 主机指纹 %s 与已确认指纹不一致, 已停止调度", n.ID, info.Fingerprint)
	}
	if err != nil {
		return nil, fmt.Errorf("管理密钥登录节点 #%d 失败: %w", n.ID, err)
	}
	return client, nil
}

func runRemote(ctx context.Context, r Runner, cmd string, d time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	res, err := r.Run(ctx, cmd)
	if err != nil {
		return res.Stdout, err
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stderr)
		if msg == "" {
			msg = strings.TrimSpace(res.Stdout)
		}
		return res.Stdout, fmt.Errorf("退出码 %d: %s", res.ExitCode, truncate(msg, 300))
	}
	return res.Stdout, nil
}

// firewall ②。
func (p *provisionRun) firewall(ctx context.Context) error {
	rules, err := p.s.applyFirewall(ctx, p.client, p.n)
	if err != nil {
		return err
	}
	p.out.Firewall = append(p.out.Firewall, rules...)
	return nil
}

// applyFirewall 下发节点租约端口段来源限制(幂等; 开通 ② 与节点重启对账共用), 返回各后端的结论。
func (s *Service) applyFirewall(ctx context.Context, client Runner, n *Node) ([]string, error) {
	if _, err := ensureImageOn(ctx, client, s.cfg.ProbeImage); err != nil {
		return nil, fmt.Errorf("平台探针镜像(下发防火墙规则用): %w", err)
	}
	start, end := portRange(n)
	cmd, err := firewallCommand(s.cfg.ProbeImage, s.gwEgress, start, end)
	if err != nil {
		return nil, err
	}
	out, err := runRemote(ctx, client, cmd, cmdTimeout)
	if strings.Contains(out, "@@nochain") {
		return nil, fmt.Errorf("节点上没有 DOCKER-USER 链(Docker 未启用 iptables 管理), 无法限制租约端口来源")
	}
	if err != nil {
		return nil, fmt.Errorf("下发租约端口防火墙规则失败: %w", err)
	}
	var rules []string
	for _, l := range nonEmptyLines(out) {
		if strings.HasPrefix(l, "@@present ") || strings.HasPrefix(l, "@@applied ") {
			rules = append(rules, strings.TrimPrefix(l, "@@"))
		}
	}
	if len(rules) == 0 {
		return nil, fmt.Errorf("防火墙规则脚本无结论输出: %q", truncate(strings.TrimSpace(out), 200))
	}
	return rules, nil
}

// ensureImage 镜像不在本地时拉取(≤30min), 返回是否发生了拉取。
func (p *provisionRun) ensureImage(ctx context.Context, image string) (bool, error) {
	return ensureImageOn(ctx, p.client, image)
}

func ensureImageOn(ctx context.Context, client Runner, image string) (bool, error) {
	cmd, err := imagePresentCommand(image)
	if err != nil {
		return false, err
	}
	out, err := runRemote(ctx, client, cmd, cmdTimeout)
	if err != nil {
		return false, fmt.Errorf("检查镜像失败: %w", err)
	}
	if strings.Contains(out, "@@have") {
		return false, nil
	}
	if !strings.Contains(out, "@@missing") {
		return false, fmt.Errorf("检查镜像无结论输出: %q", truncate(strings.TrimSpace(out), 200))
	}
	pull, err := pullCommand(image)
	if err != nil {
		return false, err
	}
	if _, err := runRemote(ctx, client, pull, pullTimeout); err != nil {
		return false, fmt.Errorf("拉取镜像失败: %w", err)
	}
	return true, nil
}

// baseImage S2 只提供一个默认基础镜像(fleet.base_images 第一项); 买家选镜像属后续迭代。
func (s *Service) baseImage() (string, error) {
	if len(s.cfg.BaseImages) == 0 || s.cfg.BaseImages[0].Ref == "" {
		return "", fmt.Errorf("fleet.base_images 未配置, 无法开通容器")
	}
	return s.cfg.BaseImages[0].Ref, nil
}

// image ③。
func (p *provisionRun) image(ctx context.Context) error {
	img, err := p.s.baseImage()
	if err != nil {
		return err
	}
	pulled, err := p.ensureImage(ctx, img)
	if err != nil {
		return fmt.Errorf("租约基础镜像: %w", err)
	}
	p.out.ImagePulled = pulled
	return nil
}

// container ④。
func (p *provisionRun) container(ctx context.Context) error {
	s, l, n := p.s, p.l, p.n
	img, err := s.baseImage()
	if err != nil {
		return err
	}
	if n.CPUCores == nil || n.MemGB == nil {
		return fmt.Errorf("节点 #%d 缺少 CPU/内存实测值(须先通过完整探活)", n.ID)
	}
	cpus, mem, err := cpuMemSplit(*n.CPUCores, *n.MemGB, s.cfg.HostReserveCPU, s.cfg.HostReserveMemGB, l.Cards, n.TotalCards)
	if err != nil {
		return err
	}
	disk := 0
	switch {
	case n.DiskQuotaSupported != nil && *n.DiskQuotaSupported:
		if n.DiskPerCardGB == nil || *n.DiskPerCardGB <= 0 {
			return fmt.Errorf("节点 #%d 未设置每卡数据盘 disk_per_card_gb", n.ID)
		}
		disk = *n.DiskPerCardGB * l.Cards
	case l.Cards != n.TotalCards:
		return fmt.Errorf("节点 #%d 不支持磁盘配额(P10), 只能整机出租, 不能开通 %d/%d 卡", n.ID, l.Cards, n.TotalCards)
	}
	init, err := containerInitScript(s.gw.UpstreamPubkey, s.gwEgress)
	if err != nil {
		return err
	}
	port, err := p.claimPort(ctx)
	if err != nil {
		return err
	}
	uuids := make([]string, len(p.slots))
	for i, sl := range p.slots {
		uuids[i] = sl.GPUUUID
	}
	cmd, err := createContainerCommand(leaseContainerSpec{LeaseNo: l.LeaseNo, Image: img, UUIDs: uuids, CPUs: cpus,
		MemGB: mem, DiskGB: disk, Port: port, InitShell: init})
	if err != nil {
		return err
	}
	out, err := runRemote(ctx, p.client, cmd, 3*cmdTimeout)
	if err != nil || !strings.Contains(out, "@@created") {
		return fmt.Errorf("创建租约容器失败: %v", errText(err))
	}
	hk, err := hostKeyCommand(l.LeaseNo)
	if err != nil {
		return err
	}
	out, err = runRemote(ctx, p.client, hk, cmdTimeout)
	if err != nil {
		return fmt.Errorf("容器 sshd 未就绪: %w", err)
	}
	fp, err := parseHostKeyFP(out)
	if err != nil {
		return err
	}
	ok, err := affected(s.repo.db.Exec(`UPDATE leases SET upstream_host_key_fp=? WHERE id=? AND state='provisioning' AND node_id=?`,
		fp, l.ID, n.ID))
	if err != nil {
		return err
	}
	if !ok {
		var st string
		if err := s.repo.db.Get(&st, `SELECT state FROM leases WHERE id=?`, l.ID); err != nil {
			return err
		}
		if st != LeaseProvisioning {
			return errTaskAborted{fmt.Sprintf("租约 %s 已变为 %s, 停止开通(残留由 purge 清理)", l.LeaseNo, st)}
		}
		// 同值重写(重试)RowsAffected=0, 属正常。
	}
	p.out.UpstreamPort, p.out.UpstreamHostKeyFP = port, fp
	return nil
}

// claimPort 端口: 已占用(重试)沿用; 否则从端口段开头选第一个未被租约占用且未在监听的端口,
// 并在同一守卫式 UPDATE 中写入 container_name/volume_name(清理依据), 端口冲突由 uk_node_port 拦截。
func (p *provisionRun) claimPort(ctx context.Context) (int, error) {
	s, l, n := p.s, p.l, p.n
	port := 0
	if l.PortClaim == nil {
		out, err := runRemote(ctx, p.client, cmdPorts, cmdTimeout)
		if err != nil {
			return 0, fmt.Errorf("读取节点监听端口失败: %w", err)
		}
		leased, err := s.repo.leasedPorts(s.repo.db, n.ID)
		if err != nil {
			return 0, err
		}
		start, end := portRange(n)
		if port = pickLeasePort(start, end, leased, ParseListeningPorts(out)); port == 0 {
			return 0, fmt.Errorf("节点 #%d 租约端口段 %d-%d 已无空闲端口", n.ID, start, end)
		}
	} else {
		port = *l.PortClaim
	}
	_, err := s.repo.db.Exec(`UPDATE leases SET container_name=?, volume_name=?,
		upstream_port=COALESCE(port_claim, ?), port_claim=COALESCE(port_claim, ?)
		WHERE id=? AND state='provisioning' AND node_id=?`,
		containerNameOf(l.LeaseNo), volumeNameOf(l.LeaseNo), port, port, l.ID, n.ID)
	if isDuplicate(err) {
		return 0, fmt.Errorf("端口 %d 已被并发占用, 稍后重试", port)
	}
	if err != nil {
		return 0, err
	}
	// 同值重写时 RowsAffected=0, 因此以回读结果判定(状态已变 → 放弃)。
	var cur struct {
		State string `db:"state"`
		Port  *int   `db:"port_claim"`
	}
	if err := s.repo.db.Get(&cur, `SELECT state, port_claim FROM leases WHERE id=?`, l.ID); err != nil {
		return 0, err
	}
	if cur.State != LeaseProvisioning || cur.Port == nil {
		return 0, errTaskAborted{fmt.Sprintf("租约 %s 已变为 %s, 停止开通", l.LeaseNo, cur.State)}
	}
	cn, vn := containerNameOf(l.LeaseNo), volumeNameOf(l.LeaseNo)
	l.PortClaim, l.UpstreamPort, l.ContainerName, l.VolumeName = cur.Port, cur.Port, &cn, &vn
	return *cur.Port, nil
}

// selftest ⑤。网关未接入时明确失败, 不伪造通过。
func (p *provisionRun) selftest(ctx context.Context) error {
	if p.s.gateway == nil {
		return fmt.Errorf("访问网关(cmd/accessgw)未接入, 无法经网关做开通自检; 接入前不能交付")
	}
	out, err := p.s.gateway.SelfTestLease(ctx, p.l.LeaseNo)
	if err != nil {
		return fmt.Errorf("经网关自检登录失败: %w", err)
	}
	got := ParseGPUListL(out)
	want := make([]string, len(p.slots))
	for i, sl := range p.slots {
		want[i] = sl.GPUUUID
	}
	if !sameUUIDSet(got, want) {
		return fmt.Errorf("自检: 容器内可见 GPU %v 与分配的卡 %v 不一致", got, want)
	}
	host, port := *p.n.SSHHost, *p.l.PortClaim
	d := p.s.dialer.with(5*time.Second, 1)
	conn, err := d.TCPCheck(ctx, host, port)
	switch {
	case err == nil:
		_ = conn.Close()
		return fmt.Errorf("自检: 从平台主服务出口可直接连上 %s:%d, 租约端口未限制来源", host, port)
	case errors.Is(err, ErrAddrNotAllowed):
		// 连不上是因为目标地址被策略拒绝, 不能当作"端口未裸露"的证据。
		return fmt.Errorf("自检: 节点地址命中目标地址拒绝名单: %w", err)
	}
	return nil
}

// activate ⑥。
func (p *provisionRun) activate() error {
	s, l := p.s, p.l
	var order *compute.Order
	var end *time.Time
	err := s.inTx(func(tx *sqlx.Tx) error {
		var err error
		end, err = s.products.ActivateManagedOrderTx(tx, l.OrderID, time.Now(),
			compute.ManagedAccess{GatewayHost: s.gw.PublicHost, GatewayPort: s.gw.PublicPort, Login: l.LeaseNo})
		if err != nil || end == nil {
			return err
		}
		cur, err := s.repo.lockLease(tx, l.ID)
		if err != nil {
			return err
		}
		if cur == nil {
			return fmt.Errorf("租约 %s 不存在", l.LeaseNo)
		}
		if err := s.mustTransition(tx, cur, LeaseActive, ", access_until=?, provisioned_at=NOW(), grace_until=NULL", *end); err != nil {
			return err
		}
		moved, err := s.repo.moveLeaseSlots(tx, l.ID, []string{SlotReserved}, SlotLeased)
		if err != nil {
			return err
		}
		if int(moved) != l.Cards {
			return fmt.Errorf("租约 %s 卡槽 reserved→leased 只迁移了 %d/%d", l.LeaseNo, moved, l.Cards)
		}
		if _, err := s.repo.setResourceAlert(tx, l.ID, ""); err != nil {
			return err
		}
		order, err = s.products.LockManagedOrderTx(tx, l.OrderID)
		return err
	})
	if err != nil {
		return err
	}
	if end == nil {
		p.out.Abandoned = true
		slog.Info("开通完成前订单已不在开通中, 放弃激活(残留由 purge 清理)", "lease_no", l.LeaseNo)
		return nil
	}
	slog.Info("托管租约已开通", "lease_no", l.LeaseNo, "node_id", l.NodeID, "port", p.out.UpstreamPort)
	if order != nil {
		s.notifyLink(order.BuyerID, "算力已开通",
			fmt.Sprintf("订单 %s 已开通, 可在订单详情查看网关登录方式(登录名 %s)。", order.OrderNo, l.LeaseNo),
			"/console/buyer/orders/"+order.OrderNo)
	}
	return nil
}
