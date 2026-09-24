package hooks

import (
	"context"
	"encoding/json"

	"github.com/seven7628/hai-harness/events"
)

// Dispatcher 是产品与内核之间的唯一入口：把外部命令 hook 适配成内核的
// events.ToolHooks（七个钩子全部就位），宿主只需一行：
//
//	session.WithHooks(dispatcher.ToolHooks(workspace, sessionID))
//
// 七个钩子的派发点（内核侧）：
//   - PreToolUse        agents/agent_loop.go（模式过滤后、审批前；可 Block / 改写入参）
//   - PostToolUse       agents/agent_loop.go runTools 尾部（逐工具，注入进结果文本）
//   - PostToolBatch     agents/agent_loop.go（批级回调）
//   - PreCompact        agents/agent_loop.go（注入压缩输入）
//   - Stop              agents/agent_loop.go（自然结束门控，带 8 次上限）
//   - SessionStart      session/session.go（每会话一次 → system 层注入）
//   - UserPromptSubmit  session/session.go Ask（注入本轮 user 消息 / 可拒绝输入）
type Dispatcher struct {
	runner *Runner
	warn   func(name, message string)
}

// NewDispatcher 构造派发器；warn 由产品注入（建议转成 events.ToolHookWarning）。
func NewDispatcher(cfg *Config, trust *TrustStore, warn func(name, message string)) *Dispatcher {
	d := &Dispatcher{warn: warn}
	d.runner = NewRunner(cfg, trust, d.emitRun)
	return d
}

// Runner 暴露只读的执行器（测试与干跑命令用）。
func (d *Dispatcher) Runner() *Runner { return d.runner }

// ToolHooks 把外部 hook 适配成内核的 events.ToolHooks（七个钩子）。
//
// cwd/sessionID 在构造时绑定：cwd 作为 hook 子进程的工作目录与 GOCODE_PROJECT_DIR，
// sessionID 用于 payload.session_id（会话内去重/统计）。
func (d *Dispatcher) ToolHooks(cwd, sessionID string) *events.ToolHooks {
	return &events.ToolHooks{
		PreToolUse:       d.preToolUse(cwd, sessionID),
		PostToolUse:      d.postToolUse(cwd, sessionID),
		PostToolBatch:    d.postToolBatch(cwd, sessionID),
		PreCompact:       d.preCompact(cwd, sessionID),
		Stop:             d.stop(cwd, sessionID),
		SessionStart:     d.sessionStart(cwd, sessionID),
		UserPromptSubmit: d.userPromptSubmit(cwd, sessionID),
	}
}

// ── 七个钩子的实现（ToolHooks 与 On* 方法共用同一份逻辑）──────────────────

func (d *Dispatcher) preToolUse(cwd, sessionID string) events.PreToolUseHook {
	return func(ctx context.Context, pc events.PreToolUseContext) events.PreToolUseResult {
		out := d.runner.Dispatch(ctx, EventPreToolUse, Payload{
			SessionID:     sessionID,
			Cwd:           cwd,
			HookEventName: string(EventPreToolUse),
			ToolName:      pc.Call.Name,
			ToolInput:     json.RawMessage(pc.Arguments),
		}, MatchValue{ToolName: pc.Call.Name})
		d.report(EventPreToolUse, out)
		res := events.PreToolUseResult{Decision: out.Decision.toToolDecision(), Reason: out.Reason}
		// 改写只在非阻断时生效（阻断时引擎根本不会执行该调用）
		if len(out.UpdatedInput) > 0 && out.Decision != DecisionBlock {
			res.UpdatedArguments = string(out.UpdatedInput)
		}
		return res
	}
}

func (d *Dispatcher) postToolUse(cwd, sessionID string) events.PostToolUseHook {
	return func(ctx context.Context, pc events.PostToolUseContext) events.PostToolUseResult {
		out := d.runner.Dispatch(ctx, EventPostToolUse, Payload{
			SessionID:     sessionID,
			Cwd:           cwd,
			HookEventName: string(EventPostToolUse),
			ToolName:      pc.Call.Name,
			ToolInput:     json.RawMessage(pc.Arguments),
			ToolResponse:  &ToolResponse{IsError: pc.IsError, Text: pc.Result},
		}, MatchValue{ToolName: pc.Call.Name})
		d.report(EventPostToolUse, out)
		text, _ := BuildInject(out.Contexts, maxEventInjectBytes)
		return events.PostToolUseResult{
			AdditionalContext: text,
			ReplaceResult:     out.ToolOutput,
		}
	}
}

func (d *Dispatcher) postToolBatch(cwd, sessionID string) events.PostToolBatchHook {
	return func(ctx context.Context, _ []events.ToolRunResult) bool {
		out := d.runner.Dispatch(ctx, EventPostToolBatch, Payload{
			SessionID: sessionID, Cwd: cwd, HookEventName: string(EventPostToolBatch),
		}, MatchValue{})
		d.report(EventPostToolBatch, out)
		// 外部 hook 不允许终止整个运行（终止只走 abort 语义）：返回值恒 false。
		// 需要"别结束"的场景用 Stop 钩子（有上限护栏）。
		return false
	}
}

