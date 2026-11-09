package fleet

// 托管续租的租约侧(compute.LeaseHooks.RenewTx, docs/25 §25.7.2, T-088)。
//
//	extend  租约在租(active/parked)且 access_until 未到: 收款即顺延 access_until(网关按新时刻放行), 退款回退。
//	restart 宽限期内(grace): 只能回原节点(数据卷在那里), 原卡优先, 不足时同机补空闲卡; 收款后租约 → pending,
//	        开通任务 Resume=true(同卡 docker start, 换卡 commit 后重建)。
//	        宽限期后(purged): 复用同一租约行, 按商品全部节点重新分配, 全新开通。
//
// 调用方(compute)已按锁顺序锁订单与交易配置; 本文件继续按 租约 → 节点行 → 卡槽 → 商品行 加锁。

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"tokenfactory/internal/compute"
	"tokenfactory/pkg/errcode"

	"github.com/jmoiron/sqlx"
)

var (
	errRestartNodeBusy = errcode.New(errcode.Conflict, "原节点暂无足够空闲卡或暂不可调度, 宽限期内只能在原节点恢复(数据卷在原节点); 可稍后再试或重新下单")
	errRestartPurging  = errcode.New(errcode.Conflict, "租约数据清除中, 请稍后再续租(清除完成后按全新环境开通)")
)

func (s *Service) renewLeaseTx(tx *sqlx.Tx, ev string, parent *compute.Order, r *compute.Renewal) error {
	if parent == nil || r == nil {
		return fmt.Errorf("续租参数缺失")
	}
	l, err := s.repo.lockLeaseByOrder(tx, parent.ID)
	if err != nil {
		return err
	}
	if l == nil {
		return fmt.Errorf("托管订单 %s 缺少租约记录", parent.OrderNo)
	}
	switch r.Mode {
	case "extend":
		return s.renewExtendTx(tx, ev, l, r)
	case "restart":
		return s.renewRestartTx(tx, ev, l)
	}
	return fmt.Errorf("未知续租方式: %q", r.Mode)
}

// ===== extend =====

func (s *Service) renewExtendTx(tx *sqlx.Tx, ev string, l *Lease, r *compute.Renewal) error {
	inLease := (l.State == LeaseActive || l.State == LeaseParked) && l.AccessUntil != nil && l.AccessUntil.After(time.Now())
	switch ev {
	case compute.RenewCheck, compute.RenewReserve, compute.RenewVerify:
		if !inLease {
			return compute.ErrRenewalConflict
		}
		return nil
	case compute.RenewRelease:
		return nil
	case compute.RenewApply:
		if !inLease {
			return compute.ErrRenewalConflict
		}
		if r.RenewedUntil == nil {
			return fmt.Errorf("续租单缺少 renewed_until")
		}
		ok, err := affected(tx.Exec(`UPDATE leases SET access_until=?, access_changed_at=NOW()
			WHERE id=? AND state IN ('active','parked')`, *r.RenewedUntil, l.ID))
		if err != nil {
			return err
		}
		if !ok {
			return compute.ErrRenewalConflict
		}
		return nil
	case compute.RenewRevert:
		ok, err := affected(tx.Exec(`UPDATE leases SET access_until=?, access_changed_at=NOW()
			WHERE id=? AND state IN ('active','parked','suspended')`, r.LeaseEndAt, l.ID))
		if err != nil {
			return err
		}
		if !ok {
			return errcode.Newf(errcode.Conflict, "租约 %s 已%s, 无法回退续租时长, 请联系平台处理", l.LeaseNo, l.State)
		}
		return nil
	}
	return fmt.Errorf("未知续租事件: %q", ev)
}

// ===== restart =====

func (s *Service) renewRestartTx(tx *sqlx.Tx, ev string, l *Lease) error {
	switch ev {
	case compute.RenewCheck:
		return s.restartCheckTx(tx, l)
	case compute.RenewReserve:
		return s.restartReserveTx(tx, l)
	case compute.RenewVerify:
		return s.restartVerifyTx(tx, l)
	case compute.RenewRelease:
		return s.restartReleaseTx(tx, l)
	case compute.RenewApply:
		return s.restartApplyTx(tx, l)
	case compute.RenewRevert:
		return s.restartRevertTx(tx, l)
	}
	return fmt.Errorf("未知续租事件: %q", ev)
}

