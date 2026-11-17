package main

// plugin_runtime.go —— 第三种会话容器：插件虚拟 Project（09-agent-bridge §4 / D1、D2、D10、D11）。
//
// 一个插件 = 一个虚拟 Project：
//
//	{ virtual: true, plugin_id, name, icon,
//	  path: ~/.go-code/plugin-sessions/<id>/home,
//	  sessions: [ 普通 Session ] }
//
// 与 Hub 智能体同构（见 hub_session.go 的 hubRuntime）：**只吃全局层**（全局技能 /
// 全局子代理 / 用户级 MCP / 全局插件注册表），绝不读 {home}/ 下的项目层配置 ——
// 那是插件自己可写的目录，把项目层读进来等于「插件写配置、宿主执行」。
//
// 与 m.workspaces 分开登记（manager.pluginRuntimes，key = pluginID）：m.workspaces 的
// 所有遍历都是项目语义（workspace_list / reload_settings / MCP 重放 / cron 归档 /
// workspaces.json），虚拟 Project 混进去就会「一个插件变成一个项目」。
//
// 存储（0700，卸载不删）：
//
//	plugin-sessions/<id>/home       bash / file 工具的根（沙箱里这棵子树可写）
//	plugin-sessions/<id>/sessions   会话 jsonl + record + 子任务 journal + codemode store
//	plugin-sessions/<id>/events     事件日志（snapshot / trace 数据源）
//
// 为什么 sessions/events 不落 {appData}/sessions/<wsKey>：wsKey 是**路径**的函数
// （workspaceKey），而插件会话的归属必须跟 pluginID 走 —— 否则 home 目录改名/迁移后
// 历史会话「丢失」，而 record 键（sessionRecordKey = plugin-<id>）又还在。两套键指向不同
// 目录正是「会话删了一半」的成因，所以这里把整棵子树锚在 pluginID 上。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/mcp"
)

// pluginSessionsRootName 插件虚拟 Project 的根目录名（~/.go-code/plugin-sessions）。
const pluginSessionsRootName = "plugin-sessions"

// pluginSessionsRoot 虚拟 Project 根：{appData}/plugin-sessions。
func pluginSessionsRoot() string {
	return filepath.Join(appDataDir(), pluginSessionsRootName)
}

// pluginProjectRoot 单个插件的根（home/sessions/events 的共同父目录）。
func pluginProjectRoot(pluginID string) string {
	return filepath.Join(pluginSessionsRoot(), pluginID)
}

// pluginHomePath 插件虚拟 Project 的 home —— 命令与回执里对外的 `workspace` 就是这个路径。
func pluginHomePath(pluginID string) string {
	return filepath.Join(pluginProjectRoot(pluginID), "home")
}

// pluginSessionsDir 插件会话存储目录（record / jsonl / journal / codemode）。
func pluginSessionsDir(pluginID string) string {
	return filepath.Join(pluginProjectRoot(pluginID), "sessions")
}

// pluginEventsDir 插件事件日志目录（snapshot / trace 数据源）。
func pluginEventsDir(pluginID string) string {
	return filepath.Join(pluginProjectRoot(pluginID), "events")
}

