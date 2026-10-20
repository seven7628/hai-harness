package main

// visual_skill_content_test.go：随包视觉/UI 技能的内容与分发守卫（2026-10）。
//
// 守三件事：
//  1. 目录集合**恰好**是声明的三个（和 office_skill_content_test.go 同一范式：
//     多一个 = 新增没登记，少一个 = 误删/漏同步）；
//  2. 每个技能能被真实注册表解析、能按 frontmatter `requires` 被**能力面**正确过滤
//     —— 这是「用会话选中模型决定 skills 可见性」在随包内容上的落地断言；
//  3. 内容与分发一致：ensureVisualSkillsSynced() → ~/.agents/skills → 可加载。
//
// 为什么单独一个文件而不是塞进 office 那套：办公技能的内容规则（`## How to run this`
// 必须提 run_python、已装/未装模块清单）对视觉技能不成立 —— 它们大多不跑 Python。
// 混在一起会让后来者以为视觉技能也要遵守 run_python 的契约。

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/skills"
)

// visualSkillsSourceDir 随包视觉技能的源目录（相对 desktop/bridge：go test 的 cwd 是包目录）。
const visualSkillsSourceDir = "../vendor/visual-skills"

// wantVisualSkills 期望随包分发的视觉技能集合：**恰好**这三个。
//
// 为什么是这三个（而不是照搬智谱 GLM-skills 的 17 个）：本仓的实现口径是
// 「会话选中的模型自己看图、自己生成」——技能只提供流程与约束，不调外部视觉 API。
// 那 17 个技能里绝大多数是「脚本调 GLM-4V/GLM-OCR 接口」，与本口径相反
// （模型退化成调脚本的人，且需要额外 ZHIPU key + requests 依赖），故没有随包分发。
var wantVisualSkills = []string{"screenshot-to-ui", "visual-review", "visual-ui"}

// visualSkillMinDescriptionLen description 长度下限：发现阶段只注入 name/description，
// 它是模型判断「何时该用」的唯一依据。与办公技能同阈值（80）。
const visualSkillMinDescriptionLen = 80

// TestVisualSkillDirsExactlyMatch 随包视觉技能目录必须恰好是声明的三个。
func TestVisualSkillDirsExactlyMatch(t *testing.T) {
	entries, err := os.ReadDir(visualSkillsSourceDir)
	if err != nil {
		t.Fatalf("随包视觉技能源目录 %s 不可读: %v", visualSkillsSourceDir, err)
	}
	var got []string
	for _, e := range entries {
		fi, err := os.Stat(filepath.Join(visualSkillsSourceDir, e.Name()))
		if err != nil || !fi.IsDir() {
			continue // README 之类非技能条目
		}
		got = append(got, e.Name())
	}
	slices.Sort(got)
	want := slices.Clone(wantVisualSkills)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("随包视觉技能目录不匹配（%s）:\n got  %v\n want %v\n"+
			"少一个 = 用户侧少一个技能；多一个 = 需要同步更新本测试的 wantVisualSkills",
			visualSkillsSourceDir, got, want)
	}
}

// TestVisualSkillContentLoadable 每个技能能被注册表解析、description 足够长、
// frontmatter 有 version（随包覆盖策略依赖它）。
func TestVisualSkillContentLoadable(t *testing.T) {
	reg, err := skills.NewRegistry(visualSkillsSourceDir)
	if err != nil {
		t.Fatalf("加载随包视觉技能目录 %s 失败: %v", visualSkillsSourceDir, err)
	}
	for _, name := range wantVisualSkills {
		s, err := reg.Skill(name)
		if err != nil {
			t.Errorf("技能 %q 无法从随包目录加载: %v", name, err)
			continue
		}
		if n := len([]rune(s.Description)); n <= visualSkillMinDescriptionLen {
			t.Errorf("技能 %s 的 description 只有 %d 字（< %d）——它是模型判断何时使用的唯一依据:\n%q",
				name, n, visualSkillMinDescriptionLen, s.Description)
		}
		// version 是随包覆盖策略的前提：无 version 的内置技能**永不覆盖**已装用户
		// （office_skill_sync.go 的 shouldUpdateSkill），改了内容也发不出去。
		if strings.TrimSpace(s.Version) == "" {
			t.Errorf("技能 %s 缺 frontmatter version —— 随包技能必须带 version，"+
				"否则内容更新永远到不了已安装用户", name)
		}
		if strings.TrimSpace(s.Instructions) == "" {
			t.Errorf("技能 %s 正文为空", name)
		}
	}
}

