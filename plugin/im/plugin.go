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
	"github.com/seven7628/hai-harness/owner"
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
// 两层职责（owner 隔离）：
//   - **连接面**（Enable/Disable）：读 im-config.json → 实例化并 Start 所有 enabled Gateway。
//     连接是**进程级共享**的：不为每个 owner 各起一条账号长连接（那会变成同一账号多条
//     连接、消息重复回复）。
//   - **归属面**（OwnerEnabled/SetOwnerEnabled/OwnerView）：每个 owner（agent/group/
//     workspace）有独立的启用状态与路由。A 群关闭只停 A 群的路由投递，B 智能体照常收发。
//
// State：连接状态汇总（connected / error / 未配置）；owner 视角见 OwnerView。
type Plugin struct {
	cfgPath string
	// 可注入依赖（测试）
	LoadConfig func(path string) (im.Config, error)

	mu      sync.Mutex
	runtime *Runtime // 启用后持有（nil = 未启用）
}

// Runtime IM 运行时（启用后持有；bridge 事件流接入点）。
//
// 并发：配置与连接表由**同一把锁**保护（它们总是一起变更：路由/镜像更新与
// Gateway 差异重建是同一次 applyIMConfig）。事件回调跑在 Gateway 自己的 goroutine
// 里、命令处理跑在 stdin 循环里，两者都会读这两张表 —— 直接读字段就是 -race 的
// data race（IM 的 owner 改造把读点加到 im_bridge 的投递闸门后更明显）。
// 因此：**只经由 ConfigSnapshot/SetConfig/Gateway/GatewaySnapshot/PutGateway/
// DeleteGateway/TakeGateways 访问**，字段本身不导出。
type Runtime struct {
	Router   *im.Router
	Security *im.Security

	mu       sync.RWMutex
	cfg      im.Config
	gateways map[string]im.Gateway // 实例 id → Gateway（已启动的）
	// types 已启动 Gateway 的类型（按注册顺序；诊断用）
	types []string
}

// ConfigSnapshot 当前配置的**独立副本**（深拷贝可变容器：owners/gateways/routes/
// mirrors）。调用方可以随意读甚至改自己的副本，改副本不会影响运行时。
func (r *Runtime) ConfigSnapshot() im.Config {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return r.cfg.Clone()
}

// SetConfig 整体替换配置（发布后旧副本不再被原地修改：写入方必须先 Clone）。
func (r *Runtime) SetConfig(cfg im.Config) {
	r.mu.Lock()
	r.cfg = cfg
	r.mu.Unlock()
}

// Gateway 按实例 id 取已启动的 Gateway。
func (r *Runtime) Gateway(id string) (im.Gateway, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	g, ok := r.gateways[id]
	return g, ok
}

// GatewaySnapshot 连接表的浅拷贝（值是不可变接口；遍历期间不会被增删打乱）。
func (r *Runtime) GatewaySnapshot() map[string]im.Gateway {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make(map[string]im.Gateway, len(r.gateways))
	for id, g := range r.gateways {
		out[id] = g
	}
	return out
}

// PutGateway 登记一条已启动连接。
func (r *Runtime) PutGateway(id string, g im.Gateway) {
	r.mu.Lock()
	if r.gateways == nil {
		r.gateways = map[string]im.Gateway{}
	}
	r.gateways[id] = g
	r.mu.Unlock()
}

// AddType 记录已启动 Gateway 的类型（诊断用；与连接表同锁）。
func (r *Runtime) AddType(t string) {
	r.mu.Lock()
	r.types = append(r.types, t)
	r.mu.Unlock()
}

// DeleteGateway 摘除一条连接（**不停**它：调用方拿到返回值后自己 Stop）。
func (r *Runtime) DeleteGateway(id string) im.Gateway {
	r.mu.Lock()
	defer r.mu.Unlock()
	g := r.gateways[id]
	delete(r.gateways, id)
	return g
}

