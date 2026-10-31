package fleet

import (
	"encoding/json"
	"fmt"

	"tokenfactory/pkg/errcode"

	"github.com/jmoiron/sqlx"
)

// ===== 供给方节点操作(docs/25 §25.10) =====

// Verify ⑥ 入队完整探活, 返回 task_id; 进行中重复提交返回同一任务。
func (s *Service) Verify(supplierID, nodeID int64) (int64, error) {
	var taskID int64
	err := s.inTx(func(tx *sqlx.Tx) error {
		n, err := s.ownedNode(tx, supplierID, nodeID)
		if err != nil {
			return err
		}
		switch n.VerifyStatus {
		case VerifySuspended:
			return errcode.New(errcode.Conflict, "节点已被运营停用")
		case VerifyNeedsReimage:
			return errcode.New(errcode.Conflict, "节点待重装确认, 请先调用重装确认")
		case VerifyKeyMismatch:
			return errcode.New(errcode.Conflict, "节点主机指纹已变化, 请先核对并确认新指纹")
		}
		if !n.sshReady() {
			return errcode.New(errcode.ParamInvalid, "请先填写 SSH 连接信息(host/port/user)")
		}
		taskID, err = s.enqueueProbeTx(tx, n, ProbeFull, "supplier", 5)
		return err
	})
	return taskID, err
}

