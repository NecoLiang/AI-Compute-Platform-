package compute_test

import (
	"bytes"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
	"tokenfactory/internal/legal"

	"tokenfactory/internal/auth"
	"tokenfactory/internal/sms"
	"tokenfactory/internal/user"
	"tokenfactory/pkg/middleware"

	"github.com/gin-gonic/gin"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
)

func TestRegistrationRecordsExplicitVersionedConsent(t *testing.T) {
	db := setupTradeDB(t)
	addr := os.Getenv("TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("TEST_REDIS_ADDR is required")
	}
	rdb := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { rdb.Close() })
	roles := user.NewRepository(db)
	secret := "consent-test-access-secret-at-least-32-characters"
	svc := auth.NewService(auth.NewRepository(db), roles, rdb, sms.NewPreviewSender(), time.Minute, secret, secret+"-refresh", 900, 3600)
	h := auth.NewHandler(svc, auth.NewCapVerifier("", "", "consent-test"))
	router := gin.New()
	h.RegisterPublicRoutes(router.Group("/api/v1"))
	h.RegisterProtectedRoutes(router.Group("/api/v1", middleware.AuthRequired(secret, rdb, roles)))
	request := func(method, path string, body any, token string, code int) envelope {
		t.Helper()
		encoded, err := json.Marshal(body)
		require.NoError(t, err)
		req := httptest.NewRequest(method, "/api/v1"+path, bytes.NewReader(encoded))
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		router.ServeHTTP(w, req)
		var result envelope
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
		require.Equal(t, code, result.Code, "%s %s: %s", method, path, result.Message)
		return result
	}
	phone := fmt.Sprintf("18803%06d", time.Now().UnixNano()%1000000)
	sent := request(http.MethodPost, "/auth/sms/code", map[string]any{"phone": phone, "purpose": "register", "captcha_token": "consent-test"}, "", 0)
	input := map[string]any{"phone": phone, "sms_code": sent.Data.(map[string]any)["preview_code"], "agree_tos": true}
	request(http.MethodPost, "/auth/register", input, "", 40001)
	input["terms_version"], input["privacy_version"] = "old-version", "2026-09-06.1"
	request(http.MethodPost, "/auth/register", input, "", 40001)
	input["terms_version"] = "2026-09-06.1"
	input["agree_tos"] = false
	request(http.MethodPost, "/auth/register", input, "", 40001)
	input["agree_tos"] = true
	input["accepted_at"] = "2000-01-01T00:00:00Z"
	registered := request(http.MethodPost, "/auth/register", input, "", 0)
	token := registered.Data.(map[string]any)["access_token"].(string)
	request(http.MethodGet, "/auth/consents", nil, "", 40100)
	records := request(http.MethodGet, "/auth/consents", nil, token, 0).Data.([]any)
	require.Len(t, records, 2)
	documents := map[string]bool{}
	for _, value := range records {
		record := value.(map[string]any)
		documents[record["document"].(string)] = true
		require.Equal(t, "2026-09-06.1", record["version"])
		require.Equal(t, "registration", record["action"])
		accepted, err := time.Parse(time.RFC3339Nano, record["accepted_at"].(string))
		require.NoError(t, err)
		require.WithinDuration(t, time.Now(), accepted, time.Minute)
	}
	require.Equal(t, map[string]bool{"terms": true, "privacy": true}, documents)
}

