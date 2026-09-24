package events

import "time"

// CommandResult 命令执行结果事件（命令框架）：Session.Command 纯 enqueue 立即返回，
// 命令在 AgentLoop 注入段异步执行，结果经此事件回传调用者（产品 UI）。
// 发事件者 = AgentLoop（经 Run 的 handler）；compact 命令不发本事件
// （结果经 CompressStart/End 事件感知）。
type CommandResult struct {
	RunId     string    `json:"run_id,omitempty"`
	Name      string    `json:"name"`
	Result    string    `json:"result,omitempty"` // 命令产出文本（如 skills 清单）
	Error     string    `json:"error,omitempty"`  // 执行失败信息
	Timestamp time.Time `json:"timestamp"`

	EventType EventType `json:"event_type"`
}

func (c *CommandResult) Type() EventType { return c.EventType }
