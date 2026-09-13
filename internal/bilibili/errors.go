package bilibili

import (
	"errors"
	"fmt"
)

// B 站的错误码翻译成人话，而不是把 code: -412 直接甩给用户。
// 社区文档明确记载的风控码是 -352 / -799 / -503；-412 未见可靠出处，
// 因此除下表列出的之外，所有负数码统一按风控处理。

// Kind 是错误的大类，用于决定退出码与重试策略。
type Kind int

const (
	KindUnknown   Kind = iota
	KindSignature      // 签名失效，刷新密钥后可重试
	KindRateLimit      // 触发风控，退避后可重试
	KindNeedLogin      // 需要登录
	KindNoComment      // 评论区不可用
	KindNotFound       // 视频不存在
)

// APIError 是带平台错误码的响应错误。
type APIError struct {
	Code int
	Msg  string
	Kind Kind
	URL  string
}

func (e *APIError) Error() string {
	msg := e.Msg
	if msg == "" {
		msg = codeHint(e.Code)
	}
	return fmt.Sprintf("B站接口返回 %d：%s", e.Code, msg)
}

// Retryable 表示该错误是否值得重试。
func (e *APIError) Retryable() bool {
	return e.Kind == KindSignature || e.Kind == KindRateLimit
}

// codeHint 给出错误码的可读解释与建议动作。
func codeHint(code int) string {
	switch code {
	case -403:
		return "签名校验失败，可能需要刷新 WBI 密钥"
	case -352:
		return "触发频率限制，请加大 --delay 或稍后重试"
	case -799:
		return "请求过于频繁，请加大 --delay"
	case -503:
		return "服务暂时不可用，请稍后重试"
	case -404:
		return "视频不存在或已被删除"
	case -101:
		return "账号未登录"
	case 12002:
		return "评论区已关闭"
	case 12009:
		return "评论类型不正确"
	case 12061:
		return "需要登录，或该评论区仅对粉丝开放。请先执行 bili login"
	default:
		if code < 0 {
			return "可能触发了风控，请加大 --delay 或稍后重试"
		}
		return "未知错误"
	}
}

// classify 把平台错误码归入大类。
func classify(code int) Kind {
	switch code {
	case 0:
		return KindUnknown
	case -403:
		return KindSignature
	case -352, -799, -503:
		return KindRateLimit
	case -101, 12061:
		return KindNeedLogin
	case -404:
		return KindNotFound
	case 12002:
		return KindNoComment
	}
	if code < 0 {
		// 未列出的负数码几乎都是风控，宁可当风控处理（退避重试）也不要直接失败。
		return KindRateLimit
	}
	return KindUnknown
}

// NewAPIError 构造带分类的接口错误。
func NewAPIError(code int, msg, url string) *APIError {
	return &APIError{Code: code, Msg: msg, Kind: classify(code), URL: url}
}

// KindOf 提取错误的分类。非 APIError 返回 KindUnknown。
func KindOf(err error) Kind {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Kind
	}
	return KindUnknown
}

// WithMessage 保留错误的分类（进而保留退出码与重试策略），只替换提示文案。
// 上层命令需要给出比服务端原文更贴合当前场景的说明时用它，
// 直接用 fmt.Errorf 重新包装会把错误分类丢掉，退出码就跟着错了。
func WithMessage(err error, format string, args ...any) error {
	var ae *APIError
	if errors.As(err, &ae) {
		c := *ae
		c.Msg = fmt.Sprintf(format, args...)
		return &c
	}
	return fmt.Errorf(format+": %w", append(args, err)...)
}
