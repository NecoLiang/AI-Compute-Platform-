package fleet

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"tokenfactory/internal/compute"
	"tokenfactory/internal/invoice"
	"tokenfactory/internal/payment"
	"tokenfactory/pkg/errcode"
)

// T-106 租期末结算 / 部分退款 / 供给方违约退款集成测试: 真实 MySQL + payment.Service + 易宝桩(仅测试文件)。

// fakeYeepay 易宝网关桩: 记录退款 / 分账请求, 退款同步结果与查询结果可配置。
type fakeYeepay struct {
	mu          sync.Mutex
	configured  bool
	refundState string        // Refund 同步返回的状态
	refundErr   error         // Refund 返回的错误(优先)
	queryState  string        // QueryRefund 返回的状态
	refundDelay time.Duration // 并发测试: 拉长"已提交未返回"窗口
	refunds     []payment.RefundOrder
	divides     []payment.DivideOrder
}

func newFakeYeepay() *fakeYeepay {
	return &fakeYeepay{configured: true, refundState: "SUCCESS", queryState: "SUCCESS"}
}

func (f *fakeYeepay) Configured() bool      { return f.configured }
func (f *fakeYeepay) PayChannels() []string { return []string{"wechat"} }
func (f *fakeYeepay) CreatePayment(payment.PaymentOrder) (*payment.Checkout, error) {
	return nil, errors.New("fakeYeepay: CreatePayment not used")
}
func (f *fakeYeepay) QueryPayment(string) (*payment.PaymentResult, error) {
	return nil, errors.New("fakeYeepay: QueryPayment not used")
}
func (f *fakeYeepay) Divide(o payment.DivideOrder) (*payment.DivideResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.divides = append(f.divides, o)
	return &payment.DivideResult{Status: "SUCCESS", Details: []payment.DivideDetail{{DetailNo: "DD-" + o.RequestID, LedgerNo: o.Items[0].LedgerNo}}}, nil
}
func (f *fakeYeepay) QueryDivide(string, string) (*payment.DivideResult, error) {
	return &payment.DivideResult{Status: "SUCCESS"}, nil
}
func (f *fakeYeepay) Refund(o payment.RefundOrder) (*payment.RefundResult, error) {
	time.Sleep(f.refundDelay)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.refunds = append(f.refunds, o)
	if f.refundErr != nil {
		return nil, f.refundErr
	}
	return &payment.RefundResult{UniqueRefundNo: "YR-" + o.RequestID, Status: f.refundState, Amount: o.Amount}, nil
}
func (f *fakeYeepay) QueryRefund(_, requestID string) (*payment.RefundResult, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var amount int64
	for _, r := range f.refunds {
		if r.RequestID == requestID {
			amount = r.Amount
		}
	}
	return &payment.RefundResult{UniqueRefundNo: "YR-" + requestID, Status: f.queryState, Amount: amount}, nil
}
func (f *fakeYeepay) NetInURL(string) (string, error) {
	return "", errors.New("fakeYeepay: NetInURL not used")
}
func (f *fakeYeepay) QueryNetIn(string) (*payment.NetInResult, error) {
	return nil, errors.New("fakeYeepay: QueryNetIn not used")
}
func (f *fakeYeepay) DecryptNotify(raw string) ([]byte, error) { return []byte(raw), nil }

func (f *fakeYeepay) refundCalls() []payment.RefundOrder {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]payment.RefundOrder(nil), f.refunds...)
}

func (f *fakeYeepay) divideCalls() []payment.DivideOrder {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]payment.DivideOrder(nil), f.divides...)
}

const (
	settlePaid = 72000 // 实付(分)
	settleFee  = 7200  // 平台费(分)
)

type settleEnv struct {
	*provEnv
	pay *payment.Service
	fy  *fakeYeepay
}

