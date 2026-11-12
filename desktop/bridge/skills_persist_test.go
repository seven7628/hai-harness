package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeWorkspaceSkill 在 dir/<name> 造一个最小技能（SKILL.md）。
func writeWorkspaceSkill(t *testing.T, dir, name string) string {
	t.Helper()
	skillDir := filepath.Join(dir, name)
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	md := "---\nname: " + name + "\ndescription: " + name + " 描述\n---\n指令正文\n"
	if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(md), 0o644); err != nil {
		t.Fatal(err)
	}
	return skillDir
}

// TestSkillTogglePersistsWithoutGoCodeDir 回归：禁用技能必须持久化到
// {ws}/.go-code/skills.json —— 该目录**不存在时也要建**（普通项目工作区技能来自
// {ws}/.agents/skills 或全局 ~/.agents/skills，工作区下没有 .go-code；
// 旧实现 os.WriteFile 直接 ENOENT 且错误被丢弃 → 重启后禁用失效）。
func TestSkillTogglePersistsWithoutGoCodeDir(t *testing.T) {
	ws := t.TempDir()
	writeWorkspaceSkill(t, filepath.Join(ws, ".agents", "skills"), "code-review")
	if _, err := os.Stat(filepath.Join(ws, ".go-code")); !os.IsNotExist(err) {
		t.Fatalf("前置条件：工作区不应有 .go-code 目录（%v）", err)
	}

	m, buf := testManager(t)
	m.dispatch(cmd(0, "skill_toggle", map[string]any{"workspace": ws, "name": "code-review", "enabled": false}))
	if lines := readLines(buf); len(lines) == 0 || lines[0]["ok"] != true {
		t.Fatalf("skill_toggle 应成功: %v", lines)
	}

	b, err := os.ReadFile(filepath.Join(ws, ".go-code", "skills.json"))
	if err != nil {
		t.Fatalf("禁用集应落盘 {ws}/.go-code/skills.json: %v", err)
	}
	if !strings.Contains(string(b), "code-review") {
		t.Fatalf("skills.json 应含禁用技能名: %q", b)
	}

	// 模拟重启：注册表重建后禁用状态必须保留
	reg := buildSkillsRegistry(ws)
	for _, d := range reg.All() {
		if d.Name == "code-review" && d.Enabled {
			t.Fatal("重启（重建注册表）后禁用技能不应重新启用")
		}
	}
}
