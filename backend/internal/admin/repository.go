package admin

import (
	"time"
	"github.com/jmoiron/sqlx"
)

type Alert struct {
	ID         int64     `db:"id" json:"id"`
	Level      string    `db:"level" json:"level"`
	AlertType  string    `db:"alert_type" json:"alert_type"`
	TargetType string    `db:"target_type" json:"target_type"`
	TargetID   int64     `db:"target_id" json:"target_id"`
	RuleDetail string    `db:"rule_detail" json:"rule_detail"`
	Status     string    `db:"status" json:"status"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

type AuditLog struct {
	ID         int64  `db:"id" json:"id"`
	OperatorID int64  `db:"operator_id" json:"operator_id"`
	Action     string `db:"action" json:"action"`
	TargetType string `db:"target_type" json:"target_type"`
	TargetID   int64  `db:"target_id" json:"target_id"`
	BeforeVal  string `db:"before_value" json:"before_value"`
	AfterVal   string `db:"after_value" json:"after_value"`
	IP         string `db:"ip" json:"ip"`
}

type Repository struct{ db *sqlx.DB }
func NewRepository(db *sqlx.DB) *Repository { return &Repository{db: db} }

func (r *Repository) CreateAlert(a *Alert) error {
	_, err := r.db.Exec(
		"INSERT INTO risk_alerts (level, alert_type, target_type, target_id, rule_detail, status) VALUES (?,?,?,?,?,?)",
		a.Level, a.AlertType, a.TargetType, a.TargetID, a.RuleDetail, "pending",
	)
	return err
}

func (r *Repository) ListAlerts(level string, page, pageSize int) ([]Alert, int64, error) {
	where := ""
	args := []interface{}{}
	if level != "" { where = "WHERE level=?"; args = append(args, level) }
	var total int64
	r.db.Get(&total, "SELECT COUNT(*) FROM risk_alerts "+where, args...)
	if page <= 0 { page = 1 }
	if pageSize <= 0 { pageSize = 20 }
	var list []Alert
	r.db.Select(&list, "SELECT * FROM risk_alerts "+where+" ORDER BY created_at DESC LIMIT ? OFFSET ?", append(args, pageSize, (page-1)*pageSize)...)
	return list, total, nil
}

func (r *Repository) UpdateAlertStatus(id int64, status string) error {
	_, err := r.db.Exec("UPDATE risk_alerts SET status=? WHERE id=?", status, id)
	return err
}

func (r *Repository) CreateAuditLog(l *AuditLog) error {
	_, err := r.db.Exec(
		"INSERT INTO audit_logs (operator_id, action, target_type, target_id, before_value, after_value, ip) VALUES (?,?,?,?,?,?,?)",
		l.OperatorID, l.Action, l.TargetType, l.TargetID, l.BeforeVal, l.AfterVal, l.IP,
	)
	return err
}

func (r *Repository) ListAuditLogs(page, pageSize int) ([]AuditLog, int64, error) {
	var total int64
	r.db.Get(&total, "SELECT COUNT(*) FROM audit_logs")
	if page <= 0 { page = 1 }
	if pageSize <= 0 { pageSize = 20 }
	var list []AuditLog
	r.db.Select(&list, "SELECT * FROM audit_logs ORDER BY created_at DESC LIMIT ? OFFSET ?", pageSize, (page-1)*pageSize)
	return list, total, nil
}

type Service struct{ repo *Repository }
func NewService(repo *Repository) *Service { return &Service{repo: repo} }

func (s *Service) CreateAlert(level, alertType, targetType string, targetID int64, rule string) error {
	return s.repo.CreateAlert(&Alert{Level: level, AlertType: alertType, TargetType: targetType, TargetID: targetID, RuleDetail: rule})
}

func (s *Service) ListAlerts(level string, page, pageSize int) ([]Alert, int64, error) {
	return s.repo.ListAlerts(level, page, pageSize)
}

func (s *Service) ResolveAlert(id int64) error { return s.repo.UpdateAlertStatus(id, "resolved") }
func (s *Service) DismissAlert(id int64) error { return s.repo.UpdateAlertStatus(id, "dismissed") }

func (s *Service) LogAudit(operatorID int64, action, targetType string, targetID int64, before, after, ip string) error {
	return s.repo.CreateAuditLog(&AuditLog{OperatorID: operatorID, Action: action, TargetType: targetType, TargetID: targetID, BeforeVal: before, AfterVal: after, IP: ip})
}

func (s *Service) ListAuditLogs(page, pageSize int) ([]AuditLog, int64, error) {
	return s.repo.ListAuditLogs(page, pageSize)
}
