package fleet

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"tokenfactory/internal/compute"
	"tokenfactory/internal/modeldeploy"

	"github.com/jmoiron/sqlx"
)

// 租约钩子(compute.LeaseHooks 的 fleet 实现, docs/25 §25.3 / docs/26 §26.5.2-26.5.3, T-082/T-092 子集)。
// 全部在 compute 的订单事务内执行, 任何一步失败整体回滚。

// 任务优先级(小者优先, docs/25 §25.9 node_tasks.priority)。
const (
	prioStop         = 1
	prioKillSessions = 1
	prioProvision    = 3
	prioPurge        = 8
)

// cleanupMaxAttempts stop/purge/kill_sessions 的最大尝试次数(释放卡槽、断开访问不应因节点短暂失联很快转死信;
// 退避 1m,2m,4m…封顶 1h, 10 次合计约 6 小时)。
const cleanupMaxAttempts = 10

// LeaseHooks 供 main 注入 compute.SetLeaseHooks。
func (s *Service) LeaseHooks() compute.LeaseHooks {
	return compute.LeaseHooks{ReserveTx: s.reserveLeaseTx, OnPaidTx: s.onLeasePaidTx, ReleaseTx: s.releaseLeaseTx, RenewTx: s.renewLeaseTx,
		Feasible: s.leaseFeasible, FreezeTx: s.freezeLeaseTx, UnfreezeTx: s.unfreezeLeaseTx, WrongFreeze: s.wrongFreeze,
		NightFeasible: s.nightFeasible, NightCalendar: s.nightCalendar}
}

// wrongFreeze 误冻结停机交给停机认定(T-105); 未注入时只记日志, 不自行开/关 lease_outages。
func (s *Service) wrongFreeze(leaseID int64, frozenAt, unfrozenAt time.Time) {
	if s.outage != nil {
		s.outage.WrongFreeze(context.Background(), leaseID, frozenAt, unfrozenAt)
		return
	}
	slog.Warn("误冻结停机(停机认定 T-105 未接入, 仅记录)", "lease_id", leaseID, "frozen_at", frozenAt, "unfrozen_at", unfrozenAt)
}

// leaseFeasible 只读可行性(GET /products/:id/availability): 与 reserveSlotsTx 相同的候选规则
// (allocCandidates), 但不加锁不写库。整机交付(bare_metal)要求整机全部空闲且卡数 = 节点总卡数。
func (s *Service) leaseFeasible(p *compute.Product, cards int) (bool, error) {
	if p == nil || cards <= 0 {
		return false, nil
	}
	cands, err := s.repo.allocCandidates(s.repo.db, p.ID, cards, p.DeliveryMode == "bare_metal", 0)
	if err != nil {
		return false, err
	}
	return len(cands) > 0, nil
}

