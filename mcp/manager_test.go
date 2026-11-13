package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mark3labs/mcp-go/client/transport"
	gomcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/tools"
)

// newTestMCPServer 一个内存测试 MCP 服务器（三工具）：
//   - echo   只读，参数 text → 回显
//   - add    只读，参数 a/b → 结构化结果 {sum}
//   - boom   非只读，恒返回 IsError（错误路径）
func newTestMCPServer() *server.MCPServer {
	srv := server.NewMCPServer("test-server", "1.0.0",
		server.WithToolCapabilities(true),
		// instructions：服务器自述的使用策略（正是 graft 那 953 字符的形态）。
		// 断言它经 Manager.Instructions 暴露给宿主（宿主据此注入系统提示词）。
		server.WithInstructions("USE-echo-for-greetings; one call is usually enough."))
	srv.AddTool(
		gomcp.NewTool("echo",
			gomcp.WithDescription("回显文本"),
			gomcp.WithReadOnlyHintAnnotation(true),
			gomcp.WithString("text", gomcp.Required(), gomcp.Description("要回显的文本"))),
		func(_ context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
			args := req.GetArguments()
			text, _ := args["text"].(string)
			return &gomcp.CallToolResult{Content: []gomcp.Content{gomcp.NewTextContent("echo: " + text)}}, nil
		},
	)
	srv.AddTool(
		gomcp.NewTool("add",
			gomcp.WithDescription("求和"),
			gomcp.WithReadOnlyHintAnnotation(true),
			gomcp.WithNumber("a", gomcp.Required()),
			gomcp.WithNumber("b", gomcp.Required())),
		func(_ context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
			args := req.GetArguments()
			a, _ := args["a"].(float64)
			b, _ := args["b"].(float64)
			return &gomcp.CallToolResult{
				Content:           []gomcp.Content{gomcp.NewTextContent("sum ok")},
				StructuredContent: map[string]any{"sum": a + b},
			}, nil
		},
	)
	srv.AddTool(
		gomcp.NewTool("boom",
			gomcp.WithDescription("返回错误"),
			gomcp.WithReadOnlyHintAnnotation(false)),
		func(_ context.Context, _ gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
			return &gomcp.CallToolResult{IsError: true, Content: []gomcp.Content{gomcp.NewTextContent("boom failed")}}, nil
		},
	)
	return srv
}

// newTestManager 建 Manager + 注入 in-process 传输（连到测试服务器，不 spawn 子进程）。
// 注意：transportFor 必须在 SetConfig（建 Server）之前注入——Server 构建时捕获它。
func newTestManager(cfg map[string]ServerConfig) *Manager {
	m := &Manager{servers: map[string]*Server{}}
	testSrv := newTestMCPServer()
	m.transportFor = func(_ ServerConfig) (transport.Interface, error) {
		return transport.NewInProcessTransport(testSrv), nil
	}
	m.SetConfig(cfg)
	return m
}

func TestManagerToolsAdapter(t *testing.T) {
	m := newTestManager(map[string]ServerConfig{
		"srv": {Type: "stdio", Command: "npx", Args: []string{"-y", "@x/y"}},
	})
	got := m.Tools(context.Background())
	if len(got) != 3 {
		t.Fatalf("Tools() 数量 = %d, want 3（%v）", len(got), names(got))
	}
	byName := map[string]tools.Tool{}
	for _, tl := range got {
		byName[tl.Name()] = tl
	}
	// 命名 mcp__<server>__<tool>
	if byName["mcp__srv__echo"] == nil {
		t.Fatalf("缺少 mcp__srv__echo，实际 %v", names(got))
	}
	// 只读 hint → CanParallel/ReadOnly
	echo := byName["mcp__srv__echo"]
	if !echo.CanParallel() || !echo.(tools.ReadOnlyTool).ReadOnly() {
		t.Errorf("echo 应只读可并行")
	}
	boom := byName["mcp__srv__boom"]
	if boom.CanParallel() || boom.(tools.ReadOnlyTool).ReadOnly() {
		t.Errorf("boom 应非只读串行")
	}
	if !boom.(tools.ApprovalRequired).RequiresApproval(context.Background(), core.ToolCall{}) {
		t.Errorf("boom 非只读应需审批")
	}
	// schema 映射（type/properties/required 保持）
	params, ok := echo.Parameters().(map[string]any)
	if !ok {
		t.Fatalf("Parameters 非 map: %T", echo.Parameters())
	}
	if params["type"] != "object" {
		t.Errorf("schema type = %v, want object", params["type"])
	}
	props, _ := params["properties"].(map[string]any)
	if props["text"] == nil {
		t.Errorf("schema 缺 text 属性: %v", params)
	}
	req, _ := params["required"].([]any)
	if len(req) != 1 || req[0] != "text" {
		t.Errorf("required = %v, want [text]", req)
	}
}

