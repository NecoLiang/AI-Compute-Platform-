package modeldeploy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"tokenfactory/internal/catalog"

	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

// setupDB 在 TEST_MYSQL_DSN 指向的一次性实例上新建库并按顺序执行全部真实迁移(含 030)。
func setupDB(t *testing.T) *sqlx.DB {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("TEST_MYSQL_DSN is required for model catalog integration tests")
	}
	cfg, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	cfg.DBName = ""
	root, err := sqlx.Connect("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("omnis_modeldeploy_test_%d", time.Now().UnixNano())
	root.MustExec("CREATE DATABASE " + name + " CHARACTER SET utf8mb4")
	cfg.DBName, cfg.ParseTime, cfg.MultiStatements = name, true, true
	db, err := sqlx.Connect("mysql", cfg.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close(); root.Exec("DROP DATABASE " + name); root.Close() })
	paths, err := filepath.Glob("../../migrations/*.up.sql")
	if err != nil || len(paths) == 0 {
		t.Fatalf("migrations: %v", err)
	}
	for _, p := range paths {
		data, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(data)); err != nil {
			t.Fatalf("%s: %v", p, err)
		}
	}
	return db
}

func adminRouter(db *sqlx.DB) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	svc := NewService(NewRepository(db), catalog.NewService(catalog.NewRepository(db)))
	g := r.Group("/api/v1", func(c *gin.Context) { c.Set("user_id", int64(1)); c.Next() })
	NewHandler(svc).RegisterAdminRoutes(g)
	return r
}

type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

func call(t *testing.T, r *gin.Engine, method, path string, body any) envelope {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var env envelope
	if err := json.Unmarshal(w.Body.Bytes(), &env); err != nil {
		t.Fatalf("%s %s: 非 JSON 响应 %d %s", method, path, w.Code, w.Body.String())
	}
	return env
}

func entryBody(key string) map[string]any {
	e := validEntry()
	var manifest, args, metrics any
	_ = json.Unmarshal(e.Manifest, &manifest)
	_ = json.Unmarshal(e.EngineArgs, &args)
	_ = json.Unmarshal(e.MetricsMap, &metrics)
	return map[string]any{
		"model_key": key, "display_name": e.DisplayName, "repo": e.Repo, "revision": e.Revision,
		"manifest": manifest, "weights_bytes": e.WeightsBytes, "quant": e.Quant,
		"min_compute_capability": e.MinComputeCapability, "valid_tp": e.ValidTP,
		"kv_bytes_per_token": e.KVBytesPerToken, "runtime_overhead_bytes": e.RuntimeOverheadBytes,
		"max_model_len": e.MaxModelLen, "engine_image": e.EngineImage, "min_driver_version": e.MinDriverVersion,
		"engine_args": args, "metrics_map": metrics, "license": e.License, "commercial_use": e.CommercialUse,
	}
}

func verificationBody(gpu, result string) map[string]any {
	return map[string]any{"gpu_model": gpu, "cards": 2, "tp": 2, "max_model_len": 32768,
		"measured_overhead_bytes": 4 << 30, "result": result, "notes": "真机启动+推理通过",
		"tested_at": time.Now().Add(-time.Hour).Format(time.RFC3339)}
}

