package main

// visual_skill_references_test.go：visual-ui 渐进披露（progressive disclosure）的结构守卫（2026-10）。
//
// 背景：visual-ui 的 12 个组件骨架起初全写在 SKILL.md 里，load_skill 一次注入 42.5 KB ——
// 一次「画个 KPI 卡片」要背上 12 个用不到的完整 HTML。改成「规则留 SKILL.md、骨架进
// references/」后注入降到 ~15 KB。但这引入了一类**静默失效**：
//
//	SKILL.md 的引用表写了一个 references/*.md 路径，而文件其实不存在 / 名字拼错 /
//	重构时改名没同步。模型照着引用去 read_file → 读到 "file not found" →
//	它不会报错给你，只会**退回去凭记忆写**，产出看起来正常但没走我们的约束。
//
// 所以：引用表里出现的每个路径必须在磁盘上真实存在，且非空；反过来 references/ 里
// 每个文件都必须被引用表提到（否则是永远读不到的死文件，白占包体）。
//
// 另一条：reference 文件必须能独立读懂。它们被**单独**读进上下文时，SKILL.md 里的
// 「见上方 Layout system」这类指代是断的 —— 故禁止 reference 里出现「见 SKILL.md」。

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// 引用表里形如 `references/xxx.md` 的路径（反引号包裹，代码块外）。
var skillReferencePathRe = regexp.MustCompile("`(references/[A-Za-z0-9._-]+\\.md)`")

// TestVisualUISkillReferencesExist 引用表 → 磁盘：每个路径都存在、非空。
func TestVisualUISkillReferencesExist(t *testing.T) {
	skillDir := filepath.Join(visualSkillsSourceDir, "visual-ui")
	raw, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("读取 visual-ui/SKILL.md 失败: %v", err)
	}
	refs := skillReferencePathRe.FindAllStringSubmatch(string(raw), -1)
	if len(refs) == 0 {
		t.Fatal("visual-ui/SKILL.md 里没有任何 `references/*.md` 引用——" +
			"渐进披露的骨架文件必须被显式引用，否则模型永远读不到它们")
	}

	seen := map[string]bool{}
	for _, m := range refs {
		rel := m[1]
		if seen[rel] {
			continue // 同一路径在表格/正文里出现多次是正常的
		}
		seen[rel] = true

		abs := filepath.Join(skillDir, filepath.FromSlash(rel))
		fi, err := os.Stat(abs)
		if err != nil {
			t.Errorf("SKILL.md 引用了不存在的 %s —— 模型会 read_file 失败、"+
				"然后凭记忆瞎写（静默失效，最坑）: %v", rel, err)
			continue
		}
		if fi.Size() == 0 {
			t.Errorf("引用的 %s 是空文件——比不存在更糟：模型读到空内容会以为「没有约束」", rel)
			continue
		}
		// 反向检查：注册表要能把它列进 Resources（模型据此拿到绝对路径）。
		body, err := os.ReadFile(abs)
		if err != nil {
			t.Errorf("读取 %s 失败: %v", rel, err)
			continue
		}
		if !strings.Contains(string(body), "```html") {
			t.Errorf("%s 里没有任何 ```html 骨架——reference 文件的意义就是放可直接复制的完整片段", rel)
		}
	}
}

// TestVisualUIReferencesAllReferenced references/ 下的每个文件都必须被 SKILL.md 提到。
// 否则它是死文件：占分发包体积、可能藏着过期内容，而模型永远读不到。
func TestVisualUIReferencesAllReferenced(t *testing.T) {
	skillDir := filepath.Join(visualSkillsSourceDir, "visual-ui")
	raw, err := os.ReadFile(filepath.Join(skillDir, "SKILL.md"))
	if err != nil {
		t.Fatalf("读取 visual-ui/SKILL.md 失败: %v", err)
	}
	skillMD := string(raw)

	entries, err := os.ReadDir(filepath.Join(skillDir, "references"))
	if err != nil {
		t.Fatalf("visual-ui/references 目录不可读（渐进披露的骨架应放这里）: %v", err)
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue // 非 md 资源不在此守卫范围
		}
		rel := "references/" + e.Name()
		if !strings.Contains(skillMD, rel) {
			t.Errorf("%s 没有被 SKILL.md 引用——模型永远读不到这个文件，"+
				"要么在引用表里登记它，要么删掉", rel)
		}
	}
}

// TestVisualUIReferencesStandalone references 被单独读进上下文，禁止跨文件指代。
// 「见上方 SKILL.md 的 Layout system」在只读了 reference 的上下文里是断的。
func TestVisualUIReferencesStandalone(t *testing.T) {
	dir := filepath.Join(visualSkillsSourceDir, "visual-ui", "references")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("references 目录不可读: %v", err)
	}
	// 只禁**指向 SKILL.md 的**指代；reference 内部的「见上/下方骨架」是自洽的。
	banned := []string{
		"见 SKILL.md",
		"see SKILL.md",
		"as described in SKILL.md",
		"above in SKILL.md",
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		body, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			t.Errorf("读取 %s 失败: %v", e.Name(), err)
			continue
		}
		lower := strings.ToLower(string(body))
		for _, b := range banned {
			if strings.Contains(lower, strings.ToLower(b)) {
				t.Errorf("%s 引用了 SKILL.md（%q）——它会被**单独**读进上下文，"+
					"跨文件指代在那里是断的；把必要内容就地写清", e.Name(), b)
			}
		}
	}
}
