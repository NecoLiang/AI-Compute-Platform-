package blockchain

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

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
	if _, err := NewService(nil, NewBSNClient(BSNConfig{}), "not-hex"); err == nil {
		t.Error("非法种子应报错")
	}
	if _, err := NewService(nil, NewBSNClient(BSNConfig{}), "abcd"); err == nil {
		t.Error("长度不足的种子应报错")
	}
	seed := strings.Repeat("ab", 32)
	s, err := NewService(nil, NewBSNClient(BSNConfig{}), seed)
	if err != nil {
		t.Fatalf("合法种子报错: %v", err)
	}
	// 签名可被对应公钥验证
	sig := ed25519.Sign(s.signKey, []byte("0xdeadbeef"))
	if !ed25519.Verify(s.signKey.Public().(ed25519.PublicKey), []byte("0xdeadbeef"), sig) {
		t.Error("Ed25519 签名验证失败")
	}
	s2, err := NewService(nil, NewBSNClient(BSNConfig{}), "")
	if err != nil {
		t.Fatalf("空种子应允许(暂不签名): %v", err)
	}
	if s2.signKey != nil {
		t.Error("空种子不应产生签名密钥")
	}
}

func TestBSNClient_ConfiguredAndTxURL(t *testing.T) {
	if NewBSNClient(BSNConfig{}).Configured() {
		t.Error("空配置不应视为已接入")
	}
	if NewBSNClient(BSNConfig{GatewayURL: "https://x", APIKey: "k"}).Configured() {
		t.Error("缺 contract_key 不应视为已接入")
	}
	b := NewBSNClient(BSNConfig{GatewayURL: "https://x", APIKey: "k", ContractKey: "c", ExplorerURL: "https://scan.example.com/tx/"})
	if !b.Configured() {
		t.Error("三要素齐备应视为已接入")
	}
	if got := b.TxURL("0xabc"); got != "https://scan.example.com/tx/0xabc" {
		t.Errorf("TxURL 拼接错误: %s", got)
	}
	if got := b.TxURL(""); got != "" {
		t.Errorf("空 txID 应返回空串: %s", got)
	}
}

func TestBSNClient_UploadHash(t *testing.T) {
	var gotAuth, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("x-api-key")
		var m map[string]string
		json.NewDecoder(r.Body).Decode(&m)
		gotBody = m["hash"]
		if r.URL.Path != "/api/v1/evidence" {
			w.WriteHeader(404)
			return
		}
		json.NewEncoder(w).Encode(map[string]any{"txId": "0xTX1"})
	}))
	defer srv.Close()

	b := NewBSNClient(BSNConfig{GatewayURL: srv.URL, APIKey: "key-1", ContractKey: "c1"})
	txID, err := b.UploadHash(context.Background(), "0xhash1")
	if err != nil {
		t.Fatalf("UploadHash 失败: %v", err)
	}
	if txID != "0xTX1" || gotAuth != "key-1" || gotBody != "0xhash1" {
		t.Errorf("请求/响应不符: txID=%s auth=%s hash=%s", txID, gotAuth, gotBody)
	}

	// 未配置时必须明确报错, 不得假装成功
	if _, err := NewBSNClient(BSNConfig{}).UploadHash(context.Background(), "0x1"); err != ErrBSNNotConfigured {
		t.Errorf("未配置应返回 ErrBSNNotConfigured, got %v", err)
	}
}

func TestBSNClient_UploadHash_GatewayError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(500)
		w.Write([]byte("boom"))
	}))
	defer srv.Close()
	b := NewBSNClient(BSNConfig{GatewayURL: srv.URL, APIKey: "k", ContractKey: "c"})
	if _, err := b.UploadHash(context.Background(), "0x1"); err == nil {
		t.Error("网关 500 应返回错误")
	}
}
