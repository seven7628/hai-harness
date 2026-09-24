package mcp

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	gomcp "github.com/mark3labs/mcp-go/mcp"

	"github.com/seven7628/hai-harness/plugin/browser/trace"
)

// connectTimeout 单次连接上限（spawn 子进程 + 初始化协商）。超时标记 Error，
// 不阻塞会话构建（buildLoop 内 Tools() 受此约束）。
const connectTimeout = 15 * time.Second

// retryCooldown 连接失败后的冷却：窗口内 EnsureConnected 直接返回上次错误，
// 防止坏服务器在每次会话构建/重建时反复阻塞（thundering herd）。
const retryCooldown = 10 * time.Second

// ServerState 单服务器连接状态（UI 展示 + 调试）。
type ServerState int

const (
	StateIdle ServerState = iota // 未连接（尚未触发）
	StateConnected
	StateError
)

// ConnectPhase 连接过程阶段（P0 观测契约：回答「失败在哪一阶段」）。
type ConnectPhase string

const (
	PhaseIdle       ConnectPhase = "idle"
	PhaseSpawn      ConnectPhase = "spawn"
	PhaseInitialize ConnectPhase = "initialize"
	PhaseListTools  ConnectPhase = "list_tools"
	PhaseConnected  ConnectPhase = "connected"
	PhaseError      ConnectPhase = "error"
)

// Server 单个 MCP 服务器：持有 mcp-go 客户端，跨会话共享（每工作区一个 Manager）。
// 连接惰性：首次 Tools()/Status() 触发；成功后复用，断线由 Refresh 重连。
type Server struct {
	name string
	cfg  ServerConfig

	mu          sync.Mutex
	client      *mcpclient.Client
	tools       []Tool
	state       ServerState
	lastErr     error
	lastAttempt time.Time

	// instructions initialize 返回的 server instructions（服务器自述的使用策略）。
	// 例：graft 返回 953 字符的「什么时候用哪个工具、别链式调用」。
	// 早期版本丢弃了它（if _, err := c.Initialize(...)）——模型只看到一堆同名工具，
	// 退化风险高。经 Manager.Instructions 汇总后可注入系统提示词。
	instructions string

	// —— P0 观测契约（§3.1）——
	phase       ConnectPhase
	lastSuccess time.Time
	lastDurMs   int64
	attempt     int
	errorCode   string

	// connecting 连接 single-flight：并发 EnsureConnected 只允许一个 goroutine dial，
	// 其余等待同一结果（防止页列表/截图/agent 工具各自 spawn 子进程）。
	connecting chan struct{}
	// closed Close 已调用（dial 完成后据此丢弃新 client，不复活已关闭 Server）。
	closed bool

	// ctx/cancel：Server 生命周期上下文（跨连接存活）。传输 Start 用此 ctx——
	// mcp-go Stdio 用 exec.CommandContext 把子进程绑到 Start 的 ctx，若绑到单次连接
	// 的超时 ctx，dial 返回 cancel 会杀掉子进程（transport closed）。请求（Initialize/
	// ListTools/CallTool）用调用方 ctx（请求级超时），与传输生命周期解耦。
	ctx    context.Context
	cancel context.CancelFunc

	// transportFor 测试注入：nil = 按 cfg 构建 stdio/http/sse 传输。
	// 返回的传输必须在每次调用时是全新的（in-process 传输单次绑定）。
	transportFor func(cfg ServerConfig) (transport.Interface, error)
}

func newServer(name string, cfg ServerConfig, tr func(ServerConfig) (transport.Interface, error)) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	return &Server{name: name, cfg: cfg, transportFor: tr, ctx: ctx, cancel: cancel}
}

// Name 服务器名（配置键，工具前缀的一部分）。
func (s *Server) Name() string { return s.name }

