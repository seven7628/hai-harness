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
// 由编排型工具（codemode）在 Call 入口调用；tools.ExecuteOne 检测到它便把
// 每次嵌套调用自动汇入（记录 + 用量），实现「一次编排、完整记账」。
func WithNestedRecorder(ctx context.Context, r *core.NestedRecorder) context.Context {
	if r == nil {
		return ctx
	}
	return context.WithValue(ctx, nestedCtxKey{}, r)
}

// NestedRecorderFrom 取嵌套记录器；未注入（无编排场景）返回 nil。
//
// 消费方：引擎 ExecuteOne 汇入记录；编排工具收尾时TakeRecord 挂到 ToolResult。
func NestedRecorderFrom(ctx context.Context) *core.NestedRecorder {
	if ctx == nil {
		return nil
	}
	r, _ := ctx.Value(nestedCtxKey{}).(*core.NestedRecorder)
	return r
}
