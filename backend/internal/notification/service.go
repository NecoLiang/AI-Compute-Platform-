package notification

import (
	"fmt"
	"strings"
)

// ===== 类型常量 =====

const (
	TypeSystem = "system" // 系统通知
	TypeOrder  = "order"  // 订单动态
	TypeTicket = "ticket" // 工单消息
)

// ValidType 通知类型白名单。
func ValidType(t string) bool {
	return t == TypeSystem || t == TypeOrder || t == TypeTicket
}

// ===== Service =====

type Service struct {
	repo *Repository
}

func NewService(repo *Repository) *Service {
	return &Service{repo: repo}
}

// Record 业务事件产生通知。供 compute/invoice/ticket 等业务包注入调用;
// 写入失败返回 error 但不值得中断主流程, 调用方应记日志而非中断事务。
func (s *Service) Record(userID int64, notifType, title, content, link string) error {
	if s == nil {
		return fmt.Errorf("notification service not configured")
	}
	if userID <= 0 || !ValidType(notifType) || strings.TrimSpace(title) == "" {
		return fmt.Errorf("invalid notification payload")
	}
	_, err := s.repo.Create(&Notification{
		UserID: userID, Type: notifType,
		Title: strings.TrimSpace(title), Content: strings.TrimSpace(content), Link: link,
	})
	return err
}

func (s *Service) List(userID int64, notifType string, page, pageSize int) ([]Notification, int64, int64, map[string]int64, error) {
	if notifType != "" && !ValidType(notifType) {
		return nil, 0, 0, nil, fmt.Errorf("invalid type")
	}
	return s.repo.List(userID, notifType, page, pageSize)
}

// MarkRead 标已读: 不存在/非本人/已读过一律报 not found, 不泄露存在性。
func (s *Service) MarkRead(userID, id int64) error {
	ok, err := s.repo.MarkRead(userID, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("notification not found")
	}
	return nil
}

func (s *Service) MarkAllRead(userID int64) (int64, error) {
	return s.repo.MarkAllRead(userID)
}

func (s *Service) Delete(userID, id int64) error {
	ok, err := s.repo.SoftDelete(userID, id)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("notification not found")
	}
	return nil
}
