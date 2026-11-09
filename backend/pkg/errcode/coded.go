package errcode

import (
	"errors"
	"fmt"
)

// CodedError 携带业务错误码的错误。用于跨模块传递"这是 40900 还是 40001"这类语义,
// 避免各 handler 依赖错误文案做字符串匹配。Msg 必须是可直接展示给用户的中文提示。
type CodedError struct {
	Code int
	Msg  string
}

func (e *CodedError) Error() string { return e.Msg }

// New 构造带错误码的错误。
func New(code int, msg string) error { return &CodedError{Code: code, Msg: msg} }

// Newf 同 New, 支持格式化。
func Newf(code int, format string, a ...any) error {
	return &CodedError{Code: code, Msg: fmt.Sprintf(format, a...)}
}

// CodeOf 取错误链上的业务错误码; 不是 CodedError 时返回 fallback。
func CodeOf(err error, fallback int) int {
	var ce *CodedError
	if errors.As(err, &ce) {
		return ce.Code
	}
	return fallback
}
