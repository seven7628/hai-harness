package browser

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mark3labs/mcp-go/client/transport"
	gomcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/seven7628/hai-harness/mcp"
	"github.com/seven7628/hai-harness/plugin"
	"github.com/seven7628/hai-harness/tools"
)

// Config Browser Use 插件配置。用户可调字段持久化到插件配置文件
// （<pluginsDir>/browser/config.json，见 config.go）；默认值/校验/读写见 config.go。
// 测试通过注入项（RunCmd/LookPath/NodeCheck/TransportFor）避免依赖真实 npm/node/chrome 环境。
type Config struct {
	// SessionMode 会话模式（登录策略）：persistent（默认，持久 profile）| isolated（临时无痕）
	// | existing（连已运行 Chrome，--auto-connect）。
	SessionMode string `json:"session_mode"`
	// Headless false = 有头可见（真实 Chrome 窗口，用户可干预；默认）。
	Headless bool `json:"headless"`
	// Viewport 视口（如 "1280x720"；默认）。
	Viewport string `json:"viewport"`
	// Channel Chrome 渠道：空（系统默认 stable）| stable | canary | dev | beta。
	Channel string `json:"channel"`
	// RedactNetworkHeaders 敏感网络头脱敏（nil = 默认 true，隐私友好）。
	RedactNetworkHeaders *bool `json:"redact_network_headers"`
	// UsageStatistics 发送使用统计（默认 false = 不发，隐私友好；true 才发）。
	UsageStatistics bool `json:"usage_statistics"`
	// URLMode URL 限制模式：none（默认）| block（拦截）| allow（仅允许）；block/allow 互斥。
	URLMode string `json:"url_mode"`
	// URLPatterns URL 允许/拦截模式（URLPattern 语法；URLMode=none 时忽略）。
	URLPatterns []string `json:"url_patterns"`
	// AcceptInsecureCerts 忽略自签名/过期证书错误（慎用；默认关）。
	AcceptInsecureCerts bool `json:"accept_insecure_certs"`
	// UserDataDir 浏览器 profile 目录（持久登录）。空 = 受管默认 ~/.go-code/browser-profile。
	UserDataDir string `json:"user_data_dir"`
	// Presentation 渲染位置：window=独立浏览器窗口（默认）；embedded=右侧 web tab 内嵌视图。
	Presentation string `json:"presentation"`
	// EmbedEndpoint presentation=embedded 时的浏览器级 CDP ws 端点（由 go-code bridge 的
	// CDP 多路转发器暴露内嵌视图；运行时注入，非持久化用户配置）。
	EmbedEndpoint string `json:"-"`

	// PluginsDir 受管组件根目录（组件在 <PluginsDir>/browser）。默认 ~/.go-code/plugins。
	PluginsDir string `json:"-"`
	// PinnedVersion 内置版本（组件随应用打包，版本由 vendor 决定；manifest 记录用于
	// 校验/NodePath 追溯，不再是「npm 钉死版本」）。
	PinnedVersion string `json:"-"`
	// VendorDir 内置组件源目录（vendored chrome-devtools-mcp，随应用分发）。
	// 空 = 无内置源（测试/旧装配）：组件缺失时 State 返回 error（不再 npm 安装）。
	VendorDir string `json:"-"`
	// AutoConnect 连接已运行的 Chrome（existing 模式）——legacy 等价字段：session_mode=existing
	// 即 emit --auto-connect；保留供现有代码/测试直配。
	AutoConnect bool `json:"-"`

	// RunCmd 执行 npm 安装（测试注入；nil = exec.CommandContext）。
	RunCmd func(ctx context.Context, name string, args []string, dir string) error `json:"-"`
	// LookPath 定位 npm/node（测试注入；nil = exec.LookPath）。
	LookPath func(name string) (string, error) `json:"-"`
	// NodeCheck 校验 node 可用（测试注入；nil = 默认）。
	NodeCheck func(nodePath string) error `json:"-"`
	// TransportFor 私有 MCP 连接传输构造器（测试注入 in-process；nil = 按配置 stdio 子进程）。
	TransportFor func(cfg mcp.ServerConfig) (transport.Interface, error) `json:"-"`
}

