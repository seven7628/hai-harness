package agents

import (
	"context"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"time"
)

// Hooks 拦截的执行点与兜底逻辑。
//
// 类型定义在 events 包（events/hooks.go）——ToolContext.Hooks（events 包）是
// 子 agent 继承通道，类型放 agents 会形成 events→agents 循环依赖。
// RunOptions.Hooks 引用 events.ToolHooks。

// callPreToolUse panic 兜底执行 PreToolUse（E.5：回调 panic → 按 allow 处理，
// 不 kill 运行；警告经事件暴露）。返回 panicked 供调用方发 ToolHookWarning。
func callPreToolUse(h events.PreToolUseHook, ctx context.Context, pc events.PreToolUseContext) (res events.PreToolUseResult, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			res = events.PreToolUseResult{Decision: events.ToolDecisionAllow}
			panicked = true
		}
	}()
	return h(ctx, pc), false
}

// callPostToolBatch panic 兜底执行 PostToolBatch（panic → 不停止，按 false 处理）。
func callPostToolBatch(h events.PostToolBatchHook, ctx context.Context, results []events.ToolRunResult) (stop bool, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			stop = false
			panicked = true
		}
	}()
	return h(ctx, results), false
}

// callPreCompact panic 兜底执行 PreCompact（panic → 空串，不注入）。
func callPreCompact(h events.PreCompactHook, ctx context.Context) (inject string, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			inject = ""
			panicked = true
		}
	}()
	return h(ctx), false
}

// callPostToolUse panic 兜底执行 PostToolUse（panic → 不注入、不替换）。
func callPostToolUse(h events.PostToolUseHook, ctx context.Context, pc events.PostToolUseContext) (res events.PostToolUseResult, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			res = events.PostToolUseResult{}
			panicked = true
		}
	}()
	return h(ctx, pc), false
}

// callStop panic 兜底执行 Stop（panic → 不继续回合，避免"坏 hook 卡住会话"）。
func callStop(h events.StopHook, ctx context.Context, sc events.StopContext) (res events.StopResult, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			res = events.StopResult{}
			panicked = true
		}
	}()
	return h(ctx, sc), false
}

// callSessionStart panic 兜底执行 SessionStart（panic → 空串，不注入）。
func callSessionStart(h events.SessionStartHook, ctx context.Context, sc events.SessionStartContext) (inject string, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			inject = ""
			panicked = true
		}
	}()
	return h(ctx, sc), false
}

// callUserPromptSubmit panic 兜底执行 UserPromptSubmit（panic → 不阻断、不注入）。
func callUserPromptSubmit(h events.UserPromptSubmitHook, ctx context.Context, up events.UserPromptSubmitContext) (res events.UserPromptSubmitResult, panicked bool) {
	defer func() {
		if r := recover(); r != nil {
			res = events.UserPromptSubmitResult{}
			panicked = true
		}
	}()
	return h(ctx, up), false
}

// CallSessionStart 供 Session/宿主调用 SessionStart hook（panic 兜底：按"不注入"处理）。
// 导出而不只是包内使用的原因：SessionStart 由 Session 在首个 Run 前派发，SessionStartHook
// 的生命周期比单次 Run 长（见 session/session.go）。
func CallSessionStart(h events.SessionStartHook, ctx context.Context, sc events.SessionStartContext) (inject string, panicked bool) {
	return callSessionStart(h, ctx, sc)
}

// CallUserPromptSubmit 供 Session/宿主调用 UserPromptSubmit hook（panic 兜底：按"不阻断、不注入"处理）。
func CallUserPromptSubmit(h events.UserPromptSubmitHook, ctx context.Context, up events.UserPromptSubmitContext) (res events.UserPromptSubmitResult, panicked bool) {
	return callUserPromptSubmit(h, ctx, up)
}

// toolHookWarning 发 ToolHookWarning 事件（hook 异常时调用方统一入口）。
func toolHookWarning(ac *AgentContext, name, message string) {
	ac.Handler(ac.ctx, &events.ToolHookWarning{
		RunId:     ac.RunId,
		Name:      name,
		Message:   message,
		Timestamp: time.Now(),
		EventType: events.ToolHookWarningType,
	})
}

// injectPreCompactContext 把 PreCompact 注入串插进压缩输入（E.3）：
// 作为一条 assistant 消息插在「最后一段连续 user 块」之前。
//
// 为什么是 assistant 角色而非 user：splitMessages（llm_compressor.go）按
// 「下标 >= 最后连续 user 块起点」判定 latestUser 并原样保留（不参与压缩）——
// user 角色消息插在块前会被并入块（下标落入范围）原样保留，违背「进压缩输入、
// 出压缩输出」；assistant 角色不在 user 块内 → 归 history 段 → 进压缩输入、
// 压缩输出不含。注入内容语义为「保命上下文」（非对话轮），角色不影响渲染
// （renderCompressInput 按 [role] text 渲染）。
//
// 插入位置 = 块前紧邻位（start-1；start=0 时插开头；尾部无 user 块时 start=len，
// 插最后一条之前）——start-1 必然非 user（start 是从尾往前首个非 user 之后的位），
// 插入后块起点不变、注入串在块外。
//
// 关键约束：注入串不能占 index 0 当首条是 system —— splitMessages 只保留
// i==0 的 system 消息（框架提示词），注入串（assistant）占首位会把真正的
// system 挤进压缩输入、被摘要吞掉（系统提示词丢失）。故 pos 修正到 1
// （system 之后、user 块之前；注入串仍在 history 段，进压缩输入）。
func injectPreCompactContext(msgs []core.Message, inject string) []core.Message {
	start := len(msgs)
	for start > 0 && msgs[start-1].Role == core.User {
		start--
	}
	pos := start - 1
	if pos < 0 {
		pos = 0
	}
	if pos == 0 && len(msgs) > 0 && msgs[0].Role == core.System {
		pos = 1 // 保 system 首条不变量（splitMessages 只保留 i==0 的 system）
	}
	out := make([]core.Message, 0, len(msgs)+1)
	out = append(out, msgs[:pos]...)
	out = append(out, core.NewAssistantMessage(
		[]core.Content{{Type: "text", Content: inject}}, "", nil))
	out = append(out, msgs[pos:]...)
	return out
}