// restartNodeEligible 宽限期 restart 的原节点条件: 与分配候选一致(ssh 纳管、已验证、可调度、在线)。lock=true 时锁节点行。
func restartNodeEligible(q sqlx.Queryer, l *Lease, lock bool) (bool, error) {
	query := `SELECT COUNT(*) FROM supplier_nodes WHERE id=? AND product_id=? AND mgmt_mode='ssh'
		AND verify_status='verified' AND schedulable=1 AND status='online'`
	if lock {
		query += ` FOR UPDATE`
	}
	var n int
	err := sqlx.Get(q, &n, query, l.NodeID, l.ProductID)
	return n > 0, err
}

const restartSlotCols = `id, node_id, gpu_index, gpu_uuid, pci_bus_id, vram_mb, topo_group, state, lease_id, state_changed_at`

// restartSlotsOnNode 原节点上本租约仍持有的卡(draining: 宽限期 stop 尚未执行; reserved: 已预留)与空闲卡。
func restartSlotsOnNode(q sqlx.Queryer, l *Lease, lock bool) ([]Slot, error) {
	query := `SELECT ` + restartSlotCols + ` FROM gpu_slots WHERE node_id=?
		AND (state='free' OR lease_id=? AND state IN ('draining','reserved')) ORDER BY gpu_index`
	if lock {
		query += ` FOR UPDATE`
	}
	var list []Slot
	err := sqlx.Select(q, &list, query, l.NodeID, l.ID)
	return list, err
}

// lastStopGPUs 宽限期 stop 记录的容器原卡(StopOutcome.GPUs); 尚未停过返回 nil。
func lastStopGPUs(q sqlx.Queryer, l *Lease) ([]string, error) {
	var raw []byte
	err := sqlx.Get(q, &raw, `SELECT result FROM node_tasks WHERE lease_id=? AND node_id=? AND kind=? AND state='succeeded'
		ORDER BY id DESC LIMIT 1`, l.ID, l.NodeID, KindStop)
	if errors.Is(err, sql.ErrNoRows) || len(raw) == 0 {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out StopOutcome
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, nil // 结果不是 StopOutcome(例如中止说明): 无原卡信息, 不影响选卡正确性
	}
	return out.GPUs, nil
}

// pickRestartSlots 宽限期 restart 选卡: ① 本租约仍持有的卡; ② 容器原卡中已空闲的; ③ 其余空闲卡按拓扑规则补足。
// 优先原卡是为了能直接 docker start(换卡需要 commit 重建)。不足 k 返回 nil。
func pickRestartSlots(slots []Slot, leaseID int64, k int, orig []string) []Slot {
	if k <= 0 {
		return nil
	}
	origSet := map[string]bool{}
	for _, u := range orig {
		origSet[u] = true
	}
	var own, origFree, rest []Slot
	for _, sl := range slots {
		switch {
		case sl.LeaseID != nil && *sl.LeaseID == leaseID && (sl.State == SlotDraining || sl.State == SlotReserved):
			own = append(own, sl)
		case sl.State != SlotFree:
		case origSet[sl.GPUUUID]:
			origFree = append(origFree, sl)
		default:
			rest = append(rest, sl)
		}
	}
	byIndex := func(list []Slot) {
		sort.Slice(list, func(i, j int) bool { return list[i].GPUIndex < list[j].GPUIndex })
	}
	byIndex(own)
	byIndex(origFree)
	out := append([]Slot(nil), own...)
	out = append(out, origFree...)
	if len(out) >= k {
		return out[:k]
	}
	more := pickSlots(rest, k-len(out))
	if more == nil {
		return nil
	}
	return append(out, more...)
}

func (s *Service) restartCheckTx(tx *sqlx.Tx, l *Lease) error {
	switch l.State {
	case LeaseGrace:
		ok, err := restartNodeEligible(tx, l, false)
		if err != nil || !ok {
			if err == nil {
				err = errRestartNodeBusy
			}
			return err
		}
		slots, err := restartSlotsOnNode(tx, l, false)
		if err != nil {
			return err
		}
		orig, err := lastStopGPUs(tx, l)
		if err != nil {
			return err
		}
		if pickRestartSlots(slots, l.ID, l.Cards, orig) == nil {
			return errRestartNodeBusy
		}
		return nil
	case LeasePurged:
		cands, err := s.repo.allocCandidates(tx, l.ProductID, l.Cards, l.DeliveryKind == "bare_metal", 0)
		if err != nil {
			return err
		}
		if len(cands) == 0 {
			return ErrNoWholeNode
		}
		return nil
	case LeasePurging:
		return errRestartPurging
	}
	return compute.ErrRenewalConflict
}

