package browser

import (
	"errors"
	"fmt"
	"strings"
)

// —— P0 观测契约：结构化错误（§2.2/§3.3）——
// 让「失败在哪一阶段、是否可重试、应做什么」可编程呈现，而不是把中文错误
// 直接拼进 UI（否则前端会把安装/连接/超时都误标成「未设置」）。

// 错误码常量（前端据此分桶展示）。
const (
	ErrBrowserNotInstalled      = "BROWSER_NOT_INSTALLED"
	ErrBrowserNotEnabled        = "BROWSER_NOT_ENABLED"
	ErrBrowserMCPConnectTimeout = "BROWSER_MCP_CONNECT_TIMEOUT"
	ErrBrowserMCPInitialize     = "BROWSER_MCP_INITIALIZE_FAILED"
	ErrBrowserMCPTools          = "BROWSER_MCP_LIST_TOOLS_FAILED"
	ErrBrowserMCPDisconnected   = "BROWSER_MCP_DISCONNECTED"
	ErrBrowserEmbedNotReady     = "BROWSER_EMBED_NOT_READY"
	ErrBrowserCDPAttach         = "BROWSER_CDP_ATTACH_FAILED"
	ErrBrowserTargetGone        = "BROWSER_TARGET_GONE"
	ErrBrowserPageWaitTimeout   = "BROWSER_PAGE_WAIT_TIMEOUT"
	ErrBrowserCDPTimeout        = "BROWSER_CDP_TIMEOUT"
	ErrBrowserStaleRequest      = "BROWSER_STALE_REQUEST"
	// ReasonNodeMissing 状态 reason 的 node 缺失标记（前端按此 key 展示 i18n 引导文案；
	// 其余 error reason 为诊断原文透传）。node 是 Browser Use 运行期唯一外部依赖——
	// 组件已内置自动同步，用户只需确保本机装了 Node.js。
	ReasonNodeMissing = "NODE_MISSING"
)

// Error Browser Use 结构化错误。
type Error struct {
	Code      string
	Operation string
	Stage     string
	Workspace string
	SessionID string
	TargetID  string
	Retryable bool
	Cause     error
}

func (e *Error) Error() string {
	msg := e.Code
	if e.Operation != "" {
		msg += " " + e.Operation
	}
	if e.Stage != "" {
		msg += " @" + e.Stage
	}
	if e.Cause != nil {
		msg += ": " + e.Cause.Error()
	}
	return msg
}

func (e *Error) Unwrap() error { return e.Cause }

// IsNodeMissing 判断错误是否为「node 环境缺失」（defaultNodeCheck / resolveNodePath
// 报错统一标记；前端据此给 i18n 引导文案而非裸错误串）。
func IsNodeMissing(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "未找到 node") || strings.Contains(msg, "node 不可用") ||
		strings.Contains(msg, "请先安装 Node.js") || strings.Contains(msg, "请安装 Node.js")
}

// AsError 把任意错误映射为结构化 *Error（未命中保留原文 code=BROWSER_MCP_DISCONNECTED 兜底，
// 但**不误标 Provider key**——那不在本映射范围）。
func AsError(err error, operation string) *Error {
	if err == nil {
		return nil
	}
	var be *Error
	if errors.As(err, &be) {
		return be
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "未找到 node") || strings.Contains(msg, "请先安装") ||
		strings.Contains(msg, "组件未安装") || strings.Contains(msg, "元数据损坏") ||
		strings.Contains(msg, "未完整安装"):
		return &Error{Code: ErrBrowserNotInstalled, Operation: operation, Cause: err}
	case strings.Contains(msg, "插件未启用") || strings.Contains(msg, "未启用"):
		return &Error{Code: ErrBrowserNotEnabled, Operation: operation, Cause: err}
	case strings.Contains(msg, "初始化") || strings.Contains(msg, "initialize"):
		return &Error{Code: ErrBrowserMCPInitialize, Operation: operation, Stage: "mcp_initialize", Cause: err, Retryable: true}
	case strings.Contains(msg, "列工具") || strings.Contains(msg, "ListTools"):
		return &Error{Code: ErrBrowserMCPTools, Operation: operation, Stage: "mcp_list_tools", Cause: err, Retryable: true}
	case strings.Contains(msg, "target") && (strings.Contains(msg, "不存在") || strings.Contains(msg, "not found")):
		return &Error{Code: ErrBrowserTargetGone, Operation: operation, Stage: "resolve_target", Cause: err, Retryable: true}
	case strings.Contains(msg, "sessionId") && strings.Contains(msg, "不存在"):
		return &Error{Code: ErrBrowserTargetGone, Operation: operation, Stage: "resolve_target", Cause: err, Retryable: true}
	case strings.Contains(msg, "CDP 命令超时") || strings.Contains(msg, "45s 无响应"):
		return &Error{Code: ErrBrowserCDPTimeout, Operation: operation, Stage: "send_cdp_command", Cause: err, Retryable: true}
	case strings.Contains(msg, "Navigation timeout") || strings.Contains(msg, "页面等待超时"):
		return &Error{Code: ErrBrowserPageWaitTimeout, Operation: operation, Stage: "wait_navigation_or_page", Cause: err, Retryable: true}
	case strings.Contains(msg, "transport closed") || strings.Contains(msg, "connection closed") ||
		strings.Contains(msg, "context canceled"):
		return &Error{Code: ErrBrowserMCPDisconnected, Operation: operation, Stage: "mcp_connected", Cause: err, Retryable: true}
	case strings.Contains(msg, "浏览器视图尚未附加") || strings.Contains(msg, "内嵌浏览器未创建"):
		return &Error{Code: ErrBrowserEmbedNotReady, Operation: operation, Stage: "ensure_cdp_attached", Cause: err, Retryable: true}
	default:
		// 未命中：保留原文（不误标 Provider key；那由 Provider 状态单独负责）
		return &Error{Code: ErrBrowserMCPDisconnected, Operation: operation, Cause: err, Retryable: true}
	}
}

// ErrorCode 取错误码（未结构化返回兜底码）。
func ErrorCode(err error) string {
	if err == nil {
		return ""
	}
	var be *Error
	if errors.As(err, &be) && be.Code != "" {
		return be.Code
	}
	return ErrBrowserMCPDisconnected
}

// UserHint 用户可读的修复动作（§3.5 展示优先级用）。
func UserHint(err error) string {
	if err == nil {
		return ""
	}
	var be *Error
	if errors.As(err, &be) {
		switch be.Code {
		case ErrBrowserNotInstalled:
			return "请先安装 Browser 组件"
		case ErrBrowserNotEnabled:
			return "请先启用浏览器插件"
		case ErrBrowserMCPConnectTimeout:
			return "浏览器连接超时，请重试"
		case ErrBrowserMCPInitialize, ErrBrowserMCPTools:
			return "浏览器服务握手失败，请重试或检查 Node/组件"
		case ErrBrowserMCPDisconnected:
			return "浏览器连接已断开，正在重连"
		case ErrBrowserEmbedNotReady:
			return "内嵌浏览器未就绪，请稍候或重试"
		case ErrBrowserCDPTimeout:
			return "浏览器命令超时，请重试"
		case ErrBrowserTargetGone:
			return "页面已关闭或已变化，请重新打开页面"
		case ErrBrowserPageWaitTimeout:
			return "页面加载超时，请重试或等待页面完成"
		default:
			return fmt.Sprintf("浏览器错误（%s），请重试", be.Code)
		}
	}
	return "浏览器操作失败，请重试"
}
