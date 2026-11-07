package agenthub

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func goldenL1(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "l1.golden"))
	if err != nil {
		t.Fatalf("read L1 golden: %v", err)
	}
	return string(b)
}

// goldenRoster 读名册层 golden。文件**不带尾随换行**：比的是 renderRoster 的
// 原文（L1 的 l1.golden 同口径），编辑器自动补换行会让测试以字节差异失败。
func goldenRoster(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", "roster.golden"))
	if err != nil {
		t.Fatalf("read roster golden: %v", err)
	}
	return string(b)
}

func TestPromptL1Only(t *testing.T) {
	got := RenderSystem(PromptSpec{})
	if want := goldenL1(t); got != want {
		t.Fatalf("L1-only prompt changed:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	for _, want := range []string{"## Tools", "@Name", "assign_task", "## Workspaces", "## Safety"} {
		if !strings.Contains(got, want) {
			t.Fatalf("L1 missing %q", got)
		}
	}
	for _, unwanted := range []string{"## Group:", "## Identity"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("L1 unexpectedly contains %q", unwanted)
		}
	}
	// 真正的去 L0 是装配层 WithBaseSystemPrompt("") 的职责；RenderSystem 不接触 L0，
	// 这条注释提醒 P0-08/P1 的 buildHubLoop 测试必须单独断言。
}

// rosterGoldenSpec 是名册 golden 的固定输入：owner/member/observer 三种角色、
// CJK 职责、能力摘要（只读/断网/禁用工具）与一个缺档案的成员。
// 改这里的字段等于改 golden —— 两处必须一起改，否则测试会以字节差异失败。
func rosterGoldenSpec() PromptSpec {
	alice := &AgentProfile{
		ID: "ag_aaaaaaaa", Name: "Researcher", Role: "竞品研究员",
		Description: "负责竞品价格、功能与公开情报。",
	}
	bob := &AgentProfile{
		ID: "ag_bbbbbbbb", Name: "Critic", Role: "风险审查",
		Description: "只审阅，不给结论。",
		Tools:       ToolPolicy{Preset: ToolPresetReadonly},
	}
	carol := &AgentProfile{
		ID: "ag_cccccccc", Name: "Writer",
		Description: "长文写作与编辑。",
		Tools:       ToolPolicy{DisableNetwork: true, Disable: []string{"browser_*"}},
	}
	return PromptSpec{
		Agent: alice,
		Group: &Group{
			Name:         "竞品研究",
			OwnerAgentID: "ag_aaaaaaaa",
			Members: []Member{
				{AgentID: "ag_aaaaaaaa", Role: RoleOwner},
				{AgentID: "ag_bbbbbbbb", Role: RoleObserver},
				{AgentID: "ag_cccccccc", Role: RoleMember},
				{AgentID: "ag_dddddddd", Role: RoleMember},
			},
		},
		Members: []*AgentProfile{alice, bob, carol},
	}
}