func TestAdapterCallThroughEngine(t *testing.T) {
	m := newTestManager(map[string]ServerConfig{"srv": {Type: "http", URL: "http://127.0.0.1:9"}})
	eng := tools.NewToolEngine()
	for _, tl := range m.Tools(context.Background()) {
		eng.RegisterTool(context.Background(), tl)
	}

	ctx := context.Background()
	// 文本工具
	res := seq(eng, ctx, core.ToolCall{Id: "1", Name: "mcp__srv__echo", Arguments: `{"text":"你好"}`})
	if res.Result != "echo: 你好" {
		t.Errorf("echo 结果 = %q, want %q", res.Result, "echo: 你好")
	}
	if res.IsError {
		t.Errorf("echo 不应报错")
	}
	// 结构化结果（文本 + structuredContent）
	res = seq(eng, ctx, core.ToolCall{Id: "2", Name: "mcp__srv__add", Arguments: `{"a":1.5,"b":2}`})
	if !contains(res.Result, `"sum":3.5`) {
		t.Errorf("add 结构化结果 = %q, 应含 sum=3.5", res.Result)
	}
	// 服务器 IsError → 工具错误（IsError 标记回传模型）
	res = seq(eng, ctx, core.ToolCall{Id: "3", Name: "mcp__srv__boom", Arguments: `{}`})
	if !res.IsError {
		t.Errorf("boom 应 IsError")
	}
	if !contains(res.Result, "boom failed") {
		t.Errorf("boom 错误内容 = %q", res.Result)
	}
	// 非法参数 JSON → 校验错误
	res = seq(eng, ctx, core.ToolCall{Id: "4", Name: "mcp__srv__echo", Arguments: `not-json`})
	if !res.IsError {
		t.Errorf("非法参数应 IsError")
	}
}

func TestManagerStatusAndDisabled(t *testing.T) {
	disabled := false
	m := newTestManager(map[string]ServerConfig{
		"srv":    {Type: "stdio", Command: "npx"},
		"hidden": {Type: "stdio", Command: "npx", Enabled: &disabled},
	})
	if got := m.Tools(context.Background()); len(got) != 3 {
		t.Fatalf("禁用服务器不应暴露工具，Tools()=%d", len(got))
	}
	st := m.Status()
	if len(st) != 2 {
		t.Fatalf("Status 数量 = %d, want 2", len(st))
	}
	for _, s := range st {
		if s.Name == "hidden" {
			if s.Enabled || s.State != "idle" {
				t.Errorf("禁用服务器应 idle 且 enabled=false: %+v", s)
			}
		}
		if s.Name == "srv" {
			if s.State != "connected" || s.ToolCount != 3 {
				t.Errorf("srv 应 connected 3 工具: %+v", s)
			}
		}
	}
}

func TestManagerConnectError(t *testing.T) {
	m := &Manager{servers: map[string]*Server{}}
	testSrv := newTestMCPServer()
	m.transportFor = func(cfg ServerConfig) (transport.Interface, error) {
		if cfg.URL == "http://127.0.0.1:9" {
			return nil, fmt.Errorf("dial refused") // dead 服务器连接失败
		}
		return transport.NewInProcessTransport(testSrv), nil
	}
	m.SetConfig(map[string]ServerConfig{
		"srv":  {Type: "stdio", Command: "npx"},
		"bad":  {Type: "stdio", Command: "npx"},
		"dead": {Type: "http", URL: "http://127.0.0.1:9"},
	})
	// 连接失败不影响其他服务器暴露工具
	if got := m.Tools(context.Background()); len(got) != 6 {
		t.Fatalf("Tools() = %d, want 6（两服务器×3）", len(got))
	}
	for _, s := range m.Status() {
		if s.Name == "dead" {
			if s.State != "error" || s.Error == "" {
				t.Errorf("dead 应 error: %+v", s)
			}
		}
	}
}

