package fleet

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"tokenfactory/internal/compute"
	"tokenfactory/pkg/errcode"

	"github.com/jmoiron/sqlx"
)

// 包夜预订(docs/26 §26.6.3, T-094): night_bookings(slot_id, night_date) 唯一键保证同一张卡同一晚只被订一次;
// 同一订单所有晚使用同一组卡(容器跨晚复用、数据不丢)。下单只写预订, 不动卡槽状态: 卡槽只在每晚开通时
// free→reserved→leased, 窗口结束 park 时 →draining→free(nightly.go)。夜间窗口商品的节点只服务包夜租约。

// ErrNightNoCards 所选夜晚凑不出同机 k 张未被预订的卡。
var ErrNightNoCards = errcode.New(errcode.Conflict, "所选夜晚暂无同机空闲卡, 请调整日期或卡数")

const dateLayout = "2006-01-02"

// nightCandidates 在 [from, to] 每晚都未被预订、且非 faulty/withheld 的卡数 ≥ k 的节点(按 §26.5.2 排序)。
// 分卡容器要求节点支持磁盘配额, 不支持配额的节点只能整机(k = 总卡数)。
func (r *Repository) nightCandidates(q sqlx.Queryer, productID int64, k int, from, to time.Time, excludeNode int64) ([]allocCandidate, error) {
	var list []allocCandidate
	err := sqlx.Select(q, &list, `SELECT n.id AS node_id, COUNT(s.id) AS free_cards, n.total_cards,
		(SELECT COUNT(*) FROM node_heartbeats h WHERE h.node_id=n.id AND h.created_at > NOW() - INTERVAL 24 HOUR) AS stability
		FROM supplier_nodes n JOIN gpu_slots s ON s.node_id=n.id AND s.state NOT IN ('faulty','withheld')
		  AND NOT EXISTS (SELECT 1 FROM night_bookings b WHERE b.slot_id=s.id AND b.night_date BETWEEN ? AND ?)
		WHERE n.product_id=? AND n.mgmt_mode='ssh' AND n.verify_status='verified' AND n.schedulable=1 AND n.dispatch_hold=0
		  AND n.status='online' AND n.id<>? AND (n.disk_quota_supported = 1 OR n.total_cards = ?)
		GROUP BY n.id, n.total_cards HAVING free_cards >= ?`,
		from.Format(dateLayout), to.Format(dateLayout), productID, excludeNode, k, k)
	if err != nil {
		return nil, err
	}
	sortCandidates(list, k)
	return list, nil
}

// nightDates 起始夜晚起连续 n 晚。
func nightDates(start time.Time, n int) []time.Time {
	out := make([]time.Time, n)
	for i := range out {
		out[i] = compute.NightDate(start).AddDate(0, 0, i)
	}
	return out
}

// reserveNightsTx 在调用方事务内为租约预订 k 张同机卡的连续若干晚: 依次尝试最多 3 个候选节点,
// 每个节点先锁节点行(复核可分配), 再锁该节点全部卡槽行(串行化同节点并发下单), 排除区间内已有预订的卡后
// 按拓扑选卡, 批量 INSERT k×nights 行预订; 唯一键冲突(并发写入)换下一节点。全部失败返回 ErrNightNoCards。
func (r *Repository) reserveNightsTx(tx *sqlx.Tx, productID int64, k int, leaseID int64, dates []time.Time) (int64, []Slot, error) {
	if k <= 0 || len(dates) == 0 {
		return 0, nil, fmt.Errorf("包夜预订参数非法: %d 卡 %d 晚", k, len(dates))
	}
	from, to := dates[0], dates[len(dates)-1]
	cands, err := r.nightCandidates(tx, productID, k, from, to, 0)
	if err != nil {
		return 0, nil, err
	}
	for i, c := range cands {
		if i >= maxAllocNodes {
			break
		}
		picked, err := r.lockNightSlots(tx, productID, c.NodeID, k, from, to)
		if err != nil {
			return 0, nil, err
		}
		if picked == nil {
			continue
		}
		ok, err := r.insertNightBookings(tx, leaseID, picked, dates)
		if err != nil {
			return 0, nil, err
		}
		if !ok {
			continue
		}
		return c.NodeID, picked, nil
	}
	return 0, nil, ErrNightNoCards
}

