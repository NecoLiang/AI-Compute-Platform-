package payment

import (
	"bytes"
	"crypto"
	"crypto/aes"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

// YOP 开放平台协议(yop-auth-v3 / RSA2048-SHA256)的最小实现, 逐项对齐易宝官方
// yop-go-sdk v4.3.9 的签名、响应验签与回调解密, 不引入 SDK 本身(它依赖 logrus
// 全局日志并在请求日志里打印签名串)。

const (
	yopDefaultServerRoot = "https://openapi.yeepay.com/yop-center"
	// yopDefaultPlatformPublicKey 易宝平台 RSA2048 公钥(官方 SDK 内置值), 用于
	// 响应验签与回调验签。易宝轮换公钥时通过 yeepay.platform_public_key 覆盖。
	yopDefaultPlatformPublicKey = "MIIBIjANBgkqhkiG9w0BAQEFAAOCAQ8AMIIBCgKCAQEA6p0XWjscY+gsyqKRhw9MeLsEmhFdBRhT2emOck/F1Omw38ZWhJxh9kDfs5HzFJMrVozgU+SJFDONxs8UB0wMILKRmqfLcfClG9MyCNuJkkfm0HFQv1hRGdOvZPXj3Bckuwa7FrEXBRYUhK7vJ40afumspthmse6bs6mZxNn/mALZ2X07uznOrrc2rk41Y2HftduxZw6T4EmtWuN2x4CZ8gwSyPAW5ZzZJLQ6tZDojBK4GZTAGhnn3bg5bBsBlw2+FLkCQBuDsJVsFPiGh/b6K/+zGTvWyUcu+LUj2MejYQELDO3i2vQXVDk7lVi2/TcUYefvIcssnzsfCfjaorxsuwIDAQAB"
	yopAuthVersion              = "yop-auth-v3"
	yopAuthExpireSeconds        = "1800"
	yopFormContentType          = "application/x-www-form-urlencoded;charset=utf-8"
	yopJSONContentType          = "application/json"
	yopMaxResponseBytes         = 1 << 20
)

type yopClient struct {
	appKey     string
	serverRoot string
	privateKey *rsa.PrivateKey
	publicKey  *rsa.PublicKey
	http       *http.Client
	now        func() time.Time
}

func newYOPClient(appKey, serverRoot, privateKey, platformPublicKey string) (*yopClient, error) {
	priv, err := parseYOPPrivateKey(privateKey)
	if err != nil {
		return nil, fmt.Errorf("yeepay.private_key: %w", err)
	}
	if strings.TrimSpace(platformPublicKey) == "" {
		platformPublicKey = yopDefaultPlatformPublicKey
	}
	pub, err := parseYOPPublicKey(platformPublicKey)
	if err != nil {
		return nil, fmt.Errorf("yeepay.platform_public_key: %w", err)
	}
	if serverRoot == "" {
		serverRoot = yopDefaultServerRoot
	}
	return &yopClient{
		appKey:     appKey,
		serverRoot: strings.TrimRight(serverRoot, "/"),
		privateKey: priv,
		publicKey:  pub,
		http:       &http.Client{Timeout: 15 * time.Second},
		now:        time.Now,
	}, nil
}

// YopError 易宝网关层错误(非 2xx), 例如签名错误、参数缺失、无接口权限。
type YopError struct {
	Status     int
	Code       string `json:"code"`
	Message    string `json:"message"`
	SubCode    string `json:"subCode"`
	SubMessage string `json:"subMessage"`
}

func (e *YopError) Error() string {
	return fmt.Sprintf("易宝网关错误 http=%d code=%s %s (%s %s)", e.Status, e.Code, e.Message, e.SubCode, e.SubMessage)
}

// postForm / get / postJSON 发起签名请求, 验证响应签名后把 result 解码进 out。
func (c *yopClient) postForm(path string, params map[string]string, out any) error {
	return c.do(http.MethodPost, path, params, nil, out)
}

func (c *yopClient) get(path string, params map[string]string, out any) error {
	return c.do(http.MethodGet, path, params, nil, out)
}

func (c *yopClient) postJSON(path string, payload any, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return c.do(http.MethodPost, path, nil, body, out)
}

func (c *yopClient) do(method, path string, params map[string]string, jsonBody []byte, out any) error {
	req, err := c.buildRequest(method, path, params, jsonBody)
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("易宝请求失败: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, yopMaxResponseBytes))
	if err != nil {
		return fmt.Errorf("易宝响应读取失败: %w", err)
	}
	if sign := resp.Header.Get("x-yop-sign"); sign != "" {
		if !c.verifyResponse(body, sign) {
			return fmt.Errorf("易宝响应验签失败")
		}
	} else if resp.StatusCode/100 == 2 {
		// 无签名头时为报文内签名: {"state","result","ts","sign"}, sign 覆盖去空白后的 result 原文。
		// 成功响应必须带签名: 支付状态、分账、退款结果都以响应为准, 不能信任无签名的报文。
		return c.decodeBodySigned(body, out)
	}
	if resp.StatusCode/100 != 2 || resp.StatusCode == http.StatusNoContent {
		e := &YopError{Status: resp.StatusCode}
		_ = json.Unmarshal(body, e)
		return e
	}
	var envelope struct {
		Result json.RawMessage `json:"result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil || len(envelope.Result) == 0 {
		return fmt.Errorf("易宝响应格式异常")
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(envelope.Result, out)
}

func (c *yopClient) decodeBodySigned(body []byte, out any) error {
	var envelope struct {
		State  string          `json:"state"`
		Result json.RawMessage `json:"result"`
		Error  *YopError       `json:"error"`
		Sign   string          `json:"sign"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return fmt.Errorf("易宝响应格式异常")
	}
	if envelope.State != "SUCCESS" {
		if envelope.Error != nil {
			envelope.Error.Status = http.StatusOK
			return envelope.Error
		}
		return fmt.Errorf("易宝响应状态异常: %s", envelope.State)
	}
	if len(envelope.Result) == 0 {
		return fmt.Errorf("易宝响应格式异常")
	}
	sign := strings.TrimSuffix(envelope.Sign, "$SHA256")
	if sign == "" {
		return fmt.Errorf("易宝响应缺少签名")
	}
	// json.RawMessage 保留 result 的原始字节, 与易宝签名时的原文一致。
	if !yopVerify(c.publicKey, yopSignWhitespace.ReplaceAllString(string(envelope.Result), ""), sign) {
		return fmt.Errorf("易宝响应验签失败")
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(envelope.Result, out)
}

func (c *yopClient) buildRequest(method, path string, params map[string]string, jsonBody []byte) (*http.Request, error) {
	requestID := uuid.NewString()
	authString := yopAuthVersion + "/" + c.appKey + "/" + c.now().UTC().Format("2006-01-02T15:04:05Z") + "/" + yopAuthExpireSeconds
	canonicalParams := yopCanonicalQuery(params)

	formPost := method == http.MethodPost && jsonBody == nil
	var contentHash, canonicalQuery string
	if formPost {
		contentHash, canonicalQuery = sha256Hex([]byte(canonicalParams)), ""
	} else if jsonBody != nil {
		contentHash, canonicalQuery = sha256Hex(jsonBody), canonicalParams
	} else {
		contentHash, canonicalQuery = sha256Hex([]byte(canonicalParams)), canonicalParams
	}

	headers := map[string]string{
		"x-yop-appkey":         c.appKey,
		"x-yop-content-sha256": contentHash,
		"x-yop-request-id":     requestID,
	}
	signedHeaders := []string{"x-yop-appkey", "x-yop-content-sha256", "x-yop-request-id"}
	headerLines := make([]string, 0, len(signedHeaders))
	for _, name := range signedHeaders {
		headerLines = append(headerLines, yopNormalize(name)+":"+yopNormalize(headers[name]))
	}
	sort.Strings(headerLines)
	canonicalRequest := authString + "\n" + method + "\n" + yopNormalizePath(path) + "\n" + canonicalQuery + "\n" + strings.Join(headerLines, "\n")

	digest := sha256.Sum256([]byte(canonicalRequest))
	sig, err := rsa.SignPKCS1v15(rand.Reader, c.privateKey, crypto.SHA256, digest[:])
	if err != nil {
		return nil, err
	}
	authorization := "YOP-RSA2048-SHA256 " + authString + "/" + strings.Join(signedHeaders, ";") + "/" + base64.RawURLEncoding.EncodeToString(sig) + "$SHA256"

	target := c.serverRoot + path
	var body io.Reader
	contentType := yopFormContentType
	switch {
	case formPost:
		// 与官方 SDK 一致: 表单值先 QueryEscape 一次再整体编码(易宝网关按此双重解码)。
		form := url.Values{}
		for k, v := range params {
			form.Set(k, url.QueryEscape(v))
		}
		body = strings.NewReader(form.Encode())
	case jsonBody != nil:
		body, contentType = bytes.NewReader(jsonBody), yopJSONContentType
		if q := yopEncodedQuery(params); q != "" {
			target += "?" + q
		}
	default:
		if q := yopEncodedQuery(params); q != "" {
			target += "?" + q
		}
	}
	req, err := http.NewRequest(method, target, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", authorization)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	return req, nil
}

var yopSignWhitespace = regexp.MustCompile("[ \t\n]")

// verifyResponse 响应验签: 去掉空格/制表/换行后的响应体 + x-yop-sign。
func (c *yopClient) verifyResponse(body []byte, signature string) bool {
	return yopVerify(c.publicKey, yopSignWhitespace.ReplaceAllString(string(body), ""), signature)
}

// decryptNotify 解密并验签易宝回调的 response 参数。
// 密文结构: RSA(随机密钥)$AES-ECB(明文$签名)$AES$SHA256。返回验签通过的 JSON 明文;
// 回调接口无鉴权, 这里是它唯一的信任来源。
func (c *yopClient) decryptNotify(cipherText string) ([]byte, error) {
	parts := strings.Split(strings.TrimSpace(cipherText), "$")
	if len(parts) != 4 || parts[2] != "AES" || parts[3] != "SHA256" {
		return nil, fmt.Errorf("回调报文格式无效")
	}
	encryptedKey, err := yopBase64Decode(parts[0])
	if err != nil {
		return nil, fmt.Errorf("回调密钥解码失败")
	}
	randomKey, err := rsa.DecryptPKCS1v15(rand.Reader, c.privateKey, encryptedKey)
	if err != nil {
		return nil, fmt.Errorf("回调密钥解密失败")
	}
	data, err := yopBase64Decode(parts[1])
	if err != nil {
		return nil, fmt.Errorf("回调报文解码失败")
	}
	plain, err := aesECBDecrypt(data, yopAESKey(randomKey))
	if err != nil {
		return nil, err
	}
	sep := strings.LastIndex(string(plain), "$")
	if sep < 0 {
		return nil, fmt.Errorf("回调报文缺少签名")
	}
	content, signature := string(plain[:sep]), strings.TrimSpace(string(plain[sep+1:]))
	if !yopVerify(c.publicKey, content, signature) {
		return nil, ErrYeepayCallbackUnverifiable
	}
	return []byte(content), nil
}

func yopVerify(pub *rsa.PublicKey, content, signature string) bool {
	var sig []byte
	var err error
	if strings.ContainsAny(signature, "*+=/") {
		sig, err = base64.StdEncoding.DecodeString(signature)
	} else {
		sig, err = base64.RawURLEncoding.DecodeString(signature)
	}
	if err != nil || len(sig) == 0 {
		return false
	}
	digest := sha256.Sum256([]byte(content))
	return rsa.VerifyPKCS1v15(pub, crypto.SHA256, digest[:], sig) == nil
}

// yopAESKey 易宝随机密钥折叠为 16 字节 AES-128 密钥(超出部分按位异或回前 16 字节)。
func yopAESKey(key []byte) []byte {
	out := make([]byte, 16)
	copy(out, key)
	for i := 16; i < len(key); {
		for j := 0; j < 16 && i < len(key); j, i = j+1, i+1 {
			out[j] ^= key[i]
		}
	}
	return out
}

func aesECBDecrypt(data, key []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	size := block.BlockSize()
	if len(data) == 0 || len(data)%size != 0 {
		return nil, fmt.Errorf("回调报文长度无效")
	}
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += size {
		block.Decrypt(out[i:i+size], data[i:i+size])
	}
	pad := int(out[len(out)-1])
	if pad == 0 || pad > size || pad > len(out) {
		return nil, fmt.Errorf("回调报文解密失败")
	}
	for _, b := range out[len(out)-pad:] {
		if int(b) != pad {
			return nil, fmt.Errorf("回调报文解密失败")
		}
	}
	return out[:len(out)-pad], nil
}

// yopBase64Decode 兼容易宝的 URL-safe 无填充 base64。
func yopBase64Decode(s string) ([]byte, error) {
	s = strings.TrimRight(strings.NewReplacer("-", "+", "_", "/").Replace(s), "=")
	return base64.RawStdEncoding.DecodeString(s)
}

func yopNormalize(v string) string {
	return strings.ReplaceAll(url.QueryEscape(v), "+", "%20")
}

func yopNormalizePath(path string) string {
	if path == "" {
		return "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return strings.ReplaceAll(yopNormalize(path), "%2F", "/")
}

// yopCanonicalQuery 参与签名的规范化参数串: k=v 各自规范化编码后按字典序以 & 连接。
func yopCanonicalQuery(params map[string]string) string {
	pairs := make([]string, 0, len(params))
	for k, v := range params {
		pairs = append(pairs, yopNormalize(k)+"="+yopNormalize(v))
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

// yopEncodedQuery GET 请求实际发送的查询串(与官方 SDK 一致对值做两次规范化编码)。
func yopEncodedQuery(params map[string]string) string {
	pairs := make([]string, 0, len(params))
	for k, v := range params {
		pairs = append(pairs, yopNormalize(k)+"="+yopNormalize(yopNormalize(v)))
	}
	sort.Strings(pairs)
	return strings.Join(pairs, "&")
}

func sha256Hex(b []byte) string {
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum)
}

// parseYOPPrivateKey 接受易宝后台导出的裸 base64(PKCS#8)或 PEM。
func parseYOPPrivateKey(raw string) (*rsa.PrivateKey, error) {
	der, err := yopKeyDER(raw)
	if err != nil {
		return nil, err
	}
	if key, err := x509.ParsePKCS8PrivateKey(der); err == nil {
		if rsaKey, ok := key.(*rsa.PrivateKey); ok {
			return rsaKey, nil
		}
		return nil, errors.New("不是 RSA 私钥")
	}
	if key, err := x509.ParsePKCS1PrivateKey(der); err == nil {
		return key, nil
	}
	return nil, errors.New("无法解析 RSA 私钥(需 PKCS#8 或 PKCS#1)")
}

func parseYOPPublicKey(raw string) (*rsa.PublicKey, error) {
	der, err := yopKeyDER(raw)
	if err != nil {
		return nil, err
	}
	key, err := x509.ParsePKIXPublicKey(der)
	if err != nil {
		return nil, err
	}
	rsaKey, ok := key.(*rsa.PublicKey)
	if !ok {
		return nil, errors.New("不是 RSA 公钥")
	}
	return rsaKey, nil
}

func yopKeyDER(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if strings.Contains(raw, "-----BEGIN") {
		block, _ := pem.Decode([]byte(raw))
		if block == nil {
			return nil, errors.New("PEM 格式无效")
		}
		return block.Bytes, nil
	}
	raw = strings.Join(strings.Fields(raw), "")
	der, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		return nil, errors.New("密钥 base64 无效")
	}
	return der, nil
}