// newSettleEnv outageEnv + 订单实付 720 元(平台费 72 元) + 已清算入账的支付与两行应结 + 供给方已开通商编。
// enable=true 时打开部分退款通道并注入 fleet(等同 main.go 在 yeepay_partial_refund_confirmed=true 时的接线)。
func newSettleEnv(t *testing.T, enable bool) *settleEnv {
	t.Helper()
	e := outageEnv(t)
	e.db.MustExec(`UPDATE orders SET total_amount=?, platform_fee=? WHERE id=?`, settlePaid, settleFee, e.order.ID)
	seedPaidOrder(t, e, e.order.OrderNo, settlePaid, settleFee)
	e.db.MustExec(`INSERT INTO yeepay_merchants (user_id, request_no, merchant_no, application_status) VALUES (20, 'NETIN-20', 'LEDGER-20', 'COMPLETED')`)
	fy := newFakeYeepay()
	pay := payment.NewService(payment.NewRepository(e.db), e.db, fy)
	se := &settleEnv{provEnv: e, pay: pay, fy: fy}
	if enable {
		se.enable(t)
	}
	return se
}

func (e *settleEnv) enable(t *testing.T) {
	t.Helper()
	require.NoError(t, e.pay.EnablePartialRefunds())
	e.s.SetOutageRefunder(e.pay)
	e.s.SetBreachRefunder(e.pay)
	e.s.cfg.Outage.YeepayPartialRefundConfirmed = true
}

// seedPaidOrder 收款 + 清算入账的落库等价(payment.confirmPayment / markCleared): 支付行 + 两行 pending 应结。
func seedPaidOrder(t *testing.T, e *provEnv, orderNo string, amount, fee int64) {
	t.Helper()
	e.db.MustExec(`INSERT INTO payments (order_no, amount, channel, yeepay_tx_id, status, paid_at, cleared_at)
		VALUES (?, ?, 'wechat', ?, 'paid', NOW(), NOW())`, orderNo, amount, "YTX-"+orderNo)
	e.db.MustExec(`INSERT INTO settlements (settlement_id, order_no, payee_type, payee_id, amount, status)
		VALUES (?, ?, 'platform', 0, ?, 'pending'), (?, ?, 'supplier', 20, ?, 'pending')`,
		"SET-"+orderNo+"-platform", orderNo, fee, "SET-"+orderNo+"-supplier", orderNo, amount-fee)
}

// endedOutage 直接落一段已结束、待退费的停机(金额与责任方由测试指定)。
func (e *settleEnv) endedOutage(t *testing.T, orderID int64, liable string, amount int64) int64 {
	t.Helper()
	res := e.db.MustExec(`INSERT INTO lease_outages (lease_id, order_id, cause, liable, started_at, ended_at, seconds, minutes, counted,
		remedy, remedy_amount, liability_amount) VALUES (?, ?, 'node_offline', ?, NOW()-INTERVAL 30 MINUTE, NOW()-INTERVAL 10 MINUTE,
		1200, 20, 1, 'refund', ?, ?)`, e.lease.ID, orderID, liable, amount, amount)
	id, err := res.LastInsertId()
	require.NoError(t, err)
	return id
}

// purge 到期 → 宽限 → 宽限期满进入清除(purging)。
func (e *settleEnv) purge(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	expireLease(t, e.provEnv)
	e.s.ExpiryRound(ctx)
	require.Equal(t, LeaseGrace, e.leaseNow(t).State)
	e.db.MustExec(`UPDATE leases SET grace_until=NOW()-INTERVAL 1 SECOND WHERE id=?`, e.lease.ID)
	e.s.ExpiryRound(ctx)
	require.Contains(t, []string{LeasePurging, LeasePurged}, e.leaseNow(t).State)
}

func (e *settleEnv) settleReady(t *testing.T, orderID int64) bool {
	t.Helper()
	return countRows(t, e.db, `SELECT COUNT(*) FROM orders WHERE id=? AND settle_ready_at IS NOT NULL`, orderID) == 1
}

type settleRow struct {
	PayeeType    string `db:"payee_type"`
	Amount       int64  `db:"amount"`
	Deduction    int64  `db:"refund_deduction"`
	Compensation int64  `db:"platform_compensation"`
	Status       string `db:"status"`
}

func (e *settleEnv) settlements(t *testing.T, orderNo string) map[string]settleRow {
	t.Helper()
	var rows []settleRow
	require.NoError(t, e.db.Select(&rows, `SELECT payee_type, amount, refund_deduction, platform_compensation, status
		FROM settlements WHERE order_no=?`, orderNo))
	out := map[string]settleRow{}
	for _, r := range rows {
		out[r.PayeeType] = r
	}
	return out
}

