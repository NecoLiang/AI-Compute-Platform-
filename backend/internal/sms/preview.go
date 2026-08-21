package sms

import (
	"context"
	"sync"
)

type PreviewSender struct {
	mu    sync.Mutex
	codes map[string]string
}

func NewPreviewSender() *PreviewSender {
	return &PreviewSender{codes: make(map[string]string)}
}

func (s *PreviewSender) SendCode(_ context.Context, phone, code, purpose string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.codes[phone+"\x00"+purpose] = code
	return nil
}

func (s *PreviewSender) TakePreviewCode(phone, purpose string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := phone + "\x00" + purpose
	code, ok := s.codes[key]
	delete(s.codes, key)
	return code, ok
}
