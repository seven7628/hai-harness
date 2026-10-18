package mcp

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/server"
)

// —— 为什么这组测试用真实 HTTP 服务器而不是注入传输 ——
//
// 需求原文：「mcp http 模式不支持 Header 设置，导致链接无法成功」。Header 的**唯一**
// 价值是「服务器认不认这次请求」，而注入的 in-process 传输会在客户端与服务器之间
// 把 HTTP 层整个抹掉——用了注入传输，headers 有没有被发出去、发到哪个请求上、
// 是否也被 SSE 的 POST 通道带上，全都测不到（测试会全绿而线上依然 401）。
// 因此这里建真实 *httptest.Server：服务器中间件按「头是否齐备」决定 401/200，
// 用断言把「带上头的请求真的到了服务器」钉死。

// authGatedMCP 建一个要求请求头的真实 MCP streamable-HTTP 服务器。
// 缺头 → 401（构造「链接无法成功」的现场）；齐备 → 正常服务。
// 返回服务器、命中计数（带授权头的请求数）、最近一次看到的头快照。
func authGatedMCP(t *testing.T, want map[string]string) (*httptest.Server, *int32, *sync.Map) {
	t.Helper()
	var authorized int32
	seen := &sync.Map{}
	mux := http.NewServeMux()
	inner := server.NewStreamableHTTPServer(newTestMCPServer())
	mux.Handle("/mcp", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ok := true
		for k, v := range want {
			if r.Header.Get(k) != v {
				ok = false
			}
			seen.Store(k, r.Header.Get(k))
		}
		if !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mcp"`)
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		atomic.AddInt32(&authorized, 1)
		inner.ServeHTTP(w, r)
	}))
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts, &authorized, seen
}

// TestHTTPHeadersReachServer http 传输带 headers：连接成功且服务器确实收到（端到端）。
func TestHTTPHeadersReachServer(t *testing.T) {
	ts, authorized, seen := authGatedMCP(t, map[string]string{
		"Authorization": "Bearer secret-token",
		"X-Tenant":      "acme",
	})
	m := NewManager(map[string]ServerConfig{
		"remote": {
			Type:    "http",
			URL:     ts.URL + "/mcp",
			Headers: map[string]string{"Authorization": "Bearer secret-token", "X-Tenant": "acme"},
		},
	})
	defer m.Close()

	tools := m.Tools(context.Background())
	if len(tools) == 0 {
		t.Fatalf("带 headers 应连上并拿到工具；state=%+v", m.Status())
	}
	if n := atomic.LoadInt32(authorized); n == 0 {
		t.Fatal("服务器没有收到任何带授权头的请求（headers 未生效）")
	}
	for k, want := range map[string]string{"Authorization": "Bearer secret-token", "X-Tenant": "acme"} {
		got, _ := seen.Load(k)
		if got != want {
			t.Errorf("服务器收到 %s = %v, want %q", k, got, want)
		}
	}
}

// TestHTTPWithoutHeadersFails 反证：同样的服务器、去掉 headers → 连接失败（401）。
// 这条保证上一个测试不是「服务器根本不校验」的假绿。
func TestHTTPWithoutHeadersFails(t *testing.T) {
	ts, authorized, _ := authGatedMCP(t, map[string]string{"Authorization": "Bearer secret-token"})
	m := NewManager(map[string]ServerConfig{
		"remote": {Type: "http", URL: ts.URL + "/mcp"},
	})
	defer m.Close()

	if tools := m.Tools(context.Background()); len(tools) != 0 {
		t.Fatalf("无 headers 不该拿到工具（got %d）", len(tools))
	}
	if n := atomic.LoadInt32(authorized); n != 0 {
		t.Fatalf("无 headers 时服务器不该放行（authorized=%d）", n)
	}
	st := m.Status()
	if len(st) == 0 || st[0].State != "error" {
		t.Fatalf("无 headers 应记为连接失败: %+v", st)
	}
}

// TestStreamableHTTPAliasSameTransport 三种写法（http / streamable_http /
// streamable-http）归一为同一种传输，且都能带 headers 连上真实服务器。
func TestStreamableHTTPAliasSameTransport(t *testing.T) {
	ts, _, _ := authGatedMCP(t, map[string]string{"Authorization": "Bearer t"})
	hdr := map[string]string{"Authorization": "Bearer t"}
	for _, typ := range []string{"http", "streamable_http", "streamable-http", "STREAMABLE_HTTP"} {
		cfg, err := ServerConfig{Type: typ, URL: ts.URL + "/mcp", Headers: hdr}.Validate()
		if err != nil {
			t.Fatalf("type %q 应合法: %v", typ, err)
		}
		if cfg.Type != TransportHTTP {
			t.Errorf("type %q 归一为 %q, want http", typ, cfg.Type)
		}
		m := NewManager(map[string]ServerConfig{"remote": cfg})
		if tools := m.Tools(context.Background()); len(tools) == 0 {
			t.Errorf("type %q 应连上（归一后同一传输）", typ)
		}
		m.Close()
	}
}

