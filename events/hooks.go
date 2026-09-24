package events

import (
	"context"
	"github.com/seven7628/hai-harness/core"
	"time"
)

// 工具/压缩钩子（Hooks 拦截）：给产品三个内核拦截/注入点。
//
// 类型定义在 events 包而非 agents 包的原因：ToolContext.Hooks 是子 agent
// hooks 继承通道，ToolContext 在 events 包；而 agents 依赖 events（依赖方向
// 单向）。若类型定义在 agents，ToolContext 引用它们会形成 events→agents 循环
// 依赖（tools→events 已存在，events 不能反向）。故类型下沉到 events，
// agents 的 RunOptions 直接引用（PreToolUse events.PreToolUseHook）。
//
// 与 PLAN 差异：PreToolUseContext 不含 tools.Tool 字段（同上循环依赖原因）——
// hook 按 Call.Name 识别工具即可，需要工具实例的场景产品可在回调内自建查表。

// ToolDecision PreToolUse 的决策：放行 / 拒绝执行。
type ToolDecision int

const (
	ToolDecisionAllow ToolDecision = iota
	ToolDecisionBlock              // 拒绝执行，IsError 结果回传 LLM（与模式 block 同构）
)

// PreToolUseContext PreToolUse 回调入参：原始调用与原始参数 JSON。
type PreToolUseContext struct {
	Call      core.ToolCall
	Arguments string // 原始参数 JSON（= Call.Arguments，显式冗余便于 hook 区分改写前后）
}

// PreToolUseResult PreToolUse 回调返回值：
//   - Allow + UpdatedArguments 非空 → 用改写后的参数执行（历史 ToolCall 保留原参，
//     审计经 ToolResponse.Arguments 查实际执行参数）；
//   - Block → 不执行，Reason 作为 IsError 结果回传 LLM。
type PreToolUseResult struct {
	Decision         ToolDecision
	UpdatedArguments string // 空 = 不改写
	Reason           string // block 时进 LLM 可见结果
}

// ToolRunResult PostToolBatch 入参：批内每个工具的收敛结果。
type ToolRunResult struct {
	Name    string
	CallID  string
	IsError bool
	Result  string
}

// PreToolUseHook 工具执行前回调（模式过滤后、审批前执行；串行、顺序 = 调用顺序）。
type PreToolUseHook func(ctx context.Context, pc PreToolUseContext) PreToolUseResult

// PostToolUseContext PostToolUse 回调入参：已执行的调用 + 原始参数 + 结果文本。
type PostToolUseContext struct {
	Call      core.ToolCall
	Arguments string // 实际执行参数 JSON（PreToolUse 改写后）
	Result    string // 工具结果文本
	IsError   bool
}

// PostToolUseResult PostToolUse 回调返回值：
//   - AdditionalContext 非空 → 追加到该工具结果文本之后（模型下一轮可见）；
//   - ReplaceResult 非空 → **替换**结果文本（谨慎使用，用于脱敏/裁剪等场景）。
type PostToolUseResult struct {
	AdditionalContext string
	ReplaceResult     string
}

// PostToolUseHook 每个工具结果落定后回调（逐工具粒度，非批量）。
// Graft 的 blast radius 就挂在这里：编辑类工具执行后立刻回注"谁依赖这个文件"。
type PostToolUseHook func(ctx context.Context, pc PostToolUseContext) PostToolUseResult

// StopContext Stop 回调入参（回合自然结束的门控点）。
type StopContext struct {
	RunId         string
	LastAssistant string // 本轮最终文本（可能为空）
	Iteration     int64
}

// StopResult Stop 回调返回值：Continue=true → 不要结束本轮（调用方计数并设上限，防死循环）。
type StopResult struct {
	Continue bool
	Reason   string // 作为提醒文本注入下一轮
}

// StopHook 回合结束回调（仅自然结束路径触发；abort/出错路径不触发）。
type StopHook func(ctx context.Context, sc StopContext) StopResult

// SessionStartContext 会话开始回调入参。
type SessionStartContext struct {
	SessionId string
	Cwd       string
	Source    string // startup | resume | clear | compact
}

// SessionStartHook 会话开始回调；返回注入 system 层的文本（空串 = 不注入）。
type SessionStartHook func(ctx context.Context, sc SessionStartContext) string

// UserPromptSubmitContext 用户提交提示词回调入参。
type UserPromptSubmitContext struct {
	SessionId string
	Cwd       string
	Prompt    string
}

// UserPromptSubmitResult 用户提交提示词回调返回值：
//   - AdditionalContext → 追加到本轮 user 消息（每轮都是"新鲜全价输入"，故应只放定位符）；
//   - Block=true → 拒绝该输入（Reason 回传给用户/模型，本轮不运行）。
type UserPromptSubmitResult struct {
	AdditionalContext string
	Block             bool
	Reason            string
}

// UserPromptSubmitHook 用户提交提示词回调（每轮用户输入触发一次）。
type UserPromptSubmitHook func(ctx context.Context, up UserPromptSubmitContext) UserPromptSubmitResult

// PostToolBatchHook 工具批执行后回调；返回 true = 策略性终止本轮运行
// （AgentEnd finishReason=stop，非失败——与 ac.Abort 的 abort 语义区分）。
type PostToolBatchHook func(ctx context.Context, results []ToolRunResult) bool

// PreCompactHook 压缩前回调：返回的字符串注入压缩输入（摘要 LLM 可见、压缩输出不含），
// 用于保留关键上下文防丢失（CC/Cline/OpenClaw 三家共识点）。
type PreCompactHook func(ctx context.Context) string

// ToolHooks 三个钩子的聚合（ToolContext.Hooks 字段 + 子 agent 继承通道）。
//
// 扩展（2026-09-20，hooks 能力）：新增 PostToolUse / Stop / SessionStart / UserPromptSubmit
// 四个钩子，供"外部命令 hook"（hooks 包）与其它产品侧拦截使用。原有三个字段语义不变，
// 新增字段为 nil 时行为与扩展前完全一致（向后兼容）。
type ToolHooks struct {
	PreToolUse    PreToolUseHook
	PostToolBatch PostToolBatchHook
	PreCompact    PreCompactHook

	// 以下四个为 hooks 能力扩展（nil = 不启用）
	PostToolUse      PostToolUseHook
	Stop             StopHook
	SessionStart     SessionStartHook
	UserPromptSubmit UserPromptSubmitHook
}

// ToolHookWarning hook 异常警告事件（回调 panic、改写参数非法 JSON 等——
// 按安全默认处理不 kill 运行，异常经事件暴露供审计）。
type ToolHookWarning struct {
	RunId     string    `json:"run_id,omitempty"`
	Name      string    `json:"name,omitempty"` // 上下文（工具名 / hook 名）
	Message   string    `json:"message"`
	Timestamp time.Time `json:"timestamp"`

	EventType EventType `json:"event_type"`
}

func (w *ToolHookWarning) Type() EventType { return w.EventType }
