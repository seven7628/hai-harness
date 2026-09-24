package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	gomcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/seven7628/hai-harness/im"
	"github.com/seven7628/hai-harness/plugin"
	"github.com/seven7628/hai-harness/plugin/browser"
	"github.com/seven7628/hai-harness/plugin/browser/trace"
	"github.com/seven7628/hai-harness/plugin/computer"
	implugin "github.com/seven7628/hai-harness/plugin/im"
)

// pluginPinnedVersion Browser Use 内置组件（chrome-devtools-mcp）版本 —— 与
// desktop/vendor/chrome-devtools-mcp（随应用打包分发）保持一致。升级 = 换 vendor
// 目录内容 + 提升此常量 → 启动自动同步新版本。
const pluginPinnedVersion = "1.8.0"

// bundledBrowserVendorDir 内置组件源目录：Electron main spawn bridge 时经
// GO_CODE_BROWSER_VENDOR_DIR 注入（打包 = Resources/chrome-devtools-mcp；
// dev = desktop/vendor/chrome-devtools-mcp）。空 = 未注入（测试/裸跑 bridge）→
// 组件缺失时报错而非 npm 安装（2026-09 起组件随应用内置）。
func bundledBrowserVendorDir() string {
	return os.Getenv("GO_CODE_BROWSER_VENDOR_DIR")
}

// newBrowserRegistry 构造 Browser Use 插件注册表（读插件配置 + env 覆盖）。
// 配置来源：插件独立配置文件（browser.LoadConfig）；GO_CODE_BROWSER_HEADLESS env 恒优先
// （E2E/CI 无窗口运行，用户 UI 设置的 headless 不覆盖）。
func newBrowserRegistry(cfg browser.Config) *plugin.Registry {
	cfg.PinnedVersion = pluginPinnedVersion
	cfg.VendorDir = bundledBrowserVendorDir()
	if browserHeadless() {
		cfg.Headless = true
	}
	return plugin.NewRegistry(browser.New(cfg))
}

// buildPlugins 构造工作区插件注册表（v1 内建 Browser Use + Computer Use + IM 集成）。
// 每工作区一个（Browser Use 私有 MCP 连接按工作区隔离）；组件目录全局共享（安装幂等）。
// imCap 为全局 IM 单例（manager 持有）：跨 workspace 共享连接/路由；nil 时自建（测试用）。
func buildPlugins(imCap *implugin.Plugin) *plugin.Registry {
	cfg := browser.LoadConfig(browser.DefaultPluginsDir())
	cfg.PinnedVersion = pluginPinnedVersion
	cfg.VendorDir = bundledBrowserVendorDir()
	if browserHeadless() {
		cfg.Headless = true
	}
	if imCap == nil {
		imCap = implugin.New(implugin.DefaultConfigPath())
	}
	return plugin.NewRegistry(
		browser.New(cfg),
		computer.New(computer.Config{
			// helper 源目录（electron env 注入；dev=仓库 desktop/computer-helper，
			// 打包=Resources/computer-helper）→ 启动 EnsureSynced 同步到受管目录。
			HelperDir: os.Getenv("GO_CODE_COMPUTER_HELPER_DIR"),
			// Computer Use 统一授权（设置 → 插件 → Computer Use → 已授权 App）：
			// 未授权 App 的 computer_open_app/activate 拒绝（工具返回引导文案）。
			ApprovedApps: loadComputerApprovedApps(),
			// 懒刷新：门禁未命中时重读 settings.json（用户设置页加授权立即生效，
			// 无需重启 bridge——本机实证：长跑 bridge 内存快照旧导致误拒）。
			ReloadApproved: loadComputerApprovedApps,
		}),
		imCap,
	)
}

// browserHeadless 读 GO_CODE_BROWSER_HEADLESS（E2E/CI 无窗口运行；默认有头可见）。
// 值 "1"/"true"（大小写不敏感）= headless；其余 = 有头。
func browserHeadless() bool {
	v := strings.TrimSpace(os.Getenv("GO_CODE_BROWSER_HEADLESS"))
	return v == "1" || strings.EqualFold(v, "true")
}

// configProvider 可配置插件（plugin_list 据此给 UI 显示「设置」入口）。
type configProvider interface{ Configurable() bool }