// Config 服务器配置的深拷贝（Args/Env 是 slice/map，浅拷贝会被调用方修改污染内部配置）。
func (s *Server) Config() ServerConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	cfg := s.cfg
	if cfg.Args != nil {
		cfg.Args = append([]string(nil), cfg.Args...)
	}
	if cfg.Env != nil {
		env := make(map[string]string, len(cfg.Env))
		for k, v := range cfg.Env {
			env[k] = v
		}
		cfg.Env = env
	}
	return cfg
}

// EnsureConnected 惰性连接（幂等）：已连接跳过；失败记录状态并返回错误。
// 失败后 retryCooldown 内直接返回缓存错误（不重连），窗口外或 Refresh 重试。
// single-flight：同一 Server 并发调用时只允许一个 goroutine dial，其余等待同一结果。
func (s *Server) EnsureConnected(ctx context.Context) error {
	s.mu.Lock()
	if s.state == StateConnected && s.client != nil {
		s.mu.Unlock()
		return nil
	}
	if s.state == StateError && s.lastErr != nil && time.Since(s.lastAttempt) < retryCooldown {
		err := s.lastErr
		s.mu.Unlock()
		return err
	}
	if s.connecting != nil {
		// 已有 goroutine 在建连：等待其结果
		wait := s.connecting
		s.mu.Unlock()
		<-wait
		s.mu.Lock()
		if s.state == StateConnected && s.client != nil {
			s.mu.Unlock()
			return nil
		}
		if s.lastErr != nil {
			err := s.lastErr
			s.mu.Unlock()
			return err
		}
		s.mu.Unlock()
		return fmt.Errorf("mcp %s: 连接未完成", s.name)
	}
	// 本 goroutine 负责建连
	done := make(chan struct{})
	s.connecting = done
	s.closed = false // 新一轮连接：Close 后的重连允许（Reset 语义）
	s.lastAttempt = time.Now()
	s.attempt++
	s.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, connectTimeout)
	defer cancel()

	start := time.Now()
	c, tools, err := s.dial(ctx)
	durMs := time.Since(start).Milliseconds()

	s.mu.Lock()
	s.lastDurMs = durMs
	// Close 可能已把 connecting 置 nil 并 close 了旧通道：只有当前仍指向本 done 才 close
	if s.connecting == done {
		close(done)
		s.connecting = nil
	}
	if err != nil {
		s.state = StateError
		s.lastErr = err
		s.phase = PhaseError
		s.errorCode = phaseErrorCode(err)
		s.mu.Unlock()
		return err
	}
	if s.closed {
		// Close 已发生：不复活连接（丢弃新 client，避免已关闭 Server 悬挂活 client）
		s.mu.Unlock()
		_ = c.Close()
		return fmt.Errorf("mcp %s: 连接已关闭", s.name)
	}
	s.client = c
	s.tools = tools
	s.state = StateConnected
	s.lastErr = nil
	s.phase = PhaseConnected
	s.errorCode = ""
	s.lastSuccess = time.Now()
	s.mu.Unlock()
	return nil
}

