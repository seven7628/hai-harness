package mcp

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/mark3labs/mcp-go/client/transport"
	gomcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/seven7628/hai-harness/tools"
)

// newJSONTextServer 只回**文本 JSON** 的服务器（ai-test / api-deployment 这类真实 server
// 的形态：没有 StructuredContent，正文本身就是 JSON）。
func newJSONTextServer() *server.MCPServer {
	srv := server.NewMCPServer("json-text", "1.0.0", server.WithToolCapabilities(true))
	srv.AddTool(
		gomcp.NewTool("query",
			gomcp.WithDescription("返回 JSON 文本"),
			gomcp.WithReadOnlyHintAnnotation(true)),
		func(_ context.Context, _ gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
			body, _ := json.Marshal(map[string]any{
				"count":   2,
				"scanned": 154,
				"items":   []any{map[string]any{"id": 1}, map[string]any{"id": 2}},
			})
			return &gomcp.CallToolResult{Content: []gomcp.Content{gomcp.NewTextContent(string(body))}}, nil
		},
	)
	srv.AddTool(
		gomcp.NewTool("plain", gomcp.WithDescription("返回非 JSON 文本")),
		func(_ context.Context, _ gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
			return &gomcp.CallToolResult{Content: []gomcp.Content{gomcp.NewTextContent("plain text")}}, nil
		},
	)
	srv.AddTool(
		gomcp.NewTool("broken", gomcp.WithDescription("IsError 且正文是 JSON")),
		func(_ context.Context, _ gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
			return &gomcp.CallToolResult{
				IsError: true,
				Content: []gomcp.Content{gomcp.NewTextContent(`{"error":"nope"}`)},
			}, nil
		},
	)
	return srv
}

func newJSONTextManager() *Manager {
	m := &Manager{servers: map[string]*Server{}}
	srv := newJSONTextServer()
	m.transportFor = func(_ ServerConfig) (transport.Interface, error) {
		return transport.NewInProcessTransport(srv), nil
	}
	m.SetConfig(map[string]ServerConfig{"srv": {Type: "http", URL: "http://127.0.0.1:9"}})
	return m
}

// callAsScript 以**编排脚本**的身份调用一次 MCP 工具（= codemode 的 tools.x(...) 路径），
// 返回文本结果与写进结构化槽的内容。
//
// 用工具适配器直调而不是 engine.Sequence：结构化槽只在 Depth>0（脚本路径）写，而
// Sequence 是模型路径。脚本身份的 ctx 标记由引擎注入，测试里直接构造。
func callAsScript(t *testing.T, m *Manager, name, args string) (string, []byte, error) {
	t.Helper()
	ctx := context.Background()
	var adapter tools.Tool
	for _, tl := range m.Tools(ctx) {
		if tl.Name() == name {
			adapter = tl
		}
	}
	if adapter == nil {
		t.Fatalf("工具表里没有 %s", name)
	}
	sink := &tools.StructuredSink{}
	scriptCtx := tools.WithStructuredSink(tools.WithScriptCall(ctx), sink)
	out, err := adapter.Call(scriptCtx, name, args)
	return out, sink.Content(), err
}

// TestMCPToolTextJSONBecomesStructured 文本 JSON 必须变成脚本可编程的结构化结果。
//
// 回归的是一个**静默归零**：codemode 的表是「仅文本 → resolve 成单个 string」，
// MCP 适配器此前只返回文本 —— 脚本里 `r.count` 恒 undefined、`(r.items||[]).length`
// 恒 0，且不报错。实测代价：同一条 ai-test 调用打印原文拿到 25 条/scanned=154，改成
// 取字段就变成 got=0 scanned=undefined，模型据此判定「MCP 会话死了」并要求人工重连。
func TestMCPToolTextJSONBecomesStructured(t *testing.T) {
	m := newJSONTextManager()
	defer m.Close()
	out, structured, err := callAsScript(t, m, "mcp__srv__query", "{}")
	if err != nil {
		t.Fatalf("query 不该报错: %v", err)
	}
	// 模型路径不变：文本结果仍是原文。
	var modelPath map[string]any
	if err := json.Unmarshal([]byte(out), &modelPath); err != nil {
		t.Fatalf("结果文本应仍是 JSON 原文: %v (%q)", err, out)
	}
	// 脚本路径：结构化结果就是那段 JSON（字段可点）。
	var got map[string]any
	if err := json.Unmarshal(structured, &got); err != nil {
		t.Fatalf("结构化结果不是合法 JSON: %v (raw=%q)", err, structured)
	}
	if got["scanned"] != float64(154) || got["count"] != float64(2) {
		t.Fatalf("结构化字段不对: %v", got)
	}
	items, _ := got["items"].([]any)
	if len(items) != 2 {
		t.Fatalf("items 长度 = %d, want 2", len(items))
	}
}

