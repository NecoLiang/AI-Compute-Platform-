package user

import (
	"os"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

const kycTestDB = "tokenfactory_kyc_test"

func setupKYCTestDB(t *testing.T) *Service {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 TEST_MYSQL_DSN, 跳过 KYC 集成测试")
	}

	root, err := sqlx.Connect("mysql", dsn)
	if err != nil {
		t.Fatalf("连接 MySQL 失败: %v", err)
	}
	defer root.Close()
	root.MustExec("DROP DATABASE IF EXISTS " + kycTestDB)
	root.MustExec("CREATE DATABASE " + kycTestDB + " CHARACTER SET utf8mb4")

	db, err := sqlx.Connect("mysql", dsn+kycTestDB+"?parseTime=true")
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db.MustExec(`CREATE TABLE user_kyc (
		id BIGINT PRIMARY KEY AUTO_INCREMENT,
		user_id BIGINT NOT NULL UNIQUE,
		real_name VARCHAR(64) NOT NULL,
		id_card VARCHAR(32) NOT NULL,
		status ENUM('pending','verified','rejected') NOT NULL DEFAULT 'pending',
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`)
	db.MustExec(`CREATE TABLE enterprises (
		id BIGINT PRIMARY KEY AUTO_INCREMENT,
		user_id BIGINT NOT NULL UNIQUE,
		name VARCHAR(128) NOT NULL,
		uscc VARCHAR(32) NOT NULL,
		license_url VARCHAR(512) NOT NULL,
		legal_person VARCHAR(64) NOT NULL,
		status ENUM('pending','verified','rejected') NOT NULL DEFAULT 'pending',
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP
	)`)

	t.Cleanup(func() {
		db.Exec("DROP DATABASE IF EXISTS " + kycTestDB)
		db.Close()
	})
	return NewService(NewRepository(db))
}

func TestKYCSubmissionIsAutoVerified(t *testing.T) {
	svc := setupKYCTestDB(t)

	if err := svc.SubmitPersonalKYC(101, PersonalKYCReq{RealName: "测试用户", IDCard: "110101199001011234"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SubmitEnterprise(202, EnterpriseReq{Name: "测试企业", USCC: "91110000123456789X", LicenseURL: "license.png", LegalPerson: "测试法人"}); err != nil {
		t.Fatal(err)
	}

	personal, err := svc.GetKYCStatus(101)
	if err != nil || personal.Personal.Status != "verified" {
		t.Fatalf("个人认证应自动通过, status=%v err=%v", personal.Personal.Status, err)
	}
	enterprise, err := svc.GetKYCStatus(202)
	if err != nil || enterprise.Enterprise.Status != "verified" {
		t.Fatalf("企业认证应自动通过, status=%v err=%v", enterprise.Enterprise.Status, err)
	}
}
