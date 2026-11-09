package fleet

import (
	"errors"
	"fmt"
	"log/slog"

	"tokenfactory/internal/compute"

	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

// 开通重试耗尽后的善后(docs/25 §25.5.1, T-083):
//
//	第一次耗尽(Reallocated=false): 同商品其他节点重新分配一次 —— 租约 provisioning/pending → pending,
//	    新节点按 docs/26 §26.5.2 预留, node_id 更新, 入队新的 provision(Reallocated=true);
//	第二次耗尽或已无可用节点: 释放卡槽、租约 → failed、订单 paid/provisioning → cancelled,
//	    由 payment.queueCancelledPaidRefunds 自动全额原路退款; 通知买家与供给方, 写运营告警。
//
// 原节点卡槽: 尚未建出容器(container_name 为空)时直接 reserved→free; 已开始建容器的转 draining 并在原节点
// 入队 CleanupOnly 的 purge 任务(执行器属 T-087), 清理完成前这些卡不会被再次分配。
// 锁顺序与其他路径一致: 订单 → 租约 → 节点行 → 卡槽 → 商品行。

const failoverDeadlockRetries = 3

// onProvisionDead provision 执行器的 onDead 回调。
func (s *Service) onProvisionDead(t *Task, lastErr string) {
	var pl LeaseProvisionPayload
	if err := jsonUnmarshal(t.Payload, &pl); err != nil || pl.LeaseID == 0 {
		slog.Error("运营告警: 开通任务死信 payload 无法解析, 需人工处理", "task_id", t.ID, "error", err)
		return
	}
	var out failoverOutcome
	var err error
	for i := 0; i < failoverDeadlockRetries; i++ {
		out, err = s.provisionFailover(pl, t.NodeID, lastErr)
		if !isDeadlock(err) {
			break
		}
	}
	if err != nil {
		slog.Error("运营告警: 开通失败善后执行失败, 需人工处理", "task_id", t.ID, "lease_id", pl.LeaseID, "error", err)
		return
	}
	out.notify(s, lastErr)
}

type failoverOutcome struct {
	action     string // "" 放弃(租约/订单已变化) | "reallocated" | "failed"
	lease      *Lease
	order      *compute.Order
	supplierID int64
	oldNode    int64
	newNode    int64
}

func (o failoverOutcome) notify(s *Service, lastErr string) {
	switch o.action {
	case "reallocated":
		slog.Warn("托管开通重试耗尽, 已换节点重新分配", "lease_no", o.lease.LeaseNo, "order_no", o.order.OrderNo,
			"old_node", o.oldNode, "new_node", o.newNode, "error", lastErr)
		s.notify(o.supplierID, "托管租约开通失败",
			fmt.Sprintf("租约 %s 在节点 #%d 开通多次失败(%s), 已改由其他节点交付。请检查该节点状态。", o.lease.LeaseNo, o.oldNode, truncate(lastErr, 200)),
			o.oldNode)
	case "restart_failed":
		slog.Error("运营告警: restart 续租重新开通失败, 续租单已取消并将自动退款", "lease_no", o.lease.LeaseNo,
			"order_no", o.order.OrderNo, "node_id", o.oldNode, "lease_state", o.lease.State, "error", lastErr)
		s.notifyLink(o.order.BuyerID, "续租重新开通失败, 续租款将全额退回",
			fmt.Sprintf("订单 %s 的续租重新开通失败, 续租单已取消, 续租款将原路全额退回。%s", o.order.OrderNo, restartFailNote(o.lease)),
			"/console/buyer/orders/"+o.order.OrderNo)
		s.notify(o.supplierID, "托管租约续租开通失败",
			fmt.Sprintf("租约 %s(订单 %s)续租后在节点 #%d 重新开通失败(%s)。请检查该节点状态。",
				o.lease.LeaseNo, o.order.OrderNo, o.oldNode, truncate(lastErr, 200)), o.oldNode)
	case "failed":
		slog.Error("运营告警: 托管开通彻底失败, 订单已取消并将自动退款", "lease_no", o.lease.LeaseNo, "order_no", o.order.OrderNo,
			"node_id", o.oldNode, "error", lastErr)
		s.notifyLink(o.order.BuyerID, "算力开通失败, 将全额退款",
			fmt.Sprintf("订单 %s 开通失败, 订单已取消, 已支付款项将原路全额退回。给您带来不便深表歉意。", o.order.OrderNo),
			"/console/buyer/orders/"+o.order.OrderNo)
		s.notify(o.supplierID, "托管租约开通失败",
			fmt.Sprintf("租约 %s(订单 %s)在节点 #%d 开通失败(%s), 订单已取消。请检查该节点状态。",
				o.lease.LeaseNo, o.order.OrderNo, o.oldNode, truncate(lastErr, 200)), o.oldNode)
	}
}

func restartFailNote(l *Lease) string {
	if l.State == LeaseGrace && l.GraceUntil != nil {
		return "原环境数据仍保留至 " + l.GraceUntil.Format("2006-01-02 15:04") + ", 期满清除。"
	}
	return ""
}

// provisionFailover 单个事务内完成换节点或失败善后。
func (s *Service) provisionFailover(pl LeaseProvisionPayload, taskNode int64, lastErr string) (failoverOutcome, error) {
	var out failoverOutcome
	err := s.inTx(func(tx *sqlx.Tx) error {
		out = failoverOutcome{}
		var orderID int64
		if err := tx.Get(&orderID, `SELECT order_id FROM leases WHERE id=?`, pl.LeaseID); err != nil {
			return err
		}
		o, err := s.products.LockManagedOrderTx(tx, orderID)
		if err != nil {
			return err
		}
		l, err := s.repo.lockLease(tx, pl.LeaseID)
		if err != nil {
			return err
		}
		if o == nil || l == nil {
			return fmt.Errorf("租约 %d 或其订单不存在", pl.LeaseID)
		}
		if (o.Status != "paid" && o.Status != "provisioning") ||
			(l.State != LeasePending && l.State != LeaseProvisioning) || l.NodeID != taskNode {
			slog.Info("开通死信善后放弃: 订单/租约已变化", "lease_no", l.LeaseNo, "order_status", o.Status,
				"lease_state", l.State, "lease_node", l.NodeID, "task_node", taskNode)
			return nil
		}
		p, err := s.products.ManagedProduct(l.ProductID)
		if err != nil {
			return err
		}
		if p == nil {
			return fmt.Errorf("租约 %s 的商品不存在", l.LeaseNo)
		}
		out = failoverOutcome{lease: l, order: o, supplierID: p.SupplierID, oldNode: l.NodeID}
		if err := s.lockNodeRow(tx, l.NodeID); err != nil {
			return err
		}
		if pl.Resume {
			// 宽限期内原节点恢复(T-088): 数据卷只在原节点, 不换节点、不清理现场; 续租单退款, 租约回到宽限期
			// (grace_until 沿用续租前的值, 期满照常清除)。
			moved, err := s.products.FailManagedRestartTx(tx, o.ID)
			if err != nil {
				return err
			}
			if !moved {
				return fmt.Errorf("订单 %s 找不到待退款的 restart 续租单", o.OrderNo)
			}
			if err := s.backToGraceTx(tx, l, truncate("restart 恢复失败: "+lastErr, 200)); err != nil {
				return err
			}
			out.action = "restart_failed"
			return s.recomputeStockTx(tx, l.ProductID)
		}
		if err := s.releaseOldNodeTx(tx, l, lastErr); err != nil {
			return err
		}
		plan, err := s.repo.leaseNightPlan(tx, l.ID)
		if err != nil {
			return err
		}
		if plan != nil {
			// 包夜(S4): 全部夜晚的预订绑定在原节点的同一组卡上, 不换节点; 开通失败整单退款并释放全部预订。
			if err := s.releaseNightsTx(tx, l.ID, true); err != nil {
				return err
			}
		}
		if !pl.Reallocated && plan == nil {
			newNode, _, err := s.repo.reserveSlotsTx(tx, l.ProductID, l.Cards, l.DeliveryKind == "bare_metal", l.ID, l.NodeID)
			switch {
			case err == nil:
				// provisioning → pending(步骤 ⓪ 已执行) 或 pending 保持(步骤 ⓪ 之前即失败), 守卫式。
				ok, err := affected(tx.Exec(`UPDATE leases SET state='pending', access_changed_at=NOW(), node_id=?,
					upstream_port=NULL, port_claim=NULL, upstream_host_key_fp=NULL, container_name=NULL, volume_name=NULL
					WHERE id=? AND state IN ('pending','provisioning')`, newNode, l.ID))
				if err != nil {
					return err
				}
				if !ok {
					return fmt.Errorf("租约 %s 状态已变化", l.LeaseNo)
				}
				if _, err := s.repo.insertLeaseTask(tx, newNode, l.ID, KindProvision, prioProvision,
					LeaseProvisionPayload{LeaseID: l.ID, Reallocated: true, Renewal: pl.Renewal}); err != nil {
					return err
				}
				out.action, out.newNode = "reallocated", newNode
				return s.recomputeStockTx(tx, l.ProductID)
			case !errors.Is(err, ErrNoWholeNode):
				return err
			}
			// 无其他可用节点: 走失败分支。
		}
		// restart 续租(宽限期后全新开通)失败退续租单, 原订单回到 completed; 否则原订单取消并全额退款。
		fail, action := s.products.FailManagedOrderTx, "failed"
		if pl.Renewal {
			fail, action = s.products.FailManagedRestartTx, "restart_failed"
		}
		moved, err := fail(tx, o.ID)
		if err != nil {
			return err
		}
		if !moved {
			return fmt.Errorf("订单 %s 状态已变化", o.OrderNo)
		}
		reason := truncate("开通失败: "+lastErr, 500)
		if err := s.mustTransition(tx, l, LeaseFailed, ", fail_reason=?", reason); err != nil {
			return err
		}
		out.action = action
		return s.recomputeStockTx(tx, l.ProductID)
	})
	return out, err
}

// releaseOldNodeTx 归还租约在当前节点上的卡槽: 未建容器 → free; 已开始建容器 → draining + CleanupOnly purge。
func (s *Service) releaseOldNodeTx(tx *sqlx.Tx, l *Lease, lastErr string) error {
	if l.ContainerName == nil {
		_, err := tx.Exec(`UPDATE gpu_slots SET state='free', lease_id=NULL, state_changed_at=NOW()
			WHERE lease_id=? AND node_id=? AND state IN ('reserved','leased')`, l.ID, l.NodeID)
		return err
	}
	if _, err := tx.Exec(`UPDATE gpu_slots SET state='draining', state_changed_at=NOW()
		WHERE lease_id=? AND node_id=? AND state IN ('reserved','leased')`, l.ID, l.NodeID); err != nil {
		return err
	}
	_, err := s.repo.insertLeaseTaskMax(tx, l.NodeID, l.ID, KindPurge, prioPurge, cleanupMaxAttempts, LeaseTaskPayload{
		LeaseID: l.ID, Reason: truncate("开通失败清理残留: "+lastErr, 200), CleanupOnly: true,
		ContainerName: *l.ContainerName, VolumeName: deref(l.VolumeName)})
	return err
}

func deref(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func isDeadlock(err error) bool {
	var me *mysql.MySQLError
	return errors.As(err, &me) && (me.Number == 1213 || me.Number == 1205)
}