// ProbeReports 探活报告(不含凭证), limit 默认 10、最大 50。
func (s *Service) ProbeReports(supplierID, nodeID int64, limit int) ([]ProbeReport, error) {
	if _, err := s.getOwned(supplierID, nodeID); err != nil {
		return nil, err
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	list, err := s.repo.listReports(nodeID, limit)
	if list == nil {
		list = []ProbeReport{}
	}
	return list, err
}

// Slots 卡槽列表(含所属租约 id, 不含买家身份)。
func (s *Service) Slots(supplierID, nodeID int64) ([]Slot, error) {
	if _, err := s.getOwned(supplierID, nodeID); err != nil {
		return nil, err
	}
	list, err := s.repo.listSlots(s.repo.db, nodeID)
	if list == nil {
		list = []Slot{}
	}
	return list, err
}

// SetSlotState 供给方停用/启用单卡: 仅 free↔withheld; 在租等其他状态 40900。
func (s *Service) SetSlotState(supplierID, nodeID int64, gpuIndex int, state, ip string) error {
	var from string
	switch state {
	case SlotWithheld:
		from = SlotFree
	case SlotFree:
		from = SlotWithheld
	default:
		return errcode.New(errcode.ParamInvalid, `state 仅支持 "withheld" 或 "free"`)
	}
	var node *Node
	err := s.inTx(func(tx *sqlx.Tx) error {
		n, err := s.ownedNode(tx, supplierID, nodeID)
		if err != nil {
			return err
		}
		node = n
		var cur string
		err = tx.Get(&cur, `SELECT state FROM gpu_slots WHERE node_id=? AND gpu_index=? FOR UPDATE`, nodeID, gpuIndex)
		if err != nil {
			return errcode.New(errcode.NotFound, "卡槽不存在(节点通过探活后才会生成卡槽)")
		}
		if cur == state {
			return nil
		}
		if cur != from {
			return errcode.Newf(errcode.Conflict, "卡槽当前为 %s, 仅空闲(free)与停用(withheld)之间可切换", cur)
		}
		ok, err := s.repo.setSlotState(tx, nodeID, gpuIndex, from, state)
		if err != nil {
			return err
		}
		if !ok {
			return errcode.New(errcode.Conflict, "卡槽状态已变化, 请刷新后重试")
		}
		if err := s.repo.audit(tx, supplierID, "node.slot_"+state, "supplier_node", nodeID,
			map[string]any{"gpu_index": gpuIndex, "state": from}, map[string]any{"gpu_index": gpuIndex, "state": state}, ip); err != nil {
			return err
		}
		return s.recomputeStockTx(tx, n.ProductID)
	})
	if err == nil && node != nil {
		s.refreshHealth(node)
	}
	return err
}

// SetSchedulable drain/undrain: 排空后不再接受新分配, 存量租约照常运行至到期。
func (s *Service) SetSchedulable(supplierID, nodeID int64, v bool, ip string) error {
	var node *Node
	err := s.inTx(func(tx *sqlx.Tx) error {
		n, err := s.ownedNode(tx, supplierID, nodeID)
		if err != nil {
			return err
		}
		node = n
		if n.Schedulable == v {
			return nil
		}
		ok, err := affected(tx.Exec(`UPDATE supplier_nodes SET schedulable=? WHERE id=? AND mgmt_mode='ssh' AND schedulable=?`, v, nodeID, !v))
		if err != nil {
			return err
		}
		if !ok {
			return errcode.New(errcode.Conflict, "节点状态已变化, 请刷新后重试")
		}
		action := "node.drained"
		if v {
			action = "node.undrained"
		}
		if err := s.repo.audit(tx, supplierID, action, "supplier_node", nodeID,
			map[string]any{"schedulable": !v}, map[string]any{"schedulable": v}, ip); err != nil {
			return err
		}
		return s.recomputeStockTx(tx, n.ProductID)
	})
	if err == nil && node != nil {
		s.refreshHealth(node)
	}
	return err
}

// ===== 运营(docs/25 §25.10) =====

// Suspend 运营停用节点: 停止新分配、释放空闲卡 UUID 与指纹占用、代次 +1, 作废排队中与进行中的探活; 不影响存量租约。
// 只作废 probe 任务: 存量租约的 stop/purge/kill_sessions 必须照常执行; provision 在执行时自行检查核验状态,
// 失败走重试→死信换节点/退款的善后, 直接置 failed 会跳过善后让租约卡在开通中。
// 进行中的探活被置 failed 后, worker 的回写被 finishTask 的 attempts 守卫忽略, 结论被代次守卫作废。
func (s *Service) Suspend(operatorID, nodeID int64, reason, ip string) error {
	var node *Node
	err := s.inTx(func(tx *sqlx.Tx) error {
		n, err := s.repo.lockNode(tx, nodeID)
		if err != nil {
			return err
		}
		if n == nil {
			return errcode.New(errcode.NotFound, "托管节点不存在")
		}
		node = n
		ok, err := s.repo.suspend(tx, nodeID)
		if err != nil {
			return err
		}
		if !ok {
			return errcode.New(errcode.Conflict, "节点已处于停用状态")
		}
		if err := s.repo.deleteIdleSlots(tx, nodeID); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE node_tasks SET state='failed', last_error='节点已被运营停用', locked_until=NULL
			WHERE node_id=? AND kind='probe' AND state IN ('pending','running')`, nodeID); err != nil {
			return err
		}
		if err := s.repo.audit(tx, operatorID, "node.suspended", "supplier_node", nodeID,
			map[string]any{"verify_status": n.VerifyStatus}, map[string]any{"verify_status": VerifySuspended, "reason": reason}, ip); err != nil {
			return err
		}
		return s.recomputeStockTx(tx, n.ProductID)
	})
	if err != nil {
		return err
	}
	s.refreshHealth(node)
	msg := fmt.Sprintf("节点「%s」已被平台运营停用, 停止接受新租约(存量租约不受影响)。", node.NodeName)
	if reason != "" {
		msg += "原因: " + reason
	}
	s.notify(node.SupplierID, "节点已被停用", msg, nodeID)
	return nil
}

// Resume 运营恢复: suspended → unverified 并自动入队完整探活, 通过后才回到 verified(停用期间卡槽已释放, 须重新核验)。
func (s *Service) Resume(operatorID, nodeID int64, ip string) (int64, error) {
	var node *Node
	var taskID int64
	var reimage bool
	err := s.inTx(func(tx *sqlx.Tx) error {
		n, err := s.repo.lockNode(tx, nodeID)
		if err != nil {
			return err
		}
		if n == nil {
			return errcode.New(errcode.NotFound, "托管节点不存在")
		}
		node = n
		ok, err := s.repo.resume(tx, nodeID)
		if err != nil {
			return err
		}
		if !ok {
			return errcode.New(errcode.Conflict, "节点不在停用状态")
		}
		to := VerifyUnverified
		if n.ReimageRequiredAt != nil {
			// 停用期间整机租约结束(T-089): 恢复到 needs_reimage, 不入队探活, 等供给方重装确认。
			to = VerifyNeedsReimage
		}
		if err := s.repo.audit(tx, operatorID, "node.resumed", "supplier_node", nodeID,
			map[string]any{"verify_status": VerifySuspended}, map[string]any{"verify_status": to}, ip); err != nil {
			return err
		}
		if !n.sshReady() || to == VerifyNeedsReimage {
			reimage = to == VerifyNeedsReimage
			return nil
		}
		n.VerifyStatus = VerifyUnverified
		taskID, err = s.enqueueProbeTx(tx, n, ProbeFull, "resume", 3)
		return err
	})
	if err != nil {
		return 0, err
	}
	if reimage {
		s.notify(node.SupplierID, "节点已恢复, 待重装", fmt.Sprintf("节点「%s」已被平台运营恢复; 该节点的整机租约已结束, 请重装操作系统后在控制台确认重装, 通过完整探活后恢复接单。", node.NodeName), nodeID)
		return 0, nil
	}
	s.notify(node.SupplierID, "节点已恢复", fmt.Sprintf("节点「%s」已被平台运营恢复, 正在重新探活, 通过后恢复接单。", node.NodeName), nodeID)
	return taskID, nil
}

// Tasks 运营任务列表(默认死信)。
func (s *Service) Tasks(state string, nodeID int64, page, pageSize int) ([]Task, int64, error) {
	switch state {
	case "": // 缺省 = 全部状态(与 docs/api 一致)
	case TaskPending, TaskRunning, TaskSucceeded, TaskFailed, TaskDead:
	default:
		return nil, 0, errcode.New(errcode.ParamInvalid, "state 仅支持 pending/running/succeeded/failed/dead")
	}
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	list, total, err := s.repo.listTasks(state, nodeID, page, pageSize)
	if list == nil {
		list = []Task{}
	}
	return list, total, err
}

// RetryTask 运营重试死信任务: dead → pending(次数清零), 写审计。
func (s *Service) RetryTask(operatorID, taskID int64, ip string) error {
	t, err := s.repo.getTask(s.repo.db, taskID)
	if err != nil {
		return err
	}
	if t == nil {
		return errcode.New(errcode.NotFound, "任务不存在")
	}
	if t.State != TaskDead {
		return errcode.Newf(errcode.Conflict, "仅死信(dead)任务可重试, 当前为 %s", t.State)
	}
	if t.Kind == KindProvision {
		// 开通任务进入死信时已自动换节点或取消订单并退款(docs/25 §25.5.1), 重放会在已释放的卡槽上重复开通。
		return errcode.New(errcode.Conflict, "开通任务死信已自动善后(换节点或取消订单退款), 不可手动重试")
	}
	return s.inTx(func(tx *sqlx.Tx) error {
		if t.Kind == KindProbe {
			if err := s.prepareProbeRetry(tx, t); err != nil {
				return err
			}
		}
		ok, err := affected(tx.Exec(`UPDATE node_tasks SET state='pending', attempts=0, run_after=NOW(), locked_until=NULL
			WHERE id=? AND state='dead'`, taskID))
		if err != nil {
			return err
		}
		if !ok {
			return errcode.New(errcode.Conflict, "任务状态已变化, 请刷新后重试")
		}
		return s.repo.audit(tx, operatorID, "node_task.retry", "node_task", taskID,
			map[string]any{"state": TaskDead, "attempts": t.Attempts, "last_error": t.LastError},
			map[string]any{"state": TaskPending}, ip)
	})
}

// prepareProbeRetry 重试死信探活: 按节点当前代次重写 payload(否则会因代次过时被直接作废),
// 完整探活同时把节点置 verifying; 节点已停用/待处理或已有未结束的探活时 40900。调用方在同一事务内把任务转回 pending。
func (s *Service) prepareProbeRetry(tx *sqlx.Tx, t *Task) error {
	n, err := s.repo.lockNode(tx, t.NodeID)
	if err != nil {
		return err
	}
	if n == nil {
		return errcode.New(errcode.Conflict, "节点已删除, 不能重试")
	}
	switch n.VerifyStatus {
	case VerifySuspended, VerifyNeedsReimage, VerifyKeyMismatch:
		return errcode.Newf(errcode.Conflict, "节点当前为 %s, 不能重试探活", n.VerifyStatus)
	}
	if a, err := s.repo.activeProbe(tx, t.NodeID); err != nil {
		return err
	} else if a != nil {
		return errcode.Newf(errcode.Conflict, "节点已有未结束的探活任务 #%d", a.ID)
	}
	var pl ProbePayload
	if err := jsonUnmarshal(t.Payload, &pl); err != nil {
		return errcode.New(errcode.Conflict, "任务 payload 无法解析, 不能重试")
	}
	pl.Generation = n.Generation
	raw, err := json.Marshal(pl)
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE node_tasks SET payload=? WHERE id=? AND state='dead'`, string(raw), t.ID); err != nil {
		return err
	}
	if pl.Mode == ProbePortRecheck {
		return nil
	}
	_, err = s.repo.setVerifyStatus(tx, n.ID, []string{VerifyUnverified, VerifyFailed}, VerifyVerifying)
	return err
}
