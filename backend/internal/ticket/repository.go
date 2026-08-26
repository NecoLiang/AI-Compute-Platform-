package ticket

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// ===== Models =====

// Ticket 工单。REQ-A-044: 买家对订单发起工单(故障/不可用/申诉), 运营介入处理。
type Ticket struct {
	ID         int64      `db:"id" json:"id"`
	TicketNo   string     `db:"ticket_no" json:"ticket_no"`
	BuyerID    int64      `db:"buyer_id" json:"buyer_id"`
	OrderNo    string     `db:"order_no" json:"order_no"`
	Type       string     `db:"type" json:"type"`
	Title      string     `db:"title" json:"title"`
	Content    string     `db:"content" json:"content"`
	Status     string     `db:"status" json:"status"`
	ResolvedAt *time.Time `db:"resolved_at" json:"resolved_at"`
	ClosedAt   *time.Time `db:"closed_at" json:"closed_at"`
	CreatedAt  time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt  time.Time  `db:"updated_at" json:"updated_at"`
}

// Message 工单沟通记录。只追加, 不修改。
type Message struct {
	ID         int64     `db:"id" json:"id"`
	TicketID   int64     `db:"ticket_id" json:"ticket_id"`
	SenderType string    `db:"sender_type" json:"sender_type"`
	SenderID   int64     `db:"sender_id" json:"sender_id"`
	Content    string    `db:"content" json:"content"`
	CreatedAt  time.Time `db:"created_at" json:"created_at"`
}

const ticketColumns = `id, ticket_no, buyer_id, order_no, type, title, content, status,
	resolved_at, closed_at, created_at, updated_at`

const messageColumns = `id, ticket_id, sender_type, sender_id, content, created_at`

// ===== Repository =====

type Repository struct {
	db *sqlx.DB
}

func NewRepository(db *sqlx.DB) *Repository {
	return &Repository{db: db}
}

// OrderOwnedBy 确认订单归属买家本人, 供创建工单时校验。
func (r *Repository) OrderOwnedBy(buyerID int64, orderNo string) (bool, error) {
	var n int
	err := r.db.Get(&n, "SELECT COUNT(*) FROM orders WHERE buyer_id=? AND order_no=?", buyerID, orderNo)
	return n > 0, err
}

// NextTicketNo 事务内生成 WO-YYYYMMDD-NNN: 按日递增, UNIQUE 兜底并发。
func (r *Repository) NextTicketNo(tx *sqlx.Tx, day string) (string, error) {
	prefix := "WO-" + day + "-"
	var maxNo sql.NullString
	if err := tx.Get(&maxNo,
		"SELECT MAX(ticket_no) FROM tickets WHERE ticket_no LIKE ? FOR UPDATE", prefix+"%"); err != nil {
		return "", err
	}
	return NextTicketNoFromMax(prefix, maxNo.String), nil
}

func (r *Repository) CreateTicketTx(tx *sqlx.Tx, t *Ticket) (int64, error) {
	res, err := tx.Exec(
		`INSERT INTO tickets (ticket_no, buyer_id, order_no, type, title, content, status)
		VALUES (?,?,?,?,?,?, 'pending')`,
		t.TicketNo, t.BuyerID, t.OrderNo, t.Type, t.Title, t.Content,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (r *Repository) CreateMessageTx(tx *sqlx.Tx, m *Message) error {
	var err error
	if tx != nil {
		_, err = tx.Exec(
			"INSERT INTO ticket_messages (ticket_id, sender_type, sender_id, content) VALUES (?,?,?,?)",
			m.TicketID, m.SenderType, m.SenderID, m.Content)
	} else {
		_, err = r.db.Exec(
			"INSERT INTO ticket_messages (ticket_id, sender_type, sender_id, content) VALUES (?,?,?,?)",
			m.TicketID, m.SenderType, m.SenderID, m.Content)
	}
	return err
}

type ListFilter struct {
	BuyerID  int64 // >0 时按买家过滤(买家侧); 0 表示全部(运营侧)
	Status   string
	Keyword  string
	Page     int
	PageSize int
}

func (f *ListFilter) normalize() {
	f.Keyword = strings.TrimSpace(f.Keyword)
	if f.Page <= 0 {
		f.Page = 1
	}
	if f.PageSize <= 0 {
		f.PageSize = 20
	}
	if f.PageSize > 100 {
		f.PageSize = 100
	}
}

func (r *Repository) ListTickets(f ListFilter) ([]Ticket, int64, error) {
	f.normalize()
	where := "WHERE 1=1"
	args := []interface{}{}
	if f.BuyerID > 0 {
		where += " AND buyer_id=?"
		args = append(args, f.BuyerID)
	}
	if f.Status != "" {
		where += " AND status=?"
		args = append(args, f.Status)
	}
	if f.Keyword != "" {
		where += " AND (ticket_no LIKE ? OR title LIKE ?)"
		kw := "%" + f.Keyword + "%"
		args = append(args, kw, kw)
	}
	var total int64
	if err := r.db.Get(&total, "SELECT COUNT(*) FROM tickets "+where, args...); err != nil {
		return nil, 0, err
	}
	list := make([]Ticket, 0)
	err := r.db.Select(&list,
		"SELECT "+ticketColumns+" FROM tickets "+where+" ORDER BY created_at DESC LIMIT ? OFFSET ?",
		append(args, f.PageSize, (f.Page-1)*f.PageSize)...)
	return list, total, err
}

func (r *Repository) GetTicketByNo(buyerID int64, ticketNo string) (*Ticket, error) {
	var t Ticket
	err := r.db.Get(&t, "SELECT "+ticketColumns+" FROM tickets WHERE buyer_id=? AND ticket_no=?", buyerID, ticketNo)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &t, err
}

func (r *Repository) GetTicketByID(id int64) (*Ticket, error) {
	var t Ticket
	err := r.db.Get(&t, "SELECT "+ticketColumns+" FROM tickets WHERE id=?", id)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	return &t, err
}

func (r *Repository) ListMessages(ticketID int64) ([]Message, error) {
	list := make([]Message, 0)
	err := r.db.Select(&list,
		"SELECT "+messageColumns+" FROM ticket_messages WHERE ticket_id=? ORDER BY created_at ASC", ticketID)
	return list, err
}

// TransitionTicket CAS 状态迁移, 防并发重复操作。
func (r *Repository) TransitionTicket(id int64, from, to string) (bool, error) {
	var q string
	switch to {
	case "processing":
		q = "UPDATE tickets SET status='processing' WHERE id=? AND status=?"
	case "resolved":
		q = "UPDATE tickets SET status='resolved', resolved_at=NOW() WHERE id=? AND status=?"
	case "closed":
		q = "UPDATE tickets SET status='closed', closed_at=NOW() WHERE id=? AND status=?"
	default:
		return false, fmt.Errorf("invalid status transition")
	}
	res, err := r.db.Exec(q, id, from)
	if err != nil {
		return false, err
	}
	affected, _ := res.RowsAffected()
	return affected == 1, nil
}