func TestTradeRecordsListingAndUsageConsentWithBusinessChanges(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	supplier, buyer, admin := tradeRouter(db, 101), tradeRouter(db, 102), tradeRouter(db, 104)
	input := tradeProductInput()
	delete(input, "compliance_version")
	tradeRequest(t, supplier, "POST", "/api/v1/supplier/products", input, 40001)
	input["compliance_version"] = "2026-09-06.1"
	created := tradeRequest(t, supplier, "POST", "/api/v1/supplier/products", input, 0)
	id := int64(created.Data.(map[string]any)["id"].(float64))
	records := func(id int64) []any {
		r := gin.New()
		svc := auth.NewService(auth.NewRepository(db), user.NewRepository(db), nil, nil, time.Minute, "", "", 900, 3600)
		auth.NewHandler(svc, nil).RegisterProtectedRoutes(r.Group("/api/v1", userID(id)))
		return tradeRequest(t, r, "GET", "/api/v1/auth/consents", nil, 0).Data.([]any)
	}
	auditPath := fmt.Sprintf("/api/v1/admin/audits/products/%d", id)
	tradeRequest(t, admin, "POST", auditPath+"/reject", map[string]string{"reason": "correct specification"}, 0)
	tradeRequest(t, supplier, "PUT", fmt.Sprintf("/api/v1/supplier/products/%d", id), input, 0)
	listing := records(101)
	require.Len(t, listing, 2)
	for _, value := range listing {
		item := value.(map[string]any)
		require.Equal(t, "resource-listing-rules", item["document"])
		require.Equal(t, "2026-09-06.1", item["version"])
		require.Equal(t, fmt.Sprint(id), item["reference"])
	}
	tradeRequest(t, admin, "POST", auditPath+"/approve", nil, 0)
	order := map[string]any{"product_id": id, "quantity": 2, "duration": 3, "compliance_agreed": true, "compliance_version": "old-version"}
	tradeRequest(t, buyer, "POST", "/api/v1/orders", order, 40001)
	require.Empty(t, records(102))
	order["compliance_version"] = "2026-09-06.1"
	placed := tradeRequest(t, buyer, "POST", "/api/v1/orders", order, 0)
	usage := records(102)
	require.Len(t, usage, 1)
	item := usage[0].(map[string]any)
	require.Equal(t, "resource-usage-rules", item["document"])
	require.Equal(t, "2026-09-06.1", item["version"])
	require.Equal(t, "order", item["action"])
	require.Equal(t, placed.Data.(map[string]any)["order_no"], item["reference"])
	require.Equal(t, float64(13800), placed.Data.(map[string]any)["total_amount"])
	order["quantity"] = 99
	tradeRequest(t, buyer, "POST", "/api/v1/orders", order, 40900)
	require.Len(t, records(102), 1, "failed orders must not record acceptance")

	// A consent storage failure must roll back the business write and reserved stock.
	_, err := db.Exec("DROP TABLE legal_consents")
	require.NoError(t, err)
	order["quantity"] = 1
	tradeRequest(t, buyer, "POST", "/api/v1/orders", order, 50000)
	tradeRequest(t, supplier, "POST", "/api/v1/supplier/products", input, 50000)
	var stock, orders, products int
	require.NoError(t, db.Get(&stock, "SELECT stock FROM products WHERE id=?", id))
	require.NoError(t, db.Get(&orders, "SELECT COUNT(*) FROM orders"))
	require.NoError(t, db.Get(&products, "SELECT COUNT(*) FROM products"))
	require.Equal(t, 6, stock)
	require.Equal(t, 1, orders)
	require.Equal(t, 1, products)
}

func TestKYCSensitiveConsentIsSeparateAndAtomic(t *testing.T) {
	db := setupTradeDB(t)
	seedTradeUsers(db)
	for index, kind := range []string{"personal", "enterprise"} {
		accountID := int64(103 + index)
		router := gin.New()
		user.NewHandler(user.NewService(user.NewRepository(db))).RegisterRoutes(router.Group("/api/v1", userID(accountID)))
		input := map[string]any{"real_name": "Consent test", "id_card": "110101199001011234", "enterprise_name": "Consent test enterprise", "uscc": "91110000123456789X", "legal_person": "Consent test", "legal_person_id_card": "110101199001011234", "bank_name": "Test bank", "bank_account_name": "Test enterprise", "bank_account_number": "1234567890", "privacy_version": "2026-09-06.1"}
		post := func(expected int) {
			t.Helper()
			var body bytes.Buffer
			contentType := "application/json"
			if kind == "personal" {
				require.NoError(t, json.NewEncoder(&body).Encode(input))
			} else {
				writer := multipart.NewWriter(&body)
				for key, value := range input {
					require.NoError(t, writer.WriteField(key, fmt.Sprint(value)))
				}
				file, err := writer.CreateFormFile("business_license", "consent-test.pdf")
				require.NoError(t, err)
				_, err = file.Write([]byte("%PDF-1.4\nTest license"))
				require.NoError(t, err)
				require.NoError(t, writer.Close())
				contentType = writer.FormDataContentType()
			}
			req := httptest.NewRequest("POST", "/api/v1/user/kyc/"+kind, &body)
			req.Header.Set("Content-Type", contentType)
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			var result envelope
			require.NoError(t, json.Unmarshal(w.Body.Bytes(), &result))
			require.Equal(t, expected, result.Code, "%s: %s", kind, result.Message)
		}
		post(40001)
		input["sensitive_data_agreed"] = true
		input["privacy_version"] = "old-version"
		post(40001)
		records, err := legal.List(db, accountID)
		require.NoError(t, err)
		require.Empty(t, records)
		input["privacy_version"] = "2026-09-06.1"
		post(0)
		records, err = legal.List(db, accountID)
		require.NoError(t, err)
		require.Len(t, records, 1)
		require.Equal(t, "privacy", records[0].Document)
		require.Equal(t, "kyc_"+kind, records[0].Action)
		require.Equal(t, "2026-09-06.1", records[0].Version)
		require.WithinDuration(t, time.Now(), records[0].AcceptedAt, time.Minute)
		post(40900)
	}
	db.MustExec("DROP TABLE legal_consents")
	router := gin.New()
	user.NewHandler(user.NewService(user.NewRepository(db))).RegisterRoutes(router.Group("/api/v1", userID(101)))
	tradeRequest(t, router, "POST", "/api/v1/user/kyc/personal", map[string]any{"real_name": "Consent rollback", "id_card": "110101199001011234", "sensitive_data_agreed": true, "privacy_version": "2026-09-06.1"}, 50000)
	var count int
	require.NoError(t, db.Get(&count, "SELECT COUNT(*) FROM user_kyc WHERE user_id=101"))
	require.Zero(t, count)
}