func (e *settleEnv) refundRows(t *testing.T, orderNo string) []payment.PartialRefund {
	t.Helper()
	list, _, err := e.pay.ListPartialRefunds("", orderNo, 1, 100)
	require.NoError(t, err)
	return list
}

// assertInvariant 部分退款(成功)合计 + 两行应结 = 实付。
func (e *settleEnv) assertInvariant(t *testing.T, orderNo string, paid int64) {
	t.Helper()
	var refunded, settled int64
	require.NoError(t, e.db.Get(&refunded, `SELECT COALESCE(SUM(amount),0) FROM payment_refunds WHERE order_no=? AND status='success'`, orderNo))
	require.NoError(t, e.db.Get(&settled, `SELECT COALESCE(SUM(amount),0) FROM settlements WHERE order_no=?`, orderNo))
	assert.Equal(t, paid, refunded+settled, "部分退款 %d + 应结 %d 应等于实付", refunded, settled)
}

func refundNotify(orderNo, requestID, status string, amount int64) string {
	b, _ := json.Marshal(map[string]any{"orderId": orderNo, "refundRequestId": requestID, "uniqueRefundNo": "YN-" + requestID,
		"status": status, "refundAmount": fmt.Sprintf("%d.%02d", amount/100, amount%100)})
	return string(b)
}

// 真实停机链路: 选退费 → 停机结束即部分退款 → 租期末(清除后)才写 settle_ready_at → 分账扣减供给方。
func TestSettleOutageRefundEndToEnd(t *testing.T) {
	e := newSettleEnv(t, true)
	ctx := context.Background()
	no := e.order.OrderNo

	e.nodeDown(13 * time.Minute) // > 2h × 10%
	e.s.OutageRound(ctx)
	_, err := e.s.SetOutageRemedy(21, no, "refund")
	require.NoError(t, err)
	e.recoverContainer(t)

	rows := e.outages(t)
	require.Len(t, rows, 1)
	o := rows[0]
	require.NotNil(t, o.RemedyAmount)
	refund := *o.RemedyAmount
	assert.InDelta(t, settlePaid*13*60/7200, refund, 100)
	assert.NotNil(t, o.RemedyAppliedAt, "退款成功即补偿落地")
	calls := e.fy.refundCalls()
	require.Len(t, calls, 1)
	assert.Equal(t, refund, calls[0].Amount)
	assert.Empty(t, calls[0].DivideBack, "资金仍在待分账账户, 不涉及分账归还")
	pr := e.refundRows(t, no)
	require.Len(t, pr, 1)
	assert.Equal(t, "success", pr[0].Status)
	assert.Equal(t, payment.RefundKindOutage, pr[0].Kind)

	// 租期内 / 宽限期内不进入分账。
	e.s.SettleRound(ctx)
	require.NoError(t, e.pay.RunJobs())
	assert.False(t, e.settleReady(t, e.order.ID))
	expireLease(t, e.provEnv)
	e.s.ExpiryRound(ctx)
	require.Equal(t, LeaseGrace, e.leaseNow(t).State)
	assert.Equal(t, "completed", orderStatus(t, e.provEnv))
	e.s.SettleRound(ctx)
	assert.False(t, e.settleReady(t, e.order.ID), "宽限期内可 restart 续租, 不写 settle_ready_at")
	require.NoError(t, e.pay.RunJobs())
	assert.Empty(t, e.fy.divideCalls())

	e.db.MustExec(`UPDATE leases SET grace_until=NOW()-INTERVAL 1 SECOND WHERE id=?`, e.lease.ID)
	e.s.ExpiryRound(ctx)
	e.s.SettleRound(ctx)
	require.True(t, e.settleReady(t, e.order.ID))
	require.NoError(t, e.pay.RunJobs())

	// 供给方责任: 按锁定比例从两行扣减。
	platDed := refund * settleFee / settlePaid
	st := e.settlements(t, no)
	assert.Equal(t, settleFee-platDed, st["platform"].Amount)
	assert.Equal(t, platDed, st["platform"].Deduction)
	assert.Equal(t, settlePaid-settleFee-(refund-platDed), st["supplier"].Amount)
	assert.Equal(t, refund-platDed, st["supplier"].Deduction)
	assert.Zero(t, st["supplier"].Compensation)
	assert.Equal(t, "success", st["supplier"].Status)
	divs := e.fy.divideCalls()
	require.Len(t, divs, 1)
	assert.Equal(t, st["supplier"].Amount, divs[0].Items[0].Amount)
	assert.Equal(t, "LEDGER-20", divs[0].Items[0].LedgerNo)
	e.assertInvariant(t, no, settlePaid)

	// 分账后不能再部分退款; 续租被拒。
	_, err = e.comp.GetRenewalQuote(21, no, 1)
	assert.ErrorIs(t, err, compute.ErrRenewalConflict)
	assert.Equal(t, errcode.Conflict, compute.ErrToCode(err))
}

