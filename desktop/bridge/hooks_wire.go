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
	cfg, warnings := hooks.Load(hooks.LoadOptions{WorkspaceDir: wsPath})
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
			continue // 会话正在创建/已关闭：创建路径自己会读到新配置
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

// reloadHooksAllWorkspaces 把 hooks 变更推给**全部**已有运行态的工作区（scope=user 的写盘）。
// 用户级配置对所有工作区生效，但只有"已打开"的才有会话可换；其余下次打开时自然读到新配置。
func (m *manager) reloadHooksAllWorkspaces() int {
	m.mu.Lock()
	paths := make([]string, 0, len(m.workspaces))
	for p := range m.workspaces {
		paths = append(paths, p)
	}
	m.mu.Unlock()
	total := 0
	for _, p := range paths {
		total += m.reloadWorkspaceHooks(p)
	}
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
