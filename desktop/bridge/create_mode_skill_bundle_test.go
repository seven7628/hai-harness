// create_mode_skill_bundle_test.go —— CreateMode 技能“随包分发”的守卫（P4）。
//
// 内容测试证明技能写得对；这个文件证明**它真的能到用户机器上**：
//   - 源目录是 electron-builder extraResources 的来源（package.json）；
//   - Electron main 把源目录注入 bridge（GO_CODE_CREATE_MODE_SKILLS_DIR）；
//   - bridge 的启动同步会把技能复制到 ~/.agents/skills/create-mode。
//
// 少任何一环，打包版用户打开 surface 工具后仍然 load_skill 不到它 —— 内容测试全绿也没用。
package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/skills"
)

// TestCreateModeSkillBundleSourceShape 源目录必须是“集合目录 + 每个技能一个子目录”，
// 且当前只有一个 create-mode；syncBundledSkills 按子目录名安装，不能把 Go/TS 侧接错。
func TestCreateModeSkillBundleSourceShape(t *testing.T) {
	entries, err := os.ReadDir(createModeSkillRoot)
	if err != nil {
		t.Fatalf("读内置技能源目录 %s 失败: %v", createModeSkillRoot, err)
	}
	var skillsFound []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(createModeSkillRoot, e.Name(), "SKILL.md")); err == nil {
			skillsFound = append(skillsFound, e.Name())
		}
	}
	if len(skillsFound) != 1 || skillsFound[0] != createModeSkillName {
		t.Fatalf("内置源目录应有且只有 %q，实际 %v", createModeSkillName, skillsFound)
	}
}

// TestCreateModeSkillPackaged 锁住 electron-builder 的 extraResources 与 main 注入。
// 这是静态 wiring 检查，不是打包测试；真正的打包回归仍由 release 构建覆盖。
func TestCreateModeSkillPackaged(t *testing.T) {
	raw, err := os.ReadFile("../app/package.json")
	if err != nil {
		t.Fatalf("读 package.json 失败: %v", err)
	}
	var pkg struct {
		Build struct {
			ExtraResources []struct {
				From string `json:"from"`
				To   string `json:"to"`
			} `json:"extraResources"`
		} `json:"build"`
	}
	if err := json.Unmarshal(raw, &pkg); err != nil {
		t.Fatalf("package.json 不是合法 JSON: %v", err)
	}
	found := false
	for _, r := range pkg.Build.ExtraResources {
		if r.From == "../vendor/create-mode-skills" && r.To == "create-mode-skills" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("package.json extraResources 缺 ../vendor/create-mode-skills → create-mode-skills")
	}

	mainTS, err := os.ReadFile("../app/electron/main.ts")
	if err != nil {
		t.Fatalf("读 electron/main.ts 失败: %v", err)
	}
	for _, must := range []string{"GO_CODE_CREATE_MODE_SKILLS_DIR", "create-mode-skills"} {
		if !strings.Contains(string(mainTS), must) {
			t.Fatalf("electron/main.ts 没有注入/定位 %q —— 打包后 bridge 拿不到源目录", must)
		}
	}
	bridgeMain, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatalf("读 bridge/main.go 失败: %v", err)
	}
	if !strings.Contains(string(bridgeMain), "ensureCreateModeSkillsSynced()") {
		t.Fatal("bridge/main.go 没有调用 ensureCreateModeSkillsSynced() —— 源目录注入了也不会落盘")
	}
}

// TestCreateModeSkillSyncInstallsBundledCopy 用临时 HOME 跑一次真正的启动同步，
// 证明 syncBundledSkills 能把 vendor 里的目录（含 references）完整装到全局技能层。
func TestCreateModeSkillSyncInstallsBundledCopy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv(createModeSkillsDirEnv, createModeSkillRoot)
	if err := ensureCreateModeSkillsSynced(); err != nil {
		t.Fatalf("同步内置 CreateMode 技能失败: %v", err)
	}
	globalDir := filepath.Join(home, ".agents", "skills")
	reg, err := skills.NewRegistry(globalDir)
	if err != nil {
		t.Fatalf("扫描同步后的全局技能层失败: %v", err)
	}
	s, err := reg.Skill(createModeSkillName)
	if err != nil {
		t.Fatalf("同步后找不到技能 %q: %v", createModeSkillName, err)
	}
	if s.Version == "" {
		t.Fatal("同步后的技能缺 version —— 下次升级无法判断是否覆盖")
	}
	for _, rel := range []string{"SKILL.md", "references/panel-api.md", "scripts/validate-plugin.mjs", "assets/surface.json"} {
		if _, err := os.Stat(filepath.Join(globalDir, createModeSkillName, rel)); err != nil {
			t.Fatalf("同步后缺资源 %q: %v", rel, err)
		}
	}
}