// TestSSEHeadersReachServer sse 传输同样支持 headers（老式 SSE：GET 建流 + POST 送消息，
// 两条通道都必须带头——只带 GET 的实现会在发消息时被 401）。
func TestSSEHeadersReachServer(t *testing.T) {
	var authorized int32
	seen := &sync.Map{}
	inner := server.NewTestServer(newTestMCPServer(), server.WithUseFullURLForMessageEndpoint(false))
	defer inner.Close()
	// 注意：NewTestServer 的内部 handler 挂在 /sse 与 /message（不是 /）。
	// 直接替换 inner.Config.Handler 会给它换一个不会分发的 mux → 404（实测踩到）。
	mux := http.NewServeMux()
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer sse-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		atomic.AddInt32(&authorized, 1)
		seen.Store(r.Method, r.Header.Get("Authorization"))
		inner.Config.Handler.ServeHTTP(w, r)
	}))
	ts := httptest.NewServer(mux)
	defer ts.Close()

	m := NewManager(map[string]ServerConfig{
		"legacy": {
			Type: "sse",
			// 老式 SSE 的端点路径是 /sse（消息通道 /message 由服务器在事件里下发）。
			URL:     ts.URL + "/sse",
			Headers: map[string]string{"Authorization": "Bearer sse-token"},
		},
	})
	defer m.Close()

	if tools := m.Tools(context.Background()); len(tools) == 0 {
		t.Fatalf("sse 带 headers 应连上: %+v", m.Status())
	}
	if n := atomic.LoadInt32(&authorized); n < 2 {
		t.Errorf("SSE 的 GET 建流与 POST 送消息都应带头（命中 %d 次）", n)
	}
	if v, _ := seen.Load("POST"); v == nil {
		t.Error("POST 通道未收到请求（headers 只加在 GET 上会在这里暴露）")
	}
}

// TestStdioCommandNotFoundExplainsFix 命令不存在时给出可操作报错（不是 exec 原文）：
// 用户看到「找不到命令 uvx + 处置办法」，而不是 "executable file not found in $PATH"。
func TestStdioCommandNotFoundExplainsFix(t *testing.T) {
	m := NewManager(map[string]ServerConfig{
		"py": {Type: "stdio", Command: "definitely-not-installed-uvx-xyz", Args: []string{"mcp"}},
	})
	defer m.Close()
	_ = m.Tools(context.Background())

	st := m.Status()
	if len(st) == 0 {
		t.Fatal("应有一条状态")
	}
	msg := st[0].Error
	for _, want := range []string{"不可用", "definitely-not-installed-uvx-xyz", "绝对路径"} {
		if !strings.Contains(msg, want) {
			t.Errorf("报错缺少 %q: %s", want, msg)
		}
	}
}

// TestStdioAbsolutePathMissingExplainsFix 绝对路径写错（面板里填路径的常见错法）
// 同样给出定位信息，且提示命中「文件不存在」分支。
func TestStdioAbsolutePathMissingExplainsFix(t *testing.T) {
	m := NewManager(map[string]ServerConfig{
		"py": {Type: "stdio", Command: "/nonexistent/bin/python3"},
	})
	defer m.Close()
	_ = m.Tools(context.Background())
	st := m.Status()
	if len(st) == 0 || !strings.Contains(st[0].Error, "文件不存在") {
		t.Fatalf("应提示文件不存在: %+v", st)
	}
}

// TestStdioWorkingCommandNoFalseAlarm 正常命令（本机一定有 /bin/echo 语义：
// 用测试自身的可执行路径）不因新增的预检误报——预检只在 exec.Error 时补文案。
func TestStdioWorkingCommandNoFalseAlarm(t *testing.T) {
	m := NewManager(map[string]ServerConfig{
		// 真跑一个会立刻退出、不实现 MCP 协议的命令：报错应是协议/启动层，
		// 不含「找不到命令」这句（预检没被误触发）。
		"ok": {Type: "stdio", Command: "/bin/cat"},
	})
	defer m.Close()
	_ = m.Tools(context.Background())
	st := m.Status()
	if len(st) == 0 {
		t.Fatal("应有一条状态")
	}
	if strings.Contains(st[0].Error, "找不到命令") {
		t.Fatalf("存在的命令被误报为「找不到」: %s", st[0].Error)
	}
}

// TestInjectedTransportIgnoresHeaders 注入传输（插件/测试路径）不受 headers 影响：
// 保证新增字段不改变既有注入语义（plugin/browser 的私有 MCP 用注入传输）。
func TestInjectedTransportIgnoresHeaders(t *testing.T) {
	m := NewManagerWithTransport(map[string]ServerConfig{
		"b": {Type: "http", URL: "http://x/mcp", Headers: map[string]string{"Authorization": "Bearer x"}},
	}, func(ServerConfig) (transport.Interface, error) {
		return transport.NewInProcessTransport(newTestMCPServer()), nil
	})
	defer m.Close()
	if tools := m.Tools(context.Background()); len(tools) == 0 {
		t.Fatal("注入传输仍应连上")
	}
}

// TestURLWithoutTypeExplainsTheFix 有 url 却没写 type：报错必须指向 type（而不是
// 把人引去填 command）。Claude Code 有同款专用文案，说明这是真实高频误配。
func TestURLWithoutTypeExplainsTheFix(t *testing.T) {
	_, err := ServerConfig{URL: "http://x/mcp", Headers: map[string]string{"Authorization": "Bearer t"}}.Validate()
	if err == nil {
		t.Fatal("缺 type 的 url 配置应报错（不能静默当 stdio）")
	}
	for _, want := range []string{"没写 type", "http"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("报错缺少 %q: %v", want, err)
		}
	}
}