// TakeGateways 取走全部连接并清空表（关闭/重建时用；Stop 由调用方在锁外做）。
func (r *Runtime) TakeGateways() map[string]im.Gateway {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.gateways
	r.gateways = map[string]im.Gateway{}
	return out
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
//
// owner 隔离后的补充：owners 段存在且**没有任何归属启用**时不再自动恢复连接 ——
// 否则用户把最后一个归属关掉、重启后连接又自己起来了（明明没人用）。
// owners 段为空 = 旧配置/尚未登记归属：保持旧行为（连接照常，绑定确认流程要用）。
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
			if len(cfg.Owners) == 0 {
				return true
			}
			return anyOwnerEnabled(cfg)
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
	for _, g := range p.runtime.GatewaySnapshot() {
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
	return p.enableLocked(ctx)
}

// enableLocked Enable 的持锁主体（SetOwnerEnabled 在同一把锁内复用）。
func (p *Plugin) enableLocked(ctx context.Context) ([]tools.Tool, error) {
	if p.runtime != nil {
		return nil, nil // 已启用
	}

	cfg, err := p.LoadConfig(p.cfgPath)
	if err != nil {
		return nil, fmt.Errorf("im: 读取配置失败: %w", err)
	}
	if !cfg.Enabled {
		// 主开关关：插件"启用"但不启动任何网关（状态=已启用无连接）
		rt := &Runtime{Router: im.NewRouter(cfg), Security: im.NewSecurity(cfg, auditPath(p.cfgPath)), gateways: map[string]im.Gateway{}}
		rt.SetConfig(cfg)
		p.runtime = rt
		return nil, nil
	}

	r := &Runtime{
		Router:   im.NewRouter(cfg),
		Security: im.NewSecurity(cfg, auditPath(p.cfgPath)),
		gateways: make(map[string]im.Gateway, len(cfg.Gateways)),
	}
	r.SetConfig(cfg)
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
		r.PutGateway(gc.ID, g)
		r.AddType(gc.Type)
	}
	p.runtime = r
	if len(errs) > 0 {
		return nil, fmt.Errorf("im: 部分网关启动失败: %s", joinErrors(errs))
	}
	return nil, nil
}

// Disable 实现 plugin.Capability：停止全部网关（用户显式关闭插件）。
func (p *Plugin) Disable(_ context.Context) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.stopAll()
	return nil
}

// OwnerView 某归属的 IM 视图（管理面：plugin_list / im_status 用）。
//
// 连接状态是共享的（同一份 Gateways），但**启用状态与路由数按归属**给出 ——
// 面板因此能表达「A 群已启用 / B 智能体未启用」，而不是拿一份全局状态糊弄所有 owner。
type OwnerView struct {
	Owner     string            `json:"owner"`
	Enabled   bool              `json:"enabled"`   // 该归属生效的启用状态（含主开关）
	Master    bool              `json:"master"`    // 主开关（连接面）
	Routes    int               `json:"routes"`    // 该归属名下路由数
	Gateways  map[string]string `json:"gateways"`  // 共享连接：实例 id → 状态（仅供展示）
	Connected int               `json:"connected"` // 已连接网关数
}

// OwnerView 计算某归属的视图（只读；不触发连接）。
func (p *Plugin) OwnerView(ownerID string) OwnerView {
	p.mu.Lock()
	defer p.mu.Unlock()
	var cfg im.Config
	if p.runtime != nil {
		cfg = p.runtime.ConfigSnapshot()
	} else if c, err := p.LoadConfig(p.cfgPath); err == nil {
		cfg = c
	}
	v := OwnerView{Owner: ownerID, Master: cfg.Enabled, Enabled: cfg.OwnerEnabled(ownerID), Gateways: map[string]string{}}
	for _, g := range cfg.Gateways {
		for _, r := range g.Routes {
			if r.EffectiveOwner() == ownerID {
				v.Routes++
			}
		}
	}
	if p.runtime != nil {
		for id, g := range p.runtime.GatewaySnapshot() {
			v.Gateways[id] = string(g.Status())
			if g.Status() == im.StatusConnected {
				v.Connected++
			}
		}
	}
	return v
}

// Owners 全部归属的生效启用状态（管理面总览）。
func (p *Plugin) Owners() map[string]bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.runtime != nil {
		return p.runtime.ConfigSnapshot().OwnersAll()
	}
	cfg, err := p.LoadConfig(p.cfgPath)
	if err != nil {
		return map[string]bool{}
	}
	return cfg.OwnersAll()
}

// RoutesForOwner 某归属名下的路由（管理面：只看自己的路由）。
func (p *Plugin) RoutesForOwner(ownerID string) []im.RouteConfig {
	p.mu.Lock()
	defer p.mu.Unlock()
	var cfg im.Config
	if p.runtime != nil {
		cfg = p.runtime.ConfigSnapshot()
	} else if c, err := p.LoadConfig(p.cfgPath); err == nil {
		cfg = c
	}
	var out []im.RouteConfig
	for _, g := range cfg.Gateways {
		for _, r := range g.Routes {
			if r.EffectiveOwner() == ownerID {
				out = append(out, r)
			}
		}
	}
	return out
}

