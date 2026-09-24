package events

import (
	"github.com/seven7628/hai-harness/core"
	"time"
)

// SessionPersistError 会话快照保存失败（数据安全警示：长时运行中 UI 应提示「会话未保存」）。
type SessionPersistError struct {
	SessionId string `json:"session_id"`
	Error     string `json:"error"`

	EventType EventType `json:"event_type"`
}

func (e *SessionPersistError) Type() EventType {
	return e.EventType
}

// SessionBudgetExceeded 会话成本预算耗尽，拒绝新输入。
type SessionBudgetExceeded struct {
	SessionId string    `json:"session_id"`
	Cost      core.Cost `json:"cost"`
	MaxCost   core.Cost `json:"max_cost"`

	EventType EventType `json:"event_type"`
}

func (e *SessionBudgetExceeded) Type() EventType {
	return e.EventType
}

// SessionClosed 会话优雅关闭完成（Shutdown 收尾：运行已停、状态已落盘）。
type SessionClosed struct {
	SessionId string    `json:"session_id"`
	Timestamp time.Time `json:"timestamp"`

	EventType EventType `json:"event_type"`
}

func (e *SessionClosed) Type() EventType {
	return e.EventType
}

// SessionRunError 一次运行异常结束（重试耗尽 / 永久错误 / persist 之外的运行失败）。
// AgentEnd{FinishReason=error, Error} 是运行级细节；本事件是会话级提示
// （事件消费者如 UI 可统一感知「这次运行没跑完」）。abort（打断/Shutdown）不是失败，不发此事件。
// ErrorKind 与 AgentEnd.ErrorKind 一致（provider.ClassifyError），上层可据此区分
// 瞬时错误重试耗尽（稍后重试）与永久错误（配置/参数问题）。
type SessionRunError struct {
	SessionId string    `json:"session_id"`
	Error     string    `json:"error"`
	ErrorKind string    `json:"error_kind,omitempty"`
	Timestamp time.Time `json:"timestamp"`

	EventType EventType `json:"event_type"`
}

func (e *SessionRunError) Type() EventType {
	return e.EventType
}

// UserInputsConsumed 一批用户输入被服务端消费并推入下一轮上下文（Session.Run 从 inbox takeBatch
// 取出，进入本轮 LLM 请求）。桌面端用它精确感知「哪几条待发送已真正进入对话」——据此把对应消息
// 从「待发送」清单移入对话页（其余仍悬浮等待），而非靠 agent_start 启发式猜测。
// UserContents 携带每条消息的**完整内容块**（text + image，含 mime_type/data URL）：
// 事件日志恢复（快照重放 / 降级 messages）时据此重建用户消息的图片附件，否则图片只存在于
// 客户端暂存队列（重启即失）。与 UserTexts 并存：UserTexts 保持向后兼容（旧客户端），
// UserContents 为权威（缺失 = 旧日志，前端回退 UserTexts）。
type UserInputsConsumed struct {
	SessionId    string           `json:"session_id"`
	UserTexts    []string         `json:"user_texts"`              // 被消费的用户消息文本（顺序 = 入队顺序）
	UserContents [][]core.Content `json:"user_contents,omitempty"` // 每条消息的完整内容块（含图片；与 UserTexts 平行）
	Timestamp    time.Time        `json:"timestamp"`

	EventType EventType `json:"event_type"`
}

func (e *UserInputsConsumed) Type() EventType {
	return e.EventType
}
