package provider

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/core"
)

// RateLimitError 可重试的限流错误（HTTP 429）。
// 与 PermanentError 相对：IsPermanent 不命中，重试层按限流档位退避后重试。
type RateLimitError struct {
	StatusCode int
	Err        error
}

func (e *RateLimitError) Error() string {
	status := e.StatusCode
	if status == 0 {
		status = http.StatusTooManyRequests
	}
	return fmt.Sprintf("rate limit exceeded (HTTP %d): %v", status, e.Err)
}

func (e *RateLimitError) Unwrap() error { return e.Err }

// UpstreamUnavailableError 上游模型服务瞬时不可用（5xx 网关错误 / 连接层重置等）。
// 与 RateLimitError 同档：**可重试**，永不被 MarkPermanent（IsPermanent 恒 false）——
// 把 5xx 标成 permanent 会退化成「一次 503 就杀会话」，比不分类更糟。
// 独立成类只为**归因**：重试耗尽后上层要能告诉用户「这是供应商侧抖动」。
type UpstreamUnavailableError struct {
	StatusCode int
	Err        error
}

func (e *UpstreamUnavailableError) Error() string {
	if e.Err == nil {
		return fmt.Sprintf("upstream unavailable (HTTP %d)", e.StatusCode)
	}
	if e.StatusCode > 0 {
		return fmt.Sprintf("upstream unavailable (HTTP %d): %v", e.StatusCode, e.Err)
	}
	return e.Err.Error()
}

func (e *UpstreamUnavailableError) Unwrap() error { return e.Err }

// FirstChunkTimeoutError 首字超时（2026-09-18 定策）：**单次尝试**发出请求后，
// 在首字预算内未收到模型首个流事件 —— 建连、响应头、首个 SSE 事件全链路都算在内。
//
// 与其它超时型错误的边界（勿混用）：
//   - 「首字超时」= 还没有任何动静（事件流里无 llm_start）；这是**唯一**可安全设界的时点，
//     且必须设：实测一次尝试可静默挂 187s（21:39→21:42 连接被对端 RST），期间重试循环
//     停在「重试 N」上，用户看到的是「卡住」而不是「在重试」；
//   - 「流开始后」不设界（长 reasoning / 大文档生成可达数分钟，设界会误杀健康生成）。
//
// 归因：上游侧（网关排队 / 供应商无响应）—— 与 UpstreamUnavailableError 同档，
// 可重试（IsPermanent 恒 false），重试耗尽后按「供应商侧瞬时故障」提示用户。
type FirstChunkTimeoutError struct {
	Timeout time.Duration // 首字预算（策略值，仅用于文案）
	Cause   error         // 底层错误（诊断用；构造时剥离取消语义，见 NewFirstChunkTimeoutError）
}

// NewFirstChunkTimeoutError 构造首字超时错误。cause 里的 context.Canceled /
// context.DeadlineExceeded 一律丢弃：前者会被 retryable() 当作「用户中断」而不重试
// （错误传染链 errors.Is），后者会被误归因为 LLMTimeout 总时限。首字超时必须是一次
// **可重试的**上游故障。
func NewFirstChunkTimeoutError(timeout time.Duration, cause error) *FirstChunkTimeoutError {
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		cause = nil
	}
	return &FirstChunkTimeoutError{Timeout: timeout, Cause: cause}
}

func (e *FirstChunkTimeoutError) Error() string {
	if e.Timeout <= 0 {
		return "首字超时：请求发出后未在预算内收到模型首个流事件（上游无响应）"
	}
	msg := fmt.Sprintf("首字超时：请求发出后 %s 内未收到模型首个流事件（上游网关排队或无响应），本次尝试终止", e.Timeout)
	if e.Cause != nil {
		msg += "；底层错误：" + e.Cause.Error()
	}
	return msg
}

func (e *FirstChunkTimeoutError) Unwrap() error { return e.Cause }

// SetCause 记录底层错误（诊断用）。取消/超时类一律忽略 —— 与构造器同一理由：
// 让 errors.Is(err, context.Canceled) 永远不成立，否则重试层会把首字超时
// 误判成「用户中断」而不重试。
func (e *FirstChunkTimeoutError) SetCause(err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	e.Cause = err
}

