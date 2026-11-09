package main

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/hooks"
	"github.com/seven7628/hai-harness/session"
)

// hooks 能力（外部命令 hook）的宿主接线。
//
// 设计规范：docs/graft-integration/05-hooks-能力设计规范.md
// 实现：github.com/seven7628/hai-harness/hooks（配置 + 执行器 + 适配层）
//
// 接线方式与"接线前完全一致"的保证：
//   - 没有任何 hooks 配置（用户级/项目级/.claude 兼容层都为空）或 disableAllHooks=true
//     → setupHooks 返回 nil → session.WithHooks(nil) → 内核所有新钩子为 nil，行为不变；
//   - 配置存在但全部未信任 → 派发器仍会构造，但每次派发只产出"not trusted"告警、不执行；
//   - 任何 hook 异常都不阻断会话（超时/panic/非法输出一律 fail-open 或按事件分档）。
//   - Hub 智能体会话（P4-04a）只装**用户级**来源：home 是智能体数据目录，不是项目
//     工作区，setupUserHooks 以空 WorkspaceDir 显式排除项目层（见该函数注释）。
//
// 三个接入点（工作区级装配一次，会话级绑定 sessionID）：
//  1. PreToolUse / PostToolBatch / PreCompact / PostToolUse / Stop → session.WithHooks（本文件）
//  2. SessionStart / UserPromptSubmit → Session 内部派发（session/session.go，经 cfg.Hooks）
//  3. MCP server 的 instructions → 系统提示词（见 mcp/instructions.go；宿主侧注入点为后续工作）

// hooksStatePath 信任库路径：~/.github.com/seven7628/hai-harness/hooks-state.json。
// 与 settings.json 同目录但**独立文件**：避免与 Electron 对 settings.json 的全量重写互相
// 覆盖（第三方安装器直接改 settings.json 有被覆盖的真实风险），也避免把"已确认"混进配置。
func hooksStatePath() string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return ""
	}
	return filepath.Join(home, ".go-code", "hooks-state.json")
}

// setupHooks 为一个工作区装配 hooks 派发器；返回 nil = 该工作区没有可用的 hooks。
//
// 配置来源与顺序（hooks.Load）：用户级 ~/.go-code/settings.json → 项目级
// {ws}/.github.com/seven7628/hai-harness/hooks.json（默认需 allowProjectHooks，防止"打开别人的仓库即执行其命令"）
// → 只读兼容 .claude/settings.json（Claude Code / Codex 形态，Graft 等工具的既有 wiring）。
//
// 信任库取**进程内共享实例**（m.hookTrustStore）：信任判定发生在每次派发时
// （Runner.Dispatch → TrustStore.Check 读的是实例内存里的表），若每个会话各持一份，
// 面板里的 hooks_trust 就要重建会话才生效 —— 与契约 §5.5「信任即刻生效」矛盾。
func (m *manager) setupHooks(wsPath string, warn func(name, message string)) *hooks.Dispatcher {
	// 防御纵深（P4-04a N7）：Hub 智能体 home 不是项目工作区。若有人从项目路径
	// （createOpts / reloadWorkspaceHooks / 未来的新调用点）把它传进来，这里直接
	// 返回 nil —— 绝不读 {home}/.go-code/hooks.json 或 .claude 项目配置。
	// 正常 Hub 装配走 setupUserHooks（只读用户级），不经过本函数。
	if isHubHomePath(wsPath) {
		fmt.Fprintf(os.Stderr, "· hooks: 拒绝把 Hub 智能体 home 当项目工作区加载 hooks: %s\n", wsPath)
		return nil
	}
	cfg, warnings := hooks.Load(hooks.LoadOptions{WorkspaceDir: wsPath})
	return m.dispatcherFor(cfg, warnings, warn)
}

