package agents

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/skills"
)

// skillRegistryWith 造一个含单个技能的注册表（目录 + SKILL.md）。
func skillRegistryWith(t *testing.T, name, desc string) *skills.Registry {
	t.Helper()
	dir := t.TempDir()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + desc + "\n---\n# 正文（不应泄露到清单）\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := skills.NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// TestWithSkillsInjectSystemPrompt 子 agent / 会话装配 WithSkills 后：
// composeSystemPrompt 含技能发现清单（仅元数据，不泄露正文）。
func TestWithSkillsInjectSystemPrompt(t *testing.T) {
	reg := skillRegistryWith(t, "code-review", "审查代码变更的正确性")
	p := &reqCaptureProvider{}
	loop := NewAgentLoop(
		WithProvider(p),
		WithSystemPrompt("你是助手"),
		WithSkills(reg),
	)
	_ = loop.Run(context.Background(), []core.Message{
		core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
	}, nil)

	if len(p.messages) == 0 || p.messages[0].Role != core.System {
		t.Fatalf("system prompt should be first: %+v", p.messages)
	}
	sp := p.messages[0].Content[0].Content
	if !strings.Contains(sp, "Available skills") || !strings.Contains(sp, "code-review") {
		t.Fatalf("系统提示词应含技能清单: %q", sp)
	}
	if strings.Contains(sp, "正文") {
		t.Fatalf("清单不应泄露技能正文: %q", sp)
	}
}

// TestWithSkillsNilNoManifest WithSkills(nil) 不注入清单（空注册表同样不注入）。
func TestWithSkillsNilNoManifest(t *testing.T) {
	p := &reqCaptureProvider{}
	loop := NewAgentLoop(
		WithProvider(p),
		WithSystemPrompt("你是助手"),
		WithSkills(nil),
	)
	_ = loop.Run(context.Background(), []core.Message{
		core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
	}, nil)
	if len(p.messages) == 0 {
		t.Fatal("no system message")
	}
	sp := p.messages[0].Content[0].Content
	if strings.Contains(sp, "Available skills") {
		t.Fatalf("nil 注册表不应注入技能清单: %q", sp)
	}
}

// TestBuildSkillManifestDisabled 禁用技能从清单消失（启用过滤一致）。
func TestBuildSkillManifestDisabled(t *testing.T) {
	reg := skillRegistryWith(t, "secret-skill", "内部技能")
	if err := reg.SetEnabled("secret-skill", false); err != nil {
		t.Fatal(err)
	}
	if s := buildSkillManifest(reg, nil); strings.Contains(s, "secret-skill") {
		t.Fatalf("禁用技能不应出现在清单: %q", s)
	}
}

// —— 2026-10：技能清单按**会话选中模型**过滤（Requires + Capabilities）——

// skillRegistryWithRequires 造一个带 requires 的技能注册表。
func skillRegistryWithRequires(t *testing.T, name, desc, requiresYAML string) *skills.Registry {
	t.Helper()
	dir := t.TempDir()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + desc + "\n" + requiresYAML + "---\n# 正文\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := skills.NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	return reg
}

// TestSkillManifestFilteredByModelCaps 同一个注册表，不同会话模型 → 不同清单。
// 这是「用 session 选中的模型来决定 skills 可见性」的核心断言。
func TestSkillManifestFilteredByModelCaps(t *testing.T) {
	reg := skillRegistryWithRequires(t, "screenshot-replica", "按截图复刻前端", "requires:\n  input: [image]\n")

	textOnly := skills.NewCapabilities("glm-4.6", []string{"text"}, []string{"read_file"})
	if s := buildSkillManifest(reg, textOnly); strings.Contains(s, "screenshot-replica") {
		t.Fatalf("纯文本模型的清单不应含 requires.image 技能: %q", s)
	}

	vision := skills.NewCapabilities("glm-5v-turbo", []string{"text", "image"}, []string{"read_file"})
	if s := buildSkillManifest(reg, vision); !strings.Contains(s, "screenshot-replica") {
		t.Fatalf("支持图片的模型清单应含该技能: %q", s)
	}
}

// TestSkillManifestNilCapsUnfiltered 未注入能力面（老宿主/CLI）= 不过滤，技能照常出现。
func TestSkillManifestNilCapsUnfiltered(t *testing.T) {
	reg := skillRegistryWithRequires(t, "vision-skill", "看图", "requires:\n  input: [image]\n")
	if s := buildSkillManifest(reg, nil); !strings.Contains(s, "vision-skill") {
		t.Fatalf("nil 能力面不应过滤清单: %q", s)
	}
}

// TestComposeSystemPromptUsesSkillCaps 端到端：loop 配置了 SkillCaps 后，
// composeSystemPrompt 的技能清单层按它过滤（装配漏接会在这里红）。
func TestComposeSystemPromptUsesSkillCaps(t *testing.T) {
	reg := skillRegistryWithRequires(t, "vision-skill", "看图", "requires:\n  input: [image]\n")
	loop := NewAgentLoop(
		WithSystemPrompt("你是助手"),
		WithSkills(reg),
		WithSkillCapabilities(skills.NewCapabilities("glm-4.6", []string{"text"}, nil)),
	)
	if sp := loop.composeSystemPrompt(); strings.Contains(sp, "vision-skill") {
		t.Fatalf("system prompt 不应含能力不足的技能: %q", sp)
	}
	loop2 := NewAgentLoop(
		WithSystemPrompt("你是助手"),
		WithSkills(reg),
		WithSkillCapabilities(skills.NewCapabilities("glm-5v-turbo", []string{"text", "image"}, nil)),
	)
	if sp := loop2.composeSystemPrompt(); !strings.Contains(sp, "vision-skill") {
		t.Fatalf("换模型后 system prompt 应含该技能: %q", sp)
	}
}