// StreamStallError 流式停滞（2026-09-22）：**流已经开始**（至少收到过一个事件）后，
// 在停滞预算内没有再收到任何 provider 事件 —— 上游连接僵死（TCP 未断开，也不再发字节）。
//
// 与 FirstChunkTimeoutError 的边界（互补，勿混用）：
//   - FirstChunkTimeoutError = 「还没有第一个事件」（请求 → 首字，30s）；
//   - StreamStallError       = 「已经有事件了，但接下来没有动静」（事件 → 下一个事件，3min）。
//
// 两者都**不**限制流的总时长：健康的长流式（长 reasoning / 大文档生成）每个事件都重置
// 停滞计时，命中不了本错误 —— 这正是「流开始后不设总时限」与「不允许永远没有动静」
// 两条约定的交界。
//
// 归因同首字超时：上游侧（连接挂死/供应商静默），可重试（IsPermanent 恒 false），
// 重试耗尽后按「供应商侧瞬时故障」提示用户。动因：GLM Provider 实测「llm end 前卡住、
// 超过 5 分钟没有任何输出」—— 此前流内完全不设界，provider 的 Recv 会一直阻塞，
// 只能等对端 RST 或用户手动点停止。
type StreamStallError struct {
	Timeout time.Duration // 停滞预算（策略值，仅用于文案）
	Cause   error         // 底层错误（诊断用；构造时剥离取消语义）
}

// NewStreamStallError 构造流式停滞错误。cause 里的 context.Canceled /
// context.DeadlineExceeded 一律丢弃（同 NewFirstChunkTimeoutError 的理由：前者会被
// retryable() 判成「用户中断」而不重试，后者会被误归因为 LLMTimeout 总时限）。
func NewStreamStallError(timeout time.Duration, cause error) *StreamStallError {
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		cause = nil
	}
	return &StreamStallError{Timeout: timeout, Cause: cause}
}

func (e *StreamStallError) Error() string {
	if e.Timeout <= 0 {
		return "流式响应停滞：已收到首字后长时间未再收到模型事件（上游连接挂死），本次尝试终止"
	}
	msg := fmt.Sprintf("流式响应停滞：已收到首字后 %s 内未再收到任何模型事件（上游连接挂死或静默无响应），本次尝试终止", e.Timeout)
	if e.Cause != nil {
		msg += "；底层错误：" + e.Cause.Error()
	}
	return msg
}

func (e *StreamStallError) Unwrap() error { return e.Cause }

// SetCause 记录底层错误（诊断用）。取消/超时类一律忽略 —— 与构造器同一理由：
// 让 errors.Is(err, context.Canceled) 永远不成立，否则重试层会把停滞误判成用户中断。
func (e *StreamStallError) SetCause(err error) {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	e.Cause = err
}

// 错误分类（传导到 Agent/Session 层的类型标签，供上层区分处理：
// 瞬时错误重试耗尽可稍后重试；永久错误是配置/参数问题，重试无意义）。
const (
	ErrorKindRateLimit       = "rate_limit"       // 限流（429）重试耗尽
	ErrorKindPermanent       = "permanent"        // 永久错误（4xx 除可重试集合）
	ErrorKindCanceled        = "canceled"         // 上下文取消（自动：会话关闭/后台任务终止等）
	ErrorKindGeneric         = "generic"          // 其他（超时/网络/内部，重试耗尽）
	ErrorKindContextExceeded = "context_exceeded" // LLM 上下文超限（不可重试，需缩小范围/压缩）

	// ErrorKindAborted 用户主动中断（点停止 / agent_interrupt）。与 ErrorKindCanceled 的
	// 关键区别（2026-09-24 拆分，此前两者共用 canceled）：两者的 err 都是 context.Canceled、
	// 都不重试，但**归因完全不同** —— canceled 是系统侧取消，aborted 是用户自己按的停止。
	// 共用 canceled 时 UI 只能输出「重试已耗尽」（第 1 次就因取消而终止，一次都没重试），
	// 用户读到的却是「重试机制没了」。
	// 判定权在 agents 层（ac.IsAborted()，见 streamWithRetry.llmErrorKind）：provider 层不持有
	// abort 状态，故本常量由调用方在取消时按 abort 标志覆盖，ClassifyError 不返回它。
	ErrorKindAborted = "aborted"

	// ErrorKindUpstreamUnavailable 上游模型服务瞬时不可用（5xx 网关错误、连接被重置等）。
	// 语义：可重试、且**不是**调用方的问题——终止时必须让用户知道这是供应商侧抖动。
	ErrorKindUpstreamUnavailable = "upstream_unavailable"
)