func TestManagerSetConfig(t *testing.T) {
	m := newTestManager(map[string]ServerConfig{"a": {Type: "stdio", Command: "npx", Args: []string{"1"}}})
	// 首次连接 a
	_ = m.Tools(context.Background())
	if len(m.Status()) != 1 || m.Status()[0].State != "connected" {
		t.Fatalf("a 应 connected: %+v", m.Status())
	}
	// 变更 a（重建连接）+ 新增 b + 保留
	m.SetConfig(map[string]ServerConfig{
		"a": {Type: "stdio", Command: "npx", Args: []string{"2"}},
		"b": {Type: "stdio", Command: "npx"},
	})
	st := m.Status()
	if len(st) != 2 {
		t.Fatalf("Status = %d, want 2", len(st))
	}
	if st[0].Name != "a" {
		t.Errorf("顺序按名: first=%s", st[0].Name)
	}
	// 移除 b → 关闭并消失
	m.SetConfig(map[string]ServerConfig{"a": {Type: "stdio", Command: "npx", Args: []string{"2"}}})
	if len(m.Status()) != 1 || m.Status()[0].Name != "a" {
		t.Fatalf("移除 b 后 Status: %+v", m.Status())
	}
}

func TestLoadConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "settings.json")
	cfg := map[string]any{
		"mcpServers": map[string]any{
			"good": map[string]any{
				"type": "stdio", "command": "npx", "args": []any{"-y", "@a/b"},
				"env": map[string]any{"TOKEN": "abc"},
			},
			"remote": map[string]any{"type": "http", "url": "http://x/mcp"},
			"bad":    map[string]any{"type": "stdio"}, // 缺 command → 跳过
		},
	}
	b, _ := json.Marshal(cfg)
	_ = os.WriteFile(path, b, 0o644)

	got := LoadConfig(path)
	if len(got) != 2 {
		t.Fatalf("LoadConfig = %d, want 2（坏配置跳过）", len(got))
	}
	g := got["good"]
	if g.Type != "stdio" || g.Command != "npx" || g.Env["TOKEN"] != "abc" {
		t.Errorf("good 解析错: %+v", g)
	}
	if got["remote"].Type != "http" || got["remote"].URL != "http://x/mcp" {
		t.Errorf("remote 解析错: %+v", got["remote"])
	}
	// 缺省 type → stdio
	_ = os.WriteFile(path, []byte(`{"mcpServers":{"x":{"command":"c"}}}`), 0o644)
	if got = LoadConfig(path); got["x"].Type != "stdio" {
		t.Errorf("缺省 type 应为 stdio: %+v", got["x"])
	}
	// 缺文件 → 空
	if LoadConfig(filepath.Join(dir, "nope.json")) != nil {
		t.Errorf("缺文件应空")
	}
}

func TestSerializeResult(t *testing.T) {
	// 文本
	if s := serializeResult(&gomcp.CallToolResult{Content: []gomcp.Content{gomcp.NewTextContent("hi")}}); s != "hi" {
		t.Errorf("text = %q", s)
	}
	// 图片：文本侧只声明存在与体积（**不落 base64**——图像本体经 imagesOfResult
	// 作为内容块进上下文，见 TestImagesOfResult）
	if s := serializeResult(&gomcp.CallToolResult{Content: []gomcp.Content{gomcp.NewImageContent("QUJD", "image/png")}}); s != "[image image/png, 4 bytes — attached for direct viewing]" {
		t.Errorf("image = %q", s)
	}
	// 空 → 占位
	if s := serializeResult(&gomcp.CallToolResult{}); s != "[empty tool result]" {
		t.Errorf("empty = %q", s)
	}
	// structuredContent
	res := &gomcp.CallToolResult{StructuredContent: map[string]any{"n": 1}}
	if !contains(serializeResult(res), `"n":1`) {
		t.Errorf("structured = %q", serializeResult(res))
	}
}

