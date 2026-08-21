package auth

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"time"
	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/middleware"

	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
)

var (
	ErrUserExists        = errors.New("用户已存在")
	ErrInvalidLogin      = errors.New("账号或凭证不正确")
	ErrUserFrozen        = errors.New("账号已被冻结")
	ErrInvalidPhone      = errors.New("手机号格式不正确")
	ErrInvalidSMSCode    = errors.New("验证码错误或已失效")
	ErrSMSRateLimited    = errors.New("验证码请求过于频繁")
	ErrSMSNotConfigured  = errors.New("短信服务未配置")
	ErrSMSSendFailed     = errors.New("短信发送失败")
	ErrTermsRequired     = errors.New("必须同意用户协议和隐私政策")
	ErrInvalidSMSPurpose = errors.New("验证码用途必须是 register 或 login")
	ErrInvalidRefresh    = errors.New("登录状态已失效，请重新登录")
)

var (
	mainlandPhonePattern = regexp.MustCompile(`^1[3-9][0-9]{9}$`)
	smsCodePattern       = regexp.MustCompile(`^[0-9]{6}$`)
)

type Service struct {
	repo          UserRepository
	userRoleRepo  UserRoleRepository
	rdb           *redis.Client
	smsSender     SMSSender
	smsStore      SMSCodeStore
	smsCodeTTL    time.Duration
	jwtAccessSec  string
	jwtRefreshSec string
	accessTTL     int
	refreshTTL    int
}

type UserRepository interface {
	CreateUser(phone, email, passwordHash string) (int64, error)
	FindByPhone(phone string) (*User, error)
	FindByID(id int64) (*User, error)
}

type UserRoleRepository interface {
	GetRoles(userID int64) ([]string, error)
}

type SMSSender interface {
	SendCode(ctx context.Context, phone, code, purpose string) error
}

type SMSCodeStore interface {
	Reserve(ctx context.Context, phone, purpose, clientIP string) error
	Save(ctx context.Context, phone, purpose, codeHash string, ttl time.Duration) error
	Verify(ctx context.Context, phone, purpose, codeHash string) (bool, error)
}

func NewService(repo UserRepository, userRoleRepo UserRoleRepository, rdb *redis.Client, smsSender SMSSender, smsCodeTTL time.Duration, accessSec, refreshSec string, accessTTL, refreshTTL int) *Service {
	if smsCodeTTL <= 0 {
		smsCodeTTL = 5 * time.Minute
	}
	return &Service{
		repo:          repo,
		userRoleRepo:  userRoleRepo,
		rdb:           rdb,
		smsSender:     smsSender,
		smsStore:      NewRedisSMSCodeStore(rdb),
		smsCodeTTL:    smsCodeTTL,
		jwtAccessSec:  accessSec,
		jwtRefreshSec: refreshSec,
		accessTTL:     accessTTL,
		refreshTTL:    refreshTTL,
	}
}

type RegisterReq struct {
	Phone    string `json:"phone" binding:"required"`
	SmsCode  string `json:"sms_code" binding:"required"`
	AgreeTOS bool   `json:"agree_tos"`
}

type SendSMSCodeReq struct {
	Phone        string `json:"phone" binding:"required"`
	Purpose      string `json:"purpose" binding:"required,oneof=register login"`
	CaptchaToken string `json:"captcha_token" binding:"required"`
}

type SMSLoginReq struct {
	Phone   string `json:"phone" binding:"required"`
	SMSCode string `json:"sms_code" binding:"required"`
}

type LoginReq struct {
	Account  string `json:"account" binding:"required"`
	Password string `json:"password" binding:"required"`
}

type TokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

type UserInfo struct {
	ID    int64    `json:"id"`
	Phone string   `json:"phone"`
	Email string   `json:"email,omitempty"`
	Roles []string `json:"roles"`
}

