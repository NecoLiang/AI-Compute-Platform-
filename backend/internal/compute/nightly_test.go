package compute

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// 包夜夜晚标识与可订规则(S4 T-094): 跨零点窗口 22:00-06:00 / 不跨零点窗口 01:00-07:00, Asia/Shanghai。

func sh(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02 15:04:05", s, shanghaiLoc())
	require.NoError(t, err)
	return v
}

func day(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.ParseInLocation("2006-01-02", s, shanghaiLoc())
	require.NoError(t, err)
	return v
}

func TestNightIdentityAcrossMidnight(t *testing.T) {
	w := NightWindow{StartMin: 22 * 60, EndMin: 6 * 60}
	require.NoError(t, w.Validate())
	assert.Equal(t, 8*time.Hour, w.Length())

	d := day(t, "2026-11-01")
	assert.Equal(t, sh(t, "2026-11-01 22:00:00"), NightStartAt(d, w))
	assert.Equal(t, sh(t, "2026-11-02 06:00:00"), NightEndAt(d, w), "跨零点窗口结束在次日")
	assert.Equal(t, NightlyLeaseEnd(d, 3, w), NightEndAt(d.AddDate(0, 0, 2), w), "租期终点 = 末晚窗口结束")

	for _, c := range []struct{ now, night, first, started string }{
		{"2026-11-01 21:00:00", "2026-11-01", "2026-11-01", "2026-10-31"}, // 窗口前: 当晚可订
		{"2026-11-01 23:30:00", "2026-11-01", "2026-11-01", "2026-11-01"}, // 窗口开始 1.5h: 仍可订当晚(Q-36)
		{"2026-11-01 23:59:59", "2026-11-01", "2026-11-01", "2026-11-01"},
		{"2026-11-02 00:00:00", "2026-11-01", "2026-11-02", "2026-11-01"}, // 开始满 2h: 截止, 第一晚为 11-02
		{"2026-11-02 01:00:00", "2026-11-01", "2026-11-02", "2026-11-01"}, // 零点后仍属 11-01 晚
		{"2026-11-02 06:00:00", "2026-11-02", "2026-11-02", "2026-11-01"}, // 窗口结束: 进入 11-02 晚
	} {
		now := sh(t, c.now)
		assert.Equal(t, c.night, NightDateOf(now, w).Format("2006-01-02"), "NightDateOf %s", c.now)
		assert.Equal(t, c.first, FirstBookableNight(now, w).Format("2006-01-02"), "FirstBookableNight %s", c.now)
		assert.Equal(t, c.started, LastStartedNight(now, w).Format("2006-01-02"), "LastStartedNight %s", c.now)
	}
	// UTC 输入同样按上海时间归属。
	assert.Equal(t, "2026-11-01", NightDateOf(sh(t, "2026-11-02 01:00:00").UTC(), w).Format("2006-01-02"))
}

func TestNightIdentitySameDayWindow(t *testing.T) {
	w := NightWindow{StartMin: 60, EndMin: 7 * 60}
	assert.Equal(t, sh(t, "2026-11-02 07:00:00"), NightEndAt(day(t, "2026-11-02"), w))
	// 窗口结束后的白天属于下一晚(下一个日期的 01:00 开始)。
	assert.Equal(t, "2026-11-03", NightDateOf(sh(t, "2026-11-02 12:00:00"), w).Format("2006-01-02"))
	assert.Equal(t, "2026-11-03", FirstBookableNight(sh(t, "2026-11-02 12:00:00"), w).Format("2006-01-02"))
	assert.Equal(t, "2026-11-02", FirstBookableNight(sh(t, "2026-11-02 02:59:00"), w).Format("2006-01-02"))
	assert.Equal(t, "2026-11-03", FirstBookableNight(sh(t, "2026-11-02 03:00:00"), w).Format("2006-01-02"))
	assert.Equal(t, "2026-11-01", LastStartedNight(sh(t, "2026-11-02 00:59:00"), w).Format("2006-01-02"))
}

func TestParseNightStartDate(t *testing.T) {
	w := NightWindow{StartMin: 22 * 60, EndMin: 6 * 60}
	now := sh(t, "2026-11-02 00:30:00") // 11-01 晚已开始 2.5h, 截止
	code := func(err error) int { return ErrToCode(err) }

	_, err := ParseNightStartDate("", 1, w, now)
	assert.Equal(t, 40001, code(err))
	_, err = ParseNightStartDate("11/02", 1, w, now)
	assert.Equal(t, 40001, code(err))
	_, err = ParseNightStartDate("2026-11-01", 1, w, now)
	assert.Equal(t, 40001, code(err), "当晚已过截止")
	d, err := ParseNightStartDate("2026-11-02", 31, w, now)
	require.NoError(t, err)
	assert.Equal(t, day(t, "2026-11-02"), d)
	_, err = ParseNightStartDate("2026-12-02", 1, w, now)
	require.NoError(t, err, "今天起第 30 天可作为起始夜晚")
	_, err = ParseNightStartDate("2026-12-03", 1, w, now)
	assert.Equal(t, 40001, code(err), "超出 30 天")
	_, err = ParseNightStartDate("2026-12-02", 31, w, now)
	require.NoError(t, err, "第 30 天起订 31 晚, 末晚为第 60 天")
	_, err = ParseNightStartDate("2026-12-02", 32, w, now)
	assert.Equal(t, 40001, code(err), "末晚超出 60 天")

	// 当晚开始 1 小时: 当晚可订。
	_, err = ParseNightStartDate("2026-11-01", 1, w, sh(t, "2026-11-01 23:00:00"))
	require.NoError(t, err)
}