// ClassifyError 分类错误为类型标签（传播链终点：AgentEnd.ErrorKind / SessionRunError.ErrorKind）。
func ClassifyError(err error) string {
	if err == nil {
		return ""
	}
	// 上下文超限优先于通用 permanent：语义明确、前端据此输出专门的"为什么结束"提示
	//（context_exceeded 同时满足 IsPermanent —— 不重试，见 DetectContextExceeded）。
	var ce *ContextExceededError
	switch {
	case errors.As(err, &ce):
		return ErrorKindContextExceeded
	case errors.Is(err, context.Canceled):
		return ErrorKindCanceled
	case IsPermanent(err):
		return ErrorKindPermanent
	case isUpstreamUnavailable(err):
		// 顺序在 IsPermanent **之后**：permanent 语义优先，避免抢走真正的 4xx 配置错误。
		return ErrorKindUpstreamUnavailable
	default:
		var rl *RateLimitError
		if errors.As(err, &rl) {
			return ErrorKindRateLimit
		}
		return ErrorKindGeneric
	}
}

// ContextExceededError LLM 上下文超限错误（模型窗口被请求撑爆）。
// 与普通 4xx permanent 的区别：语义可识别 —— 前端/任务收尾可以输出明确的
// "上下文为什么结束 / 下一步怎么办"提示，而不是原始 provider 报错串。
// 实现即 Permanent（IsPermanent 恒 true）：上层绝不重试（重试只会再次发送
// 同一份超限上下文，浪费且无意义）。
type ContextExceededError struct {
	Message   string // 原始错误（附录保留）
	Window    int64  // 尽力解析的模型窗口（tokens；解析不到 = 0）
	Requested int64  // 尽力解析的请求量（tokens；解析不到 = 0）
	Err       error  // 原始错误链（Unwrap 保留）
}

func (e *ContextExceededError) Error() string {
	var b strings.Builder
	b.WriteString("LLM 上下文超限终止")
	switch {
	case e.Window > 0 && e.Requested > 0:
		fmt.Fprintf(&b, "：模型窗口 %d tokens，本次请求约 %d tokens（超出上限）", e.Window, e.Requested)
	case e.Window > 0:
		fmt.Fprintf(&b, "：模型窗口约 %d tokens", e.Window)
	}
	b.WriteString("。原因：工具结果 / 文件内容 / 历史消息累积超出模型上下文窗口。")
	b.WriteString("建议：① 缩小任务范围或拆分后重试；②（主会话）使用 /compact 压缩上下文后继续；")
	b.WriteString("③ 检查是否有工具返回超大结果（bash / read_file 长输出）。")
	if e.Message != "" {
		b.WriteString("\n原始错误：")
		b.WriteString(e.Message)
	}
	if e.Err != nil && e.Err.Error() != "" && e.Err.Error() != e.Message {
		b.WriteString("\n")
		b.WriteString(e.Err.Error())
	}
	return b.String()
}

func (e *ContextExceededError) Unwrap() error { return e.Err }

// contextExceededPatterns 各家网关对上下文超限的常见措辞（大小写不敏感，命中任一即判定）。
var contextExceededPatterns = []string{
	"maximum context length",
	"context length exceeded",
	"context_length_exceeded",
	"context length is",
	"please reduce the length of the messages",
	"please reduce the length of the message",
	"too many tokens",
	"exceeds the model's max",
	"exceeds the maximum context",
	"prompt is too long",
	"input is too long",
}

// isContextExceededMessage 判断错误文本是否表示 LLM 上下文超限。
func isContextExceededMessage(msg string) bool {
	low := strings.ToLower(msg)
	for _, p := range contextExceededPatterns {
		if strings.Contains(low, p) {
			return true
		}
	}
	return false
}

