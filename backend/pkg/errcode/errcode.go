package errcode

const (
	Success          = 0
	ParamInvalid     = 40001
	Unauthorized     = 40100
	TokenExpired     = 40101
	Forbidden        = 40300
	NotFound         = 40400
	Conflict         = 40900
	TooManyRequests  = 42900
	InternalError    = 50000
)

var messages = map[int]string{
	Success:         "success",
	ParamInvalid:    "参数错误",
	Unauthorized:    "未认证",
	TokenExpired:    "token已过期",
	Forbidden:       "无权限",
	NotFound:        "资源不存在",
	Conflict:        "冲突",
	TooManyRequests: "请求过于频繁",
	InternalError:   "服务内部错误",
}

func Message(code int) string {
	if msg, ok := messages[code]; ok {
		return msg
	}
	return "未知错误"
}
