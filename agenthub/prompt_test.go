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

func TestPromptL1Only(t *testing.T) {
	got := RenderSystem(PromptSpec{})
	if want := goldenL1(t); got != want {
		t.Fatalf("L1-only prompt changed:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
	for _, want := range []string{"## Tools", "@Name", "assign_task", "## Workspaces", "## Safety"} {
		if !strings.Contains(got, want) {
			t.Fatalf("L1 missing %q", want)
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

func TestPromptRosterAndIdentityGolden(t *testing.T) {
	alice := &AgentProfile{ID: "ag_aaaaaaaa", Name: "Researcher", Role: "竞品研究员", Description: "负责竞品价格"}
	bob := &AgentProfile{ID: "ag_bbbbbbbb", Name: "Critic", Role: "风险审查"}
	spec := PromptSpec{
		Agent: alice,
		Group: &Group{
			Name:         "竞品研究",
			OwnerAgentID: "ag_aaaaaaaa",
			Members: []Member{
				{AgentID: "ag_aaaaaaaa", Role: RoleOwner},
				{AgentID: "ag_bbbbbbbb", Role: RoleObserver},
			},
		},
		Members: []*AgentProfile{alice, bob},
	}
	roster := `## Group: 竞品研究
You are Researcher (owner). Responsibilities: 竞品研究员 / 负责竞品价格.
Members:
- Researcher (owner) — 竞品研究员 / 负责竞品价格
- Critic (observer) — 风险审查
You can talk to them with @Name, or assign tracked work with assign_task(Name, instruction).`
	identity := `## Identity
Name: Researcher
Role: 竞品研究员
Description: 负责竞品价格`
	want := goldenL1(t) + "\n\n" + roster + "\n\n" + identity
	if got := RenderSystem(spec); got != want {
		t.Fatalf("prompt golden mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
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
	if !strings.Contains(missing, "unknown member (ag_bbbbbbbb)") {
		t.Fatalf("missing profile fallback missing: %s", missing)
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
	writerLine := strings.Index(first, "- Writer (member)")
	researcherLine := strings.Index(first, "- Researcher (owner)")
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