// pluginSnapshot 序列化插件清单 + 状态（plugin_list 响应与 plugin_status 事件共用）。
// enabled 为运行时事实（IsEnabled）：State 的 update-available 会掩盖 enabled，
// UI 需要「运行中」事实来决定启用/禁用按钮与升级编排。
func pluginSnapshot(plg *plugin.Registry, ctx context.Context) []map[string]any {
	caps := []map[string]any{}
	for _, cap := range plg.List() {
		st := cap.State(ctx)
		_, configurable := cap.(configProvider)
		enabled := false
		if ec, ok := cap.(interface{ IsEnabled() bool }); ok {
			enabled = ec.IsEnabled()
		}
		caps = append(caps, map[string]any{
			"id": cap.ID(), "name": cap.Name(), "description": cap.Description(),
			"status": st.Status, "reason": st.Reason,
			"installed": st.Installed, "pinned": st.Pinned,
			"configurable": configurable, "enabled": enabled,
			// P0 观测契约（§3.4）：browser 插件带 mcp 连接阶段（plugin_list 与 browser_status 共用）
			"mcp": browserMCPState(cap),
			// computer 插件详情（权限状态/helper/工具数）：仅 computer 有 StatusSnapshot
			"computer": computerStatus(cap),
		})
	}
	return caps
}

// refreshComputerApprovedApps 热更新所有工作区的 Computer Use 已授权 App 集合
// （reload_settings 后调用：授权集合变更立即生效，无需重启插件/应用）。
func (m *manager) refreshComputerApprovedApps() {
	apps := loadComputerApprovedApps()
	m.mu.Lock()
	wsList := make([]*workspaceRuntime, 0, len(m.workspaces))
	for _, ws := range m.workspaces {
		wsList = append(wsList, ws)
	}
	m.mu.Unlock()
	for _, ws := range wsList {
		if cap, ok := ws.plugins.Get(computer.ID); ok {
			if cp, ok2 := cap.(interface{ SetApprovedApps(map[string]bool) }); ok2 {
				cp.SetApprovedApps(apps)
			}
		}
	}
}

// computerStatus 取 computer 插件的运行时状态快照（详情页数据源）。
// 非 computer 插件 / 无 StatusSnapshot 能力 → nil（前端忽略）。
func computerStatus(cap plugin.Capability) map[string]any {
	cp, ok := cap.(interface{ StatusSnapshot() map[string]any })
	if !ok {
		return nil
	}
	// 只有 computer 插件实现 StatusSnapshot；browser 也有同名方法但语义不同
	// （mcp 状态走 browserMCPState 字段），此处按 ID 甄别。
	if cap.ID() != computer.ID {
		return nil
	}
	return cp.StatusSnapshot()
}

// browserMCPState 取 browser 插件的 mcp 连接阶段（P0 观测契约）。
// 非 browser 插件 / 无 StatusSnapshot 能力 → nil（前端忽略）。
func browserMCPState(cap plugin.Capability) map[string]any {
	bp, ok := cap.(interface{ StatusSnapshot() map[string]any })
	if !ok {
		return nil
	}
	snap := bp.StatusSnapshot()
	mcpState, _ := snap["mcp"].(map[string]any)
	return mcpState
}

// asyncPluginOp 后台跑耗时的插件操作（npm install 可能 30-60s）。dispatch 循环单线程
// ——同步会阻塞该工作区全部命令（审批/interrupt/ask 排队）；异步跑完发 plugin_status
// 事件让 UI 刷新（插件 State 安装中返回 installing，完成回 ready/error）。
// 失败不静默：错误随事件透传（emitPluginStatusError），UI 在对应插件行下展示——
// 此前只进 stderr，前端乐观置的 installing 被状态事件冲回原状态，表现为「点了没反应」。
func (m *manager) asyncPluginOp(c command, ws, id, op string, fn func() error) {
	go func() {
		if err := fn(); err != nil {
			fmt.Fprintf(os.Stderr, "✗ plugin %s %s: %v\n", id, op, err)
			m.emitPluginStatusError(ws, id, err.Error())
			return
		}
		m.emitPluginStatus(ws)
	}()
	m.resp(c, true, "", nil)
}

