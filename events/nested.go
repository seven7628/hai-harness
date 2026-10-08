package events

import (
	"context"

	"github.com/seven7628/hai-harness/core"
)

// nestedCtxKey 嵌套记录器的 context 值键。
//
// 刻意**不复用** toolCtxKey：编排作用域与单次工具执行的生命周期不同 ——
// toolCtx 每次工具执行由 AgentLoop 重新注入（agents/agent_loop.go:2324），
// 而 NestedRecorder 由编排型工具在其 Call 入口注入、跨其内部多次嵌套调用存活。
// 复用同一个键会在每次 execute 时被覆盖，导致内层调用记到外层丢失。
type nestedCtxKey struct{}

// WithNestedRecorder把嵌套记录器注入 ctx。
//
// 注入方（**唯一**）：引擎在每次工具调用前经 EnsureNestedRecorder 建；
// 编排型工具（codemode）只**读**它（NestedRecorderFrom），不要自建 —— 自建的那份
// 挂不到自己的 ToolResult 上：Tool.Call 只返回 (string, error)，工具无法把记录/用量
// 写回引擎；引擎的 finish() 只认**调工具时那个 ctx** 上的 recorder。
// 由 TestOrchestratorRecordsReachOuterResult 钉住（曾经只写测试注入路径，真实
// 编排形态下 NestedCalls 恒为空）。
func WithNestedRecorder(ctx context.Context, r *core.NestedRecorder) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, nestedCtxKey{}, r)
}

// EnsureNestedRecorder 保证 ctx 上有一个 recorder：已有则原样返回（嵌套调用沿用外层
// 那一个，绝不新建 —— 新建会把记录切碎），没有则注入一个新的。
//
// 由引擎在发起工具调用前统一调用（tools/engine.go 的 execute）。开销是一次小结构体
// 分配，换来「任何工具都能成为编排者」这一条不需要改接口的能力（对齐 ImageSink/
// ExecSink 的「每次调用独立放槽」范式）。
func EnsureNestedRecorder(ctx context.Context) context.Context {
	if NestedRecorderFrom(ctx) != nil {
		return ctx
	}
	return WithNestedRecorder(ctx, core.NewNestedRecorder())
}

// NestedRecorderFrom 取嵌套记录器；未注入（无编排场景）返回 nil。
//
// 消费方：引擎 ExecuteOne 汇入记录；编排工具**在 Call 内读它**发起子调用，
// 收尾时由引擎的 finish() 自动取记录/用量挂到本次调用的 ToolResult 上。
func NestedRecorderFrom(ctx context.Context) *core.NestedRecorder {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(nestedCtxKey{}).(*core.NestedRecorder)
	return r
}