// settle_ready 闸门: 未结束停机 / 退费通道未开 / 退款处理中(异步通知落地) / 待支付续租单。
func TestSettleReadyGating(t *testing.T) {
	e := newSettleEnv(t, false)
	ctx := context.Background()
	no := e.order.OrderNo
	e.fy.refundState = "PROCESSING"
	e.purge(t)

	// 未结束的停机。
	e.db.MustExec(`INSERT INTO lease_outages (lease_id, order_id, cause, liable, started_at, open_claim)
		VALUES (?, ?, 'node_offline', 'supplier', NOW()-INTERVAL 5 MINUTE, ?)`, e.lease.ID, e.order.ID, e.lease.ID)
	blocked, err := e.s.markSettleReady(e.order.ID)
	require.NoError(t, err)
	assert.Equal(t, "outage_open", blocked)
	e.db.MustExec(`DELETE FROM lease_outages WHERE lease_id=?`, e.lease.ID)

	// 待退费停机, 退费通道未开: 不分账并告警, 退款返回 50000。
	oid := e.endedOutage(t, e.order.ID, "supplier", 3000)
	blocked, err = e.s.markSettleReady(e.order.ID)
	require.NoError(t, err)
	assert.Equal(t, "outage_refund_unavailable", blocked)
	err = e.pay.RequestOutageRefund(ctx, oid)
	assert.Equal(t, errcode.InternalError, codeOf(err))
	assert.ErrorIs(t, err, payment.ErrPartialRefundDisabled)
	e.s.SettleRound(ctx)
	assert.False(t, e.settleReady(t, e.order.ID))
	assert.Empty(t, e.fy.refundCalls())

	// 打开通道: 结算任务兜底登记并提交; 易宝返回处理中 → 等待。
	e.enable(t)
	e.s.SettleRound(ctx)
	assert.False(t, e.settleReady(t, e.order.ID))
	pr := e.refundRows(t, no)
	require.Len(t, pr, 1)
	assert.Equal(t, "processing", pr[0].Status)
	blocked, err = e.s.markSettleReady(e.order.ID)
	require.NoError(t, err)
	assert.Empty(t, blocked, "退款处理中是正常等待")

	// 待支付的续租单(清除后不应出现, 这里直接落库模拟): renewal_open。
	res := e.db.MustExec(`INSERT INTO orders (order_no, buyer_id, product_id, quantity, duration, unit_price, total_amount,
		platform_fee, status, pricing_mode, fulfillment_mode, stock_reserved, compliance_agreed)
		VALUES ('REN-T106G', 21, ?, 2, 1, 1500, 3000, 0, 'pending_payment', 'hourly', 'managed', 0, 1)`, leaseProductID)
	ren, err := res.LastInsertId()
	require.NoError(t, err)
	e.db.MustExec(`INSERT INTO order_renewals (order_id, parent_order_id, request_id, mode, pricing_mode, lease_end_at)
		VALUES (?, ?, '00000000-0000-4000-8000-000000000106', 'restart', 'hourly', NOW())`, ren, e.order.ID)
	blocked, err = e.s.markSettleReady(e.order.ID)
	require.NoError(t, err)
	assert.Equal(t, "renewal_open", blocked)
	e.db.MustExec(`UPDATE orders SET status='cancelled' WHERE id=?`, ren)

	// 易宝异步通知退款成功: 按请求号路由到部分退款, 停机补偿落地 → 下一轮写 settle_ready_at。
	require.NoError(t, e.pay.HandleNotify(payment.NotifyRefund, refundNotify(no, pr[0].RequestID, "SUCCESS", 3000)))
	require.NoError(t, e.pay.HandleNotify(payment.NotifyRefund, refundNotify(no, pr[0].RequestID, "SUCCESS", 3000)), "重复通知幂等")
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM lease_outages WHERE id=? AND remedy_applied_at IS NOT NULL`, oid))
	e.s.SettleRound(ctx)
	require.True(t, e.settleReady(t, e.order.ID))
	ready1 := countRows(t, e.db, `SELECT COUNT(*) FROM orders WHERE id=? AND settle_ready_at IS NOT NULL`, e.order.ID)
	e.s.SettleRound(ctx)
	assert.Equal(t, ready1, countRows(t, e.db, `SELECT COUNT(*) FROM orders WHERE id=? AND settle_ready_at IS NOT NULL`, e.order.ID))

	require.NoError(t, e.pay.RunJobs())
	e.assertInvariant(t, no, settlePaid)
	assert.Len(t, e.fy.refundCalls(), 1)

	// 已进入分账: 不能再登记部分退款。
	late := e.endedOutage(t, e.order.ID, "supplier", 100)
	assert.Error(t, e.pay.RequestOutageRefund(ctx, late))
}

// 多笔部分退款 + 平台 / 供给方责任: 平台责任先从平台费扣, 超出部分从供给方扣并记平台补偿。
func TestSettleLiabilitySplitAcrossPartialRefunds(t *testing.T) {
	e := newSettleEnv(t, true)
	ctx := context.Background()
	no := e.order.OrderNo
	sup := e.endedOutage(t, e.order.ID, "supplier", 3000)
	p1 := e.endedOutage(t, e.order.ID, "platform", 5000)
	p2 := e.endedOutage(t, e.order.ID, "platform", 4000)
	for _, id := range []int64{sup, p1, p2} {
		require.NoError(t, e.pay.RequestOutageRefund(ctx, id))
	}
	assert.Len(t, e.fy.refundCalls(), 3)
	left, err := e.pay.RefundableRemainder(e.db, no)
	require.NoError(t, err)
	assert.EqualValues(t, settlePaid-12000, left)

	// 发票按实付 − 已成功部分退款开具; 有未成功的部分退款时暂不可开。
	billable, err := invoice.NewRepository(e.db).ListBillableOrders(21)
	require.NoError(t, err)
	require.Len(t, billable, 1)
	assert.EqualValues(t, settlePaid-12000, billable[0].TotalAmount)
	e.db.MustExec(`UPDATE payment_refunds SET status='failed' WHERE kind='outage_compensation' AND source_id=?`, p2)
	billable, err = invoice.NewRepository(e.db).ListBillableOrders(21)
	require.NoError(t, err)
	assert.Empty(t, billable)
	e.db.MustExec(`UPDATE payment_refunds SET status='success' WHERE kind='outage_compensation' AND source_id=?`, p2)

	e.purge(t)
	e.s.SettleRound(ctx)
	require.True(t, e.settleReady(t, e.order.ID))
	require.NoError(t, e.pay.RunJobs())

	// S=3000, P=9000: 平台先按比例分担 S 的 300, 余 6900 全部抵 P; P 超出 2100 由供给方承担并记平台补偿。
	st := e.settlements(t, no)
	assert.EqualValues(t, 0, st["platform"].Amount)
	assert.EqualValues(t, 7200, st["platform"].Deduction)
	assert.EqualValues(t, 60000, st["supplier"].Amount)
	assert.EqualValues(t, 4800, st["supplier"].Deduction)
	assert.EqualValues(t, 2100, st["supplier"].Compensation)
	e.assertInvariant(t, no, settlePaid)
	divs := e.fy.divideCalls()
	require.Len(t, divs, 1)
	assert.EqualValues(t, 60000, divs[0].Items[0].Amount)
}

// 同一停机段并发请求退款: 只登记一行、只调用一次易宝。
func TestPartialRefundIdempotentConcurrent(t *testing.T) {
	e := newSettleEnv(t, true)
	ctx := context.Background()
	e.fy.refundDelay = 200 * time.Millisecond
	oid := e.endedOutage(t, e.order.ID, "supplier", 2500)

	var wg sync.WaitGroup
	errs := make([]error, 8)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = e.pay.RequestOutageRefund(ctx, oid)
		}(i)
	}
	wg.Wait()
	for _, err := range errs {
		assert.NoError(t, err)
	}
	require.NoError(t, e.pay.RequestOutageRefund(ctx, oid), "已落地后再调幂等")
	e.s.SettleRound(ctx) // 兜底任务也不重复登记
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM payment_refunds WHERE kind='outage_compensation' AND source_id=?`, oid))
	assert.Len(t, e.fy.refundCalls(), 1)
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM lease_outages WHERE id=? AND remedy_applied_at IS NOT NULL`, oid))
}

// 失败换新请求号重试(1 小时退避) → 处理中 → 查询落地。
func TestPartialRefundFailedRetryThenQuery(t *testing.T) {
	e := newSettleEnv(t, true)
	ctx := context.Background()
	no := e.order.OrderNo
	e.fy.refundState = "FAILED"
	oid := e.endedOutage(t, e.order.ID, "supplier", 1800)
	require.NoError(t, e.pay.RequestOutageRefund(ctx, oid))
	pr := e.refundRows(t, no)
	require.Len(t, pr, 1)
	assert.Equal(t, "failed", pr[0].Status)
	first := pr[0].RequestID

	require.NoError(t, e.pay.RunJobs())
	assert.Len(t, e.fy.refundCalls(), 1, "退避期内不重试")

	e.fy.refundState, e.fy.queryState = "PROCESSING", "PROCESSING"
	e.db.MustExec(`UPDATE payment_refunds SET updated_at=NOW()-INTERVAL 2 HOUR WHERE id=?`, pr[0].ID)
	require.NoError(t, e.pay.RunJobs())
	pr = e.refundRows(t, no)
	assert.Equal(t, "processing", pr[0].Status)
	assert.NotEqual(t, first, pr[0].RequestID, "失败是终态, 重提换新请求号")
	calls := e.fy.refundCalls()
	require.Len(t, calls, 2)
	assert.Equal(t, pr[0].RequestID, calls[1].RequestID)

	e.fy.queryState = "SUCCESS"
	require.NoError(t, e.pay.RunJobs())
	pr = e.refundRows(t, no)
	assert.Equal(t, "success", pr[0].Status)
	assert.Len(t, e.fy.refundCalls(), 2, "处理中只查询不重提")
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM lease_outages WHERE id=? AND remedy_applied_at IS NOT NULL`, oid))
}

