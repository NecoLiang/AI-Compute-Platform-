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
)

// 包夜开停机(docs/26 §26.6.4-26.6.5, docs/25 §25.7.6, docs/27 §27.6; S4 T-095)。
//
// 租约在整个订单期内只有一个容器(首晚开通时建出), 白天停放(parked), 每晚复用:
//
//	首晚    支付后到达 窗口开始−10min(当晚订单为支付时刻) → 认领本晚预订卡槽 free→reserved → 入队 provision(开通流水线)
//	每晚开窗 parked 且本晚已到 窗口开始−10min → 认领卡槽 → 宿主机 GPU 进程检查 → 防火墙 + docker start → 自检
//	        → 同事务 parked→active、卡槽 reserved→leased、恢复 auto_resume 的一键部署(stop_reason=nightly)
//	每晚关窗 窗口结束 → active→parked、停止一键部署(stop_reason=nightly, auto_resume 保留)、卡槽 leased→draining、
//	        入队 stop(docker stop 后 draining→free, 卡归还供给方白天自用)
//	最后一晚 窗口结束 = access_until, 由到期任务走 active→grace(宽限/清除照常), 本任务不 park
//
// 开窗失败: 宿主机进程占用本晚卡且到 应开通时刻+10min 仍未释放 → 本晚判定失败(occupied, 违约记录: 审计
// lease.night_unpark_failed + 通知供给方与买家 + ERROR 日志), 整晚记 night_unpark_failed 停机; 其他失败(连不上、
// docker start/自检失败)本晚内每分钟重试, 窗口结束仍未开通则整晚按失败原因记停机。开窗成功但晚于应开通时刻
// ≥ min_minutes 的, 延迟段同样记停机。首晚卡槽到 应开通时刻+10min 仍无法认领 → 订单开通失败全额退款。

const (
	OutageNightUnparkFailed = "night_unpark_failed"
	OutageProvisionDelay    = "provision_delay"

	nightlyInterval = time.Minute
	// nightClaimLead 窗口开始前多久认领卡槽并开窗(容器启动+自检在窗口开始前完成)。
	nightClaimLead = 10 * time.Minute
	// nightOccupiedGrace 应开通时刻后仍被宿主机进程占用多久判定本晚失败; 首晚认领卡槽同样以此为限。
	nightOccupiedGrace = 10 * time.Minute
)

// nightRemindMins 窗口结束前提醒(站内信 + 容器内 wall)。
var nightRemindMins = []int{30, 10}

// cmdGPUApps 宿主机上占用 GPU 的计算进程(开窗前检查本晚卡是否空闲)。
const cmdGPUApps = "nvidia-smi --query-compute-apps=gpu_uuid,pid --format=csv,noheader"

var shanghaiLocation = func() *time.Location {
	if loc, err := time.LoadLocation("Asia/Shanghai"); err == nil {
		return loc
	}
	return time.FixedZone("CST", 8*3600)
}()

func shanghai() *time.Location { return shanghaiLocation }

// errNightOccupied 宿主机进程占用本晚卡。
var errNightOccupied = errors.New("本晚卡被宿主机进程占用")

// LeaseNight lease_nights 一行。
type LeaseNight struct {
	ID         int64      `db:"id" json:"-"`
	LeaseID    int64      `db:"lease_id" json:"-"`
	NightDate  time.Time  `db:"night_date" json:"night_date"`
	State      string     `db:"state" json:"state"`
	Attempts   int        `db:"attempts" json:"attempts"`
	LastError  *string    `db:"last_error" json:"last_error,omitempty"`
	Occupied   bool       `db:"occupied" json:"occupied"`
	FailCause  *string    `db:"fail_cause" json:"fail_cause,omitempty"`
	DueAt      *time.Time `db:"due_at" json:"-"`
	UnparkedAt *time.Time `db:"unparked_at" json:"unparked_at,omitempty"`
	OutageID   *int64     `db:"outage_id" json:"-"`
}

const leaseNightColumns = `id, lease_id, night_date, state, attempts, last_error, occupied, fail_cause, due_at, unparked_at, outage_id`

// due 本晚应开通时刻: max(窗口开始, due_at)。
func (n *LeaseNight) due(w compute.NightWindow) time.Time {
	ws := compute.NightStartAt(n.NightDate, w)
	if n.DueAt != nil && n.DueAt.After(ws) {
		return *n.DueAt
	}
	return ws
}