// emitPluginStatusError 发带单插件错误信息的 plugin_status 事件（UI 在对应插件行下展示；
// 无错误的后续事件自然覆盖清除）。
func (m *manager) emitPluginStatusError(ws, id, msg string) {
	ctx := context.Background()
	m.out.writeLine(map[string]any{
		"event_type": "plugin_status",
		"workspace":  ws,
		"data": map[string]any{
			"plugins": pluginSnapshot(m.runtime(ws).plugins, ctx),
			"error":   map[string]any{"id": id, "message": msg},
		},
	})
}

// emitPluginStatus 发 plugin_status 事件（插件状态快照；UI 据此刷新插件 tab）。
func (m *manager) emitPluginStatus(ws string) {
	ctx := context.Background()
	m.out.writeLine(map[string]any{
		"event_type": "plugin_status",
		"workspace":  ws,
		"data":       map[string]any{"plugins": pluginSnapshot(m.runtime(ws).plugins, ctx)},
	})
}

// handlePluginCommand plugin_* 桥接命令（通用，按 id 寻址，与具体插件解耦）。
// 启用/禁用/卸载后重建本工作区会话 loop（工具集变化立即生效）。
func (m *manager) handlePluginCommand(c command) {
	ws := str(c.Payload, "workspace")
	if ws == "" {
		m.resp(c, false, "workspace 为空", nil)
		return
	}
	ctx := context.Background()
	plg := m.runtime(ws).plugins
	id := str(c.Payload, "id")

	switch c.Type {
	case "plugin_list":
		m.resp(c, true, "", map[string]any{"plugins": pluginSnapshot(plg, ctx)})

	case "plugin_install": // 受管安装 / 一键升级（异步跑 npm，不阻塞 dispatch）
		m.asyncPluginOp(c, ws, id, "install", func() error { return m.installPluginUpgrade(ctx, ws, id) })

	case "plugin_uninstall": // 移除组件（已启用先释放私有连接）
		if err := plg.Uninstall(ctx, id); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.rebuildWorkspace(ws) // 工具集可能变化
		m.resp(c, true, "", nil)

	case "plugin_enable": // 启用 → 工具注册进引擎（embedded 先确保内嵌端点，见 enablePlugin）
		// Browser 引擎互斥（2026-09）：引擎=ego 时拒绝手动启用 browser（MCP）插件，
		// 给可操作指引（切回 mcp 引擎）。ego 引擎下浏览器能力走 ego-browser skill。
		if id == browser.ID && m.getBrowserEngine() == EngineEgo {
			m.resp(c, false, "浏览器引擎当前为 ego（ego-lite）：Browser Use (MCP) 工具未注册。如需启用，请在设置中把浏览器引擎切回 MCP（browser_engine_set {engine:\"mcp\"}）", nil)
			return
		}
		if err := m.enablePlugin(ctx, ws, id); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.resp(c, true, "", nil)

	case "plugin_disable": // 禁用 → 工具从引擎摘除
		if err := plg.Disable(ctx, id); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.rebuildWorkspace(ws)
		m.resp(c, true, "", nil)
	}
}

// enablePlugin 启用插件并重建工作区工具面。browser 插件 presentation=embedded 时先确保
// 内嵌端点已启动并注入（Enable 才连内嵌视图；否则 serverArgs 无 --ws-endpoint →
// chrome-devtools-mcp 回退自启系统 Chrome）。plugin_enable 命令与升级恢复共用。
func (m *manager) enablePlugin(ctx context.Context, wsPath, id string) error {
	plg := m.runtime(wsPath).plugins
	if id == browser.ID {
		if cap, ok := plg.Get(browser.ID); ok {
			if bp, ok2 := cap.(*browser.Plugin); ok2 && bp.Config().Presentation == browser.PresentationEmbedded {
				if _, err := m.ensureBrowserEmbed(); err != nil {
					return fmt.Errorf("启动内嵌浏览器失败: %w", err)
				}
			}
		}
	}
	if _, err := plg.Enable(ctx, id); err != nil {
		return err
	}
	// IM 插件启用后装配全局事件桥（Gateway 消息/动作回调 + 会话驱动回调）
	m.attachIMBridge(wsPath)
	m.rebuildWorkspace(wsPath)
	return nil
}

