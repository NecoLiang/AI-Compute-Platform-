package accessgw

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client 网关调用主服务内部接口。任何错误(网络、非 0 业务码、验签失败)都返回 error,
// 调用方据此 fail-closed(新连接一律拒绝)。
type Client struct {
	base   string // 例如 http://10.0.0.5:8091 (不带路径)
	secret []byte
	http   *http.Client
}

// NewClient baseURL 为主服务内部监听地址(scheme://host:port), secret 为 accessgw.hmac_secret(≥32 字节)。
func NewClient(baseURL string, secret []byte, timeout time.Duration) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || (u.Path != "" && u.Path != "/") {
		return nil, fmt.Errorf("主服务内部地址须为 http(s)://host:port, 不带路径: %q", baseURL)
	}
	if len(secret) < 32 {
		return nil, fmt.Errorf("hmac secret 至少 32 字节")
	}
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &Client{base: u.Scheme + "://" + u.Host, secret: secret, http: &http.Client{Timeout: timeout}}, nil
}

type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func (c *Client) do(ctx context.Context, method, uri string, in, out any) error {
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+uri, bytes.NewReader(body))
	if err != nil {
		return err
	}
	nonce, err := NewNonce()
	if err != nil {
		return err
	}
	ts := time.Now().Unix()
	req.Header.Set(HeaderTimestamp, strconv.FormatInt(ts, 10))
	req.Header.Set(HeaderNonce, nonce)
	req.Header.Set(HeaderSignature, Sign(c.secret, method, uri, ts, nonce, body))
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("主服务内部接口 %s 返回 HTTP %d", uri, resp.StatusCode)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("主服务内部接口 %s 响应无法解析: %w", uri, err)
	}
	if env.Code != 0 {
		return fmt.Errorf("主服务内部接口 %s 返回 %d: %s", uri, env.Code, env.Message)
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(env.Data, out); err != nil {
		return fmt.Errorf("主服务内部接口 %s data 无法解析: %w", uri, err)
	}
	return nil
}

func (c *Client) Auth(ctx context.Context, req AuthRequest) (*AuthResponse, error) {
	var out AuthResponse
	if err := c.do(ctx, http.MethodPost, PathAuth, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) Revocations(ctx context.Context, cursor string) (*RevocationsResponse, error) {
	var out RevocationsResponse
	if err := c.do(ctx, http.MethodGet, PathRevocations+"?cursor="+url.QueryEscape(cursor), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ReportSessions(ctx context.Context, events []SessionEvent) (*SessionsResponse, error) {
	var out SessionsResponse
	if err := c.do(ctx, http.MethodPost, PathSessions, SessionsRequest{Events: events}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (c *Client) ReportProbe(ctx context.Context, r ProbeResult) error {
	return c.do(ctx, http.MethodPost, PathProbeResult, r, nil)
}
