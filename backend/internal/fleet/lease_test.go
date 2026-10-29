package fleet

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var allLeaseStates = []string{LeaseReserved, LeasePending, LeaseProvisioning, LeaseActive, LeaseParked, LeaseSuspended,
	LeaseGrace, LeasePurging, LeasePurged, LeaseFailed, LeaseCancelled}

func TestLeaseTransitionTable(t *testing.T) {
	legal := map[[2]string]bool{}
	for _, p := range [][2]string{
		{LeaseReserved, LeasePending}, {LeaseGrace, LeasePending}, {LeaseProvisioning, LeasePending}, {LeasePurged, LeasePending}, {LeasePending, LeaseGrace}, {LeaseProvisioning, LeaseGrace},
		{LeasePending, LeaseProvisioning},
		{LeaseProvisioning, LeaseActive}, {LeaseSuspended, LeaseActive}, {LeaseParked, LeaseActive},
		{LeaseActive, LeaseParked},
		{LeaseActive, LeaseSuspended}, {LeaseParked, LeaseSuspended},
		{LeaseActive, LeaseGrace}, {LeaseSuspended, LeaseGrace}, {LeaseParked, LeaseGrace},
		{LeasePending, LeasePurging}, {LeaseProvisioning, LeasePurging}, {LeaseActive, LeasePurging}, {LeaseParked, LeasePurging},
		{LeaseSuspended, LeasePurging}, {LeaseGrace, LeasePurging},
		{LeasePurging, LeasePurged},
		{LeaseProvisioning, LeaseFailed}, {LeasePending, LeaseFailed},
		{LeaseReserved, LeaseCancelled}, {LeasePending, LeaseCancelled},
	} {
		legal[p] = true
	}
	for _, from := range allLeaseStates {
		for _, to := range allLeaseStates {
			assert.Equal(t, legal[[2]string{from, to}], CanTransitionLease(from, to), "%s → %s", from, to)
		}
	}
	// 终态不可再迁出; 唯一例外 purged → pending(宽限期后 restart 续租复用租约行, T-088)。
	for _, term := range leaseTerminal {
		for _, to := range allLeaseStates {
			if term == LeasePurged && to == LeasePending {
				continue
			}
			assert.False(t, CanTransitionLease(term, to), "%s → %s", term, to)
		}
	}
}

func TestLeaseNo(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 200; i++ {
		no, err := newLeaseNo()
		require.NoError(t, err)
		require.True(t, ValidLeaseNo(no), no)
		require.False(t, seen[no])
		seen[no] = true
	}
	for _, bad := range []string{"", "l-", "l-abcdefg", "l-abcdefghi", "x-abcdefgh", "l-ABCDEFGH", "l-abcdefg1", "l-abcdefg8", "l-abcd-fgh"} {
		assert.False(t, ValidLeaseNo(bad), bad)
	}
	assert.True(t, ValidLeaseNo("l-a2b3c4d7"))
	assert.Equal(t, "omnis-l-a2b3c4d7", containerNameOf("l-a2b3c4d7"))
	assert.Equal(t, "omnis-l-a2b3c4d7-data", volumeNameOf("l-a2b3c4d7"))
	assert.Equal(t, "omnis-l-a2b3c4d7-net", networkNameOf("l-a2b3c4d7"))
}

func slotsOf(spec map[string][]int) []Slot {
	var out []Slot
	for g, idx := range spec {
		for _, i := range idx {
			out = append(out, Slot{ID: int64(i + 1), GPUIndex: i, TopoGroup: g})
		}
	}
	return out
}

func indexes(s []Slot) []int {
	out := make([]int, len(s))
	for i, x := range s {
		out[i] = x.GPUIndex
	}
	return out
}

