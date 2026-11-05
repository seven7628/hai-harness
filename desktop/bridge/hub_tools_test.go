package main

// P1-01：Hub 工具引擎（全工具面 + Profile 策略 + 沙箱根）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/sandbox"
	"github.com/seven7628/hai-harness/subagent"
	"github.com/seven7628/hai-harness/tools"
)

func hubEngineFor(t *testing.T, m *manager, ws *workspaceRuntime, policy agenthub.ToolPolicy) *tools.Engine {
	t.Helper()
	spec := &hubSessionSpec{Agent: agenthub.AgentProfile{
		ID: "ag_engine", Provider: "deepseek", Model: "deepseek-v4-flash", Tools: policy,
	}}
	eng, _ := m.buildHubEngine(ws, "s-hub-engine", spec, subagent.NewRegistry(),
		events.NewQuestionWaiter(0), func() context.Context { return context.Background() })
	t.Cleanup(func() { _ = eng })
	return eng
}

func toolVisible(t *testing.T, eng *tools.Engine, name string) (exists, visible bool) {
	t.Helper()
	tool, err := eng.GetTool(context.Background(), name)
	if err != nil {
		if strings.Contains(err.Error(), "disabled by policy") {
			return true, false // 已注册但被策略撤下
		}
		return false, false
	}
	return true, tools.ToolExposureOf(tool) != tools.ExposureHidden
}

// 零值策略 = 全开（D18）：常见工具必须都在，且没有一个被 hidden。
func TestHubEngineZeroValueKeepsFullToolset(t *testing.T) {
	m, _ := testManager(t)
	ws := m.hubRuntime("ag_engine")
	eng := hubEngineFor(t, m, ws, agenthub.ToolPolicy{})

	for _, name := range []string{
		"bash", "read_file", "write_file", "edit_file", "glob", "grep",
		"run_python", "agent_spawn", "subagent_explore", "ask_user", "todo_add", "load_skill",
	} {
		exists, visible := toolVisible(t, eng, name)
		if !exists {
			t.Errorf("hub engine missing %q (零值必须全开)", name)
			continue
		}
		if !visible {
			t.Errorf("hub engine hid %q under zero-value policy", name)
		}
	}
	// 不传 mesh Hub → mesh 的 session_send/session_list 不注册（§6.3.1 的唯一例外）。
	if exists, _ := toolVisible(t, eng, "session_send"); exists {
		t.Error("mesh session_send must not be registered in the hub engine")
	}
}

// Disable 精确名 / 前缀 / preset 都必须只改可见性，不删工具（codemode 也编排不到）。
func TestHubEngineProfilePolicyHidesTools(t *testing.T) {
	m, _ := testManager(t)
	ws := m.hubRuntime("ag_engine")

	eng := hubEngineFor(t, m, ws, agenthub.ToolPolicy{Disable: []string{"bash"}})
	if exists, visible := toolVisible(t, eng, "bash"); !exists || visible {
		t.Fatalf("disable=bash must keep it registered but hidden (exists=%v visible=%v)", exists, visible)
	}
	// 直呼路径也必须被拦（不是只从 schema 里消失）。
	if _, err := eng.GetTool(context.Background(), "bash"); err == nil || !strings.Contains(err.Error(), "disabled by policy") {
		t.Fatalf("GetTool(bash) under policy = %v, want disabled-by-policy", err)
	}
	// 工具实例仍在注册表里（可选接口不被包装破坏的前提）。
	found := false
	for _, tt := range eng.Tools() {
		if tt.Name() == "bash" {
			found = true
		}
	}
	if !found {
		t.Fatal("hidden tool must remain in Tools() (engine-level hide, not re-registration)")
	}
	// ToolParams 不再声明它。
	params, err := eng.ToolParams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, sc := range params {
		if sc.Name == "bash" {
			t.Fatal("hidden tool must not appear in ToolParams")
		}
	}
	if _, visible := toolVisible(t, eng, "read_file"); !visible {
		t.Fatal("disable=bash must not touch read_file")
	}

	ro := hubEngineFor(t, m, ws, agenthub.ToolPolicy{Preset: agenthub.ToolPresetReadonly})
	if _, visible := toolVisible(t, ro, "write_file"); visible {
		t.Fatal("readonly preset must hide write_file")
	}
	if _, visible := toolVisible(t, ro, "bash"); visible {
		t.Fatal("readonly preset must hide bash")
	}
	if _, visible := toolVisible(t, ro, "read_file"); !visible {
		t.Fatal("readonly preset must keep read_file (ReadOnlyTool declaration)")
	}

	custom := hubEngineFor(t, m, ws, agenthub.ToolPolicy{
		Preset: agenthub.ToolPresetCustom, Allow: []string{"read_file", "grep"},
	})
	for _, name := range []string{"read_file", "grep"} {
		if _, visible := toolVisible(t, custom, name); !visible {
			t.Errorf("custom allow must keep %q", name)
		}
	}
	for _, name := range []string{"bash", "write_file", "agent_spawn"} {
		if _, visible := toolVisible(t, custom, name); visible {
			t.Errorf("custom allow must hide %q", name)
		}
	}
}