// phaseErrorCode 从连接错误映射结构化错误码（§2.2）。
func phaseErrorCode(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	switch {
	case containsAny(msg, "初始化", "initialize"):
		return "BROWSER_MCP_INITIALIZE_FAILED"
	case containsAny(msg, "列工具", "list tools", "ListTools"):
		return "BROWSER_MCP_LIST_TOOLS_FAILED"
	case containsAny(msg, "启动", "spawn", "transport", "closed"):
		return "BROWSER_MCP_CONNECT_TIMEOUT"
	default:
		return "BROWSER_MCP_DISCONNECTED"
	}
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// dial 建连 + 握手 + 拉取工具清单（每次调用创建新客户端；连接失败无泄漏）。
// 记录各阶段（spawn/initialize/list_tools），defer 保证错误也落到观测字段。
func (s *Server) dial(ctx context.Context) (*mcpclient.Client, []Tool, error) {
	s.setPhase(PhaseSpawn)
	var tr transport.Interface
	var err error
	if s.transportFor != nil {
		tr, err = s.transportFor(s.cfg)
	} else {
		tr, err = s.buildTransport()
	}
	if err != nil {
		return nil, nil, fmt.Errorf("mcp %s: 传输: %w", s.name, err)
	}
	c := mcpclient.NewClient(tr)
	if err := c.Start(s.serverCtx()); err != nil { // 传输绑定 Server 生命周期 ctx（子进程不随请求超时被杀）
		_ = c.Close()
		return nil, nil, fmt.Errorf("mcp %s: 启动: %w", s.name, err)
	}
	s.setPhase(PhaseInitialize)
	initReq := gomcp.InitializeRequest{}
	initReq.Params.ProtocolVersion = gomcp.LATEST_PROTOCOL_VERSION
	initReq.Params.ClientInfo = gomcp.Implementation{Name: "go-code", Version: "0.1.0"}
	if res, err := c.Initialize(ctx, initReq); err != nil {
		_ = c.Close()
		return nil, nil, fmt.Errorf("mcp %s: 初始化: %w", s.name, err)
	} else {
		// 收下 server instructions（可能是空串）：它是服务器对"什么时候用哪个工具"的
		// 自述策略，Claude Code / Codex 都会把它注入系统提示词。丢弃它 = 白白浪费
		// 第三方工具最重要的引导信息（graft 的 instructions 有 953 字符）。
		s.mu.Lock()
		s.instructions = strings.TrimSpace(res.Instructions)
		s.mu.Unlock()
	}
	s.setPhase(PhaseListTools)
	lr, err := c.ListTools(ctx, gomcp.ListToolsRequest{})
	if err != nil {
		_ = c.Close()
		return nil, nil, fmt.Errorf("mcp %s: 列工具: %w", s.name, err)
	}
	tools := make([]Tool, 0, len(lr.Tools))
	for _, t := range lr.Tools {
		tools = append(tools, newTool(s.name, t))
	}
	return c, tools, nil
}

// setPhase 更新连接阶段（持锁安全）。
func (s *Server) setPhase(p ConnectPhase) {
	s.mu.Lock()
	s.phase = p
	s.mu.Unlock()
}

// buildTransport 按配置构建传输：stdio 子进程（继承父环境 + cfg.Env）、http/sse。
// 未知 type 显式报错（不再静默当 stdio——绕过 Validate 直接构造时会 spawn 不存在
// 的命令；V2 P1-PROTOCOL-06 校验统一）。
func (s *Server) buildTransport() (transport.Interface, error) {
	switch s.cfg.Type {
	case "http", "streamable_http":
		return transport.NewStreamableHTTP(s.cfg.URL)
	case "sse":
		return transport.NewSSE(s.cfg.URL)
	case "stdio":
		env := make([]string, 0, len(s.cfg.Env))
		for k, v := range s.cfg.Env {
			env = append(env, k+"="+v)
		}
		return transport.NewCommandWithEnv(s.cfg.Command, env, s.cfg.Args...), nil
	default:
		return nil, fmt.Errorf("mcp %s: 未知 transport type %q（支持 stdio/http/sse）", s.name, s.cfg.Type)
	}
}

// Tools 返回已连接服务器的工具描述（未连接则先连；失败返回 nil）。
func (s *Server) Tools(ctx context.Context) []Tool {
	if err := s.EnsureConnected(ctx); err != nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.tools
}

// Status 服务器状态快照（UI 展示 + P0 观测）。
func (s *Server) Status() ServerStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	st := ServerStatus{Name: s.name, Type: s.cfg.Type, Enabled: s.cfg.IsEnabled()}
	if s.cfg.Type == "stdio" {
		st.Command = s.cfg.Command
	} else {
		st.Command = s.cfg.URL
	}
	st.Phase = string(s.phase)
	st.LastAttempt = s.lastAttempt
	st.LastSuccess = s.lastSuccess
	st.LastDurationMs = s.lastDurMs
	st.Attempt = s.attempt
	st.ErrorCode = s.errorCode
	switch s.state {
	case StateConnected:
		st.State = "connected"
		st.ToolCount = len(s.tools)
	case StateError:
		st.State = "error"
		if s.lastErr != nil {
			st.Error = s.lastErr.Error()
		}
	default:
		st.State = "idle"
	}
	return st
}

