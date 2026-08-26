package ticket

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestValidType(t *testing.T) {
	for _, valid := range []string{"refund_dispute", "resource_fault", "unavailable", "appeal", "other"} {
		assert.True(t, ValidType(valid), valid)
	}
	for _, invalid := range []string{"", "refund", "FAULT", "unknown"} {
		assert.False(t, ValidType(invalid), invalid)
	}
}

func TestNextTicketNoFromMax(t *testing.T) {
	cases := []struct {
		name   string
		prefix string
		maxNo  string
		want   string
	}{
		{"当日首单", "WO-20260826-", "", "WO-20260826-001"},
		{"递增", "WO-20260826-", "WO-20260826-001", "WO-20260826-002"},
		{"进位", "WO-20260826-", "WO-20260826-099", "WO-20260826-100"},
		{"跨日旧编号不影响", "WO-20260826-", "WO-20260825-003", "WO-20260826-001"},
		{"异常后缀安全回落", "WO-20260826-", "WO-20260826-ABC", "WO-20260826-001"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, NextTicketNoFromMax(tc.prefix, tc.maxNo))
		})
	}
}

func TestCanAppendMessage(t *testing.T) {
	assert.True(t, CanAppendMessage("pending"))
	assert.True(t, CanAppendMessage("processing"))
	assert.False(t, CanAppendMessage("resolved"))
	assert.False(t, CanAppendMessage("closed"))
}

func TestCanTransition(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		{"pending", "processing", true},
		{"pending", "closed", true},
		{"pending", "resolved", false}, // 未受理不能直接完结
		{"processing", "resolved", true},
		{"processing", "closed", true},
		{"processing", "pending", false},
		{"resolved", "closed", true},
		{"resolved", "processing", false},
		{"closed", "processing", false}, // 终态不可恢复
		{"closed", "pending", false},
	}
	for _, tc := range cases {
		t.Run(tc.from+"->"+tc.to, func(t *testing.T) {
			assert.Equal(t, tc.want, CanTransition(tc.from, tc.to))
		})
	}
}
