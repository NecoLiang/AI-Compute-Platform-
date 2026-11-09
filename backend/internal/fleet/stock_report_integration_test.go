package fleet

import (
	"testing"

	"tokenfactory/internal/compute"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// T-093: 托管商品不接受人工盘点/库存上报(40001), 库存保持卡槽派生值, 不写盘点快照。
func TestManagedProductRejectsStockReport(t *testing.T) {
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	seedLeaseMarket(t, db, s, "container", 2, 8, 1)
	comp := wireLeases(t, s)
	db.MustExec(`INSERT INTO users (id, phone, password_hash) VALUES (90,'13800000090','x')`)
	db.MustExec(`INSERT INTO user_roles (user_id, role) VALUES (90,'admin')`)

	for _, c := range []struct {
		op    int64
		admin bool
		typ   string
	}{{20, false, "passive"}, {90, true, "active"}} {
		_, err := comp.SyncResource(c.op, c.admin, c.typ, compute.ResourceSyncReq{ProductID: leaseProductID, StockAfter: 5, Reason: "盘点"})
		require.Error(t, err)
		assert.Equal(t, 40001, compute.ErrToCode(err))
		assert.Equal(t, "托管商品库存由平台按卡槽自动计算", err.Error())
	}
	stock, status := leaseStock(t, db)
	assert.Equal(t, 16, stock)
	assert.Equal(t, "active", status)
	assert.Equal(t, 0, countRows(t, db, `SELECT COUNT(*) FROM resource_snapshots`))
}
