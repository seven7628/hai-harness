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
	"strings"

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
	// Members 群成员档案（§6.2.2 名册层）。Group 只带 member_ids/role，
	// 渲染名册还需要名字/职责 —— 缺了会退化成 "unknown member (ag_…)"（P1-02 复核）。
	Members []*agenthub.AgentProfile
	// MemoryInjection 装配期算好的记忆索引文本（空 = 不注入）。
	// 为什么要装配期算：composeSystemPrompt 每轮重跑，解析记忆目录要跑 git 读索引，
	// 放进去等于每轮重复一次（与项目会话同一个理由，见 memory_prefs.go）。
	MemoryInjection string
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
func (m *manager) hubSandbox(home string, toolsPolicy agenthub.ToolPolicy, runPolicy agenthub.RunPolicy) sandbox.Sandbox {
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
	// 额外只读根：tools.bash_roots（bash 能看的目录）+ policy.workspace_roots
	// （workspace_* 工具要操作的目录）。两者都只读 —— 智能体要改，就派任务给工作区会话
	//（§6.3.1）。去重后一次注入。
	if roots := readOnlyRoots(toolsPolicy.BashRoots, runPolicy.WorkspaceRoots); len(roots) > 0 {
		opts = append(opts, sandbox.WithReadOnlyPaths(roots...))
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

// hubWorkspaceGuard 构造 session_send 的预算/并发护栏。群日预算不在这里固化：
// 任务 Origin.GroupID 只有 Send 时才知道，Guard 通过钩子按群现读（P2.5-04 B1）。
func (m *manager) hubWorkspaceGuard(p agenthub.AgentProfile) *agenthub.Guard {
	limits := agenthub.GuardLimits{
		MaxTaskCostUSD:       p.Policy.MaxTaskCostUSD,
		DailyBudgetUSD:       p.Policy.DailyBudgetUSD,
		GlobalDailyBudgetUSD: agenthub.DefaultGlobalDailyBudgetUSD,
	}
	return &agenthub.Guard{
		Limits: limits,
		GroupDailyBudget: func(ctx context.Context, gid agenthub.GroupID) (float64, error) {
			st, err := m.hubStore()
			if err != nil {
				return 0, err
			}
			g, err := st.LoadGroup(ctx, gid)
			if err != nil {
				return 0, err
			}
			return g.Policy.DailyBudgetUSD, nil
		},
	}
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
		hubPersonaProfile(), m.hubSandbox(ws.path, p.Tools, p.Policy),
		nil, // 不传 mesh Hub：session_send/session_list 由 Hub 自己的工具提供（§6.3.1）
		m.pythonRuntime(), m.loadSkillOnLoadHook(), m.bashEgoBeforeExec(), m.memoryConfig(),
	)
	// Hub 自己的工作区/会话工具（§6.3.1 明确替换 mesh 的同名工具，语义不同：
	// 这里派给项目会话，不切全局 provider）。注册后再统一过 Profile 策略，
	// 所以 readonly 预设仍能看到 list、看不到 create/send。
	svc := agenthub.NewWorkspaceService(newHubWorkspaceOps(m), p.ID)
	svc.Origin = agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: p.ID}
	if q, err := m.hubQueueFor(); err == nil {
		svc.Queue = q // P2-03：任务落账本 + 空闲闸门
		// P2-07：预算/并发/去重/链长护栏。群日预算在初始派发时按任务
		// Origin.GroupID 现读 Group.Policy（DM 引擎自己不知道群上下文）。
		svc.Guard = m.hubWorkspaceGuard(p)
	} else {
		// 降级直派是 fail-open：至少留一条诊断，别让闸门静默失效。
		m.out.writeLine(map[string]any{
			"event_type": "hub_alert", "kind": "queue_unavailable",
			"message": "Hub 队列初始化失败，session_send 降级为直接派发: " + err.Error(),
		})
	}
	registerHubGroupTools(eng, m, p.ID)
	registerHubArtifactTools(eng, m, p.ID)
	registerHubTaskTools(eng, m, p.ID)
	registerHubWorkspaceTools(eng, svc, func(code, message string) {
		m.out.writeLine(map[string]any{"event_type": "hub_alert", "kind": code, "agent_id": string(p.ID), "message": message})
	})
	applyHubToolPolicy(context.Background(), eng, p.Tools)
	return eng, effortRef
}

