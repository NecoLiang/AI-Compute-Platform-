package compute_test

import (
	"bytes"
	"encoding/json"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"testing"

	adminpkg "tokenfactory/internal/admin"
	"tokenfactory/internal/compute"

	"github.com/gin-gonic/gin"
	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

const supplierApplicationTestDB = "tokenfactory_supplier_application_test"

func TestSupplierApplicationLifecycleIsVisibleAndAuditable(t *testing.T) {
	db := setupSupplierApplicationTestDB(t)
	db.MustExec("INSERT INTO users (id, phone, email, password_hash) VALUES (101, '18800001101', '', ''), (900, '18800001900', 'admin@test.local', '')")
	db.MustExec("INSERT INTO user_roles (user_id, role) VALUES (101, 'buyer'), (900, 'admin')")
	db.MustExec("INSERT INTO enterprises (user_id, name, uscc, status) VALUES (101, '测试企业', '91310115MA1K4X2A7Q', 'verified')")

	computeService := compute.NewService(compute.NewRepository(db), db, "")
	adminService := adminpkg.NewService(adminpkg.NewRepository(db))
	gin.SetMode(gin.TestMode)
	router := gin.New()
	userRoutes := router.Group("/api/v1", userID(101))
	compute.NewHandler(computeService).RegisterAuthenticatedRoutes(userRoutes)
	adminRoutes := router.Group("/api/v1", userID(900))
	compute.NewHandler(computeService).RegisterAdminRoutes(adminRoutes)
	adminpkg.NewHandler(adminService).RegisterRoutes(adminRoutes)

	license := []byte("%PDF-1.4 supplier application")
	created := submitSupplierApplication(t, router, license)
	qualificationID := int64(created.Data.(map[string]any)["id"].(float64))

	userList := callJSON(t, router, http.MethodGet, "/api/v1/supplier-applications", nil)
	assertSingleStatus(t, userList.Data, "pending")
	adminList := callJSON(t, router, http.MethodGet, "/api/v1/admin/audits/qualifications", nil)
	assertSingleStatus(t, adminList.Data, "pending")
	assertQualificationDocument(t, router, qualificationID, license)

	callJSON(t, router, http.MethodPost, "/api/v1/admin/audits/qualifications/"+jsonNumber(qualificationID)+"/approve", nil)
	approved := callJSON(t, router, http.MethodGet, "/api/v1/supplier-applications", nil)
	assertSingleStatus(t, approved.Data, "verified")
	history := callJSON(t, router, http.MethodGet, "/api/v1/admin/audits/qualifications?status=all", nil)
	assertSingleStatus(t, history.Data, "verified")

	users := callJSON(t, router, http.MethodGet, "/api/v1/admin/users", nil)
	if !userHasRole(users.Data, 101, "supplier") {
		t.Fatal("审核通过后 admin 用户列表未显示 supplier 角色")
	}
	audits := callJSON(t, router, http.MethodGet, "/api/v1/admin/audit-logs?page=1&page_size=20", nil)
	if !hasAudit(audits.Data, "approve_qualification", "supplier_qualification", qualificationID) {
		t.Fatal("审核通过后审计日志不可读")
	}
}

func submitSupplierApplication(t *testing.T, router http.Handler, license []byte) envelope {
	t.Helper()
	var body bytes.Buffer
	form := multipart.NewWriter(&body)
	fields := map[string]string{
		"company_name": "测试企业", "credit_code": "91310115MA1K4X2A7Q",
		"representative": "测试负责人", "representative_id_number": "110101199001011237",
		"contact_method": "18800001101", "bank_name": "测试银行", "account_name": "测试企业",
		"account_number": "6225888888888888", "facility_address": "上海市浦东新区测试路 1 号",
		"has_idc_license": "true", "power_description": "双路市电与 UPS", "cooling_description": "液冷与风冷混合系统",
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
	if _, err := file.Write(license); err != nil {
		t.Fatal(err)
	}
	if err := form.Close(); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/supplier-applications", &body)
	request.Header.Set("content-type", form.FormDataContentType())
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var result envelope
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil || response.Code != http.StatusOK || result.Code != 0 {
		t.Fatalf("提交供给方申请失败: status=%d body=%s err=%v", response.Code, response.Body.String(), err)
	}
	return result
}

func assertQualificationDocument(t *testing.T, router http.Handler, qualificationID int64, want []byte) {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, "/api/v1/admin/audits/qualifications/"+jsonNumber(qualificationID)+"/document", nil)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), want) {
		t.Fatalf("管理员无法读取申请附件: status=%d content-type=%q body=%q", response.Code, response.Header().Get("content-type"), response.Body.Bytes())
	}
}

