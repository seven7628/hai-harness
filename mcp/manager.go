package mcp

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"sync"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	gomcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/seven7628/hai-harness/tools"
)

// Manager 每工作区一个 MCP 服务器管理器：跨会话共享连接（一个 stdio 子进程服务
// 该工作区全部会话），配置变更热重载（SetConfig 差分 apply：关闭移除、重建变更、
// 保留未变）。
type Manager struct {
	mu      sync.Mutex
	servers map[string]*Server
	order   []string // 按名排序，Status/Tools 遍历稳定

	// src 每台服务器的配置来源标签（Layer.Source，"user"/"project"；Status 展示用）。
	// 与 servers 分开存：来源变化不应触发连接重建（差分只比 cfg）。
	src map[string]string

	// transportFor 测试注入（in-process）；nil = 按 cfg 构建 stdio/http/sse。
	// 返回的传输必须是全新实例（每次连接独立绑定）。
	transportFor func(ServerConfig) (transport.Interface, error)

	// DefaultExposure 未在 ServerConfig 显式声明 exposure 的 server，其工具落到哪个**引擎档**。
	//
	// 零值 = tools.ExposureDeferred（设计文档 §18 的出厂默认：不进模型工具表、也不进
	// codemode 描述 —— 几十个 server 的工具描述不会灌进描述、也不会随连接抖动）。
	//
	// ⚠️ 装配点的义务：deferred 档的**激活通道**是 codemode 的 searchTools() / tool_search
	// （Wave 2/3）。在通道存在之前把它设成 deferred，等价于把 MCP 功能对模型整体关闭
	// （既不在 ToolParams、也没有任何东西能把它加载回来）。故 desktop/bridge **按 codemode 开关
	// 条件化**设置它：codemode 激活（searchTools 可用）→ deferred，未激活 → direct ——
	// 这一条是装配决策，不放进本包（本包只管照做 + 显式配置优先）。
	// Wave 3 落地点：desktop/bridge/codemode.go 的 mcpDefaultExposure / applyMCPDefaultExposure。
	DefaultExposure tools.ToolExposure
}

// NewManager 按配置建管理器（servers 仅持有配置，连接惰性：首次 Tools 触发——
// Status 只读快照，不建连接）。
func NewManager(cfg map[string]ServerConfig) *Manager {
	return NewManagerWithSource(cfg, nil)
}

// NewManagerWithSource 建管理器并记录每台服务器的配置来源（分层加载：Layer.Source）。
// source 仅影响 Status().Source 展示，不影响连接/差分。
func NewManagerWithSource(cfg map[string]ServerConfig, source map[string]string) *Manager {
	m := &Manager{servers: map[string]*Server{}}
	m.SetConfigWithSource(cfg, source)
	return m
}

// NewManagerWithTransport 建管理器并注入传输构造器（测试用：in-process 传输连到
// fixture 服务器，不 spawn 子进程；nil = 按 cfg 构建 stdio/http/sse）。插件包
// （plugin/browser）用此构造私有 MCP 实例并注入测试传输。
func NewManagerWithTransport(cfg map[string]ServerConfig, transportFor func(ServerConfig) (transport.Interface, error)) *Manager {
	m := &Manager{servers: map[string]*Server{}, transportFor: transportFor}
	m.SetConfig(cfg)
	return m
}

// SetConfig 差分应用配置（不标注来源；= SetConfigWithSource(cfg, nil)）。
func (m *Manager) SetConfig(cfg map[string]ServerConfig) {
	m.SetConfigWithSource(cfg, nil)
}

// SetConfigWithSource 差分应用配置并更新每台服务器的来源标签（source 为 nil = 清空来源）。
//
// 差分语义：删除的服务器关闭并移除；新增/变更的建新 Server（旧连接释放）；未变的保留
// 连接（会话重建不抖动子进程）。顺序按名排序。校验统一（V2 P1-PROTOCOL-06）：非法配置
// （未知 type/缺必填）跳过——与 LoadConfig 语义一致，不静默按 stdio 处理；调用方可经
// Status 观察被跳过的项。来源只用于 Status().Source 展示：仅来源变化不重建连接（差分只比 cfg）。
func (m *Manager) SetConfigWithSource(cfg map[string]ServerConfig, source map[string]string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.src = source
	// 先校验全部：非法项剔除（保持原配置不建 Server）
	valid := make(map[string]ServerConfig, len(cfg))
	for name, c := range cfg {
		norm, err := c.Validate()
		if err != nil {
			continue // 非法配置跳过（与 LoadConfig 同语义；Status 可见缺失）
		}
		valid[name] = norm
	}
	cfg = valid
	// 关闭被移除的服务器
	for name, s := range m.servers {
		if _, ok := cfg[name]; !ok {
			_ = s.Close()
			delete(m.servers, name)
		}
	}
	names := make([]string, 0, len(cfg))
	for name := range cfg {
		names = append(names, name)
	}
	sort.Strings(names)
	m.order = names
	for _, name := range names {
		c := cfg[name]
		if s, ok := m.servers[name]; ok {
			if !reflect.DeepEqual(s.cfg, c) {
				_ = s.Close()
				m.servers[name] = newServer(name, c, m.transportFor)
			}
		} else {
			m.servers[name] = newServer(name, c, m.transportFor)
		}
	}
}

