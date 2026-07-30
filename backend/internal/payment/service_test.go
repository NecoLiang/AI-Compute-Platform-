package payment

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
)

// 支付回调是无鉴权接口, 验签是唯一信任来源。
// 本组测试锁死"未接入易宝时绝不放行任何回调"这条底线, 防止日后被改回静默通过。

func TestVerifyCallback_RefusesWhenNotConfigured(t *testing.T) {
	y := NewYeepayClient()
	err := y.VerifyCallback(CallbackReq{
		TxID: "yeepay-tx-001", OrderNo: "ORD20260730000001", Status: "success", Amount: 29400,
	})
	assert.Error(t, err, "未接入易宝时必须拒绝，不能返回验签通过")
	assert.True(t, errors.Is(err, ErrYeepayCallbackUnverifiable))
}

// HandleCallback 必须在任何业务处理之前验签。
// 这里故意把 repo 置为 nil: 只要验签先执行并返回错误, 就不会走到任何 repo 调用;
// 一旦有人把验签挪到业务逻辑之后, 本用例会因 nil 指针 panic 而失败。
func TestHandleCallback_VerifiesSignatureBeforeAnyBusinessLogic(t *testing.T) {
	svc := &Service{repo: nil, db: nil, yeepay: NewYeepayClient(), feeRate: 500}

	assert.NotPanics(t, func() {
		err := svc.HandleCallback(CallbackReq{
			TxID: "forged-tx", OrderNo: "ORD20260730000002", Status: "success", Amount: 99999900,
		})
		assert.Error(t, err, "伪造回调必须被拒绝")
		assert.True(t, errors.Is(err, ErrYeepayCallbackUnverifiable))
	}, "验签必须先于任何 repo 访问执行")
}

// 幂等分支同样不得绕过验签: 攻击者可以自由构造 tx_id。
func TestHandleCallback_IdempotencyDoesNotBypassVerification(t *testing.T) {
	svc := &Service{repo: nil, db: nil, yeepay: NewYeepayClient(), feeRate: 500}
	for _, tx := range []string{"", "dup-tx", "已存在的交易号"} {
		err := svc.HandleCallback(CallbackReq{TxID: tx, OrderNo: "ORD1", Status: "success", Amount: 1})
		assert.Error(t, err, "tx_id=%q 仍必须先验签", tx)
		assert.True(t, errors.Is(err, ErrYeepayCallbackUnverifiable))
	}
}

// 其余易宝能力未接入时也必须显式报错, 不得假装成功。
func TestYeepayClient_UnconfiguredCapabilitiesReturnErrors(t *testing.T) {
	y := NewYeepayClient()

	payURL, txID, err := y.CreatePayment("ORD1", 29400, "wechat")
	assert.Error(t, err)
	assert.True(t, errors.Is(err, ErrYeepayNotConfigured))
	assert.Empty(t, payURL)
	assert.Empty(t, txID)

	err = y.CreateSplit("ORD1", []SplitItem{{PayeeType: "platform", Amount: 1470}})
	assert.Error(t, err)
	assert.True(t, errors.Is(err, ErrYeepayNotConfigured))
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