// 通道未打开 / 易宝未接入: 不发起部分退款(50000), 不伪造成功。
func TestPartialRefundDisabled(t *testing.T) {
	e := newSettleEnv(t, false)
	ctx := context.Background()
	oid := e.endedOutage(t, e.order.ID, "supplier", 1000)
	err := e.pay.RequestOutageRefund(ctx, oid)
	assert.ErrorIs(t, err, payment.ErrPartialRefundDisabled)
	assert.Equal(t, errcode.InternalError, codeOf(err))
	assert.False(t, e.pay.PartialRefundsEnabled())

	// fleet 未注入退费实现: 选退费 50000, 违约退款 50000。
	_, err = e.s.AdminSupplierBreachRefund(ctx, 1, e.order.OrderNo, "供给方长时间离线", "127.0.0.1")
	assert.Equal(t, errcode.InternalError, codeOf(err))

	unconfigured := newFakeYeepay()
	unconfigured.configured = false
	p2 := payment.NewService(payment.NewRepository(e.db), e.db, unconfigured)
	assert.ErrorIs(t, p2.EnablePartialRefunds(), payment.ErrYeepayNotConfigured)
	assert.False(t, p2.PartialRefundsEnabled())
	assert.Zero(t, countRows(t, e.db, `SELECT COUNT(*) FROM payment_refunds`))
	assert.Empty(t, e.fy.refundCalls())
}