// applyHubToolPolicy 按 Profile 撤下工具（hidden = 模型不可见 + codemode 不可编排）。
// 返回被撤下的工具名（供事件/诊断用）。
func applyHubToolPolicy(ctx context.Context, eng *tools.Engine, policy agenthub.ToolPolicy) []string {
	if eng == nil {
		return nil
	}
	// 动态钩子：装配之后才注册的工具（MCP 重连、插件热装）同样要过策略。
	// 只按当前快照 HideTool 会漏掉它们 —— 那正是 §6.3.4「调用期再判一次」的缺口。
	eng.SetToolFilter(func(t tools.Tool) bool { return hubToolVisible(policy, t) })
	var hidden []string
	for _, t := range eng.Tools() {
		name := t.Name()
		if hubToolVisible(policy, t) {
			continue
		}
		// 引擎级撤下（不重注册 wrapper）：工具实例与其 Optional 接口保持原样，
		// GetTool 直接报「disabled by policy」→ 直呼与 codemode 两条路都拦。
		eng.HideTool(name)
		hidden = append(hidden, name)
	}
	return hidden
}

// reapplyHubToolPolicy 运行期替换策略：先恢复全部 hidden，再按新策略撤下。
//
// HideTool 是粘性的（MCP 重连/插件热更新都不会自动恢复），所以「禁 → 放开」
// 必须显式 ShowTool；否则资料页把 bash 打开后，当前会话永远回不来（P1-10 B5）。
func reapplyHubToolPolicy(eng *tools.Engine, policy agenthub.ToolPolicy) []string {
	if eng == nil {
		return nil
	}
	for _, t := range eng.Tools() {
		eng.ShowTool(t.Name())
	}
	eng.SetToolFilter(nil)
	return applyHubToolPolicy(context.Background(), eng, policy)
}

// mcpIdentity 真实 MCP 适配器暴露的身份（server/tool 自带 "__" 时名字反解不可靠）。
type mcpIdentity interface {
	MCPServerName() string
	MCPToolName() string
}

// hubToolVisible 单个工具是否满足 Profile：先看 MCP 的 server/tool 级开关，
// 再看通用规则；只读预设由工具自身的 ReadOnly() 声明决定（未声明 = 非只读）。
func hubToolVisible(policy agenthub.ToolPolicy, t tools.Tool) bool {
	name := t.Name()
	if !policy.ToolVisible(name, toolReadOnly(t)) {
		return false
	}
	if id, ok := t.(mcpIdentity); ok {
		return policy.MCPAllowed(id.MCPServerName(), id.MCPToolName())
	}
	if server, tool, ok := agenthub.SplitMCPToolName(name); ok {
		// 反解只是没有访问器时的兜底：server 自带 "__" 时边界不可辨，任何 MCP 限制
		// 都按「可能命中」拒绝（fail-closed）—— 否则 Disable/DisableServers/AllowTools
		// 会被名字歧义静默绕过（P1-01 复核 blocking）。
		if agenthub.MCPNameAmbiguous(name) && policy.MCPRestricted() {
			return false
		}
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

// readOnlyRoots 合并两组只读根（去重、保序、跳过空串）。
func readOnlyRoots(groups ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, g := range groups {
		for _, p := range g {
			p = strings.TrimSpace(p)
			if p == "" || seen[p] {
				continue
			}
			seen[p] = true
			out = append(out, p)
		}
	}
	return out
}
