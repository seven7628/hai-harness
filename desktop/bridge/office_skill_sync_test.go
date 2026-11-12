package main

// 办公技能随包分发同步测试（2026-09）：
//   - 三个技能全新 → 三个都复制（含子目录递归）
//   - 目标已存在（真实目录）→ 按 frontmatter `version` 决策（见文件头「内容更新机制」）：
//     内置无 version → 不覆盖；装了但无 version（历史副本）→ 覆盖；装了 < 内置 → 覆盖；
//     装了 >= 内置 → 保留（用户把 version 改大即可钉住自己的副本）
//   - 目标已存在（symlink）→ 跳过（尊重用户/外部来源）
//   - 源里某技能缺 SKILL.md → 该技能跳过、其余成功、返回错误
//   - 源里有非目录条目（README.md）→ 忽略不报错
//   - 无 env（dev 无注入）→ no-op

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestEnsureOfficeSkillsSyncedCopiesAll 三个技能全新 → 全部复制，子目录一并递归。
func TestEnsureOfficeSkillsSyncedCopiesAll(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(t.TempDir(), "office-skills")
	for _, name := range []string{"docx", "xlsx", "pdf"} {
		mkOfficeSkill(t, src, name)
	}

	t.Setenv("HOME", home)
	t.Setenv(officeSkillsDirEnv, src)

	if err := ensureOfficeSkillsSynced(); err != nil {
		t.Fatalf("sync err: %v", err)
	}
	for _, name := range []string{"docx", "xlsx", "pdf"} {
		dst := filepath.Join(home, ".agents", "skills", name)
		got, err := os.ReadFile(filepath.Join(dst, "SKILL.md"))
		if err != nil {
			t.Fatalf("%s/SKILL.md 未复制到 %s: %v", name, dst, err)
		}
		want, _ := os.ReadFile(filepath.Join(src, name, "SKILL.md"))
		if string(got) != string(want) {
			t.Fatalf("%s/SKILL.md 内容不一致:\ngot  %q\nwant %q", name, got, want)
		}
		// 子目录递归：scripts/helper.py
		if _, err := os.Stat(filepath.Join(dst, "scripts", "helper.py")); err != nil {
			t.Fatalf("%s/scripts/helper.py 未复制（递归复制失败）: %v", name, err)
		}
	}
}

