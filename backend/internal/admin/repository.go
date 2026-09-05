package admin

import (
	"database/sql"
	"fmt"
	"strconv"
	"strings"
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
	ID         int64     `db:"id" json:"id"`
	OperatorID int64     `db:"operator_id" json:"operator_id"`
	Action     string    `db:"action" json:"action"`
	TargetType string    `db:"target_type" json:"target_type"`
	TargetID   int64     `db:"target_id" json:"target_id"`
	BeforeVal  string    `db:"before_value" json:"before_value"`
	AfterVal   string    `db:"after_value" json:"after_value"`
	IP         string    `db:"ip" json:"ip"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

type User struct {
	ID        int64     `db:"id" json:"id"`
	Phone     string    `db:"phone" json:"phone"`
	Email     string    `db:"email" json:"email"`
	Status    string    `db:"status" json:"status"`
	RoleCSV   string    `db:"role_csv" json:"-"`
	Roles     []string  `db:"-" json:"roles"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

type Config struct {
	TradingEnabled bool `json:"trading_enabled"`
	FeeRate        int  `json:"fee_rate"`
}

type Notice struct {
	ID        int64     `db:"id" json:"id"`
	Content   string    `db:"content" json:"content"`
	Status    string    `db:"status" json:"status"`
	CreatedBy int64     `db:"created_by" json:"created_by"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
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
	if level != "" {
		where = "WHERE level=?"
		args = append(args, level)
	}
	var total int64
	if err := r.db.Get(&total, "SELECT COUNT(*) FROM risk_alerts "+where, args...); err != nil {
		return nil, 0, err
	}
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	var list []Alert
	err := r.db.Select(&list, `SELECT id, level, alert_type,
		COALESCE(target_type, '') AS target_type, COALESCE(target_id, 0) AS target_id,
		COALESCE(rule_detail, '') AS rule_detail, status, created_at
		FROM risk_alerts `+where+" ORDER BY created_at DESC LIMIT ? OFFSET ?", append(args, pageSize, (page-1)*pageSize)...)
	return list, total, err
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

func (r *Repository) GetConfig() (Config, error) {
	rows := make([]struct {
		Key   string `db:"config_key"`
		Value string `db:"config_value"`
	}, 0, 2)
	if err := r.db.Select(&rows, "SELECT config_key, config_value FROM system_config WHERE config_key IN ('trading_enabled','fee_rate')"); err != nil {
		return Config{}, err
	}
	config := Config{TradingEnabled: true, FeeRate: 500}
	for _, row := range rows {
		switch row.Key {
		case "trading_enabled":
			config.TradingEnabled = row.Value == "true"
		case "fee_rate":
			feeRate, err := strconv.Atoi(row.Value)
			if err != nil {
				return Config{}, fmt.Errorf("invalid fee_rate: %w", err)
			}
			config.FeeRate = feeRate
		}
	}
	return config, nil
}

func (r *Repository) UpdateConfig(operatorID int64, key, value, ip string) error {
	tx, err := r.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var before string
	err = tx.Get(&before, "SELECT config_value FROM system_config WHERE config_key=? FOR UPDATE", key)
	if err != nil && err != sql.ErrNoRows {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO system_config (config_key, config_value, updated_by) VALUES (?,?,?)
		ON DUPLICATE KEY UPDATE config_value=VALUES(config_value), updated_by=VALUES(updated_by)`, key, value, operatorID); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO audit_logs
		(operator_id, action, target_type, target_id, before_value, after_value, ip)
		VALUES (?, 'update_config', 'config', 0, ?, ?, ?)`, operatorID, key+"="+before, key+"="+value, ip); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Repository) CreateNotice(operatorID int64, content, ip string) (int64, error) {
	tx, err := r.db.Beginx()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	result, err := tx.Exec("INSERT INTO cms_notices (content, created_by) VALUES (?,?)", content, operatorID)
	if err != nil {
		return 0, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`INSERT INTO audit_logs
		(operator_id, action, target_type, target_id, before_value, after_value, ip)
		VALUES (?, 'create_notice', 'cms_notice', ?, '', ?, ?)`, operatorID, id, content, ip); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return id, nil
}

func (r *Repository) ListNotices() ([]Notice, error) {
	list := make([]Notice, 0)
	err := r.db.Select(&list, "SELECT id, content, status, created_by, created_at FROM cms_notices ORDER BY created_at DESC, id DESC")
	return list, err
}

func (r *Repository) ListAuditLogs(page, pageSize int) ([]AuditLog, int64, error) {
	var total int64
	if err := r.db.Get(&total, "SELECT COUNT(*) FROM audit_logs"); err != nil {
		return nil, 0, err
	}
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	var list []AuditLog
	err := r.db.Select(&list, `SELECT id, COALESCE(operator_id, 0) AS operator_id, action,
		COALESCE(target_type, '') AS target_type, COALESCE(target_id, 0) AS target_id,
		COALESCE(before_value, '') AS before_value, COALESCE(after_value, '') AS after_value,
		COALESCE(ip, '') AS ip, created_at
		FROM audit_logs ORDER BY created_at DESC LIMIT ? OFFSET ?`, pageSize, (page-1)*pageSize)
	return list, total, err
}

func (r *Repository) ListUsers() ([]User, error) {
	list := make([]User, 0)
	err := r.db.Select(&list, `SELECT u.id, u.phone, u.email, u.status, u.created_at,
		COALESCE(GROUP_CONCAT(ur.role ORDER BY ur.id SEPARATOR ','), 'buyer') AS role_csv
		FROM users u LEFT JOIN user_roles ur ON ur.user_id=u.id
		GROUP BY u.id, u.phone, u.email, u.status, u.created_at ORDER BY u.created_at DESC`)
	for i := range list {
		list[i].Roles = strings.Split(list[i].RoleCSV, ",")
	}
	return list, err
}

func (r *Repository) FreezeUser(id int64) error {
	result, err := r.db.Exec("UPDATE users SET status='frozen' WHERE id=? AND status='active'", id)
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
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

func (s *Service) GetConfig() (Config, error) {
	return s.repo.GetConfig()
}

func (s *Service) UpdateConfig(operatorID int64, key, value, ip string) error {
	return s.repo.UpdateConfig(operatorID, key, value, ip)
}

func (s *Service) CreateNotice(operatorID int64, content, ip string) (int64, error) {
	return s.repo.CreateNotice(operatorID, content, ip)
}

func (s *Service) ListNotices() ([]Notice, error) { return s.repo.ListNotices() }

func (s *Service) ListUsers() ([]User, error) { return s.repo.ListUsers() }
func (s *Service) FreezeUser(id int64) error  { return s.repo.FreezeUser(id) }
