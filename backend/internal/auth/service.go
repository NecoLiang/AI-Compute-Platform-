package auth

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/middleware"

	"github.com/golang-jwt/jwt/v5"
	"github.com/redis/go-redis/v9"
)

var (
	ErrUserExists   = errors.New("用户已存在")
	ErrInvalidLogin = errors.New("手机号或密码错误")
	ErrUserFrozen   = errors.New("账号已被冻结")
)

type Service struct {
	repo          *Repository
	userRoleRepo  UserRoleRepository
	rdb           *redis.Client
	jwtAccessSec  string
	jwtRefreshSec string
	accessTTL     int
	refreshTTL    int
}

type UserRoleRepository interface {
	GetRoles(userID int64) ([]string, error)
}

func NewService(repo *Repository, userRoleRepo UserRoleRepository, rdb *redis.Client, accessSec, refreshSec string, accessTTL, refreshTTL int) *Service {
	return &Service{
		repo:          repo,
		userRoleRepo:  userRoleRepo,
		rdb:           rdb,
		jwtAccessSec:  accessSec,
		jwtRefreshSec: refreshSec,
		accessTTL:     accessTTL,
		refreshTTL:    refreshTTL,
	}
}

type RegisterReq struct {
	Phone    string `json:"phone" binding:"required"`
	SmsCode  string `json:"sms_code" binding:"required"`
	Password string `json:"password" binding:"required,min=8"`
	AgreeTOS bool   `json:"agree_tos" binding:"required"`
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

func (s *Service) Register(req RegisterReq) (int64, error) {
	// TODO: 接入短信验证码服务商后替换此校验
	// 所需信息: 阿里云短信/腾讯云短信的 AccessKey + 签名+模板ID
	// 当前所有短信验证码均拒绝，防止跳过验证直接注册
	return 0, fmt.Errorf("短信验证码服务未接入: 请配置短信服务商(AccessKey+签名+模板ID)")
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

	return &TokenPair{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    s.accessTTL,
	}, &UserInfo{ID: user.ID, Phone: maskPhone(user.Phone), Roles: roles}, nil
}

func (s *Service) RefreshToken(refreshToken string) (*TokenPair, error) {
	claims := &middleware.Claims{}
	parsed, err := jwt.ParseWithClaims(refreshToken, claims, func(t *jwt.Token) (interface{}, error) {
		return []byte(s.jwtRefreshSec), nil
	})
	if err != nil || !parsed.Valid {
		return nil, errors.New("invalid refresh token")
	}
	accessToken, err := s.genToken(claims.UserID, claims.Phone, claims.Roles, s.jwtAccessSec, s.accessTTL)
	if err != nil {
		return nil, err
	}
	newRefreshToken, err := s.genToken(claims.UserID, claims.Phone, claims.Roles, s.jwtRefreshSec, s.refreshTTL)
	if err != nil {
		return nil, err
	}
	return &TokenPair{AccessToken: accessToken, RefreshToken: newRefreshToken, ExpiresIn: s.accessTTL}, nil
}

func (s *Service) Logout(ctx context.Context, accessToken string) error {
	claims := &middleware.Claims{}
	_, _ = jwt.ParseWithClaims(accessToken, claims, func(t *jwt.Token) (interface{}, error) {
		return []byte(s.jwtAccessSec), nil
	})
	ttl := time.Until(claims.ExpiresAt.Time)
	if ttl < 0 {
		return nil
	}
	return s.rdb.Set(ctx, "session:"+accessToken, "1", ttl).Err()
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
	claims := &middleware.Claims{
		UserID: userID,
		Phone:  phone,
		Roles:  roles,
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Duration(ttl) * time.Second)),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
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
	case errors.Is(err, ErrUserFrozen):
		return errcode.Forbidden
	case errors.Is(err, sql.ErrNoRows):
		return errcode.NotFound
	default:
		return errcode.InternalError
	}
}
