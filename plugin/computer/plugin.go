// Package computer Computer Use 插件（默认关）：用户「设置 → 插件」启用后，
// 把桌面操控语义工具族（computer_snapshot/press/type/…，见 github.com/seven7628/hai-harness/computer
// 包）注册进会话引擎。未启用不拉起 helper、不申请 TCC 权限、LLM 看不到工具。
//
// 与 Browser Use 同形态（plugin/browser）：Capability 接口 + 受管组件 +
// 持久化启用状态（.enabled 标记，重启自动恢复）。差异：computer 组件是
// 随应用分发的 Swift helper 二进制（非 npm 包），无需网络安装。
//
// helper 定位（优先级）：
//  1. env GO_CODE_COMPUTER_HELPER（测试/E2E 注入；dev 直接指编译产物）；
//  2. 组件目录 <pluginsDir>/computer/computer-helper（受管复制）；
//  3. 仓库 dev 默认 desktop/computer-helper/computer-helper（未打包开发跑）。
//
// 装配：desktop/bridge/buildPlugins 注册本插件；rebuildWorkspace 后工具生效。
package computer

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/seven7628/hai-harness/computer"
	"github.com/seven7628/hai-harness/plugin"
	"github.com/seven7628/hai-harness/tools"
)

// 插件元信息（plugin_list / UI 展示）。
const (
	ID          = "computer"
	Name        = "Computer Use"
	Description = "macOS 桌面操控：读取 a11y 快照、语义按压按钮/菜单、键入文本、验证结果（非视觉与视觉模型均可用）。"
)

// DefaultComponentName helper 组件文件名（组件目录内）。
const DefaultComponentName = "computer-helper"

// helperEnv helper 路径 env 覆盖（测试/E2E/dev 注入）。
const helperEnv = "GO_CODE_COMPUTER_HELPER"

// helperDirEnv helper 源目录 env（electron spawn bridge 注入；dev=仓库
// desktop/computer-helper，打包=Resources/computer-helper）。启动 EnsureSynced
// 时把源里的 computer-helper 二进制复制到受管组件目录。
const helperDirEnv = "GO_CODE_COMPUTER_HELPER_DIR"

// Plugin Computer Use 能力实现（实现 plugin.Capability）。
// 启用后持有 Executor + 缓存工具面；生命周期由 Registry 协调。
type Plugin struct {
	cfg Config

	mu      sync.Mutex
	backend computer.Backend // 启用后持有（helper 子进程连接）
	exec    *computer.Executor
	tools   []tools.Tool
	enabled bool
}

// Config 插件配置（含测试注入）。
type Config struct {
	// PluginsDir 受管组件根目录（默认 ~/.go-code/plugins）。
	PluginsDir string `json:"-"`
	// HelperPath 显式 helper 路径（默认按 helperEnv/组件目录/dev 解析）。测试注入。
	HelperPath string `json:"-"`
	// HelperDir helper 源目录（EnsureSynced 同步来源；electron 经 env 注入）。
	// 空 = 无内置源（仅 dev 相对路径兜底）。
	HelperDir string
	// ApprovedApps 已授权可操作的 App 集合（统一授权门禁；bridge 从 settings.json
	// permissions.computer_approved_apps 读入注入）。nil = 装配方未提供时全部放行。
	ApprovedApps map[string]bool
	// ReloadApproved 授权懒刷新（nil = 不刷新）：门禁未命中时重读 settings.json 最新
	// 集合再判——用户设置页加授权后立即生效（bridge 长跑不重启）。
	ReloadApproved func() map[string]bool
}

// New 构造 Computer Use 插件。
func New(cfg Config) *Plugin {
	if cfg.PluginsDir == "" {
		cfg.PluginsDir = defaultPluginsDir()
	}
	return &Plugin{cfg: cfg}
}

func (p *Plugin) ID() string          { return ID }
func (p *Plugin) Name() string        { return Name }
func (p *Plugin) Description() string { return Description }

// componentDir 本插件受管组件目录（<pluginsDir>/computer）。
func (p *Plugin) componentDir() string {
	return filepath.Join(p.cfg.PluginsDir, ID)
}

// enabledMarker 启用状态持久化标记（<componentDir>/.enabled）。
const enabledMarker = ".enabled"

// ---- plugin.Capability ----

