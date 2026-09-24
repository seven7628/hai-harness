package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/tools"
	"time"
)

// 异步多 agent 的内置工具全家桶（agent_spawn / agent_send / agent_interrupt /
// TaskList）。全部 CanParallel() = true（Registry 自身有锁，相互无共享状态）；
// spawn 工具**不实现 ToolUsageProvider**——后台任务成本单通道：
// TaskEnd 事件 + Registry.TotalUsage() 会话级合成（双路径会双计）。
// 结果通道：自动回传（Registry.OnTaskDone → Session.PushTaskResult），无 agent_wait。

// maxSpawnDepth agent_spawn 的最大运行深度：主 → sub（深度 1）→ sub（深度 2），
// 再深一律拒绝。2026-09-20 加固：此前未设（0 = 不限制）——子 agent 与主 Agent 共享
// 同一个 ToolEngine（"继承主 Agent 全部工具"），agent_spawn 对它同样可见，于是
// 「子 agent 再派子 agent」的层数不受约束；而每个在跑任务都占一个并发槽，递归会
// 演成自我死锁（N 个在跑子 agent 各派一个 spawn → 全部等槽，槽被等待者自己持有）。
// 两层 = 保留一次「主 → 子 → 孙」的委托，既够用又封死不收敛的递归。
const maxSpawnDepth = 2

// NewAgentTools 创建 agent_* 工具集：spawnLoop 为后台子任务的运行时（独立
// SystemPrompt/工具集/压缩配置），bgCtx 提供 Session 级 ctx，reg 为 Session
// 持有的注册表单例，journalPath 为子任务输出 journal 路径解析器（nil = 不落盘；
// bridge 注入 ~/.go-code/sessions/<wsKey>/<sid>/agents/ 约定，docs §3.6）。
// 返回的工具逐个注册进 ToolEngine。
// defReg / loopForDef（可选）：文件化 subagent 支持（2026-08-30 合并 spawn_agent）——
// agent_spawn 未携带 task 时按 name 从 *.md 加载人设；nil = 仅动态 task 模式。
func NewAgentTools(spawnLoop *agents.AgentLoop, bgCtx func() context.Context, reg *Registry, journalPath func(taskID string) (string, error), defReg *DefinitionRegistry, loopForDef func(def *AgentDefinition) *agents.AgentLoop) []tools.Tool {
	spawnTool := NewBackgroundTool(Spec{
		Name: "agent_spawn",
		// 契约：name（必填、短标签≤40字符）= 子 agent 在干什么的简短标识，用于
		// 创建结果 / TaskList / 右侧 Agent 视图展示；task 参数 = 子 agent 收到的唯一指令
		//（独立上下文、看不到主对话），描述强制主 agent 产出完整、自包含的 Prompt+Task。
		// 双模式（2026-08-30）：task 非空 = 动态任务（主 agent 生成完整 Prompt+Task）；
		// task 省略 = 文件化 subagent——从 [可用子智能体] 清单选中 name，自动加载
		// {name}.md 定义（yaml + 正文人设）作为子 agent 人设，无需主 agent 生成 Prompt。
		Description: "Spawn a background subagent to execute a task (returns immediately with task_id, does not block the main task); " +
			"when done, the result is pushed back to the main session automatically (the model sees it next turn and continues). " +
			"Monitor its progress sparingly with TaskOutput (latest journal entry); use agent_send to steer/message it mid-run; agent_interrupt to stop it. " +
			"After the call returns a task_id, the final result arrives later as a message announcing background-task completion — record the task_id immediately, continue with OTHER independent work while it runs, and do not duplicate its scope yourself nor re-invoke agent_spawn for a task already spawned; never retry this call to wait for the result (it will be delivered automatically).\n\n" +
			"TWO MODES (choose one):\n" +
			"(1) DYNAMIC task: provide a self-contained `task` (Prompt+Task) — the subagent has an independent context and cannot see this conversation; include role, goal and scope, key constraints, execution steps, expected output format. It must be a complete, self-contained Prompt+Task, not a one-liner. Use this when `name` is NOT a file-based subagent from the [Available subagents] list.\n" +
			"(2) FILE-BASED subagent: pick a name from the [Available subagents] list in the system prompt — the subagent persona is loaded automatically from its {name}.md definition (yaml + instructions); you do NOT need to generate a Prompt. `task` is OPTIONAL and, if provided, is used as the concrete instruction for this run on top of the persona. If `name` matches a file-based subagent, file-based mode takes priority even when `task` is present.\n\n" +
			"For pure research/analysis of a user question (read-only, no file changes needed), prefer the dedicated subagent_explore tool instead — it is read-only (read_file / grep / bash / glob) and returns a task_id just like this tool, with its analysis pushed back the same way; use agent_spawn for broader tasks that may need the full toolset (including edits/bash).\n\n" +
			"name (REQUIRED): a short label (max 40 chars) describing what this subagent is doing, e.g. \"code review\" or \"migrate billing state machine\"; shown in the result, TaskList and the UI. In file-based mode, this is the subagent definition name from the [Available subagents] list.\n" +
			"task (OPTIONAL in file-based mode; REQUIRED in dynamic mode): the complete, self-contained Prompt+Task. Omit it ONLY when picking a name from [Available subagents] (file-based mode).\n" +
			`Example (dynamic): "You are a code reviewer. Use read_file to review github.com/seven7628/hai-harness/subagent/subagent.go for resource leaks; ` +
			`focus on goroutines and locks; output: a list of risks (file:line:issue), or 'no obvious issues found' if none."`,
		Loop:        spawnLoop,
		Background:  true,
		MaxDepth:    maxSpawnDepth, // 主 → sub → sub；再深拒绝（防递归自锁，见常量注释）
		JournalPath: journalPath,
	}, bgCtx, reg)
	spawnTool.defReg = defReg
	spawnTool.loopForDef = loopForDef
	return []tools.Tool{
		spawnTool,
		&agentSendTool{reg: reg}, // D2 已拍板保留：主 Agent 运行中主动通信 SubAgent 的手段
		&agentInterruptTool{reg: reg},
		&taskListTool{reg: reg},
		&taskOutputTool{reg: reg}, // 子→主过程监控（journal 最新记录 + 权威状态）
	}
}

