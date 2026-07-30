package intermediary

// C-07 中登网动产融资登记查询
//
// 数据来源声明（重要，不是 TODO）:
//
//	中国人民银行征信中心「动产融资统一登记公示系统」(俗称中登网, zhongdengwang) **没有对外开放的数据接口**，
//	既无 OpenAPI 也无授权的批量下载渠道。因此本模块的数据来源只有一条: 平台运营人员登录官方系统
//	人工查询后，把查询结果录入本平台 (data_source 固定为 'manual')。
//
//	这不是"待对接接口"的临时方案，而是 v1 的最终形态。任何对外响应都必须携带 Disclaimer，
//	明确告知使用者数据为人工录入、仅供参考、以官方系统实时查询结果为准。
//	若将来官方开放接口，需要新增 data_source 枚举值并同步修改 Disclaimer 文案。

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"
	"tokenfactory/pkg/errcode"
	"tokenfactory/pkg/response"

	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"github.com/jmoiron/sqlx"
)

// Disclaimer 合规声明，所有中登网登记查询响应必须携带。
const Disclaimer = "登记数据由平台运营依据中国人民银行征信中心动产融资统一登记公示系统查询结果人工录入，仅供参考，请以官方系统实时查询结果为准。"

// ===== Model =====

// CollateralRegistration 中登网动产融资登记记录（人工录入）。
type CollateralRegistration struct {
	ID             int64      `db:"id" json:"id"`
	RegNo          string     `db:"reg_no" json:"reg_no"`
	RegType        string     `db:"reg_type" json:"reg_type"`
	LessorName     string     `db:"lessor_name" json:"lessor_name"`
	LesseeName     string     `db:"lessee_name" json:"lessee_name"`
	LesseeUscc     string     `db:"lessee_uscc" json:"lessee_uscc"`
	CollateralDesc string     `db:"collateral_desc" json:"collateral_desc"`
	RegStartDate   *time.Time `db:"reg_start_date" json:"reg_start_date"`
	RegEndDate     *time.Time `db:"reg_end_date" json:"reg_end_date"`
	Status         string     `db:"status" json:"status"`
	DataSource     string     `db:"data_source" json:"data_source"`
	SourceNote     string     `db:"source_note" json:"source_note"`
	VerifiedAt     *time.Time `db:"verified_at" json:"verified_at"`
	CreatedBy      int64      `db:"created_by" json:"created_by"`
	CreatedAt      time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at" json:"updated_at"`
}

// ===== Validation =====

// CollateralValidationError 表示入参校验失败，映射为 40001。
type CollateralValidationError struct {
	Field  string `json:"field"`
	Reason string `json:"reason"`
}

func (e *CollateralValidationError) Error() string { return e.Field + ": " + e.Reason }

func collateralInvalid(field, reason string) *CollateralValidationError {
	return &CollateralValidationError{Field: field, Reason: reason}
}

var (
	ErrCollateralNotFound = fmt.Errorf("collateral registration not found")
	ErrRegNoConflict      = fmt.Errorf("登记编号已存在，请检查是否重复录入")
	ErrQueryTooBroad      = fmt.Errorf("承租人名称与统一社会信用代码至少填写一项")
)

var validRegTypes = map[string]bool{
	"finance_lease": true, "mortgage": true, "factoring": true, "other": true,
}

const usccLength = 18

// USCC 18 位: 数字 + 大写字母（不含 I O Z S V，国标 GB32100-2015 的字符集）
const usccCharset = "0123456789ABCDEFGHJKLMNPQRTUWXY"

// ValidateUSCC 校验统一社会信用代码。空字符串视为"未填写"由调用方决定是否允许。
func ValidateUSCC(uscc string) error {
	if len(uscc) != usccLength {
		return collateralInvalid("lessee_uscc", "统一社会信用代码必须是 18 位")
	}
	for _, ch := range uscc {
		if !strings.ContainsRune(usccCharset, ch) {
			return collateralInvalid("lessee_uscc", "统一社会信用代码含非法字符（只允许数字与大写字母，不含 I/O/S/V/Z）")
		}
	}
	return nil
}

// ValidateRegDateRange 校验登记期间: 到期日必须严格晚于起始日。
func ValidateRegDateRange(start, end *time.Time) error {
	if start == nil {
		return collateralInvalid("reg_start_date", "登记起始日不能为空")
	}
	if end == nil {
		return collateralInvalid("reg_end_date", "登记到期日不能为空")
	}
	if !end.After(*start) {
		return collateralInvalid("reg_end_date", "登记到期日必须晚于起始日")
	}
	return nil
}

