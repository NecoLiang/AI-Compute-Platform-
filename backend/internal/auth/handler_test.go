package auth

import (
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
