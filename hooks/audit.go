package hooks

import "fmt"

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
