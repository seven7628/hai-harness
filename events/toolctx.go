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
	RunId       string
	ParentRunId string       // 发起运行的 RunId（子 agent 的父）
	Depth       int          // 发起运行的深度（根 = 0）
	Handler     EventHandler // 发起运行的事件消费者
	Approver    Approver     // 人工确认（engine 工具轮执行前调用）
	// Hooks 发起运行的 hooks（PreToolUse/PostToolBatch/PreCompact）：子 agent
	// 继承通道（subagent 组装子 RunOptions 时透传，Spec.DisableHookInherit 可关）。
	Hooks *ToolHooks
}

// WithToolContext 把工具执行上下文注入 context；approver nil = 不审批；hooks nil = 无。
// 旧 API 没有单独的当前运行参数，按历史语义把 parentRunId 同时作为 RunId；
// AgentLoop 使用 WithToolContextForRun 传入精确的当前/父运行 ID。
func WithToolContext(ctx context.Context, parentRunId string, depth int, handler EventHandler, approver Approver, hooks *ToolHooks) context.Context {
	return WithToolContextForRun(ctx, parentRunId, parentRunId, depth, handler, approver, hooks)
}

// WithToolContextForRun 注入精确的当前运行与父运行关联。
func WithToolContextForRun(ctx context.Context, runId, parentRunId string, depth int, handler EventHandler, approver Approver, hooks *ToolHooks) context.Context {
	return context.WithValue(ctx, toolCtxKey{}, &ToolContext{
		RunId:       runId,
		ParentRunId: parentRunId,
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
