package catalog

import (
	"os"
	"strings"
	"testing"

	_ "github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

func intp(v int) *int          { return &v }
func f64p(v float64) *float64  { return &v }
func strp(s string) *string    { return &s }

// ===== 纯校验单测 =====

func TestValidate(t *testing.T) {
	ok := &GPUModel{Vendor: "NVIDIA", ModelName: "A100-80G", Origin: "international", Grade: "datacenter"}
	if err := validate(ok); err != nil {
		t.Fatalf("合法条目报错: %v", err)
	}
	cases := []*GPUModel{
		{Vendor: "", ModelName: "x", Origin: "domestic", Grade: "datacenter"},
		{Vendor: "v", ModelName: "x", Origin: "海外", Grade: "datacenter"},
		{Vendor: "v", ModelName: "x", Origin: "domestic", Grade: "旗舰"},
		{Vendor: "v", ModelName: "x", Origin: "domestic", Grade: "consumer", VRAMGB: intp(-1)},
		{Vendor: "v", ModelName: "x", Origin: "domestic", Grade: "consumer", FP16TFLOPS: f64p(0)},
	}
	for i, m := range cases {
		if err := validate(m); err == nil {
			t.Errorf("用例 %d 应校验失败: %+v", i, m)
		}
	}
}

// ===== 走真库的 CRUD/筛选集成测试 (TEST_MYSQL_DSN 门控, 同其他模块) =====

const catalogTestDB = "tokenfactory_catalog_test"

func setupCatalogDB(t *testing.T) *Service {
	t.Helper()
	dsn := os.Getenv("TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("未设置 TEST_MYSQL_DSN, 跳过型号库集成测试")
	}
	root, err := sqlx.Connect("mysql", dsn)
	if err != nil {
		t.Fatalf("连接 MySQL 失败: %v", err)
	}
	defer root.Close()
	root.MustExec("DROP DATABASE IF EXISTS " + catalogTestDB)
	root.MustExec("CREATE DATABASE " + catalogTestDB + " CHARACTER SET utf8mb4")
	db, err := sqlx.Connect("mysql", strings.Replace(dsn, "/?", "/"+catalogTestDB+"?", 1))
	if err != nil {
		t.Fatalf("连接测试库失败: %v", err)
	}
	db.MustExec(`CREATE TABLE gpu_catalog (
		id BIGINT AUTO_INCREMENT PRIMARY KEY,
		vendor VARCHAR(32) NOT NULL, model_name VARCHAR(64) NOT NULL,
		origin ENUM('domestic','international') NOT NULL,
		grade ENUM('datacenter','consumer') NOT NULL DEFAULT 'datacenter',
		vram_gb INT NULL, vram_type VARCHAR(16) NULL, fp16_tflops DECIMAL(8,1) NULL,
		interconnect VARCHAR(32) NULL, secure_certified TINYINT(1) NOT NULL DEFAULT 0,
		spec_source VARCHAR(255) NULL,
		status ENUM('enabled','disabled') NOT NULL DEFAULT 'enabled',
		sort_weight INT NOT NULL DEFAULT 0,
		created_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP,
		updated_at TIMESTAMP DEFAULT CURRENT_TIMESTAMP ON UPDATE CURRENT_TIMESTAMP,
		UNIQUE KEY uk_model (model_name)
	)`)
	t.Cleanup(func() {
		db.MustExec("DROP DATABASE IF EXISTS " + catalogTestDB)
		db.Close()
	})
	return NewService(NewRepository(db))
}

func TestCatalog_CRUDAndFilters(t *testing.T) {
	svc := setupCatalogDB(t)

	a100 := &GPUModel{Vendor: "NVIDIA", ModelName: "A100-80G", Origin: "international",
		Grade: "datacenter", VRAMGB: intp(80), FP16TFLOPS: f64p(312), SortWeight: 86,
		SpecSource: strp("NVIDIA datasheet")}
	ascend := &GPUModel{Vendor: "华为昇腾", ModelName: "昇腾910B", Origin: "domestic",
		Grade: "datacenter", VRAMGB: intp(64), SecureCertified: true, SortWeight: 100}
	if err := svc.Create(a100); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Create(ascend); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// 型号唯一
	if err := svc.Create(&GPUModel{Vendor: "NVIDIA", ModelName: "A100-80G", Origin: "international", Grade: "datacenter"}); err == nil || !strings.Contains(err.Error(), "已存在") {
		t.Fatalf("重复型号应报已存在: %v", err)
	}

	// 公开列表: 排序按权重, 不含 spec_source
	list, err := svc.PublicList(Filter{})
	if err != nil || len(list) != 2 {
		t.Fatalf("公开列表: %v %d", err, len(list))
	}
	if list[0].ModelName != "昇腾910B" || !list[0].SecureCertified {
		t.Fatalf("权重排序/安可标记错误: %+v", list[0])
	}
	if list[1].SpecSource != nil {
		t.Fatal("公开列表不应暴露 spec_source")
	}

	// 筛选
	if l, _ := svc.PublicList(Filter{Origin: "domestic"}); len(l) != 1 || l[0].ModelName != "昇腾910B" {
		t.Fatalf("origin 筛选失败: %+v", l)
	}
	if l, _ := svc.PublicList(Filter{Query: "A100"}); len(l) != 1 {
		t.Fatalf("模糊筛选失败: %+v", l)
	}

	// 下架后从公开列表消失, 管理端仍可见
	a100.Status = "disabled"
	if _, err := svc.Update(a100.ID, a100); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if l, _ := svc.PublicList(Filter{}); len(l) != 1 {
		t.Fatalf("下架后公开列表应剩 1 条: %d", len(l))
	}
	adminList, _ := svc.AdminList(Filter{})
	if len(adminList) != 2 || adminList[1].SpecSource == nil {
		t.Fatalf("管理端应含 disabled 且带 spec_source: %+v", adminList)
	}

	// 更新不存在的 id
	ascend.Status = "enabled"
	if _, err := svc.Update(9999, ascend); err == nil || !strings.Contains(err.Error(), "不存在") {
		t.Fatalf("不存在的 id 应报错: %v", err)
	}
}