func TestModelCatalogLifecycleIntegration(t *testing.T) {
	db := setupDB(t)
	r := adminRouter(db)
	base := "/api/v1/admin/model-catalog"

	created := call(t, r, http.MethodPost, base, entryBody("qwen3-32b-bf16"))
	if created.Code != 0 {
		t.Fatalf("create: %+v", created)
	}
	var e ModelEntry
	_ = json.Unmarshal(created.Data, &e)
	if e.Status != StatusDraft || e.ValidTP != "1,2,4,8" || e.CreatedBy != 1 {
		t.Fatalf("新建条目应为 draft 且规范化: %+v", e)
	}
	id := fmt.Sprintf("%s/%d", base, e.ID)

	if env := call(t, r, http.MethodPost, base, entryBody("qwen3-32b-bf16")); env.Code != 40900 {
		t.Fatalf("重复 model_key 应 40900: %+v", env)
	}
	bad := entryBody("bad-entry")
	bad["engine_args"] = map[string]any{"tensor-parallel-size": 4}
	if env := call(t, r, http.MethodPost, base, bad); env.Code != 40001 {
		t.Fatalf("白名单外参数应 40001: %+v", env)
	}

	upd := entryBody("qwen3-32b-bf16")
	upd["display_name"] = "Qwen3 32B (BF16)"
	if env := call(t, r, http.MethodPut, id, upd); env.Code != 0 {
		t.Fatalf("draft 可编辑: %+v", env)
	}
	if env := call(t, r, http.MethodPut, id, upd); env.Code != 0 {
		t.Fatalf("相同内容重复提交不应判冲突: %+v", env)
	}

	if env := call(t, r, http.MethodPost, id+"/activate", nil); env.Code != 40001 {
		t.Fatalf("无验证记录上线应 40001: %+v", env)
	}
	if env := call(t, r, http.MethodPost, id+"/verifications", verificationBody("H100-80G", "fail")); env.Code != 0 {
		t.Fatalf("录入 fail 记录: %+v", env)
	}
	if env := call(t, r, http.MethodPost, id+"/activate", nil); env.Code != 40001 {
		t.Fatalf("只有 fail 记录上线应 40001: %+v", env)
	}
	if env := call(t, r, http.MethodPost, id+"/verifications", verificationBody("H100-SXM-不存在", "pass")); env.Code != 40001 {
		t.Fatalf("型号库外卡型应 40001: %+v", env)
	}
	future := verificationBody("H100-80G", "pass")
	future["tested_at"] = time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	if env := call(t, r, http.MethodPost, id+"/verifications", future); env.Code != 40001 {
		t.Fatalf("未来时间应 40001: %+v", env)
	}
	if env := call(t, r, http.MethodPost, id+"/verifications", verificationBody("H100-80G", "pass")); env.Code != 0 {
		t.Fatalf("录入 pass 记录: %+v", env)
	}

	act := call(t, r, http.MethodPost, id+"/activate", nil)
	_ = json.Unmarshal(act.Data, &e)
	if act.Code != 0 || e.Status != StatusActive {
		t.Fatalf("有 pass 记录应可上线: %+v", act)
	}
	if env := call(t, r, http.MethodPost, id+"/activate", nil); env.Code != 0 {
		t.Fatalf("重复上线应幂等: %+v", env)
	}
	if env := call(t, r, http.MethodPut, id, upd); env.Code != 40900 {
		t.Fatalf("active 不可编辑应 40900: %+v", env)
	}
	if env := call(t, r, http.MethodPost, id+"/verifications", verificationBody("A100-80G", "pass")); env.Code != 0 {
		t.Fatalf("active 条目可补录新卡型: %+v", env)
	}

	detail := call(t, r, http.MethodGet, id, nil)
	var d EntryDetail
	_ = json.Unmarshal(detail.Data, &d)
	if detail.Code != 0 || len(d.Verifications) != 3 {
		t.Fatalf("详情应带 3 条验证记录: %+v", detail)
	}
	passed, err := NewRepository(db).PassedGPUModels([]int64{e.ID})
	if err != nil || len(passed[e.ID]) != 2 || passed[e.ID][0] != "A100-80G" || passed[e.ID][1] != "H100-80G" {
		t.Fatalf("已验证卡型应为 A100-80G,H100-80G: %v %v", passed, err)
	}

	listEnv := call(t, r, http.MethodGet, base+"?status=active", nil)
	var list struct {
		Total int `json:"total"`
	}
	_ = json.Unmarshal(listEnv.Data, &list)
	if listEnv.Code != 0 || list.Total != 1 {
		t.Fatalf("active 列表应为 1 条: %+v", listEnv)
	}
	if env := call(t, r, http.MethodGet, base+"?status=online", nil); env.Code != 40001 {
		t.Fatalf("非法 status 应 40001: %+v", env)
	}

	ret := call(t, r, http.MethodPost, id+"/retire", nil)
	_ = json.Unmarshal(ret.Data, &e)
	if ret.Code != 0 || e.Status != StatusRetired {
		t.Fatalf("退役: %+v", ret)
	}
	if env := call(t, r, http.MethodPost, id+"/retire", nil); env.Code != 0 {
		t.Fatalf("重复退役应幂等: %+v", env)
	}
	if env := call(t, r, http.MethodPost, id+"/activate", nil); env.Code != 40900 {
		t.Fatalf("已退役不可上线应 40900: %+v", env)
	}
	if env := call(t, r, http.MethodPost, id+"/verifications", verificationBody("H100-80G", "pass")); env.Code != 40900 {
		t.Fatalf("已退役不再接受验证记录应 40900: %+v", env)
	}
	if env := call(t, r, http.MethodGet, base+"/999999", nil); env.Code != 40400 {
		t.Fatalf("不存在应 40400: %+v", env)
	}
}

// TestMigration030Schema 迁移 030 的结构断言: gpu_catalog.compute_capability 与五张新表存在。
func TestMigration030Schema(t *testing.T) {
	db := setupDB(t)
	var n int
	if err := db.Get(&n, `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema=DATABASE() AND table_name='gpu_catalog' AND column_name='compute_capability'`); err != nil || n != 1 {
		t.Fatalf("gpu_catalog.compute_capability 缺失: %d %v", n, err)
	}
	if err := db.Get(&n, `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_schema=DATABASE() AND table_name='supplier_nodes' AND column_name='model_cache_gb'`); err != nil || n != 1 {
		t.Fatalf("supplier_nodes.model_cache_gb 缺失: %d %v", n, err)
	}
	for _, tbl := range []string{"model_catalog", "model_catalog_verifications", "node_model_cache", "model_deployments", "deployment_health_samples"} {
		if err := db.Get(&n, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name=?`, tbl); err != nil || n != 1 {
			t.Fatalf("表 %s 缺失: %v", tbl, err)
		}
	}
	data, err := os.ReadFile("../../migrations/030_model_deployment.down.sql")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(string(data)); err == nil {
		t.Fatal("030 down 必须无条件拒绝执行")
	}
	if err := db.Get(&n, `SELECT COUNT(*) FROM information_schema.tables WHERE table_schema=DATABASE() AND table_name='model_catalog'`); err != nil || n != 1 {
		t.Fatal("down 被拒后 model_catalog 必须仍在")
	}
}
