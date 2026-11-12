package main

// ego-browser skill 随包分发同步测试（2026-09）：
//   - 目标不存在 + 源有效 → 复制到 ~/.agents/skills/ego-browser
//   - 目标已存在（真实目录/symlink）→ 跳过（尊重 ego onboarding）
//   - 源不完整（缺 SKILL.md）→ 报错不复制
//   - 无 env（dev 无注入）→ no-op

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEnsureEgoSkillSyncedCopiesWhenMissing 目标不存在 + 源完整 → 复制。
func TestEnsureEgoSkillSyncedCopiesWhenMissing(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(t.TempDir(), "ego-browser-skill")
	mkFakeSkill(t, src)

	t.Setenv("HOME", home)
	t.Setenv(egoSkillDirEnv, src)

	if err := ensureEgoSkillSynced(); err != nil {
		t.Fatalf("sync err: %v", err)
	}
	dst := filepath.Join(home, ".agents", "skills", "ego-browser")
	if _, err := os.Stat(filepath.Join(dst, "SKILL.md")); err != nil {
		t.Fatalf("SKILL.md 未复制到 %s: %v", dst, err)
	}
	if _, err := os.Stat(filepath.Join(dst, "scripts", "install.sh")); err != nil {
		t.Fatalf("scripts/install.sh 未复制（递归复制失败）: %v", err)
	}
}

// TestEnsureEgoSkillSyncedSkipsExisting 目标已存在（symlink，模拟 ego onboarding）→ 跳过不覆盖。
func TestEnsureEgoSkillSyncedSkipsExisting(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(t.TempDir(), "ego-browser-skill")
	mkFakeSkill(t, src)

	// 建 symlink 目标（模拟 ego onboarding：~/.agents/skills/ego-browser → ego-skills）
	linkLayer := filepath.Join(home, ".agents", "skills")
	if err := os.MkdirAll(linkLayer, 0o755); err != nil {
		t.Fatal(err)
	}
	realSkill := filepath.Join(t.TempDir(), "real-ego-skills")
	mkFakeSkill(t, realSkill)
	if err := os.Symlink(realSkill, filepath.Join(linkLayer, "ego-browser")); err != nil {
		t.Skipf("symlink 失败: %v", err)
	}

	t.Setenv("HOME", home)
	t.Setenv(egoSkillDirEnv, src)
	if err := ensureEgoSkillSynced(); err != nil {
		t.Fatalf("sync err: %v", err)
	}
	// symlink 应保持（未被替换为真实目录）
	fi, err := os.Lstat(filepath.Join(linkLayer, "ego-browser"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("已存在的 symlink 不应被覆盖为真实目录")
	}
}

// TestEnsureEgoSkillSyncedBadSource 源缺 SKILL.md → 报错。
func TestEnsureEgoSkillSyncedBadSource(t *testing.T) {
	home := t.TempDir()
	src := filepath.Join(t.TempDir(), "bad-skill")
	if err := os.MkdirAll(src, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	t.Setenv(egoSkillDirEnv, src)
	if err := ensureEgoSkillSynced(); err == nil {
		t.Fatal("源不完整应报错")
	}
}

// TestEnsureEgoSkillSyncedNoEnv 无 env → no-op（dev 场景由 ego onboarding symlink 覆盖）。
func TestEnsureEgoSkillSyncedNoEnv(t *testing.T) {
	t.Setenv(egoSkillDirEnv, "")
	if err := ensureEgoSkillSynced(); err != nil {
		t.Fatalf("无 env 应 no-op: %v", err)
	}
}

// mkFakeSkill 造最小 skill 目录（SKILL.md + scripts/install.sh）。
func mkFakeSkill(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: ego-browser\ndescription: test\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "install.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}
