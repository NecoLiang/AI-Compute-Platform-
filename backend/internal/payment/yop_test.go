package payment

import (
	"crypto"
	"crypto/aes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"
)

func testRSAKey(t *testing.T) (*rsa.PrivateKey, string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return key, base64.StdEncoding.EncodeToString(der)
}

func testPublicKey(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	der, err := x509.MarshalPKIXPublicKey(&key.PublicKey)
	if err != nil {
		t.Fatal(err)
	}
	return base64.StdEncoding.EncodeToString(der)
}

func rsaSign(t *testing.T, key *rsa.PrivateKey, content string) string {
	t.Helper()
	digest := sha256.Sum256([]byte(content))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, digest[:])
	if err != nil {
		t.Fatal(err)
	}
	return base64.RawURLEncoding.EncodeToString(sig)
}

// encryptTestNotify 按易宝回调格式加密: 商户公钥加密随机密钥, 平台私钥签名明文。
func encryptTestNotify(t *testing.T, merchant *rsa.PrivateKey, platform *rsa.PrivateKey, content string) string {
	t.Helper()
	randomKey := make([]byte, 16)
	if _, err := rand.Read(randomKey); err != nil {
		t.Fatal(err)
	}
	encKey, err := rsa.EncryptPKCS1v15(rand.Reader, &merchant.PublicKey, randomKey)
	if err != nil {
		t.Fatal(err)
	}
	plain := []byte(content + "$" + rsaSign(t, platform, content))
	pad := aes.BlockSize - len(plain)%aes.BlockSize
	for i := 0; i < pad; i++ {
		plain = append(plain, byte(pad))
	}
	block, _ := aes.NewCipher(randomKey)
	out := make([]byte, len(plain))
	for i := 0; i < len(plain); i += aes.BlockSize {
		block.Encrypt(out[i:i+aes.BlockSize], plain[i:i+aes.BlockSize])
	}
	return base64.RawURLEncoding.EncodeToString(encKey) + "$" + base64.RawURLEncoding.EncodeToString(out) + "$AES$SHA256"
}

// 官方 SDK 自带的回调样例: 实现必须能解出并验签通过, 证明解密/验签算法与易宝一致。
func TestDecryptNotify_OfficialSDKVector(t *testing.T) {
	raw, err := os.ReadFile("testdata/yop_sdk_callback_vector.json")
	if err != nil {
		t.Fatal(err)
	}
	var vector struct {
		Callback          string `json:"callback"`
		PlatformPublicKey string `json:"platform_public_key"`
		IsvPrivateKey     string `json:"isv_private_key"`
	}
	if err := json.Unmarshal(raw, &vector); err != nil {
		t.Fatal(err)
	}
	client, err := newYOPClient("app_test", "", vector.IsvPrivateKey, vector.PlatformPublicKey)
	if err != nil {
		t.Fatal(err)
	}
	content, err := client.decryptNotify(vector.Callback)
	if err != nil {
		t.Fatalf("SDK 样例回调解密失败: %v", err)
	}
	// SDK 样例的明文不是 JSON; 能解出且验签通过即证明算法一致。
	if string(content) != "a=123中文！@#¥（）*" {
		t.Fatalf("解密结果与 SDK 样例不符: %s", content)
	}

	// 同一密文换一个平台公钥必须验签失败 —— 解密成功不等于可信。
	other, _ := testRSAKey(t)
	client.publicKey = &other.PublicKey
	if _, err := client.decryptNotify(vector.Callback); err == nil {
		t.Fatal("平台公钥不匹配时必须拒绝回调")
	}
}

func TestDecryptNotify_RejectsForgedAndMalformed(t *testing.T) {
	merchant, merchantKey := testRSAKey(t)
	platform, _ := testRSAKey(t)
	attacker, _ := testRSAKey(t)
	client, err := newYOPClient("app_test", "", merchantKey, testPublicKey(t, platform))
	if err != nil {
		t.Fatal(err)
	}
	body := `{"orderId":"ORD1","status":"SUCCESS","orderAmount":"138.00"}`
	got, err := client.decryptNotify(encryptTestNotify(t, merchant, platform, body))
	if err != nil || string(got) != body {
		t.Fatalf("合法回调: %s %v", got, err)
	}
	// 攻击者知道商户公钥可以加密, 但没有易宝私钥无法签名。
	if _, err := client.decryptNotify(encryptTestNotify(t, merchant, attacker, body)); err == nil {
		t.Fatal("非易宝签名的回调必须拒绝")
	}
	for _, bad := range []string{"", "a$b", "x$y$AES$SHA256", "plain-json"} {
		if _, err := client.decryptNotify(bad); err == nil {
			t.Fatalf("畸形报文 %q 必须拒绝", bad)
		}
	}
}

