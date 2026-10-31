package fleet

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"tokenfactory/internal/compute"

	"github.com/jmoiron/sqlx"
	"github.com/redis/go-redis/v9"
)

// 租约周期任务(docs/25 §25.7.1 / §25.11, T-087):
//
//	到期扫描   30s  active/parked 到期 → 订单 completed(经 compute 守卫式流转) / 订单非 active 时租约侧 → grace;
//	                suspended 到期 → 卡槽 draining + stop(租约保持 suspended, 不进宽限计时);
//	                grace 期满 → purging + purge(有待支付 restart 续租时暂缓)
//	到期提醒   1min 到期前 24h/2h/30min 站内通知 + 容器内 wall; 进入宽限的站内通知(Redis 去重键 lease_no+档位)
//	挖矿扫描   fleet.mining.interval(默认 10min) 见 mining_scan.go
//	对账       5min 收尾任务补入队; 在租容器与节点实况对账, 节点重启后 防火墙 → docker start → 主机密钥 → 网关自检
//
// 多实例部署时以 Redis SET NX 做周期互斥(拿不到锁本轮跳过); 所有状态写入都是守卫式事务, 互斥只为省掉重复的节点操作。

const (
	expirySweepInterval = 30 * time.Second
	settleInterval      = time.Minute
	remindInterval      = time.Minute
	reconcileInterval   = 5 * time.Minute
	sweepBatch          = 200
	remindKeyTTL        = 96 * time.Hour // 覆盖最长宽限 72h + 余量
	reconcileNodeBudget = 4 * time.Minute
	reconcileAlertTag   = "容器对账: "
)

// 停机原因(docs/25 §25.7.6 lease_outages.cause)。
const (
	OutageNodeOffline   = "node_offline"
	OutageContainerDown = "container_down"
)

// LeaseObservation 对账得到的一次租约可用性观测, 交给停机认定(T-105)开/关 lease_outages。
type LeaseObservation struct {
	LeaseID int64
	NodeID  int64
	LeaseNo string
	Up      bool
	Cause   string // Up=false 时: node_offline / container_down
	Detail  string
	At      time.Time
}

// OutageRecorder 停机认定与补偿(T-105)的接入点。T-087/T-088 只产生观测与误冻结事件, 不开/关 lease_outages。
type OutageRecorder interface {
	ObserveLease(ctx context.Context, o LeaseObservation)
	// WrongFreeze 解冻时运营认定为误冻结: [frozenAt, unfrozenAt) 计入平台责任停机(docs/25 §25.7.3)。
	WrongFreeze(ctx context.Context, leaseID int64, frozenAt, unfrozenAt time.Time)
}

// SetRedis 注入 Redis(main 调用, RunLeaseJobs 之前)。
func (s *Service) SetRedis(rdb *redis.Client) { s.rdb = rdb }

// SetOutageRecorder 覆盖停机认定实现(默认 outageBook, 测试注入记录器); 设为 nil 时观测只写日志。
func (s *Service) SetOutageRecorder(r OutageRecorder) { s.outage = r }

func (s *Service) observe(ctx context.Context, o LeaseObservation) {
	if o.At.IsZero() {
		o.At = time.Now()
	}
	if s.outage != nil {
		s.outage.ObserveLease(ctx, o)
		return
	}
	if !o.Up {
		slog.Warn("租约不可用(停机认定 T-105 未接入, 仅记录)", "lease_no", o.LeaseNo, "node_id", o.NodeID,
			"cause", o.Cause, "detail", o.Detail)
	}
}