// ---------- agent_send ----------

type agentSendTool struct{ reg *Registry }

func (t *agentSendTool) Name() string { return "agent_send" }
func (t *agentSendTool) Description() string {
	return "Send a message TO a running background subagent (one-way: main → sub). " +
		"The subagent receives it at its next round boundary; this tool does NOT return the subagent's reply — " +
		"the sub's final result is pushed back to the main session automatically when the task completes. " +
		"(task_id is the identifier returned by agent_spawn)"
}
func (t *agentSendTool) Parameters() any {
	return tools.Obj(map[string]any{
		"task_id": tools.Str("Background task identifier"),
		"message": tools.Str("Message to send"),
	}, "task_id", "message")
}
func (t *agentSendTool) CanParallel() bool { return true }

type agentSendArgs struct {
	TaskID  string `json:"task_id"`
	Message string `json:"message"`
}

func (t *agentSendTool) ValidParams(_ context.Context, _, arguments string) error {
	var a agentSendArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return fmt.Errorf("agent_send: %w", err)
	}
	if a.TaskID == "" || a.Message == "" {
		return fmt.Errorf("agent_send: task_id and message are required")
	}
	return nil
}

func (t *agentSendTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}

func (t *agentSendTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a agentSendArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	if err := t.reg.Send(a.TaskID, a.Message); err != nil {
		if errors.Is(err, ErrTaskFinished) {
			return "", fmt.Errorf("agent_send: task %s already finished — its final result was (or will be) delivered to the main session automatically; use TaskList to check status, do not resend", a.TaskID)
		}
		return "", fmt.Errorf("agent_send: %w", err)
	}
	// TaskMessage 事件（审计：谁在什么时机发了什么）
	if tc := events.ToolContextFrom(ctx); tc != nil && tc.Handler != nil {
		tc.Handler(ctx, &events.TaskMessage{
			RunId:     tc.ParentRunId,
			TaskId:    a.TaskID,
			Message:   a.Message,
			Timestamp: time.Now(),
			EventType: events.TaskMessageType,
		})
	}
	return fmt.Sprintf(`{"task_id":%q,"status":"message delivered"}`, a.TaskID), nil
}

