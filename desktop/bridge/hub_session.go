package main

// Agent Hub 智能体运行时（方案 §6.1.1 / §6.1.3）。
//
// 智能体不是某个仓库：它有自己的 home（~/.go-code/hub/homes/<agentID>），
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
	"math"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/mcp"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/skills"
	"github.com/seven7628/hai-harness/subagent"
	"github.com/seven7628/hai-harness/tools"
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
// ~/.go-code/hub/homes/<agentID>（会话/事件/产物/工作文件的共同根，沙箱里这棵子树
// 可读可写）。
//
// 为什么不再用 hub/agents/<id>：那里放的是 profile.json（含 policy），而沙箱必须把
// home 整棵子树放开给 bash —— 同一个目录就等于让智能体**改自己的 policy 与工具开关**，
// 违反 §9.1「智能体不能修改自己的 policy」。可写区与策略存储因此物理分开：
// homes/<id>（智能体可写） vs agents/<id>（bridge 独占写）。
func hubAgentHome(agentID string) string {
	return filepath.Join(appDataDir(), "hub", "homes", agentID)
}

// hubAgentStoreDir 智能体的策略/档案目录（FileStore 的 agents/<id>）：
// ~/.go-code/hub/agents/<agentID>（profile.json / groups 引用）。
// 刻意**不**进沙箱的 writable 区（见 hubAgentHome）。
func hubAgentStoreDir(agentID string) string {
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

// hubRuntimeIfExists 按 agentID 查**已装配**的运行时，没有就返回 nil（不创建）。
// 用途：hub_open 只是读索引/看历史，不该为了一个 exists 字段去扫技能、连 MCP。
func (m *manager) hubRuntimeIfExists(agentID string) *workspaceRuntime {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.hubRuntimes[agentID]
}

// hubRuntimeByPath 按 home 路径查已装配的智能体运行时（nil = 不是 hub home）。
// 只查不建：调用方（如 m.get）在未命中时必须走项目路径的既有语义。
func (m *manager) hubRuntimeByPath(path string) *workspaceRuntime {
	if strings.TrimSpace(path) == "" {
		return nil
	}
	clean := filepath.Clean(path)
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, ws := range m.hubRuntimes {
		if ws != nil && filepath.Clean(ws.path) == clean {
			return ws
		}
	}
	return nil
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

// hubLoopOptions 智能体 loop 的装配选项（方案 §6.2.1）。
//
// 关键点：
//   - WithBaseSystemPrompt("")：必须**显式**传。不传的话内核会用 DefaultSystemPrompt
//     补成 L0（agents/agent_loop.go 的默认值），等于 D17 没落地。
//   - WithSystemPrompt：只有 L1 协议 + L2 身份（agenthub.RenderSystem）；L3–L5 由内核
//     （AgentMD/记忆/技能）追加，顺序不变。
//   - provider/model 全部显式：不读全局 active，A=DeepSeek、B=Anthropic 同时跑不串。
//   - 采样默认走 samplingDefaultsFor(显式 provider)，不用读 active 的 samplingDefaults()。
//
// 抽成 options 而不是直接建 loop：测试要注入 mock provider 才能看到最终发出去的
// system 字节（这一层没有可读的导出 getter）。
func (m *manager) hubLoopOptions(ws *workspaceRuntime, sid string, spec *hubSessionSpec, eng *tools.Engine) []agents.Option {
	p := spec.Agent
	effort := string(p.Effort)
	if effort == "" {
		effort = string(provider.ReasoningEffortLevelLow)
	}
	threshold := p.CompressThreshold
	if threshold == 0 {
		threshold = agenthub.DefaultCompressThreshold
	}
	// 装配层再钳一次 [0.5, 0.95]（与 agenthub.AgentProfile.Normalize 同口径）：
	// 走 Store/CRUD 的路径已经 Normalize 过，但手工 spec（测试/恢复快照）能绕过那层，
	// 0.97 这种值会让每轮都压缩（成本爆炸），0.1 会把上下文压穿（P1-02 复核）。
	threshold = math.Min(agenthub.MaxCompressThreshold, math.Max(agenthub.MinCompressThreshold, threshold))
	systemPrompt := agenthub.RenderSystem(agenthub.PromptSpec{Agent: &spec.Agent, Group: spec.Group, Members: spec.Members})
	sameProvider := m.provCfg.newProvider(p.Provider)
	opts := []agents.Option{
		agents.WithProvider(sameProvider),
		agents.WithProviderName(p.Provider),
		agents.WithModel(p.Model),
		agents.WithToolEngine(eng),
		agents.WithBaseSystemPrompt(""), // L0 关闭（D17）：显式空串，否则内核补 DefaultSystemPrompt
		agents.WithSystemPrompt(systemPrompt),
		agents.WithReasoningEffort(provider.ReasoningEffortLevel(effort)),
		agents.WithWorkingDir(ws.path),
		agents.WithAgentMDDir(ws.path), // L3：智能体 home 里的 AGENTS.md（可选）
		agents.WithContextWindow(m.provCfg.windowForIn(p.Provider, p.Model)),
		agents.WithMaxTokens(m.provCfg.maxTokensForIn(p.Provider, p.Model)),
		agents.WithCompressThreshold(threshold),
		agents.WithCompressor(agents.NewLLMCompressor(sameProvider, p.Model,
			compressTorOptions(p.Provider, p.Model, sid, m.provCfg)...)),
		agents.WithSessionID(sid),
		agents.WithLLMTimeout(0), // 不限整轮总时限（首字/停滞预算仍由内核默认兜底）
		agents.WithMaxRetries(11),
		agents.WithRetryBackoff(agents.FixedBackoff(time.Second)),
		agents.WithCompactBackoff(3 * time.Minute),
		agents.WithSkills(ws.skills), // L5：全局层技能清单
		agents.WithSkillCapabilities(skillCapabilitiesFor(m.provCfg, p.Provider, p.Model, eng)),
		agents.WithSubagents(ws.subagents),
	}
	if spec.MemoryInjection != "" {
		opts = append(opts, agents.WithMemoryInjection(spec.MemoryInjection)) // L4：记忆索引
	}
	if temperature, topP := m.provCfg.samplingDefaultsFor(p.Provider); temperature != nil && topP != nil {
		opts = append(opts, agents.WithTemperature(*temperature), agents.WithTopP(*topP))
	}
	return opts
}

// buildHubLoop 按 Profile 装配智能体的 AgentLoop。
func (m *manager) buildHubLoop(ws *workspaceRuntime, sid string, spec *hubSessionSpec, eng *tools.Engine) *agents.AgentLoop {
	return agents.NewAgentLoop(m.hubLoopOptions(ws, sid, spec, eng)...)
}

// hubSessionID 确定性会话 ID（方案 §6.1.4）：重启后直接按同一个 ID 冷恢复，
// 不需要映射表；带 agentID 前缀保证 mesh/日志/指标不会串。
func hubSessionID(agentID string, scope agenthub.Scope) string {
	if scope.Kind == agenthub.ScopeDM || scope.Kind == "" {
		return "hub." + agentID + ".dm"
	}
	return "hub." + agentID + ".g." + string(scope.GroupID)
}

// hubMemoryInjection 装配期算一次记忆索引（空 = 无记忆/未开启）。
// 与项目会话同一条通道（newMemoryHost），只是工作区换成智能体 home ——
// 记忆是 Profile 级（DM 与所有群共享，§6.9）。
func (m *manager) hubMemoryInjection(home string) string {
	mctx, cancel := context.WithTimeout(context.Background(), memoryResolveTimeout)
	defer cancel()
	return newMemoryHost(m.memoryConfig()).injection(mctx, home)
}

// hubSpecFor 组装一次 Hub 会话的装配输入（Profile + scope + 记忆索引）。
func (m *manager) hubSpecFor(p *agenthub.AgentProfile, scope agenthub.Scope, home string) *hubSessionSpec {
	return &hubSessionSpec{
		Agent:           *p,
		Scope:           scope,
		MemoryInjection: m.hubMemoryInjection(home),
	}
}

// hubDMSession 取或首次创建该智能体的 DM 会话（§6.4「只有一个 Session」）。
//
// 懒创建：第一次发消息/打开 DM 才装配。进程内已有 → 直接复用（不重建 loop、不丢内存态）；
// 进程重启后按确定性 sid 冷恢复（createIn 的 record 恢复路径）。
// 并发：同一 agent 的两个 hub_open 可能同时进来，用 ws.sessions 的既有幂等兜底
// （createIn 内部 ws.mu 下二次查表，后到者复用先到的实例）。
func (m *manager) hubDMSession(agentID string) (*bridgeSession, error) {
	st, err := m.hubStore()
	if err != nil {
		return nil, err
	}
	p, err := st.LoadAgent(context.Background(), agenthub.AgentID(agentID))
	if err != nil {
		return nil, fmt.Errorf("读取智能体失败: %w", err)
	}
	if p.Deleted {
		return nil, fmt.Errorf("智能体已删除")
	}
	ws := m.hubRuntime(agentID)
	sid := hubSessionID(agentID, agenthub.Scope{Kind: agenthub.ScopeDM})
	effort := string(p.Effort)
	if effort == "" {
		effort = string(provider.ReasoningEffortLevelLow)
	}
	spec := m.hubSpecFor(p, agenthub.Scope{Kind: agenthub.ScopeDM}, ws.path)
	bs, err := m.createIn(ws, sid, effort, sessionOpts{hub: spec})
	if err != nil {
		return nil, err
	}
	return bs, nil
}

// appendHubReply 把本轮最终回复追加进 DM 索引并发 hub_message（§6.4 第 5 步）。
//
// 只处理 DM 作用域：群组消息由群路由统一落账（P3），这里不能抢。
// 空回复（纯工具轮/中断）不落索引 —— 左栏预览不该出现空气泡。
func (m *manager) appendHubReply(spec *hubSessionSpec, content string) {
	if spec == nil || spec.Scope.Kind != agenthub.ScopeDM {
		return
	}
	text := strings.TrimSpace(content)
	if text == "" {
		return
	}
	st, err := m.hubStore()
	if err != nil {
		return
	}
	agentID := spec.Agent.ID
	msg := &agenthub.Message{
		Kind:   agenthub.KindText,
		Sender: agenthub.Sender{Kind: agenthub.SenderAgent, AgentID: agentID},
		Text:   text,
	}
	seq, err := st.AppendDirectMessage(context.Background(), agentID, msg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[hub] append dm reply %s: %v\n", agentID, err)
		return
	}
	m.emitHubMessage(string(agentID), seq, msg, "")
}

// emitHubMessage DM 索引新增一条消息（前端据此回显/替换乐观气泡）。
func (m *manager) emitHubMessage(agentID string, seq int64, msg *agenthub.Message, clientMsgID string) {
	if m.out == nil {
		return
	}
	line := map[string]any{
		"event_type": "hub_message",
		"agent_id":   agentID,
		"seq":        seq,
		"message":    msg,
	}
	if clientMsgID != "" {
		line["client_msg_id"] = clientMsgID
	}
	m.out.writeLine(line)
}