// contextExceededTokens 从错误文本尽力解析 token 数字：取所有 "N tokens" 中的 N
// （第一个 = 窗口，第二个 = 请求量；依各家报错措辞 best-effort，解析不出来 = 0）。
func contextExceededTokens(msg string) (window, requested int64) {
	re := regexp.MustCompile(`(\d[\d,]*)\s*tokens`)
	matches := re.FindAllStringSubmatch(msg, 2)
	parse := func(s string) int64 {
		n, err := strconv.ParseInt(strings.ReplaceAll(s, ",", ""), 10, 64)
		if err != nil {
			return 0
		}
		return n
	}
	for i, m := range matches {
		if i == 0 {
			window = parse(m[1])
		} else {
			requested = parse(m[1])
		}
	}
	return window, requested
}

// DetectContextExceeded 若错误链文本表明 LLM 上下文超限 → 包装为 ContextExceededError
// （幂等：已是 ContextExceededError 返回原样）。包装后 IsPermanent 恒 true（不重试），
// ClassifyError 返回 context_exceeded。调用点：provider 各错误分类出口（400 响应 /
// 流中 SSE 错误帧 / 兼容网关 200+error frame 均覆盖）。
func DetectContextExceeded(err error) error {
	if err == nil {
		return nil
	}
	var existing *ContextExceededError
	if errors.As(err, &existing) {
		return err
	}
	if !isContextExceededMessage(err.Error()) {
		return err
	}
	window, requested := contextExceededTokens(err.Error())
	return &ContextExceededError{Message: err.Error(), Window: window, Requested: requested, Err: err}
}

// NotImplementedError 标记尚未实现的协议（骨架占位用；实现完成后移除）。
type NotImplementedError struct {
	Msg string
}

func (e *NotImplementedError) Error() string { return e.Msg }

// NewNotImplementedError 构造未实现错误（永久错误，不重试）。
func NewNotImplementedError(msg string) error {
	return MarkPermanent(&NotImplementedError{Msg: msg})
}

// ClassifyHTTPError 协议无关的 HTTP 错误分类器（三协议共用）：
//   - 429 → RateLimitError（可重试，宽退避）
//   - 408/409/425 → 原样可重试（瞬时/竞争）
//   - 其余 4xx → DetectContextExceeded(MarkPermanent(err))（永久；上下文超限优先识别）
//   - 5xx（含 502/503/520/524）→ UpstreamUnavailableError（**仍可重试**，仅作归因标记）
func ClassifyHTTPError(statusCode int, err error) error {
	if statusCode == http.StatusTooManyRequests {
		return &RateLimitError{StatusCode: statusCode, Err: err}
	}
	if retryableClientErrors[statusCode] {
		return err
	}
	if statusCode >= http.StatusInternalServerError {
		return &UpstreamUnavailableError{StatusCode: statusCode, Err: err}
	}
	return DetectContextExceeded(MarkPermanent(err))
}

// retryableClientErrors 可重试的 4xx 状态码（瞬时/竞争类，退避后重试有意义）：
// 408 请求超时（网关侧瞬时）、409 冲突（并发写）、425 过早请求（连接刚建）。
// 429 单独处理为 RateLimitError（宽退避档位），不在此集合。
var retryableClientErrors = map[int]bool{
	http.StatusRequestTimeout: true,
	http.StatusConflict:       true,
	http.StatusTooEarly:       true,
}

// IsRateLimitText 流中段错误帧的限流文本识别（无 HTTP 状态码时的兜底）：
// 覆盖 OpenAI/DeepSeek/Anthropic 等网关 SSE error frame 的常见 429 措辞。
// 三协议 classify*Error 共用（流中段 200 + error frame 表达 429 时无状态码可取）。
func IsRateLimitText(msg string) bool {
	low := strings.ToLower(msg)
	for _, p := range []string{"rate limit", "rate_limit", "too many requests", "429"} {
		if strings.Contains(low, p) {
			return true
		}
	}
	return false
}

