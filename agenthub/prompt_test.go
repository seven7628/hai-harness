package agenthub

import (
	"strings"
	"testing"
)

func TestPromptL1Only(t *testing.T) {
	got := RenderSystem(PromptSpec{})
	if got != hubProtocolPrompt {
		t.Fatalf("L1-only prompt changed:\n%s", got)
	}
	for _, want := range []string{"## Tools", "@Name", "assign_task", "## Workspaces", "## Safety"} {
		if !strings.Contains(got, want) {
			t.Fatalf("L1 missing %q", want)
		}
	}
	for _, unwanted := range []string{"## Group:", "## Identity", "DesktopCodePrompt", "DefaultSystemPrompt"} {
		if strings.Contains(got, unwanted) {
			t.Fatalf("L1 unexpectedly contains %q", unwanted)
		}
	}
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
	want := hubProtocolPrompt + "\n\n" + roster + "\n\n" + identity
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
	if got := RenderSystem(PromptSpec{Agent: empty}); got != hubProtocolPrompt {
		t.Fatalf("empty identity should not append a layer:\n%s", got)
	}
	if got := RenderSystem(PromptSpec{Group: &Group{Name: "空群"}}); got != hubProtocolPrompt {
		t.Fatalf("empty roster should not append a layer:\n%s", got)
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
