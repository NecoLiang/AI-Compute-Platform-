package modeldeploy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"tokenfactory/internal/catalog"

	"github.com/gin-gonic/gin"
	"github.com/jmoiron/sqlx"
)

func buyerRouter(svc *Service, uid int64) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	g := r.Group("/api/v1", func(c *gin.Context) { c.Set("user_id", uid); c.Next() })
	h := NewHandler(svc)
	h.RegisterMarketRoutes(g)
	h.RegisterBuyerRoutes(g)
	return r
}

// 未接入 S2 租约数据源: 参数合法时一律 50000 且说明原因, 不查库、不给假结论。
func TestDeployPlansUnwiredReturns50000(t *testing.T) {
	r := buyerRouter(NewService(nil, nil), 9)
	env := call(t, r, http.MethodGet, "/api/v1/orders/ORD20261028AB12/deploy-plans?model_id=1", nil)
	if env.Code != 50000 || env.Message != "托管租约能力尚未上线(S2 未接入), 暂无法计算部署方案" {
		t.Fatalf("未接入应 50000: %+v", env)
	}
	for path, code := range map[string]int{
		"/api/v1/orders/ORD20261028AB12/deploy-plans":                              40001, // 缺 model_id
		"/api/v1/orders/ORD20261028AB12/deploy-plans?model_id=x":                   40001,
		"/api/v1/orders/bad/deploy-plans?model_id=1":                               40001,
		"/api/v1/orders/ORD20261028AB12/deploy-plans?model_id=1&gpu_mem_util=0.99": 40001,
		"/api/v1/orders/ORD20261028AB12/deploy-plans?model_id=1&gpu_mem_util=0.7":  40001,
		"/api/v1/orders/ORD20261028AB12/deploy-plans?model_id=1&gpu_mem_util=90":   40001,
	} {
		if env := call(t, r, http.MethodGet, path, nil); env.Code != code {
			t.Errorf("%s: want %d got %+v", path, code, env)
		}
	}
}

// fakeLeases 仅用于测试: 按订单号返回预置快照或哨兵错误, 并记录调用参数。
type fakeLeases struct {
	snaps   map[string]*LeaseSnapshot
	errs    map[string]error
	gotUser int64
}

func (f *fakeLeases) ManagedLeaseSnapshot(_ context.Context, buyerID int64, orderNo string, _ int64) (*LeaseSnapshot, error) {
	f.gotUser = buyerID
	if err, ok := f.errs[orderNo]; ok {
		return nil, err
	}
	if s, ok := f.snaps[orderNo]; ok {
		return s, nil
	}
	return nil, ErrOrderNotFound
}

func h100Cards(n int) []CardSnapshot {
	out := make([]CardSnapshot, n)
	for i := range out {
		out[i] = CardSnapshot{UUID: fmt.Sprintf("GPU-%d", i), VRAMTotalBytes: 80 * gib64}
	}
	return out
}