func (r *Repository) lockNight(tx *sqlx.Tx, id int64) (*LeaseNight, error) {
	var n LeaseNight
	err := tx.Get(&n, `SELECT `+leaseNightColumns+` FROM lease_nights WHERE id=? FOR UPDATE`, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &n, err
}

// firstBookedNight 租约最早一晚 booked 记录(加锁)。
func (r *Repository) firstBookedNight(tx *sqlx.Tx, leaseID int64) (*LeaseNight, error) {
	var n LeaseNight
	err := tx.Get(&n, `SELECT `+leaseNightColumns+` FROM lease_nights WHERE lease_id=? AND state='booked'
		ORDER BY night_date LIMIT 1 FOR UPDATE`, leaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return &n, err
}

// claimNightSlotsTx 认领本晚预订的卡槽: 全部 free(或已归本租约 reserved/leased)才认领, free→reserved。
// 任一张卡仍 draining(上一租约/上一晚未停完)、faulty、withheld 或被别的租约占用 → (false, 原因)。调用方已锁订单与租约。
func (s *Service) claimNightSlotsTx(tx *sqlx.Tx, l *Lease, night time.Time) (bool, string, error) {
	if err := s.lockNodeRow(tx, l.NodeID); err != nil {
		return false, "", err
	}
	var slots []Slot
	if err := tx.Select(&slots, `SELECT s.id, s.node_id, s.gpu_index, s.gpu_uuid, s.pci_bus_id, s.vram_mb, s.topo_group, s.state,
		s.lease_id, s.state_changed_at FROM gpu_slots s JOIN night_bookings b ON b.slot_id=s.id
		WHERE b.lease_id=? AND b.night_date=? ORDER BY s.gpu_index FOR UPDATE`, l.ID, night.Format(dateLayout)); err != nil {
		return false, "", err
	}
	if len(slots) != l.Cards {
		return false, fmt.Sprintf("本晚预订卡数 %d 与租约卡数 %d 不符", len(slots), l.Cards), nil
	}
	var claim []int64
	for _, sl := range slots {
		owned := sl.LeaseID != nil && *sl.LeaseID == l.ID
		switch {
		case sl.NodeID != l.NodeID:
			return false, fmt.Sprintf("预订卡 #%d 不在租约节点上", sl.ID), nil
		case owned && (sl.State == SlotReserved || sl.State == SlotLeased):
		case sl.State == SlotFree:
			claim = append(claim, sl.ID)
		default:
			return false, fmt.Sprintf("GPU%d 当前为 %s, 暂不可开通", sl.GPUIndex, sl.State), nil
		}
	}
	if len(claim) == 0 {
		return true, "", nil
	}
	q, args, err := sqlx.In(`UPDATE gpu_slots SET state='reserved', lease_id=?, state_changed_at=NOW() WHERE id IN (?) AND state='free'`,
		l.ID, claim)
	if err != nil {
		return false, "", err
	}
	n, err := affectedN(tx.Exec(q, args...))
	if err != nil {
		return false, "", err
	}
	if int(n) != len(claim) {
		return false, "", fmt.Errorf("租约 %s 认领本晚卡槽只成功 %d/%d", l.LeaseNo, n, len(claim))
	}
	return true, "", nil
}

func affectedN(res sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// nightPaidTx 包夜订单收款(租约已 pending): 记首晚应开通时刻 = max(窗口开始, 支付时刻); 已到认领时点则认领卡槽并入队开通,
// 否则留给包夜任务。认领不成功不让收款事务失败(包夜任务继续重试, 到期限仍不成功则退款)。
func (s *Service) nightPaidTx(tx *sqlx.Tx, l *Lease, plan *nightPlan) error {
	n, err := s.repo.firstBookedNight(tx, l.ID)
	if err != nil {
		return err
	}
	if n == nil {
		return fmt.Errorf("包夜租约 %s 没有待开通的夜晚", l.LeaseNo)
	}
	now := time.Now()
	if _, err := tx.Exec(`UPDATE lease_nights SET due_at=? WHERE id=?`, now, n.ID); err != nil {
		return err
	}
	if now.Before(compute.NightStartAt(n.NightDate, plan.Window).Add(-nightClaimLead)) {
		return nil
	}
	ok, why, err := s.claimNightSlotsTx(tx, l, n.NightDate)
	if err != nil {
		return err
	}
	if !ok {
		slog.Info("包夜首晚卡槽暂不可认领, 交由包夜任务重试", "lease_no", l.LeaseNo, "reason", why)
		return nil
	}
	_, err = s.repo.insertLeaseTask(tx, l.NodeID, l.ID, KindProvision, prioProvision, LeaseProvisionPayload{LeaseID: l.ID})
	return err
}

// NightlyRound 一轮包夜开停机(导出供测试同步调用)。顺序: 关窗 → 首晚认领 → 开窗 → 过期夜晚收尾 → 关窗提醒。
func (s *Service) NightlyRound(ctx context.Context) {
	s.nightParkRound(ctx)
	s.nightClaimRound(ctx)
	s.nightUnparkRound(ctx)
	s.nightSweepRound(ctx)
	s.nightRemindRound(ctx)
}

// nightRow 扫描用: 夜晚 + 租约 + 订单窗口快照。
type nightRow struct {
	NightID  int64     `db:"night_id"`
	LeaseID  int64     `db:"lease_id"`
	Night    time.Time `db:"night_date"`
	StartMin int       `db:"night_start_min"`
	EndMin   int       `db:"night_end_min"`
}

func (r nightRow) window() compute.NightWindow {
	return compute.NightWindow{StartMin: r.StartMin, EndMin: r.EndMin}
}

// selectNights 按条件取包夜夜晚(订单须有窗口快照)。
func (s *Service) selectNights(where string, args ...any) []nightRow {
	var rows []nightRow
	if err := s.repo.db.Select(&rows, `SELECT n.id AS night_id, n.lease_id, n.night_date, o.night_start_min, o.night_end_min
		FROM lease_nights n JOIN leases l ON l.id=n.lease_id JOIN orders o ON o.id=l.order_id
		WHERE o.night_start_min IS NOT NULL AND o.night_end_min IS NOT NULL AND `+where+` ORDER BY n.night_date, n.id LIMIT ?`,
		append(args, sweepBatch)...); err != nil {
		slog.Error("包夜任务: 查询夜晚失败", "error", err)
	}
	return rows
}

// lockNightCtx 包夜写事务的标准加锁: 订单 → 租约 → 节点行 → 夜晚行。任一不存在返回 nil。
func (s *Service) lockNightCtx(tx *sqlx.Tx, leaseID, nightID int64) (*compute.Order, *Lease, *LeaseNight, error) {
	var orderID int64
	if err := tx.Get(&orderID, `SELECT order_id FROM leases WHERE id=?`, leaseID); err != nil {
		return nil, nil, nil, err
	}
	o, err := s.products.LockManagedOrderTx(tx, orderID)
	if err != nil || o == nil {
		return nil, nil, nil, err
	}
	l, err := s.repo.lockLease(tx, leaseID)
	if err != nil || l == nil {
		return nil, nil, nil, err
	}
	if err := s.lockNodeRow(tx, l.NodeID); err != nil {
		return nil, nil, nil, err
	}
	n, err := s.repo.lockNight(tx, nightID)
	if err != nil || n == nil {
		return nil, nil, nil, err
	}
	return o, l, n, nil
}

// ===== 关窗 =====

func (s *Service) nightParkRound(ctx context.Context) {
	for _, r := range s.selectNights(`n.state='active'`) {
		if ctx.Err() != nil {
			return
		}
		we := compute.NightEndAt(r.Night, r.window())
		if time.Now().Before(we) {
			continue
		}
		if err := s.parkNight(ctx, r, we); err != nil {
			slog.Error("包夜关窗失败", "lease_id", r.LeaseID, "night", r.Night.Format(dateLayout), "error", err)
		}
	}
}

// parkNight 窗口结束: 在租且非最后一晚 → parked + 停部署 + 卡槽 draining + stop; 冻结中 → 卡槽 draining + stop(保持 suspended);
// 最后一晚(窗口结束 = access_until)交到期任务。本晚一律 ended。
func (s *Service) parkNight(ctx context.Context, r nightRow, we time.Time) error {
	parked := false
	var leaseNo string
	err := s.inTx(func(tx *sqlx.Tx) error {
		_, l, n, err := s.lockNightCtx(tx, r.LeaseID, r.NightID)
		if err != nil || l == nil || n.State != "active" {
			return err
		}
		leaseNo = l.LeaseNo
		pl := LeaseTaskPayload{LeaseID: l.ID, Reason: "包夜窗口结束", Nightly: true}
		switch {
		case l.State == LeaseActive && l.AccessUntil != nil && we.Before(*l.AccessUntil):
			if err := s.mustTransition(tx, l, LeaseParked, ""); err != nil {
				return err
			}
			if err := s.stopDeploymentsTx(tx, l.ID, "nightly"); err != nil {
				return err
			}
			if _, err := s.repo.moveLeaseSlots(tx, l.ID, []string{SlotLeased, SlotReserved}, SlotDraining); err != nil {
				return err
			}
			if err := s.ensureStopPayloadTx(tx, l, pl); err != nil {
				return err
			}
			parked = true
		case l.State == LeaseSuspended:
			moved, err := s.repo.moveLeaseSlots(tx, l.ID, []string{SlotLeased, SlotReserved}, SlotDraining)
			if err != nil {
				return err
			}
			if moved > 0 {
				if err := s.ensureStopPayloadTx(tx, l, pl); err != nil {
					return err
				}
			}
		}
		if _, err := tx.Exec(`UPDATE lease_nights SET state='ended', ended_at=NOW() WHERE id=? AND state='active'`, n.ID); err != nil {
			return err
		}
		return s.recomputeStockTx(tx, l.ProductID)
	})
	if err != nil || !parked {
		return err
	}
	slog.Info("包夜窗口结束: 租约已停放, 入队停容器", "lease_no", leaseNo, "night", r.Night.Format(dateLayout))
	// 本晚未结束的停机(节点离线/容器异常等)在窗口结束时结束, 白天不计。
	return s.closeOutage(ctx, r.LeaseID, we, "包夜窗口结束")
}

// ===== 首晚认领 =====

func (s *Service) nightClaimRound(ctx context.Context) {
	var ids []int64
	if err := s.repo.db.Select(&ids, `SELECT l.id FROM leases l JOIN orders o ON o.id=l.order_id
		WHERE l.state='pending' AND o.pricing_mode=? AND o.status='paid'
		AND NOT EXISTS (SELECT 1 FROM node_tasks t WHERE t.lease_id=l.id AND t.kind=? AND t.state IN ('pending','running'))
		ORDER BY l.id LIMIT ?`, compute.PricingNightly, KindProvision, sweepBatch); err != nil {
		slog.Error("包夜任务: 查询待开通租约失败", "error", err)
		return
	}
	for _, id := range ids {
		if ctx.Err() != nil {
			return
		}
		if err := s.claimFirstNight(id); err != nil {
			slog.Error("包夜首晚认领失败", "lease_id", id, "error", err)
		}
	}
}

// claimFirstNight 首晚到认领时点: 认领成功入队 provision; 到 应开通时刻+10min 仍不可认领 → 订单开通失败全额退款(释放全部预订)。
func (s *Service) claimFirstNight(leaseID int64) error {
	var failed *Lease
	var ord *compute.Order
	var why string
	err := s.inTx(func(tx *sqlx.Tx) error {
		failed = nil
		var orderID int64
		if err := tx.Get(&orderID, `SELECT order_id FROM leases WHERE id=?`, leaseID); err != nil {
			return err
		}
		o, err := s.products.LockManagedOrderTx(tx, orderID)
		if err != nil || o == nil || o.Status != "paid" {
			return err
		}
		l, err := s.repo.lockLease(tx, leaseID)
		if err != nil || l == nil || l.State != LeasePending {
			return err
		}
		live, err := s.repo.liveLeaseTasks(tx, l.ID, KindProvision)
		if err != nil || len(live) > 0 {
			return err
		}
		plan, err := s.repo.leaseNightPlan(tx, l.ID)
		if err != nil || plan == nil {
			return err
		}
		if err := s.lockNodeRow(tx, l.NodeID); err != nil {
			return err
		}
		n, err := s.repo.firstBookedNight(tx, l.ID)
		if err != nil {
			return err
		}
		now := time.Now()
		if n != nil {
			if now.Before(compute.NightStartAt(n.NightDate, plan.Window).Add(-nightClaimLead)) {
				return nil
			}
			ok, reason, err := s.claimNightSlotsTx(tx, l, n.NightDate)
			if err != nil {
				return err
			}
			if ok {
				_, err = s.repo.insertLeaseTask(tx, l.NodeID, l.ID, KindProvision, prioProvision, LeaseProvisionPayload{LeaseID: l.ID})
				return err
			}
			why = reason
			if now.Before(n.due(plan.Window).Add(nightOccupiedGrace)) {
				_, err := tx.Exec(`UPDATE lease_nights SET attempts=attempts+1, last_error=? WHERE id=?`, truncate(reason, 500), n.ID)
				return err
			}
		} else {
			why = "没有可开通的夜晚"
		}
		// 首晚无法开通: 订单取消全额退款, 预订全部释放, 租约 failed(租约从未交付, 不记停机)。
		moved, err := s.products.FailManagedOrderTx(tx, o.ID)
		if err != nil {
			return err
		}
		if !moved {
			return fmt.Errorf("订单 %s 状态已变化", o.OrderNo)
		}
		if err := s.releaseNightsTx(tx, l.ID, true); err != nil {
			return err
		}
		if _, err := s.repo.moveLeaseSlots(tx, l.ID, []string{SlotReserved}, SlotFree); err != nil {
			return err
		}
		if err := s.mustTransition(tx, l, LeaseFailed, ", fail_reason=?", truncate("包夜首晚无法开通: "+why, 500)); err != nil {
			return err
		}
		failed, ord = l, o
		return s.recomputeStockTx(tx, l.ProductID)
	})
	if err != nil || failed == nil {
		return err
	}
	slog.Error("运营告警: 包夜首晚卡槽无法认领, 订单已开通失败退款", "lease_no", failed.LeaseNo, "order_no", ord.OrderNo, "reason", why)
	s.notifyLink(ord.BuyerID, "包夜订单开通失败",
		fmt.Sprintf("订单 %s 首晚资源未能按时就绪, 订单已取消, 款项将原路退回。", ord.OrderNo), "/console/buyer/orders/"+ord.OrderNo)
	return nil
}

// ===== 开窗 =====

func (s *Service) nightUnparkRound(ctx context.Context) {
	today := compute.NightDate(time.Now().In(shanghai()))
	rows := s.selectNights(`n.state='booked' AND l.state='parked' AND n.night_date BETWEEN ? AND ?`,
		today.AddDate(0, 0, -1).Format(dateLayout), today.Format(dateLayout))
	for _, r := range rows {
		if ctx.Err() != nil {
			return
		}
		w, now := r.window(), time.Now()
		if now.Before(compute.NightStartAt(r.Night, w).Add(-nightClaimLead)) || !now.Before(compute.NightEndAt(r.Night, w)) {
			continue
		}
		if err := s.unparkNight(ctx, r); err != nil {
			slog.Error("包夜开窗处理失败", "lease_id", r.LeaseID, "night", r.Night.Format(dateLayout), "error", err)
		}
	}
}

// unparkNight 本晚开窗: 认领卡槽 → 宿主机 GPU 进程检查 → docker start + 自检 → 激活。失败记一次尝试(可能判定本晚失败)。
func (s *Service) unparkNight(ctx context.Context, r nightRow) error {
	var l *Lease
	var why, cause string
	err := s.inTx(func(tx *sqlx.Tx) error {
		o, cur, n, err := s.lockNightCtx(tx, r.LeaseID, r.NightID)
		if err != nil || cur == nil || n.State != "booked" || cur.State != LeaseParked || o.Status != "active" {
			return err
		}
		live, err := s.repo.liveLeaseTasks(tx, cur.ID, KindStop)
		if err != nil {
			return err
		}
		if len(live) > 0 {
			why, cause = "上一晚停容器任务尚未完成", OutageNightUnparkFailed
			return nil
		}
		ok, reason, err := s.claimNightSlotsTx(tx, cur, r.Night)
		if err != nil {
			return err
		}
		if !ok {
			why, cause = reason, OutageNightUnparkFailed
			return nil
		}
		l = cur
		return nil
	})
	if err != nil {
		return err
	}
	if why != "" {
		return s.nightAttemptFailed(ctx, r, cause, false, why)
	}
	if l == nil {
		return nil
	}
	if err := s.startNightContainer(ctx, l); err != nil {
		occupied := errors.Is(err, errNightOccupied)
		cause := OutageNightUnparkFailed
		switch {
		case occupied:
		case errors.Is(err, errGatewayDown):
			cause = OutageGatewayDown
		case errors.Is(err, errNodeUnreachable):
			cause = OutageNodeOffline
		case errors.Is(err, errLeaseMoved):
			return s.releaseClaimedIfIdle(l.ID)
		default:
			cause = OutageContainerDown
		}
		return s.nightAttemptFailed(ctx, r, cause, occupied, err.Error())
	}
	return s.activateNight(ctx, r, l)
}

// errNodeUnreachable 开窗时连不上节点。
var errNodeUnreachable = errors.New("节点连接失败")

// startNightContainer 宿主机 GPU 进程检查 → 防火墙 → docker start → 自检(reserved 卡 + 租约仍 parked)。自检失败把容器停回去。
// 真实 GPU/宿主机行为未验收(集成测试经桩节点执行)。
func (s *Service) startNightContainer(ctx context.Context, l *Lease) error {
	n, err := s.repo.GetNode(l.NodeID)
	if err != nil {
		return err
	}
	if n == nil {
		return fmt.Errorf("节点 #%d 已不存在", l.NodeID)
	}
	client, err := s.dialLeaseNode(ctx, l.NodeID)
	if err != nil {
		return fmt.Errorf("%w: %v", errNodeUnreachable, err)
	}
	defer client.Close()
	slots, err := s.repo.leaseSlotsOnNode(s.repo.db, l.ID, l.NodeID)
	if err != nil {
		return err
	}
	mine := map[string]bool{}
	for _, sl := range slots {
		if sl.State == SlotReserved {
			mine[strings.ToUpper(sl.GPUUUID)] = true
		}
	}
	out, err := runRemote(ctx, client, cmdGPUApps, cmdTimeout)
	if err != nil {
		return fmt.Errorf("宿主机 GPU 进程检查失败: %v", errText(err))
	}
	if busy := occupiedGPUs(out, mine); len(busy) > 0 {
		return fmt.Errorf("%w: %s", errNightOccupied, strings.Join(busy, "; "))
	}
	if _, err := s.applyFirewall(ctx, client, n); err != nil {
		return fmt.Errorf("下发租约端口防火墙规则失败(未启动容器): %w", err)
	}
	start, err := startContainerCommand(l.LeaseNo)
	if err != nil {
		return err
	}
	if out, err := runRemote(ctx, client, start, cmdTimeout); err != nil || !strings.Contains(out, "@@started") {
		return fmt.Errorf("docker start 失败: %v", errText(err))
	}
	if err := s.verifyContainer(ctx, client, l, SlotReserved, LeaseParked); err != nil {
		s.stopBack(ctx, client, l.LeaseNo)
		return err
	}
	return nil
}

// occupiedGPUs 解析 nvidia-smi --query-compute-apps=gpu_uuid,pid 输出, 返回落在本租约卡上的 "UUID pid=…"。
func occupiedGPUs(out string, mine map[string]bool) []string {
	var busy []string
	for _, line := range nonEmptyLines(out) {
		uuid, pid, _ := strings.Cut(line, ",")
		uuid = strings.ToUpper(strings.TrimSpace(uuid))
		if mine[uuid] {
			busy = append(busy, uuid+" pid="+strings.TrimSpace(pid))
		}
	}
	return busy
}

// activateNight 开窗成功: parked→active、卡槽 reserved→leased、本晚 active、恢复 auto_resume 部署; 延迟开窗记停机。
func (s *Service) activateNight(ctx context.Context, r nightRow, l *Lease) error {
	var late *nightLate
	resumed := 0
	err := s.inTx(func(tx *sqlx.Tx) error {
		late = nil
		o, cur, n, err := s.lockNightCtx(tx, r.LeaseID, r.NightID)
		if err != nil {
			return err
		}
		if cur == nil || n == nil || n.State != "booked" || cur.State != LeaseParked || o.Status != "active" {
			return errLeaseMoved
		}
		if err := s.mustTransition(tx, cur, LeaseActive, ""); err != nil {
			return err
		}
		moved, err := s.repo.moveLeaseSlots(tx, cur.ID, []string{SlotReserved}, SlotLeased)
		if err != nil {
			return err
		}
		if int(moved) != cur.Cards {
			return fmt.Errorf("租约 %s 开窗卡槽 reserved→leased 只迁移了 %d/%d", cur.LeaseNo, moved, cur.Cards)
		}
		if _, err := tx.Exec(`UPDATE lease_nights SET state='active', unparked_at=NOW(), attempts=attempts+1 WHERE id=?`, n.ID); err != nil {
			return err
		}
		if _, err := s.repo.setResourceAlert(tx, cur.ID, ""); err != nil {
			return err
		}
		if resumed, err = s.resumeNightlyDeploymentsTx(tx, cur.ID); err != nil {
			return err
		}
		now := time.Now()
		if due := n.due(r.window()); now.Sub(due) >= time.Duration(s.cfg.Outage.MinMinutes)*time.Minute {
			cause := OutageNightUnparkFailed
			if n.FailCause != nil && *n.FailCause != "" {
				cause = *n.FailCause
			}
			late = &nightLate{start: due, end: now, cause: cause, detail: "包夜开窗延迟: " + deref(n.LastError)}
			if late.id, err = s.insertNightOutageTx(tx, o, cur, n, late.cause, late.start, late.end, truncate(late.detail, 500)); err != nil {
				return err
			}
		}
		return s.recomputeStockTx(tx, cur.ProductID)
	})
	if errors.Is(err, errLeaseMoved) {
		// 自检后租约已变化(冻结/退款等): 容器停回去, 认领的卡还回去。
		if client, derr := s.dialLeaseNode(ctx, l.NodeID); derr == nil {
			s.stopBack(ctx, client, l.LeaseNo)
			client.Close()
		}
		return s.releaseClaimedIfIdle(l.ID)
	}
	if err != nil {
		return err
	}
	slog.Info("包夜开窗完成", "lease_no", l.LeaseNo, "night", r.Night.Format(dateLayout), "deployments_resumed", resumed)
	if late != nil && late.id > 0 {
		s.afterSettle(ctx, late.id)
	}
	return nil
}

type nightLate struct {
	start, end    time.Time
	cause, detail string
	id            int64
}

// releaseClaimedIfIdle 租约不在租时把本晚认领(reserved)的卡槽还回 free。
func (s *Service) releaseClaimedIfIdle(leaseID int64) error {
	return s.inTx(func(tx *sqlx.Tx) error {
		l, err := s.repo.lockLease(tx, leaseID)
		if err != nil || l == nil || l.State == LeaseActive || l.State == LeasePending || l.State == LeaseProvisioning {
			return err
		}
		if err := s.lockNodeRow(tx, l.NodeID); err != nil {
			return err
		}
		if _, err := s.repo.moveLeaseSlots(tx, l.ID, []string{SlotReserved}, SlotFree); err != nil {
			return err
		}
		return s.recomputeStockTx(tx, l.ProductID)
	})
}

// nightAttemptFailed 记一次开窗失败; 宿主机占用超过 应开通时刻+10min → 本晚判定失败(违约记录 + 整晚停机)。
// 其他原因本晚内继续重试, 窗口结束仍未开通由 nightSweepRound 判定。
func (s *Service) nightAttemptFailed(ctx context.Context, r nightRow, cause string, occupied bool, msg string) error {
	var failed bool
	err := s.inTx(func(tx *sqlx.Tx) error {
		failed = false
		_, l, n, err := s.lockNightCtx(tx, r.LeaseID, r.NightID)
		if err != nil || l == nil || n.State != "booked" {
			return err
		}
		if _, err := tx.Exec(`UPDATE lease_nights SET attempts=attempts+1, last_error=?, occupied=?, fail_cause=? WHERE id=?`,
			truncate(msg, 500), occupied, cause, n.ID); err != nil {
			return err
		}
		if !occupied || time.Now().Before(n.due(r.window()).Add(nightOccupiedGrace)) {
			return nil
		}
		failed = true
		return nil
	})
	if err != nil {
		return err
	}
	slog.Warn("包夜开窗未成功", "lease_id", r.LeaseID, "night", r.Night.Format(dateLayout), "cause", cause, "error", msg)
	if failed {
		return s.failNight(ctx, r, cause, msg)
	}
	return nil
}

// nightSweepRound 窗口已结束仍 booked 的夜晚: 停放中的租约 → 本晚失败(按最后一次失败原因; 从未尝试 → 平台原因
// provision_delay); 其他状态(冻结/已到期/已清除) → ended, 不记停机(买家侧原因或租约已结束)。
func (s *Service) nightSweepRound(ctx context.Context) {
	today := compute.NightDate(time.Now().In(shanghai()))
	for _, r := range s.selectNights(`n.state='booked' AND n.night_date <= ?`, today.Format(dateLayout)) {
		if ctx.Err() != nil {
			return
		}
		if time.Now().Before(compute.NightEndAt(r.Night, r.window())) {
			continue
		}
		var state, cause string
		var attempts int
		var fc sql.NullString
		if err := s.repo.db.QueryRowx(`SELECT l.state, n.attempts, n.fail_cause FROM lease_nights n JOIN leases l ON l.id=n.lease_id
			WHERE n.id=?`, r.NightID).Scan(&state, &attempts, &fc); err != nil {
			slog.Error("包夜收尾: 读取夜晚失败", "night_id", r.NightID, "error", err)
			continue
		}
		var err error
		if state == LeaseParked {
			cause = fc.String
			if attempts == 0 || cause == "" {
				cause = OutageProvisionDelay
			}
			err = s.failNight(ctx, r, cause, "窗口结束仍未开通")
		} else {
			_, err = s.repo.db.Exec(`UPDATE lease_nights SET state='ended', ended_at=NOW() WHERE id=? AND state='booked'`, r.NightID)
		}
		if err != nil {
			slog.Error("包夜收尾失败", "night_id", r.NightID, "error", err)
		}
	}
}

// failNight 本晚判定失败: 夜晚 failed、认领的卡 → draining + stop(容器可能已部分启动)、整晚 [应开通时刻, 窗口结束) 记停机;
// 宿主机占用另写违约记录(审计 lease.night_unpark_failed)并通知供给方。
func (s *Service) failNight(ctx context.Context, r nightRow, cause, msg string) error {
	var outageID int64
	var l *Lease
	var ord *compute.Order
	var occupied bool
	err := s.inTx(func(tx *sqlx.Tx) error {
		outageID, l = 0, nil
		o, cur, n, err := s.lockNightCtx(tx, r.LeaseID, r.NightID)
		if err != nil || cur == nil || n.State != "booked" {
			return err
		}
		if _, err := tx.Exec(`UPDATE lease_nights SET state='failed', failed_at=NOW(), fail_cause=? WHERE id=?`, cause, n.ID); err != nil {
			return err
		}
		moved, err := s.repo.moveLeaseSlots(tx, cur.ID, []string{SlotReserved}, SlotDraining)
		if err != nil {
			return err
		}
		if moved > 0 {
			if err := s.ensureStopPayloadTx(tx, cur, LeaseTaskPayload{LeaseID: cur.ID, Reason: "包夜本晚开通失败", Nightly: true}); err != nil {
				return err
			}
		}
		w := r.window()
		start, end := n.due(w), compute.NightEndAt(n.NightDate, w)
		if cur.AccessUntil != nil && cur.AccessUntil.Before(end) {
			end = *cur.AccessUntil
		}
		detail := fmt.Sprintf("包夜 %s 晚未能开通: %s", n.NightDate.Format(dateLayout), deref(n.LastError))
		if end.After(start) {
			if outageID, err = s.insertNightOutageTx(tx, o, cur, n, cause, start, end, truncate(detail, 500)); err != nil {
				return err
			}
		}
		occupied = n.Occupied
		if occupied {
			if err := s.repo.audit(tx, 0, "lease.night_unpark_failed", "lease", cur.ID, nil, map[string]any{
				"night_date": n.NightDate.Format(dateLayout), "node_id": cur.NodeID, "cause": cause,
				"attempts": n.Attempts, "last_error": deref(n.LastError), "outage_id": outageID}, ""); err != nil {
				return err
			}
		}
		l, ord = cur, o
		return s.recomputeStockTx(tx, cur.ProductID)
	})
	if err != nil || l == nil {
		return err
	}
	slog.Error("运营告警: 包夜本晚开通失败", "lease_no", l.LeaseNo, "node_id", l.NodeID, "night", r.Night.Format(dateLayout),
		"cause", cause, "occupied", occupied, "error", msg)
	link := "/console/buyer/orders/" + ord.OrderNo
	s.notifyLink(ord.BuyerID, "包夜本晚未能开通",
		fmt.Sprintf("订单 %s 的 %s 晚资源未能按时开通, 已计入停机补偿(默认顺延一晚), 可在订单详情查看。", ord.OrderNo, r.Night.Format(dateLayout)), link)
	if occupied {
		if p, err := s.products.ManagedProduct(l.ProductID); err == nil && p != nil {
			s.notifyLink(p.SupplierID, "包夜节点违约: 窗口内 GPU 被占用",
				fmt.Sprintf("节点 #%d 在 %s 晚包夜窗口开始后仍有宿主机进程占用已售 GPU, 当晚无法交付, 已记为供给方责任停机。请在夜间窗口内保持已售 GPU 空闲。",
					l.NodeID, r.Night.Format(dateLayout)), "/console/supplier/nodes")
		}
	}
	if outageID > 0 {
		s.afterSettle(ctx, outageID)
	}
	return nil
}

// insertNightOutageTx 包夜停机段(已结束)写入并结算补偿, 记到夜晚行 outage_id。幂等: 夜晚已有停机段则跳过。
func (s *Service) insertNightOutageTx(tx *sqlx.Tx, o *compute.Order, l *Lease, n *LeaseNight, cause string, start, end time.Time, detail string) (int64, error) {
	if n.OutageID != nil {
		return 0, nil
	}
	orderID, err := s.coveringOrderID(tx, l.OrderID, start)
	if err != nil {
		return 0, err
	}
	res, err := tx.Exec(`INSERT INTO lease_outages (lease_id, order_id, node_id, cause, liable, started_at, ended_at, detail)
		VALUES (?,?,?,?,?,?,?,?)`, l.ID, orderID, l.NodeID, cause, outageLiable(cause), start, end, detail)
	if err != nil {
		return 0, err
	}
	id, err := res.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE lease_nights SET outage_id=? WHERE id=?`, id, n.ID); err != nil {
		return 0, err
	}
	n.OutageID = &id
	return id, s.settleOutageTx(tx, o, l, id)
}

// recordProvisionDelay 首晚开通(provision 流水线激活)晚于应开通时刻 ≥ min_minutes: 记 provision_delay(平台责任)。
// 在 activate 事务内调用(已锁订单与租约)。
func (s *Service) recordProvisionDelayTx(tx *sqlx.Tx, o *compute.Order, l *Lease, plan *nightPlan, activated time.Time) (int64, error) {
	n, err := s.repo.firstBookedNight(tx, l.ID)
	if err != nil || n == nil {
		return 0, err
	}
	if _, err := tx.Exec(`UPDATE lease_nights SET state='active', unparked_at=?, attempts=attempts+1 WHERE id=?`, activated, n.ID); err != nil {
		return 0, err
	}
	due := n.due(plan.Window)
	end := activated
	if we := compute.NightEndAt(n.NightDate, plan.Window); we.Before(end) {
		end = we
	}
	if end.Sub(due) < time.Duration(s.cfg.Outage.MinMinutes)*time.Minute {
		return 0, nil
	}
	return s.insertNightOutageTx(tx, o, l, n, OutageProvisionDelay, due, end, "包夜首晚开通耗时")
}

// ===== 关窗提醒 =====

func (s *Service) nightRemindRound(ctx context.Context) {
	if s.rdb == nil {
		return
	}
	today := compute.NightDate(time.Now().In(shanghai()))
	rows := s.selectNights(`n.state='active' AND l.state='active' AND n.night_date BETWEEN ? AND ?`,
		today.AddDate(0, 0, -1).Format(dateLayout), today.Format(dateLayout))
	for _, r := range rows {
		if ctx.Err() != nil {
			return
		}
		l, err := s.repo.getLease(s.repo.db, r.LeaseID)
		if err != nil || l == nil || l.AccessUntil == nil {
			continue
		}
		w := r.window()
		we := compute.NightEndAt(r.Night, w)
		left := time.Until(we)
		if !we.Before(*l.AccessUntil) || left <= 0 {
			continue // 最后一晚由到期提醒负责
		}
		for _, m := range nightRemindMins {
			if left > time.Duration(m)*time.Minute {
				continue
			}
			key := fmt.Sprintf("fleet:night_remind:%d:%s:%d", l.ID, r.Night.Format(dateLayout), m)
			if !s.firstSend(ctx, key) {
				break
			}
			mins := int(left.Round(time.Minute) / time.Minute)
			if mins < 1 {
				mins = 1
			}
			next := compute.NightStartAt(r.Night.AddDate(0, 0, 1), w)
			s.notifyBuyerOfLease(l, "包夜窗口即将结束",
				fmt.Sprintf("租约 %s 将于 %s 停机(约 %d 分钟后), 容器与数据保留, 下一晚预订将于 %s 自动开放。请及时保存进度。",
					l.LeaseNo, we.In(shanghai()).Format("01-02 15:04"), mins, next.In(shanghai()).Format("01-02 15:04")))
			s.nightWall(ctx, l, mins)
			break
		}
	}
}

// notifyBuyerOfLease 站内信给租约买家, 链接到订单详情。
func (s *Service) notifyBuyerOfLease(l *Lease, title, content string) {
	var orderNo string
	if err := s.repo.db.Get(&orderNo, `SELECT order_no FROM orders WHERE id=?`, l.OrderID); err != nil {
		slog.Error("包夜提醒: 读取订单号失败", "lease_no", l.LeaseNo, "error", err)
		return
	}
	s.notifyLink(l.BuyerID, title, content, "/console/buyer/orders/"+orderNo)
}

// nightWall 容器内 wall 广播(尽力而为)。
func (s *Service) nightWall(ctx context.Context, l *Lease, mins int) {
	cmd, err := nightWallCommand(l.LeaseNo, mins)
	if err != nil {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, err := s.dialLeaseNode(wctx, l.NodeID)
	if err != nil {
		slog.Info("包夜提醒 wall 未送达(节点连接失败)", "lease_no", l.LeaseNo, "error", err)
		return
	}
	defer client.Close()
	if _, err := runRemote(wctx, client, cmd, 20*time.Second); err != nil {
		slog.Info("包夜提醒 wall 未送达", "lease_no", l.LeaseNo, "error", err)
	}
}

// ===== 停机补偿: 包夜补加一晚(docs/25 §25.7.6) =====

// nightExtension 包夜顺延计划。ok=false: 补加的那晚卡已被订走, 改退费。night 为零值: 本晚已补过一晚, 只记顺延不再加晚。
type nightExtension struct {
	ok    bool
	night time.Time
	slots []Slot
}

func (e *nightExtension) describe() string {
	if e.night.IsZero() {
		return "本晚已补加过"
	}
	return e.night.Format(dateLayout)
}

// planNightExtensionTx 非包夜返回 nil。同一晚(停机开始所在的晚)内的多段停机只补一晚: 已有一段按顺延结算 → 不再加晚。
// 否则锁节点行与本租约的卡槽, 试写末晚之后一晚的预订(唯一键冲突 → ok=false)。调用方已锁订单与租约。
func (s *Service) planNightExtensionTx(tx *sqlx.Tx, l *Lease, o *Outage) (*nightExtension, error) {
	plan, err := s.repo.leaseNightPlan(tx, l.ID)
	if err != nil || plan == nil {
		return nil, err
	}
	night := compute.LastStartedNight(o.StartedAt, plan.Window)
	ws, we := compute.NightStartAt(night, plan.Window), compute.NightEndAt(night, plan.Window)
	var dup int
	if err := tx.Get(&dup, `SELECT COUNT(*) FROM lease_outages WHERE lease_id=? AND id<>? AND counted=1 AND remedy='extend'
		AND started_at>=? AND started_at<?`, l.ID, o.ID, ws, we); err != nil {
		return nil, err
	}
	if dup > 0 {
		return &nightExtension{ok: true}, nil
	}
	if err := s.lockNodeRow(tx, l.NodeID); err != nil {
		return nil, err
	}
	var last sql.NullTime
	if err := tx.Get(&last, `SELECT MAX(night_date) FROM lease_nights WHERE lease_id=?`, l.ID); err != nil {
		return nil, err
	}
	if !last.Valid {
		return &nightExtension{}, nil
	}
	next := compute.NightDate(last.Time).AddDate(0, 0, 1)
	var slots []Slot
	if err := tx.Select(&slots, `SELECT id, node_id, gpu_index, gpu_uuid, pci_bus_id, vram_mb, topo_group, state, lease_id,
		state_changed_at FROM gpu_slots WHERE id IN (SELECT DISTINCT slot_id FROM night_bookings WHERE lease_id=?)
		ORDER BY gpu_index FOR UPDATE`, l.ID); err != nil {
		return nil, err
	}
	if len(slots) != l.Cards {
		return &nightExtension{}, nil
	}
	ok, err := s.repo.insertNightBookings(tx, l.ID, slots, []time.Time{next})
	if err != nil {
		return nil, err
	}
	return &nightExtension{ok: ok, night: next, slots: slots}, nil
}

// applyNightExtensionTx 预订已写入: 加一晚开停机记录, 租期整体后移 24h(末晚窗口结束 = 新末晚窗口结束)。
func (s *Service) applyNightExtensionTx(tx *sqlx.Tx, ord *compute.Order, l *Lease, e *nightExtension) error {
	if e.night.IsZero() {
		return nil
	}
	if err := s.repo.insertLeaseNights(tx, l.ID, []time.Time{e.night}); err != nil {
		return err
	}
	if err := s.extendForOutageTx(tx, ord, l, 24*time.Hour); err != nil {
		return err
	}
	return s.recomputeStockTx(tx, l.ProductID)
}
