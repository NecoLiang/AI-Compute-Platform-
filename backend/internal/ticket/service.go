package ticket

import (
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jmoiron/sqlx"
)

// ===== 状态与类型常量 =====

const (
	StatusPending    = "pending"    // 待处理
	StatusProcessing = "processing" // 处理中(运营已受理)
	StatusResolved   = "resolved"   // 已完结(运营完成处理)
	StatusClosed     = "closed"     // 已关闭(终态)
)

const (
	TypeRefundDispute = "refund_dispute" // 退款纠纷
	TypeResourceFault = "resource_fault" // 资源故障
	TypeUnavailable   = "unavailable"    // 资源不可用
	TypeAppeal        = "appeal"         // 申诉
	TypeOther         = "other"          // 其他
)

// ===== 纯函数(表驱动单测覆盖, 不触 DB) =====

// ValidType 工单类型白名单。
func ValidType(t string) bool {
	switch t {
	case TypeRefundDispute, TypeResourceFault, TypeUnavailable, TypeAppeal, TypeOther:
		return true
	}
	return false
}

// NextTicketNoFromMax 由当日最大编号生成下一张: 序号 +1, 无记录从 001 起。
func NextTicketNoFromMax(prefix, maxNo string) string {
	seq := 0
	if strings.HasPrefix(maxNo, prefix) {
		fmt.Sscanf(maxNo[len(prefix):], "%d", &seq)
	}
	return fmt.Sprintf("%s%03d", prefix, seq+1)
}

// CanAppendMessage 仅进行中(pending/processing)的工单允许追加沟通。
func CanAppendMessage(status string) bool {
	return status == StatusPending || status == StatusProcessing
}

// CanTransition CAS 迁移合法性: pending→processing→resolved→closed, 或 pending→closed。
func CanTransition(from, to string) bool {
	switch from {
	case StatusPending:
		return to == StatusProcessing || to == StatusClosed
	case StatusProcessing:
		return to == StatusResolved || to == StatusClosed
	case StatusResolved:
		return to == StatusClosed
	}
	return false
}

// ===== Service =====

// Notifier 消息中心写入口, 由 notification 包实现, nil 表示未启用。
type Notifier interface {
	Record(userID int64, notifType, title, content, link string) error
}

type Service struct {
	repo     *Repository
	db       *sqlx.DB
	notifier Notifier
}

func NewService(repo *Repository, db *sqlx.DB) *Service {
	return &Service{repo: repo, db: db}
}

// SetNotifier 注入消息中心; 通知写入失败只记日志, 不中断工单主流程。
func (s *Service) SetNotifier(n Notifier) {
	s.notifier = n
}

func (s *Service) notify(userID int64, title, content, link string) {
	if s.notifier == nil {
		return
	}
	if err := s.notifier.Record(userID, "ticket", title, content, link); err != nil {
		slog.Warn("工单通知写入失败", "error", err)
	}
}

// Create 买家创建工单: 订单归属校验 + 单事务写工单与首条描述消息。
func (s *Service) Create(buyerID int64, orderNo, ticketType, title, content string) (*Ticket, error) {
	orderNo = strings.TrimSpace(orderNo)
	title = strings.TrimSpace(title)
	content = strings.TrimSpace(content)
	if orderNo == "" {
		return nil, fmt.Errorf("请选择关联订单")
	}
	if !ValidType(ticketType) {
		return nil, fmt.Errorf("工单类型不正确")
	}
	if len(title) < 2 || len(title) > 128 {
		return nil, fmt.Errorf("请填写 2-128 字的工单标题")
	}
	if len(content) < 5 {
		return nil, fmt.Errorf("请填写问题描述(至少 5 个字)")
	}

	owned, err := s.repo.OrderOwnedBy(buyerID, orderNo)
	if err != nil {
		return nil, err
	}
	if !owned {
		return nil, fmt.Errorf("无权对该订单发起工单: 订单不属于当前买家")
	}

	tx, err := s.db.Beginx()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	t := &Ticket{
		BuyerID: buyerID, OrderNo: orderNo, Type: ticketType,
		Title: title, Content: content, Status: StatusPending,
	}
	if t.TicketNo, err = s.repo.NextTicketNo(tx, time.Now().Format("20060102")); err != nil {
		return nil, err
	}
	if t.ID, err = s.repo.CreateTicketTx(tx, t); err != nil {
		return nil, err
	}
	// 首条消息 = 买家提交的问题描述, 沟通记录从创建即完整。
	if err = s.repo.CreateMessageTx(tx, &Message{
		TicketID: t.ID, SenderType: "buyer", SenderID: buyerID, Content: content,
	}); err != nil {
		return nil, err
	}
	if err = tx.Commit(); err != nil {
		return nil, err
	}
	t.CreatedAt = time.Now()
	return t, nil
}

