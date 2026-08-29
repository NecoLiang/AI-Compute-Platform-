package blockchain

import (
	"context"
	"crypto/ed25519"
	"strings"
	"testing"
)

// testAccountKey 测试用 secp256k1 私钥(标量 1), 仅用于单元测试。
const testAccountKey = "0000000000000000000000000000000000000000000000000000000000000001"

func testCfg(gateway string) BSNConfig {
	return BSNConfig{GatewayURL: gateway, ProjectID: "pid-test", AccountKey: testAccountKey,
		ExplorerURL: "https://scan.test/tx/"}
}

func mustClient(t *testing.T, cfg BSNConfig) *BSNClient {
	t.Helper()
	c, err := NewBSNClient(cfg)
	if err != nil {
		t.Fatalf("NewBSNClient: %v", err)
	}
	return c
}

// TestComputeHash_Golden 锁定载荷序列化契约: 这个 hash 一旦变化, 说明有人改了
// 载荷结构体的字段名/顺序/类型 —— 历史存证将无法重算, 必须阻止 (见 service.go 契约注释)。
func TestComputeHash_Golden(t *testing.T) {
	p := OrderPayload{
		OrderNo:        "ORD20260825120000abc123",
		BuyerIDHash:    HashID(42),
		SupplierIDHash: HashID(7),
		Spec:           "product:3 qty:2 duration:24 unit_price:150000",
		TotalAmountFen: 7200000,
		PlacedAt:       "2026-08-25T04:00:00Z",
	}
	got := ComputeHash(p)
	const want = "0x1c4cb62ee24faffcf2282f50a270e200a1483e3541dab4bae83b8cd81b70ccc9"
	if got != want {
		t.Errorf("OrderPayload 序列化契约被破坏:\n got=%s\nwant=%s\n若为有意变更, 需评估历史存证兼容性后更新本金标", got, want)
	}
	if again := ComputeHash(p); again != got {
		t.Errorf("同一载荷两次 hash 不一致: %s vs %s", got, again)
	}
	p.TotalAmountFen++
	if ComputeHash(p) == got {
		t.Error("载荷变化后 hash 未变化")
	}
}

func TestHashID_StableAndOpaque(t *testing.T) {
	if HashID(1) != HashID(1) {
		t.Error("HashID 不稳定")
	}
	if HashID(1) == HashID(2) {
		t.Error("不同 ID 摘要碰撞")
	}
	if strings.Contains(HashID(123456), "123456") {
		t.Error("摘要泄露了明文 ID")
	}
}

func TestNewService_SignSeed(t *testing.T) {
	empty := mustClient(t, BSNConfig{})
	if _, err := NewService(nil, empty, "not-hex"); err == nil {
		t.Error("非法种子应报错")
	}
	if _, err := NewService(nil, empty, "abcd"); err == nil {
		t.Error("长度不足的种子应报错")
	}
	seed := strings.Repeat("ab", 32)
	s, err := NewService(nil, empty, seed)
	if err != nil {
		t.Fatalf("合法种子报错: %v", err)
	}
	sig := ed25519.Sign(s.signKey, []byte("0xdeadbeef"))
	if !ed25519.Verify(s.signKey.Public().(ed25519.PublicKey), []byte("0xdeadbeef"), sig) {
		t.Error("Ed25519 签名验证失败")
	}
	s2, err := NewService(nil, empty, "")
	if err != nil {
		t.Fatalf("空种子应允许(暂不签名): %v", err)
	}
	if s2.signKey != nil {
		t.Error("空种子不应产生签名密钥")
	}
}

