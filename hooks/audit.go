package hooks

import (
	"fmt"
	"time"
)

// HookRun 一次 handler 执行的记录（规范 §6 审计）。
// 本期走结构化日志 + events.ToolHookWarning（内核已有类型）；events 目前不支持自定义事件类型，
// 故不新增事件，将来若要进事件流需先在 events 包加类型。
type HookRun struct {
	Event         Event
	Name          string
	Command       string
	Scope         string
	Status        string // ok | ok_no_output | blocked | blocked_json | timeout | error | invalid_output_* | mismatched_event
	ExitCode      int
	TimedOut      bool
	DurationMs    int64
	InjectedBytes int
	FailureMode   string
	// At 本次执行开始时刻。日志里没有时间就无法回答"什么时候跑的"。
	At time.Time `json:"at,omitempty"`
	// SideEffect 这次执行对模型可见内容做了什么：context（注入了上下文）/
	// input（改了工具入参）/ 空（只跑了一遍，没有改变任何东西）。
	// 它决定"正常结束要不要报给用户"——见 adapter.emitRun。
	SideEffect string `json:"side_effect,omitempty"`
	// Session 触发它的会话；排查"某个会话为什么没跑 hook"时靠它定位。
	Session string `json:"session,omitempty"`
}

// Format 人类可读的一行摘要（日志用）。
func (r HookRun) Format() string {
	status := r.Status
	if r.TimedOut {
		status = "timeout"
	}
	name := r.Name
	if name == "" {
		name = firstLine(r.Command)
	}
	return fmt.Sprintf("hook %s [%s] %s exit=%d %dms injected=%dB",
		name, r.Event, status, r.ExitCode, r.DurationMs, r.InjectedBytes)
}