type envelope struct {
	Code int `json:"code"`
	Data any `json:"data"`
}

func callJSON(t *testing.T, router http.Handler, method, path string, body any) envelope {
	t.Helper()
	var payload bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&payload).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	request := httptest.NewRequest(method, path, &payload)
	if body != nil {
		request.Header.Set("content-type", "application/json")
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	var result envelope
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatalf("%s %s returned invalid JSON: status=%d body=%s", method, path, response.Code, response.Body.String())
	}
	if response.Code != http.StatusOK || result.Code != 0 {
		t.Fatalf("%s %s failed: status=%d body=%s", method, path, response.Code, response.Body.String())
	}
	return result
}

func assertSingleStatus(t *testing.T, data any, want string) {
	t.Helper()
	items := data.([]any)
	if len(items) != 1 || items[0].(map[string]any)["status"] != want {
		t.Fatalf("items=%v, want one %s application", items, want)
	}
}

func userHasRole(data any, userID int64, role string) bool {
	for _, item := range data.([]any) {
		user := item.(map[string]any)
		if int64(user["id"].(float64)) != userID {
			continue
		}
		for _, value := range user["roles"].([]any) {
			if value == role {
				return true
			}
		}
	}
	return false
}

func hasAudit(data any, action, targetType string, targetID int64) bool {
	page := data.(map[string]any)
	items, _ := page["list"].([]any)
	for _, item := range items {
		log := item.(map[string]any)
		if log["action"] == action && log["target_type"] == targetType && int64(log["target_id"].(float64)) == targetID {
			return true
		}
	}
	return false
}

func jsonNumber(value int64) string {
	return strconv.FormatInt(value, 10)
}

func userID(id int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Set("user_id", id)
		c.Next()
	}
}

func setupSupplierApplicationTestDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 TEST_MYSQL_DSN, 跳过供给方申请集成测试")
	}
	root, err := sqlx.Connect("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	root.MustExec("DROP DATABASE IF EXISTS " + supplierApplicationTestDB)
	root.MustExec("CREATE DATABASE " + supplierApplicationTestDB + " CHARACTER SET utf8mb4")
	db, err := sqlx.Connect("mysql", dsn+supplierApplicationTestDB+"?parseTime=true")
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range []string{
		`CREATE TABLE users (id BIGINT PRIMARY KEY, phone VARCHAR(20) NOT NULL, email VARCHAR(128), password_hash VARCHAR(256) NOT NULL, status ENUM('active','frozen') DEFAULT 'active', created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP)`,
		`CREATE TABLE user_roles (id BIGINT PRIMARY KEY AUTO_INCREMENT, user_id BIGINT NOT NULL, role ENUM('buyer','supplier','vendor','funder','operator','admin') NOT NULL, created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP, UNIQUE KEY user_role (user_id, role))`,
		`CREATE TABLE user_kyc (id BIGINT PRIMARY KEY AUTO_INCREMENT, user_id BIGINT NOT NULL UNIQUE, status ENUM('pending','verified','rejected') DEFAULT 'pending')`,
		`CREATE TABLE enterprises (id BIGINT PRIMARY KEY AUTO_INCREMENT, user_id BIGINT NOT NULL UNIQUE, name VARCHAR(128) NOT NULL, uscc VARCHAR(32) NOT NULL, status ENUM('pending','verified','rejected') DEFAULT 'pending')`,
		`CREATE TABLE supplier_qualifications (id BIGINT PRIMARY KEY AUTO_INCREMENT, user_id BIGINT NOT NULL, qual_type VARCHAR(64) NOT NULL, cert_name VARCHAR(128) NOT NULL, cert_number VARCHAR(64), cert_url VARCHAR(512), metadata_json TEXT, license_file_name VARCHAR(255), license_content_type VARCHAR(128), license_blob MEDIUMBLOB, expires_at DATE, status ENUM('pending','verified','rejected','expired') DEFAULT 'pending', rejected_reason VARCHAR(256), created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)`,
		`CREATE TABLE audit_logs (id BIGINT PRIMARY KEY AUTO_INCREMENT, operator_id BIGINT, action VARCHAR(64) NOT NULL, target_type VARCHAR(32), target_id BIGINT, before_value TEXT, after_value TEXT, ip VARCHAR(45), created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP)`,
	} {
		db.MustExec(statement)
	}
	t.Cleanup(func() {
		db.Close()
		root, err := sqlx.Connect("mysql", dsn)
		if err == nil {
			root.Exec("DROP DATABASE IF EXISTS " + supplierApplicationTestDB)
			root.Close()
		}
	})
	return db
}