func (s *Service) SendSMSCode(ctx context.Context, phone, purpose, clientIP string) error {
	if !mainlandPhonePattern.MatchString(phone) {
		return ErrInvalidPhone
	}
	if purpose != "register" && purpose != "login" {
		return ErrInvalidSMSPurpose
	}
	if s.smsSender == nil || s.smsStore == nil {
		return ErrSMSNotConfigured
	}
	_, err := s.repo.FindByPhone(phone)
	if err == nil && purpose == "register" {
		return ErrUserExists
	}
	if errors.Is(err, sql.ErrNoRows) && purpose == "login" {
		return nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err := s.smsStore.Reserve(ctx, phone, purpose, clientIP); err != nil {
		return err
	}
	code, err := generateSMSCode()
	if err != nil {
		return err
	}
	if err := s.smsSender.SendCode(ctx, phone, code, purpose); err != nil {
		return fmt.Errorf("%w: %v", ErrSMSSendFailed, err)
	}
	if err := s.smsStore.Save(ctx, phone, purpose, s.hashSMSCode(phone, purpose, code), s.smsCodeTTL); err != nil {
		return fmt.Errorf("%w: save verification code: %v", ErrSMSSendFailed, err)
	}
	return nil
}

func (s *Service) Register(ctx context.Context, req RegisterReq) (*TokenPair, *UserInfo, error) {
	if !mainlandPhonePattern.MatchString(req.Phone) {
		return nil, nil, ErrInvalidPhone
	}
	if !req.AgreeTOS {
		return nil, nil, ErrTermsRequired
	}
	if err := s.verifySMSCode(ctx, req.Phone, "register", req.SmsCode); err != nil {
		return nil, nil, err
	}
	if _, err := s.repo.FindByPhone(req.Phone); err == nil {
		return nil, nil, ErrUserExists
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, nil, err
	}
	userID, err := s.repo.CreateUser(req.Phone, "", "")
	if err != nil {
		return nil, nil, err
	}
	return s.issueTokens(&User{ID: userID, Phone: req.Phone, Status: "active"})
}

func (s *Service) SMSLogin(ctx context.Context, req SMSLoginReq) (*TokenPair, *UserInfo, error) {
	if !mainlandPhonePattern.MatchString(req.Phone) {
		return nil, nil, ErrInvalidPhone
	}
	if err := s.verifySMSCode(ctx, req.Phone, "login", req.SMSCode); err != nil {
		return nil, nil, err
	}
	user, err := s.repo.FindByPhone(req.Phone)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, ErrInvalidLogin
	}
	if err != nil {
		return nil, nil, err
	}
	if user.Status == "frozen" {
		return nil, nil, ErrUserFrozen
	}
	return s.issueTokens(user)
}

func (s *Service) Login(req LoginReq) (*TokenPair, *UserInfo, error) {
	user, err := s.repo.FindByPhone(req.Account)
	if err != nil {
		return nil, nil, ErrInvalidLogin
	}
	if user.Status == "frozen" {
		return nil, nil, ErrUserFrozen
	}
	if !CheckPassword(user.PasswordHash, req.Password) {
		return nil, nil, ErrInvalidLogin
	}

	return s.issueTokens(user)
}

func (s *Service) issueTokens(user *User) (*TokenPair, *UserInfo, error) {
	roles, _ := s.userRoleRepo.GetRoles(user.ID)
	if len(roles) == 0 {
		roles = []string{"buyer"}
	}
	accessToken, err := s.genToken(user.ID, user.Phone, roles, s.jwtAccessSec, s.accessTTL)
	if err != nil {
		return nil, nil, err
	}
	refreshToken, err := s.genToken(user.ID, user.Phone, roles, s.jwtRefreshSec, s.refreshTTL)
	if err != nil {
		return nil, nil, err
	}
	return &TokenPair{AccessToken: accessToken, RefreshToken: refreshToken, ExpiresIn: s.accessTTL},
		&UserInfo{ID: user.ID, Phone: maskPhone(user.Phone), Roles: roles}, nil
}

func (s *Service) verifySMSCode(ctx context.Context, phone, purpose, code string) error {
	if !smsCodePattern.MatchString(code) {
		return ErrInvalidSMSCode
	}
	if s.smsStore == nil {
		return ErrSMSNotConfigured
	}
	ok, err := s.smsStore.Verify(ctx, phone, purpose, s.hashSMSCode(phone, purpose, code))
	if err != nil {
		return err
	}
	if !ok {
		return ErrInvalidSMSCode
	}
	return nil
}

func (s *Service) hashSMSCode(phone, purpose, code string) string {
	mac := hmac.New(sha256.New, []byte(s.jwtAccessSec))
	mac.Write([]byte(phone))
	mac.Write([]byte{0})
	mac.Write([]byte(purpose))
	mac.Write([]byte{0})
	mac.Write([]byte(code))
	return hex.EncodeToString(mac.Sum(nil))
}

func generateSMSCode() (string, error) {
	n, err := rand.Int(rand.Reader, big.NewInt(1_000_000))
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("%06d", n.Int64()), nil
}