// 请求签名: 用商户公钥按 YOP 规则重建规范请求串验签, 同时锁定表单的双重编码。
func TestBuildRequest_SignatureVerifiesAgainstCanonicalRequest(t *testing.T) {
	merchant, merchantKey := testRSAKey(t)
	client, err := newYOPClient("app_10000", "https://yop.example.test/yop-center", merchantKey, "")
	if err != nil {
		t.Fatal(err)
	}
	client.now = func() time.Time { return time.Date(2026, 10, 16, 8, 30, 0, 0, time.FixedZone("CST", 8*3600)) }
	params := map[string]string{"orderId": "ORD1", "goodsName": "A100 算力 x2", "orderAmount": "138.00"}
	req, err := client.buildRequest(http.MethodPost, "/rest/v1.0/cashier/unified/order", params, nil)
	if err != nil {
		t.Fatal(err)
	}
	auth := req.Header.Get("Authorization")
	prefix := "YOP-RSA2048-SHA256 yop-auth-v3/app_10000/2026-10-16T00:30:00Z/1800/x-yop-appkey;x-yop-content-sha256;x-yop-request-id/"
	if !strings.HasPrefix(auth, prefix) || !strings.HasSuffix(auth, "$SHA256") {
		t.Fatalf("Authorization 格式错误: %s", auth)
	}
	sig := strings.TrimSuffix(strings.TrimPrefix(auth, prefix), "$SHA256")
	canonicalParams := "goodsName=A100%20%E7%AE%97%E5%8A%9B%20x2&orderAmount=138.00&orderId=ORD1"
	if req.Header.Get("x-yop-content-sha256") != sha256Hex([]byte(canonicalParams)) {
		t.Fatal("表单请求的 content-sha256 必须是规范化参数串的摘要")
	}
	canonical := "yop-auth-v3/app_10000/2026-10-16T00:30:00Z/1800\nPOST\n/rest/v1.0/cashier/unified/order\n\n" +
		"x-yop-appkey:app_10000\nx-yop-content-sha256:" + req.Header.Get("x-yop-content-sha256") + "\nx-yop-request-id:" + req.Header.Get("x-yop-request-id")
	if !yopVerify(&merchant.PublicKey, canonical, sig) {
		t.Fatal("签名与规范请求串不匹配")
	}
	body, _ := io.ReadAll(req.Body)
	form, err := url.ParseQuery(string(body))
	if err != nil {
		t.Fatal(err)
	}
	if decoded, _ := url.QueryUnescape(form.Get("goodsName")); decoded != "A100 算力 x2" {
		t.Fatalf("表单值必须双重编码: %s", body)
	}
	if req.Header.Get("Content-Type") != yopFormContentType {
		t.Fatal(req.Header.Get("Content-Type"))
	}

	get, err := client.buildRequest(http.MethodGet, "/rest/v1.0/trade/order/query", map[string]string{"orderId": "ORD1", "merchantNo": "100"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if get.URL.RawQuery != "merchantNo=100&orderId=ORD1" || get.Header.Get("x-yop-content-sha256") != sha256Hex([]byte("merchantNo=100&orderId=ORD1")) {
		t.Fatalf("GET 查询串/摘要错误: %s", get.URL.RawQuery)
	}
}

func TestDo_VerifiesResponseSignature(t *testing.T) {
	_, merchantKey := testRSAKey(t)
	platform, _ := testRSAKey(t)
	body := "{\n  \"result\" : {\"code\":\"OPR00000\",\"status\":\"SUCCESS\"}\n}"
	var signature string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if signature != "" {
			w.Header().Set("x-yop-sign", signature)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	client, err := newYOPClient("app_test", server.URL, merchantKey, testPublicKey(t, platform))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Code   string `json:"code"`
		Status string `json:"status"`
	}
	signature = rsaSign(t, platform, yopSignWhitespace.ReplaceAllString(body, ""))
	if err := client.get("/rest/v1.0/trade/order/query", map[string]string{"orderId": "ORD1"}, &out); err != nil || out.Status != "SUCCESS" {
		t.Fatalf("合法响应: %+v %v", out, err)
	}
	signature = ""
	if err := client.get("/rest/v1.0/trade/order/query", nil, &out); err == nil {
		t.Fatal("无签名的成功响应必须拒绝")
	}
	attacker, _ := testRSAKey(t)
	signature = rsaSign(t, attacker, yopSignWhitespace.ReplaceAllString(body, ""))
	if err := client.get("/rest/v1.0/trade/order/query", nil, &out); err == nil {
		t.Fatal("签名不匹配的响应必须拒绝")
	}
}

// 报文内签名(易宝网关实际返回格式, 2026-10-16 联调确认): sign 覆盖去空白的 result 原文。
func TestDo_VerifiesBodySignature(t *testing.T) {
	_, merchantKey := testRSAKey(t)
	platform, _ := testRSAKey(t)
	attacker, _ := testRSAKey(t)
	result := "{\n    \"orderId\" : \"ORD1\",\n    \"code\" : \"OPR00000\",\n    \"status\" : \"SUCCESS\",\n    \"message\" : \"成功\"\n  }"
	var body string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer server.Close()
	client, err := newYOPClient("app_test", server.URL, merchantKey, testPublicKey(t, platform))
	if err != nil {
		t.Fatal(err)
	}
	signed := func(key *rsa.PrivateKey, result string) string {
		sig := base64.StdEncoding.EncodeToString(mustDecodeRawURL(t, rsaSign(t, key, yopSignWhitespace.ReplaceAllString(result, ""))))
		return "{\n  \"state\" : \"SUCCESS\",\n  \"result\" : " + result + ",\n  \"ts\" : 1792082965341,\n  \"sign\" : \"" + sig + "$SHA256\"\n}"
	}
	var out struct {
		Status string `json:"status"`
	}
	body = signed(platform, result)
	if err := client.get("/rest/v1.0/trade/order/query", nil, &out); err != nil || out.Status != "SUCCESS" {
		t.Fatalf("合法响应: %+v %v", out, err)
	}
	body = signed(attacker, result)
	if err := client.get("/rest/v1.0/trade/order/query", nil, &out); err == nil {
		t.Fatal("非易宝签名的响应必须拒绝")
	}
	tampered := signed(platform, result)
	body = strings.Replace(tampered, `"status" : "SUCCESS"`, `"status" : "FAIL"`, 1)
	if body == tampered {
		t.Fatal("篡改用例未生效")
	}
	if err := client.get("/rest/v1.0/trade/order/query", nil, &out); err == nil {
		t.Fatal("result 被篡改必须拒绝")
	}
	body = "{\"state\":\"SUCCESS\",\"result\":" + result + "}"
	if err := client.get("/rest/v1.0/trade/order/query", nil, &out); err == nil {
		t.Fatal("无签名的成功响应必须拒绝")
	}
	body = `{"state":"FAILURE","error":{"code":"40044","message":"业务处理失败","subCode":"isv.authentication.signature.invalid"}}`
	var yerr *YopError
	if err := client.get("/rest/v1.0/trade/order/query", nil, &out); !errors.As(err, &yerr) || yerr.Code != "40044" {
		t.Fatalf("网关错误应解析为 YopError: %v", err)
	}
}

func mustDecodeRawURL(t *testing.T, s string) []byte {
	t.Helper()
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// 真实联调(默认跳过): 用易宝测试/生产商户查询一笔不存在的订单, 只要网关不报签名错误即证明签名链路通。
//
//	YEEPAY_LIVE_APP_KEY=app_xxx YEEPAY_LIVE_PRIVATE_KEY=... YEEPAY_LIVE_PARENT_MERCHANT_NO=... YEEPAY_LIVE_MERCHANT_NO=... \
//	  go test ./internal/payment -run Live -v
func TestLiveYeepayOrderQuery(t *testing.T) {
	appKey, key := os.Getenv("YEEPAY_LIVE_APP_KEY"), os.Getenv("YEEPAY_LIVE_PRIVATE_KEY")
	parent, merchant := os.Getenv("YEEPAY_LIVE_PARENT_MERCHANT_NO"), os.Getenv("YEEPAY_LIVE_MERCHANT_NO")
	if appKey == "" || key == "" || parent == "" || merchant == "" {
		t.Skip("未设置 YEEPAY_LIVE_* 环境变量, 跳过易宝真实联调")
	}
	client, err := NewYeepayClient(YeepayConfig{
		AppKey: appKey, PrivateKey: key, ParentMerchantNo: parent, MerchantNo: merchant,
		NotifyBaseURL: "https://www.omnisline.com/api/v1", ReturnBaseURL: "https://www.omnisline.com",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 不存在的订单: 请求签名被网关接受且响应验签通过时, 应得到业务错误「订单不存在」(OPR12002)。
	result, err := client.QueryPayment("LIVECHECK" + time.Now().Format("20060102150405"))
	t.Logf("query: %+v err=%v", result, err)
	if code := bizCode(err); code != "OPR12002" {
		t.Fatalf("期望易宝业务应答 OPR12002, 实际: %v", err)
	}
}
