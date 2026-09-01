package config

import (
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

type Config struct {
	Server     ServerConfig
	Database   DatabaseConfig
	Redis      RedisConfig
	JWT        JWTConfig
	SMS        SMSConfig
	Security   SecurityConfig
	Blockchain BlockchainConfig
	AI         AIConfig
}

// AIConfig 智能搜索的模型网关(OpenAI 兼容 /chat/completions)。
// 三项配齐才启用; 未配置时智能搜索接口返回明确错误, 不做规则降级。
type AIConfig struct {
	BaseURL        string // 如 https://your-gateway/v1
	APIKey         string // 生产从环境变量 AI_API_KEY 注入
	Model          string
	TimeoutSeconds int
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
	LocalPreview         bool
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
	CredentialKey    string
	CapSiteVerifyURL string
	CapSecret        string
	CapTestToken     string
}

// BlockchainConfig BSN 开放联盟链·文昌链存证接入 (docs/14 §14.8)。
// gateway_url/project_id/account_key 三项配齐才会真正上链; 未配齐时存证事件照常
// 落库为 pending, worker 待命, 配置上线重启后自动补推 —— 不阻塞业务上线。
// gateway_url/project_id 来自 BSN 门户「项目管理 → 下载接入参数」。
type BlockchainConfig struct {
	GatewayURL  string // BSN 项目网关, 如 https://opbningxia.bsngate.com:18602
	ProjectID   string // BSN 项目 id
	ProjectKey  string // BSN 项目 key; 项目未启用密钥校验时留空
	ChainID     string // 链 ID, 默认 wenchangchain
	AccountKey  string // 链账户 secp256k1 私钥 64 位 hex — 敏感, 从环境变量 BLOCKCHAIN_ACCOUNT_KEY 注入
	Denom       string // gas 面额, 默认 ugas
	GasLimit    uint64 // 默认 200000
	GasPrice    uint64 // 接入参数表 gasprice, 默认 1
	ExplorerURL string // 区块链浏览器 tx 前缀, 供"链上自行查验"链接
	SignKeySeed string // 平台见证签名 Ed25519 种子, 64 位 hex; 生产从 KMS/环境变量注入
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
			LocalPreview:         v.GetBool("sms.local_preview"),
			SignName:             v.GetString("sms.sign_name"),
			LoginTemplateCode:    v.GetString("sms.login_template_code"),
			RegisterTemplateCode: v.GetString("sms.register_template_code"),
			Endpoint:             v.GetString("sms.endpoint"),
			CodeTTL:              v.GetInt("sms.code_ttl"),
		},
		Security: SecurityConfig{
			CredentialKey:    v.GetString("security.credential_key"),
			CapSiteVerifyURL: v.GetString("security.cap_siteverify_url"),
			CapSecret:        v.GetString("security.cap_secret"),
			CapTestToken:     v.GetString("security.cap_test_token"),
		},
		AI: AIConfig{
			BaseURL:        v.GetString("ai.base_url"),
			APIKey:         v.GetString("ai.api_key"),
			Model:          v.GetString("ai.model"),
			TimeoutSeconds: v.GetInt("ai.timeout_seconds"),
		},
		Blockchain: BlockchainConfig{
			GatewayURL:  v.GetString("blockchain.gateway_url"),
			ProjectID:   v.GetString("blockchain.project_id"),
			ProjectKey:  v.GetString("blockchain.project_key"),
			ChainID:     v.GetString("blockchain.chain_id"),
			AccountKey:  v.GetString("blockchain.account_key"),
			Denom:       v.GetString("blockchain.denom"),
			GasLimit:    v.GetUint64("blockchain.gas_limit"),
			GasPrice:    v.GetUint64("blockchain.gas_price"),
			ExplorerURL: v.GetString("blockchain.explorer_url"),
			SignKeySeed: v.GetString("blockchain.sign_key_seed"),
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
	if cfg.SMS.Enabled && cfg.SMS.LocalPreview {
		return nil, fmt.Errorf("sms.enabled and sms.local_preview cannot both be enabled")
	}
	if (cfg.Security.CapSiteVerifyURL == "") != (cfg.Security.CapSecret == "") {
		return nil, fmt.Errorf("security.cap_siteverify_url and security.cap_secret must be configured together")
	}
	// BSN 三要素要么全空(未接入), 要么全配 —— 半配置多半是漏了一项, 直接报错好过静默待命。
	bsnSet := 0
	for _, s := range []string{cfg.Blockchain.GatewayURL, cfg.Blockchain.ProjectID, cfg.Blockchain.AccountKey} {
		if s != "" {
			bsnSet++
		}
	}
	if bsnSet != 0 && bsnSet != 3 {
		return nil, fmt.Errorf("blockchain.gateway_url, blockchain.project_id and blockchain.account_key must be configured together")
	}
	aiSet := 0
	for _, s := range []string{cfg.AI.BaseURL, cfg.AI.APIKey, cfg.AI.Model} {
		if s != "" {
			aiSet++
		}
	}
	if aiSet != 0 && aiSet != 3 {
		return nil, fmt.Errorf("ai.base_url, ai.api_key and ai.model must be configured together")
	}
	if cfg.Server.Port == "" {
		cfg.Server.Port = "8080"
	}
	if cfg.Server.Mode == "release" {
		if cfg.SMS.LocalPreview || cfg.Security.CapTestToken != "" {
			return nil, fmt.Errorf("local authentication preview cannot be enabled in release mode")
		}
		if len(cfg.JWT.AccessSecret) < 32 || strings.HasPrefix(cfg.JWT.AccessSecret, "change-me-") {
			return nil, fmt.Errorf("jwt.access_secret must be a non-default secret of at least 32 characters in release mode")
		}
		if len(cfg.JWT.RefreshSecret) < 32 || strings.HasPrefix(cfg.JWT.RefreshSecret, "change-me-") {
			return nil, fmt.Errorf("jwt.refresh_secret must be a non-default secret of at least 32 characters in release mode")
		}
	}
	return cfg, nil
}