func (s *Service) RefreshToken(ctx context.Context, refreshToken string) (*TokenPair, error) {
	claims, err := parseTokenClaims(refreshToken, s.jwtRefreshSec)
	if err != nil || s.rdb == nil || claims.ExpiresAt == nil {
		return nil, ErrInvalidRefresh
	}
	user, err := s.repo.FindByID(claims.UserID)
	if err != nil || user.Status == "frozen" {
		return nil, ErrInvalidRefresh
	}
	roles, err := s.userRoleRepo.GetRoles(user.ID)
	if err != nil {
		return nil, err
	}
	if len(roles) == 0 {
		roles = []string{"buyer"}
	}
	ttl := time.Until(claims.ExpiresAt.Time)
	if ttl <= 0 {
		return nil, ErrInvalidRefresh
	}
	consumed, err := s.rdb.SetNX(ctx, refreshTokenKey(refreshToken), "1", ttl).Result()
	if err != nil {
		return nil, err
	}
	if !consumed {
		return nil, ErrInvalidRefresh
	}
	accessToken, err := s.genToken(user.ID, user.Phone, roles, s.jwtAccessSec, s.accessTTL)
	if err != nil {
		return nil, err
	}
	newRefreshToken, err := s.genToken(user.ID, user.Phone, roles, s.jwtRefreshSec, s.refreshTTL)
	if err != nil {
		return nil, err
	}
	return &TokenPair{AccessToken: accessToken, RefreshToken: newRefreshToken, ExpiresIn: s.accessTTL}, nil
}

func (s *Service) Logout(ctx context.Context, accessToken, refreshToken string) error {
	if s.rdb == nil {
		return errors.New("redis is not configured")
	}
	pipe := s.rdb.Pipeline()
	if claims, err := parseTokenClaims(accessToken, s.jwtAccessSec); err == nil && claims.ExpiresAt != nil {
		if ttl := time.Until(claims.ExpiresAt.Time); ttl > 0 {
			pipe.Set(ctx, "session:"+accessToken, "1", ttl)
		}
	}
	if claims, err := parseTokenClaims(refreshToken, s.jwtRefreshSec); err == nil && claims.ExpiresAt != nil {
		if ttl := time.Until(claims.ExpiresAt.Time); ttl > 0 {
			pipe.Set(ctx, refreshTokenKey(refreshToken), "1", ttl)
		}
	}
	_, err := pipe.Exec(ctx)
	return err
}

func parseTokenClaims(token, secret string) (*middleware.Claims, error) {
	claims := &middleware.Claims{}
	parsed, err := jwt.ParseWithClaims(
		token,
		claims,
		func(*jwt.Token) (interface{}, error) { return []byte(secret), nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
	)
	if err != nil || !parsed.Valid {
		return nil, ErrInvalidRefresh
	}
	return claims, nil
}

func refreshTokenKey(token string) string {
	sum := sha256.Sum256([]byte(token))
	return "session:refresh:" + hex.EncodeToString(sum[:])
}

func (s *Service) GetUser(userID int64) (*UserInfo, error) {
	user, err := s.repo.FindByID(userID)
	if err != nil {
		return nil, err
	}
	roles, _ := s.userRoleRepo.GetRoles(userID)
	if len(roles) == 0 {
		roles = []string{"buyer"}
	}
	return &UserInfo{ID: user.ID, Phone: maskPhone(user.Phone), Email: user.Email, Roles: roles}, nil
}

func (s *Service) genToken(userID int64, phone string, roles []string, secret string, ttl int) (string, error) {
	tokenID := make([]byte, 16)
	if _, err := rand.Read(tokenID); err != nil {
		return "", err
	}
	claims := &middleware.Claims{
		UserID: userID,
		Phone:  phone,
		Roles:  roles,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Duration(ttl) * time.Second)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
			ID:        hex.EncodeToString(tokenID),
		},
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	return token.SignedString([]byte(secret))
}

func maskPhone(phone string) string {
	if len(phone) < 7 {
		return phone
	}
	return phone[:3] + "****" + phone[len(phone)-4:]
}

func ErrToCode(err error) int {
	switch {
	case errors.Is(err, ErrUserExists):
		return errcode.Conflict
	case errors.Is(err, ErrInvalidLogin):
		return errcode.Unauthorized
	case errors.Is(err, ErrInvalidRefresh):
		return errcode.Unauthorized
	case errors.Is(err, ErrUserFrozen):
		return errcode.Forbidden
	case errors.Is(err, ErrInvalidPhone), errors.Is(err, ErrInvalidSMSPurpose), errors.Is(err, ErrTermsRequired):
		return errcode.ParamInvalid
	case errors.Is(err, ErrInvalidSMSCode):
		return errcode.Unauthorized
	case errors.Is(err, ErrCaptchaInvalid):
		return errcode.ParamInvalid
	case errors.Is(err, ErrSMSRateLimited):
		return errcode.TooManyRequests
	case errors.Is(err, sql.ErrNoRows):
		return errcode.NotFound
	default:
		return errcode.InternalError
	}
}