// Plugin Browser Use 能力实现（实现 plugin.Capability）。启用后持有私有 mcp.Manager
// 与缓存工具面；生命周期由 Registry 协调（工作区 shutdown → Close 级联 Disable）。
type Plugin struct {
	cfg Config

	// managedProfile UserDataDir 为受管默认（New 缺省解析）；不可读写时自动移走重建。
	managedProfile bool

	mu    sync.Mutex
	mcpm  *mcp.Manager // 启用后持有（私有连接；nil = 未启用）
	tools []tools.Tool
	// installing 受管安装进行中（异步跑 npm 时 State 返回 installing，UI 显示过渡态）。
	installing bool
}

// New 构造 Browser Use 插件（PluginsDir/UserDataDir 缺省解析到主目录；用户字段回默认）。
func New(cfg Config) *Plugin {
	cfg = cfg.applyDefaults()
	// legacy：AutoConnect=true 等价 existing 模式（现有代码/测试直配字段）
	if cfg.AutoConnect {
		cfg.SessionMode = SessionExisting
	}
	managed := cfg.UserDataDir == ""
	if cfg.PluginsDir == "" {
		cfg.PluginsDir = DefaultPluginsDir()
	}
	if cfg.UserDataDir == "" {
		cfg.UserDataDir = filepath.Join(defaultUserHomeDir(), ".go-code", "browser-profile")
	}
	return &Plugin{cfg: cfg, managedProfile: managed}
}

func (p *Plugin) ID() string          { return ID }
func (p *Plugin) Name() string        { return Name }
func (p *Plugin) Description() string { return Description }

// componentDir 本插件受管组件目录。
func (p *Plugin) componentDir() string { return componentDir(p.cfg.PluginsDir) }

// lookPath 定位可执行文件（测试注入）。
func (p *Plugin) lookPath(name string) (string, error) {
	if p.cfg.LookPath != nil {
		return p.cfg.LookPath(name)
	}
	return defaultLookPath(name)
}

// checkNode 校验 node 可用（测试注入）。
func (p *Plugin) checkNode(nodePath string) error {
	if p.cfg.NodeCheck != nil {
		return p.cfg.NodeCheck(nodePath)
	}
	return defaultNodeCheck(nodePath)
}

// resolveNodePath 解析当前可用 node 路径：记录路径可用则沿用；缺失/失效
// （同步留空或换机/nvm 切版本/PATH 重排）→ PATH（测试注入优先）→ 常见安装目录
// 兜底（GUI 启动的 bridge PATH 不含用户 shell 配置，兜底候选目录避免「明明装了却
// 报没装」）。都不可用返回错误。返回的路径恒非空（供 serverCfg.Command 使用）。
func (p *Plugin) resolveNodePath(recorded string) (string, error) {
	if strings.TrimSpace(recorded) != "" {
		if err := p.checkNode(recorded); err == nil {
			return recorded, nil
		}
	}
	// PATH 解析：注入的 LookPath 优先（测试），否则真实 exec.LookPath
	if p.cfg.LookPath != nil {
		if np, err := p.cfg.LookPath("node"); err == nil {
			return np, nil
		}
	} else if np, err := defaultLookPath("node"); err == nil {
		return np, nil
	}
	// PATH 上没有（GUI 启动残缺 PATH）→ 候选目录兜底（Homebrew/官网 pkg/nvm/fnm/volta）
	if np := findNodeCandidate(nodeHome()); np != "" {
		return np, nil
	}
	return "", fmt.Errorf("未找到 node，请先安装 Node.js")
}

// run 执行命令（测试注入）。
func (p *Plugin) run(ctx context.Context, name string, args []string, dir string) error {
	if p.cfg.RunCmd != nil {
		return p.cfg.RunCmd(ctx, name, args, dir)
	}
	return runCmd(ctx, name, args, dir)
}

// enabled 私有连接是否已建立（持锁调用）。
func (p *Plugin) enabled() bool { return p.mcpm != nil }

// IsEnabled 运行时是否已启用（私有连接存活）。与 State 不同：State 在版本落后时
// 返回 update-available、掩盖 enabled——升级编排与 UI 需要「运行中」的真实事实时用此方法。
func (p *Plugin) IsEnabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.enabled()
}

