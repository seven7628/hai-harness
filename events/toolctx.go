package events

import "context"

// toolCtxKey context 值键（避免与其他 value 冲突）。
type toolCtxKey struct{}

// ToolContext 工具执行上下文：由 AgentLoop 在每次工具执行前注入，
// 子 agent 类工具据此关联父事件流（转发事件 / 填写 ParentRunId / 深度限制）。
// Approver 为人工确认接口（HITL）：nil = 不审批（现状）；子运行经 ctx 透传继承。
type ToolContext struct {
	// RunId 是当前正在执行工具的运行 ID；工具事件/审批事件使用它做归属。
	// ParentRunId 仍表示发起当前运行的父运行 ID，不能混用。
	RunId string
	// CallId 本次**工具调用**的 id（不是 run 的）。由引擎在 Call 之前逐次注入
	// （WithCallId），供工具发出「归属于这一行工具卡」的事件 —— 例如
	// events.ToolOutput 的 tail -f 帧。空 = 未知（直接调用工具/测试）。
	CallId      string
	ParentRunId string       // 发起运行的 RunId（子 agent 的父）
	TaskId      string       // 发起运行的后台任务 id（"" = 非后台任务 = 主 Agent 直调）
	Depth       int          // 发起运行的深度（根 = 0）
	Handler     EventHandler // 发起运行的事件消费者
	Approver    Approver     // 人工确认（engine 工具轮执行前调用）
	// Hooks 发起运行的 hooks（PreToolUse/PostToolBatch/PreCompact）：子 agent
	// 继承通道（subagent 组装子 RunOptions 时透传，Spec.DisableHookInherit 可关）。
	Hooks *ToolHooks

	// Todos 本运行自己的待办快照提供者（nil = 未注入）。引擎在 todo_* 工具结束时
	// 调用它，把清单挂到 ToolResponse.Todos —— 属主是**发起本次调用的运行**：
	// 主 run → 会话待办；子 agent run → 它自己那份独立 store。
	// 为什么用闭包而不是直接传 *todo.Store：events 不能 import todo（成环），
	// 引擎也不该知道待办语义；投影成 events.TodoSnapshot 后两边都只依赖事件层。
	Todos func() []TodoSnapshot
}

// WithTodoSnapshot 给**已注入**的工具上下文挂上「本运行待办快照」提供者
// （引擎在 todo_* 工具结束时取用，见 ToolContext.Todos）。未注入工具上下文时无处可挂，
// 原样返回 —— 工具仍可用，只是宿主拿不到 run 级清单。
func WithTodoSnapshot(ctx context.Context, fn func() []TodoSnapshot) context.Context {
	if tc := ToolContextFrom(ctx); tc != nil {
		tc.Todos = fn
	}
	return ctx
}

// WithCallId 给当前工具调用挂上调用 id，返回**新的** ctx。
//
// 为什么是复制而不是原地改：同一 run 内并行的工具调用共享同一份 *ToolContext（run 级
// 注入一次），原地写 CallId 会让两个并发工具的尾巴互相串号 —— 那是典型的「不报错、
// 只是数据错」的竞争。复制成本是一个小结构体（含几个函数指针），可以忽略。
//
// 未注入 ToolContext（工具被直接调用/测试）时原样返回：调用方不必判断。
func WithCallId(ctx context.Context, callID string) context.Context {
	tc := ToolContextFrom(ctx)
	if tc == nil || callID == "" {
		return ctx
	}
	cp := *tc
	cp.CallId = callID
	return context.WithValue(ctx, toolCtxKey{}, &cp)
}

// WithToolContext 把工具执行上下文注入 context；approver nil = 不审批；hooks nil = 无。
// 旧 API 没有单独的当前运行参数，按历史语义把 parentRunId 同时作为 RunId；
// AgentLoop 使用 WithToolContextForRun 传入精确的当前/父运行 ID。
func WithToolContext(ctx context.Context, parentRunId string, depth int, handler EventHandler, approver Approver, hooks *ToolHooks) context.Context {
	return WithToolContextForRun(ctx, parentRunId, parentRunId, depth, handler, approver, hooks)
}

// WithToolContextForRun 注入精确的当前运行与父运行关联（taskId = ""：非后台任务）。
func WithToolContextForRun(ctx context.Context, runId, parentRunId string, depth int, handler EventHandler, approver Approver, hooks *ToolHooks) context.Context {
	return WithToolContextForRunTask(ctx, runId, parentRunId, "", depth, handler, approver, hooks)
}

// WithToolContextForRunTask 与 WithToolContextForRun 同，另带发起运行的后台任务 id
// （RunOptions.TaskId）。子 agent 再派子 agent时，spawn 工具据此判定「结果该投回
// 哪个 loop」：发起方是后台子 agent（TaskId 非空）→ 投它的 inbox；主 Agent 直调
// （TaskId 为空）→ 投主会话。
func WithToolContextForRunTask(ctx context.Context, runId, parentRunId, taskId string, depth int, handler EventHandler, approver Approver, hooks *ToolHooks) context.Context {
	return context.WithValue(ctx, toolCtxKey{}, &ToolContext{
		RunId:       runId,
		ParentRunId: parentRunId,
		TaskId:      taskId,
		Depth:       depth,
		Handler:     handler,
		Approver:    approver,
		Hooks:       hooks,
	})
}

// ToolContextFrom 取出工具执行上下文；未注入（根 agent 直接调工具）返回 nil。
func ToolContextFrom(ctx context.Context) *ToolContext {
	tc, _ := ctx.Value(toolCtxKey{}).(*ToolContext)
	return tc
}