// IsExpiredOn 判断登记在给定日期是否已过期: 到期日 < 今天 即过期。
// 只比较日历日, 忽略时分秒与时区差异带来的偏移。
func IsExpiredOn(end *time.Time, today time.Time) bool {
	if end == nil {
		return false
	}
	endDay := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)
	todayDay := time.Date(today.Year(), today.Month(), today.Day(), 0, 0, 0, 0, time.UTC)
	return endDay.Before(todayDay)
}

// EffectiveStatus 计算对外展示状态。
// status 字段可能滞后（没有定时任务逐日刷新），所以到期判断必须基于 reg_end_date 动态算，
// 不能只依赖 status 列。cancelled（已作废）优先级最高，不会被日期覆盖。
func EffectiveStatus(r *CollateralRegistration, today time.Time) string {
	if r == nil {
		return ""
	}
	if r.Status == "cancelled" {
		return "cancelled"
	}
	if IsExpiredOn(r.RegEndDate, today) {
		return "expired"
	}
	return r.Status
}

// ===== Requests =====

type UpsertCollateralReq struct {
	RegNo          string `json:"reg_no"`
	RegType        string `json:"reg_type"`
	LessorName     string `json:"lessor_name"`
	LesseeName     string `json:"lessee_name"`
	LesseeUscc     string `json:"lessee_uscc"`
	CollateralDesc string `json:"collateral_desc"`
	RegStartDate   string `json:"reg_start_date"` // YYYY-MM-DD
	RegEndDate     string `json:"reg_end_date"`   // YYYY-MM-DD
	SourceNote     string `json:"source_note"`    // 录入依据: 查询截图编号/查询日期
	VerifiedAt     string `json:"verified_at"`    // YYYY-MM-DD
}

const dateLayout = "2006-01-02"

// ParseDate 解析 YYYY-MM-DD；空串返回 nil（表示未填写）。
func ParseDate(field, s string) (*time.Time, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, nil
	}
	t, err := time.ParseInLocation(dateLayout, s, time.UTC)
	if err != nil {
		return nil, collateralInvalid(field, "日期格式必须为 YYYY-MM-DD")
	}
	return &t, nil
}

// ValidatedCollateral 是校验通过后的登记数据。
type ValidatedCollateral struct {
	RegNo          string
	RegType        string
	LessorName     string
	LesseeName     string
	LesseeUscc     string
	CollateralDesc string
	RegStartDate   *time.Time
	RegEndDate     *time.Time
	SourceNote     string
	VerifiedAt     *time.Time
}

// ValidateUpsertCollateral 纯函数校验录入/修改入参。
func ValidateUpsertCollateral(req UpsertCollateralReq) (*ValidatedCollateral, error) {
	regNo := strings.TrimSpace(req.RegNo)
	if regNo == "" {
		return nil, collateralInvalid("reg_no", "中登网登记编号不能为空")
	}
	if len(regNo) > 64 {
		return nil, collateralInvalid("reg_no", "中登网登记编号不能超过 64 个字符")
	}
	if !validRegTypes[req.RegType] {
		return nil, collateralInvalid("reg_type", "登记类型不合法（finance_lease/mortgage/factoring/other）")
	}
	lessor := strings.TrimSpace(req.LessorName)
	if lessor == "" {
		return nil, collateralInvalid("lessor_name", "出租人/权利人不能为空")
	}
	if len([]rune(lessor)) > 128 {
		return nil, collateralInvalid("lessor_name", "出租人/权利人不能超过 128 个字符")
	}
	lessee := strings.TrimSpace(req.LesseeName)
	if lessee == "" {
		return nil, collateralInvalid("lessee_name", "承租人/义务人不能为空")
	}
	if len([]rune(lessee)) > 128 {
		return nil, collateralInvalid("lessee_name", "承租人/义务人不能超过 128 个字符")
	}

	uscc := strings.ToUpper(strings.TrimSpace(req.LesseeUscc))
	if uscc != "" {
		if err := ValidateUSCC(uscc); err != nil {
			return nil, err
		}
	}

	start, err := ParseDate("reg_start_date", req.RegStartDate)
	if err != nil { return nil, err }
	end, err := ParseDate("reg_end_date", req.RegEndDate)
	if err != nil { return nil, err }
	if err := ValidateRegDateRange(start, end); err != nil {
		return nil, err
	}
	verified, err := ParseDate("verified_at", req.VerifiedAt)
	if err != nil { return nil, err }

	// 人工录入必须留下依据，否则无法追溯这条数据是谁在哪天从官方系统查到的
	note := strings.TrimSpace(req.SourceNote)
	if note == "" {
		return nil, collateralInvalid("source_note", "必须填写录入依据（中登网查询截图编号或查询日期），人工录入数据需可追溯")
	}
	if len([]rune(note)) > 256 {
		return nil, collateralInvalid("source_note", "录入依据不能超过 256 个字符")
	}

	return &ValidatedCollateral{
		RegNo: regNo, RegType: req.RegType,
		LessorName: lessor, LesseeName: lessee, LesseeUscc: uscc,
		CollateralDesc: req.CollateralDesc,
		RegStartDate:   start, RegEndDate: end,
		SourceNote: note, VerifiedAt: verified,
	}, nil
}

