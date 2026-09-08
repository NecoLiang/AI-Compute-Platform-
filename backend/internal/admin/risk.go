package admin

import (
	"context"
	"errors"
	"fmt"
)

var ErrAlertConflict = errors.New("告警已处置或正在冻结，不能忽略")

func (s *Service) FreezeAlert(ctx context.Context, operatorID, id int64, ip string) error {
	tx, err := s.repo.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var alert struct {
		TargetType string `db:"target_type"`
		TargetID   int64  `db:"target_id"`
		Status     string `db:"status"`
	}
	if err := tx.Get(&alert, "SELECT COALESCE(target_type,'') AS target_type, COALESCE(target_id,0) AS target_id, status FROM risk_alerts WHERE id=? FOR UPDATE", id); err != nil {
		return err
	}
	if alert.Status == "resolved" {
		return nil
	}
	if alert.Status != "pending" && alert.Status != "processing" {
		return ErrAlertConflict
	}
	if alert.TargetID <= 0 {
		return fmt.Errorf("告警缺少有效处置对象")
	}
	var orderNo, before string
	switch alert.TargetType {
	case "order":
		if err := tx.Get(&orderNo, "SELECT order_no FROM orders WHERE id=?", alert.TargetID); err != nil {
			return err
		}
		if err := tx.Get(&before, "SELECT status FROM orders WHERE id=? FOR UPDATE", alert.TargetID); err != nil {
			return err
		}
		if err := s.orders.FreezeOrderTx(tx, orderNo); err != nil {
			return err
		}
	case "user", "account":
		if alert.TargetID == operatorID {
			return fmt.Errorf("不能冻结当前登录账户")
		}
		var err error
		before, err = s.repo.freezeUserTx(tx, alert.TargetID)
		if err != nil {
			return err
		}
	default:
		return fmt.Errorf("不支持冻结该类型的告警对象")
	}
	if alert.Status == "pending" {
		if _, err := tx.Exec(`INSERT INTO audit_logs (operator_id,action,target_type,target_id,before_value,after_value,ip)
			VALUES (?,'freeze_alert',?,?,?,?,?)`, operatorID, alert.TargetType, alert.TargetID, before, fmt.Sprintf("frozen; alert=%d", id), ip); err != nil {
			return err
		}
		if _, err := tx.Exec("UPDATE risk_alerts SET status='processing',operator_id=?,resolution='target_frozen' WHERE id=?", operatorID, id); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	// Redis is outside MySQL's transaction. Persist processing so a failed session
	// revocation can be retried without claiming success or repeating the audit.
	if orderNo != "" {
		if before != "frozen" {
			s.orders.AttestOrderFreeze(orderNo)
		}
	} else if err := s.RevokeUserSessions(ctx, alert.TargetID); err != nil {
		return err
	}
	_, err = s.repo.db.Exec("UPDATE risk_alerts SET status='resolved',resolution='frozen' WHERE id=? AND status='processing'", id)
	return err
}

func (s *Service) DismissAlert(id int64) error {
	tx, err := s.repo.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var status string
	if err := tx.Get(&status, "SELECT status FROM risk_alerts WHERE id=? FOR UPDATE", id); err != nil {
		return err
	}
	if status == "dismissed" {
		return nil
	}
	if status != "pending" {
		return ErrAlertConflict
	}
	if _, err := tx.Exec("UPDATE risk_alerts SET status='dismissed' WHERE id=?", id); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Service) FreezeUser(ctx context.Context, operatorID, id int64, ip string) error {
	if operatorID == id {
		return fmt.Errorf("不能冻结当前登录账户")
	}
	tx, err := s.repo.db.Beginx()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	before, err := s.repo.freezeUserTx(tx, id)
	if err != nil {
		return err
	}
	if before != "frozen" {
		if _, err := tx.Exec("INSERT INTO audit_logs (operator_id,action,target_type,target_id,before_value,after_value,ip) VALUES (?,'freeze_user','user',?,?,'frozen',?)", operatorID, id, before, ip); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.RevokeUserSessions(ctx, id)
}
