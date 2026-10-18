package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/subagent"
)

// TestSessionEngineMCPHeaders 端到端集成：settings.json 的 mcpServers（**http 传输 +
// 请求头**）→ 工作区 manager 连接 → newSessionEngine 注册 mcp__<name>__whoami 并可调用。
//
// 为什么必须走这条真实链路（而不是只测 mcp 包）：用户看到的路径是
// 「settings.json 里写 headers → App 里工具出现且能用」。中间的每一段都能把头弄丢
// （分层加载、Manager 差分、引擎注册），单测各自绿也不代表这条路通。
// 服务器是真实的 httptest（按头是否齐备决定 401/200），因此「头真的发出去了」是可证的。
func TestSessionEngineMCPHeaders(t *testing.T) {
	srv := server.NewMCPServer("authed", "1.0.0", server.WithToolCapabilities(true))
	srv.AddTool(
		mcp.NewTool("whoami", mcp.WithDescription("需要鉴权的工具")),
		func(_ context.Context, _ mcp.CallToolRequest) (*mcp.CallToolResult, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{mcp.NewTextContent("authenticated")}}, nil
		},
	)
	inner := server.NewStreamableHTTPServer(srv)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer e2e-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer ts.Close()

	// 先建 manager（它会重设 HOME 到隔离目录），再写 settings.json——顺序反了配置读不到
	m, _ := testManager(t)
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".go-code"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"authed": map[string]any{
				"type":    "streamable_http", // 别名写法：归一为 http
				"url":     ts.URL,
				"headers": map[string]any{"Authorization": "Bearer e2e-secret"},
			},
		},
	})
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".go-code", "settings.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	eng, _ := newSessionEngine(ws, "", nil, m.provCfg, "deepseek", m.provCfg.defaultModel(),
		string(provider.ReasoningEffortLevelLow), func() context.Context { return context.Background() },
		subagent.NewRegistry(), events.NewQuestionWaiter(0), buildSkillsRegistry(ws),
		buildSubagentRegistry(t.TempDir()), m.runtime(ws).mcp, m.runtime(ws).plugins, profiles["code"], nil, nil, nil, nil, nil)

	tool, err := eng.GetTool(context.Background(), "mcp__authed__whoami")
	if err != nil {
		t.Fatalf("带请求头的 http 服务器未注册工具（headers 在链路上丢了？）: %v", err)
	}
	res, err := tool.Call(context.Background(), "mcp__authed__whoami", `{}`)
	if err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if res != "authenticated" {
		t.Fatalf("结果 = %q, want authenticated", res)
	}
}

// TestSessionEngineMCPHeadersMissingFails 反证：同样的服务器与配置，去掉 headers →
// 工具注册不上（401）。保证上一条不是「服务器根本不校验」的假绿。
func TestSessionEngineMCPHeadersMissingFails(t *testing.T) {
	srv := server.NewMCPServer("authed", "1.0.0", server.WithToolCapabilities(true))
	inner := server.NewStreamableHTTPServer(srv)
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer e2e-secret" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		inner.ServeHTTP(w, r)
	}))
	defer ts.Close()

	m, _ := testManager(t)
	ws := t.TempDir()
	if err := os.MkdirAll(filepath.Join(os.Getenv("HOME"), ".go-code"), 0o755); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(map[string]any{
		"mcpServers": map[string]any{
			"authed": map[string]any{"type": "http", "url": ts.URL},
		},
	})
	if err := os.WriteFile(filepath.Join(os.Getenv("HOME"), ".go-code", "settings.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}

	eng, _ := newSessionEngine(ws, "", nil, m.provCfg, "deepseek", m.provCfg.defaultModel(),
		string(provider.ReasoningEffortLevelLow), func() context.Context { return context.Background() },
		subagent.NewRegistry(), events.NewQuestionWaiter(0), buildSkillsRegistry(ws),
		buildSubagentRegistry(t.TempDir()), m.runtime(ws).mcp, m.runtime(ws).plugins, profiles["code"], nil, nil, nil, nil, nil)

	if _, err := eng.GetTool(context.Background(), "mcp__authed__whoami"); err == nil {
		t.Fatal("不带请求头不该注册成功")
	}
}
