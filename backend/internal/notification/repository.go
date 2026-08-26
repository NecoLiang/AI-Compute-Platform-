package notification

import (
	"database/sql"
	"time"

	"github.com/jmoiron/sqlx"
)

// ===== Models =====

// Notification 买家通知。已读=read_at 非空; 删除为软删。
type Notification struct {
	ID        int64      `db:"id" json:"id"`
	UserID    int64      `db:"user_id" json:"user_id"`
	Type      string     `db:"type" json:"type"`
	Title     string     `db:"title" json:"title"`
	Content   string     `db:"content" json:"content"`
	Link      string     `db:"link" json:"link"`
	ReadAt    *time.Time `db:"read_at" json:"read_at"`
	CreatedAt time.Time  `db:"created_at" json:"created_at"`
}

const columns = `id, user_id, type, title, content, link, read_at, created_at`

// ===== Repository =====

type Repository struct {
	db *sqlx.DB
}

func NewRepository(db *sqlx.DB) *Repository {
	return &Repository{db: db}
}

func (r *Repository) Create(n *Notification) (int64, error) {
	res, err := r.db.Exec(
		"INSERT INTO notifications (user_id, type, title, content, link) VALUES (?,?,?,?,?)",
		n.UserID, n.Type, n.Title, n.Content, n.Link,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// List 未删除通知, type 为空查全部; 返回 total/未读数/各类总数(均不受 type 筛选影响, 供 Tab 角标)。
func (r *Repository) List(userID int64, notifType string, page, pageSize int) ([]Notification, int64, int64, map[string]int64, error) {
	if page <= 0 {
		page = 1
	}
	if pageSize <= 0 {
		pageSize = 20
	}
	if pageSize > 100 {
		pageSize = 100
	}
	where := "WHERE user_id=? AND deleted_at IS NULL"
	args := []interface{}{userID}
	if notifType != "" {
		where += " AND type=?"
		args = append(args, notifType)
	}
	var total int64
	if err := r.db.Get(&total, "SELECT COUNT(*) FROM notifications "+where, args...); err != nil {
		return nil, 0, 0, nil, err
	}
	var unread int64
	if err := r.db.Get(&unread,
		"SELECT COUNT(*) FROM notifications WHERE user_id=? AND deleted_at IS NULL AND read_at IS NULL",
		userID); err != nil {
		return nil, 0, 0, nil, err
	}
	counts, err := r.typeCounts(userID)
	if err != nil {
		return nil, 0, 0, nil, err
	}
	list := make([]Notification, 0)
	err = r.db.Select(&list,
		"SELECT "+columns+" FROM notifications "+where+" ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?",
		append(args, pageSize, (page-1)*pageSize)...)
	return list, total, unread, counts, err
}

// typeCounts 各类消息总数(未删除), 供 Tab 角标; 缺失类型的键补 0 由调用方处理。
func (r *Repository) typeCounts(userID int64) (map[string]int64, error) {
	var rows []struct {
		Type  string `db:"type"`
		Count int64  `db:"count"`
	}
	if err := r.db.Select(&rows,
		"SELECT type, COUNT(*) AS count FROM notifications WHERE user_id=? AND deleted_at IS NULL GROUP BY type",
		userID); err != nil {
		return nil, err
	}
	counts := map[string]int64{"system": 0, "order": 0, "ticket": 0}
	for _, row := range rows {
		counts[row.Type] = row.Count
	}
	return counts, nil
}

// MarkRead CAS 标记已读: 仅本人的未读通知, 重复调用幂等。
func (r *Repository) MarkRead(userID, id int64) (bool, error) {
	res, err := r.db.Exec(
		"UPDATE notifications SET read_at=NOW() WHERE id=? AND user_id=? AND read_at IS NULL AND deleted_at IS NULL",
		id, userID)
	if err != nil {
		return false, err
	}
	affected, _ := res.RowsAffected()
	return affected == 1, nil
}

func (r *Repository) MarkAllRead(userID int64) (int64, error) {
	res, err := r.db.Exec(
		"UPDATE notifications SET read_at=NOW() WHERE user_id=? AND read_at IS NULL AND deleted_at IS NULL",
		userID)
	if err != nil {
		return 0, err
	}
	affected, _ := res.RowsAffected()
	return affected, nil
}

// SoftDelete 软删本人通知(无论已读未读), 返回是否命中。
func (r *Repository) SoftDelete(userID, id int64) (bool, error) {
	res, err := r.db.Exec(
		"UPDATE notifications SET deleted_at=NOW() WHERE id=? AND user_id=? AND deleted_at IS NULL",
		id, userID)
	if err != nil {
		return false, err
	}
	affected, _ := res.RowsAffected()
	return affected == 1, nil
}

var _ = sql.ErrNoRows // 保留引用: 与兄弟包一致的 sql 导入约定
