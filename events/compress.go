package events

import (
	"github.com/seven7628/hai-harness/core"
	"time"
)

// CompressStart 上下文压缩开始；Before 为压缩前消息数。
type CompressStart struct {
	Before int    `json:"before"`
	Reason string `json:"reason,omitempty"` // manual 或 automatic
	RunId  string `json:"run_id,omitempty"` // 所属运行；旧事件缺失时前端回退主 Agent

	// Timestamp 压缩开始时刻。trace 链路（docs/TRACE_CHAIN_REDESIGN.md）需要与
	// CompressEnd.Timestamp 配对算出压缩 span 的时长与时间轴位置。
	Timestamp time.Time `json:"timestamp"`

	EventType EventType `json:"event_type"`
}

func (c *CompressStart) Type() EventType {
	return c.EventType
}

// CompressEnd 上下文压缩结束；Before/After 为压缩前后消息数（差值即压缩量）。
// 压缩为 LLM 调用时携带 Model/Usage（含成本，已计入运行结算）；
// 失败时 After == Before 且 Error 非空（降级为不压缩，上下文不丢失）。
// CtxTokens 为压缩后有效上下文的 token 估算（字符/4，agent_loop.maybeCompact
// 在 emit 时取当时 ac.Messages 计算）：前端 ctx 环「压缩后上下文大小」的数据源；
// 失败时反映未变（压前）上下文大小。
type CompressEnd struct {
	Before int    `json:"before"`
	After  int    `json:"after"`
	Reason string `json:"reason,omitempty"` // manual 或 automatic
	RunId  string `json:"run_id,omitempty"` // 所属运行；旧事件缺失时前端回退主 Agent

	// CtxTokens 压缩后有效上下文 token 估算（>0；失败降级时 = 压前大小）。
	CtxTokens int64 `json:"ctx_tokens,omitempty"`

	Model   string      `json:"model,omitempty"`   // 生成摘要的模型
	Summary string      `json:"summary,omitempty"` // 压缩后展示给用户的摘要正文
	Usage   *core.Usage `json:"usage,omitempty"`   // 压缩调用用量（含成本）
	Error   string      `json:"error,omitempty"`   // 压缩失败原因
	// Warnings 压缩质量警告（S1-G）：摘要缺失关键节（Main Requests and Intent /
	// Current Task / File Context）；前端折叠提示。
	Warnings []string `json:"warnings,omitempty"`
	// Analysis 压缩器 <analysis> 块留档（O1/G7，2026-08-24 方案）：仅观测——
	// 随事件日志与会话 jsonl 的 compaction 记录落盘，绝不进模型上下文。
	Analysis string `json:"analysis,omitempty"`

	// Timestamp 压缩结束时刻（与 CompressStart.Timestamp 差即压缩 span 时长）。
	Timestamp time.Time `json:"timestamp"`

	EventType EventType `json:"event_type"`
}

func (c *CompressEnd) Type() EventType {
	return c.EventType
}