// upstreamUnavailablePatterns 上游瞬时不可用的**无歧义措辞**（大小写不敏感，命中任一即判定）。
// 参照实测日志出现过的原文：Service Unavailable / Endpoint is unavailable /
// Upstream request failed / bad gateway / Upstream model provider is temporarily unavailable /
// connection reset by peer。
//
// 注意 1：**不含**超时/取消措辞（context deadline exceeded / context canceled）——
// 那两类必须留在原类别，不能被误归因成「供应商抖动」。
// 注意 2（2026-09-18 复核收紧）：**不再放裸数字**（原实现含 "503"/"502"/"520"/"524"）。
// 裸数字会被 request-id / token 数 / 行号里的同形数字误命中，而本类别会影响 UI 归因
// （展示「供应商侧瞬时故障」），误判等于对用户说谎。状态码改为**要求上下文**（见下）。
var upstreamUnavailablePatterns = []string{
	"service unavailable",
	"endpoint is unavailable",
	"upstream request failed",
	"upstream model provider is temporarily unavailable",
	"bad gateway",
	"connection reset by peer",
}

// upstreamStatusRe 带上下文的 5xx 状态码：必须出现 status/http 类词，紧随其后（允许
// 冒号/等号/空白/逗号）是 5xx。这样 "status code: 503"、"http 502"、"status: 520"
// 都能命中，而孤立的 "503" 字符串（request-id 等）不会。
var upstreamStatusRe = regexp.MustCompile(`(?i)(status code|http status|status|http)[:=\s,]{0,3}5\d\d`)

// IsUpstreamUnavailableText 文本兜底：无 HTTP 状态码时（SSE error frame / 连接层错误）
// 识别上游瞬时不可用的常见措辞。三协议共用，风格对齐 IsRateLimitText。
func IsUpstreamUnavailableText(msg string) bool {
	if msg == "" {
		return false
	}
	if upstreamStatusRe.MatchString(msg) {
		return true
	}
	low := strings.ToLower(msg)
	for _, p := range upstreamUnavailablePatterns {
		if strings.Contains(low, p) {
			return true
		}
	}
	return false
}

// isUpstreamUnavailable ClassifyError 用：类型化错误（含包装链）优先，其次文本兜底。
func isUpstreamUnavailable(err error) bool {
	var ue *UpstreamUnavailableError
	if errors.As(err, &ue) {
		return true
	}
	// 首字超时同档（上游侧无响应）：复用 upstream_unavailable 标签 → 前端「供应商侧
	// 瞬时故障」归因与既有 UI 分支，不再新增一个 ErrorKind。
	var ft *FirstChunkTimeoutError
	if errors.As(err, &ft) {
		return true
	}
	// 流式停滞同档（流开始后上游连接挂死）：与首字超时同一归因路径。
	var st *StreamStallError
	if errors.As(err, &st) {
		return true
	}
	return IsUpstreamUnavailableText(err.Error())
}

// PartialUsageError 把「本次尝试**已经产生**的用量」随错误一起上抛。
//
// 场景：流已开始、上游已计费，但中途失败/超时（含流内 stall、限流中断）—— 此前这部分
// 用量被整个丢弃，只有最终成功的那次进账，成本系统性低估（2026-09-23 用户决策：
// 失败尝试也要记账）。
//
// 语义契约：**每次尝试各自记账一次** —— 失败尝试的用量随 LLMError 事件进账，成功那次
// 的 LLMEnd.Usage 只含自己那次，两者相加 = 本轮真实花费，不重复计。
//
// Unwrap 保证错误链不变：ClassifyError / IsPermanent / DetectContextExceeded 等
// 全部经 errors.As/Is 穿透，分类与重试判定行为与包装前逐字一致。
type PartialUsageError struct {
	Err   error
	Usage core.Usage
}

func (e *PartialUsageError) Error() string { return e.Err.Error() }

func (e *PartialUsageError) Unwrap() error { return e.Err }

// WithPartialUsage 给 err 挂上本次尝试的部分用量（err 为 nil 或用量全零时原样返回 err）。
// provider 在各错误出口调用：WithPartialUsage(classify..., usage)。
func WithPartialUsage(err error, usage core.Usage) error {
	if err == nil || usage.IsZero() {
		return err
	}
	return &PartialUsageError{Err: err, Usage: usage}
}

// PartialUsage 取错误链里挂的部分用量（无则零值 + false）。
// 调用点：agents.streamWithRetry（事件携带 + 运行结算归集）、bridge（指标记录）。
func PartialUsage(err error) (core.Usage, bool) {
	var e *PartialUsageError
	if errors.As(err, &e) {
		return e.Usage, true
	}
	return core.Usage{}, false
}