// 续租单: 沿用根订单 settle_ready_at, 与根订单一同在租期末分账。
func TestSettleRenewalChildInheritsParent(t *testing.T) {
	e := newSettleEnv(t, true)
	ctx := context.Background()
	_, child := renewManaged(t, e.provEnv, 1)
	payRenewal(t, e.provEnv, child.OrderNo)
	childFee := child.PlatformFee
	seedPaidOrder(t, e.provEnv, child.OrderNo, child.TotalAmount, childFee)

	// 续租期内的停机属于续租单: 退款从续租单支付走。
	oid := e.endedOutage(t, child.ID, "supplier", 500)
	require.NoError(t, e.pay.RequestOutageRefund(ctx, oid))
	pr := e.refundRows(t, child.OrderNo)
	require.Len(t, pr, 1)
	assert.Equal(t, "success", pr[0].Status)

	require.NoError(t, e.pay.RunJobs())
	assert.Empty(t, e.fy.divideCalls(), "根订单未就绪, 续租单也不分账")

	// 续租后的整条链到期: 先到续租期末。
	e.db.MustExec(`UPDATE leases SET access_until=NOW()-INTERVAL 1 MINUTE WHERE id=?`, e.lease.ID)
	e.db.MustExec(`UPDATE orders SET lease_end_at=NOW()-INTERVAL 1 MINUTE WHERE id IN (?, ?)`, e.order.ID, child.ID)
	e.s.ExpiryRound(ctx)
	require.Equal(t, LeaseGrace, e.leaseNow(t).State)
	e.db.MustExec(`UPDATE leases SET grace_until=NOW()-INTERVAL 1 SECOND WHERE id=?`, e.lease.ID)
	e.s.ExpiryRound(ctx)
	e.s.SettleRound(ctx)
	require.True(t, e.settleReady(t, e.order.ID), "child=%s", orderStatusOf(t, e.db, child.OrderNo))
	assert.False(t, e.settleReady(t, child.ID), "续租单不单独写, 沿用根订单")

	require.NoError(t, e.pay.RunJobs())
	assert.Len(t, e.fy.divideCalls(), 2)
	e.assertInvariant(t, e.order.OrderNo, settlePaid)
	e.assertInvariant(t, child.OrderNo, child.TotalAmount)
	st := e.settlements(t, child.OrderNo)
	assert.Equal(t, "success", st["supplier"].Status)
	assert.EqualValues(t, 500, st["platform"].Deduction+st["supplier"].Deduction)
}

