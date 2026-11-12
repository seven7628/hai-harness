package main

// 办公技能「启动同步 → 注册表可见」的接线回归测试（2026-09）：
//
// 单测 ensureOfficeSkillsSynced 只能证明文件落盘；真正的产品行为是「用户开箱即在技能
// 清单里看到 docx/xlsx/pdf」。这两件事之间隔着调用顺序——buildSkillsRegistry 只扫一次
// 目录，若同步发生在建表之后，首次启动技能已落盘却不在清单里（E2E 复现：registry
// skills = []），用户必须手动 /reload_skills 才能 load_skill。
// 因此这里不测同步函数本身，而是走 m.dispatch("skills") 的真实路径，锁死顺序。

import (
	"os"
	"path/filepath"
	"testing"
)

// TestOfficeSkillsSyncVisibleInRegistryOnFirstLaunch 首次启动（空 HOME）→
// 同步后的三个技能必须直接出现在 skills 清单里（顺序回归）。
func TestOfficeSkillsSyncVisibleInRegistryOnFirstLaunch(t *testing.T) {
	t.Setenv(officeSkillsDirEnv, seedOfficeSkillsSource(t))

	ws := t.TempDir()
	m, buf := testManager(t) // testManager 自己设定隔离 HOME，故技能源 env 在此之后仍有效
	m.dispatch(cmd(0, "skills", map[string]any{"workspace": ws}))
	lines := readLines(buf)
	if len(lines) == 0 || lines[0]["ok"] != true {
		t.Fatalf("skills 应成功: %v", lines)
	}
	names := map[string]bool{}
	for _, it := range lines[0]["data"].(map[string]any)["skills"].([]any) {
		names[it.(map[string]any)["name"].(string)] = true
	}
	for _, want := range []string{"docx", "xlsx", "pdf"} {
		if !names[want] {
			t.Fatalf("首次启动技能 %q 不在清单里（同步晚于 buildSkillsRegistry？）: %v", want, names)
		}
	}
}

// TestOfficeSkillsSyncSkipsExistingInRegistry 全局层已有用户版本 → 首次启动用用户的，
// 不被内置副本覆盖（清单里仍是用户版本）。
func TestOfficeSkillsSyncSkipsExistingInRegistry(t *testing.T) {
	t.Setenv(officeSkillsDirEnv, seedOfficeSkillsSource(t))

	ws := t.TempDir()
	m, buf := testManager(t)
	// testManager 已设定隔离 HOME——用户版本必须种在**那个** HOME 里（早于它设 HOME
	// 会种到没人读的目录，同步照样覆盖全局层，测试即失去意义）。
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	userDir := filepath.Join(home, ".agents", "skills", "docx")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: docx\ndescription: 用户自己的版本\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(userDir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}

	m.dispatch(cmd(0, "skills", map[string]any{"workspace": ws}))
	lines := readLines(buf)
	if len(lines) == 0 || lines[0]["ok"] != true {
		t.Fatalf("skills 应成功: %v", lines)
	}
	for _, it := range lines[0]["data"].(map[string]any)["skills"].([]any) {
		meta := it.(map[string]any)
		if meta["name"] == "docx" {
			if meta["description"] != "用户自己的版本" {
				t.Fatalf("已存在的用户版本被内置副本覆盖了: %v", meta)
			}
			return
		}
	}
	t.Fatal("docx 未在清单里")
}

// seedOfficeSkillsSource 造与产品级同形的技能源目录（docx/xlsx/pdf，各含 scripts/）。
func seedOfficeSkillsSource(t *testing.T) string {
	t.Helper()
	src := filepath.Join(t.TempDir(), "office-skills")
	for _, name := range []string{"docx", "xlsx", "pdf"} {
		mkOfficeSkill(t, src, name)
	}
	return src
}
