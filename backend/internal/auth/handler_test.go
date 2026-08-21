package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"tokenfactory/internal/sms"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSendSMSCodeReturnsLocalPreviewCode(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/v1/auth/sms/code", strings.NewReader(`{
		"phone":"13900139000",
		"purpose":"register",
		"captcha_token":"demo-cap-token"
	}`))
	ctx.Request.Header.Set("Content-Type", "application/json")

	handler := NewHandler(
		newSMSService(newFakeUserRepository(), sms.NewPreviewSender(), &fakeSMSCodeStore{}),
		NewCapVerifier("", "", "demo-cap-token"),
	)
	handler.SendSMSCode(ctx)

	var body struct {
		Code int `json:"code"`
		Data struct {
			PreviewCode string `json:"preview_code"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Equal(t, 0, body.Code)
	assert.Regexp(t, `^[0-9]{6}$`, body.Data.PreviewCode)
}

func TestMeNeverReturnsRawPhoneFromTokenClaims(t *testing.T) {
	gin.SetMode(gin.TestMode)
	repo := newFakeUserRepository()
	userID, err := repo.CreateUser("13800138000", "", "")
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Set("user_id", userID)
	ctx.Set("phone", "13800138000")
	NewHandler(newSMSService(repo, &fakeSMSSender{}, &fakeSMSCodeStore{}), nil).Me(ctx)

	assert.NotContains(t, recorder.Body.String(), "13800138000")
	assert.Contains(t, recorder.Body.String(), "138****8000")
}