// CollateralQuery 查询条件。两个条件至少填一个，禁止裸查全表。
type CollateralQuery struct {
	LesseeName string
	LesseeUscc string
	Page       int
	PageSize   int
}

const collateralMaxPageSize = 100

func (q *CollateralQuery) Normalize() {
	q.LesseeName = strings.TrimSpace(q.LesseeName)
	q.LesseeUscc = strings.ToUpper(strings.TrimSpace(q.LesseeUscc))
	if q.Page <= 0 { q.Page = 1 }
	if q.PageSize <= 0 { q.PageSize = 20 }
	if q.PageSize > collateralMaxPageSize { q.PageSize = collateralMaxPageSize }
}

// Validate 拒绝空条件查询（防止把全部登记数据一次性拖走）。
func (q *CollateralQuery) Validate() error {
	if q.LesseeName == "" && q.LesseeUscc == "" {
		return ErrQueryTooBroad
	}
	if q.LesseeUscc != "" {
		if err := ValidateUSCC(q.LesseeUscc); err != nil {
			return err
		}
	}
	return nil
}

// ===== Repository =====

// CollateralRepository 独立于 intermediary.Repository，避免改动既有文件。
type CollateralRepository struct {
	db *sqlx.DB
}

func NewCollateralRepository(db *sqlx.DB) *CollateralRepository {
	return &CollateralRepository{db: db}
}

const mysqlErrDupEntry = 1062

func isDupEntry(err error) bool {
	var me *mysql.MySQLError
	if e, ok := err.(*mysql.MySQLError); ok {
		me = e
	}
	return me != nil && me.Number == mysqlErrDupEntry
}

func (r *CollateralRepository) Create(v *ValidatedCollateral, createdBy int64) (int64, error) {
	res, err := r.db.Exec(
		`INSERT INTO collateral_registrations (reg_no, reg_type, lessor_name, lessee_name, lessee_uscc,
		collateral_desc, reg_start_date, reg_end_date, status, data_source, source_note, verified_at, created_by)
		VALUES (?,?,?,?,?,?,?,?,'valid','manual',?,?,?)`,
		v.RegNo, v.RegType, v.LessorName, v.LesseeName, v.LesseeUscc,
		v.CollateralDesc, v.RegStartDate, v.RegEndDate, v.SourceNote, v.VerifiedAt, createdBy,
	)
	if err != nil {
		if isDupEntry(err) { return 0, ErrRegNoConflict }
		return 0, err
	}
	return res.LastInsertId()
}

func (r *CollateralRepository) Update(id int64, v *ValidatedCollateral) error {
	res, err := r.db.Exec(
		`UPDATE collateral_registrations SET reg_no=?, reg_type=?, lessor_name=?, lessee_name=?, lessee_uscc=?,
		collateral_desc=?, reg_start_date=?, reg_end_date=?, source_note=?, verified_at=? WHERE id=?`,
		v.RegNo, v.RegType, v.LessorName, v.LesseeName, v.LesseeUscc,
		v.CollateralDesc, v.RegStartDate, v.RegEndDate, v.SourceNote, v.VerifiedAt, id,
	)
	if err != nil {
		if isDupEntry(err) { return ErrRegNoConflict }
		return err
	}
	affected, _ := res.RowsAffected()
	if affected == 0 {
		// 可能是 id 不存在，也可能是提交了完全相同的内容；用一次 SELECT 区分
		var exists int
		if e := r.db.Get(&exists, "SELECT 1 FROM collateral_registrations WHERE id=?", id); e != nil {
			return ErrCollateralNotFound
		}
	}
	return nil
}