// Close 关闭客户端连接（幂等）。关闭后 EnsureConnected 可重连（新客户端 + 新生命周期 ctx）。
func (s *Server) Close() error {
	s.mu.Lock()
	client := s.client
	oldCancel := s.cancel
	connecting := s.connecting
	s.client = nil
	s.tools = nil
	s.state = StateIdle
	s.lastErr = nil
	s.errorCode = ""
	s.phase = PhaseIdle
	s.closed = true
	s.connecting = nil                                         // 置 nil 避免与 dial 完成时的 close(done) 竞争（dial 侧检查 s.connecting==done）
	s.ctx, s.cancel = context.WithCancel(context.Background()) // 重建：可重连（旧 ctx 作废）
	s.mu.Unlock()
	if connecting != nil {
		// 等待中的连接请求会看到「未连接」错误（dial 失败/被取消），不留 goroutine 在旧 client 上写
		close(connecting)
	}
	if client != nil {
		client.Close()
	}
	oldCancel() // 结束旧传输生命周期（exec.CommandContext 杀 stdio 子进程）；新 ctx 已就绪
	return nil
}

// serverCtx 取 Server 生命周期 ctx（dial 时用；Close 可能并发重建，须持锁读）。
func (s *Server) serverCtx() context.Context {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ctx
}

// callTool 执行一次 MCP 工具调用（适配器 Call 的底层）。arguments nil 归一化为空 map
// （mcp-go 把 nil 序列化为 arguments:null，多数服务器拒绝「expected record」）。
func (s *Server) callTool(ctx context.Context, toolName string, arguments map[string]any) (*gomcp.CallToolResult, error) {
	trStart := time.Now()
	defer func() { trace.Phase("mcp.callTool("+toolName+")", trStart) }()
	trace.Log("mcp.callTool.begin", toolName)
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("mcp %s: 未连接", s.name)
	}
	if arguments == nil {
		arguments = map[string]any{}
	}
	return client.CallTool(ctx, gomcp.CallToolRequest{
		Params: gomcp.CallToolParams{Name: toolName, Arguments: arguments},
	})
}

// CallTool 执行工具调用并返回原始结果（含 content/StructuredContent 原文；常规路径走
// 适配器 serializeResult，需要图片 base64 / 结构化 pages 等原文时用此）。
// arguments nil 归一化为空 map（mcp-go 把 nil 序列化为 arguments:null，多数服务器拒绝）。
func (s *Server) CallTool(ctx context.Context, toolName string, arguments map[string]any) (*gomcp.CallToolResult, error) {
	if arguments == nil {
		arguments = map[string]any{}
	}
	return s.callTool(ctx, toolName, arguments)
}

// pingTimeout 健康探活超时（对齐 pi-browser-use MCP_HEALTH_TIMEOUT_MS=5s）。
const pingTimeout = 5 * time.Second

// Ping 探活（健康检查）：已连接则 Ping 远端（传输死亡会在此暴露）；未连接返回错误。
func (s *Server) Ping(ctx context.Context) error {
	s.mu.Lock()
	client := s.client
	s.mu.Unlock()
	if client == nil {
		return fmt.Errorf("mcp %s: 未连接", s.name)
	}
	ctx, cancel := context.WithTimeout(ctx, pingTimeout)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		s.mu.Lock()
		s.errorCode = "BROWSER_MCP_DISCONNECTED"
		s.mu.Unlock()
		return fmt.Errorf("mcp %s: ping: %w", s.name, err)
	}
	return nil
}