// attachIMBridge 装配全局 IM 事件桥（幂等：已装配跳过）。
func (m *manager) attachIMBridge(wsPath string) {
	if m.imBridge != nil {
		return
	}
	if m.imPlugin == nil {
		return
	}
	rt := m.imPlugin.Runtime()
	if rt == nil {
		return
	}
	b := newIMBridge(rt)
	b.attachManager(m, wsPath)
	b.Attach()
	m.imBridge = b
	// 回填已有会话的 IM 桥引用（IM 插件可能晚于会话创建启用；emit 全事件漏斗转发用）
	for _, w := range m.workspaces {
		w.mu.Lock()
		for _, bs := range w.sessions {
			bs.im = b
		}
		w.mu.Unlock()
	}
}

// imBridgeMessageSink 返回把 IM 消息接到全局桥的 handler（热重建 Gateway 后重新 Attach 用）。
func (m *manager) imBridgeMessageSink() func(im.InMessage) {
	return func(msg im.InMessage) {
		if m.imBridge != nil {
			m.imBridge.OnInMessage(msg)
		}
	}
}

// imBridgeActionSink 返回把按钮回调接到全局桥的 handler。
func (m *manager) imBridgeActionSink() func(im.GatewayAction) {
	return func(a im.GatewayAction) {
		if m.imBridge != nil {
			m.imBridge.OnGatewayAction(a)
		}
	}
}

// installPluginUpgrade 受管安装 / 一键升级：已启用的插件先禁用、安装、成功后恢复启用。
// 此前直接 Registry.Install：启用态被拒（"请先禁用再重装"）且错误只进 stderr——重启后
// 插件经 autoEnablePlugins 自动恢复启用，此时点「升级」必命中该拒绝，UI 表现为闪烁后无反应。
// 编排范围为发起命令的工作区注册表（多工作区共享同一组件目录；v1 桌面单活跃工作区）。
func (m *manager) installPluginUpgrade(ctx context.Context, wsPath, id string) error {
	plg := m.runtime(wsPath).plugins
	cap, ok := plg.Get(id)
	if !ok {
		return fmt.Errorf("插件 %q 不存在", id)
	}
	// 启用判定用运行时事实（IsEnabled）：browser.Plugin 的 State 在版本落后时返回
	// update-available、掩盖 enabled——此前据此漏掉「先禁用」，Install 又被运行时检查
	// 拒绝（"插件已启用，请先禁用再重装"），升级表现为报错无效。无 IsEnabled 的插件
	// 回退 State 判定（测试 fake / 未来插件）。
	wasEnabled := false
	if ec, ok := cap.(interface{ IsEnabled() bool }); ok {
		wasEnabled = ec.IsEnabled()
	} else if st, err := plg.State(ctx, id); err == nil {
		wasEnabled = st.Status == plugin.StatusEnabled
	}
	if wasEnabled {
		if err := plg.Disable(ctx, id); err != nil {
			return fmt.Errorf("升级前禁用失败: %w", err)
		}
	}
	if err := plg.Install(ctx, id); err != nil {
		if wasEnabled { // 安装失败也要恢复可用态（旧版本组件仍在，可继续用）
			if e := m.enablePlugin(ctx, wsPath, id); e != nil {
				fmt.Fprintf(os.Stderr, "✗ 安装失败后重启用 %s: %v\n", id, e)
			}
		}
		return err
	}
	if wasEnabled {
		if err := m.enablePlugin(ctx, wsPath, id); err != nil {
			return fmt.Errorf("组件已更新到新版本，但重新启用失败: %w", err)
		}
	}
	return nil
}

// browserExt 浏览器插件扩展能力（duck typing：bridge 从 Registry 取 browser 能力后类型断言，
// 不走 Capability 接口——这是浏览器专用能力，未来其他插件有自己的扩展接口）。
type browserExt interface {
	CallRaw(ctx context.Context, tool string, args map[string]any) (*gomcp.CallToolResult, error)
}

// browserCmdTrace 记一条 browser_* 命令的耗时（诊断「全工具慢」用；全量记录）。
func browserCmdTrace(cmd string, start time.Time) {
	trace.Phase("bridge."+cmd, start)
}

