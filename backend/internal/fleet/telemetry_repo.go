package fleet

// 探活遥测落库(migration 031, docs/27 §27.3 一键部署可行性 F3–F6 的数据源)。
//
// 采集分工(docs/25 §25.4.4 "探活遥测"):
//   - 轻探活(60s): 逐卡显存已用/总量、温度、功耗、降频原因、ECC → gpu_slots 遥测列;
//     内核日志 Xid(自上次成功扫描起, 至少 2 分钟) → gpu_xid_events + 节点覆盖区间。
//   - 完整探活(12h): 两两拓扑(nvidia-smi topo -m) → gpu_slots.topo_links; 模型缓存目录容量 → supplier_nodes.cache_*;
//     P7 的 24h Xid 与 P13 后 10min 复扫同样落 gpu_xid_events 并并入覆盖区间。
//
// 全部写入都以节点代次(generation)为守卫: 停用/恢复/改连接信息/确认指纹之后, 旧连接采到的数据不再写回。
// 时间统一用平台(应用)时钟写入, 读取方同样用应用时钟判新鲜度。

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"slices"
	"sort"
	"time"

	"github.com/jmoiron/sqlx"
)

const (
	// xidEventRetention gpu_xid_events 明细保留期; F6 只看近 1 小时, 留 30 天供运营排障。
	xidEventRetention = 30 * 24 * time.Hour
	// xidFirstWindow 节点没有成功扫描记录时的首扫窗口: 多扫 1 分钟, 使一次成功扫描即可覆盖完整 1 小时。
	xidFirstWindow = 61 * time.Minute
	// xidMaxWindow 自上次成功扫描过久(如长时间离线)时, 单次最多回看 24 小时。
	xidMaxWindow = 24 * time.Hour
)

// dbSecond 写入 DATETIME(0) 列前截到整秒: MySQL 对小数秒默认四舍五入, 会把时间写到"未来"半秒,
// 使紧接着的下一次扫描被误判为"结束时刻早于已记录"。
func dbSecond(t time.Time) time.Time { return t.Truncate(time.Second) }

// lockNodeGeneration 锁节点行并返回代次; 节点已删除返回 ok=false。
func lockNodeGeneration(tx *sqlx.Tx, nodeID int64) (gen int, ok bool, err error) {
	err = tx.Get(&gen, `SELECT generation FROM supplier_nodes WHERE id=? AND mgmt_mode='ssh' FOR UPDATE`, nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, false, nil
	}
	return gen, err == nil, err
}

// recordTelemetryTx 轻探活逐卡遥测写入 gpu_slots(按 node_id+gpu_uuid 定位, 未登记的卡忽略)。
// 调用方已在事务内锁住节点行。
func (r *Repository) recordTelemetryTx(tx *sqlx.Tx, nodeID int64, gen int, tel []GPUTelemetry, at time.Time) error {
	if len(tel) == 0 {
		return nil
	}
	cur, ok, err := lockNodeGeneration(tx, nodeID)
	if err != nil || !ok || cur != gen {
		return err
	}
	for _, g := range tel {
		if _, err := tx.Exec(`UPDATE gpu_slots SET vram_used_mb=?, vram_total_mb=?, temp_c=?, power_w=?, throttle_reasons=?,
			ecc_uncorrected=?, ecc_corrected=?, telemetry_at=? WHERE node_id=? AND gpu_uuid=?`,
			g.VRAMUsedMiB, g.VRAMTotalMiB, g.TempC, g.PowerW, g.ThrottleReasons, g.ECCUncorrected, g.ECCCorrected, dbSecond(at),
			nodeID, g.UUID); err != nil {
			return err
		}
	}
	return nil
}