func seedActiveModel(t *testing.T, db *sqlx.DB, key, verifiedGPU string) int64 {
	t.Helper()
	svc := NewService(NewRepository(db), catalog.NewService(catalog.NewRepository(db)))
	e := validEntry()
	e.ModelKey = key
	created, err := svc.Create(e, 1)
	if err != nil {
		t.Fatal(err)
	}
	v := &Verification{GPUModel: verifiedGPU, Cards: 2, TP: 2, MaxModelLen: 32768, Result: "pass", TestedAt: time.Now().Add(-time.Hour)}
	if _, err := svc.AddVerification(created.ID, v, 1); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Activate(created.ID); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

func TestModelsAndDeployPlansIntegration(t *testing.T) {
	db := setupDB(t)
	db.MustExec("UPDATE gpu_catalog SET compute_capability='9.0' WHERE model_name IN ('H100-80G','H800-80G')")
	activeID := seedActiveModel(t, db, "qwen3-32b-bf16", "H100-80G")
	svc := NewService(NewRepository(db), catalog.NewService(catalog.NewRepository(db)))
	draft := validEntry()
	draft.ModelKey = "qwen3-32b-draft"
	draftEntry, err := svc.Create(draft, 1)
	if err != nil {
		t.Fatal(err)
	}

	fl := &fakeLeases{
		snaps: map[string]*LeaseSnapshot{
			"ORD20261028H100": {GPUModel: "H100-80G", DriverVersion: "550.54.15", NVLink: true, ModelCached: true, Cards: h100Cards(2)},
			"ORD20261028H800": {GPUModel: "H800-80G", DriverVersion: "550.54.15", NVLink: true, ModelCached: true, Cards: h100Cards(2)},
			"ORD20261028A100": {GPUModel: "A100-80G", DriverVersion: "550.54.15", NVLink: true, ModelCached: true, Cards: h100Cards(2)},
		},
		errs: map[string]error{
			"ORD20261028DEDI": ErrNotManaged,
			"ORD20261028STOP": fmt.Errorf("lease L1: %w", ErrLeaseNotActive),
		},
	}
	svc.SetLeaseSource(fl)
	r := buyerRouter(svc, 42)

	// GET /models 只列 active, 带已验证卡型, 不暴露运维字段
	env := call(t, r, http.MethodGet, "/api/v1/models", nil)
	var models struct {
		List  []map[string]any `json:"list"`
		Total int              `json:"total"`
	}
	_ = json.Unmarshal(env.Data, &models)
	if env.Code != 0 || models.Total != 1 || models.List[0]["model_key"] != "qwen3-32b-bf16" {
		t.Fatalf("/models: %+v", env)
	}
	if v := models.List[0]["verified_gpu_models"].([]any); len(v) != 1 || v[0] != "H100-80G" {
		t.Fatalf("已验证卡型: %+v", models.List[0])
	}
	for _, hidden := range []string{"manifest", "engine_args", "metrics_map", "engine_image", "status"} {
		if _, ok := models.List[0][hidden]; ok {
			t.Fatalf("/models 不应暴露 %s", hidden)
		}
	}

	plans := func(order string, modelID int64, extra string) (envelope, Result) {
		env := call(t, r, http.MethodGet, fmt.Sprintf("/api/v1/orders/%s/deploy-plans?model_id=%d%s", order, modelID, extra), nil)
		var res Result
		_ = json.Unmarshal(env.Data, &res)
		return env, res
	}

	env, res := plans("ORD20261028H100", activeID, "")
	if env.Code != 0 || !res.Feasible || fl.gotUser != 42 || res.Basis.GPUMemUtilPct != 90 || res.Basis.ComputeCapability != "9.0" {
		t.Fatalf("H100 应可行: %+v %+v", env, res)
	}
	if len(res.Plans) != 2 || !res.Plans[1].Recommended || res.Plans[1].TP != 2 {
		t.Fatalf("2×80 推荐 tp=2: %+v", res.Plans)
	}
	if len(res.MissingAcks(false, false)) != 0 {
		t.Fatalf("已验证卡型无需确认: %+v", res.Checks)
	}

	// H800 未实测 → F8 需 untested_ack; 自定义利用率生效
	env, res = plans("ORD20261028H800", activeID, "&gpu_mem_util=0.85")
	if missing := res.MissingAcks(false, false); env.Code != 0 || !res.Feasible || len(missing) != 1 || missing[0].Ack != AckUntested || res.Basis.GPUMemUtilPct != 85 {
		t.Fatalf("H800 应可行但需 untested_ack: %+v", res)
	}

	// A100 在型号库中未登记 compute_capability → F1 无法判定, 不可行
	env, res = plans("ORD20261028A100", activeID, "")
	if env.Code != 0 || res.Feasible || len(res.Plans) != 0 {
		t.Fatalf("A100 未登记算力架构应不可行: %+v", res)
	}

	for _, c := range []struct {
		order string
		id    int64
		code  int
	}{
		{"ORD20261028NONE", activeID, 40400},      // 非本人/不存在
		{"ORD20261028DEDI", activeID, 40001},      // 非托管
		{"ORD20261028STOP", activeID, 40900},      // 租约非 active(包装后的哨兵)
		{"ORD20261028H100", draftEntry.ID, 40400}, // draft 条目买家不可见
		{"ORD20261028H100", 999999, 40400},
	} {
		if env, _ := plans(c.order, c.id, ""); env.Code != c.code {
			t.Errorf("%s/%d: want %d got %+v", c.order, c.id, c.code, env)
		}
	}
}