// enabledServers 取启用的服务器列表（按稳定顺序）。调用方持锁。
func (m *Manager) enabledServers() []*Server {
	out := make([]*Server, 0, len(m.order))
	for _, name := range m.order {
		if s := m.servers[name]; s != nil && s.cfg.IsEnabled() {
			out = append(out, s)
		}
	}
	return out
}

// Tools 返回全部启用服务器的工具适配器（惰性连接；连接失败的服务器跳过，经 Status 可见）。
// 每次调用建全新适配器（引用共享 Server 连接）——会话构建/重建各取一份即可。
func (m *Manager) Tools(ctx context.Context) []tools.Tool {
	m.mu.Lock()
	servers := m.enabledServers()
	m.mu.Unlock()
	var out []tools.Tool
	for _, s := range servers {
		for _, t := range s.Tools(ctx) {
			out = append(out, newAdapterWithDefault(s, t, m.DefaultExposure))
		}
	}
	return out
}

// Status 全部服务器状态快照（UI 展示：连接状态/工具数/错误/配置来源）。
func (m *Manager) Status() []ServerStatus {
	m.mu.Lock()
	servers := make([]*Server, 0, len(m.order))
	src := make(map[string]string, len(m.src))
	for k, v := range m.src {
		src[k] = v
	}
	for _, name := range m.order {
		if s := m.servers[name]; s != nil {
			servers = append(servers, s)
		}
	}
	m.mu.Unlock()
	out := make([]ServerStatus, 0, len(servers))
	for _, s := range servers {
		st := s.Status()
		st.Source = src[s.name] // 分层加载的来源标签（"user"/"project"；未标注为空）
		out = append(out, st)
	}
	return out
}

// Refresh 强制重连单个服务器（断线重试/配置后手动刷新）：关闭旧连接 → 重连。
// 重连后会话工具集自动跟随（适配器引用 Server，下次工具轮生效）。
func (m *Manager) Refresh(ctx context.Context, name string) error {
	m.mu.Lock()
	s := m.servers[name]
	m.mu.Unlock()
	if s == nil {
		return fmt.Errorf("mcp 服务器 %q 不存在", name)
	}
	_ = s.Close()
	return s.EnsureConnected(ctx)
}

// WarmupResult 预热结果（P0/P1：启用后后台建连）。
type WarmupResult struct {
	Server     string
	Ready      bool
	DurationMs int64
	Stage      string
	Error      error
}

// Warmup 预热连接单个服务器（不枚举用户 MCP server；只连指定私有 server）。
// 幂等：已连接立即返回 ready；失败返回结构化错误（不阻塞，供状态呈现）。
func (m *Manager) Warmup(ctx context.Context, name string) WarmupResult {
	m.mu.Lock()
	s := m.servers[name]
	m.mu.Unlock()
	if s == nil {
		return WarmupResult{Server: name, Ready: false, Error: fmt.Errorf("mcp 服务器 %q 不存在", name)}
	}
	start := time.Now()
	err := s.EnsureConnected(ctx)
	dur := time.Since(start).Milliseconds()
	st := s.Status()
	if err != nil {
		return WarmupResult{Server: name, Ready: false, DurationMs: dur, Stage: st.Phase, Error: err}
	}
	return WarmupResult{Server: name, Ready: true, DurationMs: dur, Stage: st.Phase}
}

// Ping 探活单个服务器（健康检查；未连接/探活失败返回错误，调用方据此决定 Refresh）。
func (m *Manager) Ping(ctx context.Context, name string) error {
	m.mu.Lock()
	s := m.servers[name]
	m.mu.Unlock()
	if s == nil {
		return fmt.Errorf("mcp 服务器 %q 不存在", name)
	}
	return s.Ping(ctx)
}