// lastXidScanAt 节点最近一次成功 Xid 扫描时间(用于计算轻探活的扫描窗口)。
func (r *Repository) lastXidScanAt(nodeID int64) (*time.Time, error) {
	var t *time.Time
	err := r.db.Get(&t, `SELECT xid_scan_ok_at FROM supplier_nodes WHERE id=?`, nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return t, err
}

// xidWindowSince 本次轻探活的 Xid 扫描窗口: 自上次成功扫描起再多 1 分钟重叠(事件按原始行去重), 至少 2 分钟;
// 无记录时 61 分钟; 最多 24 小时。
func xidWindowSince(last *time.Time, now time.Time) time.Duration {
	if last == nil {
		return xidFirstWindow
	}
	w := now.Sub(*last) + time.Minute
	if w < 2*time.Minute {
		w = 2 * time.Minute
	}
	if w > xidMaxWindow {
		w = xidMaxWindow
	}
	return w
}

// recordXidScanTx 写入一次 Xid 扫描: 事件明细去重入库, 并维护节点的连续覆盖区间 [xid_covered_from, xid_scan_ok_at]。
//
//   - 扫描不可用(无权限/命令报错): 覆盖中断, xid_covered_from 置 NULL 并记原因; xid_scan_ok_at 不动。
//   - 扫描可用: 本次覆盖 [CoverFrom, now](节点时钟按 now-NodeNow 的差换算到平台时钟)。与上次覆盖相接
//     (本次起点不晚于上次 xid_scan_ok_at)则延续原起点, 否则从本次起点重新累计。
//   - 并发的完整探活与轻探活由节点行锁串行; 结束时刻早于已记录 xid_scan_ok_at 的扫描只补事件, 不回退覆盖区间。
func (r *Repository) recordXidScanTx(tx *sqlx.Tx, nodeID int64, gen int, scan XidScan, now time.Time) error {
	now = dbSecond(now)
	var cur struct {
		Gen  int        `db:"generation"`
		OK   *time.Time `db:"xid_scan_ok_at"`
		From *time.Time `db:"xid_covered_from"`
	}
	err := tx.Get(&cur, `SELECT generation, xid_scan_ok_at, xid_covered_from FROM supplier_nodes
		WHERE id=? AND mgmt_mode='ssh' FOR UPDATE`, nodeID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil || cur.Gen != gen {
		return err
	}
	if !scan.Known || scan.NodeNow.IsZero() || scan.CoverFrom.IsZero() {
		note := scan.Note
		if note == "" {
			note = "内核日志扫描输出不完整"
		}
		_, err := tx.Exec(`UPDATE supplier_nodes SET xid_covered_from=NULL, xid_scan_note=? WHERE id=? AND generation=?`,
			truncate(note, 250), nodeID, gen)
		return err
	}
	skew := now.Sub(scan.NodeNow)
	for _, e := range scan.List {
		sum := sha256.Sum256([]byte(e.Line))
		if _, err := tx.Exec(`INSERT INTO gpu_xid_events (node_id, pci_bus_id, xid, occurred_at, seen_at, source, message, event_key)
			VALUES (?,?,?,?,?,?,?,?) ON DUPLICATE KEY UPDATE id=id`,
			nodeID, e.Bus, e.Xid, dbSecond(e.At.Add(skew)), now, scan.Source, truncate(e.Line, 500), hex.EncodeToString(sum[:])); err != nil {
			return err
		}
	}
	if cur.OK != nil && now.Before(*cur.OK) {
		return nil
	}
	from := dbSecond(scan.CoverFrom.Add(skew))
	if from.After(now) {
		from = now
	}
	if cur.OK != nil && cur.From != nil && !from.After(*cur.OK) && cur.From.Before(from) {
		from = *cur.From
	}
	_, err = tx.Exec(`UPDATE supplier_nodes SET xid_scan_ok_at=?, xid_covered_from=?, xid_scan_note=NULL WHERE id=? AND generation=?`,
		now, from, nodeID, gen)
	return err
}

// recordXidScan 完整探活内的 Xid 扫描单独成事务落库(轻探活在自己的事务里调用 recordXidScanTx)。
func (s *Service) recordXidScan(nodeID int64, gen int, scan XidScan, now time.Time) error {
	return s.inTx(func(tx *sqlx.Tx) error { return s.repo.recordXidScanTx(tx, nodeID, gen, scan, now) })
}

// recordTopo 完整探活的两两拓扑写入各卡槽(按 gpu_index+gpu_uuid 定位, 换卡后旧行不会被写)。
func (s *Service) recordTopo(nodeID int64, gen int, gpus []GPU, links [][]string, now time.Time) error {
	if len(links) != len(gpus) {
		return nil
	}
	return s.inTx(func(tx *sqlx.Tx) error {
		cur, ok, err := lockNodeGeneration(tx, nodeID)
		if err != nil || !ok || cur != gen {
			return err
		}
		for _, g := range gpus {
			if g.Index < 0 || g.Index >= len(links) {
				continue
			}
			b, err := json.Marshal(links[g.Index])
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE gpu_slots SET topo_links=?, topo_at=? WHERE node_id=? AND gpu_index=? AND gpu_uuid=?`,
				string(b), dbSecond(now), nodeID, g.Index, g.UUID); err != nil {
				return err
			}
		}
		return nil
	})
}

// 模型缓存目录容量读数来源(supplier_nodes.cache_source)。
const (
	cacheSourceLight = "light"
	cacheSourceFull  = "full"
)

// recordCacheDFTx 写模型缓存目录容量(调用方持有节点行锁; 代次守卫)。
//   - 读到容量: 写容量、采集时间与来源, 清空原因;
//   - 目录不存在: 这是确定的读数, 容量与采集时间置空(读取方即为未知)并记原因;
//   - 读取失败(failNote 非空): 不动上一次读数, 只记原因 —— 上一次读数是否还能用由读取方按采集时间与来源判断。
func (r *Repository) recordCacheDFTx(tx *sqlx.Tx, nodeID int64, gen int, source string, df CacheDF, failNote string, now time.Time) error {
	var err error
	switch {
	case failNote != "":
		_, err = tx.Exec(`UPDATE supplier_nodes SET cache_note=? WHERE id=? AND generation=?`, truncate(failNote, 250), nodeID, gen)
	case df.Missing:
		_, err = tx.Exec(`UPDATE supplier_nodes SET cache_free_bytes=NULL, cache_total_bytes=NULL, cache_observed_at=NULL,
			cache_note='模型缓存目录不存在', cache_source=? WHERE id=? AND generation=?`, source, nodeID, gen)
	default:
		_, err = tx.Exec(`UPDATE supplier_nodes SET cache_free_bytes=?, cache_total_bytes=?, cache_observed_at=?, cache_note=NULL,
			cache_source=? WHERE id=? AND generation=?`, df.AvailBytes, df.TotalBytes, dbSecond(now), source, nodeID, gen)
	}
	return err
}

// recordCacheDF 自带事务的 recordCacheDFTx(完整探活用)。
func (s *Service) recordCacheDF(nodeID int64, gen int, source string, df CacheDF, failNote string, now time.Time) error {
	return s.inTx(func(tx *sqlx.Tx) error {
		cur, ok, err := lockNodeGeneration(tx, nodeID)
		if err != nil || !ok || cur != gen {
			return err
		}
		return s.repo.recordCacheDFTx(tx, nodeID, gen, source, df, failNote, now)
	})
}

// readCacheDF 在节点上读模型缓存目录容量; 返回非空 note 表示读取失败。
func (p *probeRun) readCacheDF(ctx context.Context, d time.Duration) (CacheDF, string) {
	cmd, err := cacheDFCommand(p.s.cfg.ModelCacheDir)
	if err != nil {
		return CacheDF{}, err.Error()
	}
	out, err := p.runFor(ctx, cmd, d)
	if err != nil {
		return CacheDF{}, "读取模型缓存目录容量失败: " + err.Error()
	}
	df, err := ParseCacheDF(out)
	if err != nil {
		return CacheDF{}, err.Error()
	}
	return df, ""
}

// PurgeOldXidEvents 删除超过保留期的 Xid 明细(周期任务调用, 分批删除)。
func (r *Repository) PurgeOldXidEvents(before time.Time) (int64, error) {
	var total int64
	for {
		res, err := r.db.Exec(`DELETE FROM gpu_xid_events WHERE seen_at < ? LIMIT 5000`, before)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += n
		if n < 5000 {
			return total, nil
		}
	}
}

// fatalXidHold 致命 Xid 告警(空闲卡 faulty / 在租卡 resource_alert)自最后一次致命 Xid 起保持的时长,
// 与一键部署 F6"近 1 小时无致命 Xid"同口径。不可纠正 ECC 不在此列: 以 volatile 计数为准, 归零即恢复。
const fatalXidHold = time.Hour

// recentFatalXids 节点 since(平台时钟)之后入库的致命 Xid: 归一化 PCI 地址 → 去重升序 xid。
func (r *Repository) recentFatalXids(nodeID int64, since time.Time) (map[string][]int, error) {
	var rows []struct {
		Bus string `db:"pci_bus_id"`
		Xid int    `db:"xid"`
	}
	if err := r.db.Select(&rows, `SELECT DISTINCT pci_bus_id, xid FROM gpu_xid_events WHERE node_id=? AND occurred_at >= ?`,
		nodeID, dbSecond(since)); err != nil {
		return nil, err
	}
	out := map[string][]int{}
	for _, e := range rows {
		if FatalXids[e.Xid] {
			out[NormalizeBusID(e.Bus)] = append(out[NormalizeBusID(e.Bus)], e.Xid)
		}
	}
	mergeFatalXids(out, nil)
	return out, nil
}

// fatalSince 本次扫描中发生在 since(平台时钟)之后的致命 Xid; 节点时钟按 scanAt-NodeNow 换算(同 recordXidScanTx)。
func (x XidScan) fatalSince(scanAt, since time.Time) map[string][]int {
	out := map[string][]int{}
	if !x.Known {
		return out
	}
	var skew time.Duration
	if !x.NodeNow.IsZero() {
		skew = scanAt.Sub(x.NodeNow)
	}
	for _, e := range x.List {
		if FatalXids[e.Xid] && !dbSecond(e.At.Add(skew)).Before(dbSecond(since)) {
			out[e.Bus] = append(out[e.Bus], e.Xid)
		}
	}
	return out
}

// mergeFatalXids 把 add 并入 dst, 每张卡的 xid 去重升序。
func mergeFatalXids(dst, add map[string][]int) {
	for bus, xs := range add {
		dst[bus] = append(dst[bus], xs...)
	}
	for bus, xs := range dst {
		sort.Ints(xs)
		dst[bus] = slices.Compact(xs)
	}
}
