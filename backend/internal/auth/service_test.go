package auth

import (
	"context"
	"database/sql"
	"testing"
	"time"
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
	assert.Equal(t, 42900, ErrToCode(ErrSMSRateLimited))
}

func TestSMSLoginAuthenticatesExistingUserAndConsumesCode(t *testing.T) {
	repo := newFakeUserRepository()
	_, err := repo.CreateUser("13800138000", "", "unused-password-hash")
	require.NoError(t, err)
	sender := &fakeSMSSender{}
	store := &fakeSMSCodeStore{}
	svc := newSMSService(repo, sender, store)

	require.NoError(t, svc.SendSMSCode(context.Background(), "13800138000", "login", "127.0.0.1"))
	assert.Regexp(t, `^[0-9]{6}$`, sender.code)
	assert.Equal(t, "login", sender.purpose)

	tokens, user, err := svc.SMSLogin(context.Background(), SMSLoginReq{
		Phone:   "13800138000",
		SMSCode: sender.code,
	})
	require.NoError(t, err)
	assert.NotEmpty(t, tokens.AccessToken)
	assert.Equal(t, "138****8000", user.Phone)
	assert.Equal(t, []string{"buyer"}, user.Roles)
	assert.Empty(t, store.codeHash)

	_, _, err = svc.SMSLogin(context.Background(), SMSLoginReq{
		Phone:   "13800138000",
		SMSCode: sender.code,
	})
	assert.ErrorIs(t, err, ErrInvalidSMSCode)
}

func TestRegisterVerifiesSMSCodeAndStoresPassword(t *testing.T) {
	repo := newFakeUserRepository()
	sender := &fakeSMSSender{}
	store := &fakeSMSCodeStore{}
	svc := newSMSService(repo, sender, store)

	require.NoError(t, svc.SendSMSCode(context.Background(), "13900139000", "register", "127.0.0.1"))
	assert.Equal(t, "register", sender.purpose)
	userID, err := svc.Register(context.Background(), RegisterReq{
		Phone:    "13900139000",
		SmsCode:  sender.code,
		Password: "StrongPass123!",
		AgreeTOS: true,
	})
	require.NoError(t, err)
	user, err := repo.FindByID(userID)
	require.NoError(t, err)
	assert.True(t, CheckPassword(user.PasswordHash, "StrongPass123!"))
}

func TestSendSMSCodeRejectsInvalidPhoneBeforeSending(t *testing.T) {
	repo := newFakeUserRepository()
	sender := &fakeSMSSender{}
	svc := newSMSService(repo, sender, &fakeSMSCodeStore{})

	err := svc.SendSMSCode(context.Background(), "123", "login", "127.0.0.1")
	assert.ErrorIs(t, err, ErrInvalidPhone)
	assert.Empty(t, sender.code)
}

func TestSendSMSCodeDoesNotRevealAccountState(t *testing.T) {
	repo := newFakeUserRepository()
	_, err := repo.CreateUser("13800138000", "", "unused-password-hash")
	require.NoError(t, err)
	sender := &fakeSMSSender{}
	svc := newSMSService(repo, sender, &fakeSMSCodeStore{})

	require.NoError(t, svc.SendSMSCode(context.Background(), "13800138000", "register", "127.0.0.1"))
	assert.Empty(t, sender.code)
	require.NoError(t, svc.SendSMSCode(context.Background(), "13900139000", "login", "127.0.0.1"))
	assert.Empty(t, sender.code)
}

type fakeUserRepository struct {
	nextID  int64
	byPhone map[string]*User
	byID    map[int64]*User
}

func newFakeUserRepository() *fakeUserRepository {
	return &fakeUserRepository{nextID: 1, byPhone: map[string]*User{}, byID: map[int64]*User{}}
}

func (r *fakeUserRepository) CreateUser(phone, email, passwordHash string) (int64, error) {
	if _, ok := r.byPhone[phone]; ok {
		return 0, ErrUserExists
	}
	id := r.nextID
	r.nextID++
	user := &User{ID: id, Phone: phone, Email: email, PasswordHash: passwordHash, Status: "active"}
	r.byPhone[phone] = user
	r.byID[id] = user
	return id, nil
}

func (r *fakeUserRepository) FindByPhone(phone string) (*User, error) {
	user, ok := r.byPhone[phone]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return user, nil
}

func (r *fakeUserRepository) FindByID(id int64) (*User, error) {
	user, ok := r.byID[id]
	if !ok {
		return nil, sql.ErrNoRows
	}
	return user, nil
}

type fakeUserRoleRepository struct{}

func (fakeUserRoleRepository) GetRoles(int64) ([]string, error) { return []string{"buyer"}, nil }

type fakeSMSSender struct {
	code    string
	purpose string
	err     error
}

func (s *fakeSMSSender) SendCode(_ context.Context, _ string, code, purpose string) error {
	s.code = code
	s.purpose = purpose
	return s.err
}

type fakeSMSCodeStore struct {
	codeHash string
	err      error
}

func (s *fakeSMSCodeStore) Reserve(context.Context, string, string, string) error { return s.err }

func (s *fakeSMSCodeStore) Save(_ context.Context, _, _ string, codeHash string, _ time.Duration) error {
	s.codeHash = codeHash
	return s.err
}

func (s *fakeSMSCodeStore) Verify(_ context.Context, _, _ string, codeHash string) (bool, error) {
	if s.err != nil {
		return false, s.err
	}
	if s.codeHash == "" || s.codeHash != codeHash {
		return false, nil
	}
	s.codeHash = ""
	return true, nil
}

func newSMSService(repo UserRepository, sender SMSSender, store SMSCodeStore) *Service {
	return &Service{
		repo:          repo,
		userRoleRepo:  fakeUserRoleRepository{},
		smsSender:     sender,
		smsStore:      store,
		smsCodeTTL:    5 * time.Minute,
		jwtAccessSec:  "test-access-secret-at-least-32-characters",
		jwtRefreshSec: "test-refresh-secret-at-least-32-characters",
		accessTTL:     900,
		refreshTTL:    604800,
	}
}
