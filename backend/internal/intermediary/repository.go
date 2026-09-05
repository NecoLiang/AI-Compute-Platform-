package intermediary

import (
	"database/sql"
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

func (r *Repository) ListLeads(status string, page, pageSize int) ([]Lead, int64, error) {
	where := ""
	args := []interface{}{}
	if status != "" {
		where = "WHERE status=?"
		args = append(args, status)
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

func (r *Repository) UpdateLeadStatus(id int64, status string) error {
	return execExisting(r.db, "UPDATE leads SET status=? WHERE id=?", status, id)
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

func (s *Service) CreateLead(req CreateLeadReq) (int64, error) {
	return s.repo.CreateLead(&Lead{
		Type: req.Type, ContactName: req.ContactName, ContactPhone: req.ContactPhone,
		ContactEmail: req.ContactEmail, Description: req.Description,
		AmountRange: req.AmountRange, Term: req.Term,
	})
}

func (s *Service) ListLeads(status string, page, pageSize int) ([]Lead, int64, error) {
	return s.repo.ListLeads(status, page, pageSize)
}

func (s *Service) AssignLead(id, assigneeID int64) error { return s.repo.AssignLead(id, assigneeID) }
func (s *Service) QuoteLead(id int64) error              { return s.repo.UpdateLeadStatus(id, "quoted") }

type CloseDealReq struct {
	DealAmount     int64   `json:"deal_amount"`
	CommissionRate float64 `json:"commission_rate"`
}

func (s *Service) CloseDeal(leadID int64, req CloseDealReq) error {
	tx, err := s.repo.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := execExisting(tx, "UPDATE leads SET status='closed' WHERE id=?", leadID); err != nil {
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
