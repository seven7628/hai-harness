package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeBridgeExternalSkill(t *testing.T, skillsDir, name, body string) {
	t.Helper()
	dir := filepath.Join(skillsDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: " + name + "\ndescription: " + name + " description\n---\n" + body
	if err := os.WriteFile(filepath.Join(dir, "SKILL.md"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func commandResponseData(t *testing.T, buf *syncBuf) map[string]any {
	t.Helper()
	resp, ok := lastResp(t, buf)
	if !ok {
		t.Fatal("没有 command_response")
	}
	if resp["ok"] != true {
		t.Fatalf("command failed: %v", resp)
	}
	data, ok := resp["data"].(map[string]any)
	if !ok {
		t.Fatalf("response data 类型错误: %T (%v)", resp["data"], resp)
	}
	return data
}

func skillNamesFromResponse(t *testing.T, buf *syncBuf) map[string]bool {
	t.Helper()
	data := commandResponseData(t, buf)
	items, ok := data["skills"].([]any)
	if !ok {
		t.Fatalf("skills response 类型错误: %T (%v)", data["skills"], data)
	}
	names := make(map[string]bool, len(items))
	for _, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			t.Fatalf("skill item 类型错误: %T", raw)
		}
		name, ok := item["name"].(string)
		if !ok || name == "" {
			t.Fatalf("skill item 缺少 name: %v", item)
		}
		names[name] = true
	}
	return names
}

func TestExternalSkillsCommandsRefreshAllWorkspaceRegistries(t *testing.T) {
	m, buf := testManager(t)
	home := os.Getenv("HOME")
	wsA, wsB := t.TempDir(), t.TempDir()

	// 先创建工作区运行态，让测试确认导入后是热重建，而不是只在新工作区
	// 首次 runtime 时发现技能。
	m.runtime(wsA)
	m.runtime(wsB)
	codexSkills := filepath.Join(home, ".codex", "skills")
	claudeSkills := filepath.Join(home, ".claude", "skills")
	if err := os.MkdirAll(codexSkills, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(claudeSkills, 0o755); err != nil {
		t.Fatal(err)
	}
	writeBridgeExternalSkill(t, codexSkills, "codex-only", "codex body")
	writeBridgeExternalSkill(t, codexSkills, "shared", "codex shared body")
	writeBridgeExternalSkill(t, claudeSkills, "shared", "claude shared body")

	m.dispatch(cmd(1, "external_skills_status", nil))
	statusData := commandResponseData(t, buf)
	status, ok := statusData["status"].(map[string]any)
	if !ok || status["enabled"] != true {
		t.Fatalf("外部 Skills status 应启用: %v", statusData)
	}
	sources, ok := status["sources"].([]any)
	if !ok || len(sources) != 2 {
		t.Fatalf("status sources 异常: %v", statusData)
	}

	m.dispatch(cmd(2, "external_skills_import", nil))
	importData := commandResponseData(t, buf)
	imported, ok := importData["imported"].([]any)
	if !ok || len(imported) != 3 {
		t.Fatalf("应导入 3 条来源记录: %v", importData)
	}

	if _, err := os.Stat(filepath.Join(home, ".agents", "skills", "codex-only", "SKILL.md")); err != nil {
		t.Fatalf("全局技能目录未写入: %v", err)
	}
	for _, ws := range []string{wsA, wsB} {
		reg := m.runtime(ws).skills
		for _, name := range []string{"codex-only", "shared"} {
			if _, err := reg.Skill(name); err != nil {
				t.Fatalf("工作区 %s 未刷新技能 %q: %v", ws, name, err)
			}
		}
		shared, err := reg.Skill("shared")
		if err != nil || !strings.Contains(shared.Instructions, "claude shared body") {
			t.Fatalf("Claude 同名技能未覆盖 Codex: %+v (%v)", shared, err)
		}
	}

	m.dispatch(cmd(3, "skills", map[string]any{"workspace": wsA}))
	if names := skillNamesFromResponse(t, buf); !names["codex-only"] || !names["shared"] {
		t.Fatalf("工作区 A 技能清单未刷新: %v", names)
	}
	m.dispatch(cmd(4, "skills", map[string]any{"workspace": wsB}))
	if names := skillNamesFromResponse(t, buf); !names["codex-only"] || !names["shared"] {
		t.Fatalf("工作区 B 技能清单未刷新: %v", names)
	}
}

func TestExternalSkillsStatusAndImportWithoutSource(t *testing.T) {
	m, buf := testManager(t)
	m.dispatch(cmd(1, "external_skills_status", nil))
	data := commandResponseData(t, buf)
	status, ok := data["status"].(map[string]any)
	if !ok || status["enabled"] != false {
		t.Fatalf("没有来源时 status 应禁用: %v", data)
	}

	m.dispatch(cmd(2, "external_skills_import", nil))
	resp, ok := lastResp(t, buf)
	if !ok || resp["ok"] != false {
		t.Fatalf("没有来源时导入应失败: %v", resp)
	}
	if _, ok := resp["data"].(map[string]any); !ok {
		t.Fatalf("失败响应仍应带 status/imported 数据: %v", resp)
	}
}