func (s *Service) restartReserveTx(tx *sqlx.Tx, l *Lease) error {
	switch l.State {
	case LeaseGrace:
		ok, err := restartNodeEligible(tx, l, true)
		if err != nil || !ok {
			if err == nil {
				err = errRestartNodeBusy
			}
			return err
		}
		slots, err := restartSlotsOnNode(tx, l, true)
		if err != nil {
			return err
		}
		orig, err := lastStopGPUs(tx, l)
		if err != nil {
			return err
		}
		picked := pickRestartSlots(slots, l.ID, l.Cards, orig)
		if picked == nil {
			return errRestartNodeBusy
		}
		ids := make([]int64, len(picked))
		for i, sl := range picked {
			ids[i] = sl.ID
		}
		q, args, err := sqlx.In(`UPDATE gpu_slots SET state='reserved', lease_id=?, state_changed_at=NOW()
			WHERE id IN (?) AND (state='free' OR lease_id=? AND state IN ('draining','reserved'))`, l.ID, ids, l.ID)
		if err != nil {
			return err
		}
		res, err := tx.Exec(q, args...)
		if err != nil {
			return err
		}
		// 已是 reserved 的行值不变, MySQL 默认 RowsAffected 不计; 以计数复核。
		if _, err := res.RowsAffected(); err != nil {
			return err
		}
		if n, err := s.reservedOnNode(tx, l); err != nil || n != l.Cards {
			if err == nil {
				err = fmt.Errorf("租约 %s 原节点预留 %d 张, 期望 %d", l.LeaseNo, n, l.Cards)
			}
			return err
		}
		// 未选中的本租约 draining 卡(换卡场景下多余的旧卡)保持 draining, 由宽限期 stop 释放。
	case LeasePurged:
		nodeID, _, err := s.repo.reserveSlotsTx(tx, l.ProductID, l.Cards, l.DeliveryKind == "bare_metal", l.ID, 0)
		if err != nil {
			return err
		}
		// 租约行已在本事务内锁定且确认 purged; 新节点可能与原节点相同(值不变时 RowsAffected=0), 不以行数判定。
		if _, err := tx.Exec(`UPDATE leases SET node_id=? WHERE id=? AND state='purged'`, nodeID, l.ID); err != nil {
			return err
		}
		l.NodeID = nodeID
	case LeasePurging:
		return errRestartPurging
	default:
		return compute.ErrRenewalConflict
	}
	return s.recomputeStockTx(tx, l.ProductID)
}

func (s *Service) reservedOnNode(tx *sqlx.Tx, l *Lease) (int, error) {
	var n int
	err := tx.Get(&n, `SELECT COUNT(*) FROM gpu_slots WHERE lease_id=? AND node_id=? AND state='reserved'`, l.ID, l.NodeID)
	return n, err
}

func (s *Service) restartVerifyTx(tx *sqlx.Tx, l *Lease) error {
	if l.State != LeaseGrace && l.State != LeasePurged {
		return compute.ErrRenewalConflict
	}
	n, err := s.reservedOnNode(tx, l)
	if err != nil {
		return err
	}
	if n != l.Cards {
		return compute.ErrRenewalConflict
	}
	return nil
}

// restartReleaseTx 未支付的 restart 续租单关闭: 宽限期内预留退回 draining 并确保 stop(由 stop 释放为 free);
// 宽限期后直接 free。租约已不在 grace/purged(例如已被清除)时无可退回, 幂等返回。
func (s *Service) restartReleaseTx(tx *sqlx.Tx, l *Lease) error {
	if l.State != LeaseGrace && l.State != LeasePurged {
		return nil
	}
	if err := s.lockNodeRow(tx, l.NodeID); err != nil {
		return err
	}
	if l.State == LeaseGrace {
		n, err := s.repo.moveLeaseSlots(tx, l.ID, []string{SlotReserved}, SlotDraining)
		if err != nil {
			return err
		}
		if n > 0 {
			if err := s.ensureStopTx(tx, l, "restart 续租未支付, 释放预留"); err != nil {
				return err
			}
		}
	} else if _, err := s.repo.moveLeaseSlots(tx, l.ID, []string{SlotReserved}, SlotFree); err != nil {
		return err
	}
	return s.recomputeStockTx(tx, l.ProductID)
}

