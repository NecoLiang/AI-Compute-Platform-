package modeldeploy

import (
	"context"

	"tokenfactory/pkg/errcode"

	"github.com/jmoiron/sqlx"
)

// 停止 / 重启 / 失败收尾 / 租约生命周期联动。
// 所有对节点的动作都只入队 node_tasks(与状态迁移同事务), 由 fleet 任务队列串行执行。

// Stop POST .../stop · 幂等: stopping/stopped/failed 直接返回当前状态。
func (s *Service) Stop(ctx context.Context, buyerID int64, orderNo, deployNo string) (*DeploymentView, error) {
	d, err := s.buyerDeployment(buyerID, orderNo, deployNo)
	if err != nil {
		return nil, err
	}
	if isOneOf(d.State, []string{DepStopping, DepStopped, DepFailed}) {
		return s.toView(d, true, false)
	}
	if s.rt == nil {
		return nil, newErr(errcode.InternalError, "一键部署运行期未接入(fleet 未启用), 无法停止")
	}
	tx, err := s.repo.beginTx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	cur, err := lockDeploymentTx(tx, d.ID)
	if err != nil {
		return nil, err
	}
	if isOneOf(cur.State, stoppableStates) {
		if err := s.beginStopTx(ctx, tx, cur, StopByBuyer); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
	}
	return s.viewByID(d.ID)
}

// beginStopTx 行已加锁且处于 stoppableStates: → stopping + 停止任务。
func (s *Service) beginStopTx(ctx context.Context, tx *sqlx.Tx, d *Deployment, reason string) error {
	ok, err := setState(tx, d.ID, DepStopping, stoppableStates, "stop_reason=?, next_restart_at=NULL", reason)
	if err != nil {
		return err
	}
	if !ok {
		return newErr(errcode.Conflict, "部署状态已变化, 请刷新后重试")
	}
	if err := insertEvent(tx, d.ID, EvStop, reason, ""); err != nil {
		return err
	}
	return s.enqueueStop(ctx, tx, d, DepStopped)
}

// failDeploymentTx 部署失败: → stopping(写 fail_reason/log_tail), 由 stop_model 清理节点后落 failed 并通知买家。
// 返回是否生效(状态不在 from 内时不动)。
func (s *Service) failDeploymentTx(ctx context.Context, tx *sqlx.Tx, d *Deployment, from []string, reason, logTail string) (bool, error) {
	ok, err := setState(tx, d.ID, DepStopping, from, "fail_reason=?, log_tail=COALESCE(?, log_tail), next_restart_at=NULL",
		truncateStr(reason, 512), nullIfEmpty(truncateStr(logTail, 60000)))
	if err != nil || !ok {
		return ok, err
	}
	if err := s.enqueueStop(ctx, tx, d, DepFailed); err != nil {
		return false, err
	}
	return true, nil
}

// Restart POST .../restart
//   - running → restarting(手动重启, 立即入队)
//   - restarting 且在退避等待中 → 立即入队(不再等)
//   - 其它进行中状态 → 幂等返回
//   - stopping → 40900
//   - stopped/failed → 恢复: 租约须 active, 重新占用租约(同租约已有其它未终止部署 → 40900), 走 pending 全流程
func (s *Service) Restart(ctx context.Context, buyerID int64, orderNo, deployNo string) (*DeploymentView, error) {
	d, err := s.buyerDeployment(buyerID, orderNo, deployNo)
	if err != nil {
		return nil, err
	}
	if d.State == DepStopping {
		return nil, newErr(errcode.Conflict, "部署正在停止, 请稍后再试")
	}
	if err := s.runtimeReady(); err != nil {
		return nil, err
	}
	if isOneOf(d.State, []string{DepStopped, DepFailed}) {
		lr, err := s.rt.LeaseRuntime(ctx, d.LeaseID)
		if err != nil {
			return nil, err
		}
		if lr == nil || lr.State != "active" {
			return nil, newErr(errcode.Conflict, "托管租约未处于运行中, 不能恢复部署")
		}
	}
	tx, err := s.repo.beginTx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	cur, err := lockDeploymentTx(tx, d.ID)
	if err != nil {
		return nil, err
	}
	switch {
	case cur.State == DepRunning:
		ok, err := setState(tx, cur.ID, DepRestarting, []string{DepRunning},
			"next_restart_at=NULL, last_restart_at=NOW(), restart_count=restart_count+1, health='unknown', health_reason=NULL")
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, newErr(errcode.Conflict, "部署状态已变化, 请刷新后重试")
		}
		if err := insertEvent(tx, cur.ID, EvManualRestart, "", ""); err != nil {
			return nil, err
		}
	case cur.State == DepRestarting && cur.NextRestartAt != nil:
		res, err := tx.Exec(`UPDATE model_deployments SET next_restart_at=NULL, last_restart_at=NOW(), restart_count=restart_count+1
			WHERE id=? AND state='restarting' AND next_restart_at IS NOT NULL`, cur.ID)
		if err != nil {
			return nil, err
		}
		if n, _ := res.RowsAffected(); n != 1 {
			return nil, newErr(errcode.Conflict, "部署状态已变化, 请刷新后重试")
		}
		if err := insertEvent(tx, cur.ID, EvManualRestart, "", "跳过退避等待"); err != nil {
			return nil, err
		}
	case isOneOf(cur.State, []string{DepStopped, DepFailed}):
		ok, err := setState(tx, cur.ID, DepPending, []string{DepStopped, DepFailed},
			`lease_active_claim=lease_id, fail_reason=NULL, stop_reason=NULL, stopped_at=NULL, health='unknown',
			health_reason=NULL, monitor_state=NULL, next_restart_at=NULL, container_name=NULL`)
		if isDupKey(err, "uk_lease_active") {
			return nil, newErr(errcode.Conflict, "该租约已有其它未终止的部署, 请先停止")
		}
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, newErr(errcode.Conflict, "部署状态已变化, 请刷新后重试")
		}
		if err := insertEvent(tx, cur.ID, EvResume, "", ""); err != nil {
			return nil, err
		}
	case cur.State == DepStopping:
		return nil, newErr(errcode.Conflict, "部署正在停止, 请稍后再试")
	default: // pending/checking/pulling/starting/restarting(进行中): 幂等
		return s.toView(cur, true, false)
	}
	if err := s.enqueueDeploy(ctx, tx, cur.NodeID, cur.LeaseID, cur.ID); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.viewByID(cur.ID)
}

