package fleet

// 挖矿特征扫描周期任务(docs/25 §25.7.3, Q-42 / Q-09, T-088)。
//
// 对象: state=active 的容器型租约(bare_metal 不在范围: 买家持有 root, 进程表与连接表不可信, 由整机重装确认兜底)。
// 手段: 经节点管理 SSH 通道执行 miningScanCommand(宿主机 docker top + 探针镜像加入容器网络命名空间读 /proc/net/tcp{,6}),
//       命令只拼接校验过的租约号(容器名)与探针镜像引用, 不含任何买家可控字符串。
// 判定: stratum 参数 / 矿池地址外连 → 自动冻结(与运营冻结同一路径: 订单 frozen + 租约 suspended + 断开会话 + 吊销凭证),
//       写风控告警(processing, resolution=auto_frozen)与审计日志, 通知买家与供给方;
//       仅矿工进程名 / 矿池常用端口 → 只写风控告警(pending)待人工, 同一订单在有未结告警或 24h 内已告警时不重复写。
// 扫描失败(连不上节点、输出不完整)本轮不判定, 只记日志, 不影响租约。

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"
)

const (
	miningNodeBudget    = 5 * time.Minute
	miningAlertType     = "mining_suspect"
	miningFreezeType    = "mining_freeze"
	miningAlertDedup    = 24 * time.Hour
	miningAuditAction   = "mining_auto_freeze"
	miningResAutoFrozen = "auto_frozen"
)

// miningInterval fleet.mining.interval, 未经配置加载(测试直接构造)时取默认 10min。
func (s *Service) miningInterval() time.Duration {
	if s.cfg.Mining.Interval > 0 {
		return s.cfg.Mining.Interval
	}
	return 10 * time.Minute
}

// MiningScanRound 一轮挖矿特征扫描(导出供测试与运维脚本同步调用)。
func (s *Service) MiningScanRound(ctx context.Context) {
	rules, err := newMiningRules(s.cfg.Mining)
	if err != nil {
		slog.Error("运营告警: 挖矿特征配置非法, 本轮不扫描", "error", err)
		return
	}
	var leases []Lease
	if err := s.repo.db.Select(&leases, `SELECT `+leaseColumns+` FROM leases WHERE state='active' AND delivery_kind='container'
		ORDER BY node_id, id`); err != nil {
		slog.Error("挖矿扫描: 查询在租租约失败", "error", err)
		return
	}
	s.forEachLeaseNode(ctx, leases, miningNodeBudget, func(nctx context.Context, nodeID int64, ls []Lease) {
		s.miningScanNode(nctx, nodeID, ls, rules)
	})
}

