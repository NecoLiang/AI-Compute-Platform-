package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Server   ServerConfig
	Database DatabaseConfig
	Redis    RedisConfig
	JWT      JWTConfig
	SMS      SMSConfig
	Security SecurityConfig
}

type ServerConfig struct {
	Port string
	Mode string // debug/release
}

type DatabaseConfig struct {
	DSN string // user:pass@tcp(host:port)/dbname?parseTime=true
}

type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

type JWTConfig struct {
	AccessSecret  string
	RefreshSecret string
	AccessTTL     int // seconds, default 900 (15min)
	RefreshTTL    int // seconds, default 604800 (7 days)
}

type SMSConfig struct {
	Enabled              bool
	SignName             string
	LoginTemplateCode    string
	RegisterTemplateCode string
	Endpoint             string
	CodeTTL              int // seconds
}

type SecurityConfig struct {
	// CredentialKey 是交付访问凭证的 AES-256-GCM 加密密钥, 64 位 hex 表示 32 字节。
	// 默认留空: 留空时生成访问凭证会返回明确错误而非降级存明文。
	// 生产环境应从 KMS / 环境变量注入, 不要提交进仓库。
	CredentialKey string
}

func Load(path string) (*Config, error) {
	v := viper.New()
	v.SetConfigFile(path)
	v.SetEnvKeyReplacer(strings.NewReplacer(".", "_"))
	v.AutomaticEnv()
	if err := v.ReadInConfig(); err != nil {
		return nil, err
	}
	cfg := &Config{
		Server: ServerConfig{
			Port: v.GetString("server.port"),
			Mode: v.GetString("server.mode"),
		},
		Database: DatabaseConfig{
			DSN: v.GetString("database.dsn"),
		},
		Redis: RedisConfig{
			Addr:     v.GetString("redis.addr"),
			Password: v.GetString("redis.password"),
			DB:       v.GetInt("redis.db"),
		},
		JWT: JWTConfig{
			AccessSecret:  v.GetString("jwt.access_secret"),
			RefreshSecret: v.GetString("jwt.refresh_secret"),
			AccessTTL:     v.GetInt("jwt.access_ttl"),
			RefreshTTL:    v.GetInt("jwt.refresh_ttl"),
		},
		SMS: SMSConfig{
			Enabled:              v.GetBool("sms.enabled"),
			SignName:             v.GetString("sms.sign_name"),
			LoginTemplateCode:    v.GetString("sms.login_template_code"),
			RegisterTemplateCode: v.GetString("sms.register_template_code"),
			Endpoint:             v.GetString("sms.endpoint"),
			CodeTTL:              v.GetInt("sms.code_ttl"),
		},
		Security: SecurityConfig{
			CredentialKey: v.GetString("security.credential_key"),
		},
	}
	if cfg.JWT.AccessTTL == 0 {
		cfg.JWT.AccessTTL = 900
	}
	if cfg.JWT.RefreshTTL == 0 {
		cfg.JWT.RefreshTTL = 604800
	}
	if cfg.SMS.Endpoint == "" {
		cfg.SMS.Endpoint = "dysmsapi.aliyuncs.com"
	}
	if cfg.SMS.CodeTTL == 0 {
		cfg.SMS.CodeTTL = 300
	}
	if cfg.SMS.Enabled && (cfg.SMS.SignName == "" || cfg.SMS.LoginTemplateCode == "" || cfg.SMS.RegisterTemplateCode == "") {
		return nil, fmt.Errorf("sms.sign_name, sms.login_template_code and sms.register_template_code are required when sms is enabled")
	}
	if cfg.Server.Port == "" {
		cfg.Server.Port = "8080"
	}
	if cfg.Server.Mode == "release" {
		if len(cfg.JWT.AccessSecret) < 32 || strings.HasPrefix(cfg.JWT.AccessSecret, "change-me-") {
			return nil, fmt.Errorf("jwt.access_secret must be a non-default secret of at least 32 characters in release mode")
		}
		if len(cfg.JWT.RefreshSecret) < 32 || strings.HasPrefix(cfg.JWT.RefreshSecret, "change-me-") {
			return nil, fmt.Errorf("jwt.refresh_secret must be a non-default secret of at least 32 characters in release mode")
		}
	}
	return cfg, nil
}