// ResumeNightlyForLeaseTx 包夜开窗(unpark)时由 fleet 在租约 parked→active 的同一事务内调用(docs/27 §27.6, S4 T-095):
// 该租约因包夜窗口结束而停止(stop_reason=nightly)且 auto_resume=1 的部署 → pending 并入队部署。
// 同租约已有未终止部署(uk_lease_active)时跳过该条(只记事件), 不影响开窗。未接入运行期时直接返回。
func (s *Service) ResumeNightlyForLeaseTx(ctx context.Context, tx *sqlx.Tx, leaseID int64) (int, error) {
	if s == nil || s.rt == nil {
		return 0, nil
	}
	list := []Deployment{}
	if err := tx.Select(&list, "SELECT "+deploymentColumns+` FROM model_deployments WHERE lease_id=? AND state=?
		AND stop_reason=? AND auto_resume=1 ORDER BY id FOR UPDATE`, leaseID, DepStopped, StopNightly); err != nil {
		return 0, err
	}
	resumed := 0
	for i := range list {
		d := &list[i]
		ok, err := setState(tx, d.ID, DepPending, []string{DepStopped},
			`lease_active_claim=lease_id, fail_reason=NULL, stop_reason=NULL, stopped_at=NULL, health='unknown',
			health_reason=NULL, monitor_state=NULL, next_restart_at=NULL, container_name=NULL`)
		if isDupKey(err, "uk_lease_active") {
			if err := insertEvent(tx, d.ID, EvResume, "", "包夜开窗: 同租约已有未终止部署, 未自动恢复"); err != nil {
				return resumed, err
			}
			continue
		}
		if err != nil {
			return resumed, err
		}
		if !ok {
			continue
		}
		if err := insertEvent(tx, d.ID, EvResume, StopNightly, "包夜开窗自动恢复"); err != nil {
			return resumed, err
		}
		if err := s.enqueueDeploy(ctx, tx, d.NodeID, d.LeaseID, d.ID); err != nil {
			return resumed, err
		}
		resumed++
	}
	return resumed, nil
}

// StopForLeaseTx 租约到期/冻结/清除时由 fleet 在其自身状态迁移事务内调用: 该租约所有可停止的部署 → stopping 并入队停止。
// 未接入运行期(rt=nil)时无部署可言, 直接返回。
func (s *Service) StopForLeaseTx(ctx context.Context, tx *sqlx.Tx, leaseID int64, reason string) error {
	if s == nil || s.rt == nil {
		return nil
	}
	ph, args := inList(stoppableStates)
	list := []Deployment{}
	if err := tx.Select(&list, "SELECT "+deploymentColumns+" FROM model_deployments WHERE lease_id=? AND state IN ("+ph+") FOR UPDATE",
		append([]any{leaseID}, args...)...); err != nil {
		return err
	}
	for i := range list {
		if err := s.beginStopTx(ctx, tx, &list[i], reason); err != nil {
			return err
		}
	}
	return nil
}