// 名册层逐字节 golden：群名、我的角色/职责/能力、每个成员的名字+角色+id+职责+能力，
// 缺档案的成员必须显式出现（不能静默丢）。
func TestPromptRosterGolden(t *testing.T) {
	layers := RenderLayers(rosterGoldenSpec())
	if len(layers) != 3 || layers[1].Key != "roster" {
		t.Fatalf("layers = %+v, want protocol+roster+identity", layers)
	}
	if got, want := layers[1].Text, goldenRoster(t); got != want {
		t.Fatalf("roster golden mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	for _, want := range []string{
		"- Researcher (owner, ag_aaaaaaaa)",
		"- Critic (observer, ag_bbbbbbbb)",
		"- Writer (member, ag_cccccccc)",
		"- unknown member (member, ag_dddddddd) — profile unavailable",
		"read-only tools",
		"no network",
	} {
		if !strings.Contains(layers[1].Text, want) {
			t.Fatalf("roster missing %q:\n%s", want, layers[1].Text)
		}
	}
}

func TestPromptRosterAndIdentityGolden(t *testing.T) {
	spec := rosterGoldenSpec()
	roster := goldenRoster(t)
	identity := `## Identity
Name: Researcher
Role: 竞品研究员
Description: 负责竞品价格、功能与公开情报。`
	want := goldenL1(t) + "\n\n" + roster + "\n\n" + identity
	if got := RenderSystem(spec); got != want {
		t.Fatalf("prompt golden mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// 名册只放稳定信息：状态（idle/running）绝不能进 system prompt，否则每次状态变化
// 都要重建 loop、prompt cache 失效（§6.5.6）。状态走 group_members()/agent_status()。
func TestPromptRosterHasNoLiveStatus(t *testing.T) {
	layers := RenderLayers(rosterGoldenSpec())
	if len(layers) < 2 || layers[1].Key != "roster" {
		t.Fatalf("layers = %+v, want a roster layer", layers)
	}
	// 只看名册层：L1 里「long-running」这种词是协议文案，不是运行态。
	for _, unwanted := range []string{"idle", "running", "waiting", "status"} {
		if strings.Contains(layers[1].Text, unwanted) {
			t.Fatalf("roster leaked live status %q into the system prompt:\n%s", unwanted, layers[1].Text)
		}
	}
}

// 成员顺序 = Group.Members 的落盘顺序，与 Members（profile 切片）的传入顺序无关。
func TestPromptRosterOrderIsGroupOrder(t *testing.T) {
	spec := rosterGoldenSpec()
	// 反转 profile 切片：名册仍然按群成员表输出，不能靠 map 迭代或切片顺序。
	rev := make([]*AgentProfile, 0, len(spec.Members))
	for i := len(spec.Members) - 1; i >= 0; i-- {
		rev = append(rev, spec.Members[i])
	}
	shuffled := spec
	shuffled.Members = rev
	if got, want := RenderSystem(shuffled), RenderSystem(spec); got != want {
		t.Fatalf("roster depends on profile slice order:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// 能力摘要来自 Profile 的工具声明：custom/readonly/断网/MCP/disable 都要如实出现，
// 且长列表要截断（提示词保持紧凑）。
func TestPromptRosterCapabilities(t *testing.T) {
	cases := []struct {
		name  string
		tools ToolPolicy
		want  string
	}{
		{"default", ToolPolicy{}, "all tools"},
		{"readonly", ToolPolicy{Preset: ToolPresetReadonly}, "read-only tools"},
		{"custom", ToolPolicy{Preset: ToolPresetCustom, Allow: []string{"read_file", "grep"}}, "custom tools (2 allowed)"},
		{"no-network", ToolPolicy{DisableNetwork: true}, "all tools · no network"},
		{"no-mcp", ToolPolicy{MCP: MCPPolicy{Disable: true}}, "all tools · no MCP"},
		{"disable", ToolPolicy{Disable: []string{"browser_*", "cron_*"}}, "all tools · off: browser_*, cron_*"},
		{"truncated", ToolPolicy{Disable: []string{"a", "b", "c", "d"}}, "all tools · off: a, b, c…"},
	}
	for _, tc := range cases {
		p := &AgentProfile{ID: "ag_caps", Name: "Caps", Role: "测试", Tools: tc.tools}
		g := &Group{Name: "能力群", Members: []Member{{AgentID: p.ID, Role: RoleMember}}}
		got := RenderSystem(PromptSpec{Agent: p, Group: g, Members: []*AgentProfile{p}})
		if !strings.Contains(got, tc.want) {
			t.Fatalf("%s: roster missing %q:\n%s", tc.name, tc.want, got)
		}
		if n := strings.Count(got, tc.want); n != 2 {
			// 「我是谁」与成员行各一次：两处都来自同一份 Profile，不能只更新一处。
			t.Fatalf("%s: capability %q appears %d times, want 2 (self + member line):\n%s", tc.name, tc.want, n, got)
		}
	}
}

func TestPromptIdentityInstructionsAndEmptyLayers(t *testing.T) {
	p := &AgentProfile{Name: "Writer", Instructions: "先列提纲。\n再写正文。", Persona: "简洁"}
	got := RenderSystem(PromptSpec{Agent: p})
	for _, want := range []string{"## Identity", "Instructions:\n先列提纲。", "Persona: 简洁"} {
		if !strings.Contains(got, want) {
			t.Fatalf("identity missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "## Group:") {
		t.Fatal("empty group should not append a roster layer")
	}

	empty := &AgentProfile{}
	if got := RenderSystem(PromptSpec{Agent: empty}); got != goldenL1(t) {
		t.Fatalf("empty identity should not append a layer:\n%s", got)
	}
	if got := RenderSystem(PromptSpec{Group: &Group{Name: "空群"}}); got != goldenL1(t) {
		t.Fatalf("empty roster should not append a layer:\n%s", got)
	}
}

func TestPromptCJKAndFallbackRendering(t *testing.T) {
	alice := &AgentProfile{ID: "ag_aaaaaaaa", Name: "Researcher", Role: "竞品研究员", Description: "负责竞品价格。"}
	got := RenderSystem(PromptSpec{
		Group:   &Group{ID: "grp_aaaaaa", Members: []Member{{AgentID: "ag_aaaaaaaa", Role: RoleOwner}}},
		Agent:   alice,
		Members: []*AgentProfile{alice},
	})
	if strings.Contains(got, "。。") {
		t.Fatalf("CJK double punctuation: %s", got)
	}
	if !strings.Contains(got, "## Group: grp_aaaaaa") {
		t.Fatalf("empty group name should fall back to id: %s", got)
	}
	missing := RenderSystem(PromptSpec{
		Group: &Group{Name: "G", Members: []Member{{AgentID: "ag_bbbbbbbb", Role: RoleMember}}},
	})
	if !strings.Contains(missing, "unknown member (member, ag_bbbbbbbb)") {
		t.Fatalf("missing profile fallback missing: %s", missing)
	}
	if !strings.Contains(missing, "profile unavailable") {
		t.Fatalf("missing profile fallback must say it is unavailable: %s", missing)
	}
	// 多行 description 必须压成单行：裸换行会把后面的成员顶出列表。
	multiline := &AgentProfile{ID: "ag_mmmmmmmm", Name: "Multi", Description: "第一行\n第二行\t缩进"}
	line := RenderSystem(PromptSpec{
		Group:   &Group{Name: "G", Members: []Member{{AgentID: multiline.ID, Role: RoleMember}}},
		Members: []*AgentProfile{multiline},
	})
	if !strings.Contains(line, "- Multi (member, ag_mmmmmmmm) — 第一行 第二行 缩进") {
		t.Fatalf("multiline description broke the roster line:\n%s", line)
	}
}

func TestPromptByteStable(t *testing.T) {
	spec := PromptSpec{
		Agent: &AgentProfile{ID: "ag_aaaaaaaa", Name: "Researcher", Role: "研究"},
		Group: &Group{Name: "G", OwnerAgentID: "ag_aaaaaaaa", Members: []Member{
			{AgentID: "ag_bbbbbbbb", Role: RoleMember},
			{AgentID: "ag_aaaaaaaa", Role: RoleOwner},
		}},
		Members: []*AgentProfile{
			{ID: "ag_aaaaaaaa", Name: "Researcher", Role: "研究"},
			{ID: "ag_bbbbbbbb", Name: "Writer", Role: "写作"},
		},
	}
	first := RenderSystem(spec)
	for i := 0; i < 32; i++ {
		if got := RenderSystem(spec); got != first {
			t.Fatalf("RenderSystem changed on call %d", i)
		}
	}
	// 成员顺序是群元数据的一部分，不能靠 map 迭代改变名册顺序。
	writerLine := strings.Index(first, "- Writer (member, ag_bbbbbbbb)")
	researcherLine := strings.Index(first, "- Researcher (owner, ag_aaaaaaaa)")
	if writerLine < 0 || researcherLine < 0 || writerLine > researcherLine {
		t.Fatalf("roster order not preserved:\n%s", first)
	}
}

// 分层预览必须与真正下发的提示词逐字节同源（设置页不能另写一份文案）。
func TestRenderLayersMatchesSystem(t *testing.T) {
	alice := &AgentProfile{ID: "ag_aaaaaaaa", Name: "Researcher", Role: "竞品研究员", Instructions: "只信一手来源"}
	g := &Group{ID: "gr_gggggggg", Name: "Research", Members: []Member{{AgentID: alice.ID, Role: RoleOwner}}}
	spec := PromptSpec{Agent: alice, Group: g, Members: []*AgentProfile{alice}}

	layers := RenderLayers(spec)
	if len(layers) != 3 {
		t.Fatalf("layers = %d, want 3 (protocol/roster/identity): %+v", len(layers), layers)
	}
	wantKeys := []string{"protocol", "roster", "identity"}
	parts := make([]string, 0, len(layers))
	for i, layer := range layers {
		if layer.Key != wantKeys[i] {
			t.Fatalf("layer[%d].key = %q, want %q", i, layer.Key, wantKeys[i])
		}
		if layer.Title == "" || strings.TrimSpace(layer.Text) == "" {
			t.Fatalf("layer %q must have title and text: %+v", layer.Key, layer)
		}
		parts = append(parts, layer.Text)
	}
	if got, want := strings.Join(parts, "\n\n"), RenderSystem(spec); got != want {
		t.Fatalf("layers joined != RenderSystem:\n--- layers ---\n%s\n--- system ---\n%s", got, want)
	}

	// 空输入：只剩 L1，且仍是同一个字节串（空层不产生空串层）。
	bare := RenderLayers(PromptSpec{})
	if len(bare) != 1 || bare[0].Key != "protocol" {
		t.Fatalf("bare layers = %+v, want only protocol", bare)
	}
	if bare[0].Text != RenderSystem(PromptSpec{}) {
		t.Fatal("L1-only layers text != RenderSystem")
	}
}
