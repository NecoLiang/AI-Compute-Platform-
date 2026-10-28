package modeldeploy

import (
	"fmt"
	"strings"
	"time"

	"tokenfactory/pkg/errcode"
)

// Error 带错误码的业务错误, handler 按 Code 原样返回。
type Error struct {
	Code int
	Msg  string
	Data any
}

func (e *Error) Error() string { return e.Msg }

func newErr(code int, format string, args ...any) *Error {
	return &Error{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// GPUSpecLookup 由 catalog.Service 实现: 按标准型号名取 compute_capability。
// found=false 表示型号库无此型号; cc="" 表示型号存在但未登记。
type GPUSpecLookup interface {
	ComputeCapabilityByModel(modelName string) (cc string, found bool, err error)
}

type Service struct {
	repo   *Repository
	gpus   GPUSpecLookup
	leases LeaseSource // nil 直到 S2 fleet 接入; 见 SetLeaseSource
	now    func() time.Time
}

func NewService(repo *Repository, gpus GPUSpecLookup) *Service {
	return &Service{repo: repo, gpus: gpus, now: time.Now}
}

var validEntryStatus = map[string]bool{StatusDraft: true, StatusActive: true, StatusRetired: true}

// AdminList 运营查看目录, status 为空即全部。
func (s *Service) AdminList(status string) ([]ModelEntry, error) {
	if status != "" && !validEntryStatus[status] {
		return nil, newErr(errcode.ParamInvalid, "status 须为 draft/active/retired")
	}
	return s.repo.ListEntries(status)
}

// EntryDetail 运营详情: 条目 + 全部验证记录。
type EntryDetail struct {
	ModelEntry
	Verifications []Verification `json:"verifications"`
}

func (s *Service) AdminGet(id int64) (*EntryDetail, error) {
	e, err := s.repo.GetEntry(id)
	if err != nil {
		return nil, err
	}
	if e == nil {
		return nil, newErr(errcode.NotFound, "模型目录条目不存在")
	}
	vs, err := s.repo.ListVerifications(id)
	if err != nil {
		return nil, err
	}
	return &EntryDetail{ModelEntry: *e, Verifications: vs}, nil
}

func isDuplicate(err error) bool { return err != nil && strings.Contains(err.Error(), "Duplicate") }

// Create 新建条目, 一律为 draft。
func (s *Service) Create(e *ModelEntry, operatorID int64) (*ModelEntry, error) {
	if err := normalizeEntry(e); err != nil {
		return nil, newErr(errcode.ParamInvalid, "%s", err.Error())
	}
	e.CreatedBy = operatorID
	if err := s.repo.InsertEntry(e); err != nil {
		if isDuplicate(err) {
			return nil, newErr(errcode.Conflict, "model_key %s 已存在; revision 变化须使用新的 model_key", e.ModelKey)
		}
		return nil, err
	}
	return s.repo.GetEntry(e.ID)
}

// Update 整体更新, 只允许 draft。active 条目修改须新建条目(revision 变化即为新模型)。
func (s *Service) Update(id int64, e *ModelEntry) (*ModelEntry, error) {
	if err := normalizeEntry(e); err != nil {
		return nil, newErr(errcode.ParamInvalid, "%s", err.Error())
	}
	tx, err := s.repo.beginTx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	cur, err := lockEntryTx(tx, id)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, newErr(errcode.NotFound, "模型目录条目不存在")
	}
	if cur.Status != StatusDraft {
		return nil, newErr(errcode.Conflict, "只可编辑 draft 条目; 当前为 %s, 修改请新建条目", cur.Status)
	}
	// 行已加锁且确认为 draft; UPDATE 仍带 status='draft' 守卫。值完全未变时 RowsAffected 为 0,
	// 不能据此判冲突, 因此不看影响行数。
	if _, err := updateDraftTx(tx, id, e); err != nil {
		if isDuplicate(err) {
			return nil, newErr(errcode.Conflict, "model_key %s 已存在", e.ModelKey)
		}
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.repo.GetEntry(id)
}

// AddVerification 录入真机验证记录。draft/active 均可录入(active 条目后续补测新卡型以解除 F8);
// retired 条目不再接受记录。gpu_model 必须是型号库中的标准型号名, 否则 F8 永远匹配不上。
func (s *Service) AddVerification(id int64, v *Verification, operatorID int64) (*Verification, error) {
	tx, err := s.repo.beginTx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	entry, err := lockEntryTx(tx, id)
	if err != nil {
		return nil, err
	}
	if entry == nil {
		return nil, newErr(errcode.NotFound, "模型目录条目不存在")
	}
	if entry.Status == StatusRetired {
		return nil, newErr(errcode.Conflict, "条目已退役, 不再接受验证记录")
	}
	if err := validateVerification(v, entry); err != nil {
		return nil, newErr(errcode.ParamInvalid, "%s", err.Error())
	}
	if v.TestedAt.After(s.now().Add(5 * time.Minute)) {
		return nil, newErr(errcode.ParamInvalid, "tested_at 不能晚于当前时间")
	}
	_, found, err := s.gpus.ComputeCapabilityByModel(v.GPUModel)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, newErr(errcode.ParamInvalid, "gpu_model %s 不在 GPU 型号库中, 请使用型号库标准型号名", v.GPUModel)
	}
	v.ModelID, v.TestedBy = id, operatorID
	if err := insertVerificationTx(tx, v); err != nil {
		return nil, err
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return v, nil
}

// Activate draft → active。必须至少有一条 result=pass 的真机验证记录, 否则 40001。
// 已是 active 时幂等返回; retired 不可重新上线(40900)。
func (s *Service) Activate(id int64) (*ModelEntry, error) {
	tx, err := s.repo.beginTx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	cur, err := lockEntryTx(tx, id)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, newErr(errcode.NotFound, "模型目录条目不存在")
	}
	switch cur.Status {
	case StatusActive:
		return cur, nil
	case StatusRetired:
		return nil, newErr(errcode.Conflict, "条目已退役, 不能重新上线; 请新建条目")
	}
	passed, err := hasPassVerificationTx(tx, id)
	if err != nil {
		return nil, err
	}
	if !passed {
		return nil, newErr(errcode.ParamInvalid, "上线前须至少有一条 result=pass 的真机验证记录")
	}
	n, err := transitionTx(tx, id, StatusActive, StatusDraft)
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, newErr(errcode.Conflict, "条目状态已变化, 请刷新后重试")
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.repo.GetEntry(id)
}

// Retire draft/active → retired。已退役时幂等返回。不影响运行中部署(docs/27 §27.10)。
func (s *Service) Retire(id int64) (*ModelEntry, error) {
	tx, err := s.repo.beginTx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	cur, err := lockEntryTx(tx, id)
	if err != nil {
		return nil, err
	}
	if cur == nil {
		return nil, newErr(errcode.NotFound, "模型目录条目不存在")
	}
	if cur.Status == StatusRetired {
		return cur, nil
	}
	n, err := transitionTx(tx, id, StatusRetired, StatusDraft, StatusActive)
	if err != nil {
		return nil, err
	}
	if n != 1 {
		return nil, newErr(errcode.Conflict, "条目状态已变化, 请刷新后重试")
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return s.repo.GetEntry(id)
}
