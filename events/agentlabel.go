package events

import "context"

// 子智能体身份标签（`agent_task_id`）—— 2026-11-19。
//
// ## 为什么需要这个字段
//
// 事件流里**本来就有**子智能体的概念，但它散在三个语义完全不同的字段上：
//
//	| 事件类型 | 字段 | 实际语义 |
//	|---|---|---|
//	| `AgentStart`/`AgentEnd`/`TaskStarted`/`TaskEnd`/`TaskMessage`/`AgentUserMessage` | `task_id` | **子智能体 id**（`task-xxxxxxxx`） |
//	| `ToolStart`/`ToolResponse` | `task_id` | **后台工具任务 id**（`tooltask-xxxxxxxx`）—— 完全不同的一件事 |
//	| `LLMStart`/`LLMEnd`/`LLMError`/`ContentChunk`/`ReasoningChunk`/`Compress*` 等 | —— | **根本没有这个字段** |
//
// ⇒ 后果（实测 `s-1792660803279727000-3`，子智能体 run 的全部事件）：
//
//	tool_run_end   134,387 行  带 task_id 的：**0.0%**
//	llm_end       112,206 行  带 task_id 的：**0.0%**
//	llm_start     112,432 行  带 task_id 的：**0.0%**
//	tool_run_start 134,409 行 带 task_id 的：100%，但全是**互不相同**的 `tooltask-*`
//
// 即：**子智能体的模型输出与工具结果，一行都没有会话归属标记**。
// 而 `tool_run_start.task_id` 又是**挪用**的（工具任务 ≠ 子智能体），
// ⇒ **拿 `task_id` 过滤子智能体事件会得到错误答案**（主会话的工具调用也会被算进去）。
//
// 归属链今天唯一可靠的形态是 **run_id 树**（`agent_start.parent_run_id` 逐级上溯到根），
// 见 `desktop/bridge/trace.go` 的 `buildTraces` —— 但那要求消费方**先扫到 agent_start**
// 才能归组，无法对单条事件直接判定。
//
// ## 本字段的语义（刻意与 `task_id` 分开）
//
//	`agent_task_id` = **子智能体身份**（后台任务 id `task-xxxxxxxx`）
//	                空 = 主会话自己的事件
//
// 不用 `task_id` 的理由：那个名字在 events 里已经是三义（见上表），
// 改它的语义会破坏前端（`ToolStart.task_id` 被前端用来关联后台工具任务）。
// ⇒ 用**语义单一的新字段**，只加在**原本没有身份字段**的事件类型上
//   （Agent* 系列已有 `task_id`，不重复加）。
//
// ## 覆盖面（不变式）
//
//	**凡是带 `run_id` 的事件类型，都带 `agent_task_id`。**
//	（run-scoped ⇒ 按运行归属；task-scoped 的 `TaskPromoted`/`TaskResultDelivered`
//	  已有 `task_id`，不重复。）
//
// 由 `agentlabel_coverage_test.go` 用 AST 扫本包源码钉住 —— 新增事件类型若忘了
// 加字段或忘了进盖章函数，那条测试会红。
//
// ## 兼容性（为什么这不影响 checkpoint / 恢复 / 前端）
//
//  1. `omitempty` ⇒ **主会话与同步子运行的事件 JSON 逐字节不变**
//     ⇒ reducer / checkpoint / 会话恢复 / 前端解析 **零影响**。
//  2. 只有**后台子智能体**的事件多一个字段 ⇒ 前端多一个未知字段；
//     前端事件类型是 `AnyEvent`（`[k: string]: unknown`，未知字段忽略）⇒ 零行为变化。
//  3. 字段只增不改已有字段的语义 ⇒ 老事件文件（无此字段）继续可读，
//     legacy 迁移重放路径也照常工作（字段缺失 = 空值 = 主会话语义）。

// StampAgentTaskID 给事件盖上子智能体身份标记。
//
// 只改**支持该字段**的事件类型；其余（Agent* 系列已有 `task_id`）原样返回。
// id 为空 ⇒ 不动（保持「主会话事件逐字节不变」这条不变量）。
//
// 覆盖面不变式见 agentlabel_coverage_test.go。
func StampAgentTaskID(ev Event, id string) {
	if ev == nil || id == "" {
		return
	}
	switch e := ev.(type) {
	// —— run-scoped：模型与流式增量 ——
	case *LLMStart:
		e.AgentTaskId = id
	case *LLMEnd:
		e.AgentTaskId = id
	case *LLMError:
		e.AgentTaskId = id
	case *ContentChunk:
		e.AgentTaskId = id
	case *ReasoningChunk:
		e.AgentTaskId = id
	// —— run-scoped：工具 ——
	case *ToolStart:
		e.AgentTaskId = id
	case *ToolResponse:
		e.AgentTaskId = id
	case *ToolOutput:
		e.AgentTaskId = id
	case *ToolApprovalRequested:
		e.AgentTaskId = id
	case *ToolHookWarning:
		e.AgentTaskId = id
	// —— run-scoped：压缩与提示 ——
	case *CompressStart:
		e.AgentTaskId = id
	case *CompressEnd:
		e.AgentTaskId = id
	case *GoalAlignmentReminder:
		e.AgentTaskId = id
	case *RunSilentEnd:
		e.AgentTaskId = id
	case *AskUserQuestion:
		e.AgentTaskId = id
	case *CommandResult:
		e.AgentTaskId = id
	case *RefsLoaded:
		e.AgentTaskId = id
	case *MemoryActivity:
		e.AgentTaskId = id
	}
}

// ToolAgentTaskID 从 ctx 的**工具上下文**取本次运行的子智能体身份
// （= RunOptions.TaskId，tools 引擎经 ToolContext 拿到）；空 = 主会话 / 无工具上下文。
//
// 工具类事件（ToolStart/ToolResponse/ToolOutput/ToolApprovalRequested）由**工具引擎**
// 发射（不在 agent loop 里），但引擎的 ctx 上带着工具上下文 ⇒ 身份可取。
func ToolAgentTaskID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	tc := ToolContextFrom(ctx)
	if tc == nil {
		return ""
	}
	return tc.TaskId
}

// WithAgentTaskID 按 ctx 里的工具上下文给事件盖上子智能体身份，返回**原事件**。
//
// 为什么是「返回原事件」而不是返回 error/void：让发射点能写成
//
//	handler(ctx, events.WithAgentTaskID(ctx, &events.ToolStart{...}))
//
// 这样一个表达式里完成盖章，不必把字面量拆成两步（拆开会动更多行）。
func WithAgentTaskID(ctx context.Context, ev Event) Event {
	StampAgentTaskID(ev, ToolAgentTaskID(ctx))
	return ev
}