// fakeTool 最小工具（BaseTool 提供元数据；Call/ValidParams 必需）。
type fakeTool struct {
	tools.BaseTool
	readOnly bool
}

func (f *fakeTool) ValidParams(context.Context, string, string) error { return nil }
func (f *fakeTool) Call(context.Context, string, string) (string, error) {
	return "", nil
}

func (f *fakeTool) ReadOnly() bool { return f.readOnly }

// 策略应用层：前缀、MCP server/tool 级开关、allowed tools 白名单。
func TestHubEnginePolicyAppliesToMCPAndPrefixes(t *testing.T) {
	eng := tools.NewToolEngine()
	for _, t2 := range []*fakeTool{
		{BaseTool: tools.BaseTool{Name_: "bash"}},
		{BaseTool: tools.BaseTool{Name_: "browser_navigate"}},
		{BaseTool: tools.BaseTool{Name_: "browserx"}},
		{BaseTool: tools.BaseTool{Name_: "mcp__gh__search"}},
		{BaseTool: tools.BaseTool{Name_: "mcp__gh__write"}},
		{BaseTool: tools.BaseTool{Name_: "read_file"}, readOnly: true},
	} {
		eng.RegisterTool(context.Background(), t2)
	}

	policy := agenthub.ToolPolicy{
		Disable: []string{"bash", "browser_*", "mcp__gh__write"},
		MCP:     agenthub.MCPPolicy{DisableServers: []string{"github"}},
	}
	hidden := applyHubToolPolicy(context.Background(), eng, policy)
	got := map[string]bool{}
	for _, n := range hidden {
		got[n] = true
	}
	for _, want := range []string{"bash", "browser_navigate", "mcp__gh__write"} {
		if !got[want] {
			t.Errorf("policy must hide %q (hidden=%v)", want, hidden)
		}
	}
	for _, keep := range []string{"browserx", "mcp__gh__search", "read_file"} {
		if got[keep] {
			t.Errorf("policy must keep %q", keep)
		}
	}
	// 撤下是调用期硬约束：GetTool 直接报 disabled（直呼与 codemode 两条路都走它）。
	if _, err := eng.GetTool(context.Background(), "bash"); err == nil {
		t.Fatal("hidden tool must not be retrievable")
	}

	// MCP 白名单：只允许 gh.search → write 被隐藏（即使不在 disable 里）。
	eng2 := tools.NewToolEngine()
	eng2.RegisterTool(context.Background(), &fakeTool{BaseTool: tools.BaseTool{Name_: "mcp__gh__search"}})
	eng2.RegisterTool(context.Background(), &fakeTool{BaseTool: tools.BaseTool{Name_: "mcp__gh__write"}})
	applyHubToolPolicy(context.Background(), eng2, agenthub.ToolPolicy{
		MCP: agenthub.MCPPolicy{AllowTools: []string{"gh.search"}},
	})
	if _, visible := toolVisible(t, eng2, "mcp__gh__search"); !visible {
		t.Fatal("allow_tools must keep gh.search")
	}
	if _, visible := toolVisible(t, eng2, "mcp__gh__write"); visible {
		t.Fatal("allow_tools must hide gh.write")
	}

	// 名字里含 "__" 的 server/tool：真实 MCP 适配器走 MCPServerName/MCPToolName 访问器，
	// 不靠拆名字（EngineName 裸拼接，拆名字在这里必然有歧义）。
	eng3 := tools.NewToolEngine()
	eng3.RegisterTool(context.Background(), &fakeMCPTool{
		fakeTool: fakeTool{BaseTool: tools.BaseTool{Name_: "mcp__my__server__search"}},
		server:   "my__server", tool: "search",
	})
	eng3.RegisterTool(context.Background(), &fakeMCPTool{
		fakeTool: fakeTool{BaseTool: tools.BaseTool{Name_: "mcp__my__server__a__b"}},
		server:   "my__server", tool: "a__b",
	})
	applyHubToolPolicy(context.Background(), eng3, agenthub.ToolPolicy{
		MCP: agenthub.MCPPolicy{DisableServers: []string{"my__server"}},
	})
	for _, name := range []string{"mcp__my__server__search", "mcp__my__server__a__b"} {
		if _, visible := toolVisible(t, eng3, name); visible {
			t.Errorf("disable_servers must hide %q (名字含 __ 也不能绕过)", name)
		}
	}

	// 没有身份访问器的第三方 MCP 工具：名字反解有歧义时，只要 Profile 限制了 MCP，
	// 就按「可能命中」拒绝（fail-closed）—— 不能因为分不清边界而放行。
	plain := func(name string) *fakeTool {
		return &fakeTool{BaseTool: tools.BaseTool{Name_: name, Description_: name, Params_: map[string]any{}}}
	}
	eng4 := tools.NewToolEngine()
	eng4.RegisterTool(context.Background(), plain("mcp__my__server__search"))
	applyHubToolPolicy(context.Background(), eng4, agenthub.ToolPolicy{
		MCP: agenthub.MCPPolicy{DisableServers: []string{"my__server"}},
	})
	if _, visible := toolVisible(t, eng4, "mcp__my__server__search"); visible {
		t.Error("ambiguous mcp name must fail closed when the profile restricts MCP")
	}

	// 同样的歧义名字，但 Profile 完全不限制 MCP → 照常可见（零值全开，D18）。
	eng5 := tools.NewToolEngine()
	eng5.RegisterTool(context.Background(), plain("mcp__my__server__search"))
	applyHubToolPolicy(context.Background(), eng5, agenthub.ToolPolicy{})
	if _, visible := toolVisible(t, eng5, "mcp__my__server__search"); !visible {
		t.Error("zero-value policy must not hide ambiguous mcp names")
	}
}