// 运营发起供给方违约退款: 门槛 / 截断访问 / 未使用部分退款 / 一租约一次 / 计入供给方责任。
func TestSupplierBreachRefund(t *testing.T) {
	e := newSettleEnv(t, true)
	ctx := context.Background()
	no := e.order.OrderNo

	_, err := e.s.AdminSupplierBreachRefund(ctx, 1, no, " ", "127.0.0.1")
	assert.Equal(t, errcode.ParamInvalid, codeOf(err))
	_, err = e.s.AdminSupplierBreachRefund(ctx, 1, no, "离线", "127.0.0.1")
	assert.Equal(t, errcode.Conflict, codeOf(err), "无供给方责任停机")

	// 租期还剩 1 小时; 进行中的供给方停机 15 分钟 > 12 分钟: 先结束并顺延, 再按剩余访问时长退款。
	end := time.Now().Add(time.Hour).Truncate(time.Second)
	e.db.MustExec(`UPDATE leases SET access_until=? WHERE id=?`, end, e.lease.ID)
	e.db.MustExec(`UPDATE orders SET lease_end_at=? WHERE id=?`, end, e.order.ID)
	e.db.MustExec(`UPDATE order_deliveries SET access_expires_at=? WHERE order_id=?`, end, e.order.ID)
	e.nodeDown(15 * time.Minute)
	e.s.OutageRound(ctx)
	before := e.times(t)
	out, err := e.s.AdminSupplierBreachRefund(ctx, 1, no, "供给方长时间离线", "127.0.0.1")
	require.NoError(t, err)
	assert.Equal(t, no, out.RefundOrderNo)
	remaining := before.AccessUntil.Add(15*time.Minute).Sub(time.Now()) / time.Second
	assert.InDelta(t, float64(settlePaid)*float64(remaining)/7200, out.Amount, 400)
	after := e.times(t)
	assert.WithinDuration(t, time.Now(), after.AccessUntil, 5*time.Second, "访问截断到当前时刻")
	assert.WithinDuration(t, time.Now(), after.LeaseEnd, 5*time.Second)
	pr := e.refundRows(t, no)
	require.Len(t, pr, 1)
	assert.Equal(t, payment.RefundKindSupplierBreach, pr[0].Kind)
	assert.Equal(t, "success", pr[0].Status)
	assert.Equal(t, out.Amount, pr[0].Amount)
	require.NotNil(t, pr[0].SourceID)
	assert.Equal(t, e.lease.ID, *pr[0].SourceID)
	assert.Equal(t, 1, countRows(t, e.db, `SELECT COUNT(*) FROM audit_logs WHERE action='lease.supplier_breach_refund' AND target_id=?`, e.lease.ID))

	_, err = e.s.AdminSupplierBreachRefund(ctx, 1, no, "再来一次", "127.0.0.1")
	assert.Equal(t, errcode.Conflict, codeOf(err))
	assert.Len(t, e.fy.refundCalls(), 1)

	// 到期 → 清除 → 分账: 违约退款全额计入供给方责任(按比例分摊)。
	e.db.MustExec(`UPDATE leases SET access_until=NOW()-INTERVAL 1 MINUTE WHERE id=?`, e.lease.ID)
	e.s.ExpiryRound(ctx)
	require.Equal(t, LeaseGrace, e.leaseNow(t).State)
	e.db.MustExec(`UPDATE leases SET grace_until=NOW()-INTERVAL 1 SECOND WHERE id=?`, e.lease.ID)
	e.s.ExpiryRound(ctx)
	e.s.SettleRound(ctx)
	require.True(t, e.settleReady(t, e.order.ID))
	require.NoError(t, e.pay.RunJobs())
	platDed := out.Amount * settleFee / settlePaid
	st := e.settlements(t, no)
	assert.Equal(t, settleFee-platDed, st["platform"].Amount)
	assert.Equal(t, settlePaid-settleFee-(out.Amount-platDed), st["supplier"].Amount)
	e.assertInvariant(t, no, settlePaid)
}

