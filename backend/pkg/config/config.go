package config

import "github.com/spf13/viper"

type Config struct {
	Server   ServerConfig
	Database DatabaseConfig
	Redis    RedisConfig
	JWT      JWTConfig
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