func (d *Dispatcher) preCompact(cwd, sessionID string) events.PreCompactHook {
	return func(ctx context.Context) string {
		out := d.runner.Dispatch(ctx, EventPreCompact, Payload{
			SessionID: sessionID, Cwd: cwd, HookEventName: string(EventPreCompact),
		}, MatchValue{})
		d.report(EventPreCompact, out)
		text, _ := BuildInject(out.Contexts, maxEventInjectBytes)
		return text
	}
}

func (d *Dispatcher) stop(cwd, sessionID string) events.StopHook {
	return func(ctx context.Context, sc events.StopContext) events.StopResult {
		out := d.runner.Dispatch(ctx, EventStop, Payload{
			SessionID: sessionID, Cwd: cwd, HookEventName: string(EventStop),
			TurnID: sc.RunId,
		}, MatchValue{})
		d.report(EventStop, out)
		if out.Decision == DecisionBlock {
			// 上限由内核侧计数（AgentContext.stopHookBlocks，8 次）—— hook 无需自知。
			return events.StopResult{Continue: true, Reason: firstNonEmpty(out.Reason, "continue requested by hook")}
		}
		return events.StopResult{}
	}
}

func (d *Dispatcher) sessionStart(cwd, sessionID string) events.SessionStartHook {
	return func(ctx context.Context, sc events.SessionStartContext) string {
		out := d.runner.Dispatch(ctx, EventSessionStart, Payload{
			SessionID: sessionID, Cwd: cwd, HookEventName: string(EventSessionStart),
			Source: sc.Source,
		}, MatchValue{Source: sc.Source})
		d.report(EventSessionStart, out)
		text, _ := BuildInject(out.Contexts, maxEventInjectBytes)
		return text
	}
}

func (d *Dispatcher) userPromptSubmit(cwd, sessionID string) events.UserPromptSubmitHook {
	return func(ctx context.Context, up events.UserPromptSubmitContext) events.UserPromptSubmitResult {
		out := d.runner.Dispatch(ctx, EventUserPromptSubmit, Payload{
			SessionID: sessionID, Cwd: cwd, HookEventName: string(EventUserPromptSubmit),
			Prompt: up.Prompt,
		}, MatchValue{Prompt: up.Prompt})
		d.report(EventUserPromptSubmit, out)
		if out.Decision == DecisionBlock {
			return events.UserPromptSubmitResult{Block: true, Reason: firstNonEmpty(out.Reason, "blocked by hook")}
		}
		text, _ := BuildInject(out.Contexts, maxEventInjectBytes)
		return events.UserPromptSubmitResult{AdditionalContext: text}
	}
}

// ── 便捷方法（供不装配 events.ToolHooks 的宿主直接调用）──────────────────

// OnSessionStart 直接派发 SessionStart（返回 system 层注入文本）。
func (d *Dispatcher) OnSessionStart(ctx context.Context, p Payload, source string) string {
	return d.sessionStart(p.Cwd, p.SessionID)(ctx, events.SessionStartContext{Source: source})
}

// OnUserPromptSubmit 直接派发 UserPromptSubmit。
func (d *Dispatcher) OnUserPromptSubmit(ctx context.Context, p Payload) (inject string, blocked bool, reason string) {
	res := d.userPromptSubmit(p.Cwd, p.SessionID)(ctx, events.UserPromptSubmitContext{Prompt: p.Prompt})
	return res.AdditionalContext, res.Block, res.Reason
}

// OnPostToolUse 直接派发 PostToolUse。
func (d *Dispatcher) OnPostToolUse(ctx context.Context, p Payload) (inject string, replaceOutput string) {
	pc := events.PostToolUseContext{Arguments: string(p.ToolInput)}
	if p.ToolResponse != nil {
		pc.Result, pc.IsError = p.ToolResponse.Text, p.ToolResponse.IsError
	}
	res := d.postToolUse(p.Cwd, p.SessionID)(ctx, pc)
	return res.AdditionalContext, res.ReplaceResult
}

// OnStop 直接派发 Stop。
func (d *Dispatcher) OnStop(ctx context.Context, p Payload) (continueTurn bool, reason string) {
	res := d.stop(p.Cwd, p.SessionID)(ctx, events.StopContext{RunId: p.TurnID})
	return res.Continue, res.Reason
}

// ── 审计 ────────────────────────────────────────────────────────────────

// emitRun 每次 handler 执行都回调一次；只上报"非正常结束"的跑法（正常跑法走日志）。
func (d *Dispatcher) emitRun(run HookRun) {
	if d.warn == nil {
		return
	}
	switch run.Status {
	case "ok", "ok_no_output":
		return
	}
	d.warn("hook:"+run.Name, run.Format())
}

// report 把聚合告警升级为内核可见的 ToolHookWarning（经产品注入的 warn）。
func (d *Dispatcher) report(ev Event, out Outcome) {
	if d.warn == nil {
		return
	}
	for _, w := range out.Warnings {
		d.warn(string(ev), w)
	}
}
