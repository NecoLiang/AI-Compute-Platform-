package fleet

// 冻结/解冻的租约侧(docs/25 §25.7.3, S2 T-088)。均在 compute 的订单事务内执行(锁顺序 订单 → 租约 → 节点行)。
//
// 冻结: 在租(active/parked) → suspended, 记 suspended_at, 入队 kill_sessions(优先级 1, 快速领取)断开现有会话;
//       网关只放行 lease_state=active, 迁移同时刷新 access_changed_at, 新登录立即被拒。容器不停、卡不释放
//       (核查现场); 冻结中到期由到期扫描停容器释放卡、保留数据卷(expireSuspended)。
//       reserved/pending/provisioning: 租约不动, 开通流水线在 ⓪/⑥ 发现订单 frozen 即放弃。
// 解冻: 见 unfreezeLeaseTx。

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"tokenfactory/internal/compute"
	"tokenfactory/pkg/errcode"

	"github.com/jmoiron/sqlx"
)

func (s *Service) freezeLeaseTx(tx *sqlx.Tx, o *compute.Order) error {
	l, err := s.repo.lockLeaseByOrder(tx, o.ID)
	if err != nil {
		return err
	}
	if l == nil {
		return fmt.Errorf("托管订单 %s 缺少租约记录", o.OrderNo)
	}
	switch l.State {
	case LeaseActive, LeaseParked:
		if err := s.mustTransition(tx, l, LeaseSuspended, ""); err != nil {
			return err
		}
		live, err := s.repo.liveLeaseTasks(tx, l.ID, KindKillSessions)
		if err != nil {
			return err
		}
		if len(live) == 0 {
			if _, err := s.repo.insertLeaseTaskMax(tx, l.NodeID, l.ID, KindKillSessions, prioKillSessions, cleanupMaxAttempts,
				LeaseTaskPayload{LeaseID: l.ID, Reason: "订单冻结"}); err != nil {
				return err
			}
		}
		slog.Info("订单冻结: 租约已暂停, 入队断开会话", "lease_no", l.LeaseNo, "order_no", o.OrderNo)
	default:
		// 状态不变但订单凭证随冻结吊销(compute 同事务): 刷新撤销游标, 网关丢弃该登录名的缓存放行(防御性, 非 active 本就不放行)。
		if err := s.repo.touchAccess(tx, l.ID); err != nil {
			return err
		}
		slog.Info("订单冻结: 租约不在租, 只刷新撤销游标", "lease_no", l.LeaseNo, "state", l.State, "order_no", o.OrderNo)
	}
	return nil
}

// unfreezeLeaseTx 解冻: 按租约实况决定订单去向(compute 负责订单状态、凭证与释放路径)。
//
//	reserved                       → 订单 pending_payment(未付款冻结)
//	pending / provisioning         → 订单 paid; 租约 provisioning→pending, 无待领取的开通任务时重新入队 provision
//	                                  (开通任务正在执行时拒绝 40900, 待其因订单 frozen 放弃后再解冻)
//	active/parked/suspended 未到期 → 订单 active; suspended → active(清 suspended_at), 凭证由 compute 恢复
//	active/parked/suspended 已到期 → 订单 completed; compute 经 ReleaseTx 转 grace, 宽限自解冻起算
//	grace / purging / purged       → 订单 completed(租约侧已在收尾, ReleaseTx 幂等)
//	failed / cancelled             → 40900: 租约已作废, 订单只能关闭/退款
func (s *Service) unfreezeLeaseTx(tx *sqlx.Tx, o *compute.Order) (*compute.ManagedUnfreeze, error) {
	l, err := s.repo.lockLeaseByOrder(tx, o.ID)
	if err != nil {
		return nil, err
	}
	if l == nil {
		return nil, fmt.Errorf("托管订单 %s 缺少租约记录", o.OrderNo)
	}
	if err := s.lockNodeRow(tx, l.NodeID); err != nil {
		return nil, err
	}
	plan := &compute.ManagedUnfreeze{LeaseID: l.ID, SuspendedAt: l.SuspendedAt}
	switch l.State {
	case LeaseReserved:
		plan.OrderStatus = "pending_payment"
	case LeasePending, LeaseProvisioning:
		if err := s.requeueProvisionTx(tx, l); err != nil {
			return nil, err
		}
		plan.OrderStatus = "paid"
	case LeaseActive, LeaseParked, LeaseSuspended:
		if l.AccessUntil != nil && !l.AccessUntil.After(time.Now()) {
			plan.OrderStatus = "completed"
			break
		}
		if l.State == LeaseSuspended {
			if err := s.mustTransition(tx, l, LeaseActive, ""); err != nil {
				return nil, err
			}
		}
		plan.OrderStatus = "active"
	case LeaseGrace, LeasePurging, LeasePurged:
		plan.OrderStatus = "completed"
	default:
		return nil, errcode.Newf(errcode.Conflict, "租约 %s 已%s, 订单无法恢复, 请关闭订单或退款", l.LeaseNo, l.State)
	}
	slog.Info("订单解冻(租约侧)", "lease_no", l.LeaseNo, "lease_state", l.State, "order_no", o.OrderNo, "order_to", plan.OrderStatus)
	return plan, nil
}

// requeueProvisionTx 解冻后恢复开通: provisioning → pending; 已有待领取的开通任务(同节点)则沿用, 否则入队一条,
// 沿用最近一次开通任务的 Reallocated 标记(换节点只允许一次)。
func (s *Service) requeueProvisionTx(tx *sqlx.Tx, l *Lease) error {
	live, err := s.repo.liveLeaseTasks(tx, l.ID, KindProvision)
	if err != nil {
		return err
	}
	for _, t := range live {
		if t.State == "running" {
			return errcode.New(errcode.Conflict, "该订单的开通任务仍在执行(将因订单冻结自行放弃), 请稍后再解冻")
		}
	}
	if l.State == LeaseProvisioning {
		if err := s.mustTransition(tx, l, LeasePending, ""); err != nil {
			return err
		}
	}
	for _, t := range live {
		if t.NodeID == l.NodeID {
			return nil
		}
	}
	pl := LeaseProvisionPayload{LeaseID: l.ID}
	var raw []byte
	err = tx.Get(&raw, `SELECT payload FROM node_tasks WHERE lease_id=? AND kind=? ORDER BY id DESC LIMIT 1`, l.ID, KindProvision)
	switch {
	case err == nil:
		var prev LeaseProvisionPayload
		if err := json.Unmarshal(raw, &prev); err != nil {
			return fmt.Errorf("租约 %s 最近一次开通任务 payload 无法解析: %w", l.LeaseNo, err)
		}
		pl.Reallocated, pl.Renewal, pl.Resume = prev.Reallocated, prev.Renewal, prev.Resume
	case !errors.Is(err, sql.ErrNoRows):
		return err
	}
	_, err = s.repo.insertLeaseTask(tx, l.NodeID, l.ID, KindProvision, prioProvision, pl)
	return err
}
