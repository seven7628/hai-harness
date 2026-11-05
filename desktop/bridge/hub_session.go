package main

// Agent Hub 智能体运行时（方案 §6.1.1 / §6.1.3）。
//
// 智能体不是某个仓库：它有自己的 home（~/.go-code/hub/agents/<agentID>），
// 只吃**全局层**的技能 / 子代理 / MCP / 插件。项目级的三层（{ws}/.agents/skills、
// {ws}/.mcp.json、{ws}/.go-code/settings.json）一律不读 —— 需要项目专属能力时，
// 把任务派发到那个工作区，由工作区自己的运行时执行，而不是把项目配置复制进 Hub。
//
// 与 m.runtime(path) 分开登记（manager.hubRuntimes）：m.workspaces 的遍历都是
// 项目语义（workspace_list / reload_settings / MCP 重放 / cron 归档），智能体运行时
// 混进去会被当成「一个项目」，属跨层泄漏。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/seven7628/hai-harness/mcp"
	"github.com/seven7628/hai-harness/skills"
	"github.com/seven7628/hai-harness/subagent"
)

// sessionRecordKey 会话 record 的维度键（§6.1.3）：默认是工作区路径哈希；
// Hub 智能体固定 hub-<agentID>（不跟 home 路径走 —— 目录改名/迁移后历史会话会「丢失」）；
// sessionOpts.wsKey 最后生效（cron 派生会话用它隔离自己的存储目录）。
func sessionRecordKey(ws *workspaceRuntime, wsPath string, opts sessionOpts) string {
	key := workspaceKey(wsPath)
	if ws != nil && ws.kind == runtimeHubAgent && ws.agentID != "" {
		key = "hub-" + ws.agentID
	}
	if opts.wsKey != "" {
		key = opts.wsKey
	}
	return key
}

// hubAgentHome 智能体的虚拟工作区目录（方案 §6.1.3）：
// ~/.go-code/hub/agents/<agentID>（会话/事件/记忆/产物的共同根）。
func hubAgentHome(agentID string) string {
	return filepath.Join(appDataDir(), "hub", "agents", agentID)
}

// hubRuntime 取（或首次装配）智能体的运行时。
//
// 装配内容 = 全局工具面所需的全局层：全局技能注册表、全局子代理定义、用户级 MCP
// 配置（~/.go-code/settings.json）、全局插件注册表；目录为本智能体自己的
// sessions/events。**不做**：项目技能层、项目 MCP 层、插件自动启用、git 检测。
func (m *manager) hubRuntime(agentID string) *workspaceRuntime {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.hubRuntimes == nil {
		m.hubRuntimes = map[string]*workspaceRuntime{}
	}
	if ws, ok := m.hubRuntimes[agentID]; ok {
		return ws
	}
	home := hubAgentHome(agentID)
	// 全局技能 / 子代理：与项目 runtime 同一套分层函数，只是不传项目目录。
	skillsReg := buildGlobalSkillsRegistry()
	subagentsReg := buildGlobalSubagentRegistry()
	// MCP 只有用户层（低 → 高优先级在这里只有一层）。项目层刻意不叠。
	mcpLoader := &mcp.Loader{}
	mcpRes := mcpLoader.Load(mcp.Layer{Path: homeCfgPath("settings.json"), Source: mcpLayerUser})
	mcpm := mcp.NewManagerWithSource(mcpRes.Config, mcpRes.Source)
	applyMCPDefaultExposure(mcpm) // codemode 段决定 deferred/direct，与项目路径同口径
	ws := &workspaceRuntime{
		kind:       runtimeHubAgent,
		agentID:    agentID,
		path:       home,
		storeDir:   filepath.Join(home, "sessions"),
		eventsDir:  filepath.Join(home, "events"),
		skills:     skillsReg,
		subagents:  subagentsReg,
		mcp:        mcpm,
		mcpCfg:     mcpLoader,
		mcpLayers:  mcpRes.Layers,
		mcpApplied: mcpRes.Config,
		plugins:    buildPlugins(m.imPlugin),
		sessions:   map[string]*bridgeSession{},
	}
	_ = os.MkdirAll(ws.storeDir, 0o700) // 会话/事件目录含敏感内容，0700
	_ = os.MkdirAll(ws.eventsDir, 0o700)
	// MCP 低频探活：只 Ping 已有连接、断线重连，不读任何项目配置，可以照开。
	// （配置监听 startMCPWatch 不装：它按 ws.path 重读分层，而 Hub 只有用户层，
	// 且用户层变化由 settings 热重载统一处理，见 P1。）
	m.startMCPHealth(ws)
	m.hubRuntimes[agentID] = ws
	return ws
}