// setupUserHooks 装配"仅用户级来源"的 hooks 派发器（Hub 智能体会话用，P4-04a）。
//
// 为什么 Hub 不能复用 setupHooks(agentHome)：Hub 的 home（~/.go-code/hub/homes/<id>）
// 是智能体自己的数据目录，**不是**用户的项目工作区。把它当 WorkspaceDir 传进去，
// hooks.Load 会把 {home}/.go-code/hooks.json 与 {home}/.claude/settings.json 当成
// 项目层配置读入 —— 等于"打开一个智能体"就可能执行该目录里的命令（信任闸门只能挡住
// 未确认过的命令，挡不住用户此前确认过的同一条命令）。
//
// 空 WorkspaceDir 就是这条安全边界：hooks.Load 的项目级分支与项目形态的 .claude
// 路径都由 WorkspaceDir != "" 门控，空值 = 只读用户级来源（~/.go-code/settings.json
// 与只读兼容的 ~/.claude/settings.json）。信任库仍走进程内共享实例，与 hooks_set 同源。
func (m *manager) setupUserHooks(warn func(name, message string)) *hooks.Dispatcher {
	cfg, warnings := hooks.Load(hooks.LoadOptions{})
	return m.dispatcherFor(cfg, warnings, warn)
}

// dispatcherFor 是两条装配链的公共尾部：告警落 stderr → 空配置/全关 → nil → 派发器。
// 抽出来是为了让"用户级"与"工作区级"只在**加载范围**上不同，构造与信任判定逐字一致
// （否则两套装配会在"改了配置之后"出现难查的偏差）。
func (m *manager) dispatcherFor(cfg *hooks.Config, warnings []string, warn func(name, message string)) *hooks.Dispatcher {
	for _, w := range warnings {
		// 配置期告警发生在会话创建之前（bs.s 尚未赋值），只走 stderr，不发事件。
		fmt.Fprintf(os.Stderr, "· hooks: %s\n", w)
	}
	if cfg == nil || cfg.DisableAll || len(cfg.Events) == 0 {
		return nil
	}
	d := hooks.NewDispatcher(cfg, m.hookTrustStore(), warn)
	fmt.Fprintf(os.Stderr, "· hooks: 已装配 %d 类事件（来源 %v）\n", len(cfg.Events), cfg.Sources)
	return d
}

// hookTrustStore 进程内共享的信任库（懒读一次；~/.github.com/seven7628/hai-harness/hooks-state.json）。
//
// 用独立锁（hookTrustMu）而不是 m.mu：本函数可能在任何装配路径被调用（含持 ws.mu 的
// 会话遍历），走 m.mu 会引入不必要的锁序耦合。
func (m *manager) hookTrustStore() *hooks.TrustStore {
	m.hookTrustMu.Lock()
	defer m.hookTrustMu.Unlock()
	if m.hookTrust == nil {
		ts, err := hooks.LoadTrust(hooksStatePath())
		if err != nil {
			fmt.Fprintf(os.Stderr, "· hooks: 信任库不可读，全部按未信任处理: %v\n", err)
		}
		m.hookTrust = ts
	}
	return m.hookTrust
}

// hooksFor 返回某会话应装配的 hook 聚合。
// 参数 d 为 nil（该工作区无 hooks）→ 返回 nil，Session 侧全钩子为 nil（零开销）。
func hooksFor(d *hooks.Dispatcher, workspace, sessionID string) *events.ToolHooks {
	if d == nil {
		return nil
	}
	return d.ToolHooks(workspace, sessionID)
}

// hooksOption 装配某会话的 hooks（配置 + 信任库 → 派发器 → ToolHooks）并作为 Session 选项返回。
//
// 为什么抽成函数：会话**首次创建**（createOpts）与**配置热替换**（reloadWorkspaceHooks）
// 必须走完全同一条装配链 —— 否则"新建的会话"和"改过配置后的会话"会拿到不同的 hooks，
// 而且只在改过一次配置之后才暴露（最难查的一类偏差）。
func (m *manager) hooksOption(wsPath, id string, warn func(name, message string)) session.Option {
	return session.WithHooks(hooksFor(m.setupHooks(wsPath, warn), wsPath, id))
}

