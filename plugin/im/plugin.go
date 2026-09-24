// Package im 把 github.com/seven7628/hai-harness/im 抽象层封装成插件能力（plugin.Capability），
// 复用现有插件框架（plugin_list / plugin_enable / plugin_disable + 设置页）。
//
// 定位（docs/IM_INTEGRATION.md §9.2）：
//   - 管理面：插件壳（本包）负责启停/状态，走现有插件生命周期
//   - 运行面：im 包（Router/Renderer/Approval/Security/Gateway）负责消息路由/渲染/审批
//   - 用户看到的是"IM 插件"（plugin 视角）；内部跑的是 im 抽象层（运行视角）
//
// 配置：~/.github.com/seven7628/hai-harness/im-config.json（im.Config），Enable 时读取并启动全部 enabled Gateway。
package im

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/seven7628/hai-harness/im"
	"github.com/seven7628/hai-harness/plugin"
	"github.com/seven7628/hai-harness/tools"
	// blank import：触发内建 Gateway 的 init 注册（im.Register + RegisterSchema）。
	// 不 import 则 feishu 的 init 不执行 → 注册表空 → im_schema_list 无下拉选项。
	_ "github.com/seven7628/hai-harness/im/feishu"
)

// ID/Name/Description 插件元数据（plugin_list 展示）。
const (
	ID          = "im"
	Name        = "IM 集成"
	Description = "外部 IM 网关：通过飞书等 IM 远程对话 go-code（发任务/审批/结果推送）。"
)

// DefaultConfigPath 默认 IM 配置文件（与 appdata 的 ~/.go-code 一致）。
func DefaultConfigPath() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ".github.com/seven7628/hai-harness/im-config.json"
	}
	return filepath.Join(home, ".go-code", "im-config.json")
}

// Plugin IM 集成能力实现（实现 plugin.Capability）。
//
// Enable：读 im-config.json → 实例化并 Start 所有 enabled Gateway + 构建 Router/Security。
// Disable：Stop 全部 Gateway。
// State：连接状态汇总（connected / error / 未配置）。
type Plugin struct {
	cfgPath string
	// 可注入依赖（测试）
	LoadConfig func(path string) (im.Config, error)

	mu      sync.Mutex
	runtime *Runtime // 启用后持有（nil = 未启用）
}

// Runtime IM 运行时（启用后持有；bridge 事件流接入点）。
type Runtime struct {
	Config   im.Config
	Router   *im.Router
	Security *im.Security
	// Gateways 实例 id → Gateway（已启动的）
	Gateways map[string]im.Gateway
	// GatewayTypes 类型 → 实例列表（按注册顺序）
	types []string
}

// New 构造 IM 插件。
func New(cfgPath string) *Plugin {
	if cfgPath == "" {
		cfgPath = DefaultConfigPath()
	}
	return &Plugin{
		cfgPath:    cfgPath,
		LoadConfig: im.LoadConfig,
	}
}

func (p *Plugin) ID() string          { return ID }
func (p *Plugin) Name() string        { return Name }
func (p *Plugin) Description() string { return Description }

// IsEnabled 运行时事实：是否已启用（runtime 非 nil）。
func (p *Plugin) IsEnabled() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runtime != nil
}

// WasEnabled 重启前是否应自动恢复：配置驱动（im-config.json 主开关开 + 有 enabled gateway）。
// IM 无独立持久化标记——配置即启用（区别于 browser 的 .enabled 标记）。
func (p *Plugin) WasEnabled() bool {
	cfg, err := p.LoadConfig(p.cfgPath)
	if err != nil {
		return false
	}
	if !cfg.Enabled {
		return false
	}
	for _, g := range cfg.Gateways {
		if g.Enabled {
			return true
		}
	}
	return false
}