// TestVisualSkillRequirementsGateByModel 能力面过滤的核心内容断言：
// 读图的两个技能必须声明 requires.input: [image]（纯文本模型下不可见），
// 而 visual-ui（只写 HTML，不需要看图）必须**不**声明 —— 声明了会把纯文本模型
// 上唯一可用的视觉能力也挡掉。
func TestVisualSkillRequirementsGateByModel(t *testing.T) {
	reg, err := skills.NewRegistry(visualSkillsSourceDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"screenshot-to-ui", "visual-review"} {
		s, err := reg.Skill(name)
		if err != nil {
			t.Fatalf("技能 %s: %v", name, err)
		}
		if !slices.Contains(s.Requires.Input, "image") {
			t.Errorf("技能 %s 要读截图，必须声明 requires.input: [image]，"+
				"否则纯文本模型会看到它、加载一大段流程、最后才发现自己看不见图", name)
		}
	}
	s, err := reg.Skill("visual-ui")
	if err != nil {
		t.Fatal(err)
	}
	if !s.Requires.Empty() {
		t.Errorf("visual-ui 只产出 HTML、不读图，不应有 requires（否则纯文本模型上"+
			"唯一可用的视觉能力也会被挡掉）: %+v", s.Requires)
	}

	// 端到端：同一注册表，两种模型 → 可见集合不同。
	textCaps := skills.NewCapabilities("glm-4.6", []string{"text"}, nil)
	visionCaps := skills.NewCapabilities("glm-5v-turbo", []string{"text", "image"}, nil)
	if hasSkill(reg.ListFor(textCaps), "screenshot-to-ui") {
		t.Error("纯文本模型不应看到 screenshot-to-ui")
	}
	if !hasSkill(reg.ListFor(textCaps), "visual-ui") {
		t.Error("纯文本模型必须仍能看到 visual-ui（不需要看图）")
	}
	for _, n := range wantVisualSkills {
		if !hasSkill(reg.ListFor(visionCaps), n) {
			t.Errorf("支持图片的模型应看到全部视觉技能，缺 %s", n)
		}
	}
}

// TestVisualSkillSyncsToGlobalLayer 走真实分发链路：ensureVisualSkillsSynced() →
// ~/.agents/skills/<name> → 可加载。这同时证明「新增技能只加子目录，Go/TS 接线不动」。
func TestVisualSkillSyncsToGlobalLayer(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(visualSkillsDirEnv, visualSkillsSourceDir)

	if err := ensureVisualSkillsSynced(); err != nil {
		t.Fatalf("同步随包视觉技能到全局层失败: %v", err)
	}
	globalDir := filepath.Join(home, ".agents", "skills")
	for _, name := range wantVisualSkills {
		dst := filepath.Join(globalDir, name, "SKILL.md")
		got, err := os.ReadFile(dst)
		if err != nil {
			t.Errorf("技能 %s 未同步到全局层（%s）: %v", name, dst, err)
			continue
		}
		want, err := os.ReadFile(filepath.Join(visualSkillsSourceDir, name, "SKILL.md"))
		if err != nil {
			t.Fatalf("随包技能 %s 不可读: %v", name, err)
		}
		if !bytes.Equal(got, want) {
			t.Errorf("全局层的 %s/SKILL.md 与随包源不一致——同步必须是逐字复制", name)
		}
	}
	// 全局层注册表：打包版用户的真实加载路径。requires 也必须一路带过去
	// （frontmatter 解析在 skills 包里，同步只复制文件，这里确认没有丢字段）。
	reg, err := skills.NewRegistry(globalDir)
	if err != nil {
		t.Fatalf("加载全局层技能目录失败: %v", err)
	}
	visionCaps := skills.NewCapabilities("glm-5v-turbo", []string{"text", "image"}, nil)
	for _, name := range wantVisualSkills {
		s, err := reg.LoadFor(name, visionCaps)
		if err != nil {
			t.Errorf("全局层技能 %q 在支持图片的模型下应可加载: %v", name, err)
			continue
		}
		if name != "visual-ui" && len(s.Requires.Input) == 0 {
			t.Errorf("全局层技能 %s 丢了 requires（同步/解析链路丢字段）", name)
		}
	}
}

// hasSkill 列表里是否含该名字。
func hasSkill(list []skills.Descriptor, name string) bool {
	for _, d := range list {
		if d.Name == name {
			return true
		}
	}
	return false
}
