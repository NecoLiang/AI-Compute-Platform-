package fleet

import (
	"context"
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseComputeApps(t *testing.T) {
	m, err := ParseComputeApps("")
	require.NoError(t, err)
	assert.Empty(t, m)
	m, err = ParseComputeApps("No running processes found\n")
	require.NoError(t, err)
	assert.Empty(t, m)
	m, err = ParseComputeApps("GPU-" + strings.ToUpper(fakeUUID0[4:]) + ", 4242\n" + fakeUUID0 + ", 4243\n" + fakeUUID1 + ", 77\n")
	require.NoError(t, err)
	assert.Equal(t, []string{"4242", "4243"}, m[strings.ToUpper(fakeUUID0)])
	assert.Equal(t, []string{"77"}, m[strings.ToUpper(fakeUUID1)])
	_, err = ParseComputeApps("[Insufficient Permissions]\n")
	assert.Error(t, err, "无法识别的输出按占用未知处理")
}

// 宿主机进程占用的空闲卡(如供给方自己在跑推理服务): 不挂探针容器、不压测、不计入可售; 进程退出后下次完整探活恢复。
func TestFullProbeExcludesHostBusyGPUs(t *testing.T) {
	s, db, fake, id, o := fullProbedNode(t, func(fake *fakeNode, _ *sqlx.DB, _ int64) {
		fake.Override(cmdGPUApps, fakeReply{Stdout: fakeUUID0 + ", 4242\n"})
		fake.Override("docker run --rm --network none --gpus", fakeReply{Stdout: "GPU 0: NVIDIA H100 80GB HBM3 (UUID: " + fakeUUID1 + ")\n"})
	})
	require.True(t, o.Pass, "failed=%v", o.Failed)
	assert.Equal(t, SlotFaulty, slotState(t, db, id, 0))
	assert.Equal(t, SlotFree, slotState(t, db, id, 1))
	_, checks := latestChecks(t, db, id)
	assert.True(t, checks["P7"].OK, "P7 只反映硬件健康: %s", checks["P7"].Detail)
	for _, c := range fake.Commands() {
		if strings.Contains(c, "--gpus") {
			assert.NotContains(t, c, fakeUUID0, "被占用的卡不得挂进任何探针容器: %s", c)
		}
	}
	assert.Contains(t, lastCmdWith(fake, "docker run --rm --name omnis-probe-"), `--gpus '"device=`+fakeUUID1+`"'`)
	assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM node_probe_reports WHERE node_id=? AND inventory LIKE '%宿主机计算进程占用(pid 4242)%'`, id))

	fake.ClearOverrides()
	_, err := s.Verify(10, id)
	require.NoError(t, err)
	done := runTasks(t, s)
	require.Len(t, done, 1)
	require.True(t, outcomeOf(t, done[0]).Pass)
	assert.Equal(t, SlotFree, slotState(t, db, id, 0), "进程退出后完整探活恢复可售")
}

// 读不到 GPU 计算进程: 无法确认哪些卡空闲, P4 不通过且不做任何挂卡侵入项。
func TestFullProbeFailsWhenGPUAppsUnknown(t *testing.T) {
	_, _, fake, _, o := fullProbedNode(t, func(fake *fakeNode, _ *sqlx.DB, _ int64) {
		fake.Override(cmdGPUApps, fakeReply{Stderr: "Failed to initialize NVML\n", Code: 9})
	})
	require.False(t, o.Pass)
	assert.Contains(t, o.Failed, "P4")
	for _, c := range fake.Commands() {
		assert.NotContains(t, c, "--gpus", "占用未知时不得挂卡: %s", c)
	}
}

// 轻探活: 空闲卡出现宿主机进程 → 停止出租; 平台探针容器运行中(@@probe)本轮不判; 读取失败不计探活失败。
func TestLightProbeMarksHostBusyFreeSlot(t *testing.T) {
	s, db, fake, id, o := fullProbedNode(t, nil)
	require.True(t, o.Pass, "failed=%v", o.Failed)
	n, err := s.repo.GetNode(id)
	require.NoError(t, err)

	fake.Override(cmdLightGPUApps, fakeReply{Stdout: "@@probe\n"})
	s.lightProbe(context.Background(), n)
	assert.Equal(t, SlotFree, slotState(t, db, id, 0))

	fake.Override(cmdLightGPUApps, fakeReply{Stderr: "docker: permission denied\n", Code: 1})
	s.lightProbe(context.Background(), n)
	assert.Equal(t, SlotFree, slotState(t, db, id, 0))
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM supplier_nodes WHERE id=? AND probe_fail_count>0`, id))

	fake.Override(cmdLightGPUApps, fakeReply{Stdout: fakeUUID0 + ", 5150\n"})
	s.lightProbe(context.Background(), n)
	assert.Equal(t, SlotFaulty, slotState(t, db, id, 0))
	assert.Equal(t, SlotFree, slotState(t, db, id, 1))
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM supplier_nodes WHERE id=? AND probe_fail_count>0`, id))
}
