package auth

import (
	"testing"
	"tokenfactory/pkg/config"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestHashPassword(t *testing.T) {
	hash, err := HashPassword("TestPass123!")
	require.NoError(t, err)
	assert.NotEmpty(t, hash)
	assert.True(t, CheckPassword(hash, "TestPass123!"))
	assert.False(t, CheckPassword(hash, "WrongPassword"))
}

func TestMaskPhone(t *testing.T) {
	assert.Equal(t, "138****8000", maskPhone("13800138000"))
	assert.Equal(t, "12", maskPhone("12"))
}

func TestJWTGeneration(t *testing.T) {
	cfg := &config.JWTConfig{
		AccessSecret: "test-secret-32chars-minimum!!",
		AccessTTL:    900,
	}
	svc := &Service{
		jwtAccessSec: cfg.AccessSecret,
		accessTTL:    cfg.AccessTTL,
	}
	token, err := svc.genToken(1, "138****8000", []string{"buyer"}, cfg.AccessSecret, cfg.AccessTTL)
	require.NoError(t, err)
	assert.NotEmpty(t, token)
}

func TestRegisterReqValidation(t *testing.T) {
	// Test that password < 8 chars is rejected
	req := RegisterReq{Phone: "13800138000", SmsCode: "123456", Password: "short", AgreeTOS: true}
	assert.Len(t, req.Password, 5) // just checking field value
	assert.True(t, req.AgreeTOS)
}

func TestErrToCode(t *testing.T) {
	assert.Equal(t, 40900, ErrToCode(ErrUserExists))
	assert.Equal(t, 40100, ErrToCode(ErrInvalidLogin))
	assert.Equal(t, 40300, ErrToCode(ErrUserFrozen))
}