// browserExtRefresh 可选的连接刷新能力（transport-closed 重连，§15.1）。
type browserExtRefresh interface {
	RefreshConn(ctx context.Context) error
}

// isTransportClosed 判断 MCP 传输断开错误（§15.1，与 wrapper 一致）。
func isTransportClosedErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "transport closed") ||
		strings.Contains(msg, "connection closed") ||
		strings.Contains(msg, "context canceled")
}

// handleBrowserCommand browser_* 桥接命令（web tab 数据源：状态/页列表/截图）。
// 走浏览器插件私有连接，与用户 MCP tab 无关。
func (m *manager) handleBrowserCommand(c command) {
	ws := str(c.Payload, "workspace")
	if ws == "" {
		m.resp(c, false, "workspace 为空", nil)
		return
	}

	// 配置读写不需要浏览器连接（插件级，不依赖启用状态）——先于 browserExt 断言处理。
	switch c.Type {
	case "browser_config_get": // 当前有效配置（默认 + 覆盖；插件详情设置表单数据源）
		trStart := time.Now()
		defer func() { browserCmdTrace("browser_config_get", trStart) }()
		cfg := browser.LoadConfig(browser.DefaultPluginsDir())
		m.resp(c, true, "", map[string]any{"config": browserConfigToMap(cfg)})
		return

	case "browser_config_set": // 保存并热应用插件配置（校验失败 → 拒绝，配置不变）
		trStart := time.Now()
		defer func() { browserCmdTrace("browser_config_set", trStart) }()
		cfg, err := browserConfigFromPayload(c.Payload)
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		if err := browser.SaveConfig(browser.DefaultPluginsDir(), cfg); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.applyBrowserConfig(cfg)
		m.resp(c, true, "", nil)
		return
	}

	ctx := context.Background()
	plg := m.runtime(ws).plugins
	cap, ok := plg.Get(browser.ID)
	if !ok {
		m.resp(c, false, "browser 插件不存在", nil)
		return
	}
	bp, ok := cap.(browserExt)
	if !ok {
		m.resp(c, false, "browser 插件不支持原始调用", nil)
		return
	}

	switch c.Type {
	case "browser_status": // 插件状态 + 分层运行时状态（P0 观测契约 §3.4）
		trStart := time.Now()
		defer func() { browserCmdTrace("browser_status", trStart) }()
		st := cap.State(ctx)
		enabled := false
		if ec, ok := cap.(interface{ IsEnabled() bool }); ok {
			enabled = ec.IsEnabled()
		}
		installed := st.Status != plugin.StatusNotInstalled && st.Status != plugin.StatusInstalling
		// 分层：mcp 连接状态来自插件 StatusSnapshot（私有 Manager），保留旧 status/reason/tool_count
		snap := map[string]any{}
		if bp, ok := cap.(interface{ StatusSnapshot() map[string]any }); ok {
			snap = bp.StatusSnapshot()
		}
		data := map[string]any{
			"status": st.Status, "reason": st.Reason,
			"tool_count": len(plg.EnabledTools(ctx)),
			"enabled":    enabled, "installed": installed,
			"mcp": snap["mcp"],
			// Browser Use 不依赖 Provider key（required:false）；key 配置状态由前端
			// providerKeys（refreshProviderKeys 真实来源）单独呈现，不在此硬编码。
			"provider": map[string]any{"required": false},
		}
		m.resp(c, true, "", data)

	case "browser_pages": // 当前打开的页面列表（structuredContent.pages）
		trStart := time.Now()
		defer func() { browserCmdTrace("browser_pages", trStart) }()
		res, err := bp.CallRaw(ctx, "list_pages", nil)
		if err != nil && isTransportClosedErr(err) {
			if rf, ok := bp.(browserExtRefresh); ok {
				if rerr := rf.RefreshConn(ctx); rerr == nil {
					res, err = bp.CallRaw(ctx, "list_pages", nil)
				}
			}
		}
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		m.resp(c, true, "", map[string]any{"pages": extractPages(res)})

	case "browser_screenshot": // 指定/当前页截图（base64 → data URL，web tab 直接 img src）
		trStart := time.Now()
		defer func() { browserCmdTrace("browser_screenshot", trStart) }()
		args := map[string]any{}
		if n, ok := int64Val(c.Payload["page_id"]); ok {
			args["pageId"] = n
		}
		res, err := bp.CallRaw(ctx, "take_screenshot", args)
		if err != nil && isTransportClosedErr(err) {
			if rf, ok := bp.(browserExtRefresh); ok {
				if rerr := rf.RefreshConn(ctx); rerr == nil {
					res, err = bp.CallRaw(ctx, "take_screenshot", args)
				}
			}
		}
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		data, mime, ok := extractImage(res)
		if !ok {
			// 服务器返回结果但无图片（如 page-id-routing 下缺 pageId）→ 透出服务器文本便于诊断
			if msg := textContent(res); msg != "" {
				m.resp(c, false, "截图失败: "+msg, nil)
				return
			}
			m.resp(c, false, "截图无图片内容", nil)
			return
		}
		m.resp(c, true, "", map[string]any{"image": "data:" + mime + ";base64," + data})

	case "browser_close": // 关闭指定页面（web tab 页列表每页的 ✕；关闭成功顺手刷新页列表）
		trStart := time.Now()
		defer func() { browserCmdTrace("browser_close", trStart) }()
		args := map[string]any{}
		if n, ok := int64Val(c.Payload["page_id"]); ok {
			args["pageId"] = n
		}
		if _, err := bp.CallRaw(ctx, "close_page", args); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		if res, err := bp.CallRaw(ctx, "list_pages", nil); err == nil {
			m.resp(c, true, "", map[string]any{"pages": extractPages(res)})
			return
		}
		m.resp(c, true, "", nil)

	case "browser_embed_start": // M2：起内嵌转发器 + ws 端点（enable 时调用，无视图，不可见）
		trStart := time.Now()
		defer func() { browserCmdTrace("browser_embed_start", trStart) }()
		target := str(c.Payload, "target_id")
		if target == "" {
			target = "go-code-embed"
		}
		endpoint, err := m.embedStart(target)
		if err != nil {
			m.resp(c, false, "启动嵌入式浏览器失败: "+err.Error(), nil)
			return
		}
		m.applyEmbedEndpoint(endpoint) // 注入 ws 端点 → embedded 模式 chrome-devtools-mcp 连它
		m.resp(c, true, "", map[string]any{"endpoint": endpoint})

	case "browser_embed_attach": // M2：把内嵌视图 debugger 代理 attach 到转发器（真正使用时）
		trStart := time.Now()
		defer func() { browserCmdTrace("browser_embed_attach", trStart) }()
		addr := str(c.Payload, "debugger_addr")
		if addr == "" {
			m.resp(c, false, "debugger_addr 为空", nil)
			return
		}
		if err := m.embedAttach(addr); err != nil {
			m.resp(c, false, "attach 内嵌视图失败: "+err.Error(), nil)
			return
		}
		m.resp(c, true, "", nil)

	case "browser_embed_stop": // M2：停止内嵌转发器
		trStart := time.Now()
		defer func() { browserCmdTrace("browser_embed_stop", trStart) }()
		m.embedStop()
		m.resp(c, true, "", nil)
	}
}

