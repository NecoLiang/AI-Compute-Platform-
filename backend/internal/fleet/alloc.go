package fleet

import (
	"fmt"
	"sort"

	"tokenfactory/pkg/errcode"

	"github.com/jmoiron/sqlx"
)

// 卡槽分配(docs/26 §26.5.2, 全天商品; T-092 的托管全天子集在此实现)。确定性, 无随机。

const maxAllocNodes = 3

// ErrNoWholeNode 候选节点都凑不出同机 k 张空闲卡。
var ErrNoWholeNode = errcode.New(errcode.Conflict, "所选卡数暂无整机余量")

type allocCandidate struct {
	NodeID     int64 `db:"node_id"`
	Free       int   `db:"free_cards"`
	TotalCards int   `db:"total_cards"`
	Stability  int   `db:"stability"`
}

// sortCandidates (空闲数−k) 升序(best-fit) → 稳定性(近 24h 心跳数)降序 → node_id 升序。
func sortCandidates(c []allocCandidate, k int) {
	sort.SliceStable(c, func(i, j int) bool {
		a, b := c[i], c[j]
		if a.Free-k != b.Free-k {
			return a.Free-k < b.Free-k
		}
		if a.Stability != b.Stability {
			return a.Stability > b.Stability
		}
		return a.NodeID < b.NodeID
	})
}

// pickSlots 在单节点的空闲卡中选 k 张:
//
//	a. 若某 topo_group 空闲数 ≥ k → 取空闲数最少且 ≥ k 的组(同数取组名小者), 组内按 gpu_index 升序取 k 张;
//	b. 否则按组空闲数降序(同数按组名)整组拼接, 最后一组按 gpu_index 取足。
//
// free 不足 k 返回 nil。
func pickSlots(free []Slot, k int) []Slot {
	if k <= 0 || len(free) < k {
		return nil
	}
	groups := map[string][]Slot{}
	var names []string
	for _, s := range free {
		if _, ok := groups[s.TopoGroup]; !ok {
			names = append(names, s.TopoGroup)
		}
		groups[s.TopoGroup] = append(groups[s.TopoGroup], s)
	}
	for _, g := range groups {
		sort.Slice(g, func(i, j int) bool { return g[i].GPUIndex < g[j].GPUIndex })
	}
	sort.Strings(names)
	best := ""
	for _, n := range names {
		if len(groups[n]) >= k && (best == "" || len(groups[n]) < len(groups[best])) {
			best = n
		}
	}
	if best != "" {
		return append([]Slot(nil), groups[best][:k]...)
	}
	sort.SliceStable(names, func(i, j int) bool { return len(groups[names[i]]) > len(groups[names[j]]) })
	out := make([]Slot, 0, k)
	for _, n := range names {
		for _, s := range groups[n] {
			if len(out) == k {
				return out
			}
			out = append(out, s)
		}
	}
	return out
}