// TestEnsureOfficeSkillsSyncedSkipsExistingDir 目标已存在（真实目录，用户改过）→ 跳过不覆盖。
//
// 注：本条现在走的是「**内置技能无 version** → 一律不覆盖用户副本」这条分支（mkOfficeSkill
// 造的源技能没有 version）。版本策略的其余分支（更旧→覆盖 / 相同或更新→保留 / 无版本→修复）
// 见 TestEnsureOfficeSkillsSyncedVersionPolicy* 系列。
func TestEnsureOfficeSkillsSyncedSkipsExistingDir(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(t.TempDir(), "office-skills")
	mkOfficeSkill(t, src, "docx")

	// 预置用户版本：内容与源不同，同步后必须原样保留
	dst := filepath.Join(home, ".agents", "skills", "docx")
	if err := os.MkdirAll(dst, 0o755); err != nil {
		t.Fatal(err)
	}
	userMD := "---\nname: docx\n---\n\n用户自己改过的版本\n"
	if err := os.WriteFile(filepath.Join(dst, "SKILL.md"), []byte(userMD), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv(officeSkillsDirEnv, src)
	if err := ensureOfficeSkillsSynced(); err != nil {
		t.Fatalf("sync err: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dst, "SKILL.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != userMD {
		t.Fatalf("已存在的技能被覆盖了:\ngot  %q\nwant %q", got, userMD)
	}
	if _, err := os.Stat(filepath.Join(dst, "scripts", "helper.py")); err == nil {
		t.Fatal("已存在的技能不应被补写文件")
	}
}

// TestEnsureOfficeSkillsSyncedSkipsExistingSymlink 目标已存在（symlink）→ 跳过、symlink 保持。
func TestEnsureOfficeSkillsSyncedSkipsExistingSymlink(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(t.TempDir(), "office-skills")
	mkOfficeSkill(t, src, "pdf")

	linkLayer := filepath.Join(home, ".agents", "skills")
	if err := os.MkdirAll(linkLayer, 0o755); err != nil {
		t.Fatal(err)
	}
	realSkill := filepath.Join(t.TempDir(), "real-pdf-skill")
	mkOfficeSkillAt(t, realSkill, "pdf")
	if err := os.Symlink(realSkill, filepath.Join(linkLayer, "pdf")); err != nil {
		t.Skipf("symlink 失败: %v", err)
	}

	t.Setenv("HOME", home)
	t.Setenv(officeSkillsDirEnv, src)
	if err := ensureOfficeSkillsSynced(); err != nil {
		t.Fatalf("sync err: %v", err)
	}
	fi, err := os.Lstat(filepath.Join(linkLayer, "pdf"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("已存在的 symlink 不应被覆盖为真实目录")
	}
}

// TestEnsureOfficeSkillsSyncedBadSkillDoesNotBlockOthers 某技能缺 SKILL.md →
// 该技能跳过、其余照常复制、返回聚合错误（一个坏技能不拖累其他）。
func TestEnsureOfficeSkillsSyncedBadSkillDoesNotBlockOthers(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(t.TempDir(), "office-skills")
	mkOfficeSkill(t, src, "docx")
	mkOfficeSkill(t, src, "xlsx")
	// 坏技能：目录存在但缺 SKILL.md
	if err := os.MkdirAll(filepath.Join(src, "pdf"), 0o755); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv(officeSkillsDirEnv, src)
	err := ensureOfficeSkillsSynced()
	if err == nil {
		t.Fatal("缺 SKILL.md 应返回错误")
	}
	if !strings.Contains(err.Error(), "pdf") {
		t.Fatalf("错误应点名坏技能 pdf: %v", err)
	}
	skillsRoot := filepath.Join(home, ".agents", "skills")
	for _, name := range []string{"docx", "xlsx"} {
		if _, err := os.Stat(filepath.Join(skillsRoot, name, "SKILL.md")); err != nil {
			t.Fatalf("好技能 %s 应照常复制: %v", name, err)
		}
	}
	if _, err := os.Lstat(filepath.Join(skillsRoot, "pdf")); err == nil {
		t.Fatal("坏技能不应留下目录")
	}
}

// TestEnsureOfficeSkillsSyncedIgnoresNonDir 源里的非目录条目（README.md）被忽略。
func TestEnsureOfficeSkillsSyncedIgnoresNonDir(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(t.TempDir(), "office-skills")
	mkOfficeSkill(t, src, "docx")
	if err := os.WriteFile(filepath.Join(src, "README.md"), []byte("# 办公技能\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	t.Setenv("HOME", home)
	t.Setenv(officeSkillsDirEnv, src)
	if err := ensureOfficeSkillsSynced(); err != nil {
		t.Fatalf("非目录条目不应报错: %v", err)
	}
	skillsRoot := filepath.Join(home, ".agents", "skills")
	if _, err := os.Stat(filepath.Join(skillsRoot, "docx", "SKILL.md")); err != nil {
		t.Fatalf("docx 应被复制: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(skillsRoot, "README.md")); err == nil {
		t.Fatal("非目录条目不应被同步")
	}
}

// TestEnsureOfficeSkillsSyncedNoEnv 无 env → no-op（dev 场景技能由工作区 .agents/skills 提供）。
func TestEnsureOfficeSkillsSyncedNoEnv(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(officeSkillsDirEnv, "")
	if err := ensureOfficeSkillsSynced(); err != nil {
		t.Fatalf("无 env 应 no-op: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(home, ".agents")); err == nil {
		t.Fatal("无 env 不应创建任何目录")
	}
}

// TestEnsureOfficeSkillsSyncedMissingSource 源目录不存在 → 报错（不 panic、不误建目标）。
func TestEnsureOfficeSkillsSyncedMissingSource(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(officeSkillsDirEnv, filepath.Join(t.TempDir(), "not-exist"))
	if err := ensureOfficeSkillsSynced(); err == nil {
		t.Fatal("源目录不存在应报错")
	}
	if _, err := os.Lstat(filepath.Join(home, ".agents")); err == nil {
		t.Fatal("源不可读时不应创建目标目录")
	}
}

// mkOfficeSkill 造最小办公技能目录 root/<name>（SKILL.md + scripts/helper.py）。
func mkOfficeSkill(t *testing.T, root, name string) {
	t.Helper()
	mkOfficeSkillAt(t, filepath.Join(root, name), name)
}

// mkOfficeSkillAt 在任意路径造最小办公技能目录（symlink 目标等非 root/<name> 布局用）。
func mkOfficeSkillAt(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: " + name + "\ndescription: test\n---\n\nbody of " + name + "\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "helper.py"), []byte("# "+name+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}
