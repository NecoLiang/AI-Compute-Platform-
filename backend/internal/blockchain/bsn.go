package blockchain

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ErrBSNNotConfigured 未配齐 BSN 接入三要素时的明确错误, 不做任何降级假装上链。
var ErrBSNNotConfigured = fmt.Errorf("BSN-DDC 区块链未接入: 需配置 blockchain.gateway_url + api_key + contract_key (获取方式见 docs/14 §14.8)")

// BSNConfig BSN-DDC 文昌链接入配置 (docs/14 §14.8)。
// 三项核心配置在 bsnbase.com 完成企业实名认证、开通标准存证合约后获得。
type BSNConfig struct {
	GatewayURL  string // 用户网关地址
	APIKey      string // 开放平台 API Key
	ContractKey string // 存证合约标识(官方标准存证合约模板)
	ExplorerURL string // 区块链浏览器 tx 查询前缀, 供前端"自行查验"链接
}

// BSNClient 调 BSN 开放联盟链 REST 网关做存证与查询。
//
// ⚠️ 联调前的已知未知: 下面两个 endpoint 路径与请求/响应字段按 BSN 通用存证网关的
// 形态编写, 拿到 API Key 后须对照 https://ddc.bsnbase.com 的实际接口文档核对调整——
// 改动应只落在 UploadHash/VerifyHash 的请求构造与解析两处, 不影响其余链路。
type BSNClient struct {
	cfg  BSNConfig
	http *http.Client
}

func NewBSNClient(cfg BSNConfig) *BSNClient {
	return &BSNClient{cfg: cfg, http: &http.Client{Timeout: 15 * time.Second}}
}

// Configured 三要素齐备才算接入。未接入时 worker 空转等待, 存证以 pending 积累。
func (b *BSNClient) Configured() bool {
	return b != nil && b.cfg.GatewayURL != "" && b.cfg.APIKey != "" && b.cfg.ContractKey != ""
}

// TxURL 拼区块链浏览器链接。未配置浏览器地址时返回空串, 前端隐藏"自行查验"入口。
func (b *BSNClient) TxURL(txID string) string {
	if b == nil || b.cfg.ExplorerURL == "" || txID == "" {
		return ""
	}
	return strings.TrimSuffix(b.cfg.ExplorerURL, "/") + "/" + url.PathEscape(txID)
}

// UploadHash 把 hash 写入存证合约, 返回链上交易 ID (REQ-H-022)。
func (b *BSNClient) UploadHash(ctx context.Context, hash string) (string, error) {
	if !b.Configured() {
		return "", ErrBSNNotConfigured
	}
	reqBody, _ := json.Marshal(map[string]string{
		"contractKey": b.cfg.ContractKey,
		"hash":        hash,
	})
	var resp struct {
		TxID    string `json:"txId"`
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := b.post(ctx, "/api/v1/evidence", reqBody, &resp); err != nil {
		return "", err
	}
	if resp.TxID == "" {
		return "", fmt.Errorf("BSN 存证未返回 txId: code=%d message=%s", resp.Code, resp.Message)
	}
	return resp.TxID, nil
}

// VerifyHash 查链上是否存在该 hash 的存证, 返回 (是否存在, 交易ID)。
func (b *BSNClient) VerifyHash(ctx context.Context, hash string) (bool, string, error) {
	if !b.Configured() {
		return false, "", ErrBSNNotConfigured
	}
	reqBody, _ := json.Marshal(map[string]string{
		"contractKey": b.cfg.ContractKey,
		"hash":        hash,
	})
	var resp struct {
		Exists bool   `json:"exists"`
		TxID   string `json:"txId"`
	}
	if err := b.post(ctx, "/api/v1/evidence/query", reqBody, &resp); err != nil {
		return false, "", err
	}
	return resp.Exists, resp.TxID, nil
}

func (b *BSNClient) post(ctx context.Context, path string, body []byte, out any) error {
	u := strings.TrimSuffix(b.cfg.GatewayURL, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", b.cfg.APIKey)
	res, err := b.http.Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return fmt.Errorf("BSN 网关返回 %d: %s", res.StatusCode, string(raw))
	}
	return json.Unmarshal(raw, out)
}