func TestBSNClient_ConfiguredAndAddress(t *testing.T) {
	if mustClient(t, BSNConfig{}).Configured() {
		t.Error("空配置不应视为已接入")
	}
	if mustClient(t, BSNConfig{GatewayURL: "https://x", ProjectID: "p"}).Configured() {
		t.Error("缺链账户私钥不应视为已接入")
	}
	if _, err := NewBSNClient(BSNConfig{AccountKey: "not-hex"}); err == nil {
		t.Error("非法私钥应报错")
	}

	c := mustClient(t, testCfg("https://x"))
	if !c.Configured() {
		t.Error("三要素齐备应视为已接入")
	}
	// 地址推导金标: secp256k1 标量 1 的压缩公钥 → sha256 → ripemd160 → bech32(iaa)。
	// 数据段与 BIP-173 标准向量(同公钥的 hash160)一致, 且已被文昌链节点 bech32 解码
	// 接受(返回 account not found 而非解码错误)。该值变化说明地址推导被改坏。
	const wantAddr = "iaa1w508d6qejxtdg4y5r3zarvary0c5xw7k0lhtdf"
	if c.Address() != wantAddr {
		t.Errorf("链账户地址推导变化:\n got=%s\nwant=%s", c.Address(), wantAddr)
	}
	if got := c.TxURL("ABCDEF"); got != "https://scan.test/tx/ABCDEF" {
		t.Errorf("TxURL 拼接错误: %s", got)
	}
	if got := c.TxURL(""); got != "" {
		t.Errorf("空 txID 应返回空串: %s", got)
	}
}

// TestBSNClient_UploadThenVerify 用 fake Tendermint 网关做「编码 → 广播 → 按 hash
// 查回 → 解码比对」的全链路往返: fake 端原样保存 TxRaw, VerifyHash 解码的是我们
// 自己编码的字节 —— protobuf 编/解码任何一侧出错测试都会失败。
func TestBSNClient_UploadThenVerify(t *testing.T) {
	fake := newFakeChain(t)
	c := mustClient(t, testCfg(fake.URL()))

	const hash = "0x1c4cb62ee24faffcf2282f50a270e200a1483e3541dab4bae83b8cd81b70ccc9"
	txID, err := c.UploadHash(context.Background(), hash)
	if err != nil {
		t.Fatalf("UploadHash: %v", err)
	}
	if txID == "" {
		t.Fatal("未返回交易 hash")
	}
	if fake.lastPath != "/api/pid-test/rpc" {
		t.Errorf("网关路径错误: %s", fake.lastPath)
	}

	ok, _, err := c.VerifyHash(context.Background(), txID, hash)
	if err != nil || !ok {
		t.Fatalf("回查比对应通过: ok=%v err=%v", ok, err)
	}
	ok, _, err = c.VerifyHash(context.Background(), txID, "0xanother")
	if err != nil || ok {
		t.Fatalf("digest 不同应比对失败: ok=%v err=%v", ok, err)
	}

	// 未配置时必须明确报错, 不得假装成功
	if _, err := mustClient(t, BSNConfig{}).UploadHash(context.Background(), "0x1"); err != ErrBSNNotConfigured {
		t.Errorf("未配置应返回 ErrBSNNotConfigured, got %v", err)
	}
}

func TestBSNClient_GatewayError(t *testing.T) {
	fake := newFakeChain(t)
	fake.fail.Store(true)
	c := mustClient(t, testCfg(fake.URL()))
	if _, err := c.UploadHash(context.Background(), "0x1"); err == nil {
		t.Error("网关 500 应返回错误")
	}
}

func TestProtobufWireRoundtrip(t *testing.T) {
	msg := append(append(pbString(1, "hello"), pbVarint(3, 42)...), pbBytes(2, []byte{0xff, 0x00})...)
	if v, ok := pbField(msg, 1); !ok || string(v) != "hello" {
		t.Errorf("pbField(1) = %q, %v", v, ok)
	}
	if v, ok := pbField(msg, 2); !ok || len(v) != 2 {
		t.Errorf("pbField(2) = %v, %v", v, ok)
	}
	if v, ok := pbVarintField(msg, 3); !ok || v != 42 {
		t.Errorf("pbVarintField(3) = %d, %v", v, ok)
	}
	if _, ok := pbField(msg, 9); ok {
		t.Error("不存在的字段不应命中")
	}
}
