package main

// hub_tools.go：Agent Hub 智能体的工具引擎（方案 §6.3）。
//
// 与项目会话引擎的差别只有两处：
//   1. cwd / 文件根 = 智能体 home（不是项目工作区），bash 沙箱也只写 home；
//   2. Profile 的工具开关在装配后按「同名重注册为 hidden」撤下 —— 模型看不到、
//      codemode 也编排不到（§6.3.1：唯一不注册的是 mesh 的 session_send/session_list，
//      因为 Hub 有同名工具且语义不同）。
//
// 其余全部复用 newSessionEngine：文件/bash/run_python/ask_user/子智能体/codemode/
// cron/MCP/技能/插件 —— 「不预先阉割能力」（D18）落在同一份装配代码上，避免两套
// 工具面慢慢漂移。

import (
	"context"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/sandbox"
	"github.com/seven7628/hai-harness/subagent"
	"github.com/seven7628/hai-harness/tools"
)

// hubSessionSpec 一次 Hub 会话的装配输入。字段随 P1 逐项补齐（记忆/群组/scope）。
type hubSessionSpec struct {
	Agent agenthub.AgentProfile
	Scope agenthub.Scope
	Group *agenthub.Group
}

// hubPersonaProfile 工具面等价于 Code（bash + 子智能体全开），但系统提示词由
// buildHubLoop 单独给（L1+L2），这里只用来驱动 newSessionEngine 的注册分支。
func hubPersonaProfile() personaProfile {
	return personaProfile{name: "hub", withBash: true, withAgent: true, replaceBase: true}
}

// hubSandbox 智能体的执行沙箱：root = 智能体 home。
// **刻意不传 WithHomeWrite** —— 项目会话为了 pip/npm 放开了整个 home 写权限，
// Hub 智能体只该写自己的 home（§6.3.1）；要改工作区文件必须派任务给工作区会话。
// 返回 nil = 本机没开 seatbelt（与项目会话同口径：此时不做进程级隔离，靠 cwd/审批）。
func (m *manager) hubSandbox(home string, policy agenthub.ToolPolicy) sandbox.Sandbox {
	ss := loadSandboxSettings()
	if ss.Mode != "seatbelt" || !sandbox.Available() {
		return nil
	}
	var opts []sandbox.SeatbeltOption
	if len(ss.SensitivePaths) > 0 {
		opts = append(opts, sandbox.WithSensitivePaths(ss.SensitivePaths))
	}
	// 智能体 home 在 ~/.go-code 之下，而 ~/.go-code 因含 settings/密钥被沙箱整段 deny：
	// 不在 deny 之后重开这一棵子树，智能体连自己的 home 都读不了写不了（P1-01 实测）。
	opts = append(opts, sandbox.WithOpenSubpaths(home))
	// tools.bash_roots：额外的只读根（能看不能改 —— 要改就派任务给工作区）。
	if len(policy.BashRoots) > 0 {
		opts = append(opts, sandbox.WithReadOnlyPaths(policy.BashRoots...))
	}
	sb, err := sandbox.NewSeatbelt(home, opts...)
	if err != nil {
		m.out.writeLine(map[string]any{
			"event_type": "sandbox_status", "status": "degraded",
			"error": err.Error(), "workspace": home, "kind": "hub_agent",
		})
		return nil
	}
	return sb
}

// buildHubEngine 装配智能体的工具引擎，并按 Profile 应用工具开关。
// 返回的 effortRef 与主 loop 共享（切档时子智能体同档）。
func (m *manager) buildHubEngine(ws *workspaceRuntime, sid string, spec *hubSessionSpec, reg *subagent.Registry, qw *events.QuestionWaiter, bgCtx func() context.Context) (*tools.Engine, *subagentEffortRef) {
	if ws == nil || spec == nil {
		return tools.NewToolEngine(), newSubagentEffortRef(provider.ReasoningEffortLevelLow)
	}
	p := spec.Agent
	effort := string(p.Effort)
	eng, effortRef := newSessionEngine(
		ws.path, sid, m.cron, m.provCfg, p.Provider, p.Model, effort,
		bgCtx, reg, qw, ws.skills, ws.subagents, ws.mcp, ws.plugins,
		hubPersonaProfile(), m.hubSandbox(ws.path, p.Tools),
		nil, // 不传 mesh Hub：session_send/session_list 由 Hub 自己的工具提供（§6.3.1）
		m.pythonRuntime(), m.loadSkillOnLoadHook(), m.bashEgoBeforeExec(), m.memoryConfig(),
	)
	applyHubToolPolicy(context.Background(), eng, p.Tools)
	return eng, effortRef
}

// applyHubToolPolicy 按 Profile 撤下工具（hidden = 模型不可见 + codemode 不可编排）。
// 返回被撤下的工具名（供事件/诊断用）。
func applyHubToolPolicy(ctx context.Context, eng *tools.Engine, policy agenthub.ToolPolicy) []string {
	if eng == nil {
		return nil
	}
	var hidden []string
	for _, t := range eng.Tools() {
		name := t.Name()
		if hubToolVisible(policy, t) {
			continue
		}
		eng.RegisterTool(ctx, tools.Hide(t))
		hidden = append(hidden, name)
	}
	return hidden
}

// hubToolVisible 单个工具是否满足 Profile：先看 MCP 的 server/tool 级开关，
// 再看通用规则；只读预设由工具自身的 ReadOnly() 声明决定（未声明 = 非只读）。
func hubToolVisible(policy agenthub.ToolPolicy, t tools.Tool) bool {
	name := t.Name()
	if !policy.ToolVisible(name, toolReadOnly(t)) {
		return false
	}
	if server, tool, ok := agenthub.SplitMCPToolName(name); ok {
		return policy.MCPAllowed(server, tool)
	}
	return true
}

func toolReadOnly(t tools.Tool) bool {
	if r, ok := t.(tools.ReadOnlyTool); ok {
		return r.ReadOnly()
	}
	return false
}