// lockNightSlots 锁节点行与其卡槽, 返回在 [from,to] 内每晚都未被预订的 k 张卡(拓扑优先); 节点不再可分配或不足返回 nil。
func (r *Repository) lockNightSlots(tx *sqlx.Tx, productID, nodeID int64, k int, from, to time.Time) ([]Slot, error) {
	var eligible int
	if err := tx.Get(&eligible, `SELECT COUNT(*) FROM supplier_nodes WHERE id=? AND product_id=? AND mgmt_mode='ssh'
		AND verify_status='verified' AND schedulable=1 AND dispatch_hold=0 AND status='online'
		AND (disk_quota_supported = 1 OR total_cards = ?) FOR UPDATE`, nodeID, productID, k); err != nil {
		return nil, err
	}
	if eligible == 0 {
		return nil, nil
	}
	var slots []Slot
	if err := tx.Select(&slots, `SELECT id, node_id, gpu_index, gpu_uuid, pci_bus_id, vram_mb, topo_group, state, lease_id,
		state_changed_at FROM gpu_slots WHERE node_id=? ORDER BY gpu_index FOR UPDATE`, nodeID); err != nil {
		return nil, err
	}
	// 持卡槽行锁后再读预订: 所有写预订的路径都先锁同一批卡槽行, READ COMMITTED 下读到的是已提交的最新预订。
	var booked []int64
	if err := tx.Select(&booked, `SELECT DISTINCT b.slot_id FROM night_bookings b JOIN gpu_slots s ON s.id=b.slot_id
		WHERE s.node_id=? AND b.night_date BETWEEN ? AND ?`, nodeID, from.Format(dateLayout), to.Format(dateLayout)); err != nil {
		return nil, err
	}
	taken := map[int64]bool{}
	for _, id := range booked {
		taken[id] = true
	}
	free := make([]Slot, 0, len(slots))
	for _, s := range slots {
		if s.State != SlotFaulty && s.State != SlotWithheld && !taken[s.ID] {
			free = append(free, s)
		}
	}
	return pickSlots(free, k), nil
}

// insertNightBookings 批量写 k×nights 行预订。唯一键冲突返回 false(语句级回滚, 事务内其他写入不受影响)。
func (r *Repository) insertNightBookings(tx *sqlx.Tx, leaseID int64, slots []Slot, dates []time.Time) (bool, error) {
	rows := make([]map[string]any, 0, len(slots)*len(dates))
	for _, d := range dates {
		for _, s := range slots {
			rows = append(rows, map[string]any{"slot_id": s.ID, "night_date": d.Format(dateLayout), "lease_id": leaseID})
		}
	}
	_, err := tx.NamedExec(`INSERT INTO night_bookings (slot_id, night_date, lease_id) VALUES (:slot_id, :night_date, :lease_id)`, rows)
	if isDuplicate(err) {
		return false, nil
	}
	return err == nil, err
}

// insertLeaseNights 每晚一行开停机记录(state=booked)。
func (r *Repository) insertLeaseNights(tx *sqlx.Tx, leaseID int64, dates []time.Time) error {
	rows := make([]map[string]any, 0, len(dates))
	for _, d := range dates {
		rows = append(rows, map[string]any{"lease_id": leaseID, "night_date": d.Format(dateLayout)})
	}
	_, err := tx.NamedExec(`INSERT INTO lease_nights (lease_id, night_date) VALUES (:lease_id, :night_date)`, rows)
	return err
}

// reserveNightLeaseTx 包夜下单: 租约 reserved + k 卡×n 晚预订 + 每晚记录 + 派生库存重算。
func (s *Service) reserveNightLeaseTx(tx *sqlx.Tx, o *compute.Order, p *compute.Product, l *Lease) error {
	if p.DeliveryMode != "container" {
		return errcode.New(errcode.ParamInvalid, "包夜仅适用于分卡容器商品")
	}
	if o.NightStartDate == nil || compute.OrderNightWindow(o) == nil {
		return fmt.Errorf("包夜订单 %s 缺少起始夜晚或夜间窗口快照", o.OrderNo)
	}
	dates := nightDates(*o.NightStartDate, o.Duration)
	nodeID, _, err := s.repo.reserveNightsTx(tx, p.ID, o.Quantity, l.ID, dates)
	if err != nil {
		return err
	}
	if err := s.repo.insertLeaseNights(tx, l.ID, dates); err != nil {
		return err
	}
	if _, err := tx.Exec(`UPDATE leases SET node_id=? WHERE id=? AND state='reserved'`, nodeID, l.ID); err != nil {
		return err
	}
	return s.recomputeStockTx(tx, p.ID)
}