func TestPickSlots(t *testing.T) {
	// a. 某组空闲 ≥ k: 取空闲数最少且够用的组(best-fit), 组内 gpu_index 升序。
	free := slotsOf(map[string][]int{"g0": {0, 1, 2, 3}, "g1": {5, 6}})
	assert.Equal(t, []int{5, 6}, indexes(pickSlots(free, 2)))
	assert.Equal(t, []int{0, 1, 2}, indexes(pickSlots(free, 3)))
	// 同数取组名小者。
	free = slotsOf(map[string][]int{"g1": {6, 4}, "g0": {3, 1}})
	assert.Equal(t, []int{1, 3}, indexes(pickSlots(free, 2)))
	// b. 无单组够用: 按组空闲数降序整组拼接, 最后一组按 gpu_index 取足。
	free = slotsOf(map[string][]int{"g0": {0, 1}, "g1": {4, 5, 6}, "g2": {7}})
	assert.Equal(t, []int{4, 5, 6, 0}, indexes(pickSlots(free, 4)))
	// 不足 k / k 非法。
	assert.Nil(t, pickSlots(free, 7))
	assert.Nil(t, pickSlots(free, 0))
}

func TestSortCandidates(t *testing.T) {
	c := []allocCandidate{
		{NodeID: 1, Free: 8, Stability: 100},
		{NodeID: 2, Free: 4, Stability: 10},
		{NodeID: 3, Free: 4, Stability: 50},
		{NodeID: 4, Free: 5, Stability: 999},
		{NodeID: 5, Free: 4, Stability: 50},
	}
	sortCandidates(c, 4)
	var ids []int64
	for _, x := range c {
		ids = append(ids, x.NodeID)
	}
	// best-fit(空闲−k 小者) → 稳定性高者 → node_id 小者。
	assert.Equal(t, []int64{3, 5, 2, 4, 1}, ids)
}

func TestCPUMemSplitAndShm(t *testing.T) {
	cpu, mem, err := cpuMemSplit(128, 1024, 8, 32, 2, 8)
	require.NoError(t, err)
	assert.Equal(t, 30, cpu) // (128-8)*2/8
	assert.Equal(t, 248, mem)
	cpu, mem, err = cpuMemSplit(128, 1024, 8, 32, 8, 8)
	require.NoError(t, err)
	assert.Equal(t, 120, cpu)
	assert.Equal(t, 992, mem)
	cpu, _, err = cpuMemSplit(30, 100, 8, 32, 1, 8) // 22/8 = 2
	require.NoError(t, err)
	assert.Equal(t, 2, cpu)
	_, _, err = cpuMemSplit(12, 64, 8, 32, 1, 8) // 4/8 = 0 核
	assert.Error(t, err)
	for _, bad := range [][2]int{{0, 8}, {9, 8}, {1, 0}} {
		_, _, err = cpuMemSplit(128, 1024, 8, 32, bad[0], bad[1])
		assert.Error(t, err, "%v", bad)
	}
	assert.Equal(t, 1, shmSizeGB(1))
	assert.Equal(t, 31, shmSizeGB(62))
	assert.Equal(t, 64, shmSizeGB(248))
}

func TestPickLeasePort(t *testing.T) {
	assert.Equal(t, 30000, pickLeasePort(30000, 30010, nil, nil))
	assert.Equal(t, 30002, pickLeasePort(30000, 30010, map[int]bool{30000: true}, map[int]bool{30001: true}))
	all := map[int]bool{30000: true, 30001: true}
	assert.Equal(t, 0, pickLeasePort(30000, 30001, all, nil))
}

// T-092: max_free_per_node 的单节点可租卡数与 allocCandidates 同口径。
func TestRentableOnNode(t *testing.T) {
	cases := []struct {
		free, total         int
		quota, wholeMachine bool
		want                int
	}{
		{5, 8, true, false, 5},  // 容器 + 磁盘配额: 任意剩余卡数可租
		{0, 8, true, false, 0},  // 无空闲
		{5, 8, false, false, 0}, // 无磁盘配额: 只能整机租
		{8, 8, false, false, 8}, // 无磁盘配额 + 全空闲: 整机
		{7, 8, true, true, 0},   // 整机交付: 必须全空闲
		{8, 8, true, true, 8},
	}
	for _, c := range cases {
		assert.Equal(t, c.want, rentableOnNode(c.free, c.total, c.quota, c.wholeMachine), "%+v", c)
	}
}