// Cancel 作废（软删）: status='cancelled'，绝不物理删除，登记数据需长期留痕。
func (r *CollateralRepository) Cancel(id int64) error {
	res, err := r.db.Exec("UPDATE collateral_registrations SET status='cancelled' WHERE id=? AND status<>'cancelled'", id)
	if err != nil { return err }
	affected, _ := res.RowsAffected()
	if affected == 0 {
		var exists int
		if e := r.db.Get(&exists, "SELECT 1 FROM collateral_registrations WHERE id=?", id); e != nil {
			return ErrCollateralNotFound
		}
		// 已经是 cancelled，幂等返回成功
	}
	return nil
}

func (r *CollateralRepository) GetByID(id int64) (*CollateralRegistration, error) {
	var c CollateralRegistration
	err := r.db.Get(&c, "SELECT * FROM collateral_registrations WHERE id=?", id)
	if err == sql.ErrNoRows { return nil, nil }
	if err != nil { return nil, err }
	return &c, nil
}

func (r *CollateralRepository) Query(q CollateralQuery) ([]CollateralRegistration, int64, error) {
	where := "WHERE 1=1"
	args := []interface{}{}
	if q.LesseeUscc != "" {
		where += " AND lessee_uscc=?"
		args = append(args, q.LesseeUscc)
	}
	if q.LesseeName != "" {
		// 前缀匹配可用 idx_lessee 索引；不用 %xx% 以免索引失效
		where += " AND lessee_name LIKE ?"
		args = append(args, q.LesseeName+"%")
	}

	var total int64
	if err := r.db.Get(&total, "SELECT COUNT(*) FROM collateral_registrations "+where, args...); err != nil {
		return nil, 0, err
	}
	query := fmt.Sprintf(
		"SELECT * FROM collateral_registrations %s ORDER BY reg_start_date DESC, id DESC LIMIT ? OFFSET ?", where,
	)
	args = append(args, q.PageSize, (q.Page-1)*q.PageSize)
	var list []CollateralRegistration
	err := r.db.Select(&list, query, args...)
	return list, total, err
}

// ===== Service =====

type CollateralService struct {
	repo *CollateralRepository
	now  func() time.Time
}

func NewCollateralService(repo *CollateralRepository) *CollateralService {
	return &CollateralService{repo: repo, now: time.Now}
}

func (s *CollateralService) Query(q CollateralQuery) ([]CollateralRegistration, int64, error) {
	q.Normalize()
	if err := q.Validate(); err != nil {
		return nil, 0, err
	}
	return s.repo.Query(q)
}

func (s *CollateralService) Create(operatorID int64, req UpsertCollateralReq) (int64, error) {
	if operatorID <= 0 {
		return 0, collateralInvalid("created_by", "未识别到录入人身份")
	}
	v, err := ValidateUpsertCollateral(req)
	if err != nil { return 0, err }
	return s.repo.Create(v, operatorID)
}

func (s *CollateralService) Update(id int64, req UpsertCollateralReq) error {
	v, err := ValidateUpsertCollateral(req)
	if err != nil { return err }
	existing, err := s.repo.GetByID(id)
	if err != nil { return err }
	if existing == nil { return ErrCollateralNotFound }
	if existing.Status == "cancelled" {
		return collateralInvalid("status", "已作废的登记记录不可修改")
	}
	return s.repo.Update(id, v)
}

func (s *CollateralService) Cancel(id int64) error {
	existing, err := s.repo.GetByID(id)
	if err != nil { return err }
	if existing == nil { return ErrCollateralNotFound }
	return s.repo.Cancel(id)
}

// Now 暴露服务时钟供 handler 计算展示状态。
func (s *CollateralService) Now() time.Time { return s.now() }

// ===== Handler =====

// CollateralHandler 独立于 intermediary.Handler，由 main.go 单独装配。
type CollateralHandler struct {
	svc *CollateralService
}

func NewCollateralHandler(svc *CollateralService) *CollateralHandler {
	return &CollateralHandler{svc: svc}
}

