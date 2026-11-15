package main

// plugin_tools.go —— 插件虚拟 Project 的工具面（09-agent-bridge §7 / D8、D10、D11）。
//
// 装配口径 = Hub 的**全局层**（见 buildHubEngine）：内置 file/bash/edit/glob/grep/
// run_python/todo/ask_user/agent_* 系列/codemode/办公工具/MCP/技能/子代理，不叠任何
// 项目层。在此之上两件事：
//
//  1. 集中 denylist（§7.2）：能操作宿主 / 其它会话 / 其它插件 / 真实仓库的工具一律剔除；
//  2. 装配点唯一：将来 P2 的 plugin__* 代理工具**只**在 buildPluginEngine 里注册 ——
//     项目 runtime 走 newSessionEngine 的另一条分支，永远不会看到插件工具（D10：
//     「项目管理」不是插件工具的开关）。
//
// 为什么是 denylist 而不是 allowlist（§12 风险表建议过 allowlist）：工具面的绝大多数
// 名字来自动态来源（MCP 的 mcp__<srv>__<tool>、插件自带工具、办公带内嵌工具的开关），
// 白名单在这里等于「每个动态来源都要维护一份前缀表」，漏一处就是「装了 MCP 但模型看不到」。
// 反过来 denylist 的失效模式是「新增的越权工具默认放行」——所以下面的表按**类别**列全
// （surface/session/workspace/任务板/cron/仓库/旧只读命令），并由 TestPluginToolDenylist
// 钉住「表里每一项确实不在引擎里」，同时用隐藏钩子兜住装配后动态注册的工具。

import (
	"context"
	"strings"

	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/sandbox"
	"github.com/seven7628/hai-harness/subagent"
	"github.com/seven7628/hai-harness/tools"
)

// pluginToolDenyExact 插件虚拟 Project 里**绝不注册/绝不可调用**的工具名（09 §7.2 + D11）。
//
// 每条都写「是什么 → 为什么」：这张表是安全边界，注释掉了它下次就会被当成噪音删掉。
var pluginToolDenyExact = map[string]bool{
	// 面板插件安装/卸载/清单：插件能装卸插件 = 权限升级；含只读 surface_list（D11）。
	"surface_install": true,
	"surface_remove":  true,
	"surface_list":    true,
	// Session Mesh：给其它会话/节点发消息 = 跨会话注入、内容外发。
	"session_send": true,
	"session_list": true,
	// Hub 工作区/会话工具：插件能建真实工作区 = 污染 workspaces.json；能建别的会话 =
	// 把虚拟 Project 当编排台。
	"workspace_create": true,
	"workspace_list":   true,
	"session_create":   true,
	// Hub 多智能体/任务板：插件不该编排 Hub、改写任务状态。
	"assign_task":  true,
	"handoff":      true,
	"handoff_ack":  true,
	"agent_status": true,
	// 后台持久/定时：用户不看着时持续消耗 token 与写盘（promote_task 是宿主的摘离命令，
	// 当前没有同名工具 —— 一并列入，防将来把它做成工具时静默放行）。
	"promote_task": true,
	"CronCreate":   true,
	"CronList":     true,
	"CronUpdate":   true,
	"CronDelete":   true,
	// 真实仓库操作：插件 home 不是仓库，用户明确不需要。
	"git_worktree": true,
	// 旧只读命令（D11：已从 command 表移除；作为工具存在的同名项同样剔除）：
	// 能读真实项目/会话元数据，与「不碰 Project」冲突。
	"git_snapshot": true,
	"ui_inspect":   true,
}

// pluginToolDenyPrefix 按前缀剔除的一族（任务板 / 群工具全部在族内）。
// 用前缀而不是逐个名字：task_ack / task_cancel / task_status 与 group_history /
// group_members / group_say 会随 Hub 演进增减，逐个列等于每次新增都开一个口子。
var pluginToolDenyPrefix = []string{"group_", "task_"}