// forEachLeaseNode 按节点分组并发处理租约(并发上限 fleet.probe_concurrency, 每节点独立时限)。
func (s *Service) forEachLeaseNode(ctx context.Context, leases []Lease, budget time.Duration,
	fn func(ctx context.Context, nodeID int64, ls []Lease)) {
	byNode := map[int64][]Lease{}
	var order []int64
	for _, l := range leases {
		if _, ok := byNode[l.NodeID]; !ok {
			order = append(order, l.NodeID)
		}
		byNode[l.NodeID] = append(byNode[l.NodeID], l)
	}
	limit := s.cfg.ProbeConcurrency
	if limit <= 0 {
		limit = 32
	}
	sem := make(chan struct{}, limit)
	var wg sync.WaitGroup
	for _, nodeID := range order {
		if ctx.Err() != nil {
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(nodeID int64, ls []Lease) {
			defer func() { <-sem; wg.Done() }()
			nctx, cancel := context.WithTimeout(ctx, budget)
			defer cancel()
			fn(nctx, nodeID, ls)
		}(nodeID, byNode[nodeID])
	}
	wg.Wait()
}

func (s *Service) miningScanNode(ctx context.Context, nodeID int64, leases []Lease, rules *miningRules) {
	client, err := s.dialLeaseNode(ctx, nodeID)
	if err != nil {
		slog.Warn("挖矿扫描: 连接节点失败, 本轮跳过", "node_id", nodeID, "error", err)
		return
	}
	defer client.Close()
	for i := range leases {
		if ctx.Err() != nil {
			return
		}
		l := &leases[i]
		cmd, err := miningScanCommand(s.cfg.ProbeImage, l.LeaseNo)
		if err != nil {
			slog.Error("挖矿扫描: 无法生成扫描命令", "lease_no", l.LeaseNo, "error", err)
			continue
		}
		out, err := runRemote(ctx, client, cmd, cmdTimeout)
		if err != nil {
			slog.Warn("挖矿扫描: 执行失败, 本轮不判定", "lease_no", l.LeaseNo, "node_id", nodeID, "error", err)
			continue
		}
		sc, err := ParseMiningScan(out)
		if err != nil {
			slog.Warn("挖矿扫描: 输出无法解析, 本轮不判定", "lease_no", l.LeaseNo, "error", err)
			continue
		}
		if !sc.Running {
			continue // 容器未运行由对账处理
		}
		if hits := rules.Analyze(sc); len(hits) > 0 {
			s.handleMiningHits(l, hits)
		}
	}
}

func (s *Service) handleMiningHits(l *Lease, hits []MiningHit) {
	if hits[0].Freeze { // Analyze 保证冻结类在前
		if err := s.miningFreeze(l, hits); err != nil {
			slog.Error("运营告警: 挖矿特征命中但自动冻结失败, 需人工处理", "lease_no", l.LeaseNo, "error", err,
				"hits", miningHitsText(hits))
		}
		return
	}
	created, err := s.miningAlert(l, hits)
	if err != nil {
		slog.Error("写挖矿疑似告警失败", "lease_no", l.LeaseNo, "error", err)
		return
	}
	if created {
		slog.Warn("运营告警: 租约疑似挖矿(仅告警, 待人工核查)", "lease_no", l.LeaseNo, "hits", miningHitsText(hits))
	}
}

func miningHitsText(hits []MiningHit) string {
	parts := make([]string, 0, len(hits))
	for _, h := range hits {
		parts = append(parts, h.Kind+": "+h.Detail)
	}
	return strings.Join(parts, "; ")
}

// miningRuleDetail 风控告警 rule_detail(JSON): 租约号、订单号与全部命中。
func miningRuleDetail(l *Lease, orderNo string, hits []MiningHit) string {
	b, _ := json.Marshal(map[string]any{"lease_no": l.LeaseNo, "order_no": orderNo, "node_id": l.NodeID, "hits": hits})
	return string(b)
}

// miningFreeze stratum 参数 / 矿池地址外连: 自动冻结订单(同运营冻结路径), 同事务写风控告警与审计日志。
func (s *Service) miningFreeze(l *Lease, hits []MiningHit) error {
	o, err := s.products.ManagedOrderByID(l.OrderID)
	if err != nil {
		return err
	}
	if o == nil {
		return fmt.Errorf("租约 %s 的订单 #%d 不存在", l.LeaseNo, l.OrderID)
	}
	detail := miningRuleDetail(l, o.OrderNo, hits)
	tx, err := s.repo.db.BeginTxx(context.Background(), &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return err
	}
	defer tx.Rollback()
	before, err := s.products.FreezeOrderTx(tx, o.OrderNo)
	if err != nil {
		return err
	}
	if before == "frozen" {
		// 订单已冻结而租约仍在租: 冻结路径不会再动租约, 交人工核查(不应出现)。
		return fmt.Errorf("订单 %s 已是 frozen 但租约 %s 仍为 active", o.OrderNo, l.LeaseNo)
	}
	res, err := tx.Exec(`INSERT INTO risk_alerts (level, alert_type, target_type, target_id, rule_detail, status, resolution)
		VALUES ('high', ?, 'order', ?, ?, 'processing', ?)`, miningFreezeType, o.ID, detail, miningResAutoFrozen)
	if err != nil {
		return err
	}
	alertID, err := res.LastInsertId()
	if err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO audit_logs (operator_id, action, target_type, target_id, before_value, after_value, ip)
		VALUES (NULL, ?, 'order', ?, ?, ?, '')`, miningAuditAction, o.ID, before,
		truncate(fmt.Sprintf("frozen; alert=%d; lease=%s; %s", alertID, l.LeaseNo, miningHitsText(hits)), 2000)); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	s.products.AttestOrderFreeze(o.OrderNo)
	slog.Error("运营告警: 租约命中挖矿特征, 订单已自动冻结, 待运营核查(解冻或退款)", "lease_no", l.LeaseNo,
		"order_no", o.OrderNo, "alert_id", alertID, "hits", miningHitsText(hits))
	s.notifyLink(o.BuyerID, "订单已冻结",
		fmt.Sprintf("订单 %s 的算力环境检测到挖矿特征(%s), 依据平台规则已自动冻结: 现有会话已断开、暂停登录, 容器与数据保留待核查。如有异议请联系平台客服。",
			o.OrderNo, hits[0].Kind), "/console/buyer/orders/"+o.OrderNo)
	if n, err := s.repo.GetNode(l.NodeID); err == nil && n != nil {
		s.notify(n.SupplierID, "在租环境已冻结",
			fmt.Sprintf("节点 %s 上的租约 %s 检测到挖矿特征, 平台已自动冻结该订单(容器保留、GPU 仍占用), 待平台核查后处理。", n.NodeName, l.LeaseNo),
			n.ID)
	}
	return nil
}

// miningAlert 仅告警类命中: 同一订单有未结(pending/processing)的挖矿告警, 或 24h 内已写过, 不重复写。
func (s *Service) miningAlert(l *Lease, hits []MiningHit) (bool, error) {
	o, err := s.products.ManagedOrderByID(l.OrderID)
	if err != nil {
		return false, err
	}
	if o == nil {
		return false, fmt.Errorf("租约 %s 的订单 #%d 不存在", l.LeaseNo, l.OrderID)
	}
	return affected(s.repo.db.Exec(`INSERT INTO risk_alerts (level, alert_type, target_type, target_id, rule_detail, status)
		SELECT 'medium', ?, 'order', ?, ?, 'pending' FROM DUAL WHERE NOT EXISTS (
			SELECT 1 FROM risk_alerts WHERE target_type='order' AND target_id=? AND alert_type IN (?, ?)
			AND (status IN ('pending','processing') OR created_at > NOW() - INTERVAL ? SECOND))`,
		miningAlertType, o.ID, miningRuleDetail(l, o.OrderNo, hits), o.ID, miningAlertType, miningFreezeType,
		int(miningAlertDedup/time.Second)))
}
