package mcp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/mark3labs/mcp-go/client/transport"
	gomcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/tools"
)

// flakyTransport 可遥控「会话失效」的传输：正常时透传给 in-process 传输；dead=true 时
// 所有请求立刻返回 transport.ErrSessionTerminated —— 与 streamable HTTP 在服务端重启 /
// 空闲回收后收到的 404 同形（mcp-go v0.58 streamable_http.go 直接返回该哨兵）。
type flakyTransport struct {
	inner transport.Interface
	dead  atomic.Bool
}

func (f *flakyTransport) Start(ctx context.Context) error { return f.inner.Start(ctx) }

func (f *flakyTransport) SendRequest(ctx context.Context, req transport.JSONRPCRequest) (*transport.JSONRPCResponse, error) {
	if f.dead.Load() {
		return nil, transport.ErrSessionTerminated
	}
	return f.inner.SendRequest(ctx, req)
}

func (f *flakyTransport) SendNotification(ctx context.Context, n gomcp.JSONRPCNotification) error {
	if f.dead.Load() {
		return transport.ErrSessionTerminated
	}
	return f.inner.SendNotification(ctx, n)
}

func (f *flakyTransport) SetNotificationHandler(h func(gomcp.JSONRPCNotification)) {
	f.inner.SetNotificationHandler(h)
}

func (f *flakyTransport) Close() error         { return f.inner.Close() }
func (f *flakyTransport) GetSessionId() string { return f.inner.GetSessionId() }

// flakyFactory 每次 dial 造一个新传输并记住它（遥控当前连接 + 统计重拨次数）。
type flakyFactory struct {
	srv  *server.MCPServer
	mu   sync.Mutex
	live []*flakyTransport
}

func (f *flakyFactory) transportFor(_ ServerConfig) (transport.Interface, error) {
	t := &flakyTransport{inner: transport.NewInProcessTransport(f.srv)}
	f.mu.Lock()
	f.live = append(f.live, t)
	f.mu.Unlock()
	return t, nil
}

// current 最近一次 dial 得到的传输（= 当前会话）。
func (f *flakyFactory) current() *flakyTransport {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.live) == 0 {
		return nil
	}
	return f.live[len(f.live)-1]
}

func (f *flakyFactory) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.live)
}

// newFlakyManager 建 Manager + 可遥控传输的管理器（含一个只读工具 echo）。
func newFlakyManager() (*Manager, *flakyFactory) {
	srv := server.NewMCPServer("flaky", "1.0.0", server.WithToolCapabilities(true))
	srv.AddTool(
		gomcp.NewTool("echo",
			gomcp.WithDescription("回显"),
			gomcp.WithReadOnlyHintAnnotation(true),
			gomcp.WithString("text", gomcp.Description("文本"))),
		func(_ context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
			text, _ := req.GetArguments()["text"].(string)
			return &gomcp.CallToolResult{Content: []gomcp.Content{gomcp.NewTextContent("echo: " + text)}}, nil
		},
	)
	f := &flakyFactory{srv: srv}
	m := &Manager{servers: map[string]*Server{}}
	m.transportFor = f.transportFor
	m.SetConfig(map[string]ServerConfig{"srv": {Type: "http", URL: "http://127.0.0.1:9"}})
	return m, f
}

// engineFor 用管理器的工具适配器建一个引擎（走真实调用路径）。
func engineFor(ctx context.Context, m *Manager) *tools.Engine {
	eng := tools.NewToolEngine()
	for _, tl := range m.Tools(ctx) {
		eng.RegisterTool(ctx, tl)
	}
	return eng
}

func serverStatus(t *testing.T, m *Manager, name string) ServerStatus {
	t.Helper()
	for _, st := range m.Status() {
		if st.Name == name {
			return st
		}
	}
	t.Fatalf("状态里没有服务器 %q", name)
	return ServerStatus{}
}

// TestCallFailureMarksBrokenAndRecoversOnNextCall 「用一次就自愈」的完整闭环。
//
// 回归两处缺口（ai-test 真机踩过）：
//  1. callTool 失败原本**不改 state** —— 死了的连接永远显示 connected，每次调用都打在同一
//     具尸体上；只有 mcp_refresh / 改配置 / 重启桥能救。
//  2. 打回错误态后**不能**进 retryCooldown（默认 10s）：否则用户重试的那一次直接拿到缓存
//     错误，体验上仍然是「不会恢复」。
func TestCallFailureMarksBrokenAndRecoversOnNextCall(t *testing.T) {
	m, f := newFlakyManager()
	ctx := context.Background()
	eng := engineFor(ctx, m)

	if res := seq(eng, ctx, core.ToolCall{Id: "1", Name: "mcp__srv__echo", Arguments: `{"text":"hi"}`}); res.IsError {
		t.Fatalf("首次调用不该失败: %s", res.Result)
	}
	if got := serverStatus(t, m, "srv").State; got != "connected" {
		t.Fatalf("状态 = %q, want connected", got)
	}

	// 服务端重启 / 会话被回收：当前会话从此 404。
	f.current().dead.Store(true)
	res := seq(eng, ctx, core.ToolCall{Id: "2", Name: "mcp__srv__echo", Arguments: `{"text":"hi"}`})
	if !res.IsError {
		t.Fatalf("死连接上的调用应报错，实际成功: %s", res.Result)
	}
	st := serverStatus(t, m, "srv")
	if st.State != "error" {
		t.Fatalf("调用失败后状态 = %q（应打回 error，否则永远显示已连接）", st.State)
	}
	if st.Error == "" {
		t.Errorf("错误应可见（mcp_list 展示）")
	}

	// 下一次调用必须自动重拨成功（不需要人工 mcp_refresh）。
	res = seq(eng, ctx, core.ToolCall{Id: "3", Name: "mcp__srv__echo", Arguments: `{"text":"again"}`})
	if res.IsError {
		t.Fatalf("重拨后应成功，实际: %s", res.Result)
	}
	if !contains(res.Result, "echo: again") {
		t.Fatalf("结果 = %q", res.Result)
	}
	if f.count() < 2 {
		t.Fatalf("dial 次数 = %d，应重拨过（>=2）", f.count())
	}
}