// TestMCPToolStructuredAbsentCases 非 JSON 文本 / IsError → 不给结构化（退回文本路径）。
//
// IsError 刻意不写槽：codemode 里「IsError + 有结构化」是 resolve（§15.4），写进去会让
// 错误变成一个能点字段的对象，脚本的 try/catch 收不到 —— 失败被静默吃掉。
func TestMCPToolStructuredAbsentCases(t *testing.T) {
	m := newJSONTextManager()
	defer m.Close()
	if _, structured, err := callAsScript(t, m, "mcp__srv__plain", "{}"); err != nil || structured != nil {
		t.Errorf("非 JSON 文本不该有结构化结果: err=%v raw=%q", err, structured)
	}
	out, structured, err := callAsScript(t, m, "mcp__srv__broken", "{}")
	if err == nil {
		t.Fatalf("broken 应报错（IsError → 工具错误）")
	}
	if structured != nil {
		t.Errorf("IsError 不该有结构化结果（否则脚本侧 resolve 而非 reject）: %q", structured)
	}
	if !contains(out, "nope") {
		t.Errorf("错误文本应保留: %q", out)
	}
}

// TestMCPToolStructuredContentPreferred 服务器给了 StructuredContent → 用它（协议原生字段优先）。
func TestMCPToolStructuredContentPreferred(t *testing.T) {
	m := newTestManager(map[string]ServerConfig{"srv": {Type: "http", URL: "http://127.0.0.1:9"}})
	defer m.Close()
	_, structured, err := callAsScript(t, m, "mcp__srv__add", `{"a":1.5,"b":2}`)
	if err != nil {
		t.Fatalf("add 不该报错: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(structured, &got); err != nil {
		t.Fatalf("结构化结果不是合法 JSON: %v (raw=%q)", err, structured)
	}
	if got["sum"] != 3.5 {
		t.Fatalf("sum = %v, want 3.5", got["sum"])
	}
}

// TestMCPToolStructuredOnlyOnScriptPath 模型路径不写结构化（与 bash 同款：只有 codemode
// 消费结构化结果，模型路径重复落一份没有读者）。
func TestMCPToolStructuredOnlyOnScriptPath(t *testing.T) {
	m := newJSONTextManager()
	defer m.Close()
	ctx := context.Background()
	var adapter tools.Tool
	for _, tl := range m.Tools(ctx) {
		if tl.Name() == "mcp__srv__query" {
			adapter = tl
		}
	}
	sink := &tools.StructuredSink{}
	if _, err := adapter.Call(tools.WithStructuredSink(ctx, sink), "mcp__srv__query", "{}"); err != nil {
		t.Fatalf("调用失败: %v", err)
	}
	if sink.Content() != nil {
		t.Fatalf("模型路径不该写结构化结果: %q", sink.Content())
	}
}

// TestMCPToolDeclaresOutputSchema 适配器必须实现 tools.OutputSchemaProvider（否则引擎不读槽位）。
func TestMCPToolDeclaresOutputSchema(t *testing.T) {
	m := newJSONTextManager()
	defer m.Close()
	ctx := context.Background()
	for _, tl := range m.Tools(ctx) {
		osp, ok := tl.(tools.OutputSchemaProvider)
		if !ok {
			t.Fatalf("%s 未实现 OutputSchemaProvider（引擎不会读结构化槽位）", tl.Name())
		}
		if osp.OutputSchema() != nil {
			t.Errorf("%s OutputSchema 应为 nil（MCP 不提供输出 schema，硬编就是撒谎）", tl.Name())
		}
	}
}
