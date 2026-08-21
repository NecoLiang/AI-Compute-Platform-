package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadUsesDeploymentEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  port: "8080"
  mode: debug
database:
  dsn: file-dsn
redis:
  addr: file-redis:6379
  password: ""
  db: 0
jwt:
  access_secret: change-me-access-secret-key-32chars
  refresh_secret: change-me-refresh-secret-key-32chars
security:
  credential_key: ""
`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SERVER_MODE", "release")
	t.Setenv("DATABASE_DSN", "deploy-dsn")
	t.Setenv("REDIS_ADDR", "wanxiang-redis:6379")
	t.Setenv("REDIS_PASSWORD", "deploy-redis-password")
	t.Setenv("JWT_ACCESS_SECRET", "deployment-access-secret-at-least-32")
	t.Setenv("JWT_REFRESH_SECRET", "deployment-refresh-secret-at-least-32")
	t.Setenv("SECURITY_CREDENTIAL_KEY", "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Server.Mode != "release" || cfg.Database.DSN != "deploy-dsn" {
		t.Fatalf("deployment environment not applied: %+v", cfg)
	}
	if cfg.Redis.Addr != "wanxiang-redis:6379" || cfg.Redis.Password != "deploy-redis-password" {
		t.Fatalf("redis environment not applied: %+v", cfg.Redis)
	}
}

func TestLoadRejectsDefaultJWTSecretsInRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  mode: release
jwt:
  access_secret: change-me-access-secret-key-32chars
  refresh_secret: change-me-refresh-secret-key-32chars
`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("expected release mode to reject default JWT secrets")
	}
}

func TestLoadSMSDefaultsAndEnvironment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  mode: debug
sms:
  enabled: false
`), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("SMS_ENABLED", "true")
	t.Setenv("SMS_SIGN_NAME", "万象硅芯科技")
	t.Setenv("SMS_LOGIN_TEMPLATE_CODE", "SMS_LOGIN")
	t.Setenv("SMS_REGISTER_TEMPLATE_CODE", "SMS_REGISTER")

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SMS.Enabled || cfg.SMS.SignName != "万象硅芯科技" || cfg.SMS.LoginTemplateCode != "SMS_LOGIN" || cfg.SMS.RegisterTemplateCode != "SMS_REGISTER" {
		t.Fatalf("sms environment not applied: %+v", cfg.SMS)
	}
	if cfg.SMS.Endpoint != "dysmsapi.aliyuncs.com" || cfg.SMS.CodeTTL != 300 {
		t.Fatalf("sms defaults not applied: %+v", cfg.SMS)
	}
}

func TestLoadRejectsIncompleteSMSConfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  mode: debug
sms:
  enabled: true
  sign_name: ""
  login_template_code: ""
  register_template_code: ""
`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("expected enabled SMS without sign and template to fail")
	}
}

func TestLoadEnablesLocalAuthPreviewInDebug(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  mode: debug
sms:
  local_preview: true
security:
  cap_test_token: demo-cap-token
`), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.SMS.LocalPreview || cfg.Security.CapTestToken != "demo-cap-token" {
		t.Fatalf("local auth preview not loaded: sms=%+v security=%+v", cfg.SMS, cfg.Security)
	}
}

func TestLoadRejectsLocalAuthPreviewInRelease(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`
server:
  mode: release
jwt:
  access_secret: deployment-access-secret-at-least-32
  refresh_secret: deployment-refresh-secret-at-least-32
sms:
  local_preview: true
security:
  cap_test_token: demo-cap-token
`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := Load(path); err == nil {
		t.Fatal("expected release mode to reject local authentication preview")
	}
}
