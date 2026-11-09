package fleet

// 整机(bare_metal)托管的租约侧流转(T-089, docs/25 §25.7.4)。命令模板见 baremetal_cmds.go。

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jmoiron/sqlx"

	"tokenfactory/pkg/errcode"
)

// runHostRoot 确保探针镜像在节点上, 以特权容器 chroot 宿主机执行整机脚本并解析结论行。
// 脚本自带结论标记(@@missing 等)时优先返回可读错误, 否则返回远端执行错误。
func runHostRoot(ctx context.Context, client Runner, image, script, what string) (tag, val string, err error) {
	if _, err := ensureImageOn(ctx, client, image); err != nil {
		return "", "", fmt.Errorf("%s: 平台探针镜像: %w", what, err)
	}
	cmd, err := hostRootCommand(image, script)
	if err != nil {
		return "", "", errTaskAborted{err.Error()}
	}
	out, rerr := runRemote(ctx, client, cmd, cmdTimeout)
	tag, val, perr := parseBareMetalOut(out)
	switch {
	case perr != nil && rerr != nil && out != "":
		return "", "", fmt.Errorf("%s失败: %v(%w)", what, perr, rerr)
	case rerr != nil:
		return "", "", fmt.Errorf("%s失败: %w", what, rerr)
	case perr != nil:
		return "", "", fmt.Errorf("%s失败: %w", what, perr)
	}
	return tag, val, nil
}

// bareMetalTeardown 撤销整机租约账户: keep=true 只撤权限并结束进程(冻结中到期保留现场), 否则连同家目录删除。
// 返回结论(locked/removed/absent)与被结束的进程数。
func (s *Service) bareMetalTeardown(ctx context.Context, client Runner, leaseNo string, keep bool) (string, string, error) {
	script, err := bareMetalTeardownScript(leaseNo, keep)
	if err != nil {
		return "", "", errTaskAborted{err.Error()}
	}
	tag, val, err := runHostRoot(ctx, client, s.cfg.ProbeImage, script, "撤销整机租约账户")
	if err != nil {
		return "", "", err
	}
	switch tag {
	case "absent", "removed":
	case "locked":
		if !keep {
			return "", "", fmt.Errorf("撤销整机租约账户: 意外结论 %q", tag)
		}
	default:
		return "", "", fmt.Errorf("撤销整机租约账户: 意外结论 %q", tag)
	}
	return tag, val, nil
}

// bareMetalPurgeSteps purge/cleanup_only 的整机版本: 删除租约账户(含家目录、sudoers、网关公钥)。
func (s *Service) bareMetalPurgeSteps(ctx context.Context, nodeID int64, leaseNo string) ([]PurgeStep, error) {
	user, err := bareMetalUser(leaseNo)
	if err != nil {
		return nil, errTaskAborted{err.Error()}
	}
	client, err := s.dialLeaseNode(ctx, nodeID)
	if err != nil {
		return nil, err
	}
	defer client.Close()
	tag, _, err := s.bareMetalTeardown(ctx, client, leaseNo, false)
	if err != nil {
		return nil, fmt.Errorf("清除步骤 account 执行失败: %w", err)
	}
	return []PurgeStep{{Step: "account", Target: user, Outcome: tag}}, nil
}

const (
	bareMetalPortRulesNote = "整机租约无端口段规则; 上游为宿主机 sshd, 网关公钥带 from=网关出口 IP 限制, 已随账户删除"
	bareMetalErasureNote   = "删除租约账户、家目录、sudoers 授权与网关公钥并结束账户进程; 买家以 root 身份留下的其他内容不在清除范围, " +
		"节点已转待重装(needs_reimage), 由供给方重装系统后经重装确认与全量探测才能再售"
)

// bareMetalReceipt 整机清除回执: 不涉及容器/数据卷/租约网络。
func (s *Service) bareMetalReceipt(l *Lease, nodeID int64, steps []PurgeStep, reason string) PurgeReceipt {
	r := s.receiptFor(l, nodeID, steps, reason)
	r.Container, r.Volume, r.Network, r.CommitImage = "", "", "", ""
	r.PortRules, r.Erasure = bareMetalPortRulesNote, bareMetalErasureNote
	r.Account, _ = bareMetalUser(l.LeaseNo)
	return r
}

