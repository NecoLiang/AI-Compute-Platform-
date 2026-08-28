package user

import (
	"encoding/json"
	"errors"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRespondKYCErrorHidesDatabaseDetails(t *testing.T) {
	gin.SetMode(gin.TestMode)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	respondKYCError(ctx, errors.New("missing destination name rejected_reason"))

	var body struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))
	assert.Equal(t, 50000, body.Code)
	assert.Equal(t, "认证服务暂不可用", body.Message)
	assert.NotContains(t, recorder.Body.String(), "rejected_reason")
}