// State 计算当前状态（从 fs / 运行时，只读）：组件缺失/版本不符（且无 vendor 可同步）
// → error；node 缺失 → error（就绪前唯一外部依赖）；就绪 → 已启用(enabled) / 禁用(ready)。
// 2026-09 起组件随应用内置：不再有「未安装（需手动装）/安装中/可升级」态——
// 启动时 bridge 调 EnsureSynced 从 vendor 同步，此处只反映同步后的真实状态。
func (p *Plugin) State(_ context.Context) plugin.State {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.installing {
		return plugin.State{Status: plugin.StatusInstalling, Pinned: p.cfg.PinnedVersion}
	}
	dir := p.componentDir()
	man, err := readManifest(dir)
	if err != nil {
		return plugin.State{Status: plugin.StatusError, Reason: "组件未同步（启动时自动补齐，或检查应用资源）", Pinned: p.cfg.PinnedVersion}
	}
	if !dirExists(installedPackageDir(dir)) {
		return plugin.State{Status: plugin.StatusError, Reason: "组件未完整同步", Installed: man.Version, Pinned: p.cfg.PinnedVersion}
	}
	if err := p.checkNode(man.NodePath); err != nil {
		// node 缺失：reason 用稳定 key（前端 i18n 引导）；其余错误原样（诊断用）
		reason := err.Error()
		if IsNodeMissing(err) {
			reason = ReasonNodeMissing
		}
		return plugin.State{Status: plugin.StatusError, Reason: reason, Installed: man.Version, Pinned: p.cfg.PinnedVersion}
	}
	status := plugin.StatusReady
	if p.enabled() {
		status = plugin.StatusEnabled
	}
	return plugin.State{Status: status, Installed: man.Version, Pinned: p.cfg.PinnedVersion}
}

// EnsureSynced 启动/安装命令时同步内置组件（幂等）：组件缺失/未受管/版本与 vendor
// 不一致 → 从 VendorDir 复制到组件包目录 + 写 manifest。无 VendorDir（测试/资源
// 缺失）→ 仅在组件完全缺失时报错（旧装配兼容：保留已有组件不动）。
// 只依赖文件复制（不依赖 npm）；node 检测在 State/Enable 时做（node 是运行期依赖，
// 同步本身不需要 node）。
func (p *Plugin) EnsureSynced() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ensureSyncedLocked()
}

// ensureSyncedLocked 持锁同步实现（EnsureSynced / Install 共用）。
// 受管布局：<组件根>/{package.json(壳), node_modules/chrome-devtools-mcp/,
// config.json, .enabled, .manifest.json}。同步只动 node_modules 包子树 + manifest，
// **保留 config.json（用户配置）与 .enabled（启用标记）**——早期整目录平铺拷贝会在
// 版本不一致时清掉二者（9/5 引入回归）。
func (p *Plugin) ensureSyncedLocked() error {
	dir := p.componentDir()

	// 布局归一化：把平铺旧结构/空目录规整为受管布局；返回 needSync（包缺失/未受管）。
	needSync, err := ensureComponentLayout(dir)
	if err != nil {
		return err
	}

	man, manErr := readManifest(dir)
	// 组件已存在且已受管（vendor 同步过/legacy 正常安装）且版本一致 → no-op。
	// 判定「已受管」：包目录存在 + manifest 存在；legacy npm 安装（无
	// SyncedFromVendor）在版本一致时保留不动（避免把用户手动装的版本换掉），
	// 但若它缺 SyncedFromVendor 标记且版本一致 → 视为受管不足，仍需同步一次
	// （迁移到 vendor 受管；manifest 会补上标记）。
	if !needSync && manErr == nil && man.SyncedFromVendor && man.Version == p.cfg.PinnedVersion {
		return nil
	}
	if p.cfg.VendorDir == "" {
		// 无内置源：组件已存在 → 保留（含 legacy 手动安装）；完全缺失才报错。
		if !needSync || dirExists(dir) {
			return nil
		}
		return fmt.Errorf("内置组件源缺失（VendorDir 未配置），且 %s 不存在", dir)
	}
	// 有内置源：从 vendor 包目录同步（vendor 内容 = 组件包本体）。
	vendorPkg := filepath.Join(p.cfg.VendorDir, "package.json")
	if !fileExists(vendorPkg) {
		return fmt.Errorf("内置组件源不完整：%s 缺 package.json", p.cfg.VendorDir)
	}
	p.installing = true
	defer func() { p.installing = false }()
	if err := copyVendorPackage(p.cfg.VendorDir, installedPackageDir(dir)); err != nil {
		return fmt.Errorf("同步内置组件失败: %w", err)
	}
	// 写 manifest：版本 = vendor 的 Pinned；nodePath 留空（同步不需要 node；
	// Enable 时 resolveNodePath 解析当前可用 node）。
	return writeManifest(dir, Manifest{Version: p.cfg.PinnedVersion, SyncedFromVendor: true})
}