// defaultHealthInterval 低频探活周期（没传/传非正值时用）。
//
// 60s 的取舍：比一次对话的思考+工具轮短，用户撞上死连接的概率可忽略；又远大于服务器
// 重启/回收的时间尺度之外的人工操作频率，一轮探活在面板里是零噪声。
const defaultHealthInterval = 60 * time.Second

// WatchHealth 低频探活 + 失败自动重连，直到 ctx 结束。
//
// **只 Ping 已连接（StateConnected）的服务器**：没人用过的 server 不该被探活顺手 dial 出来
// （stdio 会 spawn 子进程、http 会建会话），"惰性连接"这条设计不能被探活破坏。
//
// 失败即 Refresh（Close + 重连）：这样下一次工具调用不会打在死连接上。两条自愈通道各有
// 分工，缺一不可：
//   - 用一次就自愈：Server.callTool 失败 → markBroken → 下一次调用重拨（延迟最低，
//     但只覆盖"真的被调用过"的服务器）；
//   - 闲着也自愈：本方法（覆盖"放着不用、服务端先重启"——用户下次打开面板或发起对话时
//     连接已经好了，不必先吃一次失败）。
//
// interval <= 0 用 defaultHealthInterval。阻塞直到 ctx 结束；调用方负责 goroutine。
func (m *Manager) WatchHealth(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultHealthInterval
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			m.probeOnce(ctx)
		}
	}
}

// probeOnce 一轮探活（WatchHealth 的 tick 体；单独拆出便于测试与将来手动触发）。
func (m *Manager) probeOnce(ctx context.Context) {
	m.mu.Lock()
	servers := make(map[string]*Server, len(m.servers))
	for name, s := range m.servers {
		servers[name] = s
	}
	m.mu.Unlock()
	for name, s := range servers {
		if ctx.Err() != nil {
			return
		}
		if !s.Connected() {
			continue // 惰性连接：未连接的服务器不碰
		}
		pctx, cancel := context.WithTimeout(ctx, pingTimeout)
		err := s.Ping(pctx)
		cancel()
		if err == nil {
			continue
		}
		if ctx.Err() != nil {
			return
		}
		_ = m.Refresh(ctx, name) // 断线：Close + 重连；失败留给 Status（错误码/阶段可见）
	}
}

// CallTool 原始调用单个服务器工具（返回 mcp-go 结果含 content 原文；web tab 截图等场景）。
func (m *Manager) CallTool(ctx context.Context, server, tool string, arguments map[string]any) (*gomcp.CallToolResult, error) {
	m.mu.Lock()
	s := m.servers[server]
	m.mu.Unlock()
	if s == nil {
		return nil, fmt.Errorf("mcp 服务器 %q 不存在", server)
	}
	// 与适配器路径一致：调用前确保连接（幂等；断线后自动重拨，见 Server.markBroken）。
	if err := s.EnsureConnected(ctx); err != nil {
		return nil, err
	}
	return s.CallTool(ctx, tool, arguments)
}

// Close 关闭全部服务器连接（工作区 shutdown 时调用；幂等）。
func (m *Manager) Close() {
	m.mu.Lock()
	servers := m.servers
	m.servers = map[string]*Server{}
	m.mu.Unlock()
	for _, s := range servers {
		_ = s.Close()
	}
}

// ServerStatus 单服务器状态快照（UI 展示 + P0 观测）。
type ServerStatus struct {
	Name      string
	Type      string
	Command   string // stdio: command；http/sse: url
	Enabled   bool
	State     string // "idle" | "connected" | "error"
	ToolCount int
	Error     string

	// Source 配置来源层标签（分层加载：Layer.Source，"user"/"project"；未标注为空）。
	Source string `json:"source,omitempty"`

	// —— P0 观测契约扩展（§3.1）——
	Phase          string    `json:"phase,omitempty"`            // 连接阶段
	LastAttempt    time.Time `json:"last_attempt,omitempty"`     // 最近尝试时刻
	LastSuccess    time.Time `json:"last_success,omitempty"`     // 最近成功时刻
	LastDurationMs int64     `json:"last_duration_ms,omitempty"` // 最近一次连接耗时 ms
	Attempt        int       `json:"attempt,omitempty"`          // 累计尝试次数
	ErrorCode      string    `json:"error_code,omitempty"`       // 结构化错误码
}
