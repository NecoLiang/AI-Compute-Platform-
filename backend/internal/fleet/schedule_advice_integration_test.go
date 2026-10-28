package fleet

import (
	"strings"
	"testing"

	"tokenfactory/internal/scheduler"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// docs/26 §26.5.5 (T-093): 托管商品的调度建议按 gpu_slots 计算容量与碎片, 不读节点 available_cards。
func TestScheduleAdviceManagedUsesSlots(t *testing.T) {
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	nodes := seedLeaseMarket(t, db, s, "container", 2, 8, 3)
	comp := wireLeases(t, s)
	// 上报值与卡槽事实故意不一致, 证明托管口径不读它。
	db.MustExec(`UPDATE supplier_nodes SET available_cards=1`)

	o1, err := placeManaged(comp, 21, 4)
	require.NoError(t, err)
	l1, err := s.repo.getLeaseByOrder(db, o1.ID)
	require.NoError(t, err)
	o2, err := placeManaged(comp, 22, 2)
	require.NoError(t, err)
	l2, err := s.repo.getLeaseByOrder(db, o2.ID)
	require.NoError(t, err)
	require.Equal(t, l1.NodeID, l2.NodeID, "best-fit: 第二单落在已用节点")
	other := nodes[0]
	if other == l1.NodeID {
		other = nodes[1]
	}

	sched := scheduler.NewService(scheduler.NewRepository(db))
	adv, err := sched.Advise(o2.OrderNo, 20)
	require.NoError(t, err)
	require.Len(t, adv.Nodes, 2)
	byID := map[int64]scheduler.NodeAdvice{}
	for _, n := range adv.Nodes {
		byID[n.NodeID] = n
		assert.Equal(t, "gpu_slots", n.CapacitySource)
		require.NotNil(t, n.MaxGroupFree)
	}
	// 已预留节点: 空闲 2 + 本单 2 = 4 可用; 另一台 8 可用。需求 2 → 贴合的已用节点得分更高并被推荐。
	used, idle := byID[l1.NodeID], byID[other]
	assert.Equal(t, 4, used.AvailableCards)
	assert.Equal(t, 8, used.TotalCards)
	assert.Equal(t, 8, idle.AvailableCards)
	assert.Equal(t, "recommended", used.Verdict)
	assert.Greater(t, used.Score, idle.Score)
	assert.Contains(t, strings.Join(used.Reasons, ";"), "该订单租约已在本节点预留 2 张卡")
	assert.Equal(t, 4, *idle.MaxGroupFree)

	// 需求 8 的订单: 已用节点不足, 另一台可用。
	o3, err := placeManaged(comp, 23, 8)
	require.NoError(t, err)
	adv, err = sched.Advise(o3.OrderNo, 0)
	require.NoError(t, err)
	for _, n := range adv.Nodes {
		if n.NodeID == l1.NodeID {
			assert.Equal(t, "unavailable", n.Verdict)
		} else {
			assert.Equal(t, "recommended", n.Verdict)
			assert.Equal(t, 8, n.AvailableCards, "本单 8 张已占计入可用")
		}
	}

	// 暂停调度的节点不可推荐。
	db.MustExec(`UPDATE supplier_nodes SET schedulable=0 WHERE id=?`, l1.NodeID)
	adv, err = sched.Advise(o2.OrderNo, 20)
	require.NoError(t, err)
	for _, n := range adv.Nodes {
		if n.NodeID == l1.NodeID {
			assert.Equal(t, "unavailable", n.Verdict)
			assert.Contains(t, strings.Join(n.Reasons, ";"), "暂停调度")
		}
	}
}