// hooksOptionUser 是 Hub 智能体会话的装配入口（P4-04a）：**只装用户级来源**，
// cwd = 智能体 home，sessionID = Hub 会话 id（DM: hub.<agent>.dm；群: hub.<agent>.g.<group>）。
//
// Stop 就是 Hub 的 AgentEnd 锚点：EventStop 在 agents/agent_loop.go 的收尾门控点
// （截断判定之前）派发（与项目会话同一份契约），payload.hook_event_name = "Stop"、
// turn_id = run id。截断轮也会派发（LastAssistant 可能为空），abort/更早出错不派发。
// 不新增公共事件字符串，也不另造第二套 AgentEnd hook —— 外部 hook 只认 Stop。
//
// cwd 传 home 而不是空串：hook 进程的 GOCODE_PROJECT_DIR/CLAUDE_PROJECT_DIR 与
// payload.cwd 需要指向一个真实存在、且属于该智能体的目录（其读写边界）。
func (m *manager) hooksOptionUser(cwd, id string, warn func(name, message string)) session.Option {
	return session.WithHooks(hooksFor(m.setupUserHooks(warn), cwd, id))
}

// reloadWorkspaceHooks 重读该工作区的 hooks 配置并推给它的**全部会话**（hooks_set 后调用）。
//
// 生效语义（契约 §7）：下一次 Run 生效 —— SetHooks 换的是 Run 入口快照（session.SetHooks），
// 运行中的 Run 继续用旧配置收尾，与 loop 快照（session.go 的 s.loop）同语义：改配置不打断
// 正在跑的一轮，也绝不半途换配置（同一轮里 PreToolUse 用新配置、PostToolUse 用旧配置不可解释）。
//
// 返回被热替换的会话数（无运行态 = 0：没打开的项目没有会话要换；它下次打开时读新配置）。
// **不创建运行态**：打开设置页本身不得让项目"活起来"。
func (m *manager) reloadWorkspaceHooks(wsPath string) int {
	ws := m.peekRuntime(wsPath)
	if ws == nil {
		return 0
	}
	ws.mu.Lock()
	sessions := make([]*bridgeSession, 0, len(ws.sessions))
	for _, bs := range ws.sessions {
		sessions = append(sessions, bs)
	}
	ws.mu.Unlock()

	n := 0
	for _, bs := range sessions {
		if bs == nil || bs.s == nil {
			continue // nil 会话（防御）：跳过；创建中的会话可能已读旧配置，且晚于本次快照注册（窄窗口，下次创建/重开读新配置）
		}
		// 每会话一个派发器（与会话创建路径一致）：派发器的会话级去重表属于该会话。
		bs.s.SetHooks(hooksFor(m.setupHooks(wsPath, bs.warnHook), wsPath, bs.id))
		n++
	}
	if n > 0 {
		fmt.Fprintf(os.Stderr, "· hooks 配置变更已应用（下一次 Run 生效）: %s（%d 个会话）\n", wsPath, n)
	}
	return n
}