// closeHubRuntimes 关闭全部智能体运行时（进程退出 / 测试清理）。
// 与 shutdownWorkspace 同口径：停 MCP 监听/探活、关 MCP 连接、**关插件注册表**
// （buildPlugins 每次返回新 registry，不关会漏 Browser Use 子进程；
//
//	P0 阶段 hub 插件还没被 Enable，先按同口径收尾，免得 P1 打开插件后泄漏）。
func (m *manager) closeHubRuntimes() {
	m.mu.Lock()
	wss := make([]*workspaceRuntime, 0, len(m.hubRuntimes))
	for _, ws := range m.hubRuntimes {
		wss = append(wss, ws)
	}
	m.hubRuntimes = map[string]*workspaceRuntime{}
	m.mu.Unlock()
	for _, ws := range wss {
		// 与 shutdownWorkspace 同口径：**先逐个 Shutdown 会话**（落 checkpoint/事件、
		// 停 run goroutine），再停监听/探活，最后关连接。直接清空 map 会丢未落盘状态。
		// Refresh 会取 Manager 锁：这段不持 m.mu。
		ws.mu.Lock()
		items := make([]*bridgeSession, 0, len(ws.sessions))
		for _, bs := range ws.sessions {
			items = append(items, bs)
		}
		ws.sessions = map[string]*bridgeSession{}
		ws.mu.Unlock()
		for _, bs := range items {
			if bs.ctx != nil && bs.ctx.Err() != nil {
				continue // 已关闭，避免重复 SessionClosed
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if bs.s != nil {
				_ = bs.s.Shutdown(ctx)
			}
			cancel()
			if bs.cancel != nil {
				bs.cancel()
			}
			bs.runtime = nil // 不再指向已 Close 的 runtime（rebuildLoop 会按路径兜底）
		}
		ws.stopMCPWatch()
		ws.stopMCPHealth()
		if ws.mcp != nil {
			ws.mcp.Close()
		}
		if ws.plugins != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			ws.plugins.Close(ctx)
			cancel()
		}
	}
}

// buildGlobalSkillsRegistry 只扫全局层的技能注册表：
// ~/.agents/skills（主）→ ~/.go-code/skills（兼容旧路径）。
func buildGlobalSkillsRegistry() *skills.Registry {
	reg, err := skills.NewLayeredRegistry(agentSkillsDir(), globalSkillsDir())
	if err != nil {
		return reg // 目录不可读：空注册表（技能功能降级，不影响其他能力）
	}
	return reg
}

// buildGlobalSubagentRegistry 只扫全局层的子代理定义：
// ~/.claude/agents（宽松）→ ~/.go-code/agents → ~/.agents/agents。
func buildGlobalSubagentRegistry() *subagent.DefinitionRegistry {
	reg, err := subagent.NewLayeredDefinitionRegistry(
		subagent.Layer{Dir: claudeSubagentsDir(), Lenient: true},
		subagent.Layer{Dir: globalSubagentsDir()},
		subagent.Layer{Dir: globalAgentSubagentsDir()},
	)
	if err != nil {
		fmt.Fprintf(os.Stderr, "⚠ Hub 子代理定义加载失败（降级为空）: %v\n", err)
		return reg
	}
	return reg
}
