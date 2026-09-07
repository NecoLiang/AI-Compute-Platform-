package intermediary

import (
	"database/sql"
	"errors"
	"regexp"
	"strings"
	"github.com/jmoiron/sqlx"
	"time"
)

type Lead struct {
	ID           int64     `db:"id" json:"id"`
	Type         string    `db:"type" json:"type"`
	ContactName  string    `db:"contact_name" json:"contact_name"`
	ContactPhone string    `db:"contact_phone" json:"contact_phone"`
	ContactEmail string    `db:"contact_email" json:"contact_email"`
	Description  string    `db:"description" json:"description"`
	AmountRange  string    `db:"amount_range" json:"amount_range"`
	Term         string    `db:"term" json:"term"`
	Status       string    `db:"status" json:"status"`
	AssigneeID   *int64    `db:"assignee_id" json:"assignee_id"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}

type Commission struct {
	ID               int64   `db:"id" json:"id"`
	LeadID           int64   `db:"lead_id" json:"lead_id"`
	DealAmount       int64   `db:"deal_amount" json:"deal_amount"`
	CommissionRate   float64 `db:"commission_rate" json:"commission_rate"`
	CommissionAmount int64   `db:"commission_amount" json:"commission_amount"`
	Status           string  `db:"status" json:"status"`
}

type Repository struct{ db *sqlx.DB }

func NewRepository(db *sqlx.DB) *Repository { return &Repository{db: db} }

func (r *Repository) CreateLead(l *Lead) (int64, error) {
	res, err := r.db.Exec(
		"INSERT INTO leads (type, contact_name, contact_phone, contact_email, description, amount_range, term, status) VALUES (?,?,?,?,?,?,?,?)",
		l.Type, l.ContactName, l.ContactPhone, l.ContactEmail, l.Description, l.AmountRange, l.Term, "new",
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (r *Repository) GetLead(id int64) (*Lead, error) {
	var l Lead
	err := r.db.Get(&l, "SELECT * FROM leads WHERE id=?", id)
	return &l, err
}

// ListLeads assigneeID>0 时只返回分配给该账号的线索(vendor 视角);
// 0 = 不过滤(仅限运营视角调用)。线索含留资人姓名/手机等 PII, 归属过滤是硬约束。
func (r *Repository) ListLeads(assigneeID int64, status string, page, pageSize int) ([]Lead, int64, error) {
	conds, args := []string{}, []interface{}{}
	if assigneeID > 0 {
		conds = append(conds, "assignee_id=?")
		args = append(args, assigneeID)
	}
	if status != "" {
		conds = append(conds, "status=?")
		args = append(args, status)
	}
	where := ""
	if len(conds) > 0 {
		where = "WHERE " + strings.Join(conds, " AND ")
	}
	var total int64
	if err := r.db.Get(&total, "SELECT COUNT(*) FROM leads "+where, args...); err != nil {
		return nil, 0, err
	}
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	var list []Lead
	err := r.db.Select(&list, `SELECT id, type,
		COALESCE(contact_name, '') AS contact_name, COALESCE(contact_phone, '') AS contact_phone,
		COALESCE(contact_email, '') AS contact_email, COALESCE(description, '') AS description,
		COALESCE(amount_range, '') AS amount_range, COALESCE(term, '') AS term,
		COALESCE(status, 'new') AS status, assignee_id, created_at
		FROM leads `+where+" ORDER BY created_at DESC LIMIT ? OFFSET ?", append(args, pageSize, (page-1)*pageSize)...)
	return list, total, err
}

func (r *Repository) AssignLead(id, assigneeID int64) error {
	return execExisting(r.db, "UPDATE leads SET assignee_id=?, status='assigned' WHERE id=?", assigneeID, id)
}

// QuoteLeadOwned 报价: 仅限被分配人, 且线索处于可报价状态。守卫在 SQL 条件里,
// RowsAffected=0 即「不存在/不属于你/状态不允许」, 不泄露具体是哪种。
func (r *Repository) QuoteLeadOwned(id, assigneeID int64) error {
	return execExisting(r.db,
		"UPDATE leads SET status='quoted' WHERE id=? AND assignee_id=? AND status IN ('assigned','following')",
		id, assigneeID)
}

func (r *Repository) CreateCommission(c *Commission) error {
	_, err := r.db.Exec(
		"INSERT INTO commissions (lead_id, deal_amount, commission_rate, commission_amount, status) VALUES (?,?,?,?,?)",
		c.LeadID, c.DealAmount, c.CommissionRate, c.CommissionAmount, "pending",
	)
	return err
}

func (r *Repository) ListCommissions(userID int64) ([]Commission, error) {
	var list []Commission
	err := r.db.Select(&list, "SELECT * FROM commissions WHERE lead_id IN (SELECT id FROM leads WHERE assignee_id=?)", userID)
	return list, err
}

func execExisting(db sqlx.Execer, query string, args ...interface{}) error {
	result, err := db.Exec(query, args...)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return sql.ErrNoRows
	}
	return nil
}

type Service struct{ repo *Repository }

func NewService(repo *Repository) *Service { return &Service{repo: repo} }

type CreateLeadReq struct {
	Type         string `json:"type"`
	ContactName  string `json:"contact_name"`
	ContactPhone string `json:"contact_phone"`
	ContactEmail string `json:"contact_email"`
	Description  string `json:"description"`
	AmountRange  string `json:"amount_range"`
	Term         string `json:"term"`
}

var leadPhone = regexp.MustCompile(`^\+?[0-9 -]{6,20}$`)

// validLeadTypes 公开留资允许的类型白名单; compute 线索只能由商品询价接口内部产生。
var validLeadTypes = map[string]bool{"equipment": true, "construction": true, "finance_lease": true}

func (s *Service) CreateLead(req CreateLeadReq) (int64, error) {
	if !validLeadTypes[req.Type] {
		return 0, errors.New("线索类型不正确")
	}
	req.ContactName = strings.TrimSpace(req.ContactName)
	req.ContactPhone = strings.TrimSpace(req.ContactPhone)
	req.Description = strings.TrimSpace(req.Description)
	if req.ContactName == "" || len([]rune(req.ContactName)) > 64 {
		return 0, errors.New("请填写 1-64 字的联系人姓名")
	}
	if !leadPhone.MatchString(req.ContactPhone) {
		return 0, errors.New("请填写有效的联系电话")
	}
	if len([]rune(req.Description)) > 2000 {
		return 0, errors.New("需求描述过长(≤2000字)")
	}
	return s.repo.CreateLead(&Lead{
		Type: req.Type, ContactName: req.ContactName, ContactPhone: req.ContactPhone,
		ContactEmail: req.ContactEmail, Description: req.Description,
		AmountRange: req.AmountRange, Term: req.Term,
	})
}

func (s *Service) ListLeads(assigneeID int64, status string, page, pageSize int) ([]Lead, int64, error) {
	return s.repo.ListLeads(assigneeID, status, page, pageSize)
}

func (s *Service) AssignLead(id, assigneeID int64) error { return s.repo.AssignLead(id, assigneeID) }

// ErrLeadNotActionable 归属/状态守卫统一出口: 不区分「不存在」与「不是你的」。
var ErrLeadNotActionable = errors.New("线索不存在、未分配给当前账号或状态不允许该操作")

func (s *Service) QuoteLead(id, vendorID int64) error {
	if err := s.repo.QuoteLeadOwned(id, vendorID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeadNotActionable
		}
		return err
	}
	return nil
}

type CloseDealReq struct {
	DealAmount     int64   `json:"deal_amount"`
	CommissionRate float64 `json:"commission_rate"`
}

func (s *Service) CloseDeal(leadID, vendorID int64, req CloseDealReq) error {
	// 成交金额与佣金率入台账, 必须先过数值合法性 —— 佣金由 rate 直接算出, 脏数据即假账。
	if req.DealAmount <= 0 {
		return errors.New("成交金额必须为正(单位: 分)")
	}
	if req.CommissionRate <= 0 || req.CommissionRate > 100 {
		return errors.New("佣金率须在 (0, 100] 区间(单位: %)")
	}
	tx, err := s.repo.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := execExisting(tx,
		"UPDATE leads SET status='closed' WHERE id=? AND assignee_id=? AND status IN ('assigned','following','quoted')",
		leadID, vendorID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrLeadNotActionable
		}
		return err
	}
	commissionFen := int64(float64(req.DealAmount) * req.CommissionRate / 100.0)
	if _, err := tx.Exec(
		"INSERT INTO commissions (lead_id, deal_amount, commission_rate, commission_amount, status) VALUES (?,?,?,?,?)",
		leadID, req.DealAmount, req.CommissionRate, commissionFen, "pending",
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) GetCommissions(userID int64) ([]Commission, error) {
	return s.repo.ListCommissions(userID)
}