// State 当前状态（只读）：helper 存在 + .enabled 标记。
func (p *Plugin) State(_ context.Context) plugin.State {
	if _, err := os.Stat(p.helperPath()); err != nil {
		return plugin.State{Status: plugin.StatusError, Reason: "computer-helper 缺失（未随应用分发）"}
	}
	p.mu.Lock()
	enabled := p.enabled
	p.mu.Unlock()
	if enabled {
		return plugin.State{Status: plugin.StatusEnabled}
	}
	if p.wasEnabled() {
		return plugin.State{Status: plugin.StatusReady, Reason: "上次已启用（重启自动恢复）"}
	}
	return plugin.State{Status: plugin.StatusReady}
}

// EnsureSynced 把内置 helper 源同步到受管组件目录（启动时调用；对齐 browser 组件
// 随应用内置模式）。幂等：受管 helper 已存在且与源一致 → no-op。
// 源缺失（未打包 dev 且仓库无编译产物）→ 报错（UI 插件卡可见原因 + 构建引导）。
func (p *Plugin) EnsureSynced() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ensureSyncedLocked()
}

// ensureSyncedLocked 持锁同步实现（EnsureSynced / Install 共用）。
func (p *Plugin) ensureSyncedLocked() error {
	src := p.helperSource()
	if src == "" {
		// 无内置源：受管已有 helper → 保留可用；否则报缺失
		if _, err := os.Stat(filepath.Join(p.componentDir(), DefaultComponentName)); err == nil {
			return nil
		}
		return fmt.Errorf("computer-helper 源缺失（未随应用分发）。开发环境先构建：cd desktop/computer-helper && swiftc -O -o computer-helper computer_helper.swift")
	}
	dst := filepath.Join(p.componentDir(), DefaultComponentName)
	// 已同步（源与目标都存在）→ no-op（不重复复制；源构建时间新于目标才更新——
	// 打包态源固定；dev 反复 swiftc 重编后重启应用会同步新版本）。
	if si, err1 := os.Stat(src); err1 == nil {
		if di, err2 := os.Stat(dst); err2 == nil &&
			!si.ModTime().After(di.ModTime()) && di.Size() == si.Size() {
			// 源不新于目标且大小一致 → 已同步（复制保留目标 mtime 近似源；源改动
			// mtime 变新 → 触发重同步）
			return nil
		}
	}
	if err := os.MkdirAll(p.componentDir(), 0o755); err != nil {
		return fmt.Errorf("创建组件目录: %w", err)
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return fmt.Errorf("读 helper 源: %w", err)
	}
	if err := os.WriteFile(dst, data, 0o755); err != nil {
		return fmt.Errorf("同步 computer-helper: %w", err)
	}
	return nil
}

// helperSource helper 源二进制路径（HelperDir 配置 / env 注入）。
func (p *Plugin) helperSource() string {
	dir := p.cfg.HelperDir
	if dir == "" {
		dir = os.Getenv(helperDirEnv)
	}
	if dir == "" {
		// dev 兜底：仓库相对路径（bridge cwd = 仓库根时有效）
		dev := filepath.Join("desktop", "computer-helper")
		if _, err := os.Stat(filepath.Join(dev, DefaultComponentName)); err == nil {
			if abs, aerr := filepath.Abs(dev); aerr == nil {
				return filepath.Join(abs, DefaultComponentName)
			}
		}
		return ""
	}
	return filepath.Join(dir, DefaultComponentName)
}

// Install 兼容入口：确保 helper 同步到受管目录（组件随应用分发，安装 = 同步）。
func (p *Plugin) Install(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.ensureSyncedLocked()
}

// Uninstall 移除组件目录（已启用先释放）。
func (p *Plugin) Uninstall(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.enabled = false
	return os.RemoveAll(p.componentDir())
}

// Enable 启用：拉起 helper → 建 Backend/Executor → 返回语义工具族。幂等。
func (p *Plugin) Enable(ctx context.Context) ([]tools.Tool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.enabled {
		return p.tools, nil
	}
	helper := p.helperPath()
	if _, err := os.Stat(helper); err != nil {
		return nil, fmt.Errorf("computer-helper 不存在：%v（先构建 desktop/computer-helper）", err)
	}
	be := computer.NewStdioBackend(helper)
	// 启动探测：hello 握手 + 权限自检（失败快速反馈 UI）
	if _, err := be.PermStatus(); err != nil {
		_ = be.Close()
		return nil, fmt.Errorf("computer-helper 启动失败：%v", err)
	}
	ex := computer.NewExecutor(be, computer.ExecutorConfig{ApprovedApps: p.cfg.ApprovedApps, ReloadApproved: p.cfg.ReloadApproved})
	p.backend = be
	p.exec = ex
	p.tools = computer.SemTools(ex)
	p.enabled = true
	p.persistEnabled()
	return p.tools, nil
}

