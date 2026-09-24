package provider

import (
	"github.com/seven7628/hai-harness/core"
	"time"
)

// StreamEvent 是 provider 层输出的 LLM 事件，描述单次模型调用的流式过程。
type StreamEvent interface {
	streamEvent()
}

type LLMStartEvent struct {
	RequestId string
	Model     string

	Timestamp time.Time
}

func (e LLMStartEvent) streamEvent() {}

type LLMContentDeltaEvent struct {
	RequestId string // 请求 ID（厂商响应 id），事件自包含标识
	Delta     string
	// Index 该请求内 content 块序号（0 起自增）：流式渲染顺序与审计用。
	// （非厂商 choices 序号——SDK 恒单 choice 恒 0，无信息量；2026-08-10 修正）
	Index int64
}

func (e LLMContentDeltaEvent) streamEvent() {}

type LLMReasoningDeltaEvent struct {
	RequestId string // 请求 ID（厂商响应 id），事件自包含标识
	Delta     string
	// Index 该请求内 reasoning 块序号（0 起自增），与 content 独立计数。
	Index int64
}

func (e LLMReasoningDeltaEvent) streamEvent() {}

type LLMToolCallStartEvent struct {
	Id    string
	Index int64
	Name  string
}

func (e LLMToolCallStartEvent) streamEvent() {}

type LLMToolCallDeltaEvent struct {
	Id    string
	Index int64
	Name  string
	Delta string // arguments 增量
}

func (e LLMToolCallDeltaEvent) streamEvent() {}

type LLMEndEvent struct {
	RequestId string
	Model     string

	Content   string
	Reasoning string
	// ReasoningSignature Anthropic thinking 块 signature（多轮 thinking 回传必需；
	// OpenAI 系为空）。2026-08 新增：anthropic 流解析填充，agents 存历史时透传。
	ReasoningSignature string
	ToolCalls          []core.ToolCall

	FinishReason core.FinishReason

	Usage core.Usage

	Timestamp time.Time
}

func (e LLMEndEvent) streamEvent() {}