// —— helpers ——

func seq(eng *tools.Engine, ctx context.Context, call core.ToolCall) core.ToolResult {
	res, err := eng.Sequence(ctx, []core.ToolCall{call}, nil)
	if err != nil {
		return core.ToolResult{Id: call.Id, Result: "seq error: " + err.Error(), IsError: true}
	}
	return res[0]
}

func names(ts []tools.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, t := range ts {
		out = append(out, t.Name())
	}
	return out
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}

// TestImagesOfResult MCP 图片内容 → data URL 内容块（模型可见）。此前被降级为
// 占位文本（"[image mime: N bytes base64]"），截图类 MCP 工具对模型不可见。
func TestImagesOfResult(t *testing.T) {
	res := &gomcp.CallToolResult{Content: []gomcp.Content{
		gomcp.NewTextContent("here is the shot"),
		gomcp.NewImageContent("QUJD", "image/png"),
	}}
	imgs := imagesOfResult(res)
	if len(imgs) != 1 {
		t.Fatalf("want 1 image, got %d", len(imgs))
	}
	if imgs[0].Type != core.ContentTypeImage {
		t.Fatalf("type = %q", imgs[0].Type)
	}
	if imgs[0].Content != "data:image/png;base64,QUJD" {
		t.Fatalf("data URL = %q", imgs[0].Content)
	}
	if imgs[0].MimeType != "image/png" {
		t.Fatalf("mime = %q", imgs[0].MimeType)
	}
}

// TestImagesOfResultDefaultsAndSkips 空 mime → image/png 兜底；空 data 跳过；
// 非图片内容不产出图片块。
func TestImagesOfResultDefaultsAndSkips(t *testing.T) {
	res := &gomcp.CallToolResult{Content: []gomcp.Content{
		gomcp.NewTextContent("text only"),
		gomcp.NewImageContent("", "image/png"), // 空 data → 跳过
		gomcp.NewImageContent("QUJD", ""),      // 空 mime → 兜底
	}}
	imgs := imagesOfResult(res)
	if len(imgs) != 1 {
		t.Fatalf("want exactly 1 image (empty data skipped), got %d", len(imgs))
	}
	if imgs[0].MimeType != "image/png" {
		t.Fatalf("mime fallback = %q", imgs[0].MimeType)
	}
}

// TestImagesOfResultNilAndNoImage 边界：nil 结果 / 无图片 → nil（不产出空切片）。
func TestImagesOfResultNilAndNoImage(t *testing.T) {
	if imgs := imagesOfResult(nil); imgs != nil {
		t.Fatalf("nil result → nil, got %+v", imgs)
	}
	if imgs := imagesOfResult(&gomcp.CallToolResult{Content: []gomcp.Content{gomcp.NewTextContent("x")}}); imgs != nil {
		t.Fatalf("text-only → nil, got %+v", imgs)
	}
}

// TestMCPToolCallAddsImagesToSink MCP 工具把结果图片写进本次调用的收集槽
// （sink 按调用独立 → 天然无跨调用残留）。
func TestMCPToolCallAddsImagesToSink(t *testing.T) {
	ctx := tools.WithImageSink(context.Background(), &tools.ImageSink{})
	sink := tools.ImageSinkFrom(ctx)
	sink.Add(imagesOfResult(&gomcp.CallToolResult{Content: []gomcp.Content{
		gomcp.NewImageContent("QUJD", "image/png"),
	}})...)
	got := sink.Images()
	if len(got) != 1 || got[0].Content != "data:image/png;base64,QUJD" {
		t.Fatalf("got %+v", got)
	}
	// 另一次调用（新槽）不受影响
	other := &tools.ImageSink{}
	if len(other.Images()) != 0 {
		t.Fatalf("other sink must be empty: %+v", other.Images())
	}
}