// SetOwnerEnabled 设置某归属的启用状态并落盘（owner 级启用，不是全局开关）。
//
// 语义：
//   - 启用该归属 → 主开关若关着则打开（「启用某归属」的意思就是它要能收发），
//     连接未起则按新配置起（enableLocked）；已起则只热更新 Router/Security。
//   - 关闭该归属 → 只写该归属的状态；**只有在没有任何归属仍启用时**才停连接
//     （否则关 A 群会把 B 智能体的连接一起掐掉 —— 这正是本条要修的跨 owner 影响）。
//
// 持久化：写 im-config.json（与配置页同一个真相源，不另起一份账本）。
func (p *Plugin) SetOwnerEnabled(ctx context.Context, ownerID string, enabled bool) error {
	if !owner.Valid(ownerID) {
		return fmt.Errorf("im: 非法 owner %q（必须是 workspace:/agent:/group: 之一）", ownerID)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	cfg, err := p.loadConfigLocked()
	if err != nil {
		return err
	}
	flag := enabled
	if err := cfg.SetOwnerEnabled(ownerID, &flag); err != nil {
		return err
	}
	if enabled && !cfg.Enabled {
		cfg.Enabled = true
	}
	// 关掉最后一个归属 = 没有任何人还开着 IM：主开关一起关（否则重启后 WasEnabled
	// 会把连接又拉起来，而没有任何归属能收消息）。
	if !enabled && !anyOwnerEnabledAfterSet(cfg, ownerID) {
		cfg.Enabled = false
	}
	if err := im.SaveConfig(p.cfgPath, cfg); err != nil {
		return fmt.Errorf("im: 保存归属状态失败: %w", err)
	}
	if p.runtime == nil {
		if !enabled {
			return nil // 本来就没连接：关一个归属无需起连接
		}
		_, err := p.enableLocked(ctx) // 读刚写下的配置
		return err
	}
	p.runtime.SetConfig(cfg)
	p.runtime.Router.Update(cfg)
	p.runtime.Security.Update(cfg)
	if !enabled && !anyOwnerEnabled(cfg) {
		p.stopAllLocked()
	}
	return nil
}

// anyOwnerEnabledAfterSet 本次写入后是否还有**其它**归属处于启用状态
// （刚关掉的那个已经写成 false，不能再把它算进来）。
func anyOwnerEnabledAfterSet(cfg im.Config, justDisabled string) bool {
	for own := range cfg.OwnersAll() {
		if own == justDisabled {
			continue
		}
		if cfg.OwnerEnabled(own) {
			return true
		}
	}
	return false
}

// loadConfigLocked 读当前配置（运行态优先：热改过的内存态才是真相）。
func (p *Plugin) loadConfigLocked() (im.Config, error) {
	if p.runtime != nil {
		// 快照是独立副本：调用方（SetOwnerEnabled）要改 Owners 后重新发布，
		// 直接在运行时的那份上改就是 data race（读者在事件 goroutine 里）。
		return p.runtime.ConfigSnapshot(), nil
	}
	return p.LoadConfig(p.cfgPath)
}

// anyOwnerEnabled 是否还有归属处于启用状态（决定「能不能停连接」）。
// 迁移语义：没有 owners 段的旧配置，只要有路由就算启用（与 Config.OwnerEnabled 同源）。
func anyOwnerEnabled(cfg im.Config) bool {
	for own := range cfg.OwnersAll() {
		if cfg.OwnerEnabled(own) {
			return true
		}
	}
	return false
}

// stopAllLocked 停全部网关（持锁调用）。
func (p *Plugin) stopAllLocked() { p.stopAll() }

// ClosePreserve 实现 plugin.ClosePreserver：**工作区级 Close 不释放进程级连接**。
//
// 背景（BR-21）：bridge 把同一个 IM 插件实例登记进**每个**工作区的插件注册表
// （buildPlugins(m.imPlugin)：IM 是跨 workspace 的一条连接 + 一张路由表，按工作区
// 各造一份会变成同一账号多条连接、消息重复回复）。而 plugin.Registry.Close 的语义是
// 工作区级收尾 —— 会在这些路径被调到：
//   - shutdown_workspace / stdin EOF（main.go shutdownWorkspace/shutdownAll）
//   - delete_workspace（删掉**别的**工作区）
//   - applyBrowserConfig（用户改浏览器设置 → 逐工作区 prev.Close）
//   - Hub 智能体运行时收尾（hub_session.go）
//
// 没有本方法时，任何一条都会走 Disable → stopAll：用户只是删了 B 项目 / 改了个浏览器
// 配置，A 项目正在用的 IM 连接就被静默掐断（且 imBridge 仍持着已停的 Runtime）。
//
// 故这里显式空实现：工作区级 Close 对进程级单例没有可释放的东西。真正的收尾在进程级
// （bridge 的 closeIMPlugin：shutdownAll 时 Disable 一次），用户显式关闭插件走 Disable。
func (p *Plugin) ClosePreserve(_ context.Context) error { return nil }

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
	for _, g := range p.runtime.TakeGateways() {
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