// Executor 返回当前执行器（未启用时 nil）。宿主据此注册视觉档看图工具
// （computer_screenshot）：仅当模型支持图片输入时才挂载，见 computer.VisionTools。
func (p *Plugin) Executor() *computer.Executor {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.exec
}

// Disable 释放 helper（幂等）。用户显式禁用：清除持久化启用标记。
func (p *Plugin) Disable(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.clearEnabled()
	p.closeLocked()
	return nil
}

// ClosePreserve 实现 plugin.ClosePreserver：释放连接但保留启用标记
// （应用关闭后下次启动自动恢复）。
func (p *Plugin) ClosePreserve(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closeLocked()
	return nil
}

// closeLocked 释放 helper 连接与工具面（持锁调用；幂等）。
func (p *Plugin) closeLocked() {
	if p.backend != nil {
		_ = p.backend.Close()
		p.backend = nil
	}
	p.exec = nil
	p.tools = nil
	p.enabled = false
}

// ---- 查询接口（bridge/UI 用） ----

// IsEnabled 运行时启用事实（plugin_list enabled 字段）。
func (p *Plugin) IsEnabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.enabled
}

// WasEnabled 组件目录是否存在启用标记（重启自动启用判定）。
func (p *Plugin) WasEnabled() bool { return p.wasEnabled() }

// HelperPath 当前生效 helper 路径（暴露给状态/诊断）。
func (p *Plugin) HelperPath() string { return p.helperPath() }

// SetApprovedApps 热更新已授权 App 集合（settings.json 变更后 bridge 调用；
// 未启用时仅存 cfg，下次 Enable 生效）。
func (p *Plugin) SetApprovedApps(approved map[string]bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.cfg.ApprovedApps = approved
	if p.exec != nil {
		p.exec.SetApprovedApps(approved)
	}
}

// Configurable 标记插件有用户可调配置（plugin_list 据此显示「设置」按钮，
// UI 展开 ComputerSettingsEditor）。
func (p *Plugin) Configurable() bool { return true }

// StatusSnapshot Computer Use 运行时状态快照（详情页数据源；只读，不建连）。
// 未启用：权限字段按组件目录/helper 存在性给基础信息，accessibility 恒 false
// （未拉起 helper 时无权限可查——语义：未启用 = 未授权）。
func (p *Plugin) StatusSnapshot() map[string]any {
	p.mu.Lock()
	enabled := p.enabled
	be := p.backend
	exec := p.exec
	toolCount := 0
	if p.tools != nil {
		toolCount = len(p.tools)
	}
	p.mu.Unlock()

	snap := map[string]any{
		"helperPath": p.helperPath(),
		"enabled":    enabled,
		"toolCount":  toolCount,
	}
	if enabled && be != nil && exec != nil {
		if ps, err := be.PermStatus(); err == nil {
			snap["accessibility"] = ps.Accessibility
			snap["screenCapture"] = ps.ScreenCapture
			snap["trustedApp"] = ps.TrustedApp
		} else {
			snap["permError"] = err.Error()
		}
	} else {
		snap["accessibility"] = false
		snap["screenCapture"] = false
	}
	return snap
}

// ---- 内部 ----

// helperPath helper 可执行路径解析。
func (p *Plugin) helperPath() string {
	if p.cfg.HelperPath != "" {
		return p.cfg.HelperPath
	}
	if env := os.Getenv(helperEnv); env != "" {
		return env
	}
	// 受管组件目录（Install/分发落点）
	managed := filepath.Join(p.componentDir(), DefaultComponentName)
	if _, err := os.Stat(managed); err == nil {
		return managed
	}
	// dev 默认（仓库内编译产物）
	dev := filepath.Join("desktop", "computer-helper", DefaultComponentName)
	if _, err := os.Stat(dev); err == nil {
		abs, aerr := filepath.Abs(dev)
		if aerr == nil {
			return abs
		}
		return dev
	}
	return managed // 兜底返回受管路径（State 会报缺失）
}

func (p *Plugin) persistEnabled() error {
	if err := os.MkdirAll(p.componentDir(), 0o755); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(p.componentDir(), enabledMarker), []byte("1\n"), 0o600)
}

func (p *Plugin) clearEnabled() {
	_ = os.Remove(filepath.Join(p.componentDir(), enabledMarker))
}

func (p *Plugin) wasEnabled() bool {
	_, err := os.Stat(filepath.Join(p.componentDir(), enabledMarker))
	return err == nil
}

// defaultPluginsDir ~/.go-code/plugins（与 browser 对齐；避免循环依赖不引 browser 包）。
func defaultPluginsDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".go-code", "plugins")
	}
	return filepath.Join(home, ".go-code", "plugins")
}