// 全额退款扣除已成功的部分退款, 未终结的部分退款作废。
func TestFullRefundNetsPartialRefunds(t *testing.T) {
	e := newSettleEnv(t, true)
	ctx := context.Background()
	no := e.order.OrderNo
	ok := e.endedOutage(t, e.order.ID, "supplier", 3000)
	require.NoError(t, e.pay.RequestOutageRefund(ctx, ok))
	e.fy.refundState = "FAILED"
	bad := e.endedOutage(t, e.order.ID, "supplier", 1000)
	require.NoError(t, e.pay.RequestOutageRefund(ctx, bad))

	e.db.MustExec(`UPDATE orders SET status='refunding' WHERE id=?`, e.order.ID)
	e.fy.refundState = "SUCCESS"
	_, err := e.pay.ExecuteRefund(1, no, "运营退款")
	require.NoError(t, err)
	calls := e.fy.refundCalls()
	require.Len(t, calls, 3)
	assert.EqualValues(t, settlePaid-3000, calls[2].Amount, "全额退款 = 实付 − 已成功部分退款")
	var st string
	require.NoError(t, e.db.Get(&st, `SELECT status FROM payment_refunds WHERE kind='outage_compensation' AND source_id=?`, bad))
	assert.Equal(t, "failed", st)
	require.NoError(t, e.pay.RunJobs())
	assert.Len(t, e.fy.refundCalls(), 3, "全额退款后不再重提部分退款")
}

// 部分退款合计等于实付: 无可分资金, 两行应结关闭, 不调用分账; 超出实付的退款被拒。
func TestSettleFullyRefundedByPartials(t *testing.T) {
	e := newSettleEnv(t, true)
	ctx := context.Background()
	no := e.order.OrderNo
	a := e.endedOutage(t, e.order.ID, "supplier", 50000)
	b := e.endedOutage(t, e.order.ID, "platform", settlePaid-50000)
	require.NoError(t, e.pay.RequestOutageRefund(ctx, a))
	require.NoError(t, e.pay.RequestOutageRefund(ctx, b))
	over := e.endedOutage(t, e.order.ID, "supplier", 1)
	assert.Error(t, e.pay.RequestOutageRefund(ctx, over), "累计不得超过实付")
	e.db.MustExec(`DELETE FROM lease_outages WHERE id=?`, over)

	e.purge(t)
	e.s.SettleRound(ctx)
	require.True(t, e.settleReady(t, e.order.ID))
	require.NoError(t, e.pay.RunJobs())
	st := e.settlements(t, no)
	assert.Equal(t, "cancelled", st["platform"].Status)
	assert.Equal(t, "cancelled", st["supplier"].Status)
	assert.Zero(t, st["platform"].Amount+st["supplier"].Amount)
	assert.Empty(t, e.fy.divideCalls())
	e.assertInvariant(t, no, settlePaid)
}