// Install 兼容入口（旧命令/UI 升级路径）：2026-09 起组件随应用内置，安装 = 从
// VendorDir 复制同步（不再 npm 下载；node 仅运行期需要）。已启用时拒绝。
func (p *Plugin) Install(ctx context.Context) (err error) {
	_ = ctx
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.enabled() {
		return fmt.Errorf("插件已启用，请先禁用再重装")
	}
	return p.ensureSyncedLocked()
}

// Uninstall 移除组件（已启用先释放私有连接）。
func (p *Plugin) Uninstall(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.closeManager()
	return os.RemoveAll(p.componentDir())
}

// Enable 启用：构造私有 mcp.Manager（指向受管组件入口 + 默认参数），返回工具面。
// 幂等：已启用直接返回缓存。连接失败不缓存（状态经 plugin_list 可见）。
func (p *Plugin) Enable(ctx context.Context) ([]tools.Tool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.enableLocked(ctx)
}

// enableLocked Enable 实体（持锁调用；rebindTools 自愈路径复用）。
func (p *Plugin) enableLocked(ctx context.Context) ([]tools.Tool, error) {
	if p.enabled() {
		return p.tools, nil
	}
	dir := p.componentDir()
	man, err := readManifest(dir)
	if err != nil || !dirExists(installedPackageDir(dir)) {
		// 组件缺失/损坏：内置组件可自动同步（vendor 复制），先自愈一次再启用。
		if serr := p.ensureSyncedLocked(); serr != nil {
			return nil, fmt.Errorf("组件缺失且自动同步失败：%v", serr)
		}
		man, err = readManifest(dir)
		if err != nil {
			return nil, fmt.Errorf("组件元数据损坏：%v", err)
		}
	}
	entry, err := resolveEntry(dir)
	if err != nil {
		return nil, err
	}
	// §1.8.0 兼容补丁：DevTools universe 初始化在 Electron 内嵌 CDP 上会挂起 ~12s
	// （McpPage.init 的 Promise.allSettled 等它落定）→ new_page/fill 等工具被拖到超时。
	// DevTools universe 只服务 lighthouse/performance 工具（wrapper 已排除）——给它
	// 加 2s 超时快速放弃，核心浏览工具不受影响。patch 幂等（已 patch 则跳过）。
	// **先于 node 检查执行**：打补丁是纯文件操作（不依赖 node），即使 node 缺失导致
	// Enable 失败，组件目录也已处于补丁就绪态——用户装好 node 后直接可用。
	patchDevToolsTimeout(entry)
	// node 路径：manifest 记录优先；缺失/失效（同步留空或换机/nvm 切版本/PATH 重排）
	// → 从 PATH 解析当前可用 node 并回写 manifest 自愈。
	nodePath, err := p.resolveNodePath(man.NodePath)
	if err != nil {
		return nil, err
	}
	if nodePath != man.NodePath {
		man.NodePath = nodePath
		_ = writeManifest(dir, man)
	}
	// 仅 presentation=window 且自启持久 profile（persistent）校验权限：isolated 用临时目录、
	// existing 连已运行 Chrome、embedded 连 go-code 内嵌视图，均无启动行为（无需 profile）。
	if p.cfg.Presentation != PresentationEmbedded &&
		p.cfg.SessionMode != SessionIsolated && p.cfg.SessionMode != SessionExisting && !p.cfg.AutoConnect {
		if err := ensureProfileAccessible(p.cfg.UserDataDir, p.managedProfile); err != nil {
			return nil, err
		}
	}
	// 注意：presentation=embedded 不要求 EmbedEndpoint 必现——Enable 始终成功，端点由 web tab
	// 真正使用时注入（浏览器_embed_start + attach），届时 serverArgs 发 --ws-endpoint 并重启连接。
	// 缺端点时 chrome-devtools-mcp 回退到默认连接（后续经 applyEmbedEndpoint 切换为内嵌）。

	serverCfg := mcp.ServerConfig{Type: "stdio", Command: nodePath, Args: p.serverArgs(entry)}
	var mcpm *mcp.Manager
	if p.cfg.TransportFor != nil {
		mcpm = mcp.NewManagerWithTransport(map[string]mcp.ServerConfig{ID: serverCfg}, p.cfg.TransportFor)
	} else {
		mcpm = mcp.NewManager(map[string]mcp.ServerConfig{ID: serverCfg})
	}
	tl := Wrap(mcpm, WrapperConfig{Rebind: p.rebindTools}) // 包裹：browser_ 前缀 + 排除噪声 + 后处理 + 健康守卫 + 自愈换绑
	if st := mcpm.Status(); len(st) == 1 && st[0].State == "error" {
		errMsg := st[0].Error
		mcpm.Close()
		return nil, fmt.Errorf("浏览器连接失败：%s", errMsg)
	}
	p.mcpm = mcpm
	p.tools = tl
	// 持久化启用状态（<componentDir>/.enabled）：重启后 bridge 据此自动恢复启用，
	// 否则重启即丢工具面，LLM 感知不到 browser_* 工具。
	_ = p.persistEnabled()
	return tl, nil
}

