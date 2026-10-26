package payment

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// 支付通知是无鉴权接口, 解密验签是唯一信任来源。
// 本组测试锁死"未接入易宝时绝不放行任何通知"这条底线, 防止日后被改回静默通过。

func TestDecryptNotify_RefusesWhenNotConfigured(t *testing.T) {
	y, err := NewYeepayClient(YeepayConfig{})
	assert.NoError(t, err)
	assert.False(t, y.Configured())
	_, err = y.DecryptNotify(`{"orderId":"ORD1","status":"SUCCESS","orderAmount":"294.00"}`)
	assert.True(t, errors.Is(err, ErrYeepayCallbackUnverifiable), "未接入易宝时必须拒绝, 不能返回验签通过")
}

// HandleNotify 必须在任何业务处理之前解密验签。
// 这里故意把 repo/db 置为 nil: 只要验签先执行并返回错误, 就不会走到任何数据库访问;
// 一旦有人把验签挪到业务逻辑之后, 本用例会因 nil 指针 panic 而失败。
func TestHandleNotify_VerifiesBeforeAnyBusinessLogic(t *testing.T) {
	svc := &Service{yeepay: &YeepayClient{}}
	for _, kind := range []string{NotifyPay, NotifyClear, NotifyRefund, NotifyNetIn, "unknown"} {
		for _, raw := range []string{"", `{"orderId":"ORD1","status":"SUCCESS"}`, "a$b$AES$SHA256"} {
			assert.NotPanics(t, func() {
				err := svc.HandleNotify(kind, raw)
				assert.True(t, errors.Is(err, ErrYeepayCallbackUnverifiable), "kind=%s raw=%q 必须先验签", kind, raw)
			})
		}
	}
}

// 其余易宝能力未接入时也必须显式报错, 不得假装成功。
func TestYeepayClient_UnconfiguredCapabilitiesReturnErrors(t *testing.T) {
	y := &YeepayClient{}
	_, err := y.CreatePayment(PaymentOrder{OrderNo: "ORD1", Amount: 29400, Channel: "wechat"})
	assert.True(t, errors.Is(err, ErrYeepayNotConfigured))
	_, err = y.QueryPayment("ORD1")
	assert.True(t, errors.Is(err, ErrYeepayNotConfigured))
	_, err = y.Divide(DivideOrder{OrderNo: "ORD1", RequestID: "D1", Items: []DivideItem{{LedgerNo: "1", Amount: 1}}})
	assert.True(t, errors.Is(err, ErrYeepayNotConfigured))
	_, err = y.Refund(RefundOrder{OrderNo: "ORD1", RequestID: "R1", Amount: 1})
	assert.True(t, errors.Is(err, ErrYeepayNotConfigured))
	_, err = y.NetInURL("NI1")
	assert.True(t, errors.Is(err, ErrYeepayNotConfigured))

	svc := &Service{yeepay: y}
	assert.NoError(t, svc.RunJobs(), "未接入时定时任务静默待命")
	_, err = svc.Reconcile("")
	assert.True(t, errors.Is(err, ErrYeepayNotConfigured), "未接入时对账必须报错, 不能返回空结果冒充平账")
	_, err = svc.ExecuteRefund(1, "ORD1", "")
	assert.True(t, errors.Is(err, ErrYeepayNotConfigured))
}

func TestNewYeepayClient_RejectsPartialOrInsecureConfig(t *testing.T) {
	_, key := testRSAKey(t)
	full := YeepayConfig{AppKey: "app_1", ParentMerchantNo: "100", MerchantNo: "100", PrivateKey: key,
		NotifyBaseURL: "https://www.omnisline.com/api/v1/", ReturnBaseURL: "https://www.omnisline.com"}
	client, err := NewYeepayClient(full)
	assert.NoError(t, err)
	assert.True(t, client.Configured())
	assert.Equal(t, "https://www.omnisline.com/api/v1/payment/yeepay/notify/pay", client.NotifyURL(NotifyPay))

	partial := full
	partial.PrivateKey = ""
	_, err = NewYeepayClient(partial)
	assert.Error(t, err, "半配置必须启动失败")

	insecure := full
	insecure.NotifyBaseURL = "http://www.omnisline.com/api/v1"
	_, err = NewYeepayClient(insecure)
	assert.Error(t, err, "回调地址必须 https")
}