// List 买家工单列表(强制买家归属)。
func (s *Service) List(buyerID int64, status, keyword string, page, pageSize int) ([]Ticket, int64, error) {
	if status != "" && status != StatusPending && status != StatusProcessing &&
		status != StatusResolved && status != StatusClosed {
		return nil, 0, fmt.Errorf("invalid status")
	}
	return s.repo.ListTickets(ListFilter{
		BuyerID: buyerID, Status: status, Keyword: keyword, Page: page, PageSize: pageSize,
	})
}

// Detail 买家工单详情(归属校验) + 沟通记录。
func (s *Service) Detail(buyerID int64, ticketNo string) (*Ticket, []Message, error) {
	t, err := s.repo.GetTicketByNo(buyerID, ticketNo)
	if err != nil {
		return nil, nil, err
	}
	if t == nil {
		return nil, nil, fmt.Errorf("ticket not found")
	}
	msgs, err := s.repo.ListMessages(t.ID)
	return t, msgs, err
}

// AppendBuyerMessage 买家追加沟通, 仅进行中的工单可回复。
func (s *Service) AppendBuyerMessage(buyerID int64, ticketNo, content string) error {
	content = strings.TrimSpace(content)
	if len(content) < 2 {
		return fmt.Errorf("回复内容至少 2 个字")
	}
	t, err := s.repo.GetTicketByNo(buyerID, ticketNo)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("ticket not found")
	}
	if !CanAppendMessage(t.Status) {
		return fmt.Errorf("工单已完结或关闭, 无法继续回复")
	}
	return s.repo.CreateMessageTx(nil, &Message{
		TicketID: t.ID, SenderType: "buyer", SenderID: buyerID, Content: content,
	})
}

// Close 买家关闭工单: 任意非终态 → closed, CAS 幂等。
func (s *Service) Close(buyerID int64, ticketNo string) error {
	t, err := s.repo.GetTicketByNo(buyerID, ticketNo)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("ticket not found")
	}
	if t.Status == StatusClosed {
		return fmt.Errorf("工单已关闭")
	}
	ok, err := s.repo.TransitionTicket(t.ID, t.Status, StatusClosed)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("invalid status transition")
	}
	return nil
}

// ---- Admin ----

func (s *Service) AdminList(status, keyword string, page, pageSize int) ([]Ticket, int64, error) {
	if status != "" && status != StatusPending && status != StatusProcessing &&
		status != StatusResolved && status != StatusClosed {
		return nil, 0, fmt.Errorf("invalid status")
	}
	return s.repo.ListTickets(ListFilter{Status: status, Keyword: keyword, Page: page, PageSize: pageSize})
}

func (s *Service) AdminDetail(id int64) (*Ticket, []Message, error) {
	t, err := s.repo.GetTicketByID(id)
	if err != nil {
		return nil, nil, err
	}
	if t == nil {
		return nil, nil, fmt.Errorf("ticket not found")
	}
	msgs, err := s.repo.ListMessages(t.ID)
	return t, msgs, err
}

// AdminTransition 运营状态操作: 受理(pending→processing) / 完结(processing→resolved) / 关闭(→closed)。
func (s *Service) AdminTransition(id int64, to string) error {
	t, err := s.repo.GetTicketByID(id)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("ticket not found")
	}
	if !CanTransition(t.Status, to) {
		return fmt.Errorf("invalid status transition")
	}
	ok, err := s.repo.TransitionTicket(t.ID, t.Status, to)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("invalid status transition")
	}
	copy := map[string]string{
		StatusProcessing: "已受理, 处理中",
		StatusResolved:   "已完结",
		StatusClosed:     "已关闭",
	}[to]
	s.notify(t.BuyerID, "工单状态更新",
		fmt.Sprintf("您的工单 %s「%s」%s。", t.TicketNo, t.Title, copy),
		"/console/buyer/tickets/"+t.TicketNo)
	return nil
}

// AdminAppendMessage 运营回复; 受理后(processing)才能回复, 避免未受理先答复。
func (s *Service) AdminAppendMessage(operatorID, id int64, content string) error {
	content = strings.TrimSpace(content)
	if len(content) < 2 {
		return fmt.Errorf("回复内容至少 2 个字")
	}
	t, err := s.repo.GetTicketByID(id)
	if err != nil {
		return err
	}
	if t == nil {
		return fmt.Errorf("ticket not found")
	}
	if t.Status != StatusProcessing {
		return fmt.Errorf("请先受理工单再回复")
	}
	if err := s.repo.CreateMessageTx(nil, &Message{
		TicketID: t.ID, SenderType: "operator", SenderID: operatorID, Content: content,
	}); err != nil {
		return err
	}
	s.notify(t.BuyerID, "工单有新回复",
		fmt.Sprintf("您的工单 %s「%s」收到平台运营回复。", t.TicketNo, t.Title),
		"/console/buyer/tickets/"+t.TicketNo)
	return nil
}