// restartApplyTx 收款落账: 租约 → pending 并入队开通。grace_until 保留(开通失败/退款回到宽限期时沿用)。
func (s *Service) restartApplyTx(tx *sqlx.Tx, l *Lease) error {
	if err := s.restartVerifyTx(tx, l); err != nil {
		return err
	}
	resume := l.State == LeaseGrace
	if resume {
		if err := s.mustTransition(tx, l, LeasePending, ", access_until=NULL"); err != nil {
			return err
		}
	} else if err := s.mustTransition(tx, l, LeasePending, `, access_until=NULL, grace_until=NULL, purged_at=NULL,
		provisioned_at=NULL, container_name=NULL, volume_name=NULL, upstream_host_key_fp=NULL, upstream_port=NULL,
		commit_image=NULL, fail_reason=NULL`); err != nil {
		return err
	}
	_, err := s.repo.insertLeaseTask(tx, l.NodeID, l.ID, KindProvision, prioProvision,
		LeaseProvisionPayload{LeaseID: l.ID, Renewal: true, Resume: resume})
	return err
}

// restartRevertTx 未使用的 restart 续租退款(开通完成前): 宽限期内恢复 → 回到宽限期(数据保留到原 grace_until);
// 宽限期后全新开通 → 清除。租约已失败/已回到宽限期(开通失败善后已处理)时幂等。
func (s *Service) restartRevertTx(tx *sqlx.Tx, l *Lease) error {
	switch l.State {
	case LeasePending, LeaseProvisioning:
	case LeaseGrace, LeaseFailed, LeasePurging, LeasePurged, LeaseCancelled:
		return nil
	default:
		return errcode.Newf(errcode.Conflict, "租约 %s 已%s, 续租已开始使用, 请联系平台处理退款", l.LeaseNo, l.State)
	}
	if err := s.lockNodeRow(tx, l.NodeID); err != nil {
		return err
	}
	resume, err := s.leaseResuming(tx, l)
	if err != nil {
		return err
	}
	if resume {
		err = s.backToGraceTx(tx, l, "restart 续租退款")
	} else {
		err = s.purgeTx(tx, l, "restart 续租退款")
	}
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE node_tasks SET state='failed', last_error=?, locked_until=NULL
		WHERE lease_id=? AND kind=? AND state='pending'`, "restart 续租已退款", l.ID, KindProvision); err != nil {
		return err
	}
	return s.recomputeStockTx(tx, l.ProductID)
}

// leaseResuming 最近一次开通任务是否为宽限期内原节点恢复(Resume)。
func (s *Service) leaseResuming(q sqlx.Queryer, l *Lease) (bool, error) {
	var raw []byte
	err := sqlx.Get(q, &raw, `SELECT payload FROM node_tasks WHERE lease_id=? AND kind=? ORDER BY id DESC LIMIT 1`, l.ID, KindProvision)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	var pl LeaseProvisionPayload
	if err := json.Unmarshal(raw, &pl); err != nil {
		return false, fmt.Errorf("租约 %s 最近一次开通任务 payload 无法解析: %w", l.LeaseNo, err)
	}
	return pl.Resume, nil
}

// backToGraceTx 宽限期内 restart 未能完成(退款/开通失败): 租约回到 grace, grace_until 沿用续租前的值
// (已过则下一轮宽限期扫描清除); 卡槽 → draining 并确保 stop(可能已 docker start)。调用方已锁节点行。
func (s *Service) backToGraceTx(tx *sqlx.Tx, l *Lease, reason string) error {
	extra := ""
	var args []any
	if l.GraceUntil == nil {
		// 防御: 续租落账保留了 grace_until, 理论上不会为空; 为空时立即到期, 不凭空延长保留期。
		extra, args = ", grace_until=NOW()", nil
	}
	if err := s.mustTransition(tx, l, LeaseGrace, ", access_until=NULL"+extra, args...); err != nil {
		return err
	}
	if _, err := s.repo.moveLeaseSlots(tx, l.ID, []string{SlotReserved, SlotLeased}, SlotDraining); err != nil {
		return err
	}
	slog.Info("restart 续租未完成, 租约回到宽限期", "lease_no", l.LeaseNo, "reason", reason)
	return s.ensureStopTx(tx, l, reason)
}
