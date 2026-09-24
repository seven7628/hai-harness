package plugin

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/seven7628/hai-harness/tools"
)

// Registry 内建插件注册表（通用扩展点）：持有能力实例，协调安装/启用生命周期。
// 每个工作区一个（私有 MCP 连接按工作区隔离）；组件目录全局共享（安装幂等）。
// mu 保护 enabledTools（异步安装/重建并发读）；能力调用（Enable/Install 等可能长耗时）
// 放在锁外，避免阻塞 Engine 重建。
type Registry struct {
	mu           sync.Mutex
	caps         []Capability
	enabledTools map[string][]tools.Tool // 已启用插件的工具（引擎注册数据源）
}

// NewRegistry 建注册表（caps 保持传入顺序；List 返回同序）。
func NewRegistry(caps ...Capability) *Registry {
	return &Registry{caps: caps, enabledTools: map[string][]tools.Tool{}}
}

// List 全部已注册能力（注册顺序）。
func (r *Registry) List() []Capability { return r.caps }

// Get 按 id 取能力。
func (r *Registry) Get(id string) (Capability, bool) {
	for _, c := range r.caps {
		if c.ID() == id {
			return c, true
		}
	}
	return nil, false
}

// State 委托到具体能力（只读，无副作用）。
func (r *Registry) State(ctx context.Context, id string) (State, error) {
	c, ok := r.Get(id)
	if !ok {
		return State{}, fmt.Errorf("插件 %q 不存在", id)
	}
	return c.State(ctx), nil
}

// Install 受管安装组件（幂等：已装且版本一致 no-op）。
func (r *Registry) Install(ctx context.Context, id string) error {
	c, ok := r.Get(id)
	if !ok {
		return fmt.Errorf("插件 %q 不存在", id)
	}
	return c.Install(ctx)
}

// Uninstall 移除组件（已启用先释放连接）。
func (r *Registry) Uninstall(ctx context.Context, id string) error {
	c, ok := r.Get(id)
	if !ok {
		return fmt.Errorf("插件 %q 不存在", id)
	}
	r.mu.Lock()
	delete(r.enabledTools, id)
	r.mu.Unlock()
	return c.Uninstall(ctx)
}

// Enable 启用插件并返回注入引擎的工具面。幂等：已启用直接返回缓存工具。
// 调用方（bridge）负责在工具集变化后重建会话 loop。
func (r *Registry) Enable(ctx context.Context, id string) ([]tools.Tool, error) {
	r.mu.Lock()
	cached, ok := r.enabledTools[id]
	r.mu.Unlock()
	if ok {
		return cached, nil
	}
	c, ok := r.Get(id)
	if !ok {
		return nil, fmt.Errorf("插件 %q 不存在", id)
	}
	got, err := c.Enable(ctx) // 可能长耗时（私有 MCP 连接），锁外
	if err != nil {
		return nil, err
	}
	r.mu.Lock()
	r.enabledTools[id] = got
	r.mu.Unlock()
	return got, nil
}

// Disable 释放插件（私有连接）；从工具缓存摘除。
func (r *Registry) Disable(ctx context.Context, id string) error {
	c, ok := r.Get(id)
	if !ok {
		return fmt.Errorf("插件 %q 不存在", id)
	}
	r.mu.Lock()
	delete(r.enabledTools, id)
	r.mu.Unlock()
	return c.Disable(ctx)
}

// ClosePreserve 单插件关闭（保留持久化启用标记）：从工具缓存摘除 + 调 ClosePreserve。
// 引擎切换等编排性停用用（区别于用户显式 Disable——Disable 清持久标记，下次启动不自动恢复）。
// 无 ClosePreserver 的插件回退 Disable。
func (r *Registry) ClosePreserve(ctx context.Context, id string) error {
	c, ok := r.Get(id)
	if !ok {
		return fmt.Errorf("插件 %q 不存在", id)
	}
	r.mu.Lock()
	delete(r.enabledTools, id)
	r.mu.Unlock()
	if cp, ok := c.(ClosePreserver); ok {
		return cp.ClosePreserve(ctx)
	}
	return c.Disable(ctx)
}

// EnabledTools 全部已启用插件的工具（引擎注册数据源）。
func (r *Registry) EnabledTools(ctx context.Context) []tools.Tool {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []tools.Tool
	for _, c := range r.caps {
		if ts, ok := r.enabledTools[c.ID()]; ok {
			out = append(out, ts...)
		}
	}
	return out
}

// ClosePreserver 可选的关闭语义：Registry.Close 优先调用此方法而非 Disable。
// 用于「释放运行时资源但不改变持久化启用状态」——应用/工作区关闭时应保留"上次已启用"
// 以便下次启动自动恢复；只有用户显式 Disable 才走 Disable（清除启用标记）。
type ClosePreserver interface {
	ClosePreserve(ctx context.Context) error
}

// Close 关闭全部插件（工作区 shutdown：释放私有连接）。幂等。
// 实现 ClosePreserver 的插件保留持久化启用状态（下次启动自动恢复）；
// 否则回退 Disable。
func (r *Registry) Close(ctx context.Context) {
	for _, c := range r.caps {
		if cp, ok := c.(ClosePreserver); ok {
			_ = cp.ClosePreserve(ctx)
		} else {
			_ = c.Disable(ctx)
		}
	}
	r.mu.Lock()
	r.enabledTools = map[string][]tools.Tool{}
	r.mu.Unlock()
}

// Snapshot 序列化全部插件状态（plugin_list 数据源；按 id 排序稳定）。
func (r *Registry) Snapshot(ctx context.Context) []State {
	out := make([]State, 0, len(r.caps))
	for _, c := range r.caps {
		out = append(out, c.State(ctx))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Status < out[j].Status }) // 无 id 字段，按状态排稳定
	return out
}