// applyBrowserConfig 热应用浏览器配置：逐工作区重建插件注册表（释放旧连接 → 换入新配置），
// 原先已启用则自动重新启用，再重建会话 loop（工具集按新配置注册）。参照 mcp_set 热应用。
func (m *manager) applyBrowserConfig(cfg browser.Config) {
	ctx := context.Background()
	m.mu.Lock()
	wsList := make([]*workspaceRuntime, 0, len(m.workspaces))
	for _, ws := range m.workspaces {
		wsList = append(wsList, ws)
	}
	m.mu.Unlock()
	for _, ws := range wsList {
		prev := ws.plugins
		wasEnabled := false
		if st, err := prev.State(ctx, browser.ID); err == nil {
			wasEnabled = st.Status == plugin.StatusEnabled
		}
		prev.Close(ctx) // 释放旧私有连接（浏览器子进程清理）
		reg := newBrowserRegistry(cfg)
		// 新注册表丢失运行时注入的 EmbedEndpoint（json:"-" 不持久化）：embedded 且需重启用前
		// 重新注入当前端点，否则 serverArgs 无 --ws-endpoint → chrome-devtools-mcp 回退自启系统 Chrome。
		if wasEnabled && cfg.Presentation == browser.PresentationEmbedded {
			if ep, err := m.embedStart("go-code-embed"); err == nil {
				if cap, ok := reg.Get(browser.ID); ok {
					if bp, ok2 := cap.(*browser.Plugin); ok2 {
						bp.SetEmbedEndpoint(ep)
					}
				}
			}
		}
		ws.mu.Lock()
		ws.plugins = reg
		ws.mu.Unlock()
		if wasEnabled {
			if _, err := reg.Enable(ctx, browser.ID); err != nil {
				fmt.Fprintf(os.Stderr, "✗ 热应用 browser 配置后重启用失败: %v\n", err)
			}
		}
		m.rebuildWorkspace(ws.path)
	}
}