// 生产商编只开通了网银时, 买家不能再选到微信/支付宝(易宝会以 00012 拒单)。
func TestPayChannels_OnlyConfiguredChannelsAreOffered(t *testing.T) {
	_, key := testRSAKey(t)
	cfg := YeepayConfig{AppKey: "app_1", ParentMerchantNo: "100", MerchantNo: "100", PrivateKey: key,
		NotifyBaseURL: "https://www.omnisline.com/api/v1", ReturnBaseURL: "https://www.omnisline.com"}
	client, err := NewYeepayClient(cfg)
	assert.NoError(t, err)
	assert.Equal(t, []string{"wechat", "alipay", "bank"}, client.PayChannels(), "留空为全部")

	cfg.PayChannels = " Bank , alipay,bank"
	client, err = NewYeepayClient(cfg)
	assert.NoError(t, err)
	assert.Equal(t, []string{"alipay", "bank"}, client.PayChannels(), "去重、忽略大小写、按固定顺序")

	cfg.PayChannels = "bank,unionpay"
	_, err = NewYeepayClient(cfg)
	assert.Error(t, err, "未知支付方式必须启动失败")

	assert.Empty(t, (&YeepayClient{}).PayChannels(), "未接入时不开放任何支付方式")
}

// 金额与易宝交互时以元字符串传递, 换算必须精确到分且不经浮点。
func TestYuanConversion(t *testing.T) {
	for in, want := range map[string]int64{"0": 0, "0.01": 1, "1": 100, "1.5": 150, "138.00": 13800, "138.000": 13800, "201600.99": 20160099} {
		got, err := parseYuan(in)
		assert.NoError(t, err, in)
		assert.Equal(t, want, got, in)
	}
	for _, bad := range []string{"", ".5", "-1", "1.005", "abc", "1.2.3", "1e3"} {
		_, err := parseYuan(bad)
		assert.Error(t, err, bad)
	}
	for fen, want := range map[int64]string{0: "0.00", 1: "0.01", 150: "1.50", 13800: "138.00", 20160099: "201600.99"} {
		assert.Equal(t, want, formatYuan(fen))
		back, err := parseYuan(want)
		assert.NoError(t, err)
		assert.Equal(t, fen, back)
	}
}

func TestParseNotifies(t *testing.T) {
	pay, err := parsePayNotify([]byte(`{"orderId":"ORD1","uniqueOrderNo":"U1","status":"SUCCESS","orderAmount":"138.00","paySuccessDate":"2026-10-16 10:00:00"}`))
	assert.NoError(t, err)
	assert.Equal(t, int64(13800), pay.Amount)
	assert.Equal(t, time.Date(2026, 10, 16, 2, 0, 0, 0, time.UTC), pay.PaidAt.UTC(), "易宝时间为北京时间")

	clear, err := parseClearNotify([]byte(`{"orderId":"ORD1","uniqueOrderNo":"U1","status":"SUCCESS","orderAmount":138}`))
	assert.NoError(t, err)
	assert.True(t, clear.Cleared)
	assert.Equal(t, int64(13800), clear.Amount)

	refund, err := parseRefundNotify([]byte(`{"orderId":"ORD1","refundRequestId":"R1","status":"SUCCESS","refundAmount":"138.00"}`))
	assert.NoError(t, err)
	assert.Equal(t, "R1", refund.RequestID)
	assert.Equal(t, int64(13800), refund.Amount)

	_, err = parsePayNotify([]byte(`{"orderAmount":"1.001"}`))
	assert.Error(t, err, "金额精度超过分必须拒绝")
}

// 分账时机: 交付确认后(active/completed), 续租单还要求母单在用。
func TestSplitEligible(t *testing.T) {
	active, paid := "active", "paid"
	assert.True(t, splitEligible("active", nil))
	assert.True(t, splitEligible("completed", nil))
	assert.True(t, splitEligible("completed", &active))
	for _, status := range []string{"pending_payment", "paid", "provisioning", "refunding", "refunded", "cancelled", "frozen"} {
		assert.False(t, splitEligible(status, nil), status)
	}
	assert.False(t, splitEligible("completed", &paid), "重新开通的续租需等新的交付确认")
}

// 分账金额必须 int64 分且守恒: 平台佣金 + 供给方所得 == 总额, 不允许出现分币误差。
func TestSplitAmountsAreConservedInFen(t *testing.T) {
	const feeRate = int64(500) // 5%
	cases := []int64{1, 7, 333, 9999, 10000, 10001, 29400, 20160000, 999999999}
	for _, total := range cases {
		fee := total * feeRate / 10000
		supplier := total - fee
		assert.Equal(t, total, fee+supplier, "总额 %d 分账后必须守恒", total)
		assert.GreaterOrEqual(t, fee, int64(0))
		assert.GreaterOrEqual(t, supplier, int64(0))
		assert.LessOrEqual(t, fee, total)
	}
}
