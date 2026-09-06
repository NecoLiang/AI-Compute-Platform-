package catalog

import (
	"database/sql"
	"time"

	"github.com/jmoiron/sqlx"
)

// GPUModel 型号字典条目。平台自维护的权威源(docs/api/gpu-catalog-api.md):
// 国产以安可认证目录为骨架, 海外取数据中心与主流消费卡; 规格待运营按厂商官网复核。
type GPUModel struct {
	ID        int64  `db:"id" json:"id"`
	Vendor    string `db:"vendor" json:"vendor"`
	ModelName string `db:"model_name" json:"model_name"`
	Origin    string `db:"origin" json:"origin"` // domestic 国产 / international 海外
	Grade     string `db:"grade" json:"grade"`   // datacenter 数据中心 / consumer 消费级
	// 规格字段可空: 宁缺毋错, 未经厂商官网核实的数字不上。
	VRAMGB       *int     `db:"vram_gb" json:"vram_gb"`
	VRAMType     *string  `db:"vram_type" json:"vram_type"`
	FP16TFLOPS   *float64 `db:"fp16_tflops" json:"fp16_tflops"` // 口径: FP16 稠密 Tensor
	Interconnect *string  `db:"interconnect" json:"interconnect"`
	// SecureCertified 是否入选国家安可认证目录(2026-05 起「AI 训练与推理芯片」类)。
	SecureCertified bool      `db:"secure_certified" json:"secure_certified"`
	SpecSource      *string   `db:"spec_source" json:"spec_source,omitempty"` // 仅管理端返回
	Status          string    `db:"status" json:"status"`
	SortWeight      int       `db:"sort_weight" json:"sort_weight"`
	CreatedAt       time.Time `db:"created_at" json:"created_at"`
	UpdatedAt       time.Time `db:"updated_at" json:"updated_at"`
}

const gpuColumns = "id, vendor, model_name, origin, grade, vram_gb, vram_type, fp16_tflops, interconnect, secure_certified, spec_source, status, sort_weight, created_at, updated_at"

// Filter 列表筛选。IncludeDisabled 仅管理端为 true。
type Filter struct {
	Origin          string
	Grade           string
	Vendor          string
	Query           string // 型号模糊
	IncludeDisabled bool
}

type Repository struct{ db *sqlx.DB }

func NewRepository(db *sqlx.DB) *Repository { return &Repository{db: db} }

func (r *Repository) List(f Filter) ([]GPUModel, error) {
	where, args := "WHERE 1=1", []any{}
	if !f.IncludeDisabled {
		where += " AND status='enabled'"
	}
	if f.Origin != "" {
		where += " AND origin=?"
		args = append(args, f.Origin)
	}
	if f.Grade != "" {
		where += " AND grade=?"
		args = append(args, f.Grade)
	}
	if f.Vendor != "" {
		where += " AND vendor=?"
		args = append(args, f.Vendor)
	}
	if f.Query != "" {
		where += " AND model_name LIKE ?"
		args = append(args, "%"+f.Query+"%")
	}
	var list []GPUModel
	err := r.db.Select(&list,
		"SELECT "+gpuColumns+" FROM gpu_catalog "+where+" ORDER BY sort_weight DESC, vendor ASC, model_name ASC", args...)
	return list, err
}

func (r *Repository) Get(id int64) (*GPUModel, error) {
	var m GPUModel
	err := r.db.Get(&m, "SELECT "+gpuColumns+" FROM gpu_catalog WHERE id=?", id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &m, nil
}

func (r *Repository) Create(m *GPUModel) error {
	res, err := r.db.Exec(`INSERT INTO gpu_catalog
		(vendor, model_name, origin, grade, vram_gb, vram_type, fp16_tflops, interconnect, secure_certified, spec_source, sort_weight)
		VALUES (?,?,?,?,?,?,?,?,?,?,?)`,
		m.Vendor, m.ModelName, m.Origin, m.Grade, m.VRAMGB, m.VRAMType, m.FP16TFLOPS,
		m.Interconnect, m.SecureCertified, m.SpecSource, m.SortWeight)
	if err != nil {
		return err
	}
	m.ID, _ = res.LastInsertId()
	return nil
}

// Update 全量更新可维护字段(管理端表单整体提交)。存在性由 service 层先行校验,
// 这里不看 RowsAffected —— 值未变化时它也是 0, 不能当"不存在"用。
func (r *Repository) Update(m *GPUModel) error {
	_, err := r.db.Exec(`UPDATE gpu_catalog SET
		vendor=?, model_name=?, origin=?, grade=?, vram_gb=?, vram_type=?, fp16_tflops=?,
		interconnect=?, secure_certified=?, spec_source=?, status=?, sort_weight=? WHERE id=?`,
		m.Vendor, m.ModelName, m.Origin, m.Grade, m.VRAMGB, m.VRAMType, m.FP16TFLOPS,
		m.Interconnect, m.SecureCertified, m.SpecSource, m.Status, m.SortWeight, m.ID)
	return err
}
