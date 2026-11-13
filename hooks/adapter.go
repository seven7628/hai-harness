package hooks

import (
	"context"
	"encoding/json"

	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/owner"
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
//   - Stop              agents/agent_loop.go（收尾门控，截断判定之前，带 8 次上限）
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
// dispatchScope 一次装配的绑定上下文。
//
// owner 是**信任判定的归属**（owner 隔离）：同一条 project 级 hook 命令在
// workspace:/a、agent:x、group:g 下是三条独立授权，互不继承。cwd/sessionID 只影响
// hook 子进程的运行环境与 payload，不参与授权。
type dispatchScope struct {
	cwd       string
	sessionID string
	owner     string
}

// cwd/sessionID 在构造时绑定：cwd 作为 hook 子进程的工作目录与 GOCODE_PROJECT_DIR，
// sessionID 用于 payload.session_id（会话内去重/统计）。
//
// 兼容入口：owner 按 cwd 派生为 workspace:<cwd>（**不是**全局）—— 旧的 ToolHooks 调用方
// 自动获得「按工作区隔离」的最小安全语义；Hub 会话必须走 ToolHooksFor 显式给 agent/group。
func (d *Dispatcher) ToolHooks(cwd, sessionID string) *events.ToolHooks {
	return d.ToolHooksFor(cwd, sessionID, owner.Workspace(cwd))
}

// ToolHooksFor 绑定 owner 的装配入口（Hub DM → agent:<id>；群聊 → group:<g>；
// 项目会话 → workspace:<path>）。owner 为空/非法时信任闸门一律判未信任（不猜归属）。
func (d *Dispatcher) ToolHooksFor(cwd, sessionID, ownerID string) *events.ToolHooks {
	sc := dispatchScope{cwd: cwd, sessionID: sessionID, owner: ownerID}
	return &events.ToolHooks{
		PreToolUse:       d.preToolUse(sc),
		PostToolUse:      d.postToolUse(sc),
		PostToolBatch:    d.postToolBatch(sc),
		PreCompact:       d.preCompact(sc),
		Stop:             d.stop(sc),
		SessionStart:     d.sessionStart(sc),
		UserPromptSubmit: d.userPromptSubmit(sc),
	}
}

// ── 七个钩子的实现（ToolHooks 与 On* 方法共用同一份逻辑）──────────────────

func (d *Dispatcher) preToolUse(sc dispatchScope) events.PreToolUseHook {
	return func(ctx context.Context, pc events.PreToolUseContext) events.PreToolUseResult {
		out := d.runner.Dispatch(ctx, EventPreToolUse, Payload{
			SessionID:     sc.sessionID,
			Cwd:           sc.cwd,
			Owner:         sc.owner,
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

func (d *Dispatcher) postToolUse(sc dispatchScope) events.PostToolUseHook {
	return func(ctx context.Context, pc events.PostToolUseContext) events.PostToolUseResult {
		out := d.runner.Dispatch(ctx, EventPostToolUse, Payload{
			SessionID:     sc.sessionID,
			Cwd:           sc.cwd,
			Owner:         sc.owner,
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

func (d *Dispatcher) postToolBatch(sc dispatchScope) events.PostToolBatchHook {
	return func(ctx context.Context, _ []events.ToolRunResult) bool {
		out := d.runner.Dispatch(ctx, EventPostToolBatch, Payload{
			SessionID: sc.sessionID, Cwd: sc.cwd, Owner: sc.owner, HookEventName: string(EventPostToolBatch),
		}, MatchValue{})
		d.report(EventPostToolBatch, out)
		// 外部 hook 不允许终止整个运行（终止只走 abort 语义）：返回值恒 false。
		// 需要"别结束"的场景用 Stop 钩子（有上限护栏）。
		return false
	}
}

func (d *Dispatcher) preCompact(sc dispatchScope) events.PreCompactHook {
	return func(ctx context.Context) string {
		out := d.runner.Dispatch(ctx, EventPreCompact, Payload{
			SessionID: sc.sessionID, Cwd: sc.cwd, Owner: sc.owner, HookEventName: string(EventPreCompact),
		}, MatchValue{})
		d.report(EventPreCompact, out)
		text, _ := BuildInject(out.Contexts, maxEventInjectBytes)
		return text
	}
}

// stop 是收尾门控的适配：内核在**截断判定之前**派发本钩子（见 EventStop 契约），
// 所以截断轮也会走到这里（LastAssistant 可能为空）。exit 2 / decision=block →
// Continue=true，内核续跑一轮（上限 8）——截断轮因此可被外部 hook 要求继续；
// 引擎自身不自动重试截断。abort / ctx 取消 / 更早的出错路径不进入本钩子。
func (d *Dispatcher) stop(scope dispatchScope) events.StopHook {
	return func(ctx context.Context, sc events.StopContext) events.StopResult {
		out := d.runner.Dispatch(ctx, EventStop, Payload{
			SessionID: scope.sessionID, Cwd: scope.cwd, Owner: scope.owner, HookEventName: string(EventStop),
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

func (d *Dispatcher) sessionStart(scope dispatchScope) events.SessionStartHook {
	return func(ctx context.Context, sc events.SessionStartContext) string {
		out := d.runner.Dispatch(ctx, EventSessionStart, Payload{
			SessionID: scope.sessionID, Cwd: scope.cwd, Owner: scope.owner, HookEventName: string(EventSessionStart),
			Source: sc.Source,
		}, MatchValue{Source: sc.Source})
		d.report(EventSessionStart, out)
		text, _ := BuildInject(out.Contexts, maxEventInjectBytes)
		return text
	}
}

func (d *Dispatcher) userPromptSubmit(sc dispatchScope) events.UserPromptSubmitHook {
	return func(ctx context.Context, up events.UserPromptSubmitContext) events.UserPromptSubmitResult {
		out := d.runner.Dispatch(ctx, EventUserPromptSubmit, Payload{
			SessionID: sc.sessionID, Cwd: sc.cwd, Owner: sc.owner, HookEventName: string(EventUserPromptSubmit),
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
	return d.sessionStart(dispatchScope{cwd: p.Cwd, sessionID: p.SessionID, owner: p.Owner})(ctx, events.SessionStartContext{Source: source})
}

// OnUserPromptSubmit 直接派发 UserPromptSubmit。
func (d *Dispatcher) OnUserPromptSubmit(ctx context.Context, p Payload) (inject string, blocked bool, reason string) {
	res := d.userPromptSubmit(dispatchScope{cwd: p.Cwd, sessionID: p.SessionID, owner: p.Owner})(ctx, events.UserPromptSubmitContext{Prompt: p.Prompt})
	return res.AdditionalContext, res.Block, res.Reason
}

// OnPostToolUse 直接派发 PostToolUse。
func (d *Dispatcher) OnPostToolUse(ctx context.Context, p Payload) (inject string, replaceOutput string) {
	pc := events.PostToolUseContext{Arguments: string(p.ToolInput)}
	if p.ToolResponse != nil {
		pc.Result, pc.IsError = p.ToolResponse.Text, p.ToolResponse.IsError
	}
	res := d.postToolUse(dispatchScope{cwd: p.Cwd, sessionID: p.SessionID, owner: p.Owner})(ctx, pc)
	return res.AdditionalContext, res.ReplaceResult
}

// OnStop 直接派发 Stop。
func (d *Dispatcher) OnStop(ctx context.Context, p Payload) (continueTurn bool, reason string) {
	res := d.stop(dispatchScope{cwd: p.Cwd, sessionID: p.SessionID, owner: p.Owner})(ctx, events.StopContext{RunId: p.TurnID})
	return res.Continue, res.Reason
}

// ── 审计 ────────────────────────────────────────────────────────────────

// emitRun 每次 handler 执行都回调一次。
//
// 口径（2026-11 修）：异常一律上报；正常结束**原本一律静默**（注释说"走日志"，
// 而日志并不存在），于是"hook 到底跑没跑"在界面与磁盘上都答不出来。现在按副作用
// 分档：注入了上下文 / 改了工具输入 / 替换了输出的，模型看得到而用户看不到来源，
// 必须报；纯粹跑完没说话的仍然静默（PostToolUse 每个工具一次，报了就是刷屏）。
// 全量记录落在 Journal 里，供事后追问。
func (d *Dispatcher) emitRun(run HookRun) {
	if d.warn == nil {
		return
	}
	switch run.Status {
	case "ok", "ok_no_output":
		if run.SideEffect == "" {
			return
		}
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