// browserConfigToMap 序列化有效配置（browser_config_get 响应；布尔/枚举均为解析后值）。
func browserConfigToMap(cfg browser.Config) map[string]any {
	return map[string]any{
		"session_mode":           cfg.SessionMode,
		"presentation":           cfg.Presentation,
		"headless":               cfg.Headless,
		"viewport":               cfg.Viewport,
		"channel":                cfg.Channel,
		"redact_network_headers": cfg.Redact(),
		"usage_statistics":       cfg.UsageStatistics,
		"url_mode":               cfg.URLMode,
		"url_patterns":           cfg.URLPatterns,
		"accept_insecure_certs":  cfg.AcceptInsecureCerts,
		"user_data_dir":          cfg.UserDataDir,
	}
}

// browserConfigFromPayload 解析并校验 browser_config_set payload（非法 → 返回错误拒绝）。
func browserConfigFromPayload(m map[string]any) (browser.Config, error) {
	cfg := browser.DefaultConfig()
	cfg.SessionMode = str(m, "session_mode")
	cfg.Presentation = str(m, "presentation")
	cfg.Headless = boolVal(m["headless"])
	cfg.Viewport = str(m, "viewport")
	cfg.Channel = str(m, "channel")
	if v, ok := m["redact_network_headers"].(bool); ok {
		cfg.RedactNetworkHeaders = &v
	}
	cfg.UsageStatistics = boolVal(m["usage_statistics"])
	cfg.URLMode = str(m, "url_mode")
	cfg.URLPatterns = stringSliceVal(m["url_patterns"])
	cfg.AcceptInsecureCerts = boolVal(m["accept_insecure_certs"])
	cfg.UserDataDir = str(m, "user_data_dir")
	return cfg.Validate()
}

// stringSliceVal payload 字符串数组统一解析（JSON 解码 → []any；Go 测试/直传 → []string）。
func stringSliceVal(v any) []string {
	if s, ok := v.([]string); ok {
		return s
	}
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// textContent 取结果首个文本内容（错误诊断透出）。
func textContent(res *gomcp.CallToolResult) string {
	if res == nil {
		return ""
	}
	for _, c := range res.Content {
		if tc, ok := c.(gomcp.TextContent); ok && tc.Text != "" {
			return tc.Text
		}
	}
	return ""
}

// extractPages 从 list_pages 结果提取结构化页面列表（StructuredContent.pages，含 id/url/title/selected）。
func extractPages(res *gomcp.CallToolResult) []map[string]any {
	if res == nil || res.StructuredContent == nil {
		return nil
	}
	m, ok := res.StructuredContent.(map[string]any)
	if !ok {
		return nil
	}
	raw, ok := m["pages"].([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(raw))
	for _, p := range raw {
		if pm, ok := p.(map[string]any); ok {
			out = append(out, pm)
		}
	}
	return out
}

// extractImage 从 take_screenshot 结果提取图片 base64 + mime。
func extractImage(res *gomcp.CallToolResult) (data, mime string, ok bool) {
	if res == nil {
		return "", "", false
	}
	for _, c := range res.Content {
		if ic, isImg := c.(gomcp.ImageContent); isImg && ic.Data != "" {
			return ic.Data, ic.MIMEType, true
		}
	}
	return "", "", false
}