// nightPlan 租约所属订单的包夜快照(非包夜订单为 nil)。
type nightPlan struct {
	Window compute.NightWindow
	Start  time.Time // 首晚 night_date
	Nights int
}

// leaseNightPlan 读租约订单的计费方式与夜间窗口快照。包夜订单缺窗口快照报错(不猜默认窗口)。
func (r *Repository) leaseNightPlan(q sqlx.Queryer, leaseID int64) (*nightPlan, error) {
	var row struct {
		Mode     sql.NullString `db:"pricing_mode"`
		Start    sql.NullTime   `db:"night_start_date"`
		StartMin sql.NullInt64  `db:"night_start_min"`
		EndMin   sql.NullInt64  `db:"night_end_min"`
		Duration int            `db:"duration"`
	}
	err := sqlx.Get(q, &row, `SELECT o.pricing_mode, o.night_start_date, o.night_start_min, o.night_end_min, o.duration
		FROM leases l JOIN orders o ON o.id=l.order_id WHERE l.id=?`, leaseID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil || row.Mode.String != compute.PricingNightly {
		return nil, err
	}
	if !row.Start.Valid || !row.StartMin.Valid || !row.EndMin.Valid {
		return nil, fmt.Errorf("包夜租约 %d 的订单缺少起始夜晚或夜间窗口快照", leaseID)
	}
	return &nightPlan{Window: compute.NightWindow{StartMin: int(row.StartMin.Int64), EndMin: int(row.EndMin.Int64)},
		Start: compute.NightDate(row.Start.Time), Nights: row.Duration}, nil
}

// productNightWindow 夜间窗口商品的窗口(全天商品返回 nil)。
func (r *Repository) productNightWindow(q sqlx.Queryer, productID int64) (*compute.NightWindow, error) {
	var row struct {
		Availability string        `db:"availability"`
		StartMin     sql.NullInt64 `db:"night_start_min"`
		EndMin       sql.NullInt64 `db:"night_end_min"`
	}
	err := sqlx.Get(q, &row, `SELECT availability, night_start_min, night_end_min FROM products WHERE id=?`, productID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil || row.Availability != compute.AvailabilityNightWindow || !row.StartMin.Valid || !row.EndMin.Valid {
		return nil, err
	}
	return &compute.NightWindow{StartMin: int(row.StartMin.Int64), EndMin: int(row.EndMin.Int64)}, nil
}

// nightNode 夜间矩阵的一个节点: 可用卡(非 faulty/withheld)与各晚已预订数。
type nightNode struct {
	NodeID int64 `db:"node_id"`
	Usable int   `db:"usable"`
	Total  int   `db:"total_cards"`
	Quota  bool  `db:"quota"`
	booked map[string]int
}

// nightMatrix 商品下可分配节点在 [from, from+days) 内每晚的已预订卡数(不加锁快照)。
func (r *Repository) nightMatrix(q sqlx.Queryer, productID int64, from time.Time, days int) ([]*nightNode, error) {
	var nodes []*nightNode
	if err := sqlx.Select(q, &nodes, `SELECT n.id AS node_id, COUNT(s.id) AS usable, n.total_cards,
		COALESCE(n.disk_quota_supported,0)=1 AS quota
		FROM supplier_nodes n JOIN gpu_slots s ON s.node_id=n.id AND s.state NOT IN ('faulty','withheld')
		WHERE n.product_id=? AND n.mgmt_mode='ssh' AND n.verify_status='verified' AND n.schedulable=1 AND n.dispatch_hold=0
		  AND n.status='online'
		GROUP BY n.id, n.total_cards, n.disk_quota_supported`, productID); err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, nil
	}
	var rows []struct {
		NodeID int64     `db:"node_id"`
		Night  time.Time `db:"night_date"`
		Booked int       `db:"booked"`
	}
	if err := sqlx.Select(q, &rows, `SELECT s.node_id, b.night_date, COUNT(*) AS booked
		FROM night_bookings b JOIN gpu_slots s ON s.id=b.slot_id JOIN supplier_nodes n ON n.id=s.node_id
		WHERE n.product_id=? AND s.state NOT IN ('faulty','withheld') AND b.night_date BETWEEN ? AND ?
		GROUP BY s.node_id, b.night_date`, productID, from.Format(dateLayout), from.AddDate(0, 0, days-1).Format(dateLayout)); err != nil {
		return nil, err
	}
	byID := map[int64]*nightNode{}
	for _, n := range nodes {
		n.booked = map[string]int{}
		byID[n.NodeID] = n
	}
	for _, x := range rows {
		if n := byID[x.NodeID]; n != nil {
			n.booked[compute.NightDate(x.Night).Format(dateLayout)] = x.Booked
		}
	}
	return nodes, nil
}

// free 某晚节点未被预订的可用卡数。
func (n *nightNode) free(d time.Time) int {
	f := n.Usable - n.booked[d.Format(dateLayout)]
	if f < 0 {
		return 0
	}
	return f
}

// canHost 节点某晚能否一次容纳 k 卡(与 nightCandidates 同口径)。
func (n *nightNode) canHost(free, k int) bool {
	if n.Quota {
		return free >= k
	}
	return n.Total == k && free == n.Total
}

// nightStats 某晚: 全部节点未预订卡合计、单节点最多可订卡数(k>0 时只统计能容纳 k 卡的节点)。
func nightStats(nodes []*nightNode, d time.Time, k int) (sum, maxNode int) {
	for _, n := range nodes {
		f := n.free(d)
		sum += f
		if k > 0 && !n.canHost(f, k) {
			continue
		}
		if m := rentableOnNode(f, n.Total, n.Quota, false); m > maxNode {
			maxNode = m
		}
	}
	return sum, maxNode
}

// nightStock 夜间窗口商品派生库存: 可订的第一晚起 NightCalendarMaxDays(31)晚中, 未预订卡合计最多的一晚及单节点最多可订卡数的最大值。
func (r *Repository) nightStock(q sqlx.Queryer, productID int64, w compute.NightWindow, now time.Time) (free, maxNode int, err error) {
	first := compute.FirstBookableNight(now, w)
	nodes, err := r.nightMatrix(q, productID, first, compute.NightCalendarMaxDays)
	if err != nil {
		return 0, 0, err
	}
	for i := 0; i < compute.NightCalendarMaxDays; i++ {
		sum, m := nightStats(nodes, first.AddDate(0, 0, i), 0)
		free, maxNode = max(free, sum), max(maxNode, m)
	}
	return free, maxNode, nil
}

// nightFeasible LeaseHooks.NightFeasible: 只读判断能否在同一节点为 k 卡预订自 start 起连续 nights 晚。
func (s *Service) nightFeasible(p *compute.Product, cards int, start time.Time, nights int) (bool, error) {
	if p == nil || cards <= 0 || nights <= 0 || p.DeliveryMode != "container" {
		return false, nil
	}
	dates := nightDates(start, nights)
	cands, err := s.repo.nightCandidates(s.repo.db, p.ID, cards, dates[0], dates[len(dates)-1], 0)
	if err != nil {
		return false, err
	}
	return len(cands) > 0, nil
}

// nightCalendar LeaseHooks.NightCalendar: 自 from 起 days 晚, 每晚单节点最多可订卡数(cards>0 时只统计能容纳的节点)。
func (s *Service) nightCalendar(p *compute.Product, cards int, from time.Time, days int) ([]int, error) {
	out := make([]int, days)
	if p == nil || days <= 0 {
		return out, nil
	}
	nodes, err := s.repo.nightMatrix(s.repo.db, p.ID, compute.NightDate(from), days)
	if err != nil {
		return nil, err
	}
	for i := range out {
		_, out[i] = nightStats(nodes, compute.NightDate(from).AddDate(0, 0, i), cards)
	}
	return out, nil
}

// releaseNightsTx 释放预订(§26.6.3): all=true(关单/取消/开通失败) 删除全部; 否则只删尚未开始的晚,
// 已开始的晚保留作履约记录。lease_nights 中对应的 booked 行一并删除。
func (s *Service) releaseNightsTx(tx *sqlx.Tx, leaseID int64, all bool) error {
	cond, args := "", []any{leaseID}
	if !all {
		plan, err := s.repo.leaseNightPlan(tx, leaseID)
		if err != nil || plan == nil {
			return err
		}
		c := compute.LastStartedNight(time.Now(), plan.Window)
		cond, args = " AND night_date > ?", append(args, c.Format(dateLayout))
	}
	if _, err := tx.Exec(`DELETE FROM night_bookings WHERE lease_id=?`+cond, args...); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM lease_nights WHERE lease_id=? AND state='booked'`+cond, args...)
	return err
}