// pluginToolDenied 该工具名是否被 §7.2 的 denylist 剔除。
func pluginToolDenied(name string) bool {
	if pluginToolDenyExact[name] {
		return true
	}
	for _, p := range pluginToolDenyPrefix {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// applyPluginToolPolicy 把 denylist 落到引擎上（与 applyHubToolPolicy 同一套机制）：
//
//	· SetToolFilter = 动态钩子：装配完成后才注册的工具（MCP 重连 / 插件热装）同样过表；
//	· HideTool     = 引擎级撤下：GetTool 直接报「disabled by policy」，直呼与 codemode
//	  两条路都拦（只靠 filter 时已经在工具表里的实例仍可能被 codemode 编排到）。
//
// 返回被撤下的工具名（诊断/测试用）。
func applyPluginToolPolicy(eng *tools.Engine) []string {
	if eng == nil {
		return nil
	}
	eng.SetToolFilter(func(t tools.Tool) bool { return !pluginToolDenied(t.Name()) })
	var denied []string
	for _, t := range eng.Tools() {
		name := t.Name()
		if !pluginToolDenied(name) {
			continue
		}
		eng.HideTool(name)
		denied = append(denied, name)
	}
	return denied
}

// buildPluginEngine 装配插件虚拟 Project 的工具引擎（全局层 + denylist）。
//
// 返回的 effortRef 与主 loop 共享（切档时子智能体同档）。**cron / mesh 传 nil**：
// 分流表第 6 点要求「全部跳过」—— 光靠 denylist 会让工具先被注册再从引擎里撤下，
// 而这两族工具的构造本身就要拿宿主级 Store / Hub（不该出现在虚拟 Project 的装配里）。
func (m *manager) buildPluginEngine(ws *workspaceRuntime, sid, prov, model, effort string, reg *subagent.Registry, qw *events.QuestionWaiter, bgCtx func() context.Context) (*tools.Engine, *subagentEffortRef) {
	if ws == nil {
		return tools.NewToolEngine(), newSubagentEffortRef(provider.ReasoningEffortLevelLow)
	}
	if effort == "" {
		effort = string(provider.ReasoningEffortLevelLow)
	}
	eng, effortRef := newSessionEngineAt(
		ws.sessionsRoot(),
		ws.path, sid,
		nil, // cron：分流点 6 —— 插件会话不注册定时任务工具
		m.provCfg, prov, model, effort,
		bgCtx, reg, qw,
		ws.skills, ws.subagents, ws.mcp, ws.plugins,
		profiles["code"],
		m.pluginSandbox(ws.path, sid, &sandboxReleaseGate{m: m, ws: ws.path, id: sid}),
		nil, // mesh Hub：分流点 6 —— 插件会话不进 Session Mesh（session_* 不注册）
		m.pythonRuntime(), m.loadSkillOnLoadHook(), m.bashEgoBeforeExec(), m.memoryConfig(),
	)
	// 办公组件验收工具（§7.1 允许清单含 office）：kit 不可用时注册函数自己跳过。
	m.registerOfficeTools(eng, ws.path)
	// ⚠ 刻意**不**注册 registerUITools / registerSurfaceTools：
	//   ui_inspect（读宿主 UI 快照）、surface_*（装/卸插件）都在 §7.2/D11 的剔除表里。
	//   项目会话（buildLoop 的非插件分支）照旧注册，那里是用户自己的操作面。
	applyPluginToolPolicy(eng)
	// P2 的插件工具（plugin__<id>__<name>）装配点就在这里 —— 只对 ws.pluginID 注册，
	// 且只在 runtimePlugin 上走到本函数。项目 runtime 没有这条路径（D10）。
	//
	// 放在 applyPluginToolPolicy **之后**：代理工具名一定不含 denylist 里的名字，
	// 先注册也只是让「撤下」这一步多看几眼；但顺序反过来（先注册工具、后装 filter）
	// 会让运行期新建的代理工具漏过 filter —— 一次装配只发生一次，仍按「策略先就位」
	// 写，读代码时不用在脑子里排时序。
	m.registerPluginProxyTools(eng, ws, sid)
	return eng, effortRef
}

// pluginSandbox 插件虚拟 Project 的执行沙箱：root = 虚拟 home。
//
// **刻意不传 WithHomeWrite**（与 hub 同口径）：项目会话为了 pip/npm 放开了整个 home 写权限，
// 插件只该写自己的 home。但 home 在 ~/.go-code 之下，而沙箱把 ~/.go-code 整段 deny
// （settings/密钥），所以必须 WithOpenSubpaths(home) 把这棵子树重新打开 —— 否则插件连
// 自己的 home 都读不了写不了。返回 nil = 本机没开 seatbelt（与项目会话同口径）。
func (m *manager) pluginSandbox(home, sid string, gate sandbox.ReleaseGate) sandbox.Sandbox {
	if home == "" {
		return nil
	}
	ss := loadSandboxSettings()
	if ss.Mode != "seatbelt" || !sandbox.Available() {
		return nil
	}
	var opts []sandbox.SeatbeltOption
	if len(ss.SensitivePaths) > 0 {
		opts = append(opts, sandbox.WithSensitivePaths(ss.SensitivePaths))
	}
	opts = append(opts, sandbox.WithOpenSubpaths(home))
	sb, err := sandbox.NewSeatbelt(home, opts...)
	if err != nil {
		m.out.writeLine(map[string]any{
			"event_type": "sandbox_status", "status": "degraded",
			"error": err.Error(), "workspace": home, "kind": "plugin_project",
		})
		return nil
	}
	strict := sandbox.WithRelease(sb, gate)
	// 会话切到 full-access 时直通（与项目会话同口径：mode 是用户的显式选择）。
	return sandbox.WithBackendSelector(func() sandbox.Sandbox {
		if bs := m.get(home, sid); bs != nil && bs.mode != nil {
			switch modeName(bs.mode) {
			case "full-access", "all-pass":
				return sandbox.NoSandbox{}
			}
		}
		return strict
	})
}
