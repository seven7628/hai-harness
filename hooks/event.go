// Package hooks 实现 go-code 的统一外部命令 hook 能力：
// 配置驱动的生命周期事件 → 外部进程 → stdin/stdout JSON → 注入/阻断/改写。
//
// 设计规范：docs/graft-integration/05-hooks-能力设计规范.md
// 参考实现：docs/graft-integration/06-hooks-参考实现（Go）.md
//
// 本包只依赖 events/core 类型，不依赖 agents/session/tools（避免 import 环）；
// 目前**尚未接线**到 desktop/bridge：接线点见规范 §10，由 session.WithHooks(Dispatcher.ToolHooks(...))
// 与四个 On* 方法在锚点处调用即可生效。
package hooks

import (
	"encoding/json"
	"regexp"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/events"
)

// Event 生命周期事件名。与 Claude Code / Codex 同名，便于第三方 wiring 零改动复用。
type Event string

const (
	EventSessionStart     Event = "SessionStart"
	EventUserPromptSubmit Event = "UserPromptSubmit"
	EventPreToolUse       Event = "PreToolUse"
	EventPostToolUse      Event = "PostToolUse"
	EventStop             Event = "Stop"
	EventPreCompact       Event = "PreCompact"    // 复用内核已有回调
	EventPostToolBatch    Event = "PostToolBatch" // 复用内核已有回调
)

// MatcherField 说明该事件的 matcher 匹配什么（写进文档，避免"写了 matcher 不生效"的坑）。
type MatcherField int

const (
	MatchNone MatcherField = iota
	MatchToolName
	MatchPrompt
	MatchSource
)

// Anchor 一个事件的挂载属性。新增事件 = 加一行 + 在新位置调用 Dispatcher。
type Anchor struct {
	Event          Event
	MatcherField   MatcherField
	Sync           bool // 结果参与本轮（同步等待）
	CanBlock       bool
	CanInject      bool
	CanRewrite     bool
	DefaultTimeout time.Duration
}

// Anchors 锚点表；DefaultTimeout 见规范 §3.3。
var Anchors = map[Event]Anchor{
	EventSessionStart:     {EventSessionStart, MatchSource, true, false, true, false, 10 * time.Second},
	EventUserPromptSubmit: {EventUserPromptSubmit, MatchPrompt, true, true, true, false, 15 * time.Second},
	EventPreToolUse:       {EventPreToolUse, MatchToolName, true, true, true, true, 5 * time.Second},
	EventPostToolUse:      {EventPostToolUse, MatchToolName, true, false, true, true, 10 * time.Second},
	EventStop:             {EventStop, MatchNone, true, true, false, false, 10 * time.Second},
	EventPreCompact:       {EventPreCompact, MatchNone, true, false, true, false, 10 * time.Second},
	EventPostToolBatch:    {EventPostToolBatch, MatchNone, true, false, true, false, 10 * time.Second},
}

// Anchor 返回事件的锚点属性。
func (e Event) Anchor() (Anchor, bool) { a, ok := Anchors[e]; return a, ok }

// String 事件名字符串。
func (e Event) String() string { return string(e) }

// Payload 是交给 hook 进程的 stdin（一行 JSON）。字段名与 Claude Code 一致。
type Payload struct {
	SessionID      string          `json:"session_id"`
	TranscriptPath string          `json:"transcript_path,omitempty"`
	Cwd            string          `json:"cwd"`
	HookEventName  string          `json:"hook_event_name"`
	ToolName       string          `json:"tool_name,omitempty"`
	ToolInput      json.RawMessage `json:"tool_input,omitempty"`
	ToolResponse   *ToolResponse   `json:"tool_response,omitempty"`
	Prompt         string          `json:"prompt,omitempty"`
	Source         string          `json:"source,omitempty"`
	PermissionMode string          `json:"permission_mode,omitempty"`
	TurnID         string          `json:"turn_id,omitempty"`
	AgentID        string          `json:"agent_id,omitempty"`
	AgentType      string          `json:"agent_type,omitempty"`
}

// ToolResponse 工具结果的精简表示（够 hook 判断成败与拿到文件路径）。
type ToolResponse struct {
	IsError bool           `json:"is_error"`
	Text    string         `json:"text,omitempty"`
	Diff    string         `json:"diff,omitempty"`
	Exec    *ExecInfo      `json:"exec,omitempty"`
	Extra   map[string]any `json:"extra,omitempty"`
}

// ExecInfo 命令执行结果摘要。
type ExecInfo struct {
	ExitCode int  `json:"exit_code"`
	TimedOut bool `json:"timed_out,omitempty"`
}

// Result 是 hook 进程可选的 stdout JSON。
type Result struct {
	Continue           *bool           `json:"continue,omitempty"`
	Decision           string          `json:"decision,omitempty"` // allow|block|deny
	Reason             string          `json:"reason,omitempty"`
	SystemMessage      string          `json:"systemMessage,omitempty"`
	SuppressOutput     bool            `json:"suppressOutput,omitempty"`
	HookSpecificOutput *SpecificOutput `json:"hookSpecificOutput,omitempty"`
}

// SpecificOutput 与 Claude Code / Codex 同构的嵌套字段。
type SpecificOutput struct {
	HookEventName            string          `json:"hookEventName"`
	AdditionalContext        string          `json:"additionalContext,omitempty"`
	PermissionDecision       string          `json:"permissionDecision,omitempty"` // allow|deny|ask
	PermissionDecisionReason string          `json:"permissionDecisionReason,omitempty"`
	UpdatedInput             json.RawMessage `json:"updatedInput,omitempty"`
}

// Decision 聚合后的决策（顺序：Block > Ask > Allow > None）。
type Decision int

const (
	DecisionNone Decision = iota
	DecisionAllow
	DecisionAsk
	DecisionBlock
)

// MatchValue 是 matcher 的输入：按事件的 MatcherField 取用。
type MatchValue struct {
	ToolName string
	Prompt   string
	Source   string
}

func (mv MatchValue) forField(f MatcherField) string {
	switch f {
	case MatchToolName:
		return mv.ToolName
	case MatchPrompt:
		return mv.Prompt
	case MatchSource:
		return mv.Source
	default:
		return ""
	}
}

var alnum = regexp.MustCompile(`^[A-Za-z0-9_\-|,]+$`)

// matches 实现三态 matcher（规范 §3.4）：
//   - 空 / "*" / ".*"     → 全匹配
//   - 纯 [A-Za-z0-9_-|,]+ → 精确候选列表（| 或 , 分隔）
//   - 其它                 → 未锚定正则（编译失败由 validate 按"配置错误"处理）
func matches(pattern string, mv MatchValue, field MatcherField, re *regexp.Regexp) bool {
	p := strings.TrimSpace(pattern)
	if p == "" || p == "*" || p == ".*" {
		return true
	}
	val := mv.forField(field)
	if alnum.MatchString(p) {
		for _, cand := range strings.FieldsFunc(p, func(r rune) bool { return r == '|' || r == ',' }) {
			if strings.TrimSpace(cand) == val {
				return true
			}
		}
		return false
	}
	if re == nil {
		return false
	}
	return re.MatchString(val)
}

// toToolDecision 本包的 Decision → 内核的 ToolDecision。
func (d Decision) toToolDecision() events.ToolDecision {
	if d == DecisionBlock {
		return events.ToolDecisionBlock
	}
	return events.ToolDecisionAllow
}
