package auth

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

var (
	ErrCaptchaInvalid       = errors.New("安全验证无效或已过期")
	ErrCaptchaNotConfigured = errors.New("安全验证服务未配置")
	ErrCaptchaUnavailable   = errors.New("安全验证服务暂不可用")
)

type CapVerifier struct {
	siteVerifyURL string
	secret        string
	testToken     string
	client        *http.Client
}

func NewCapVerifier(siteVerifyURL, secret, testToken string) *CapVerifier {
	return &CapVerifier{
		siteVerifyURL: strings.TrimSpace(siteVerifyURL),
		secret:        strings.TrimSpace(secret),
		testToken:     strings.TrimSpace(testToken),
		client:        &http.Client{Timeout: 5 * time.Second},
	}
}

func (v *CapVerifier) Verify(ctx context.Context, token string) error {
	if v != nil && v.testToken != "" && strings.TrimSpace(token) == v.testToken {
		return nil
	}
	if v == nil || v.siteVerifyURL == "" || v.secret == "" {
		return ErrCaptchaNotConfigured
	}
	if strings.TrimSpace(token) == "" {
		return ErrCaptchaInvalid
	}

	body, err := json.Marshal(map[string]string{
		"secret":   v.secret,
		"response": token,
	})
	if err != nil {
		return fmt.Errorf("%w: encode request: %v", ErrCaptchaUnavailable, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, v.siteVerifyURL, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("%w: create request: %v", ErrCaptchaUnavailable, err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := v.client.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrCaptchaUnavailable, err)
	}
	defer resp.Body.Close()
	var result struct {
		Success bool `json:"success"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&result); err != nil {
		return fmt.Errorf("%w: decode response: %v", ErrCaptchaUnavailable, err)
	}
	if resp.StatusCode >= 500 {
		return fmt.Errorf("%w: unexpected status %d", ErrCaptchaUnavailable, resp.StatusCode)
	}
	if !result.Success {
		return ErrCaptchaInvalid
	}
	return nil
}
