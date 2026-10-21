package events

import "context"

type EventType string

const (
	LLMStartType       EventType = "llm_start"
	LLMEndType         EventType = "llm_end"
	LLMErrorType       EventType = "llm_error"
	ReasoningChunkType EventType = "reasoning_chunk"
	ContentChunkType   EventType = "content_chunk"
	ToolRunStartType   EventType = "tool_run_start"
	ToolRunEndType     EventType = "tool_run_end"
	ToolApprovalType   EventType = "tool_approval_requested"
	// ToolOutputType 工具**运行中**的输出尾部（客户端 tail -f 用，绝不进模型上下文）。
	// 不落盘：可重建的展示层增量，与 content_chunk / reasoning_chunk 同口径。
	ToolOutputType EventType = "tool_output"
	// TaskPromotedType 运行中的工具/同步子 agent 已被摘离为后台任务（promote 成功时发出）。
	TaskPromotedType EventType = "task_promoted"

	AgentStartType EventType = "agent_start"
	AgentEndType   EventType = "agent_end"
	// AgentUserMessageType 有一条 user 消息被投入子运行上下文（见 AgentUserMessage）。
	// 让子会话的转录里能看到「谁对它说了什么」—— 在此之前 agent_send 只往 inbox 投递，
	// 宿主完全看不见，发完屏幕上什么都不显示。
	AgentUserMessageType      EventType = "agent_user_message"
	GoalAlignmentReminderType EventType = "goal_alignment_reminder"
	// RunSilentEndType run 在**收尾轮无文本**时的自然结束（原判据为「全程无文本」，
	// C9 2026-09-18 放宽；两种形态由 SawAssistantText 拆分，见 events.RunSilentEnd）。
	// 注意：这是**可观测性**事件，不是失败（IsError / AgentEnd 语义不受影响）。
	RunSilentEndType EventType = "run_silent_end"

	// MemoryActivityType Project Memory 的读/写活动（官方 memory#how-it-works 的
	// "Saved N memories" / "Recalled N memories"）。逐文件发，聚合展示由前端做，
	// 理由见 events/memory_activity.go 的注释。
	MemoryActivityType EventType = "memory_activity"

	CompressStartType EventType = "compress_start"
	CompressEndType   EventType = "compress_end"

	CommandResultType   EventType = "command_result"
	ToolHookWarningType EventType = "tool_hook_warning"
	AskUserQuestionType EventType = "ask_user_question"

	TaskStartedType EventType = "task_started"
	TaskMessageType EventType = "task_message"
	TaskEndType     EventType = "task_end"
	// TaskResultDeliveredType 后台任务结果已推入主会话上下文（Session.PushTaskResult 发出）。
	TaskResultDeliveredType EventType = "task_result_delivered"

	SessionPersistErrorType   EventType = "session_persist_error"
	SessionBudgetExceededType EventType = "session_budget_exceeded"
	SessionClosedType         EventType = "session_closed"
	SessionRunErrorType       EventType = "session_run_error"
	UserInputsConsumedType    EventType = "user_inputs_consumed"
)

type Event interface {
	Type() EventType
}

// EventHandler 事件消费者，agent 循环与工具引擎共用。
type EventHandler func(ctx context.Context, e Event)
