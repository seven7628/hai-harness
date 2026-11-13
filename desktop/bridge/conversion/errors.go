package conversion

import (
	"errors"
	"fmt"
)

// 稳定错误码（R08a，01-design §4.7「限额与口径」）。
//
// 为什么带码而不是只给一句话：前端只有 error 字符串，分支只能猜（"超时"还是"文件坏了"？
// 前者该重试、后者重试没用）。码进响应信封的 code 字段（复用 bridge 的 respCode），
// error 文本保持人话 —— 两条信息，互不替代。
const (
	// CodeTooLarge 输入超过硬上限（默认 100 MiB）：**不转换**，直接拒。
	CodeTooLarge = "too_large"
	// CodeTimeout 单次转换超过时限（默认 60s）被终止（同步 kill 掉进程组）。
	CodeTimeout = "timeout"
	// CodeCanceled 调用方取消（工作区关闭/应用退出）；与 timeout 分开，前端说法不同。
	CodeCanceled = "canceled"
	// CodeQueueTimeout 排队等待超过上界（默认 2×60s）：机器上同时有别的转换在跑，
	// 本次**还没开始转**就被放弃。与 timeout 分开：前者「快到了、重试就行」，
	// 后者「这份文件转不动」。
	CodeQueueTimeout = "queue_timeout"
	// CodeFailed 转换失败（Python 异常、产物缺失、解析不了这份文件……）。
	CodeFailed = "failed"
	// CodeNotFound 输入文件不存在/读不了。
	CodeNotFound = "not_found"
	// CodeInvalidArgument 请求本身不合法（kind 未知、路径为空/不是普通文件）。
	CodeInvalidArgument = "invalid_argument"
)

// Error 带稳定码的转换错误。Message 原样进响应信封的 error（人话，已截断）。
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Message }

// NewError 构造带码错误。
func NewError(code, message string) *Error { return &Error{Code: code, Message: message} }

// CodeOf 取错误码（非本包错误 → 空串 = 不写 code 字段，与历史行为一致）。
func CodeOf(err error) string {
	var ce *Error
	if errors.As(err, &ce) {
		return ce.Code
	}
	return ""
}

// maxErrChars 错误文本的截断上限。
//
// 为什么要截断：Python 异常经 `singleLine` 压成一行后可能带整段 traceback + 源码片段
// （实测一条 ModuleNotFoundError 能到 8 KiB），塞进响应信封只会把错误面板刷屏。
// 为什么**两头都留**（而不是简单截尾）：开头是我们自己的判据（"转换失败: "），
// 结尾才是 python 的异常行（traceback 的最后一行）—— 只留头会丢掉唯一有用的那句，
// 只留尾会丢掉「这是哪一步失败的」。
const maxErrChars = 1200

// clipErr 把错误文本截到 maxErrChars（超长时保留头 300 + 尾 ~900，见 maxErrChars）。
func clipErr(s string) string {
	if len(s) <= maxErrChars {
		return s
	}
	const head = 300
	tail := maxErrChars - head
	// 按字节切中文会切出半个字：退到一个 UTF-8 起始字节的安全边界。
	return s[:head] + " …（中间已省略）… " + safeTail(s, tail)
}

// safeTail 取 UTF-8 安全的后 n 字节（不从一个多字节字符中间开始）。
func safeTail(s string, n int) string {
	if len(s) <= n {
		return s
	}
	t := s[len(s)-n:]
	for i := 0; i < 4 && i < len(t); i++ {
		if t[i]&0xC0 != 0x80 { // 非续字节 = 字符起点
			return t[i:]
		}
	}
	return t
}

// failed 把任意转换错误规整成带码错误（保留原文，只截长度）。
func failed(format string, args ...any) *Error {
	return NewError(CodeFailed, clipErr(fmt.Sprintf(format, args...)))
}