// Disable 释放私有连接（幂等）。用户显式禁用：清除持久化启用标记（不再自动恢复）。
func (p *Plugin) Disable(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	p.clearEnabled()
	p.closeManager()
	return nil
}

// ClosePreserve 实现 plugin.ClosePreserver：释放私有连接但**不清除**持久化启用标记
// （应用/工作区关闭时调用，保留"上次已启用"状态供下次启动自动恢复；区别于用户显式 Disable）。
func (p *Plugin) ClosePreserve(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeManager()
	return nil
}

// serverArgs 生成 chrome-devtools-mcp 启动参数（受管入口 + 用户配置）。
// 渲染位置与连接互斥发射（上游 yargs conflict：isolated/user-data-dir/auto-connect/ws-endpoint 互斥）：
//   - presentation=embedded → --ws-endpoint（连接 go-code 内嵌视图，不发其他连接参数）；
//     登录状态由内嵌视图的持久 partition 承载（session_mode=persistent 时）。
//     viewport 不发射：内嵌视图尺寸 = 右侧栏面板实际尺寸，仿真视口会造成截断与坐标错位。
//   - presentation=window（默认）→ 按 session_mode：persistent → --user-data-dir（有头可见 + 持久
//     profile）；isolated → --isolated；existing → --auto-connect。
//
// 隐私默认：--redact-network-headers + --no-usage-statistics。
func (p *Plugin) serverArgs(entry string) []string {
	args := []string{entry}
	if p.cfg.Presentation == PresentationEmbedded {
		if p.cfg.EmbedEndpoint != "" {
			args = append(args, "--ws-endpoint="+p.cfg.EmbedEndpoint)
		}
	} else {
		switch p.cfg.SessionMode {
		case SessionIsolated:
			args = append(args, "--isolated")
		case SessionExisting:
			args = append(args, "--auto-connect")
		default: // persistent
			args = append(args, "--user-data-dir="+p.cfg.UserDataDir)
		}
	}
	// viewport 仅 window 模式发射：embedded 模式下视图尺寸 = 右侧栏面板实际尺寸，
	// 若强制 1280x720 仿真，页面按 1280 布局塞进窄面板 → 截断/滚动条，且 AI 的 CDP
	// Input 坐标（按仿真视口）与用户所见错位。不发该参数让连接方沿用真实视口，
	// 所见即所得、坐标一一对应。
	if p.cfg.Viewport != "" && p.cfg.Presentation != PresentationEmbedded {
		args = append(args, "--viewport="+p.cfg.Viewport)
	}
	if p.cfg.Headless {
		args = append(args, "--headless")
	}
	if p.cfg.Channel != "" {
		args = append(args, "--channel="+p.cfg.Channel)
	}
	if p.cfg.Redact() {
		args = append(args, "--redact-network-headers")
	}
	if !p.cfg.UsageStatistics {
		args = append(args, "--no-usage-statistics")
	}
	if p.cfg.AcceptInsecureCerts {
		args = append(args, "--accept-insecure-certs")
	}
	switch p.cfg.URLMode {
	case URLModeBlock:
		for _, pat := range p.cfg.URLPatterns {
			args = append(args, "--blocked-url-pattern="+pat)
		}
	case URLModeAllow:
		for _, pat := range p.cfg.URLPatterns {
			args = append(args, "--allowed-url-pattern="+pat)
		}
	}
	// pageId 路由：chrome-devtools-mcp ≥1.8.0 的 pageIdRouting 默认开启（schema 注入
	// 必填 pageId 并按 id 路由），无需再传已移除的 --experimental-page-id-routing。
	// 结构化响应（list_pages 的 structuredContent.pages 等）：web tab 页列表/原始调用依赖。
	args = append(args, "--experimentalStructuredContent")
	return args
}

// chromeDevToolsLogFile 已停用（不再写 chrome-devtools-mcp 日志落盘）。
func chromeDevToolsLogFile() string { return "" }

