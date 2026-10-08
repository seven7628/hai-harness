package mcp

// exposure_test.go：MCP 工具的暴露档位（ExposureProvider / NamespaceProvider）钉子。
//
// 本次要钉住的**安全不变量**只有一条，但它有多个面向：MCP 工具默认（以及配置成 codemode
// 档时）**既不进模型工具表、也不进 codemode 描述**，只可从脚本调用。若默认档写成引擎的
// codemode，几十个 server 的工具描述会灌进 codemode 描述、并随 MCP 连接/断线抖动，
// provider 前缀缓存全失效 —— 即重新引入 pi #10212（设计文档 §18、IMPLEMENTATION-SPEC §4）。
//
// 另一个不变量是**字节稳定**：加/删一个 MCP server 不得改变其它工具的 schema 字节。
//
// 本文件**自包含**（不引用其它 _test.go 里的夹具/辅助函数）：本 worktree 的既有测试文件
// 都在 .git/info/exclude 里（只存在于工作区，不进本分支的提交），靠它们会让本文件在干净
// 检出上编译不过 —— 所以下面的 in-process 测试服务器与 Manager 夹具都在本文件内定义。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/mark3labs/mcp-go/client/transport"
	gomcp "github.com/mark3labs/mcp-go/mcp"
	"github.com/mark3labs/mcp-go/server"
	"github.com/seven7628/hai-harness/tools"
)

// testInstructions fixture 服务器的自述策略（断言 Namespace() 透传的就是它）。
const testInstructions = "USE-echo-for-greetings; one call is usually enough."

// ---- config 层：字段 / 默认值 / 校验 ----

// TestServerConfigExposureDefaultsToDeferred 空值 = deferred（出厂默认 = 安全档，
// 不需要用户显式配置；零值即安全是这一条的意义）。
func TestServerConfigExposureDefaultsToDeferred(t *testing.T) {
	cfg, err := ServerConfig{Command: "npx"}.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Exposure != "deferred" {
		t.Fatalf("默认 exposure = %q, want deferred", cfg.Exposure)
	}
	if !cfg.IsEnabled() {
		t.Fatal("启用语义不受 exposure 影响（缺省仍为启用）")
	}
	// 显式值归一（空白/大小写）
	got, err := ServerConfig{Command: "npx", Exposure: "  DIRECT "}.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if got.Exposure != "direct" {
		t.Fatalf("归一后 exposure = %q, want direct", got.Exposure)
	}
}

