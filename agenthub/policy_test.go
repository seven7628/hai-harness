package agenthub

// P1-01：工具面策略的判定（引擎装配时用）。零值必须全开 —— 回归里专门钉这一条。

import "testing"

func TestToolPolicyZeroValueAllowsEverything(t *testing.T) {
	var p ToolPolicy
	for _, name := range []string{"bash", "write_file", "read_file", "agent_spawn", "mcp__gh__search", "browser_navigate"} {
		if !p.ToolVisible(name, false) {
			t.Fatalf("zero-value policy must allow %q (D18: 默认全开)", name)
		}
	}
	if !p.MCPAllowed("gh", "search") {
		t.Fatal("zero-value MCP policy must allow everything")
	}
}

func TestToolPolicyDisableExactAndPrefix(t *testing.T) {
	p := ToolPolicy{Disable: []string{"bash", "browser_*"}}
	cases := []struct {
		name     string
		readOnly bool
		want     bool
	}{
		{"bash", false, false},
		{"browser_navigate", false, false},
		{"browser_screenshot", false, false},
		{"read_file", true, true},     // 前缀只吃 browser_，不误伤
		{"browserx", false, true},     // 前缀必须带下划线边界：browser_* 不匹配 browserx
		{"bash_session", false, true}, // 精确名不成前缀
	}
	for _, c := range cases {
		if got := p.ToolVisible(c.name, c.readOnly); got != c.want {
			t.Errorf("ToolVisible(%q) = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestToolPolicyPresets(t *testing.T) {
	readonly := ToolPolicy{Preset: ToolPresetReadonly}
	if readonly.ToolVisible("write_file", false) {
		t.Fatal("readonly preset must hide write tools")
	}
	if !readonly.ToolVisible("read_file", true) {
		t.Fatal("readonly preset must keep read-only tools")
	}
	// 工具没声明只读（readOnly=false）→ 保守隐藏
	if readonly.ToolVisible("mystery_tool", false) {
		t.Fatal("readonly preset must hide tools without a read-only declaration")
	}

	custom := ToolPolicy{Preset: ToolPresetCustom, Allow: []string{"read_file", "grep", "task_*"}}
	for _, name := range []string{"read_file", "grep", "task_status"} {
		if !custom.ToolVisible(name, false) {
			t.Errorf("custom preset must allow %q", name)
		}
	}
	for _, name := range []string{"bash", "write_file"} {
		if custom.ToolVisible(name, false) {
			t.Errorf("custom preset must hide %q", name)
		}
	}

	// 非法 preset 按最保守的自定义白名单处理（Validate 会拒，这里兜运行期手改文件）
	bad := ToolPolicy{Preset: "everything", Allow: []string{"read_file"}}
	if bad.ToolVisible("bash", false) || !bad.ToolVisible("read_file", false) {
		t.Fatal("invalid preset must fall back to the custom whitelist")
	}
}

func TestToolPolicyMCP(t *testing.T) {
	cases := []struct {
		name         string
		p            ToolPolicy
		server, tool string
		want         bool
	}{
		{"default", ToolPolicy{}, "gh", "search", true},
		{"all disabled", ToolPolicy{MCP: MCPPolicy{Disable: true}}, "gh", "search", false},
		{"server disabled", ToolPolicy{MCP: MCPPolicy{DisableServers: []string{"gh"}}}, "gh", "search", false},
		{"server prefix", ToolPolicy{MCP: MCPPolicy{DisableServers: []string{"gh-*"}}}, "gh-remote", "search", false},
		{"tool disabled", ToolPolicy{MCP: MCPPolicy{DisableTools: []string{"gh.write"}}}, "gh", "write", false},
		{"tool wildcard", ToolPolicy{MCP: MCPPolicy{DisableTools: []string{"gh.*"}}}, "gh", "search", false},
		{"other tool fine", ToolPolicy{MCP: MCPPolicy{DisableTools: []string{"gh.write"}}}, "gh", "search", true},
		{"allow list hit", ToolPolicy{MCP: MCPPolicy{AllowTools: []string{"gh.search"}}}, "gh", "search", true},
		{"allow list miss", ToolPolicy{MCP: MCPPolicy{AllowTools: []string{"gh.search"}}}, "gh", "write", false},
		{"allow list wildcard", ToolPolicy{MCP: MCPPolicy{AllowTools: []string{"gh.*"}}}, "gh", "write", true},
	}
	for _, c := range cases {
		if got := c.p.MCPAllowed(c.server, c.tool); got != c.want {
			t.Errorf("%s: MCPAllowed(%q,%q) = %v, want %v", c.name, c.server, c.tool, got, c.want)
		}
	}
}

func TestSplitMCPToolName(t *testing.T) {
	cases := []struct {
		in           string
		server, tool string
		ok           bool
	}{
		{"mcp__gh__search", "gh", "search", true},
		{"mcp__dev-radius__get", "dev-radius", "get", true},
		{"mcp__gh__a__b", "", "", false},   // tool 里出现 __ → 不是本仓库命名
		{"mcp____x", "", "", false},        // server 为空
		{"mcp__gh__", "", "", false},       // tool 为空
		{"search", "", "", false},          // 非 MCP
		{"mcp_gh_search", "", "", false},   // 前缀不完整
		{"Mcp__gh__search", "", "", false}, // 大小写敏感
	}
	for _, c := range cases {
		server, tool, ok := SplitMCPToolName(c.in)
		if ok != c.ok || server != c.server || tool != c.tool {
			t.Errorf("SplitMCPToolName(%q) = (%q,%q,%v), want (%q,%q,%v)", c.in, server, tool, ok, c.server, c.tool, c.ok)
		}
	}
}