// isPluginHomePath 该路径是否落在 plugin-sessions/ 之下（09 §4.4 的第一道闸门）。
//
// 这是**前缀判定**而不是「home 目录精确判定」：plugin-sessions/<id>/home 的父目录
// （sessions/events/项目根）同样不是工作区，任何一段漏判都会让某条按 workspace 解析的
// 路径把它当项目目录写进 workspaces.json 或落一份项目 runtime。
func isPluginHomePath(p string) bool {
	if strings.TrimSpace(p) == "" {
		return false
	}
	root := filepath.Clean(pluginSessionsRoot())
	clean := filepath.Clean(p)
	if clean == root {
		return false // 根本身不是某个插件
	}
	rel, err := filepath.Rel(root, clean)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// pluginIDFromHomePath 从虚拟 home 反解 pluginID（只认 “<root>/<id>/home“ 这一种形状）。
//
// 为什么严格：这个 id 会参与目录创建与删除（plugin-sessions/<id>），宽进就等于把
// 「任意相对路径段」交给调用方。id 还必须过 surfaceIDPattern（与安装期同一判据）。
func pluginIDFromHomePath(p string) string {
	if !isPluginHomePath(p) {
		return ""
	}
	rel, err := filepath.Rel(filepath.Clean(pluginSessionsRoot()), filepath.Clean(p))
	if err != nil {
		return ""
	}
	parts := strings.Split(rel, string(filepath.Separator))
	if len(parts) != 2 || parts[1] != "home" {
		return ""
	}
	if !surfaceIDPattern.MatchString(parts[0]) {
		return ""
	}
	return parts[0]
}

// pluginSessionDirsForHome 由虚拟 home 推出（sessions, events）目录（不查运行态）。
//
// 用途：插件**已卸载**（没有 runtime）时，历史会话仍要能列 / 读 / 删 —— 那三条路径
// 只需要目录，不需要装配技能/MCP/插件注册表。
func pluginSessionDirsForHome(p string) (storeDir, eventsDir string, ok bool) {
	id := pluginIDFromHomePath(p)
	if id == "" {
		return "", "", false
	}
	return pluginSessionsDir(id), pluginEventsDir(id), true
}

// sessionsRoot 本 runtime 的「会话子树根」：子任务 journal、codemode store、record 的父目录。
//
// 项目 / Hub 保持既有口径（{appData}/sessions/<wsKey>，见 wsSessionsDir）——Hub 的
// storeDir 虽在 <home>/sessions，但它的 journal 一直落在 wsSessionsDir，改口径会搬走
// 既有历史。插件走自己的 storeDir：虚拟 Project 的整棵子树锚在 pluginID 上（文件头）。
func (ws *workspaceRuntime) sessionsRoot() string {
	if ws == nil {
		return ""
	}
	if ws.kind == runtimePlugin {
		return ws.storeDir
	}
	return wsSessionsDir(ws.path)
}

// pluginRuntimeByPath 按虚拟 home 查**已装配**的插件运行时（nil = 未命中插件表）。
// 只查不建：m.runtime 的守卫要求「未命中就返回 nil」，装配只发生在显式入口。
func (m *manager) pluginRuntimeByPath(path string) *workspaceRuntime {
	id := pluginIDFromHomePath(path)
	if id == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pluginRuntimes[id]
}

// createPluginSession 插件虚拟 Project 的会话入口（09 §4.4：createOpts 对虚拟 home 硬拒绝）。
//
// 签名刻意与会话命令同形（workspace=虚拟 home / session_id / effort）：new_session
// 只多一个分支，不新增 plugin_session_* 命令族。
func (m *manager) createPluginSession(home, id, effort string) (*bridgeSession, error) {
	ws, err := m.pluginRuntimeForHome(home)
	if err != nil {
		return nil, err
	}
	if ws == nil {
		return nil, fmt.Errorf("%w: 插件运行时未装配", errPluginWorkspace)
	}
	return m.createIn(ws, id, effort, sessionOpts{})
}

// pluginAskDenyReason ask/ask_batch 的前置拒绝原因（"" = 允许）。
// 只对插件虚拟 home 生效：项目 / Hub 会话保持原路径，逐字不变。
func (m *manager) pluginAskDenyReason(wsPath string) string {
	if !isPluginHomePath(wsPath) {
		return ""
	}
	id := pluginIDFromHomePath(wsPath)
	if id == "" {
		return pluginReasonUninstalled
	}
	return m.pluginRunDenyReason(id)
}

// pluginRuntimeIfExists 按 pluginID 查已装配的运行时（不装配）。
func (m *manager) pluginRuntimeIfExists(pluginID string) *workspaceRuntime {
	if m == nil || pluginID == "" {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pluginRuntimes[pluginID]
}

// pluginRuntimeForHome 显式装配入口：由虚拟 home 取（必要时装配）插件运行时。
//
// 与 m.runtime 的分工是刻意的：m.runtime 是**判定**（插件路径要么命中插件表、要么 nil，
// 绝不回落到项目装配），这里是**装配**（只有确实要开会话/读历史的命令才调）。
func (m *manager) pluginRuntimeForHome(home string) (*workspaceRuntime, error) {
	id := pluginIDFromHomePath(home)
	if id == "" {
		return nil, fmt.Errorf("%w: %q 不是插件虚拟 Project 的 home", errPluginWorkspace, home)
	}
	return m.pluginRuntime(id), nil
}

// pluginRuntime 取（或首次装配）某插件的虚拟 Project 运行时。
//
// 装配内容 = 全局工具面所需的全局层（与 hubRuntime 逐项对齐）：全局技能注册表、
// 全局子代理定义、用户级 MCP（~/.go-code/settings.json）、全局插件注册表；目录为本插件
// 自己的 home/sessions/events。**不做**：项目技能层、项目 MCP 层、插件自动启用、
// 配置文件监听（watcher 按 ws.path 重读分层，而这里只有用户层，且用户层变化由
// reload_settings 统一重建）。
//
// 已卸载的插件也允许装配（插件已卸载但会话要能读）：装配只依赖磁盘上的 home 与全局层，
// 不依赖 surface 注册表。是否允许**发起新 run** 由 pluginRunDenyReason 另行判定。
func (m *manager) pluginRuntime(pluginID string) *workspaceRuntime {
	if m == nil || !surfaceIDPattern.MatchString(pluginID) {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pluginRuntimes == nil {
		m.pluginRuntimes = map[string]*workspaceRuntime{}
	}
	if ws, ok := m.pluginRuntimes[pluginID]; ok {
		return ws
	}
	home := pluginHomePath(pluginID)
	skillsReg := buildGlobalSkillsRegistry()
	subagentsReg := buildGlobalSubagentRegistry()
	// MCP 只有用户层（低 → 高优先级在这里只有一层）。项目层刻意不叠。
	mcpLoader := &mcp.Loader{}
	mcpRes := mcpLoader.Load(mcp.Layer{Path: homeCfgPath("settings.json"), Source: mcpLayerUser})
	mcpm := mcp.NewManagerWithSource(mcpRes.Config, mcpRes.Source)
	applyMCPDefaultExposure(mcpm) // codemode 段决定 deferred/direct，与项目路径同口径
	ws := &workspaceRuntime{
		kind:       runtimePlugin,
		pluginID:   pluginID,
		path:       home,
		storeDir:   pluginSessionsDir(pluginID),
		eventsDir:  pluginEventsDir(pluginID),
		skills:     skillsReg,
		subagents:  subagentsReg,
		mcp:        mcpm,
		mcpCfg:     mcpLoader,
		mcpLayers:  mcpRes.Layers,
		mcpApplied: mcpRes.Config,
		plugins:    buildPlugins(m.imPlugin),
		sessions:   map[string]*bridgeSession{},
	}
	// 0700：home/sessions/events 都可能含对话与用户文件
	_ = os.MkdirAll(ws.path, 0o700)
	_ = os.MkdirAll(ws.storeDir, 0o700)
	_ = os.MkdirAll(ws.eventsDir, 0o700)
	// MCP 低频探活：照开（与 hub 同口径，不读任何项目配置）。
	m.startMCPHealth(ws)
	m.pluginRuntimes[pluginID] = ws
	return ws
}

// closePluginRuntime 优雅关闭一个插件的虚拟 Project（停用 / 卸载 / shutdown 路径）。
//
// 与 shutdownWorkspace 同口径：先逐个 Shutdown 会话（落 checkpoint/事件、停 run goroutine）
// → 停 MCP 探活 → 关 MCP 连接 → 关插件注册表（Browser Use 子进程）。
// sessions/events/home **不删**（09 §4.1：卸载不删；会话保留为「已卸载插件」，可手动删）。
func (m *manager) closePluginRuntime(pluginID string) {
	if m == nil || pluginID == "" {
		return
	}
	m.mu.Lock()
	ws, ok := m.pluginRuntimes[pluginID]
	if ok {
		delete(m.pluginRuntimes, pluginID) // 先摘表：关停期间新命令不会路由到半死的 runtime
	}
	m.mu.Unlock()
	if !ok || ws == nil {
		return
	}
	// 插件没了 = 等待中的工具调用再也回不来（§6.4 plugin_unloaded）：立即失败，
	// 别让模型等满 manifest 的 deadline 才发现插件已经卸载。
	m.failSurfaceAgentToolCalls(pluginID, surfaceReasonPluginUnloaded, "插件已卸载或停用，工具结果不会再回来")
	ws.mu.Lock()
	items := make([]*bridgeSession, 0, len(ws.sessions))
	for _, bs := range ws.sessions {
		items = append(items, bs)
	}
	ws.sessions = map[string]*bridgeSession{}
	ws.mu.Unlock()
	for _, bs := range items {
		if bs == nil {
			continue
		}
		if bs.ctx == nil || bs.ctx.Err() != nil {
			continue // 已关闭：避免重复 SessionClosed
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = bs.s.Shutdown(ctx)
		cancel()
		bs.cancel()
	}
	ws.stopMCPHealth()
	ws.mcp.Close()
	ws.plugins.Close(context.Background())
}

// closePluginRuntimes 关闭全部插件运行时（进程退出 / 测试清理 / shutdownAll）。
func (m *manager) closePluginRuntimes() {
	if m == nil {
		return
	}
	m.mu.Lock()
	ids := make([]string, 0, len(m.pluginRuntimes))
	for id := range m.pluginRuntimes {
		ids = append(ids, id)
	}
	m.mu.Unlock()
	for _, id := range ids {
		m.closePluginRuntime(id)
	}
}

// 插件停用 / 卸载后 ask 的前置拒绝原因（封闭集合，09 §4.2）。
//
// 让 run 在 dispatch 就被挡下来而不是跑到一半：会话装配会读全局技能/MCP，跑起来再失败
// 时用户看到的是「发了消息没反应」。
const (
	pluginReasonDisabled    = "plugin_disabled"    // 已安装但用户停用
	pluginReasonUninstalled = "plugin_uninstalled" // index 里没有（卸载 / id 未知）
)

// 插件虚拟 Project **运行态未装配**时的封闭拒绝（命令路径）。
//
// 与 pluginReasonDisabled/Uninstalled 分工：那两个回答「插件还能不能用」，
// 这个回答「这条按 workspace 解析的命令现在有没有可操作的运行态」。取值刻意与
// §4.2 的 denied 一致（前端只认一套"被拒"语义），细节在 data.reason。
const (
	pluginNotReadyCode   = "denied"
	pluginNotReadyReason = "plugin_not_ready"
)

// respPluginNotReady 命令回执：ok:false, code=denied, data.reason=plugin_not_ready。
//
// 为什么是拒绝而不是「顺手装配」：这些命令（skills/mcp/skill_*）都是**读或改运行态**，
// 在只读路径里创建 runtime 会让「打开设置页」产生副作用（09 §4.1）；而造出的若是
// kind=runtimeProject 的 runtime，就更把插件 home 当成了项目（§4.4 第 1 条）。
func (m *manager) respPluginNotReady(c command) {
	m.respCode(c, false, "插件虚拟 Project 运行态未装配（重新打开插件会话后重试）",
		pluginNotReadyCode, map[string]any{"reason": pluginNotReadyReason})
}

// pluginRunDenyReason 该插件当前是否允许发起新 run（"" = 允许）。
//
// 判据必须来自 surface 注册表（安装期与启用状态的**唯一**真相源），不看内存里的 runtime：
// 停用后 runtime 可能还活着（会话要能读），但它不再是「用户正在用的插件」。
// 注册表读不出来时 fail closed —— 无法证明「已启用」就等于不允许花用户的 token。
func (m *manager) pluginRunDenyReason(pluginID string) string {
	if pluginID == "" {
		return pluginReasonUninstalled
	}
	res := m.surfaces.List(false)
	if res.Status == surfaceStatusError {
		return pluginReasonUninstalled
	}
	for _, it := range res.Items {
		if it.ID != pluginID {
			continue
		}
		if !it.Enabled {
			return pluginReasonDisabled
		}
		return ""
	}
	return pluginReasonUninstalled
}

// ---- plugin_projects_list（09 §4.1 / §5.1）----

// pluginProjectInfo 虚拟 Project 的回执形状（字段名即协议，前端直接读）。
//
// 跨任务契约（P1b1 客户端）：`plugin_projects_list` 的数组在 **data.projects**
// （`{ok:true, data:{projects:[...]}}`），且每条**恒含**这七个键 —— icon 刻意不加
// omitempty：空 icon 时键消失会让 TS 侧「字段可选」与运行时形状对不上（`p.icon` 从
// "" 变成 undefined，拼 URL 的地方会变成 "undefined"）。
type pluginProjectInfo struct {
	Path         string `json:"path"`      // 虚拟 home 绝对路径（= 命令里的 workspace）
	PluginID     string `json:"plugin_id"` // surface id
	Name         string `json:"name"`
	Icon         string `json:"icon"`
	Enabled      bool   `json:"enabled"`
	Installed    bool   `json:"installed"`
	SessionCount int    `json:"session_count"`
}

// pluginProjectsData 列出全部虚拟 Project：surface 注册表（已安装，含停用）∪
// plugin-sessions/ 下有历史会话的插件（已卸载，installed:false）。
//
// 为什么要并集：卸载后会话是「保留可读可删」的（09 §4.2），只列已安装的话那些会话就
// 变成用户看不见、也删不掉的孤儿目录；只列磁盘的话「刚装好还没聊过」的插件不会出现，
// 而左栏插件分组的「新建会话」入口正好需要它。
func (m *manager) pluginProjectsData() []pluginProjectInfo {
	byID := map[string]*pluginProjectInfo{}
	res := m.surfaces.List(false)
	if res.Status != surfaceStatusError {
		for _, it := range res.Items {
			info := &pluginProjectInfo{
				Path:      pluginHomePath(it.ID),
				PluginID:  it.ID,
				Name:      it.ID, // manifest 读不出来时至少有个名字
				Enabled:   it.Enabled,
				Installed: true,
			}
			if mf, err := m.surfaces.Manifest(it.ID); err == nil {
				if n := manifestDisplayName(mf, it.ID); n != "" {
					info.Name = n
				}
				info.Icon = mf.Icon
			}
			// 与主进程读索引同一条缺省（electron/surface-index.ts：manifest 没声明
			// 但目录里有 icon.svg 就用它）。否则「模板自带的 icon.svg」在左栏永远不出现 ——
			// 作者看到的是「我明明放了图标」。
			if strings.TrimSpace(info.Icon) == "" {
				if st, err := os.Stat(filepath.Join(m.surfaces.Dir(), it.ID, "icon.svg")); err == nil && st.Mode().IsRegular() {
					info.Icon = "icon.svg"
				}
			}
			byID[it.ID] = info
		}
	}
	// 磁盘侧：只认「真有过会话」的插件目录（home 空目录不算，否则装过又删的插件会留下幽灵）。
	entries, err := os.ReadDir(pluginSessionsRoot())
	if err == nil {
		for _, e := range entries {
			if !e.IsDir() || !surfaceIDPattern.MatchString(e.Name()) {
				continue
			}
			id := e.Name()
			n := m.pluginSessionCount(id)
			if info := byID[id]; info != nil {
				info.SessionCount = n
				continue
			}
			if n == 0 {
				continue
			}
			byID[id] = &pluginProjectInfo{
				Path:         pluginHomePath(id),
				PluginID:     id,
				Name:         id, // manifest 随卸载一起进 .trash，这里没有可读的名字
				Enabled:      false,
				Installed:    false,
				SessionCount: n,
			}
		}
	}
	out := make([]pluginProjectInfo, 0, len(byID))
	for _, info := range byID {
		out = append(out, *info)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PluginID < out[j].PluginID })
	return out
}

// pluginSessionCount 插件虚拟 Project 的会话数 = 磁盘 <sid>.jsonl ∪ 进程内会话。
//
// 为什么不是单纯数文件：新建但还没聊过的会话磁盘上还没有 jsonl（首次 Run 才落盘），
// 而它已经是一个会话 —— 与 listSessions 的「内存 + 磁盘」并集口径必须一致，否则
// 左栏显示 0 个会话、点进去却有会话（同一份数据两个说法）。
// 不递归读子目录：<sid>/ 是子任务 journal，不是会话（与 listSessions 的扫描范围同口径）。
func (m *manager) pluginSessionCount(pluginID string) int {
	seen := map[string]bool{}
	if entries, err := os.ReadDir(pluginSessionsDir(pluginID)); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".jsonl") {
				continue
			}
			seen[strings.TrimSuffix(e.Name(), ".jsonl")] = true
		}
	}
	if ws := m.pluginRuntimeIfExists(pluginID); ws != nil {
		ws.mu.Lock()
		for id := range ws.sessions {
			seen[id] = true
		}
		ws.mu.Unlock()
	}
	return len(seen)
}

// manifestDisplayName manifest.name 的一句人读名字（zh → en → 任意非空 → id）。
//
// 顺序沿用 surfaceEntryTitle.Primary 的取舍（中文优先）：插件页与 manifest 校验
// 对「哪种写法合法」只该有一份口径，但这里是**展示**，所以最后兜底到 id 而不是空串。
func manifestDisplayName(mf surfaceManifest, id string) string {
	if v := strings.TrimSpace(mf.Name["zh"]); v != "" {
		return v
	}
	if v := strings.TrimSpace(mf.Name["en"]); v != "" {
		return v
	}
	keys := make([]string, 0, len(mf.Name))
	for k := range mf.Name {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := strings.TrimSpace(mf.Name[k]); v != "" {
			return v
		}
	}
	return id
}