// fakeMCPTool 暴露 MCP 身份访问器（模拟 mcp 包的适配器）。
type fakeMCPTool struct {
	fakeTool
	server, tool string
}

func (f *fakeMCPTool) MCPServerName() string { return f.server }
func (f *fakeMCPTool) MCPToolName() string   { return f.tool }

// 沙箱根 = 智能体 home，且**不放开整个 home 写权限**（项目会话为了 pip/npm 传
// WithHomeWrite；Hub 智能体只该写自己的 home）。执行侧验证在 temp 目录下不可靠
// （seatbelt 对 /tmp 整段可写），所以这里断言策略文本：
//   - home 有 file-write* 放行；
//   - 用户 home 只有读放行、没有写放行（WithHomeWrite 没被传进来）；
//   - home 在 ~/.go-code 之下时，重开 allow 必须排在敏感 deny 之后（见
//     sandbox/open_subpaths_test.go 的顺序测试）。
func TestHubEngineSandboxRootsAtAgentHome(t *testing.T) {
	if !sandbox.Available() {
		t.Skip("seatbelt not available")
	}
	m, _ := testManager(t)
	// 把 appDataDir 挪到 $HOME/.go-code 下 —— 这才是生产布局，也是「home 落在默认
	// 敏感 deny 里、必须在 deny 之后重开」这条策略唯一能被观察到的形态。
	homeDir, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	prevOverride := appDataDirOverride
	appDataDirOverride = filepath.Join(homeDir, ".go-code")
	t.Cleanup(func() { appDataDirOverride = prevOverride })
	ws := m.hubRuntime("ag_sandbox")
	sb := m.hubSandbox(ws.path, agenthub.ToolPolicy{}, agenthub.RunPolicy{})
	if sb == nil {
		t.Fatal("hub sandbox should be available on this host")
	}
	seat, ok := sb.(*sandbox.Seatbelt)
	if !ok {
		t.Fatalf("hub sandbox type = %T, want *sandbox.Seatbelt", sb)
	}
	home := ws.path
	realHome, err := filepath.EvalSymlinks(home)
	if err != nil {
		realHome = home
	}
	exposed := `(allow file-write* (subpath "` + realHome + `"))`
	denyLine := `(deny file-write* (subpath "` + filepath.Dir(filepath.Dir(filepath.Dir(realHome))) + `"))`
	if !strings.Contains(seat.Policy, exposed) {
		t.Fatalf("hub sandbox does not allow writing its own home:\n%s", seat.Policy)
	}
	// 生产布局下 ~/.go-code 是默认敏感 deny：重开 allow 必须排在 deny 之后（SBPL 后规则胜）。
	if di := strings.Index(seat.Policy, denyLine); di >= 0 {
		if ai := strings.LastIndex(seat.Policy, exposed); ai < di {
			t.Fatalf("hub home re-open must come AFTER the sensitive deny\n%s", seat.Policy)
		}
	} else {
		t.Fatalf("expected ~/.go-code deny in policy:\n%s", seat.Policy)
	}
	// tools.bash_roots：额外根只读（读放行、写不放行）。
	ro := t.TempDir()
	wsRoot := t.TempDir()
	sb2 := m.hubSandbox(ws.path, agenthub.ToolPolicy{BashRoots: []string{ro}}, agenthub.RunPolicy{WorkspaceRoots: []string{wsRoot, ro}})
	seat2, ok := sb2.(*sandbox.Seatbelt)
	if !ok {
		t.Fatalf("hub sandbox type = %T", sb2)
	}
	roReal, err := filepath.EvalSymlinks(ro)
	if err != nil {
		roReal = ro
	}
	if !strings.Contains(seat2.Policy, `(allow file-read* (subpath "`+roReal+`"))`) {
		t.Fatalf("bash_roots %s missing read allow", roReal)
	}
	if strings.Contains(seat2.Policy, `(allow file-write* (subpath "`+roReal+`"))`) {
		t.Fatalf("bash_roots %s must not be writable", roReal)
	}
	if !strings.Contains(seat2.Policy, `(deny file-write* (subpath "`+roReal+`"))`) {
		t.Fatalf("bash_roots %s must get an explicit write deny (父级 allow 会覆盖只读语义)", roReal)
	}
	wsReal, err := filepath.EvalSymlinks(wsRoot)
	if err != nil {
		wsReal = wsRoot
	}
	if !strings.Contains(seat2.Policy, `(allow file-read* (subpath "`+wsReal+`"))`) {
		t.Fatalf("workspace_roots %s missing read allow", wsReal)
	}
	if strings.Contains(seat2.Policy, `(allow file-write* (subpath "`+wsReal+`"))`) {
		t.Fatalf("workspace_roots %s must not be writable", wsReal)
	}

	userHome, _ := os.UserHomeDir()
	if userHome != "" {
		uh, err := filepath.EvalSymlinks(userHome)
		if err != nil {
			uh = userHome
		}
		if strings.Contains(seat.Policy, `(allow file-write* (subpath "`+uh+`"))`) {
			t.Fatal("hub sandbox must not grant write access to the whole user home")
		}
	}
}

