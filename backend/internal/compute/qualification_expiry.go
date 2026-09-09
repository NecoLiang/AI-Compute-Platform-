package compute

import (
	"database/sql"
	"fmt"
	"time"

	"tokenfactory/internal/notification"
)

// A newly approved certificate of the same type replaces earlier certificates.
// Different qualification types cannot cover each other's expiry.
const currentQualification = `NOT EXISTS (SELECT 1 FROM supplier_qualifications newer
    WHERE newer.user_id=q.user_id AND newer.qual_type=q.qual_type AND newer.id>q.id
    AND newer.status='verified' AND (newer.expires_at IS NULL OR newer.expires_at>=CURRENT_DATE))`

// ProcessQualificationExpiries uses the database's calendar date: the certificate
// remains valid on its expiry day. Returns the number of notifications committed.
func (s *Service) ProcessQualificationExpiries() (int, error) {
	var ids []int64
	if err := s.db.Select(&ids, `SELECT id FROM supplier_qualifications
        WHERE status='verified' AND expires_at<=DATE_ADD(CURRENT_DATE, INTERVAL 30 DAY) ORDER BY id`); err != nil {
		return 0, err
	}
	count := 0
	var firstErr error
	for _, id := range ids {
		sent, err := s.processQualificationExpiry(id)
		if err != nil && firstErr == nil {
			firstErr = err
		}
		if sent {
			count++
		}
	}
	return count, firstErr
}

func (s *Service) processQualificationExpiry(id int64) (bool, error) {
	tx, err := s.db.Beginx()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var q SupplierQualification
	// Serialize each certificate's status, event evidence and notification. The
	// audit survives notification deletion and distinguishes changed expiry dates.
	if err := tx.Get(&q, "SELECT "+qualificationColumns+" FROM supplier_qualifications WHERE id=? FOR UPDATE", id); err != nil {
		if err == sql.ErrNoRows {
			return false, nil
		}
		return false, err
	}
	if q.Status != "verified" || q.ExpiresInDays == nil || *q.ExpiresInDays > 30 {
		return false, nil
	}
	expired := *q.ExpiresInDays < 0
	var current bool
	if err := tx.Get(&current, "SELECT "+currentQualification+" FROM supplier_qualifications q WHERE q.id=?", id); err != nil {
		return false, err
	}
	if !current && !expired {
		return false, nil
	}
	action, title := "qualification_expiring", "资质即将到期"
	date := q.ExpiresAt.Format(time.DateOnly)
	content := fmt.Sprintf("您的资质「%s」将于 %s 到期，请及时提交新证照审核。", q.CertName, date)
	if expired {
		action, title = "qualification_expired", "资质已过期"
		content = fmt.Sprintf("您的资质「%s」已于 %s 到期，在售商品已冻结。请提交同类型新证照审核后再发布商品。", q.CertName, date)
	}
	var recorded bool
	if err := tx.Get(&recorded, `SELECT EXISTS(SELECT 1 FROM audit_logs
        WHERE target_type='supplier_qualification' AND target_id=? AND action=? AND after_value=?)`, id, action, date); err != nil {
		return false, err
	}
	if recorded {
		return false, nil
	}
	if expired {
		if _, err := tx.Exec("UPDATE supplier_qualifications SET status='expired' WHERE id=?", id); err != nil {
			return false, err
		}
		if current {
			if _, err := tx.Exec("UPDATE products SET status='frozen' WHERE supplier_id=? AND status IN ('active','sold_out')", q.UserID); err != nil {
				return false, err
			}
		}
	}
	if current {
		if _, err := notification.Create(tx, &notification.Notification{
			UserID: q.UserID, Type: notification.TypeSystem, Title: title, Content: content,
			Link: fmt.Sprintf("/console/supplier/qualifications#qualification-%d", id),
		}); err != nil {
			return false, err
		}
	}
	if _, err := tx.Exec(`INSERT INTO audit_logs (operator_id,action,target_type,target_id,before_value,after_value)
        VALUES (NULL,?,'supplier_qualification',?,?,?)`, action, id, q.Status, date); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return current, nil
}

func (r *Repository) requireUnexpiredQualifications(userID int64) error {
	var expired bool
	if err := r.db.Get(&expired, `SELECT EXISTS(SELECT 1 FROM supplier_qualifications q
        WHERE q.user_id=? AND q.status IN ('verified','expired') AND q.expires_at<CURRENT_DATE AND `+currentQualification+`)`, userID); err != nil {
		return err
	}
	if expired {
		return fmt.Errorf("供给方资质已过期，请更新资质并通过审核后再交易")
	}
	return nil
}
