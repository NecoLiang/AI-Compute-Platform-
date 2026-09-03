package user

import (
	"bytes"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

const kycTestDB = "tokenfactory_kyc_test"

func setupKYCTestDB(t *testing.T) (*Service, *sqlx.DB) {
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
		rejected_reason VARCHAR(256),
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
	)`)
	db.MustExec(`CREATE TABLE enterprises (
		id BIGINT PRIMARY KEY AUTO_INCREMENT,
		user_id BIGINT NOT NULL UNIQUE,
		name VARCHAR(128) NOT NULL,
		uscc VARCHAR(32) NOT NULL,
		license_url VARCHAR(512) NOT NULL,
		legal_person VARCHAR(64) NOT NULL,
		legal_person_id_card VARCHAR(32) NOT NULL DEFAULT '',
		bank_name VARCHAR(128) NOT NULL DEFAULT '',
		bank_account_name VARCHAR(128) NOT NULL DEFAULT '',
		bank_account_number VARCHAR(64) NOT NULL DEFAULT '',
		license_file_name VARCHAR(255) NOT NULL DEFAULT '',
		license_content_type VARCHAR(128) NOT NULL DEFAULT '',
		license_blob MEDIUMBLOB NULL,
		status ENUM('pending','verified','rejected') NOT NULL DEFAULT 'pending',
		rejected_reason VARCHAR(256),
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP
	)`)

	t.Cleanup(func() {
		db.Exec("DROP DATABASE IF EXISTS " + kycTestDB)
		db.Close()
	})
	return NewService(NewRepository(db)), db
}

func TestKYCSubmissionIsAutoVerified(t *testing.T) {
	svc, _ := setupKYCTestDB(t)

	if err := svc.SubmitPersonalKYC(101, PersonalKYCReq{RealName: "测试用户", IDCard: "110101199001011234"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.SubmitEnterprise(202, EnterpriseReq{
		Name: "测试企业", USCC: "91110000123456789X", LicenseURL: "license.png",
		LegalPerson: "测试法人", LegalPersonIDCard: "110101199001011234",
		BankName: "测试银行", BankAccountName: "测试企业", BankAccountNumber: "1234567890",
		LicenseFileName: "license.png", LicenseContentType: "image/png", LicenseData: []byte("png"),
	}); err != nil {
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

func TestEnterpriseKYCSubmissionPersistsCompleteApplication(t *testing.T) {
	svc, db := setupKYCTestDB(t)
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		c.Set("user_id", int64(303))
		c.Next()
	})
	NewHandler(svc).RegisterRoutes(router.Group(""))

	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	fields := map[string]string{
		"enterprise_name":      "万象算力测试有限公司",
		"uscc":                 "91310115MA1K4X2A7Q",
		"legal_person":         "张明远",
		"legal_person_id_card": "110101199001011237",
		"bank_name":            "招商银行上海张江支行",
		"bank_account_name":    "万象算力测试有限公司",
		"bank_account_number":  "6225888888888888",
	}
	for name, value := range fields {
		if err := form.WriteField(name, value); err != nil {
			t.Fatal(err)
		}
	}
	file, err := form.CreateFormFile("business_license", "business-license.pdf")
	if err != nil {
		t.Fatal(err)
	}
	license := []byte("%PDF-1.4\nKYC test document")
	if _, err := file.Write(license); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}

	request := httptest.NewRequest(http.MethodPost, "/user/kyc/enterprise", &body)
	request.Header.Set("content-type", form.FormDataContentType())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("企业 KYC 提交失败: status=%d body=%s", response.Code, response.Body.String())
	}

	var stored struct {
		Name               string `db:"name"`
		USCC               string `db:"uscc"`
		LegalPerson        string `db:"legal_person"`
		LegalPersonIDCard  string `db:"legal_person_id_card"`
		BankName           string `db:"bank_name"`
		BankAccountName    string `db:"bank_account_name"`
		BankAccountNumber  string `db:"bank_account_number"`
		LicenseFileName    string `db:"license_file_name"`
		LicenseContentType string `db:"license_content_type"`
		LicenseBlob        []byte `db:"license_blob"`
		Status             string `db:"status"`
	}
	if err := db.Get(&stored, `SELECT name, uscc, legal_person, legal_person_id_card,
		bank_name, bank_account_name, bank_account_number, license_file_name,
		license_content_type, license_blob, status FROM enterprises WHERE user_id=?`, 303); err != nil {
		t.Fatal(err)
	}
	if stored.Name != fields["enterprise_name"] || stored.USCC != fields["uscc"] ||
		stored.LegalPerson != fields["legal_person"] || stored.LegalPersonIDCard != fields["legal_person_id_card"] ||
		stored.BankName != fields["bank_name"] || stored.BankAccountName != fields["bank_account_name"] ||
		stored.BankAccountNumber != fields["bank_account_number"] || stored.LicenseFileName != "business-license.pdf" ||
		stored.LicenseContentType != "application/pdf" || !bytes.Equal(stored.LicenseBlob, license) || stored.Status != "verified" {
		t.Fatalf("企业 KYC 未完整持久化: %+v", stored)
	}
}
