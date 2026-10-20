package skills

// capability_test.go：按会话能力面过滤技能（2026-10）。
//
// 守的是三条不变式（任何一条破了都会造成"静默失败"，比报错更难查）：
//  1. 能力面过滤 = 清单与加载**同源**（ListFor 里没有的技能，LoadFor 必须拒绝）；
//  2. nil 能力面 = 不过滤（向后兼容：老宿主/CLI/测试不知道能力这回事）；
//  3. requires 写坏/未知模态 → 保守判定为不满足，但**不**让技能解析失败
//     （整条技能消失是最难排查的失败形态）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// skillWithRequires 造一个带 requires frontmatter 的技能。
func skillWithRequires(t *testing.T, dir, name, requiresYAML string) {
	t.Helper()
	sd := filepath.Join(dir, name)
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: 测试技能 " + name + "\n" + requiresYAML + "---\n\n# 正文\n指令\n"
	if err := os.WriteFile(filepath.Join(sd, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestParseRequiresFrontmatter requires 的两种写法（块列表 / 空格分隔单值）都能解析。
func TestParseRequiresFrontmatter(t *testing.T) {
	dir := t.TempDir()
	skillWithRequires(t, dir, "vision", "requires:\n  input: [image]\n  tools: [run_python]\n")
	skillWithRequires(t, dir, "plain", "")
	skillWithRequires(t, dir, "inline", "requires:\n  input: image\n")

	reg, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	v, err := reg.Skill("vision")
	if err != nil {
		t.Fatal(err)
	}
	if len(v.Requires.Input) != 1 || v.Requires.Input[0] != "image" {
		t.Errorf("input = %v, want [image]", v.Requires.Input)
	}
	if len(v.Requires.Tools) != 1 || v.Requires.Tools[0] != "run_python" {
		t.Errorf("tools = %v, want [run_python]", v.Requires.Tools)
	}
	p, _ := reg.Skill("plain")
	if !p.Requires.Empty() {
		t.Errorf("无 requires 应为空: %+v", p.Requires)
	}
	in, _ := reg.Skill("inline")
	if len(in.Requires.Input) != 1 || in.Requires.Input[0] != "image" {
		t.Errorf("单值写法应归一为 [image]: %v", in.Requires.Input)
	}
}

// TestListForFiltersByModelInput 纯文本模型看不到 requires.image 的技能；
// 支持图片的模型能看到。**不**影响无 requires 的技能。
func TestListForFiltersByModelInput(t *testing.T) {
	dir := t.TempDir()
	skillWithRequires(t, dir, "vision", "requires:\n  input: [image]\n")
	skillWithRequires(t, dir, "text-only", "")
	reg, _ := NewRegistry(dir)

	textCaps := NewCapabilities("glm-4.6", []string{"text"}, nil)
	names := descriptorNames(reg.ListFor(textCaps))
	if names["vision"] {
		t.Fatal("纯文本模型不应看到 requires.image 的技能")
	}
	if !names["text-only"] {
		t.Fatal("无 requires 的技能必须仍然可见")
	}

	visionCaps := NewCapabilities("glm-5v-turbo", []string{"text", "image"}, nil)
	if !descriptorNames(reg.ListFor(visionCaps))["vision"] {
		t.Fatal("支持图片的模型应看到 requires.image 的技能")
	}
}

// TestListForFiltersByTool Work persona（无 bash）看不到 requires.bash 的技能。
func TestListForFiltersByTool(t *testing.T) {
	dir := t.TempDir()
	skillWithRequires(t, dir, "needs-bash", "requires:\n  tools: [bash]\n")
	skillWithRequires(t, dir, "needs-python", "requires:\n  tools: [run_python]\n")
	reg, _ := NewRegistry(dir)

	workCaps := NewCapabilities("glm-4.6", []string{"text"}, []string{"read_file", "write_file", "run_python"})
	names := descriptorNames(reg.ListFor(workCaps))
	if names["needs-bash"] {
		t.Fatal("无 bash 的会话不应看到 requires.bash 的技能")
	}
	if !names["needs-python"] {
		t.Fatal("有 run_python 的会话应看到对应技能")
	}
}

// TestNilCapabilitiesDoesNotFilter nil = 不过滤（老宿主兼容），且 List/Load 与之等价。
func TestNilCapabilitiesDoesNotFilter(t *testing.T) {
	dir := t.TempDir()
	skillWithRequires(t, dir, "vision", "requires:\n  input: [image]\n")
	reg, _ := NewRegistry(dir)

	if !descriptorNames(reg.ListFor(nil))["vision"] {
		t.Fatal("nil 能力面不应过滤")
	}
	if !descriptorNames(reg.List())["vision"] {
		t.Fatal("List 必须等价 ListFor(nil)")
	}
	if _, err := reg.LoadFor("vision", nil); err != nil {
		t.Fatalf("nil 能力面应允许加载: %v", err)
	}
}

// TestManifestAndLoadAgree 核心不变式：清单里没有的，加载必须拒绝；反之必须放行。
// 这条对应「模型看到清单 → load_skill」的真实链路，漂移的表现是"提示词里没有但工具能加载"。
func TestManifestAndLoadAgree(t *testing.T) {
	dir := t.TempDir()
	skillWithRequires(t, dir, "vision", "requires:\n  input: [image]\n")
	skillWithRequires(t, dir, "text-only", "")
	reg, _ := NewRegistry(dir)

	caps := NewCapabilities("glm-4.6", []string{"text"}, nil)
	for _, d := range reg.AllFor(caps) {
		_, err := reg.LoadFor(d.Name, caps)
		if d.Enabled && err != nil {
			t.Fatalf("清单里可见（%s）但加载失败: %v", d.Name, err)
		}
		if !d.Enabled && d.Name == "vision" {
			// 能力不足 → Enabled=false（宿主 UI 标灰）；加载必须拒绝且说明原因
			if err == nil {
				t.Fatal("能力不足的技能必须拒绝加载")
			}
			if !strings.Contains(err.Error(), "requires model input: image") {
				t.Fatalf("拒绝原因应指明缺什么: %v", err)
			}
			if !strings.Contains(err.Error(), "glm-4.6") {
				t.Fatalf("拒绝原因应带当前模型（用户才知道该换什么）: %v", err)
			}
		}
	}
}

// TestLoadSkillToolRejectsUnavailable load_skill 工具必须与清单同源拒绝。
func TestLoadSkillToolRejectsUnavailable(t *testing.T) {
	dir := t.TempDir()
	skillWithRequires(t, dir, "vision", "requires:\n  input: [image]\n")
	reg, _ := NewRegistry(dir)

	tool := NewLoadSkillTool(reg).WithCapabilities(NewCapabilities("glm-4.6", []string{"text"}, nil))
	if _, err := tool.Call(nil, "", `{"name":"vision"}`); err == nil {
		t.Fatal("load_skill 应拒绝能力不足的技能")
	}

	ok := NewLoadSkillTool(reg).WithCapabilities(NewCapabilities("glm-5v-turbo", []string{"text", "image"}, nil))
	if _, err := ok.Call(nil, "", `{"name":"vision"}`); err != nil {
		t.Fatalf("能力满足时应加载成功: %v", err)
	}
}

// TestBrokenRequiresDoesNotDropSkill requires 写错不能让技能消失（解析宽容）。
func TestBrokenRequiresDoesNotDropSkill(t *testing.T) {
	dir := t.TempDir()
	skillWithRequires(t, dir, "weird", "requires: not-a-map\n")
	reg, err := NewRegistry(dir)
	if err != nil {
		t.Fatalf("坏 requires 不应让注册表失败: %v", err)
	}
	if _, err := reg.Skill("weird"); err != nil {
		t.Fatalf("坏 requires 不应让技能消失: %v", err)
	}
	// 无需求 → 任何能力面都可用
	if !descriptorNames(reg.ListFor(NewCapabilities("x", []string{"text"}, nil)))["weird"] {
		t.Fatal("requires 读不出来应视为无需求（任何模型可用）")
	}
}

// TestUnknownInputIsUnsatisfied 未知模态名保守判为不满足（避免"以为能看图"）。
func TestUnknownInputIsUnsatisfied(t *testing.T) {
	dir := t.TempDir()
	skillWithRequires(t, dir, "hologram", "requires:\n  input: [hologram]\n")
	reg, _ := NewRegistry(dir)
	if descriptorNames(reg.ListFor(NewCapabilities("m", []string{"text", "image"}, nil)))["hologram"] {
		t.Fatal("未知模态必须判为不满足")
	}
}

// TestDisabledStillWins 禁用优先于能力：即使能力满足，禁用技能仍不可见/不可加载。
func TestDisabledStillWins(t *testing.T) {
	dir := t.TempDir()
	skillWithRequires(t, dir, "vision", "requires:\n  input: [image]\n")
	reg, _ := NewRegistry(dir)
	if err := reg.SetEnabled("vision", false); err != nil {
		t.Fatal(err)
	}
	caps := NewCapabilities("glm-5v-turbo", []string{"text", "image"}, nil)
	if descriptorNames(reg.ListFor(caps))["vision"] {
		t.Fatal("禁用技能不应出现")
	}
	if _, err := reg.LoadFor("vision", caps); err == nil || !strings.Contains(err.Error(), "disabled") {
		t.Fatalf("禁用应报 disabled（与能力不足区分开）: %v", err)
	}
}

// descriptorNames 名字集合（断言用）。
func descriptorNames(list []Descriptor) map[string]bool {
	out := make(map[string]bool, len(list))
	for _, d := range list {
		out[d.Name] = true
	}
	return out
}

// TestSkillDetailResourcesNeverNil 技能详情给宿主的资源清单**恒为数组**，绝不是 null。
//
// 2026-10 真机事故：skills/resources 为 nil 切片时 JSON 序列化成 `null`，前端
// `skillDetail.resources.length` 直接 TypeError，整个技能面板白屏。trigger 条件很窄
// —— 只有「没有 scripts/references/assets 目录」的技能会踩到（visual-ui 等三个
// 随包视觉技能恰好都没有），所以办公四件套的测试是全绿的也发现不了。
func TestSkillDetailResourcesNeverNil(t *testing.T) {
	dir := t.TempDir()
	// 无任何资源子目录的技能
	skillWithRequires(t, dir, "no-assets", "")
	reg, err := NewRegistry(dir)
	if err != nil {
		t.Fatal(err)
	}
	s, err := reg.Skill("no-assets")
	if err != nil {
		t.Fatal(err)
	}
	if s.Resources == nil {
		t.Fatal("Resources 是 nil 切片（JSON 会序列化成 null，前端 .length 直接崩）—— 必须给空数组")
	}
	if len(s.Resources) != 0 {
		t.Fatalf("无资源技能应给空数组: %v", s.Resources)
	}
}