// RunLeaseJobs 启动租约周期任务, 阻塞到 ctx 结束。
func (s *Service) RunLeaseJobs(ctx context.Context) {
	if s.rdb == nil {
		slog.Warn("fleet 未注入 Redis: 到期提醒(去重依赖 Redis)不执行, 周期任务不做多实例互斥")
	}
	var wg sync.WaitGroup
	wg.Add(6)
	go func() { defer wg.Done(); s.jobLoop(ctx, "remind", remindInterval, s.RemindRound) }()
	go func() { defer wg.Done(); s.jobLoop(ctx, "outage", outageInterval, s.OutageRound) }()
	go func() { defer wg.Done(); s.jobLoop(ctx, "credit", creditInterval, s.CreditRound) }()
	go func() { defer wg.Done(); s.jobLoop(ctx, "mining", s.miningInterval(), s.MiningScanRound) }()
	go func() { defer wg.Done(); s.jobLoop(ctx, "reconcile", reconcileInterval, s.ReconcileRound) }()
	go func() { defer wg.Done(); s.jobLoop(ctx, "settle", settleInterval, s.SettleRound) }()
	s.jobLoop(ctx, "expiry", expirySweepInterval, s.ExpiryRound)
	wg.Wait()
}

func (s *Service) jobLoop(ctx context.Context, name string, every time.Duration, fn func(context.Context)) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if s.acquireJob(ctx, name, every) {
			fn(ctx)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

var jobOwner = func() string {
	h, _ := os.Hostname()
	return fmt.Sprintf("%s/%d", h, os.Getpid())
}()

// acquireJob 本周期互斥: 锁时长取周期的 9/10, 到期自然释放。Redis 不可用时照常执行(写入均为守卫式)。
func (s *Service) acquireJob(ctx context.Context, name string, every time.Duration) bool {
	if s.rdb == nil {
		return true
	}
	ok, err := s.rdb.SetNX(ctx, "fleet:job:"+name, jobOwner, every*9/10).Result()
	if err != nil {
		slog.Error("fleet 周期任务互斥锁失败, 本轮照常执行", "job", name, "error", err)
		return true
	}
	return ok
}

// ===== 到期扫描 =====

// ExpiryRound 一轮到期扫描(导出供测试与运维脚本同步调用)。
func (s *Service) ExpiryRound(ctx context.Context) {
	s.expireActive(ctx)
	s.expireSuspended()
	s.expireGrace()
}

func (s *Service) selectLeases(where string, args ...any) []Lease {
	var list []Lease
	if err := s.repo.db.Select(&list, `SELECT `+leaseColumns+` FROM leases WHERE `+where+` LIMIT ?`, append(args, sweepBatch)...); err != nil {
		slog.Error("租约到期扫描查询失败", "where", where, "error", err)
		return nil
	}
	return list
}

// expireActive active/parked 且 access_until 已过。订单 active → compute 守卫式 completed(同事务 ReleaseTx 转 grace +
// 入队 stop); 订单冻结/退款中 → 以租约为准, 租约侧转 grace(docs/25 §25.7.1"冻结或退款中的订单到期")。
func (s *Service) expireActive(ctx context.Context) {
	for _, l := range s.selectLeases(`state IN ('active','parked') AND access_until IS NOT NULL AND access_until<=NOW() ORDER BY access_until`) {
		if ctx.Err() != nil {
			return
		}
		o, err := s.products.ManagedOrderByID(l.OrderID)
		if err != nil || o == nil {
			slog.Error("到期租约读取订单失败", "lease_no", l.LeaseNo, "order_id", l.OrderID, "error", err)
			continue
		}
		if o.Status == "active" {
			moved, err := s.products.CompleteExpiredManagedOrder(o.OrderNo)
			if err != nil {
				slog.Error("托管订单到期流转失败", "order_no", o.OrderNo, "lease_no", l.LeaseNo, "error", err)
			} else if !moved && o.LeaseEnd != nil && o.LeaseEnd.After(time.Now()) {
				// 订单租期未到而租约访问期限已到: 两者应同事务更新(续租 extend), 不一致说明数据异常, 不自行裁决。
				slog.Error("运营告警: 租约访问期限早于订单租期, 需核查", "lease_no", l.LeaseNo, "order_no", o.OrderNo,
					"access_until", l.AccessUntil, "lease_end_at", o.LeaseEnd)
			}
			continue
		}
		if err := s.expireLeaseSide(l.ID, l.OrderID); err != nil {
			slog.Error("租约侧到期处理失败", "lease_no", l.LeaseNo, "order_no", o.OrderNo, "error", err)
		}
	}
}

// expireLeaseSide 订单非 active(冻结/退款中)的到期租约: 订单状态不动, 租约 → grace + stop。
// 锁顺序 订单 → 租约 → 节点行 → 卡槽 → 商品行。
func (s *Service) expireLeaseSide(leaseID, orderID int64) error {
	return s.inTx(func(tx *sqlx.Tx) error {
		o, err := s.products.LockManagedOrderTx(tx, orderID)
		if err != nil || o == nil {
			return err
		}
		if o.Status == "active" {
			return nil // 下一轮经 compute 守卫式流转
		}
		l, err := s.repo.lockLease(tx, leaseID)
		if err != nil {
			return err
		}
		if l == nil || (l.State != LeaseActive && l.State != LeaseParked) || l.AccessUntil == nil || l.AccessUntil.After(time.Now()) {
			return nil
		}
		if err := s.lockNodeRow(tx, l.NodeID); err != nil {
			return err
		}
		switch o.Status {
		case "cancelled", "refunded":
			// 订单已终态而租约仍在租: 释放钩子曾失败或被绕过, 按宽限 0 处理。
			if err := s.purgeTx(tx, l, "订单已"+o.Status+"(到期扫描)"); err != nil {
				return err
			}
		default:
			if err := s.enterGraceTx(tx, l, o, "租期结束(订单"+o.Status+")"); err != nil {
				return err
			}
		}
		slog.Info("租约到期(订单非 active, 以租约为准)", "lease_no", l.LeaseNo, "order_no", o.OrderNo, "order_status", o.Status, "to", l.State)
		return s.recomputeStockTx(tx, l.ProductID)
	})
}

// enterGraceTx 租约 → grace(宽限按订单计费方式快照取档), 卡槽 → draining, 入队 stop。调用方已锁订单、租约与节点行。
func (s *Service) enterGraceTx(tx *sqlx.Tx, l *Lease, o *compute.Order, reason string) error {
	if l.DeliveryKind == "bare_metal" {
		// 整机无宽限期(docs/25 §25.7.4): 到期即断开、删账户、节点待重装。
		return s.purgeTx(tx, l, reason+"(整机无宽限)")
	}
	mode, err := compute.OrderPricingMode(o)
	if err != nil {
		return err
	}
	until := time.Now().Add(s.graceFor(mode))
	if err := s.mustTransition(tx, l, LeaseGrace, ", grace_until=?", until); err != nil {
		return err
	}
	l.GraceUntil = &until
	if _, err := s.repo.moveLeaseSlots(tx, l.ID, []string{SlotLeased, SlotReserved}, SlotDraining); err != nil {
		return err
	}
	// 冻结中到期已入队过 stop 的(解冻转 grace), 不重复入队。
	return s.ensureStopTx(tx, l, reason)
}

// expireSuspended 冻结中到期: 停容器、释放卡, 容器与数据卷保留作核查现场; 租约保持 suspended, 不进宽限计时,
// 运营结案(解冻/退款)时再流转(docs/25 §25.7.1)。
func (s *Service) expireSuspended() {
	list := s.selectLeases(`state='suspended' AND access_until IS NOT NULL AND access_until<=NOW()
		AND EXISTS (SELECT 1 FROM gpu_slots g WHERE g.lease_id=leases.id AND g.state IN ('leased','reserved'))`)
	for _, l := range list {
		err := s.inTx(func(tx *sqlx.Tx) error {
			cur, err := s.repo.lockLease(tx, l.ID)
			if err != nil || cur == nil || !stopApplies(cur, time.Now()) || cur.State != LeaseSuspended {
				return err
			}
			if err := s.lockNodeRow(tx, cur.NodeID); err != nil {
				return err
			}
			n, err := s.repo.moveLeaseSlots(tx, cur.ID, []string{SlotLeased, SlotReserved}, SlotDraining)
			if err != nil || n == 0 {
				return err
			}
			if err := s.ensureStopTx(tx, cur, "冻结中租期结束"); err != nil {
				return err
			}
			if cur.DeliveryKind == "bare_metal" {
				// 整机: stop 只撤权限、结束进程, 账户与家目录保留作现场; 节点同时待重装(买家曾有 root)。
				if err := s.markBareMetalReimageTx(tx, cur, "冻结中租期结束"); err != nil {
					return err
				}
			}
			slog.Info("冻结中租约到期: 停容器释放卡, 保留现场", "lease_no", cur.LeaseNo, "slots", n)
			return s.recomputeStockTx(tx, cur.ProductID)
		})
		if err != nil {
			slog.Error("冻结租约到期处理失败", "lease_no", l.LeaseNo, "error", err)
		}
	}
}

// ensureStopTx 租约当前节点上没有未结束的 stop 时入队一条。
func (s *Service) ensureStopTx(tx *sqlx.Tx, l *Lease, reason string) error {
	live, err := s.repo.liveLeaseTasks(tx, l.ID, KindStop)
	if err != nil {
		return err
	}
	for _, t := range live {
		if t.NodeID == l.NodeID {
			return nil
		}
	}
	_, err = s.repo.insertLeaseTaskMax(tx, l.NodeID, l.ID, KindStop, prioStop, cleanupMaxAttempts, LeaseTaskPayload{LeaseID: l.ID, Reason: reason})
	return err
}

// expireGrace 宽限期满 → purging + purge。父单有待支付的 restart 续租时暂缓(续租单超时关单后下一轮清除)。
func (s *Service) expireGrace() {
	for _, l := range s.selectLeases(`state='grace' AND grace_until IS NOT NULL AND grace_until<=NOW() ORDER BY grace_until`) {
		err := s.inTx(func(tx *sqlx.Tx) error {
			o, err := s.products.LockManagedOrderTx(tx, l.OrderID)
			if err != nil {
				return err
			}
			if o != nil {
				pending, err := s.products.HasPendingRestartRenewalTx(tx, o.ID)
				if err != nil {
					return err
				}
				if pending {
					slog.Info("宽限期满但有待支付的续租单, 暂缓清除", "lease_no", l.LeaseNo, "order_no", o.OrderNo)
					return nil
				}
			}
			cur, err := s.repo.lockLease(tx, l.ID)
			if err != nil || cur == nil || cur.State != LeaseGrace || cur.GraceUntil == nil || cur.GraceUntil.After(time.Now()) {
				return err
			}
			if err := s.lockNodeRow(tx, cur.NodeID); err != nil {
				return err
			}
			if err := s.purgeTx(tx, cur, "宽限期满"); err != nil {
				return err
			}
			slog.Info("宽限期满, 入队清除", "lease_no", cur.LeaseNo)
			return s.recomputeStockTx(tx, cur.ProductID)
		})
		if err != nil {
			slog.Error("宽限期满处理失败", "lease_no", l.LeaseNo, "error", err)
		}
	}
}

// ===== 到期提醒 =====

// remindTier 到期提醒档位(docs/25 §25.7.1)。
type remindTier struct {
	name   string
	before time.Duration
}

var remindTiers = []remindTier{{"24h", 24 * time.Hour}, {"2h", 2 * time.Hour}, {"30m", 30 * time.Minute}}

// currentRemindTier 当前应发的档位: 只取短于本租期(term)的档位(租期不足 24h 时取后两档、不足 2h 只取 30min,
// 即 docs/25"不足的周期按比例取前两档"的 S2 口径), 剩余时间落入的最小档; 不在任何档内返回 nil。
func currentRemindTier(term, remaining time.Duration) *remindTier {
	var cur *remindTier
	for i := range remindTiers {
		t := &remindTiers[i]
		if t.before < term && remaining <= t.before {
			cur = t
		}
	}
	return cur
}

// RemindRound 一轮到期提醒(导出供测试同步调用)。去重依赖 Redis, 未注入时不执行。
func (s *Service) RemindRound(ctx context.Context) {
	if s.rdb == nil {
		return
	}
	now := time.Now()
	for _, l := range s.selectLeases(`state IN ('active','parked') AND access_until>NOW() AND access_until<=NOW()+INTERVAL 24 HOUR`) {
		if ctx.Err() != nil {
			return
		}
		if l.ProvisionedAt == nil || l.AccessUntil == nil {
			continue
		}
		tier := currentRemindTier(l.AccessUntil.Sub(*l.ProvisionedAt), l.AccessUntil.Sub(now))
		if tier == nil || !s.firstSend(ctx, fmt.Sprintf("fleet:remind:%s:%s:%d", l.LeaseNo, tier.name, l.AccessUntil.Unix())) {
			continue
		}
		mins := int(l.AccessUntil.Sub(now).Round(time.Minute) / time.Minute)
		if mins < 1 {
			mins = 1
		}
		if o, err := s.products.ManagedOrderByID(l.OrderID); err == nil && o != nil {
			s.notifyLink(l.BuyerID, "租期即将结束",
				fmt.Sprintf("订单 %s 的算力环境将于 %s 到期(约 %s 后)。到期后容器停止、GPU 释放, 数据卷在宽限期内保留, 宽限期满将清除; 如需继续使用请及时续租。",
					o.OrderNo, l.AccessUntil.Format("2006-01-02 15:04"), humanMinutes(mins)),
				"/console/buyer/orders/"+o.OrderNo)
		}
		s.wallReminder(ctx, &l, mins)
	}
	// 刚进入宽限的租约: 通知买家宽限截止时间(进入宽限即 access_changed_at 更新, 只看最近 1 小时)。
	for _, l := range s.selectLeases(`state='grace' AND grace_until>NOW() AND access_changed_at>=NOW()-INTERVAL 1 HOUR`) {
		if l.GraceUntil == nil || !s.firstSend(ctx, fmt.Sprintf("fleet:remind:%s:grace:%d", l.LeaseNo, l.GraceUntil.Unix())) {
			continue
		}
		if o, err := s.products.ManagedOrderByID(l.OrderID); err == nil && o != nil {
			s.notifyLink(l.BuyerID, "租期已结束",
				fmt.Sprintf("订单 %s 的租期已结束, 容器已停止、GPU 已释放。数据卷保留至 %s, 期间续租可恢复原环境(原节点有空闲卡时); 逾期将清除且不可恢复。",
					o.OrderNo, l.GraceUntil.Format("2006-01-02 15:04")),
				"/console/buyer/orders/"+o.OrderNo)
		}
	}
}

func humanMinutes(m int) string {
	if m >= 60 {
		if m%60 == 0 {
			return fmt.Sprintf("%d 小时", m/60)
		}
		return fmt.Sprintf("%d 小时 %d 分钟", m/60, m%60)
	}
	return fmt.Sprintf("%d 分钟", m)
}

// firstSend Redis SET NX 去重; Redis 出错按"未发送"处理(宁可漏发一次提醒, 不重复刷屏)。
func (s *Service) firstSend(ctx context.Context, key string) bool {
	ok, err := s.rdb.SetNX(ctx, key, "1", remindKeyTTL).Result()
	if err != nil {
		slog.Error("到期提醒去重失败, 本次不发送", "key", key, "error", err)
		return false
	}
	return ok
}

// wallReminder 容器内 wall 广播, 尽力而为(失败只记日志)。parked 租约容器已停, 跳过。
func (s *Service) wallReminder(ctx context.Context, l *Lease, mins int) {
	if l.State != LeaseActive || l.DeliveryKind != "container" {
		return
	}
	cmd, err := wallCommand(l.LeaseNo, mins)
	if err != nil {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	client, err := s.dialLeaseNode(wctx, l.NodeID)
	if err != nil {
		slog.Info("到期提醒 wall 未送达(节点连接失败)", "lease_no", l.LeaseNo, "error", err)
		return
	}
	defer client.Close()
	if _, err := runRemote(wctx, client, cmd, 20*time.Second); err != nil {
		slog.Info("到期提醒 wall 未送达", "lease_no", l.LeaseNo, "error", err)
	}
}

// ===== 对账 =====

// ReconcileRound 一轮对账(导出供测试同步调用)。
func (s *Service) ReconcileRound(ctx context.Context) {
	s.requeueStuckCleanup()
	s.reconcileContainers(ctx)
}

// requeueStuckCleanup 收尾任务补入队: stop/purge 因竞态被放弃(errTaskAborted)或丢失后, 卡槽会一直 draining。
// 已进死信(dead)的不补, 由运营处理。
func (s *Service) requeueStuckCleanup() {
	var stops []int64
	if err := s.repo.db.Select(&stops, `SELECT DISTINCT l.id FROM leases l
		JOIN gpu_slots g ON g.lease_id=l.id AND g.node_id=l.node_id AND g.state='draining'
		WHERE (l.state='grace' OR (l.state='suspended' AND l.access_until<=NOW()))
		AND NOT EXISTS (SELECT 1 FROM node_tasks t WHERE t.lease_id=l.id AND t.node_id=l.node_id AND t.kind='stop'
			AND t.state IN ('pending','running','dead'))
		LIMIT ?`, sweepBatch); err != nil {
		slog.Error("对账: 查询待补 stop 失败", "error", err)
	}
	for _, id := range stops {
		err := s.inTx(func(tx *sqlx.Tx) error {
			l, err := s.repo.lockLease(tx, id)
			if err != nil || l == nil || !stopApplies(l, time.Now()) {
				return err
			}
			slog.Warn("对账: 补入队 stop", "lease_no", l.LeaseNo)
			return s.ensureStopTx(tx, l, "对账补入队")
		})
		if err != nil {
			slog.Error("对账: 补入队 stop 失败", "lease_id", id, "error", err)
		}
	}
	var purges []int64
	if err := s.repo.db.Select(&purges, `SELECT l.id FROM leases l WHERE l.state='purging'
		AND NOT EXISTS (SELECT 1 FROM node_tasks t WHERE t.lease_id=l.id AND t.node_id=l.node_id AND t.kind='purge'
			AND t.state IN ('pending','running','dead') AND JSON_EXTRACT(t.payload, '$.cleanup_only') IS NULL)
		LIMIT ?`, sweepBatch); err != nil {
		slog.Error("对账: 查询待补 purge 失败", "error", err)
	}
	for _, id := range purges {
		err := s.inTx(func(tx *sqlx.Tx) error {
			l, err := s.repo.lockLease(tx, id)
			if err != nil || l == nil || l.State != LeasePurging {
				return err
			}
			slog.Warn("对账: 补入队 purge", "lease_no", l.LeaseNo)
			_, err = s.repo.insertLeaseTaskMax(tx, l.NodeID, l.ID, KindPurge, prioPurge, cleanupMaxAttempts, LeaseTaskPayload{LeaseID: l.ID, Reason: "对账补入队"})
			return err
		})
		if err != nil {
			slog.Error("对账: 补入队 purge 失败", "lease_id", id, "error", err)
		}
	}
}

// reconcileContainers 在租(active)容器租约逐节点对账, 节点并发上限 fleet.probe_concurrency。
func (s *Service) reconcileContainers(ctx context.Context) {
	var leases []Lease
	if err := s.repo.db.Select(&leases, `SELECT `+leaseColumns+` FROM leases WHERE state='active' AND delivery_kind='container'
		ORDER BY node_id, id`); err != nil {
		slog.Error("对账: 查询在租租约失败", "error", err)
		return
	}
	s.forEachLeaseNode(ctx, leases, reconcileNodeBudget, s.reconcileNode)
}

func (s *Service) reconcileNode(ctx context.Context, nodeID int64, leases []Lease) {
	down := func(l *Lease, cause, detail string) {
		if _, err := s.repo.setResourceAlert(s.repo.db, l.ID, reconcileAlertTag+detail); err != nil {
			slog.Error("写租约资源告警失败", "lease_no", l.LeaseNo, "error", err)
		}
		s.observe(ctx, LeaseObservation{LeaseID: l.ID, NodeID: nodeID, LeaseNo: l.LeaseNo, Cause: cause, Detail: detail})
	}
	up := func(l *Lease) {
		if _, err := s.repo.db.Exec(`UPDATE leases SET resource_alert=NULL WHERE id=? AND resource_alert LIKE ?`,
			l.ID, reconcileAlertTag+"%"); err != nil {
			slog.Error("清除对账告警失败", "lease_no", l.LeaseNo, "error", err)
		}
		s.observe(ctx, LeaseObservation{LeaseID: l.ID, NodeID: nodeID, LeaseNo: l.LeaseNo, Up: true})
	}
	n, err := s.repo.GetNode(nodeID)
	if err != nil || n == nil {
		slog.Error("对账: 读取节点失败", "node_id", nodeID, "error", err)
		return
	}
	client, err := s.dialLeaseNode(ctx, nodeID)
	if err != nil {
		// 节点离线由探活判定(node_offline 的开始/结束以节点状态为准), 这里只上报观测, 不写租约告警。
		for i := range leases {
			s.observe(ctx, LeaseObservation{LeaseID: leases[i].ID, NodeID: nodeID, LeaseNo: leases[i].LeaseNo,
				Cause: OutageNodeOffline, Detail: err.Error()})
		}
		return
	}
	defer client.Close()
	out, err := runRemote(ctx, client, cmdLeaseContainers, cmdTimeout)
	if err != nil {
		for i := range leases {
			down(&leases[i], OutageContainerDown, "读取节点容器列表失败: "+err.Error())
		}
		return
	}
	states := parseLeaseContainers(out)
	fw := false
	for i := range leases {
		l := &leases[i]
		st, ok := states[containerNameOf(l.LeaseNo)]
		switch {
		case !ok:
			down(l, OutageContainerDown, "节点上找不到租约容器")
		case st == "running":
			up(l)
		case st == "exited" || st == "created":
			restarted, err := s.restartLeaseContainer(ctx, client, n, l, &fw)
			switch {
			case err != nil:
				slog.Error("运营告警: 在租容器重启失败", "lease_no", l.LeaseNo, "node_id", nodeID, "error", err)
				cause := OutageContainerDown
				if errors.Is(err, errGatewayDown) {
					cause = OutageGatewayDown // 容器已起但网关不可达/指纹不符: 平台责任(docs/25 §25.7.6)
				}
				down(l, cause, "容器已停止且重启失败: "+err.Error())
			case restarted:
				slog.Info("在租容器已重启并通过网关自检", "lease_no", l.LeaseNo, "node_id", nodeID)
				up(l)
			}
		default:
			down(l, OutageContainerDown, "容器状态 "+truncate(st, 20))
		}
	}
}

// errGatewayDown 自检失败的原因是访问网关本身不可用(未接入/连不上/主机指纹不符), 停机记平台责任 gateway_down。
var errGatewayDown = errors.New("访问网关不可用")

// errLeaseMoved 重启过程中租约已不在租(到期/冻结/清除), 放弃重启。
var errLeaseMoved = errors.New("租约已不在租")

// restartLeaseContainer 节点重启/容器退出后的恢复(docs/25 §25.5.2 重启策略):
// ① 节点主机指纹(dialManaged 已校验) ② 重新下发 DOCKER-USER 规则(每节点一次) ③ docker start
// ④ 容器 sshd 主机密钥须与开通时一致 ⑤ 经网关以系统自检身份登录, 容器内可见 GPU 须与租约卡槽一致。
// ②失败不启动容器; ④⑤失败把容器停回去(不让未自检通过的环境对外), 下一轮再试。
// 返回 (false, nil) 表示租约已不在租, 未操作。
func (s *Service) restartLeaseContainer(ctx context.Context, client Runner, n *Node, l *Lease, fw *bool) (bool, error) {
	if !*fw {
		if _, err := s.applyFirewall(ctx, client, n); err != nil {
			return false, fmt.Errorf("重新下发租约端口防火墙规则失败(未启动容器): %w", err)
		}
		*fw = true
	}
	cur, err := s.repo.getLease(s.repo.db, l.ID)
	if err != nil {
		return false, err
	}
	if cur == nil || cur.State != LeaseActive || cur.NodeID != n.ID {
		return false, nil
	}
	start, err := startContainerCommand(l.LeaseNo)
	if err != nil {
		return false, err
	}
	out, err := runRemote(ctx, client, start, cmdTimeout)
	if err != nil || !strings.Contains(out, "@@started") {
		return false, fmt.Errorf("docker start 失败: %v", errText(err))
	}
	if err := s.verifyRestarted(ctx, client, cur); err != nil {
		s.stopBack(ctx, client, l.LeaseNo)
		if errors.Is(err, errLeaseMoved) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func (s *Service) verifyRestarted(ctx context.Context, client Runner, l *Lease) error {
	hk, err := hostKeyCommand(l.LeaseNo)
	if err != nil {
		return err
	}
	out, err := runRemote(ctx, client, hk, cmdTimeout)
	if err != nil {
		return fmt.Errorf("容器 sshd 未就绪: %w", err)
	}
	fp, err := parseHostKeyFP(out)
	if err != nil {
		return err
	}
	if l.UpstreamHostKeyFP == nil || *l.UpstreamHostKeyFP != fp {
		return fmt.Errorf("容器 sshd 主机密钥与开通时不一致(%s), 不恢复访问", fp)
	}
	if s.gateway == nil {
		return fmt.Errorf("%w: 访问网关(cmd/accessgw)未接入, 无法经网关自检", errGatewayDown)
	}
	got, err := s.gateway.RecheckLease(ctx, l.LeaseNo)
	if err != nil {
		var de *gatewayDialError
		var he *gatewayHostKeyError
		if errors.As(err, &de) || errors.As(err, &he) {
			return fmt.Errorf("%w: 经网关自检登录失败: %v", errGatewayDown, err)
		}
		return fmt.Errorf("经网关自检登录失败: %w", err)
	}
	slots, err := s.repo.leaseSlotsOnNode(s.repo.db, l.ID, l.NodeID)
	if err != nil {
		return err
	}
	var want []string
	for _, sl := range slots {
		if sl.State == SlotLeased {
			want = append(want, sl.GPUUUID)
		}
	}
	if gpus := ParseGPUListL(got); !sameUUIDSet(gpus, want) {
		return fmt.Errorf("自检: 容器内可见 GPU %v 与租约卡槽 %v 不一致", gpus, want)
	}
	// 启动与自检期间租约可能已到期/冻结: 到期的由 stop 收尾, 但若 stop 先于本次 start 执行完, 卡已释放,
	// 容器必须停回去。冻结(suspended)保持运行(冻结本就不停容器)。
	cur, err := s.repo.getLease(s.repo.db, l.ID)
	if err != nil {
		return err
	}
	if cur == nil || (cur.State != LeaseActive && cur.State != LeaseSuspended) || cur.NodeID != l.NodeID {
		return errLeaseMoved
	}
	return nil
}

// stopBack 重启后校验未通过: 把容器停回去(尽力而为, 失败只记日志; 下一轮对账会再次处理)。
func (s *Service) stopBack(ctx context.Context, client Runner, leaseNo string) {
	cmd, err := stopContainerCommand(leaseNo)
	if err != nil {
		return
	}
	if _, err := runRemote(context.WithoutCancel(ctx), client, cmd, stopTimeout); err != nil {
		slog.Error("运营告警: 未通过自检的租约容器停止失败", "lease_no", leaseNo, "error", err)
	}
}