// reloadHubRuntimeHooks 把用户级 hooks 变更推给 Hub 智能体的全部会话（P4-04a B1）。
//
// 为什么必须单独一条：Hub runtime 不在 m.workspaces（刻意分开登记），项目路径的
// reloadWorkspaceHooks 永远看不到它们。而 Hub 会话只能在创建时拿一次 dispatcher，
// 不热替换就会出现「hooks_set 之后旧 Stop 还继续跑 / 新配置永不生效」。
//
// 装配链必须与创建路径**逐字同源**：setupUserHooks（空 WorkspaceDir，只读用户级）。
// 绝不能复用 reloadWorkspaceHooks —— 它走 setupHooks({hub home})，会把 Hub home 里的
// .go-code/hooks.json / .claude 项目层读进来（安全边界，见 setupUserHooks 注释）。
//
// 调用方在 m.mu 内快照 runtimes 指针、**释放 m.mu 之后**才进入本函数；本函数逐个
// runtime 取 ws.mu 快照会话（两把锁不嵌套持有，避免反向锁序）。
//
// 快照窗口：runtimes/会话在快照之后才注册的不在本次推送内。会话若在创建路径已读
// 旧配置、又在本次快照之后注册，会漏掉这次热替换（与项目路径同语义的窄窗口）；
// 它下一次重新创建/打开时读取新配置。
func (m *manager) reloadHubRuntimeHooks(runtimes []*workspaceRuntime) int {
	total := 0
	for _, ws := range runtimes {
		if ws == nil {
			continue
		}
		ws.mu.Lock()
		sessions := make([]*bridgeSession, 0, len(ws.sessions))
		for _, bs := range ws.sessions {
			sessions = append(sessions, bs)
		}
		ws.mu.Unlock()

		n := 0
		for _, bs := range sessions {
			if bs == nil || bs.s == nil {
				continue // nil 会话（防御）：跳过；创建中的会话可能已读旧配置，且晚于本次快照注册（窄窗口，下次创建/重开读新配置）
			}
			// 每会话一个派发器（与创建路径一致）：派发器的会话级去重表属于该会话。
			bs.s.SetHooks(hooksFor(m.setupUserHooks(bs.warnHook), ws.path, bs.id))
			n++
		}
		if n > 0 {
			fmt.Fprintf(os.Stderr, "· hooks 配置变更已应用（Hub，下一次 Run 生效）: %s（%d 个会话）\n", ws.path, n)
		}
		total += n
	}
	return total
}

// reloadHooksAllWorkspaces 把 hooks 变更推给**全部**已有运行态的会话（scope=user 的写盘）。
// 用户级配置对所有工作区生效，但只有"已打开"的才有会话可换；其余下次打开时自然读到新配置。
//
// 项目会话与 Hub 会话是两条装配链：前者 setupHooks（用户 + 项目层），后者 setupUserHooks
// （仅用户级）。这里在**同一把 m.mu** 内快照两边的 runtimes 指针，随即释放 m.mu；
// 之后才逐个取 ws.mu 推送（m.mu 与 ws.mu 不嵌套持有）—— 不把 Hub home 混进
// m.workspaces，也不让项目路径去读 Hub home。
func (m *manager) reloadHooksAllWorkspaces() int {
	m.mu.Lock()
	paths := make([]string, 0, len(m.workspaces))
	for p := range m.workspaces {
		paths = append(paths, p)
	}
	hubs := make([]*workspaceRuntime, 0, len(m.hubRuntimes))
	for _, ws := range m.hubRuntimes {
		if ws != nil {
			hubs = append(hubs, ws)
		}
	}
	m.mu.Unlock()
	total := 0
	for _, p := range paths {
		total += m.reloadWorkspaceHooks(p)
	}
	total += m.reloadHubRuntimeHooks(hubs)
	return total
}

// warnHook 上报 hook 异常：stderr 一行 + （会话已就绪时）ToolHookWarning 事件。
// 事件类型复用内核已有的 ToolHookWarning（events 目前不支持自定义事件类型），
// 桌面端把它渲染为一条提示，便于用户发现"某个 hook 超时了/未受信任"。
func (bs *bridgeSession) warnHook(name, message string) {
	fmt.Fprintf(os.Stderr, "· hooks: %s: %s\n", name, message)
	if bs == nil || bs.s == nil {
		return // 会话尚未创建（配置期告警）：只落 stderr
	}
	bs.emit(&events.ToolHookWarning{
		Name:      name,
		Message:   message,
		Timestamp: time.Now(),
		EventType: events.ToolHookWarningType,
	})
}
