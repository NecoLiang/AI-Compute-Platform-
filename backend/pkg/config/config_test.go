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