func (h *CollateralHandler) RegisterPublicRoutes(r *gin.RouterGroup) {
	r.GET("/collateral-registrations", h.Query)
}

func (h *CollateralHandler) RegisterAdminRoutes(r *gin.RouterGroup) {
	r.POST("/admin/collateral-registrations", h.Create)
	r.PUT("/admin/collateral-registrations/:id", h.Update)
	r.DELETE("/admin/collateral-registrations/:id", h.Cancel)
}

func (h *CollateralHandler) Query(c *gin.Context) {
	q := CollateralQuery{
		LesseeName: c.Query("lessee_name"),
		LesseeUscc: c.Query("lessee_uscc"),
	}
	q.Page, _ = strconv.Atoi(c.DefaultQuery("page", "1"))
	q.PageSize, _ = strconv.Atoi(c.DefaultQuery("page_size", "20"))
	q.Normalize()

	list, total, err := h.svc.Query(q)
	if err != nil { response.Error(c, CollateralErrToCode(err), err.Error()); return }

	today := h.svc.Now()
	result := make([]gin.H, 0, len(list))
	for i := range list {
		result = append(result, collateralToJSON(&list[i], today))
	}
	response.Success(c, gin.H{
		"list":      result,
		"total":     total,
		"page":      q.Page,
		"page_size": q.PageSize,
		// 合规声明: 数据为人工录入，中登网无对外数据接口
		"data_source": "manual",
		"disclaimer":  Disclaimer,
	})
}

func (h *CollateralHandler) Create(c *gin.Context) {
	var req UpsertCollateralReq
	if err := c.ShouldBindJSON(&req); err != nil { response.Error(c, errcode.ParamInvalid, err.Error()); return }
	id, err := h.svc.Create(c.GetInt64("user_id"), req)
	if err != nil { response.Error(c, CollateralErrToCode(err), err.Error()); return }
	response.Success(c, gin.H{"id": id, "data_source": "manual", "disclaimer": Disclaimer})
}

func (h *CollateralHandler) Update(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 { response.Error(c, errcode.ParamInvalid, "登记记录ID不合法"); return }
	var req UpsertCollateralReq
	if err := c.ShouldBindJSON(&req); err != nil { response.Error(c, errcode.ParamInvalid, err.Error()); return }
	if err := h.svc.Update(id, req); err != nil { response.Error(c, CollateralErrToCode(err), err.Error()); return }
	response.Success(c, nil)
}

// Cancel 作废（软删）: 只置 status='cancelled'，不物理删除。
func (h *CollateralHandler) Cancel(c *gin.Context) {
	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil || id <= 0 { response.Error(c, errcode.ParamInvalid, "登记记录ID不合法"); return }
	if err := h.svc.Cancel(id); err != nil { response.Error(c, CollateralErrToCode(err), err.Error()); return }
	response.Success(c, gin.H{"status": "cancelled"})
}

func collateralToJSON(r *CollateralRegistration, today time.Time) gin.H {
	return gin.H{
		"id": r.ID, "reg_no": r.RegNo, "reg_type": r.RegType,
		"lessor_name": r.LessorName, "lessee_name": r.LesseeName, "lessee_uscc": r.LesseeUscc,
		"collateral_desc": r.CollateralDesc,
		"reg_start_date":  formatDate(r.RegStartDate),
		"reg_end_date":    formatDate(r.RegEndDate),
		// status 是库里存的原值；display_status 按 reg_end_date 动态判定，前端应展示后者
		"status":         r.Status,
		"display_status": EffectiveStatus(r, today),
		"is_expired":     IsExpiredOn(r.RegEndDate, today),
		"data_source":    r.DataSource,
		"source_note":    r.SourceNote,
		"verified_at":    formatDate(r.VerifiedAt),
		"created_at":     r.CreatedAt,
	}
}

func formatDate(t *time.Time) string {
	if t == nil { return "" }
	return t.Format(dateLayout)
}

func CollateralErrToCode(err error) int {
	if err == nil { return errcode.Success }
	switch err.Error() {
	case ErrCollateralNotFound.Error():
		return errcode.NotFound
	case ErrRegNoConflict.Error():
		return errcode.Conflict
	case ErrQueryTooBroad.Error():
		return errcode.ParamInvalid
	}
	if _, ok := err.(*CollateralValidationError); ok {
		return errcode.ParamInvalid
	}
	return errcode.InternalError
}
