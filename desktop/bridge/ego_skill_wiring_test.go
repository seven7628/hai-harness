package main

// ego-browser 技能「启动同步 → 注册表可见」的接线回归测试（2026-09）：
//
// 与 office_skill_wiring_test.go 同因同形：ensureEgoSkillSynced 晚于
// buildSkillsRegistry 调用时，首次启动技能已落盘却不在清单里 —— 用户必须手动
// /reload_skills 才能 load_skill，而需要这条分发的正是「未装 ego lite 的机器」
// （ego lite onboarding 会用 symlink 在本机建好，只有那个场景要走内置副本）。
//
// 单测 ensureEgoSkillSynced 只能证明文件落盘，证明不了顺序，故这里走
// m.dispatch("skills") 的真实路径。

import (
	"os"
	"path/filepath"
	"testing"
)

// TestEgoSkillSyncVisibleInRegistryOnFirstLaunch 首次启动（空 HOME）→ 同步后的
// ego-browser 必须直接出现在 skills 清单里（顺序回归）。
func TestEgoSkillSyncVisibleInRegistryOnFirstLaunch(t *testing.T) {
	t.Setenv(egoSkillDirEnv, seedEgoSkillSource(t))

	ws := t.TempDir()
	m, buf := testManager(t)
	m.dispatch(cmd(0, "skills", map[string]any{"workspace": ws}))
	lines := readLines(buf)
	if len(lines) == 0 || lines[0]["ok"] != true {
		t.Fatalf("skills 应成功: %v", lines)
	}
	for _, it := range lines[0]["data"].(map[string]any)["skills"].([]any) {
		if it.(map[string]any)["name"] == "ego-browser" {
			return
		}
	}
	t.Fatalf("首次启动 ego-browser 不在清单里（同步晚于 buildSkillsRegistry？）: %v",
		lines[0]["data"].(map[string]any)["skills"])
}

// seedEgoSkillSource 造与产品级同形的 ego-browser 技能源（SKILL.md + scripts/）。
// 不落 learnings/references：本测试只关心「被发现」，内容齐全与否由 ego 的单测覆盖。
func seedEgoSkillSource(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "ego-browser-skill")
	if err := os.MkdirAll(filepath.Join(dir, "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: ego-browser\ndescription: test\n---\n\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scripts", "install.sh"), []byte("#!/bin/sh\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}
