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
	if _, visible := toolVisible(t, eng, "bash"); visible {
		t.Fatal("disable=bash must hide bash")
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
	// hidden 档真的不可达：codemode 的可编排判定为 false。
	tool, err := eng.GetTool(context.Background(), "bash")
	if err != nil {
		t.Fatal(err)
	}
	if tools.CallableFromScript(tool) {
		t.Fatal("hidden tool must not be callable from codemode scripts")
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
}

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
	ws := m.hubRuntime("ag_sandbox")
	sb := m.hubSandbox(ws.path, agenthub.ToolPolicy{})
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
	if !strings.Contains(seat.Policy, `(allow file-write* (subpath "`+realHome+`"))`) {
		t.Fatalf("hub sandbox does not allow writing its own home:\n%s", seat.Policy)
	}
	// tools.bash_roots：额外根只读（读放行、写不放行）。
	ro := t.TempDir()
	sb2 := m.hubSandbox(ws.path, agenthub.ToolPolicy{BashRoots: []string{ro}})
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
