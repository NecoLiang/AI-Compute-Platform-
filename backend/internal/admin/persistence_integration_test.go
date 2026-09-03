package admin

import (
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

const adminPersistenceTestDB = "tokenfactory_admin_persistence_test"

func TestConfigAndNoticeSurviveServiceRecreation(t *testing.T) {
	db := setupAdminPersistenceTestDB(t)
	svc := NewService(NewRepository(db))

	if err := svc.UpdateConfig(7, "fee_rate", "650", "127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	config, err := NewService(NewRepository(db)).GetConfig()
	if err != nil {
		t.Fatal(err)
	}
	if config.FeeRate != 650 || !config.TradingEnabled {
		t.Fatalf("config=%+v", config)
	}

	id, err := svc.CreateNotice(7, "维护公告", "127.0.0.1")
	if err != nil {
		t.Fatal(err)
	}
	notices, err := NewService(NewRepository(db)).ListNotices()
	if err != nil {
		t.Fatal(err)
	}
	if len(notices) != 1 || notices[0].ID != id || notices[0].Content != "维护公告" {
		t.Fatalf("notices=%+v", notices)
	}

	var audits int
	if err := db.Get(&audits, "SELECT COUNT(*) FROM audit_logs WHERE operator_id=7 AND action IN ('update_config','create_notice')"); err != nil || audits != 2 {
		t.Fatalf("audits=%d err=%v", audits, err)
	}
}

func setupAdminPersistenceTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 TEST_MYSQL_DSN, 跳过 Admin 持久化集成测试")
	}
	root, err := sqlx.Connect("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	root.MustExec("DROP DATABASE IF EXISTS " + adminPersistenceTestDB)
	root.MustExec("CREATE DATABASE " + adminPersistenceTestDB + " CHARACTER SET utf8mb4")
	db, err := sqlx.Connect("mysql", dsn+adminPersistenceTestDB+"?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE system_config (config_key VARCHAR(64) PRIMARY KEY, config_value VARCHAR(255) NOT NULL, updated_by BIGINT, updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP)`,
		`INSERT INTO system_config (config_key, config_value) VALUES ('trading_enabled','true'),('fee_rate','500')`,
		`CREATE TABLE cms_notices (id BIGINT PRIMARY KEY AUTO_INCREMENT, content TEXT NOT NULL, status ENUM('published','withdrawn') NOT NULL DEFAULT 'published', created_by BIGINT NOT NULL, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE audit_logs (id BIGINT PRIMARY KEY AUTO_INCREMENT, operator_id BIGINT, action VARCHAR(64) NOT NULL, target_type VARCHAR(32), target_id BIGINT, before_value TEXT, after_value TEXT, ip VARCHAR(45), created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)`,
	} {
		db.MustExec(statement)
	}
	t.Cleanup(func() {
		db.Close()
		root.Exec("DROP DATABASE IF EXISTS " + adminPersistenceTestDB)
		root.Close()
	})
	return db
}