// closeManager 关闭私有 MCP 连接并清缓存（持锁调用；幂等）。
func (p *Plugin) closeManager() {
	if p.mcpm != nil {
		p.mcpm.Close()
		p.mcpm = nil
		p.tools = nil
	}
}

// rebindTools 自愈回调（wrapper Rebind）：旧连接已被释放（Disable/工作区关闭/重启）但
// 引擎仍持有旧 wrapped 工具——调用只会报「mcp 服务器 不存在」。此处重建私有连接并返回
// 新工具面（wrapper 换绑同名适配器，工具名不变、引擎无需重建）。仍存活 → 直接返回现工具面。
func (p *Plugin) rebindTools(ctx context.Context) ([]tools.Tool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.enabled() {
		if _, err := p.enableLocked(ctx); err != nil {
			return nil, err
		}
	}
	return p.tools, nil
}

// enabledMarker 启用状态持久化标记文件名（<componentDir>/.enabled）。
// 内容无意义，存在即「上次启用过」；重启后 bridge 据此自动恢复启用。
const enabledMarker = ".enabled"

// persistEnabled 写启用标记（Enable 成功后调用；失败不阻断——本进程内仍可用，仅下次重启不自动恢复）。
func (p *Plugin) persistEnabled() error {
	return os.WriteFile(filepath.Join(p.componentDir(), enabledMarker), []byte("1\n"), 0o600)
}

// clearEnabled 移除启用标记（Disable 时调用，用户显式关闭后不再自动恢复）。
func (p *Plugin) clearEnabled() {
	_ = os.Remove(filepath.Join(p.componentDir(), enabledMarker))
}

// WasEnabled 组件目录是否存在启用标记（跨进程持久状态；供 bridge 启动自动启用判定）。
// 未安装（目录不存在）→ false。
func (p *Plugin) WasEnabled() bool {
	return fileExists(filepath.Join(p.componentDir(), enabledMarker))
}

// Config 返回解析后有效配置（browser_config_get 数据源；含默认值，非持久态原文）。
func (p *Plugin) Config() Config { return p.cfg }

// SetEmbedEndpoint 运行时注入嵌入式浏览器 CDP ws 端点（M2：bridge 起转发器后调用，
// 使 embedded 模式的 serverArgs 发 --ws-endpoint）。不持久化。
func (p *Plugin) SetEmbedEndpoint(ep string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg.EmbedEndpoint = ep
}

// Configurable 标记插件有用户可调配置（plugin_list 据此显示「设置」按钮）。
func (p *Plugin) Configurable() bool { return true }

// CallRaw 经私有连接原始调用工具（绕过包裹层——web tab 需要截图 base64 / 结构化 pages 原文，
// 不经 serializeResult 占位）。未启用返回错误。
func (p *Plugin) CallRaw(ctx context.Context, tool string, args map[string]any) (*gomcp.CallToolResult, error) {
	p.mu.Lock()
	mcpm := p.mcpm
	p.mu.Unlock()
	if mcpm == nil {
		return nil, fmt.Errorf("插件未启用")
	}
	return mcpm.CallTool(ctx, ID, tool, args)
}

// StatusSnapshot 浏览器运行时状态快照（P0 观测契约：browser_status 分层数据源）。
// 持锁读取私有 Manager 状态，不建连、不阻塞。
func (p *Plugin) StatusSnapshot() map[string]any {
	p.mu.Lock()
	mcpm := p.mcpm
	p.mu.Unlock()
	mcpState := map[string]any{"state": "idle"}
	if mcpm != nil {
		st := mcpm.Status()
		if len(st) == 1 {
			s := st[0]
			mcpState = map[string]any{
				"state": s.State, "phase": s.Phase,
				"tool_count": s.ToolCount, "error": s.Error,
				"last_duration_ms": s.LastDurationMs, "error_code": s.ErrorCode,
			}
		}
	}
	return map[string]any{
		"mcp": mcpState,
	}
}

// RefreshConn 重连私有 MCP 连接（§15.1：web tab 的 CallRaw transport-closed 自愈）。
// 实现 browserExtRefresh 接口（bridge 据此在 browser_pages/screenshot 失败时重连重发）。
func (p *Plugin) RefreshConn(ctx context.Context) error {
	p.mu.Lock()
	mcpm := p.mcpm
	p.mu.Unlock()
	if mcpm == nil {
		return fmt.Errorf("插件未启用")
	}
	return mcpm.Refresh(ctx, ID)
}