// TestServerConfigExposureInvalidValueErrors 非法档位**明确报错**（不静默降级：
// 拼错的 hidden 若被当成默认档，用户以为撤下的工具仍可被脚本调用）。
func TestServerConfigExposureInvalidValueErrors(t *testing.T) {
	_, err := ServerConfig{Command: "npx", Exposure: "defrred"}.Validate()
	if err == nil {
		t.Fatal("非法 exposure 必须报错")
	}
	msg := err.Error()
	for _, want := range []string{"defrred", "direct", "codemode", "deferred", "hidden"} {
		if !strings.Contains(msg, want) {
			t.Errorf("错误信息应包含 %q（说清合法值）: %q", want, msg)
		}
	}
	// 坏配置的 blast radius（既有语义）：整条 server 被跳过，而不是静默按默认档加载。
	path := filepath.Join(t.TempDir(), "settings.json")
	raw := `{"mcpServers":{"bad":{"command":"npx","exposure":"defrred"},
	                          "good":{"command":"npx"}}}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded := LoadConfig(path)
	if _, ok := loaded["bad"]; ok {
		t.Fatal("非法 exposure 的 server 不得被加载")
	}
	if got := loaded["good"].Exposure; got != "deferred" {
		t.Fatalf("合法 server 照常加载且默认 deferred, got %q", got)
	}
}

// ---- 适配器：两层映射（config 档 → 引擎档） ----

// TestMCPToolDefaultsToDeferredExposure 默认档 = 引擎 deferred：
// 不进模型工具表（ToolParams）、不进 codemode 描述（ListedInCodemodeCatalog）、但可被脚本调用。
func TestMCPToolDefaultsToDeferredExposure(t *testing.T) {
	m := fixtureManager(t, map[string]ServerConfig{"srv": {Type: "stdio", Command: "unused"}})
	ctx := context.Background()
	adapters := m.Tools(ctx)
	if len(adapters) != 3 {
		t.Fatalf("适配器数量 = %d, want 3（%v）", len(adapters), adapterNames(adapters))
	}
	for _, tl := range adapters {
		if got := tools.ToolExposureOf(tl); got != tools.ExposureDeferred {
			t.Errorf("%s 的引擎档 = %q, want deferred", tl.Name(), got)
		}
		if tools.DeclaredToModel(tl) {
			t.Errorf("%s 不得进模型工具表（安全不变量）", tl.Name())
		}
		if tools.ListedInCodemodeCatalog(tl) {
			t.Errorf("%s 不得进 codemode 描述（否则重新引入 pi #10212）", tl.Name())
		}
		if !tools.CallableFromScript(tl) {
			t.Errorf("%s 必须可被脚本调用（deferred 的全部价值）", tl.Name())
		}
	}
	// 引擎级对照：ToolParams 一个 MCP 工具都不该有。
	eng := tools.NewToolEngine()
	for _, tl := range adapters {
		eng.RegisterTool(ctx, tl)
	}
	params, err := eng.ToolParams(ctx)
	if err != nil {
		t.Fatalf("ToolParams: %v", err)
	}
	if len(params) != 0 {
		t.Fatalf("默认档下 ToolParams = %v, want 空（MCP 工具不得声明给模型）", params)
	}
}

// TestMCPExposureCodemodeTierFoldsToDeferred 显式配置 codemode 档 → **引擎 deferred**。
// config 层保留原档（两层映射发生在适配器侧），但引擎侧必须折成 deferred —— 这是
// pi toToolExposure() 的关键一条（mcp/tools.ts:39-41）：MCP 的 codemode 档同样不写进描述。
func TestMCPExposureCodemodeTierFoldsToDeferred(t *testing.T) {
	cfg, err := ServerConfig{Command: "unused", Exposure: "codemode"}.Validate()
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if cfg.Exposure != "codemode" {
		t.Fatalf("config 层应保留配置档, got %q", cfg.Exposure)
	}
	m := fixtureManager(t, map[string]ServerConfig{"srv": cfg})
	adapters := m.Tools(context.Background())
	if len(adapters) != 3 {
		t.Fatalf("适配器数量 = %d, want 3", len(adapters))
	}
	for _, tl := range adapters {
		if got := tools.ToolExposureOf(tl); got != tools.ExposureDeferred {
			t.Errorf("%s 的引擎档 = %q, want deferred（codemode 档折成 deferred，不得进描述）", tl.Name(), got)
		}
		if tools.ListedInCodemodeCatalog(tl) {
			t.Errorf("%s 不得进 codemode 描述（#10212）", tl.Name())
		}
		if tools.DeclaredToModel(tl) {
			t.Errorf("%s 不得进模型工具表", tl.Name())
		}
	}
}

// TestMCPExposureDirectAndHidden 显式 direct 档进模型工具表；hidden 档彻底不可达
// （不声明、不进描述、脚本内也不可达 —— 撤下工具的唯一手段）。
func TestMCPExposureDirectAndHidden(t *testing.T) {
	ctx := context.Background()
	m := fixtureManager(t, map[string]ServerConfig{
		"shown": {Command: "unused", Exposure: "direct"},
		"gone":  {Command: "unused", Exposure: "hidden"},
	})
	eng := tools.NewToolEngine()
	for _, tl := range m.Tools(ctx) {
		eng.RegisterTool(ctx, tl)
	}
	params, err := eng.ToolParams(ctx)
	if err != nil {
		t.Fatalf("ToolParams: %v", err)
	}
	var declared []string
	for _, s := range params {
		declared = append(declared, s.Name)
	}
	sort.Strings(declared)
	want := "mcp__shown__add,mcp__shown__boom,mcp__shown__echo"
	if strings.Join(declared, ",") != want {
		t.Fatalf("声明集 = %v, want %s（direct 生效、hidden 撤下）", declared, want)
	}
	for _, tl := range m.Tools(ctx) {
		if !strings.HasPrefix(tl.Name(), "mcp__gone__") {
			continue
		}
		if tools.DeclaredToModel(tl) || tools.ListedInCodemodeCatalog(tl) || tools.CallableFromScript(tl) {
			t.Errorf("hidden 档的 %s 必须完全不可达", tl.Name())
		}
	}
}

// ---- NamespaceProvider：server 级分组 + instructions 透传 ----

// TestMCPToolNamespacePassthrough 已连接的 server：Name = server 名、Instructions =
// initialize 缓存的自述策略（mcp.Server.Instructions 的同源数据）；Description 只用
// 配置合成（与连接状态无关 → 描述字节稳定，见下一用例的字节不变量）。
func TestMCPToolNamespacePassthrough(t *testing.T) {
	m := fixtureManager(t, map[string]ServerConfig{"srv": {Command: "unused"}})
	ctx := context.Background()
	adapters := m.Tools(ctx) // 触发惰性连接（宿主构建工具集时发生的事）
	if len(adapters) == 0 {
		t.Fatal("适配器为空")
	}
	ns := tools.NamespaceOf(adapters[0])
	if ns == nil {
		t.Fatal("mcpTool 必须实现 tools.NamespaceProvider")
	}
	if ns.Name != "srv" {
		t.Errorf("namespace = %q, want srv（工具名前缀即分组键）", ns.Name)
	}
	if strings.TrimSpace(ns.Description) == "" {
		t.Error("Description 不得为空（描述里分组标题下的一句话）")
	}
	if ns.Instructions != testInstructions {
		t.Errorf("Instructions = %q, want %q（透传 server instructions）", ns.Instructions, testInstructions)
	}
	if m.Instructions(ctx)["srv"] != ns.Instructions {
		t.Error("Namespace().Instructions 必须与 Manager.Instructions 同源")
	}
}

// TestMCPToolNamespaceDoesNotConnect Namespace() 不得触发惰性连接：它会在描述生成
// 路径（工具轮内）被调用，一次上限 15s 的 initialize 会卡住整个 turn。未连接时给
// 空 instructions（MCP 工具本身就只在连接成功后才存在，正常路径不会丢数据）。
func TestMCPToolNamespaceDoesNotConnect(t *testing.T) {
	var dials atomic.Int32
	m := &Manager{servers: map[string]*Server{}}
	fixture := newFixtureServer(testInstructions)
	m.transportFor = func(ServerConfig) (transport.Interface, error) {
		dials.Add(1)
		return transport.NewInProcessTransport(fixture), nil
	}
	m.SetConfig(map[string]ServerConfig{"srv": {Command: "unused"}})

	s := m.servers["srv"]
	adapter := NewAdapter(s, Tool{server: "srv", name: "echo"})
	ns := tools.NamespaceOf(adapter)
	if ns == nil || ns.Name != "srv" {
		t.Fatalf("未连接时也要给出分组信息（Name/Description 来自配置）: %+v", ns)
	}
	if ns.Instructions != "" {
		t.Errorf("未连接时 Instructions 应为空（连接会阻塞工具轮），got %q", ns.Instructions)
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("Namespace() 触发了惰性连接：dials=%d", got)
	}
	// 对照：真正需要连接时（构建工具集）照样连，且此后 instructions 透传。
	if len(m.Tools(context.Background())) == 0 {
		t.Fatal("Manager.Tools 应连接并产出适配器")
	}
	if got := dials.Load(); got == 0 {
		t.Fatal("Manager.Tools 应触发一次连接（对照）")
	}
	if ns := tools.NamespaceOf(adapter); ns.Instructions != testInstructions {
		t.Fatalf("连接后 Instructions = %q, want %q", ns.Instructions, testInstructions)
	}
}

// ---- 缓存不变量：schema 字节稳定 ----

// TestMCPToolsDoNotChangeToolParamsBytes 加/删 MCP server 不得改变**其它工具**的
// schema 字节：默认档（deferred/codemode/hidden）完全不进 ToolParams；显式 direct 档
// 时新增/移除另一台 server 也不得影响已声明工具的字节（provider 前缀缓存的前提）。
func TestMCPToolsDoNotChangeToolParamsBytes(t *testing.T) {
	baseline := toolParamsBytes(t, nil)
	if _, ok := baseline["baseline"]; !ok {
		t.Fatalf("基线工具应在声明集里: %v", baseline)
	}
	for name, cfg := range map[string]map[string]ServerConfig{
		"默认档":      {"a": {Command: "unused"}},
		"deferred": {"a": {Command: "unused", Exposure: "deferred"}},
		"codemode": {"a": {Command: "unused", Exposure: "codemode"}},
		"hidden":   {"a": {Command: "unused", Exposure: "hidden"}},
	} {
		if got := toolParamsBytes(t, cfg); !reflect.DeepEqual(got, baseline) {
			t.Errorf("%s：MCP server 改变了 ToolParams 字节\n got=%v\nwant=%v", name, got, baseline)
		}
	}

	one := toolParamsBytes(t, map[string]ServerConfig{"a": {Command: "unused", Exposure: "direct"}})
	two := toolParamsBytes(t, map[string]ServerConfig{
		"a": {Command: "unused", Exposure: "direct"},
		"b": {Command: "unused", Exposure: "direct"},
	})
	for name, want := range one {
		if got := two[name]; got != want {
			t.Errorf("加 server b 改变了 %s 的 schema 字节:\n got=%s\nwant=%s", name, got, want)
		}
	}
	if len(two) != len(one)+3 {
		t.Errorf("direct 档 server b 应新增它自己的 3 个声明工具: one=%d two=%d", len(one), len(two))
	}
	// 删回 b：字节回到 one（同一份 set 的确定性渲染）
	if again := toolParamsBytes(t, map[string]ServerConfig{"a": {Command: "unused", Exposure: "direct"}}); !reflect.DeepEqual(again, one) {
		t.Errorf("删掉 server b 后字节应与加之前一致:\n got=%v\nwant=%v", again, one)
	}
}

// ---- 测试夹具 ----

// baselineTool 固定基线工具：证明「加/删 MCP server 不改变其它工具」里的「其它」。
type baselineTool struct{ tools.BaseTool }

func (*baselineTool) Call(context.Context, string, string) (string, error) { return "ok", nil }
func (*baselineTool) ValidParams(context.Context, string, string) error    { return nil }

// toolParamsBytes 用「基线工具 + 给定 MCP 配置的适配器」渲染 ToolParams，
// 返回 工具名 → schema JSON 字节（ToolParams 已按名排序，json.Marshal 对 map 键排序稳定）。
func toolParamsBytes(t *testing.T, cfg map[string]ServerConfig) map[string]string {
	t.Helper()
	ctx := context.Background()
	eng := tools.NewToolEngine()
	eng.RegisterTool(ctx, &baselineTool{BaseTool: tools.BaseTool{
		Name_:        "baseline",
		Description_: "baseline tool",
		Params_:      tools.Obj(map[string]any{"x": tools.Str("x")}, "x"),
	}})
	if cfg != nil {
		for _, tl := range fixtureManager(t, cfg).Tools(ctx) {
			eng.RegisterTool(ctx, tl)
		}
	}
	params, err := eng.ToolParams(ctx)
	if err != nil {
		t.Fatalf("ToolParams: %v", err)
	}
	out := make(map[string]string, len(params))
	for _, s := range params {
		raw, err := json.Marshal(s)
		if err != nil {
			t.Fatalf("marshal schema %s: %v", s.Name, err)
		}
		out[s.Name] = string(raw)
	}
	return out
}

// fixtureManager 每台 server 各连一个**独立**的 in-process 测试服务器（不 spawn 子进程，
// 但每个 server 一份 fixture：同一个 fixture 被两个 client 同时连接虽可行，独立实例更接近
// 真实拓扑）。transportFor 必须在 SetConfig 前注入（Server 构造时捕获它）。
func fixtureManager(t *testing.T, cfg map[string]ServerConfig) *Manager {
	t.Helper()
	m := &Manager{servers: map[string]*Server{}}
	m.transportFor = func(ServerConfig) (transport.Interface, error) {
		return transport.NewInProcessTransport(newFixtureServer(testInstructions)), nil
	}
	m.SetConfig(cfg)
	return m
}

// adapterNames 适配器名清单（断言失败信息用）。
func adapterNames(ts []tools.Tool) []string {
	out := make([]string, 0, len(ts))
	for _, tl := range ts {
		out = append(out, tl.Name())
	}
	return out
}

// newFixtureServer 三工具测试 MCP 服务器（对齐本包既有夹具的形态，本文件自包含）：
//   - echo 只读（readOnlyHint）
//   - add  只读，带 structuredContent
//   - boom 非只读，恒返回 IsError
func newFixtureServer(instr string) *server.MCPServer {
	srv := server.NewMCPServer("fixture", "1.0.0",
		server.WithToolCapabilities(true), server.WithInstructions(instr))
	srv.AddTool(
		gomcp.NewTool("echo",
			gomcp.WithDescription("回显文本"),
			gomcp.WithReadOnlyHintAnnotation(true),
			gomcp.WithString("text", gomcp.Required())),
		func(_ context.Context, req gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
			text, _ := req.GetArguments()["text"].(string)
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
			a, _ := req.GetArguments()["a"].(float64)
			b, _ := req.GetArguments()["b"].(float64)
			return &gomcp.CallToolResult{
				Content:           []gomcp.Content{gomcp.NewTextContent("sum ok")},
				StructuredContent: map[string]any{"sum": a + b},
			}, nil
		},
	)
	srv.AddTool(
		gomcp.NewTool("boom", gomcp.WithDescription("返回错误"), gomcp.WithReadOnlyHintAnnotation(false)),
		func(_ context.Context, _ gomcp.CallToolRequest) (*gomcp.CallToolResult, error) {
			return &gomcp.CallToolResult{IsError: true, Content: []gomcp.Content{gomcp.NewTextContent("boom failed")}}, nil
		},
	)
	return srv
}