// State 实现 plugin.Capability：从运行时计算（只读）。
func (p *Plugin) State(_ context.Context) plugin.State {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runtime == nil {
		// 未启用（IM 无外部组件，无"未安装"态；Install 是 no-op）
		return plugin.State{Status: plugin.StatusReady, Reason: "未启用（配置后启用）"}
	}
	// 汇总：任一 connected → ready；有 error → error
	connected, errCount := 0, 0
	for _, g := range p.runtime.Gateways {
		switch g.Status() {
		case im.StatusConnected:
			connected++
		case im.StatusError:
			errCount++
		}
	}
	if connected > 0 {
		return plugin.State{Status: plugin.StatusEnabled, Reason: fmt.Sprintf("%d 个网关已连接", connected)}
	}
	if errCount > 0 {
		return plugin.State{Status: plugin.StatusError, Reason: fmt.Sprintf("%d 个网关连接失败", errCount)}
	}
	return plugin.State{Status: plugin.StatusEnabled, Reason: "已启用（无网关连接）"}
}

// Install 实现 plugin.Capability：IM 无外部组件，幂等 no-op。
func (p *Plugin) Install(_ context.Context) error { return nil }

// Uninstall 实现 plugin.Capability：停止全部网关并清理配置。
func (p *Plugin) Uninstall(_ context.Context) error {
	p.mu.Lock()
	r := p.runtime
	p.mu.Unlock()
	if r != nil {
		p.stopAll()
	}
	return nil
}

// Enable 实现 plugin.Capability：读配置 → 启动全部 enabled Gateway → 构建运行时。
// 返回空工具面（IM 不注入工具）。
func (p *Plugin) Enable(ctx context.Context) ([]tools.Tool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runtime != nil {
		return nil, nil // 已启用
	}

	cfg, err := p.LoadConfig(p.cfgPath)
	if err != nil {
		return nil, fmt.Errorf("im: 读取配置失败: %w", err)
	}
	if !cfg.Enabled {
		// 主开关关：插件"启用"但不启动任何网关（状态=已启用无连接）
		p.runtime = &Runtime{Config: cfg, Router: im.NewRouter(cfg), Security: im.NewSecurity(cfg, auditPath(p.cfgPath)), Gateways: map[string]im.Gateway{}}
		return nil, nil
	}

	r := &Runtime{
		Config:   cfg,
		Router:   im.NewRouter(cfg),
		Security: im.NewSecurity(cfg, auditPath(p.cfgPath)),
		Gateways: make(map[string]im.Gateway, len(cfg.Gateways)),
	}
	// 启动全部 enabled Gateway
	var errs []string
	for _, gc := range cfg.Gateways {
		if !gc.Enabled {
			continue
		}
		g, err := im.New(gc.Type, gc.Config)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", gc.ID, err))
			continue
		}
		if err := g.Start(ctx); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", gc.ID, err))
			continue
		}
		r.Gateways[gc.ID] = g
		r.types = append(r.types, gc.Type)
	}
	p.runtime = r
	if len(errs) > 0 {
		return nil, fmt.Errorf("im: 部分网关启动失败: %s", joinErrors(errs))
	}
	return nil, nil
}

// Disable 实现 plugin.Capability：停止全部网关。
func (p *Plugin) Disable(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopAll()
	return nil
}

// Runtime 返回当前运行时（未启用返回 nil）。
func (p *Plugin) Runtime() *Runtime {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.runtime
}

// stopAll 停止全部网关（持锁调用）。
func (p *Plugin) stopAll() {
	if p.runtime == nil {
		return
	}
	ctx := context.Background()
	for _, g := range p.runtime.Gateways {
		_ = g.Stop(ctx)
	}
	p.runtime = nil
}

// auditPath 审计日志路径（与配置同目录）。
func auditPath(cfgPath string) string {
	return filepath.Join(filepath.Dir(cfgPath), "im-audit.jsonl")
}

func joinErrors(errs []string) string {
	out := ""
	for i, e := range errs {
		if i > 0 {
			out += "; "
		}
		out += e
	}
	return out
}