// hubDynTool 最小可注册工具：用来模拟「装配之后才出现」的工具（MCP 重连、插件热装）。
type hubDynTool struct{ tools.BaseTool }

func (t *hubDynTool) ValidParams(context.Context, string, string) error { return nil }
func (t *hubDynTool) Call(context.Context, string, string) (string, error) {
	return "RAN", nil
}

// 策略必须覆盖**装配之后**注册的工具（§6.3.4「调用期再判一次」）。
// 只按装配时的快照 HideTool 会漏：那条路径只认识当时已存在的名字。
func TestHubEnginePolicyAppliesToLateRegisteredTools(t *testing.T) {
	m, _ := testManager(t)
	ws := m.hubRuntime("ag_engine")
	eng := hubEngineFor(t, m, ws, agenthub.ToolPolicy{Disable: []string{"dyn_*"}})

	for _, name := range []string{"dyn_shell", "later_ok"} {
		eng.RegisterTool(context.Background(), &hubDynTool{BaseTool: tools.BaseTool{
			Name_: name, Description_: name, Params_: map[string]any{},
		}})
	}

	// 命中策略 → 调不到（GetTool 报 disabled，而不是 not registered）
	if _, err := eng.GetTool(context.Background(), "dyn_shell"); err == nil ||
		!strings.Contains(err.Error(), "disabled by policy") {
		t.Fatalf("late-registered dyn_shell should be blocked by policy, got err=%v", err)
	}
	// 未命中策略 → 照常可见（不能把整批动态工具一刀切）
	tool, err := eng.GetTool(context.Background(), "later_ok")
	if err != nil {
		t.Fatalf("late-registered later_ok should stay visible: %v", err)
	}
	if tool == nil {
		t.Fatal("later_ok: nil tool")
	}
	// 也不能出现在模型可见的 schema 列表里
	schemas, err := eng.ToolParams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, s := range schemas {
		if s.Name == "dyn_shell" {
			t.Fatal("dyn_shell must not be declared to the model")
		}
	}
}