// TestWatchHealthRepairsDroppedSession 「闲着也自愈」：没人调用的服务器会话失效后，
// 低频探活（Ping 失败 → Refresh）应把它重连好，用户下一次调用不必先吃一次失败。
func TestWatchHealthRepairsDroppedSession(t *testing.T) {
	m, f := newFlakyManager()
	ctx := context.Background()
	eng := engineFor(ctx, m)
	if res := seq(eng, ctx, core.ToolCall{Id: "1", Name: "mcp__srv__echo", Arguments: `{"text":"hi"}`}); res.IsError {
		t.Fatalf("首次调用不该失败: %s", res.Result)
	}

	f.current().dead.Store(true) // 放置期间服务端悄悄重启
	wctx, cancel := context.WithCancel(ctx)
	defer cancel()
	go m.WatchHealth(wctx, 10*time.Millisecond)

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if f.count() >= 2 && serverStatus(t, m, "srv").State == "connected" {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if f.count() < 2 {
		t.Fatalf("探活没有重拨（dial 次数 = %d）", f.count())
	}
	if st := serverStatus(t, m, "srv").State; st != "connected" {
		t.Fatalf("探活后状态 = %q, want connected", st)
	}
	// 重连后直接可用（这一条才是用户感知：不必先失败一次）。
	if res := seq(eng, ctx, core.ToolCall{Id: "2", Name: "mcp__srv__echo", Arguments: `{"text":"x"}`}); res.IsError {
		t.Fatalf("探活重连后调用应成功: %s", res.Result)
	}
}

// TestWatchHealthSkipsNeverConnectedServers 探活不得破坏「惰性连接」：没人用过的服务器
// 不该被探活顺手 dial 出来（stdio 会因此 spawn 子进程）。
func TestWatchHealthSkipsNeverConnectedServers(t *testing.T) {
	m, f := newFlakyManager()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Millisecond)
	defer cancel()
	m.WatchHealth(ctx, 10*time.Millisecond)
	if f.count() != 0 {
		t.Fatalf("探活为从未连接的服务器建了 %d 次连（应 0）", f.count())
	}
	if st := serverStatus(t, m, "srv").State; st != "idle" {
		t.Fatalf("状态 = %q, want idle", st)
	}
}

// TestWatchHealthStopsOnCancel ctx 结束即返回（工作区删除/进程退出时不留 goroutine）。
func TestWatchHealthStopsOnCancel(t *testing.T) {
	m, _ := newFlakyManager()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { m.WatchHealth(ctx, time.Hour); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("WatchHealth 未随 ctx 退出")
	}
}

// TestTransportDead 判定表：哪些错误意味着「连接死了」（该重拨），哪些是业务错误（不该）。
func TestTransportDead(t *testing.T) {
	cases := []struct {
		err  error
		want bool
		why  string
	}{
		{transport.ErrSessionTerminated, true, "streamable HTTP 会话 404（服务端重启/空闲回收）"},
		{transport.ErrTransportClosed, true, "stdio 传输关闭"},
		{fmt.Errorf("request failed: %w", transport.ErrSessionTerminated), true, "被包一层也能认出来"},
		{io.EOF, true, "对端关闭"},
		{fmt.Errorf("read: %w", io.ErrUnexpectedEOF), true, "流被截断"},
		{errors.New("invalid params: missing field project"), false, "JSON-RPC 业务错误（重拨也照样错）"},
		{errors.New("tool returned 400"), false, "远端业务失败"},
		{nil, false, "没错误"},
	}
	for _, c := range cases {
		if got := transportDead(c.err); got != c.want {
			t.Errorf("transportDead(%v) = %v, want %v（%s）", c.err, got, c.want, c.why)
		}
	}
}

// TestCallerCancelDoesNotKillConnection 调用方自己取消/超时不是「连接坏了」：
// 长跑工具超时后连接必须还能继续用（否则每次超时都要重拨，stdio 会反复重启子进程）。
func TestCallerCancelDoesNotKillConnection(t *testing.T) {
	m, _ := newFlakyManager()
	ctx := context.Background()
	eng := engineFor(ctx, m)
	if res := seq(eng, ctx, core.ToolCall{Id: "1", Name: "mcp__srv__echo", Arguments: `{"text":"hi"}`}); res.IsError {
		t.Fatalf("首次调用不该失败: %s", res.Result)
	}
	s := m.servers["srv"]
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	_, _ = s.callTool(cctx, "echo", nil) // 结果不论（in-process 可能不看 ctx），关键是状态不能被判死
	if !s.Connected() {
		t.Fatalf("调用方取消后连接被判死了（应保持 connected）")
	}
}