// markBareMetalReimageTx 整机租约结束(买家曾持有 root): 节点置重装闸门 → needs_reimage, 记审计。
// 闸门在供给方提交重装确认(新主机指纹)前挡住一切回到 verified 的路径(见 repository.markVerified)。
// 调用方已锁租约与节点行; 重复调用幂等(闸门时间与旧指纹只在第一次记录)。
func (s *Service) markBareMetalReimageTx(tx *sqlx.Tx, l *Lease, reason string) error {
	if err := s.repo.markNeedsReimage(tx, l.NodeID); err != nil {
		return err
	}
	return s.repo.audit(tx, 0, "node.needs_reimage", "supplier_node", l.NodeID, nil,
		map[string]any{"lease_no": l.LeaseNo, "reason": reason}, "")
}

// bareMetalKillSessions 冻结断会话(整机): 只结束租约账户的 sshd 会话进程, 买家其他进程保留作现场。
func (s *Service) bareMetalKillSessions(ctx context.Context, client Runner, l *Lease) (any, error) {
	script, err := bareMetalKillSessionsScript(l.LeaseNo)
	if err != nil {
		return nil, errTaskAborted{err.Error()}
	}
	tag, val, err := runHostRoot(ctx, client, s.cfg.ProbeImage, script, "断开整机租约会话")
	if err != nil {
		return nil, err
	}
	res := KillSessionsOutcome{LeaseNo: l.LeaseNo}
	switch tag {
	case "absent":
		return res, nil
	case "killed":
		if _, err := fmt.Sscanf(val, "%d", &res.Killed); err != nil {
			return nil, fmt.Errorf("断开会话输出无法解析: %q", truncate(val, 40))
		}
		res.Running = true
		slog.Info("冻结整机租约会话已断开", "lease_no", l.LeaseNo, "killed", res.Killed)
		return res, nil
	}
	return nil, fmt.Errorf("断开整机租约会话: 意外结论 %q", tag)
}

// notifyReimage 整机租约清除完成后提醒供给方重装系统并提交重装确认(新主机指纹)。
func (s *Service) notifyReimage(nodeID int64, leaseNo string) {
	n, err := s.repo.getNode(s.repo.db, nodeID)
	if err != nil || n == nil {
		slog.Error("整机待重装通知: 读取节点失败", "node_id", nodeID, "error", err)
		return
	}
	s.notify(n.SupplierID, "整机节点待重装",
		fmt.Sprintf("节点「%s」上的整机租约 %s 已结束, 租约账户已删除。买家在租期内持有 root 权限, 请重装操作系统并"+
			"重新安装管理公钥后, 在节点页提交重装确认(填写重装后的新主机指纹); 平台全量探测通过后节点恢复可售。", n.NodeName, leaseNo), nodeID)
}