// 给模型看的清单（codemode 目录 / 技能能力清单）不能出现被撤下的工具：
// 调不到但仍被描述，等于误导模型去试（P1-01 复核的 non-blocking）。
func TestHubEngineVisibleToolsExcludeHidden(t *testing.T) {
	m, _ := testManager(t)
	ws := m.hubRuntime("ag_engine")
	eng := hubEngineFor(t, m, ws, agenthub.ToolPolicy{Disable: []string{"bash"}})

	inAll, inVisible := false, false
	for _, tool := range eng.Tools() {
		if tool.Name() == "bash" {
			inAll = true
		}
	}
	for _, tool := range eng.VisibleTools() {
		if tool.Name() == "bash" {
			inVisible = true
		}
	}
	if !inAll {
		t.Fatal("Tools() must still list the retired tool (诊断/编排方需要知道注册表里有什么)")
	}
	if inVisible {
		t.Fatal("VisibleTools() must not expose a tool hidden by policy")
	}
	// 未撤下的工具照常在 VisibleTools 里。
	foundRead := false
	for _, tool := range eng.VisibleTools() {
		if tool.Name() == "read_file" {
			foundRead = true
		}
	}
	if !foundRead {
		t.Fatal("VisibleTools() dropped a tool that is not hidden")
	}
}