// reserveLeaseTx 下单: 租约 reserved + 同机 k 卡预留 + 派生库存重算。卡数 = 订单 quantity。
func (s *Service) reserveLeaseTx(tx *sqlx.Tx, o *compute.Order, p *compute.Product) error {
	if o.ID == 0 {
		return fmt.Errorf("订单尚未写入, 无法建租约")
	}
	kind := p.DeliveryMode
	if kind != "container" && kind != "bare_metal" {
		return fmt.Errorf("托管商品交付方式非法: %q", kind)
	}
	l := &Lease{OrderID: o.ID, BuyerID: o.BuyerID, ProductID: p.ID, Cards: o.Quantity, DeliveryKind: kind}
	if err := s.repo.insertLease(tx, l); err != nil {
		return err
	}
	if o.PricingMode != nil && *o.PricingMode == compute.PricingNightly {
		return s.reserveNightLeaseTx(tx, o, p, l)
	}
	nodeID, _, err := s.repo.reserveSlotsTx(tx, p.ID, o.Quantity, kind == "bare_metal", l.ID, 0)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE leases SET node_id=? WHERE id=? AND state='reserved'`, nodeID, l.ID); err != nil {
		return err
	}
	return s.recomputeStockTx(tx, p.ID)
}

// LeaseProvisionPayload provision 任务参数。Reallocated=true 表示已换过一次节点(docs/25 §25.5.1 重试耗尽规则)。
//
// Renewal=true: restart 续租触发的重新开通(T-088), 开通彻底失败时退的是续租单而不是原订单。
// Resume=true: 宽限期内原节点重新开通, 沿用保留的容器/数据卷(同卡 docker start, 换卡 commit 后重建);
// 数据在原节点上, 不换节点, 失败回到宽限期。
type LeaseProvisionPayload struct {
	LeaseID     int64 `json:"lease_id"`
	Reallocated bool  `json:"reallocated,omitempty"`
	Renewal     bool  `json:"renewal,omitempty"`
	Resume      bool  `json:"resume,omitempty"`
}

// LeaseTaskPayload stop/purge/kill_sessions 任务参数。
//
// CleanupOnly=true(开通失败换节点/失败善后, T-083 入队): 只清理任务所在节点上的残留容器/数据卷/网络
// (ContainerName/VolumeName 为入队时的名字), 完成后把本租约在该节点上的 draining 卡槽释放为 free,
// 不迁移租约状态(租约此时已在别的节点 pending 或已 failed)。执行器属 T-087。
type LeaseTaskPayload struct {
	LeaseID       int64  `json:"lease_id"`
	Reason        string `json:"reason,omitempty"`
	CleanupOnly   bool   `json:"cleanup_only,omitempty"`
	ContainerName string `json:"container_name,omitempty"`
	VolumeName    string `json:"volume_name,omitempty"`
}

// onLeasePaidTx 收款确认: 租约 reserved → pending, 入队 provision(优先级 3)。
func (s *Service) onLeasePaidTx(tx *sqlx.Tx, o *compute.Order) error {
	l, err := s.repo.lockLeaseByOrder(tx, o.ID)
	if err != nil {
		return err
	}
	if l == nil {
		return fmt.Errorf("托管订单 %s 缺少租约记录", o.OrderNo)
	}
	ok, err := s.repo.transitionLease(tx, l.ID, LeasePending, "")
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("托管订单 %s 的租约状态为 %s, 无法进入开通", o.OrderNo, l.State)
	}
	_, err = s.repo.insertLeaseTask(tx, l.NodeID, l.ID, KindProvision, prioProvision, LeaseProvisionPayload{LeaseID: l.ID})
	return err
}

// graceFor 宽限期(Q-33): 时/日计费 grace.short, 周/月计费 grace.long, 包夜 grace.nightly(docs/25 §25.7.1)。
// 宽限期内不收存储费。
func (s *Service) graceFor(pricingMode string) time.Duration {
	switch pricingMode {
	case compute.PricingWeekly, compute.PricingMonthly:
		return s.cfg.Grace.Long
	case compute.PricingNightly:
		return s.cfg.Grace.Nightly
	default:
		return s.cfg.Grace.Short
	}
}

// releaseLeaseTx 订单进入终态时释放租约(docs/26 §26.5.3):
//
//	reserved/pending            → cancelled, 卡槽 reserved→free(尚未建容器)
//	provisioning                → purging, 卡槽 →draining, 入队 purge(可能已建出部分容器)
//	active/parked/suspended + completed            → grace(宽限计时), 卡槽 leased→draining, 入队 stop
//	active/parked/suspended + cancelled/refunded   → purging(宽限 0), 卡槽 →draining, 入队 purge
//	grace + cancelled/refunded  → purging(退款中到期进入宽限, 退款完成后不再保留数据), 入队 purge
//	已处于 grace(completed)/purging/终态 → 幂等无操作
//
// stop/purge/kill_sessions 执行器见 lease_tasks.go(T-087): 容器停止/清除完成后卡槽 draining → free。
func (s *Service) releaseLeaseTx(tx *sqlx.Tx, o *compute.Order, to string) error {
	l, err := s.repo.lockLeaseByOrder(tx, o.ID)
	if err != nil {
		return err
	}
	if l == nil {
		return fmt.Errorf("托管订单 %s 缺少租约记录", o.OrderNo)
	}
	// 锁顺序: 订单 → 租约 → 节点行 → 卡槽 → 商品行(见 reserveSlotsTx)。
	if err := s.lockNodeRow(tx, l.NodeID); err != nil {
		return err
	}
	// 包夜预订(§26.6.3): 关单/取消删除全部; 退款/到期只删尚未开始的晚(已开始的晚保留作履约记录)。
	switch l.State {
	case LeaseReserved, LeasePending:
		err = s.releaseNightsTx(tx, l.ID, true)
	case LeaseProvisioning, LeaseActive, LeaseParked, LeaseSuspended, LeaseGrace:
		err = s.releaseNightsTx(tx, l.ID, false)
	}
	if err != nil {
		return err
	}
	switch l.State {
	case LeaseReserved, LeasePending:
		if err := s.mustTransition(tx, l, LeaseCancelled, ""); err != nil {
			return err
		}
		if _, err := s.repo.moveLeaseSlots(tx, l.ID, []string{SlotReserved}, SlotFree); err != nil {
			return err
		}
		// 已入队尚未领取的开通任务作废; 已在执行的由开通流水线在步骤 ⓪ 锁租约时发现非 pending 而放弃。
		if _, err := tx.Exec(`UPDATE node_tasks SET state='failed', last_error=?, locked_until=NULL
			WHERE lease_id=? AND kind=? AND state='pending'`, "租约已取消(订单"+to+")", l.ID, KindProvision); err != nil {
			return err
		}
	case LeaseProvisioning:
		if err := s.purgeTx(tx, l, "开通中订单"+to); err != nil {
			return err
		}
	case LeaseActive, LeaseParked, LeaseSuspended:
		if to == "completed" {
			// 宽限期按订单计费方式快照(S3 T-091)取档, 不读商品当前展示主价; 快照缺失报 50000。
			if err := s.enterGraceTx(tx, l, o, "租期结束"); err != nil {
				return err
			}
		} else if err := s.purgeTx(tx, l, "订单"+to); err != nil {
			return err
		}
	case LeaseGrace:
		if to == "completed" {
			return nil
		}
		if err := s.purgeTx(tx, l, "宽限期内订单"+to); err != nil {
			return err
		}
	default:
		slog.Info("租约已释放, 订单终态流转不再处理", "lease_no", l.LeaseNo, "state", l.State, "order_no", o.OrderNo, "to", to)
		return nil
	}
	return s.recomputeStockTx(tx, l.ProductID)
}

// purgeTx 租约 → purging, 卡槽 → draining, 入队 purge(宽限 0)。
// 整机租约若买家已拿到过 root(active/parked/suspended/grace), 节点同时转 needs_reimage(T-089, docs/25 §25.7.4):
// 删账户清不掉买家以 root 留下的东西, 必须供给方重装确认后才能再售。调用方已锁节点行。
func (s *Service) purgeTx(tx *sqlx.Tx, l *Lease, reason string) error {
	from := l.State
	if err := s.mustTransition(tx, l, LeasePurging, ""); err != nil {
		return err
	}
	if err := s.stopDeploymentsTx(tx, l.ID, modeldeploy.StopLeasePurged); err != nil {
		return err
	}
	if l.DeliveryKind == "bare_metal" {
		switch from {
		case LeaseActive, LeaseParked, LeaseSuspended, LeaseGrace:
			if err := s.markBareMetalReimageTx(tx, l, reason); err != nil {
				return err
			}
		}
	}
	if _, err := s.repo.moveLeaseSlots(tx, l.ID, []string{SlotReserved, SlotLeased}, SlotDraining); err != nil {
		return err
	}
	_, err := s.repo.insertLeaseTaskMax(tx, l.NodeID, l.ID, KindPurge, prioPurge, cleanupMaxAttempts, LeaseTaskPayload{LeaseID: l.ID, Reason: reason})
	return err
}

func (s *Service) mustTransition(tx *sqlx.Tx, l *Lease, to, extraSet string, extraArgs ...any) error {
	if !CanTransitionLease(l.State, to) {
		return errIllegalLease(l.State, to)
	}
	ok, err := s.repo.transitionLease(tx, l.ID, to, extraSet, extraArgs...)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("租约 %s 状态已变化(期望 %s)", l.LeaseNo, l.State)
	}
	l.State = to
	return nil
}

// lockNodeRow 节点行锁(卡槽变更前的串行点)。节点已被删除时无行可锁, 不报错。
func (s *Service) lockNodeRow(tx *sqlx.Tx, nodeID int64) error {
	var ids []int64
	return tx.Select(&ids, `SELECT id FROM supplier_nodes WHERE id=? FOR UPDATE`, nodeID)
}