// ReimageConfirm 供给方确认整机节点已重装(T-089, docs/25 §25.7.4): 必须提交重装后的新主机指纹
// (expected_host_fingerprint), 且不得等于租期内使用的旧指纹(主机密钥未变 = 未重装, 40001)。
// 成功后: 新指纹直接钉死(之后的连接按它严格校验)、清除重装闸门、代次 +1(重装前的探活/任务结论作废),
// 并入队完整探活; 探活通过 → verified 可售(markVerified 再次核对指纹已变)。
//
// 节点上仍有未结束租约时: purging 的整机租约随重装一并结案(账户已随系统重装清除, 不再连旧系统删除),
// 冻结核查中(suspended)或其他在用租约 → 40900(现场需保留/仍在服务, 不能重装确认)。
func (s *Service) ReimageConfirm(supplierID, nodeID int64, expectedFP, ip string) (int64, error) {
	if expectedFP == "" {
		return 0, errcode.New(errcode.ParamInvalid, "expected_host_fingerprint 必填: 请在重装后的机器上执行 "+
			fingerprintCheckCommand+" 并填写输出的第二列")
	}
	fp, err := parseExpectedFingerprint(expectedFP)
	if err != nil {
		return 0, err
	}
	if _, err := s.getOwned(supplierID, nodeID); err != nil {
		return 0, err
	}
	var taskID int64
	var finalized []string
	err = s.inTx(func(tx *sqlx.Tx) error {
		// 锁顺序: 租约 → 节点行 → 卡槽 → 商品行。
		var ids []int64
		q, args, err := sqlx.In(`SELECT id FROM leases WHERE node_id=? AND state NOT IN (?) ORDER BY id`, nodeID, leaseTerminal)
		if err != nil {
			return err
		}
		if err := tx.Select(&ids, q, args...); err != nil {
			return err
		}
		var purging []*Lease
		for _, id := range ids {
			l, err := s.repo.lockLease(tx, id)
			if err != nil {
				return err
			}
			if l == nil || l.NodeID != nodeID || containsStr(leaseTerminal, l.State) {
				continue
			}
			switch {
			case l.State == LeasePurging && l.DeliveryKind == "bare_metal":
				purging = append(purging, l)
			case l.State == LeaseSuspended:
				return errcode.Newf(errcode.Conflict, "节点上的租约 %s 处于冻结核查中, 现场须保留, 待运营结案后再重装确认", l.LeaseNo)
			default:
				return errcode.Newf(errcode.Conflict, "节点上的租约 %s 尚未结束(%s), 不能重装确认", l.LeaseNo, l.State)
			}
		}
		n, err := s.ownedNode(tx, supplierID, nodeID)
		if err != nil {
			return err
		}
		gated := n.ReimageRequiredAt != nil
		if n.VerifyStatus != VerifyNeedsReimage && !(gated && n.VerifyStatus == VerifyKeyMismatch) {
			return errcode.New(errcode.Conflict, "仅待重装(needs_reimage)的节点可确认重装")
		}
		old := n.ReimagePrevFP
		if old == nil && n.hostKeyConfirmed() {
			old = n.HostKeyFingerprint
		}
		if old != nil && fp == *old {
			return errReusedHostKey
		}
		for _, l := range purging {
			if err := s.finalizeReimagedLeaseTx(tx, l); err != nil {
				return err
			}
			finalized = append(finalized, l.LeaseNo)
		}
		ok, err := affected(tx.Exec(`UPDATE supplier_nodes SET host_key_fingerprint=?, host_key_confirmed_at=NOW(), host_fp_claim=NULL,
			reimage_required_at=NULL, generation=generation+1
			WHERE id=? AND mgmt_mode='ssh' AND (verify_status='needs_reimage' OR (verify_status='key_mismatch' AND reimage_required_at IS NOT NULL))`,
			fp, nodeID))
		if err != nil {
			return err
		}
		if !ok {
			return errcode.New(errcode.Conflict, "节点状态已变化, 请刷新后重试")
		}
		before := map[string]any{"verify_status": n.VerifyStatus, "fingerprint": n.HostKeyFingerprint}
		after := map[string]any{"fingerprint": fp, "finalized_leases": finalized}
		if err := s.repo.audit(tx, supplierID, "node.reimage_confirmed", "supplier_node", nodeID, before, after, ip); err != nil {
			return err
		}
		if len(purging) > 0 {
			if err := s.recomputeStockTx(tx, n.ProductID); err != nil {
				return err
			}
		}
		if !n.sshReady() {
			return nil
		}
		taskID, err = s.enqueueProbeTx(tx, n, ProbeFull, "reimage_confirm", 5)
		return err
	})
	if err != nil {
		return 0, err
	}
	if len(finalized) > 0 {
		slog.Info("整机重装确认: 未完成清除的租约随重装结案", "node_id", nodeID, "leases", finalized)
	}
	return taskID, nil
}

const reimageFinalizeReason = "节点已由供给方重装确认(新主机指纹), 租约账户随系统重装清除"

// finalizeReimagedLeaseTx purging 的整机租约随节点重装结案: → purged, 释放 draining 卡槽, 未领取的 stop/purge 作废,
// 写清除回执(步骤 account=reimaged)。在跑的收尾任务提交时发现租约已 purged 自行跳过。调用方已锁租约与节点行。
func (s *Service) finalizeReimagedLeaseTx(tx *sqlx.Tx, l *Lease) error {
	if err := s.mustTransition(tx, l, LeasePurged, ", purged_at=NOW(), commit_image=NULL"); err != nil {
		return err
	}
	if _, err := s.repo.moveLeaseSlotsOnNode(tx, l.ID, l.NodeID, []string{SlotDraining}, SlotFree); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE node_tasks SET state='failed', last_error=?, locked_until=NULL
		WHERE lease_id=? AND node_id=? AND kind IN (?, ?) AND state='pending'`,
		"租约已随节点重装结案", l.ID, l.NodeID, KindStop, KindPurge); err != nil {
		return err
	}
	user, _ := bareMetalUser(l.LeaseNo)
	receipt := s.bareMetalReceipt(l, l.NodeID, []PurgeStep{{Step: "account", Target: user, Outcome: "reimaged"}}, reimageFinalizeReason)
	receipt.Executor = "supplier_reimage"
	return s.repo.audit(tx, 0, "lease.purged", "lease", l.ID, nil, receipt, "")
}
