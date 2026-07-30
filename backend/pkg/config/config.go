package config

import "github.com/spf13/viper"

type Config struct {
	Server   ServerConfig
	Database DatabaseConfig
	Redis    RedisConfig
	JWT      JWTConfig
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

type SecurityConfig struct {
	// CredentialKey 是交付访问凭证的 AES-256-GCM 加密密钥, 64 位 hex 表示 32 字节。
	// 默认留空: 留空时生成访问凭证会返回明确错误而非降级存明文。
	// 生产环境应从 KMS / 环境变量注入, 不要提交进仓库。
	CredentialKey string
}

func Load(path string) (*Config, error) {
	viper.SetConfigFile(path)
	viper.AutomaticEnv()
	if err := viper.ReadInConfig(); err != nil {
		return nil, err
	}
	cfg := &Config{
		Server: ServerConfig{
			Port: viper.GetString("server.port"),
			Mode: viper.GetString("server.mode"),
		},
		Database: DatabaseConfig{
			DSN: viper.GetString("database.dsn"),
		},
		Redis: RedisConfig{
			Addr:     viper.GetString("redis.addr"),
			Password: viper.GetString("redis.password"),
			DB:       viper.GetInt("redis.db"),
		},
		JWT: JWTConfig{
			AccessSecret:  viper.GetString("jwt.access_secret"),
			RefreshSecret: viper.GetString("jwt.refresh_secret"),
			AccessTTL:     viper.GetInt("jwt.access_ttl"),
			RefreshTTL:    viper.GetInt("jwt.refresh_ttl"),
		},
		Security: SecurityConfig{
			CredentialKey: viper.GetString("security.credential_key"),
		},
	}
	if cfg.JWT.AccessTTL == 0 {
		cfg.JWT.AccessTTL = 900
	}
	if cfg.JWT.RefreshTTL == 0 {
		cfg.JWT.RefreshTTL = 604800
	}
	if cfg.Server.Port == "" {
		cfg.Server.Port = "8080"
	}
	return cfg, nil
}
