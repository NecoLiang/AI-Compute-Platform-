package fleet

import (
	"fmt"
	"log/slog"
	"time"

	"tokenfactory/internal/compute"
	"tokenfactory/pkg/errcode"

	"github.com/jmoiron/sqlx"
)

// 租约钩子(compute.LeaseHooks 的 fleet 实现, docs/25 §25.3 / docs/26 §26.5.2-26.5.3, T-082/T-092 子集)。
// 全部在 compute 的订单事务内执行, 任何一步失败整体回滚。

// 任务优先级(小者优先, docs/25 §25.9 node_tasks.priority)。
const (
	prioStop      = 1
	prioProvision = 3
	prioPurge     = 8
)

// LeaseHooks 供 main 注入 compute.SetLeaseHooks。RenewTx 留空: 托管续租属 T-088。
func (s *Service) LeaseHooks() compute.LeaseHooks {
	return compute.LeaseHooks{ReserveTx: s.reserveLeaseTx, OnPaidTx: s.onLeasePaidTx, ReleaseTx: s.releaseLeaseTx,
		Feasible: s.leaseFeasible}
}

// leaseFeasible 只读可行性(GET /products/:id/availability): 与 reserveSlotsTx 相同的候选规则
// (allocCandidates), 但不加锁不写库。整机交付(bare_metal)未接入, 由 compute 侧先行拦截。
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
	if kind == "bare_metal" {
		// 整机托管的开通(物理机交付、带外重装)属 T-089, 未接入前不接受下单, 避免收款后无法交付。
		return errcode.New(errcode.InternalError, "整机托管开通尚未接入(T-089), 暂不可下单")
	}
	if kind != "container" {
		return fmt.Errorf("托管商品交付方式非法: %q", kind)
	}
	l := &Lease{OrderID: o.ID, BuyerID: o.BuyerID, ProductID: p.ID, Cards: o.Quantity, DeliveryKind: kind}
	if err := s.repo.insertLease(tx, l); err != nil {
		return err
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
type LeaseProvisionPayload struct {
	LeaseID     int64 `json:"lease_id"`
	Reallocated bool  `json:"reallocated,omitempty"`
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

// graceFor 宽限期: 时/日计费 grace.short, 周/月计费 grace.long(docs/25 §25.7.1)。
func (s *Service) graceFor(pricingMode string) time.Duration {
	switch pricingMode {
	case compute.PricingWeekly, compute.PricingMonthly:
		return s.cfg.Grace.Long
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
//	已处于 grace/purging/终态   → 幂等无操作
//
// stop/purge 的执行器属 T-087, 未接入前任务保持 pending、卡槽保持 draining(不会被再次分配)。
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
			mode, err := compute.OrderPricingMode(o)
			if err != nil {
				return err
			}
			until := time.Now().Add(s.graceFor(mode))
			if err := s.mustTransition(tx, l, LeaseGrace, ", grace_until=?", until); err != nil {
				return err
			}
			if _, err := s.repo.moveLeaseSlots(tx, l.ID, []string{SlotLeased, SlotReserved}, SlotDraining); err != nil {
				return err
			}
			if _, err := s.repo.insertLeaseTask(tx, l.NodeID, l.ID, KindStop, prioStop, LeaseTaskPayload{LeaseID: l.ID, Reason: "租期结束"}); err != nil {
				return err
			}
		} else if err := s.purgeTx(tx, l, "订单"+to); err != nil {
			return err
		}
	default:
		slog.Info("租约已释放, 订单终态流转不再处理", "lease_no", l.LeaseNo, "state", l.State, "order_no", o.OrderNo, "to", to)
		return nil
	}
	return s.recomputeStockTx(tx, l.ProductID)
}

// purgeTx 租约 → purging, 卡槽 → draining, 入队 purge(宽限 0)。
func (s *Service) purgeTx(tx *sqlx.Tx, l *Lease, reason string) error {
	if err := s.mustTransition(tx, l, LeasePurging, ""); err != nil {
		return err
	}
	if _, err := s.repo.moveLeaseSlots(tx, l.ID, []string{SlotReserved, SlotLeased}, SlotDraining); err != nil {
		return err
	}
	_, err := s.repo.insertLeaseTask(tx, l.NodeID, l.ID, KindPurge, prioPurge, LeaseTaskPayload{LeaseID: l.ID, Reason: reason})
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
