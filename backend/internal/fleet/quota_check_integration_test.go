package fleet

import (
	"strings"
	"testing"

	"github.com/jmoiron/sqlx"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestQuotaCheckCommandSyntax(t *testing.T) {
	cmd, err := quotaCheckCommand("registry.example.com/omnis/probe@sha256:" + strings.Repeat("ab", 32))
	require.NoError(t, err)
	shSyntaxOK(t, cmd)
	assert.Contains(t, cmd, "--storage-opt size=64M")
	_, err = quotaCheckCommand("busybox:latest")
	assert.Error(t, err)
}

// containerd snapshotter(overlayfs)接受 --storage-opt 却不限写入: P10 必须判不通过, 节点仍可上架但只能整机出租。
func TestP10QuotaAcceptedButNotEnforced(t *testing.T) {
	for _, tc := range []struct {
		name, out, want string
	}{
		{"未生效", "@@not-enforced\n", "被接受但未生效"},
		{"无结论", "sh: dd: not found\n@@small-fail\n", "未得出结论"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, db, _, id, o := fullProbedNode(t, func(fake *fakeNode, _ *sqlx.DB, _ int64) {
				fake.Override("docker run --rm --network none --storage-opt", fakeReply{Stdout: tc.out})
			})
			require.True(t, o.Pass, "P10 非必需项, failed=%v", o.Failed)
			_, checks := latestChecks(t, db, id)
			assert.False(t, checks["P10"].OK)
			assert.Contains(t, checks["P10"].Detail, tc.want)
			assert.Equal(t, 1, countRows(t, db, `SELECT COUNT(*) FROM supplier_nodes WHERE id=? AND disk_quota_supported=0`, id))
		})
	}
}
