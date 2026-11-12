package main

// 办公技能「内容更新」版本策略测试（2026-09-18 用户拍板 frontmatter `version` 的配套）。
//
// 策略（实现见 office_skill_sync.go 的 shouldUpdateSkill）：
//
//	内置无 version                → 不覆盖（不拿来源不明的内容盖用户副本）
//	装了但无 version（历史副本）   → 覆盖（否则老用户永远停在首装那天）
//	装了 < 内置                    → 覆盖
//	装了 >= 内置                   → 保留（用户把 version 改大即可钉住自己的副本）
//	symlink / 非目录               → 一律跳过
//
// 这些用例覆盖的正是「技能内容是代码、会带 bug 发布」那条教训的**分发端**：
// 修好了却发不出去 = 没修。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkOfficeSkillVer 造带 frontmatter version 的最小技能（version 传空串 = 不写该字段，
// 用于模拟历史副本/无版本内置技能）。
func mkOfficeSkillVer(t *testing.T, root, name, version string) {
	t.Helper()
	dir := filepath.Join(root, name)
	mkOfficeSkillAt(t, dir, name)
	if version == "" {
		return
	}
	md := "---\nname: " + name + "\nversion: " + version + "\ndescription: test\n---\n\nbody of " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeInstalledSkill 在假 HOME 的全局技能层预置一份「用户副本」。
func writeInstalledSkill(t *testing.T, home, name, version, body string) string {
	t.Helper()
	dir := filepath.Join(home, ".agents", "skills", name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: " + name + "\n"
	if version != "" {
		md += "version: " + version + "\n"
	}
	md += "description: 用户副本\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

// readSkillMD 读目标技能内容。
func readSkillMD(t *testing.T, dir string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "SKILL.md"))
	if err != nil {
		t.Fatalf("读 %s/SKILL.md: %v", dir, err)
	}
	return string(b)
}

func TestEnsureOfficeSkillsSyncedVersionPolicyUpdatesOlder(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(t.TempDir(), "office-skills")
	mkOfficeSkillVer(t, src, "docx", "1.2.0")
	dst := writeInstalledSkill(t, home, "docx", "0.9.0", "用户改过的旧版")

	t.Setenv("HOME", home)
	t.Setenv(officeSkillsDirEnv, src)
	if err := ensureOfficeSkillsSynced(); err != nil {
		t.Fatalf("sync err: %v", err)
	}
	if got, want := readSkillMD(t, dst), readSkillMD(t, filepath.Join(src, "docx")); got != want {
		t.Fatalf("装了更旧版本应被覆盖：\ngot  %q\nwant %q", got, want)
	}
}

func TestEnsureOfficeSkillsSyncedVersionPolicyUpdatesLegacyWithoutVersion(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(t.TempDir(), "office-skills")
	mkOfficeSkillVer(t, src, "docx", "1.2.0")
	dst := writeInstalledSkill(t, home, "docx", "", "首装那天的老副本")

	t.Setenv("HOME", home)
	t.Setenv(officeSkillsDirEnv, src)
	if err := ensureOfficeSkillsSynced(); err != nil {
		t.Fatalf("sync err: %v", err)
	}
	if got, want := readSkillMD(t, dst), readSkillMD(t, filepath.Join(src, "docx")); got != want {
		t.Fatalf("历史副本（无 version）应被内置版修好：\ngot  %q\nwant %q", got, want)
	}
}

func TestEnsureOfficeSkillsSyncedVersionPolicyKeepsSameVersionUserEdits(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(t.TempDir(), "office-skills")
	mkOfficeSkillVer(t, src, "docx", "1.2.0")
	dst := writeInstalledSkill(t, home, "docx", "1.2.0", "同版本下用户自己改的内容")

	t.Setenv("HOME", home)
	t.Setenv(officeSkillsDirEnv, src)
	if err := ensureOfficeSkillsSynced(); err != nil {
		t.Fatalf("sync err: %v", err)
	}
	if got := readSkillMD(t, dst); !strings.Contains(got, "同版本下用户自己改的内容") {
		t.Fatalf("同版本时用户改动应保留，got %q", got)
	}
}

func TestEnsureOfficeSkillsSyncedVersionPolicyKeepsNewerUserVersion(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(t.TempDir(), "office-skills")
	mkOfficeSkillVer(t, src, "docx", "1.2.0")
	dst := writeInstalledSkill(t, home, "docx", "2.0.0", "用户自管版本（钉住自己的副本）")

	t.Setenv("HOME", home)
	t.Setenv(officeSkillsDirEnv, src)
	if err := ensureOfficeSkillsSynced(); err != nil {
		t.Fatalf("sync err: %v", err)
	}
	if got := readSkillMD(t, dst); !strings.Contains(got, "用户自管版本") {
		t.Fatalf("用户副本 version 更高时应保留，got %q", got)
	}
}

func TestEnsureOfficeSkillsSyncedVersionPolicyRepairsBrokenInstalled(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(t.TempDir(), "office-skills")
	mkOfficeSkillVer(t, src, "docx", "1.2.0")
	// 坏副本：frontmatter 缺 name/description（ParseSkill 会失败 → 注册表根本加载不了它）
	dst := filepath.Join(home, ".agents", "skills", "docx")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dst, "SKILL.md"), []byte("这不是 frontmatter\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv(officeSkillsDirEnv, src)
	if err := ensureOfficeSkillsSynced(); err != nil {
		t.Fatalf("sync err: %v", err)
	}
	if got, want := readSkillMD(t, dst), readSkillMD(t, filepath.Join(src, "docx")); got != want {
		t.Fatalf("坏副本应被内置版修好：\ngot  %q\nwant %q", got, want)
	}
}

func TestEnsureOfficeSkillsSyncedVersionPolicySkipsUnversionedBundled(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(t.TempDir(), "office-skills")
	mkOfficeSkillVer(t, src, "docx", "") // 内置无 version
	dst := writeInstalledSkill(t, home, "docx", "", "用户副本")

	t.Setenv("HOME", home)
	t.Setenv(officeSkillsDirEnv, src)
	if err := ensureOfficeSkillsSynced(); err != nil {
		t.Fatalf("sync err: %v", err)
	}
	if got := readSkillMD(t, dst); !strings.Contains(got, "用户副本") {
		t.Fatalf("内置无 version 时不得覆盖用户副本，got %q", got)
	}
}

func TestCompareSkillVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
		ok   bool
	}{
		{"1.0.0", "1.0.0", 0, true},
		{"0.9.0", "1.0.0", -1, true},
		{"1.0.1", "1.0.0", 1, true},
		{"1.2", "1.2.0", 0, true},    // 缺省段按 0
		{"2", "1.9.9", 1, true},      // 单段也成立
		{"1.10.0", "1.9.0", 1, true}, // 数字比较而非字典序
		{"", "1.0.0", 0, false},
		{"1.0.0", "", 0, false},
		{"v1.0.0", "1.0.0", 0, false}, // 前缀字母不可解析（不猜）
		{"1.0.0-beta", "1.0.0", 0, false},
	}
	for _, c := range cases {
		got, ok := compareSkillVersions(c.a, c.b)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("compareSkillVersions(%q,%q) = (%d,%v)，期望 (%d,%v)", c.a, c.b, got, ok, c.want, c.ok)
		}
	}
}