// allocCandidates 商品下可分配 k 卡的节点。整机交付(bare_metal)要求整机全部空闲且 k=总卡数;
// 分卡容器要求节点支持磁盘配额, 不支持配额的节点只能整机出租(docs/25 §25.5.2)。
func (r *Repository) allocCandidates(q sqlx.Queryer, productID int64, k int, wholeMachine bool, excludeNode int64) ([]allocCandidate, error) {
	var list []allocCandidate
	err := sqlx.Select(q, &list, `SELECT n.id AS node_id, COUNT(s.id) AS free_cards, n.total_cards,
		(SELECT COUNT(*) FROM node_heartbeats h WHERE h.node_id=n.id AND h.created_at > NOW() - INTERVAL 24 HOUR) AS stability
		FROM supplier_nodes n JOIN gpu_slots s ON s.node_id=n.id AND s.state='free'
		WHERE n.product_id=? AND n.mgmt_mode='ssh' AND n.verify_status='verified' AND n.schedulable=1 AND n.dispatch_hold=0
		  AND n.status='online' AND n.id<>?
		  AND (? = 1 AND n.total_cards = ? OR ? = 0 AND (n.disk_quota_supported = 1 OR n.total_cards = ?))
		GROUP BY n.id, n.total_cards HAVING free_cards >= ?`,
		productID, excludeNode, boolInt(wholeMachine), k, boolInt(wholeMachine), k, k)
	if err != nil {
		return nil, err
	}
	if wholeMachine {
		filtered := list[:0]
		for _, c := range list {
			if c.Free == c.TotalCards {
				filtered = append(filtered, c)
			}
		}
		list = filtered
	}
	sortCandidates(list, k)
	return list, nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// reserveSlotsTx 在调用方事务内为租约预留 k 张同机卡: 依次尝试最多 3 个候选节点,
// 每个节点先锁节点行, 再 SELECT … FOR UPDATE 锁定空闲卡后守卫式 free→reserved, RowsAffected 必须 = k。
// 成功返回节点 ID 与卡槽; 全部失败返回 ErrNoWholeNode。不重算库存(调用方在同一事务内统一重算)。
func (r *Repository) reserveSlotsTx(tx *sqlx.Tx, productID int64, k int, wholeMachine bool, leaseID, excludeNode int64) (int64, []Slot, error) {
	cands, err := r.allocCandidates(tx, productID, k, wholeMachine, excludeNode)
	if err != nil {
		return 0, nil, err
	}
	for i, c := range cands {
		if i >= maxAllocNodes {
			break
		}
		// 锁顺序 节点行 → 卡槽 → 商品行(与供给方/运营/探活路径一致): 卡槽锁定读走 idx_node_state 二级索引,
		// 单卡路径走主键, 不先串行到节点行上会出现二级索引/主键交叉死锁。持锁后复核节点仍可分配。
		var eligible int
		if err := tx.Get(&eligible, `SELECT COUNT(*) FROM supplier_nodes WHERE id=? AND product_id=? AND mgmt_mode='ssh'
			AND verify_status='verified' AND schedulable=1 AND dispatch_hold=0 AND status='online' FOR UPDATE`, c.NodeID, productID); err != nil {
			return 0, nil, err
		}
		if eligible == 0 {
			continue
		}
		var free []Slot
		if err := tx.Select(&free, `SELECT id, node_id, gpu_index, gpu_uuid, pci_bus_id, vram_mb, topo_group, state, lease_id,
			state_changed_at FROM gpu_slots WHERE node_id=? AND state='free' ORDER BY gpu_index FOR UPDATE`, c.NodeID); err != nil {
			return 0, nil, err
		}
		if wholeMachine && len(free) != c.TotalCards {
			continue
		}
		picked := pickSlots(free, k)
		if picked == nil {
			continue
		}
		ids := make([]int64, len(picked))
		for j, s := range picked {
			ids[j] = s.ID
		}
		q, args, err := sqlx.In(`UPDATE gpu_slots SET state='reserved', lease_id=?, state_changed_at=NOW()
			WHERE id IN (?) AND state='free'`, leaseID, ids)
		if err != nil {
			return 0, nil, err
		}
		res, err := tx.Exec(q, args...)
		if err != nil {
			return 0, nil, err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return 0, nil, err
		}
		if int(n) != k {
			// 锁定读之后仍不足 k 行说明数据被并发改动; 本节点已部分预留的行必须退回再换下一个节点。
			if _, err := tx.Exec(`UPDATE gpu_slots SET state='free', lease_id=NULL, state_changed_at=NOW()
				WHERE lease_id=? AND state='reserved' AND node_id=?`, leaseID, c.NodeID); err != nil {
				return 0, nil, err
			}
			continue
		}
		for j := range picked {
			picked[j].State, picked[j].LeaseID = SlotReserved, &leaseID
		}
		return c.NodeID, picked, nil
	}
	return 0, nil, ErrNoWholeNode
}

// cpuMemSplit 容器 CPU/内存配额: 先扣主机保留, 剩余按 k/总卡数等比切分, 向下取整(docs/25 §25.5.2)。
// 整机(k=总卡数)同样扣保留。切分后不足 1 核/1GB 返回错误(节点资源不足以开通)。
func cpuMemSplit(cpuCores, memGB, reserveCPU, reserveMemGB, k, total int) (int, int, error) {
	if total <= 0 || k <= 0 || k > total {
		return 0, 0, fmt.Errorf("卡数非法: %d/%d", k, total)
	}
	cpu := (cpuCores - reserveCPU) * k / total
	mem := (memGB - reserveMemGB) * k / total
	if cpu < 1 || mem < 1 {
		return 0, 0, fmt.Errorf("节点资源扣除主机保留(%d 核/%dGB)后不足以切分 %d/%d 卡: CPU %d 核, 内存 %dGB",
			reserveCPU, reserveMemGB, k, total, cpuCores, memGB)
	}
	return cpu, mem, nil
}

// shmSizeGB --shm-size = min(M/2, 64)G, 至少 1G。
func shmSizeGB(memGB int) int {
	s := memGB / 2
	if s > 64 {
		s = 64
	}
	if s < 1 {
		s = 1
	}
	return s
}

// pickLeasePort 租约端口从端口段**开头**向上分配(P11 自检从末尾向下选, 二者相向), 排除已被租约占用
// 与当前节点上正在监听的端口。无可用端口返回 0。
func pickLeasePort(start, end int, leased, listening map[int]bool) int {
	for p := start; p <= end; p++ {
		if !leased[p] && !listening[p] {
			return p
		}
	}
	return 0
}
