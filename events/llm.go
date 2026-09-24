package events

import (
	"github.com/seven7628/hai-harness/core"
	"time"
)

// LLMStart 一次模型调用开始（与 LLMEnd 对称的调用级标识）。
type LLMStart struct {
	RunId     string    `json:"run_id,omitempty"` // 所属运行（事件自包含，多运行区分归属）
	Model     string    `json:"model"`
	RequestId string    `json:"request_id,omitempty"`
	Timestamp time.Time `json:"timestamp"`

	EventType EventType `json:"event_type"`
}

func (l *LLMStart) Type() EventType {
	return l.EventType
}

// LLMEnd 单次模型调用结束（一次 Run 可有多个）：该轮回复/思考/工具调用/用量。
type LLMEnd struct {
	RunId     string          `json:"run_id,omitempty"` // 所属运行（事件自包含）
	Content   string          `json:"content"`
	Reasoning string          `json:"reasoning"`
	ToolCalls []core.ToolCall `json:"tool_calls"`

	FinishReason core.FinishReason `json:"finish_reason"`

	Usage *core.Usage `json:"usage"`

	// 单次调用级标识（与运行级 AgentEnd 区分）
	Model     string    `json:"model"`
	RequestId string    `json:"request_id,omitempty"`
	Timestamp time.Time `json:"timestamp"`

	EventType EventType `json:"event_type"`
}

func (l *LLMEnd) Type() EventType {
	return l.EventType
}

// LLMError 一次 LLM 调用尝试失败；WillRetry 表示是否还会自动重试。
// Attempt 语义（2026-09-18 明确，防误读）：**已经失败的那一次**是第 Attempt 次，
// 不是「正在重试第几轮」—— 失败后进入退避与下一次尝试，期间前端展示的仍是本事件
// 的 Attempt（故文案统一改为「第 N/M 次失败 → 重试第 N+1/M 次」，配合 MaxAttempts
// 让用户看见进度总量，而不是像卡在「重试 1」）。
// RetryDelayMs 是本次失败后到下次尝试的退避等待时长（毫秒）；最后一次失败（不重试）为 0。
// ErrorKind 是错误分类标签（provider.ClassifyError）：rate_limit / permanent / canceled /
// generic / context_exceeded / upstream_unavailable —— 前端据此区分"上下文超限（需缩小
// 范围/压缩）"、"上游侧瞬时故障（重试耗尽后归因供应商）"等专门提示。
type LLMError struct {
	RunId        string `json:"run_id"`          // 所属运行（事件自包含）
	Model        string `json:"model,omitempty"` // 失败的那次调用用的模型（metrics 归因/展示用）
	Message      string `json:"message"`
	Attempt      int    `json:"attempt"`                  // 第几次尝试（1-based）
	MaxAttempts  int    `json:"max_attempts,omitempty"`   // 本次运行的总尝试次数上限（含首次）。UI 据此显示「第 N/M 次」
	WillRetry    bool   `json:"will_retry"`               // 是否还有重试机会
	RetryDelayMs int64  `json:"retry_delay_ms,omitempty"` // 本次失败后的退避等待（ms）
	ErrorKind    string `json:"error_kind,omitempty"`     // 错误分类标签（context_exceeded 等）

	// Usage 本次失败尝试**已经产生**的用量（含成本 µUSD；nil = 上游未回 usage 或失败在
	// 首字节之前）。语义（2026-09-23 决策「失败尝试也要记账」）：**每次尝试各自记账一次**
	// —— 失败尝试的用量走本字段，成功那次的用量走 LLMEnd.Usage，两者相加 = 本轮真实花费，
	// 不重复计。用途：会话成本累计、metrics（Kind=attempt_failed，重试浪费）、重试块展示。
	Usage *core.Usage `json:"usage,omitempty"`

	// Timestamp 本次失败时刻。trace 链路（docs/TRACE_CHAIN_REDESIGN.md）据此把重试
	// 徽章钉在时间轴上（挂在所属 run 的 llm span 上）。
	Timestamp time.Time `json:"timestamp"`

	EventType EventType `json:"event_type"`
}

func (l *LLMError) Type() EventType {
	return l.EventType
}
