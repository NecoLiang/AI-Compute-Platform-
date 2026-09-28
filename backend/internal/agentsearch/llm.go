package agentsearch

// LLM 客户端: OpenAI 兼容的 /chat/completions 接口, 配置驱动 —— 指向平台自有的模型
// 网关即可(base_url + api_key + model)。未配置时返回明确错误, 不做规则引擎降级
// 假装智能(项目「不得虚假完成」原则)。

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var ErrAINotConfigured = fmt.Errorf("智能搜索未接入: 需配置 ai.base_url + ai.api_key + ai.model (OpenAI 兼容网关)")

type LLMConfig struct {
	BaseURL        string
	APIKey         string
	Model          string
	TimeoutSeconds int
}

type LLMClient struct {
	cfg  LLMConfig
	http *http.Client
}

func NewLLMClient(cfg LLMConfig) *LLMClient {
	if cfg.TimeoutSeconds <= 0 {
		cfg.TimeoutSeconds = 30
	}
	return &LLMClient{cfg: cfg, http: &http.Client{Timeout: time.Duration(cfg.TimeoutSeconds) * time.Second}}
}

func (c *LLMClient) Configured() bool {
	return c != nil && c.cfg.BaseURL != "" && c.cfg.APIKey != "" && c.cfg.Model != ""
}

// ChatJSON 单轮对话, 要求模型输出 JSON, 返回助手消息原文。
func (c *LLMClient) ChatJSON(ctx context.Context, system, user string) (string, error) {
	res, err := c.openChat(ctx, system, user, false)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if err != nil {
		return "", err
	}

	var parsed struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return "", fmt.Errorf("解析模型响应失败: %w", err)
	}
	if len(parsed.Choices) == 0 || parsed.Choices[0].Message.Content == "" {
		return "", fmt.Errorf("模型未返回内容")
	}
	return parsed.Choices[0].Message.Content, nil
}

func (c *LLMClient) openChat(ctx context.Context, system, user string, stream bool) (*http.Response, error) {
	if !c.Configured() {
		return nil, ErrAINotConfigured
	}
	reqBody, _ := json.Marshal(map[string]any{
		"model":  c.cfg.Model,
		"stream": stream,
		"messages": []map[string]string{
			{"role": "system", "content": system},
			{"role": "user", "content": user},
		},
		"temperature":     0.2,
		"response_format": map[string]string{"type": "json_object"},
	})
	u := strings.TrimSuffix(c.cfg.BaseURL, "/") + "/chat/completions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(reqBody))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+c.cfg.APIKey)

	res, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		res.Body.Close()
		return nil, fmt.Errorf("模型网关返回 %d", res.StatusCode)
	}
	return res, nil
}
