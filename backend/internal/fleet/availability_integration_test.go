package fleet

import (
	"context"
	"fmt"
	"os"
	"testing"

	"tokenfactory/internal/compute"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// availabilityRedis 每个测试用独立的 Redis DB 与清空的限流 key, 避免与其他包并行测试互相计数。
func availabilityRedis(t *testing.T, db int, userIDs ...int64) *redis.Client {
	t.Helper()
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR is required for availability rate limit tests")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr, DB: db})
	t.Cleanup(func() { _ = rdb.Close() })
	require.NoError(t, rdb.Ping(context.Background()).Err())
	for _, id := range userIDs {
		require.NoError(t, rdb.Del(context.Background(), fmt.Sprintf("market:availability:rl:%d", id)).Err())
	}
	return rdb
}

func availabilityOf(t *testing.T, comp *compute.Service, buyer int64, req compute.AvailabilityReq) *compute.Availability {
	t.Helper()
	a, err := comp.ProductAvailability(context.Background(), buyer, leaseProductID, req)
	require.NoError(t, err)
	return a
}

func slotStateCounts(t *testing.T, db interface {
	Select(dest any, query string, args ...any) error
}) map[string]int {
	t.Helper()
	var rows []struct {
		State string `db:"state"`
		N     int    `db:"n"`
	}
	require.NoError(t, db.Select(&rows, `SELECT state, COUNT(*) AS n FROM gpu_slots GROUP BY state`))
	out := map[string]int{}
	for _, r := range rows {
		out[r.State] = r.N
	}
	return out
}

// T-093: 托管商品 availability 只读判定 + 与下单同一报价函数; 卡数须在可选卡数内; 无整机余量返回 available=false。
func TestManagedAvailabilityMatchesOrderAndNeverReserves(t *testing.T) {
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	seedLeaseMarket(t, db, s, "container", 2, 8, 4)
	comp := wireLeases(t, s)
	comp.SetRateLimiter(availabilityRedis(t, 5, 21, 22, 23, 24))

	var maxFree int
	require.NoError(t, db.Get(&maxFree, `SELECT max_free_per_node FROM products WHERE id=?`, leaseProductID))
	assert.Equal(t, 8, maxFree)

	a := availabilityOf(t, comp, 21, compute.AvailabilityReq{Cards: 4, Duration: 2})
	require.True(t, a.Available, a.Reason)
	require.NotNil(t, a.Quote)
	assert.Equal(t, "hourly", a.Quote.PricingMode)
	assert.EqualValues(t, 1500, a.Quote.UnitPrice)
	assert.EqualValues(t, 1500*4*2, a.Quote.TotalAmount)
	require.NotNil(t, a.LeasePreview)
	assert.Equal(t, compute.LeaseStartOnActivation, a.LeasePreview.StartRule)
	require.NotNil(t, a.LeasePreview.StartAt)
	require.NotNil(t, a.LeasePreview.EndAt)
	assert.Equal(t, "2h0m0s", a.LeasePreview.EndAt.Sub(*a.LeasePreview.StartAt).String())
	assert.Equal(t, map[string]int{"free": 16}, slotStateCounts(t, db), "availability 不得预留卡槽")

	// 报价与真实下单一致(同一 QuoteOrder + 同一费率)。
	o, err := placeManaged(comp, 21, 4)
	require.NoError(t, err)
	assert.Equal(t, a.Quote.TotalAmount, o.TotalAmount)
	assert.Equal(t, a.Quote.PlatformFee, o.PlatformFee)
	assert.Equal(t, a.Quote.UnitPrice, o.UnitPrice)

	// 卡数不在可选集合(默认 1,2,4,6,8) → 40001; 下单同样拒绝。
	_, err = comp.ProductAvailability(context.Background(), 21, leaseProductID, compute.AvailabilityReq{Cards: 3, Duration: 2})
	require.Error(t, err)
	assert.Equal(t, 40001, compute.ErrToCode(err))
	_, err = placeManaged(comp, 22, 3)
	require.Error(t, err)
	assert.Equal(t, 40001, compute.ErrToCode(err))
	// 托管商品不支持买断/未开放的方式 → 40001。
	_, err = comp.ProductAvailability(context.Background(), 21, leaseProductID, compute.AvailabilityReq{PricingMode: "daily", Cards: 4, Duration: 1})
	require.Error(t, err)
	assert.Equal(t, 40001, compute.ErrToCode(err))

	// 再下 2 单 4 卡: 一台满、另一台剩 4 张 → 8 卡无整机余量, 4 卡仍可租; 查询前后卡槽不变。
	for _, b := range []int64{22, 23} {
		_, err := placeManaged(comp, b, 4)
		require.NoError(t, err)
	}
	before := slotStateCounts(t, db)
	assert.Equal(t, map[string]int{"free": 4, "reserved": 12}, before)
	require.NoError(t, db.Get(&maxFree, `SELECT max_free_per_node FROM products WHERE id=?`, leaseProductID))
	assert.Equal(t, 4, maxFree)

	full := availabilityOf(t, comp, 24, compute.AvailabilityReq{Cards: 8, Duration: 2})
	assert.False(t, full.Available)
	assert.Equal(t, "所选卡数暂无整机余量", full.Reason)
	require.NotNil(t, full.Quote, "不可下单时仍给出报价")
	assert.EqualValues(t, 1500*8*2, full.Quote.TotalAmount)
	assert.True(t, availabilityOf(t, comp, 24, compute.AvailabilityReq{Cards: 4, Duration: 2}).Available)
	assert.Equal(t, before, slotStateCounts(t, db))

	// fleet 未接线(gate=nil)时托管商品不可下单, 但不报错。
	comp.SetManagedGate(nil)
	off := availabilityOf(t, comp, 24, compute.AvailabilityReq{Cards: 4, Duration: 2})
	assert.False(t, off.Available)
	assert.Equal(t, compute.ErrManagedDisabled.Error(), off.Reason)
	comp.SetManagedGate(s)

	// 包夜: 价格行存在也返回 available=false + 原因, 不给报价。
	db.MustExec(`INSERT INTO product_prices (product_id, pricing_mode, unit_price, min_duration, enabled) VALUES (?, 'hourly', 1500, 1, 1), (?, 'nightly', 9000, 1, 1)`,
		leaseProductID, leaseProductID)
	night := availabilityOf(t, comp, 24, compute.AvailabilityReq{PricingMode: "nightly", Cards: 4, Duration: 1, NightStartDate: "2026-11-01"})
	assert.False(t, night.Available)
	assert.Contains(t, night.Reason, "包夜")
	assert.Nil(t, night.Quote)
	assert.Nil(t, night.LeasePreview)
	// night_start_date 只适用于包夜。
	_, err = comp.ProductAvailability(context.Background(), 24, leaseProductID, compute.AvailabilityReq{Cards: 4, Duration: 2, NightStartDate: "2026-11-01"})
	assert.Equal(t, 40001, compute.ErrToCode(err))
	_, err = comp.ProductAvailability(context.Background(), 24, leaseProductID, compute.AvailabilityReq{PricingMode: "nightly", Cards: 4, Duration: 1, NightStartDate: "11/01"})
	assert.Equal(t, 40001, compute.ErrToCode(err))
}

// T-093: 每用户每分钟 30 次, 第 31 次 42900; 用户之间互不影响。
func TestAvailabilityRateLimitPerUser(t *testing.T) {
	db := setupFleetDB(t)
	s := newTestFleet(t, db)
	seedLeaseMarket(t, db, s, "container", 1, 8, 2)
	comp := wireLeases(t, s)
	rdb := availabilityRedis(t, 6, 21, 22)
	comp.SetRateLimiter(rdb)

	req := compute.AvailabilityReq{Cards: 1, Duration: 1}
	for i := 0; i < 30; i++ {
		availabilityOf(t, comp, 21, req)
	}
	_, err := comp.ProductAvailability(context.Background(), 21, leaseProductID, req)
	require.Error(t, err)
	assert.Equal(t, 42900, compute.ErrToCode(err))
	assert.True(t, availabilityOf(t, comp, 22, req).Available)
	ttl, err := rdb.PTTL(context.Background(), "market:availability:rl:21").Result()
	require.NoError(t, err)
	assert.Greater(t, ttl.Milliseconds(), int64(0))
	assert.LessOrEqual(t, ttl.Milliseconds(), int64(60000))

	// 未装配 Redis 时不放开限流。
	comp.SetRateLimiter(nil)
	_, err = comp.ProductAvailability(context.Background(), 22, leaseProductID, req)
	assert.Equal(t, 50000, compute.ErrToCode(err))
}
