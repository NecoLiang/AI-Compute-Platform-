package fleet

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestPickRestartSlots(t *testing.T) {
	lid, other := int64(7), int64(9)
	sl := func(id int64, idx int, uuid, state string, lease *int64) Slot {
		return Slot{ID: id, GPUIndex: idx, GPUUUID: uuid, TopoGroup: "g0", State: state, LeaseID: lease}
	}
	ids := func(list []Slot) []int64 {
		var out []int64
		for _, s := range list {
			out = append(out, s.ID)
		}
		return out
	}
	slots := []Slot{
		sl(1, 0, "GPU-a", SlotFree, nil),
		sl(2, 1, "GPU-b", SlotDraining, &lid), // stop 尚未执行, 仍属本租约
		sl(3, 2, "GPU-c", SlotFree, nil),      // 原卡, 已被 stop 释放
		sl(4, 3, "GPU-d", SlotFree, nil),
		sl(5, 4, "GPU-e", SlotLeased, &other),
	}
	orig := []string{"GPU-b", "GPU-c"}
	// 原卡(本租约持有的 + 已空闲的)优先。
	assert.Equal(t, []int64{2, 3}, ids(pickRestartSlots(slots, lid, 2, orig)))
	// 原卡不足时同机补空闲卡。
	assert.Equal(t, []int64{2, 3, 1}, ids(pickRestartSlots(slots, lid, 3, orig)))
	// 别的租约占用的卡不可选; 不足返回 nil。
	assert.Nil(t, pickRestartSlots(slots, lid, 5, orig))
	// 无原卡记录: 本租约持有的优先, 再按拓扑补足。
	assert.Equal(t, []int64{2, 1}, ids(pickRestartSlots(slots, lid, 2, nil)))
	assert.Nil(t, pickRestartSlots(slots, lid, 0, orig))
}

func TestSameGPUSet(t *testing.T) {
	assert.True(t, sameGPUSet([]string{"GPU-b", "GPU-a"}, []string{"GPU-a", "GPU-b"}))
	assert.False(t, sameGPUSet([]string{"GPU-a"}, []string{"GPU-a", "GPU-b"}))
	assert.False(t, sameGPUSet([]string{"GPU-a", "GPU-c"}, []string{"GPU-a", "GPU-b"}))
	assert.True(t, sameGPUSet(nil, nil))
}

func TestIsLeaseCommitRef(t *testing.T) {
	assert.True(t, isLeaseCommitRef("l-abcdefgh", "omnis-commit/l-abcdefgh:1700000000"))
	assert.False(t, isLeaseCommitRef("l-abcdefgh", "omnis-commit/l-zzzzzzzz:1700000000"))
	assert.False(t, isLeaseCommitRef("l-abcdefgh", "omnis-commit/l-abcdefgh:latest"))
	assert.False(t, isLeaseCommitRef("bad", "omnis-commit/bad:1700000000"))
}