func (t *agentSendTool) AfterCall(context.Context, core.ToolCall) {}

// ---------- agent_interrupt ----------

// agentTaskArgs 仅 task_id 的入参（agent_interrupt 用）。
type agentTaskArgs struct {
	TaskID string `json:"task_id"`
}

type agentInterruptTool struct{ reg *Registry }

func (t *agentInterruptTool) Name() string { return "agent_interrupt" }
func (t *agentInterruptTool) Description() string {
	return "Request interruption of a running background task: a subagent spawned via agent_spawn, or a tool call promoted to background (tooltask-*). A successful call means the request was accepted (status interrupting); the final interrupted status is pushed into the main session automatically. Interruption is not a failure, but the task is not completed."
}
func (t *agentInterruptTool) Parameters() any {
	return tools.Obj(map[string]any{
		"task_id": tools.Str("Background task identifier"),
	}, "task_id")
}
func (t *agentInterruptTool) CanParallel() bool { return true }

func (t *agentInterruptTool) ValidParams(_ context.Context, _, arguments string) error {
	var a agentTaskArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return fmt.Errorf("agent_interrupt: %w", err)
	}
	if a.TaskID == "" {
		return fmt.Errorf("agent_interrupt: task_id is required")
	}
	return nil
}

func (t *agentInterruptTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}

func (t *agentInterruptTool) Call(_ context.Context, _, arguments string) (string, error) {
	var a agentTaskArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	if err := t.reg.Interrupt(a.TaskID); err != nil {
		return "", err
	}
	return fmt.Sprintf(`{"task_id":%q,"status":"interrupting","message":"interrupt request accepted; final interrupted status will be pushed to the main session automatically"}`, a.TaskID), nil
}

func (t *agentInterruptTool) AfterCall(context.Context, core.ToolCall) {}

// ---------- TaskList ----------

// taskListTool 枚举全部后台子 agent（task_id + 状态 + name 短标签）。
// 由 NewAgentTools 注册为工具名 "TaskList"（与 agent_spawn/agent_send/agent_interrupt
// 同一 reg 实例，读 Registry.List——running/done/abandoned 全含）。
type taskListTool struct{ reg *Registry }

func (t *taskListTool) Name() string { return "TaskList" }
func (t *taskListTool) Description() string {
	return "List all background tasks: subagents spawned via agent_spawn AND tool calls promoted to background execution (tooltask-*). Each entry includes task_id, kind (agent/tool), name, tool_name, status, an optional error and output_file (raw JSONL journal of the subagent's progress — readable with read_file/grep, or use TaskOutput for the latest entry; tool tasks have no output_file — their results are delivered to the main session automatically when finished). " +
		"Statuses: running, interrupting (request accepted, awaiting shutdown), completed, interrupted (not a failure but unfinished), failed, abandoned. " +
		"Use it after spawning tasks or promoting a tool to background to check status or recover a task_id for agent_send/agent_interrupt/TaskOutput."
}
func (t *taskListTool) Parameters() any {
	return tools.Obj(map[string]any{})
}
func (t *taskListTool) CanParallel() bool { return true }

func (t *taskListTool) ValidParams(context.Context, string, string) error { return nil }
func (t *taskListTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}

func (t *taskListTool) Call(context.Context, string, string) (string, error) {
	data, err := json.Marshal(t.reg.List())
	if err != nil {
		return "", err
	}
	return string(data), nil
}

func (t *taskListTool) AfterCall(context.Context, core.ToolCall) {}
