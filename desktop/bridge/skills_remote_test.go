package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// gitRepoFilesFixture 建本地 git 仓库（自定义文件集），返回 file:// URL。
func gitRepoFilesFixture(t *testing.T, files map[string]string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("需要 git")
	}
	repo := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	run("init", "-q")
	for path, content := range files {
		fp := filepath.Join(repo, filepath.FromSlash(path))
		if err := os.MkdirAll(filepath.Dir(fp), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fp, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("add", "-A")
	run("commit", "-q", "-m", "init")
	return "file://" + repo
}

// gitRepoFixture 建本地 git 仓库（根 SKILL.md），返回 file:// URL。
func gitRepoFixture(t *testing.T, skillName, desc string) string {
	t.Helper()
	md := fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n# %s\n指令正文\n", skillName, desc, skillName)
	return gitRepoFilesFixture(t, map[string]string{"SKILL.md": md})
}

// TestSkillInstallCommands：skill_install（全局）→ 技能进注册表 + 清单带来源 → 卸载移除。
// HOME 隔离避免污染真实 ~/.agents/skills / ~/.go-code/skills。
func TestSkillInstallCommands(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, buf := testManager(t)
	ws := t.TempDir()
	repo := gitRepoFixture(t, "remote-skill", "远端技能")

	m.runtime(ws) // 建工作区

	// 安装（全局）
	m.dispatch(cmd(1, "skill_install", map[string]any{"workspace": ws, "url": repo, "scope": "global"}))
	resp, ok := lastResp(t, buf)
	if !ok || resp["ok"] != true {
		t.Fatalf("install 应 ok: %v", resp)
	}
	skills := resp["data"].(map[string]any)["skills"].([]any)
	if len(skills) != 1 || skills[0].(map[string]any)["name"] != "remote-skill" {
		t.Fatalf("install 结果 = %v", skills)
	}
	// 已安装目录（主路径 ~/.agents/skills）
	if _, err := os.Stat(filepath.Join(agentSkillsDir(), "remote-skill", "SKILL.md")); err != nil {
		t.Fatalf("全局目录应有技能: %v", err)
	}

	// skills 清单带 source
	m.dispatch(cmd(2, "skills", map[string]any{"workspace": ws}))
	resp, _ = lastResp(t, buf)
	listed := resp["data"].(map[string]any)["skills"].([]any)
	found := false
	for _, s := range listed {
		sm := s.(map[string]any)
		if sm["name"] == "remote-skill" {
			found = true
			if sm["source"] != repo {
				t.Fatalf("远端技能应带 source: %+v", sm)
			}
		}
	}
	if !found {
		t.Fatalf("技能应出现在清单: %v", listed)
	}

	// 重复安装 → 冲突报错
	m.dispatch(cmd(3, "skill_install", map[string]any{"workspace": ws, "url": repo, "scope": "global"}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != false {
		t.Fatalf("重复安装应报错: %v", resp)
	}

	// 卸载 → 从清单消失 + 目录删除
	m.dispatch(cmd(4, "skill_uninstall", map[string]any{"workspace": ws, "name": "remote-skill"}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != true {
		t.Fatalf("uninstall 应 ok: %v", resp)
	}
	m.dispatch(cmd(5, "skills", map[string]any{"workspace": ws}))
	resp, _ = lastResp(t, buf)
	for _, s := range resp["data"].(map[string]any)["skills"].([]any) {
		if s.(map[string]any)["name"] == "remote-skill" {
			t.Fatalf("卸载后技能不应在清单: %v", resp["data"])
		}
	}

	// 未安装技能卸载 → 报错
	m.dispatch(cmd(6, "skill_uninstall", map[string]any{"workspace": ws, "name": "nope"}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != false {
		t.Fatalf("未安装卸载应报错: %v", resp)
	}
}

// TestSkillBrowseCommand：skill_browse 浏览市场仓库（只读）→ 返回全部技能 + 已装标记；
// 带 name 的 skill_install 精确安装选中的那个（市场逐个选装）。
func TestSkillBrowseCommand(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, buf := testManager(t)
	ws := t.TempDir()
	repo := gitRepoFilesFixture(t, map[string]string{
		".claude-plugin/marketplace.json": `{"plugins":[{"name":"p1","source":"./plugins/p1"}]}`,
		"plugins/p1/skills/a/SKILL.md":    "---\nname: a\ndescription: 技能A\n---\n# a\n指令正文\n",
		"plugins/p1/skills/b/SKILL.md":    "---\nname: b\ndescription: 技能B\n---\n# b\n指令正文\n",
	})

	m.runtime(ws) // 建工作区

	// 浏览 → 2 个技能，均未安装，各带描述
	m.dispatch(cmd(1, "skill_browse", map[string]any{"workspace": ws, "url": repo}))
	resp, ok := lastResp(t, buf)
	if !ok || resp["ok"] != true {
		t.Fatalf("browse 应 ok: %v", resp)
	}
	skills := resp["data"].(map[string]any)["skills"].([]any)
	if len(skills) != 2 {
		t.Fatalf("市场浏览应返回 2 技能: %v", skills)
	}
	byName := map[string]map[string]any{}
	for _, s := range skills {
		sm := s.(map[string]any)
		byName[sm["name"].(string)] = sm
		if sm["installed"] != false {
			t.Fatalf("浏览时未安装: %+v", sm)
		}
		if sm["update_available"] != false {
			t.Fatalf("浏览时应带 update_available=false: %+v", sm)
		}
		if sm["description"] == "" {
			t.Fatalf("浏览应带描述: %+v", sm)
		}
	}
	if byName["a"] == nil || byName["b"] == nil {
		t.Fatalf("市场技能缺 a/b: %v", skills)
	}

	// 带 name 精确安装 b → 只装 b
	m.dispatch(cmd(2, "skill_install", map[string]any{"workspace": ws, "url": repo, "scope": "global", "name": "b"}))
	resp, ok = lastResp(t, buf)
	if !ok || resp["ok"] != true {
		t.Fatalf("install b 应 ok: %v", resp)
	}
	m.dispatch(cmd(3, "skills", map[string]any{"workspace": ws}))
	resp, _ = lastResp(t, buf)
	listed := resp["data"].(map[string]any)["skills"].([]any)
	var names []string
	for _, s := range listed {
		names = append(names, s.(map[string]any)["name"].(string))
	}
	if len(names) != 1 || names[0] != "b" {
		t.Fatalf("精确安装后清单应只有 b: %v", names)
	}
	// a 未被装进全局目录
	if _, err := os.Stat(filepath.Join(agentSkillsDir(), "a")); !os.IsNotExist(err) {
		t.Fatalf("a 不应被安装: %v", err)
	}

	// 缺 url 的浏览 → 报错
	m.dispatch(cmd(4, "skill_browse", map[string]any{"workspace": ws}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != false {
		t.Fatalf("缺 url 浏览应报错: %v", resp)
	}
}

// TestMarketCommands：market_add/list/remove + skill_install 全装 + skill_update_market 按市场更新。
func TestMarketCommands(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, buf := testManager(t)
	ws := t.TempDir()
	// 无根 SKILL.md 的 skills/ 目录仓库（resolveSkills → skills/ 全部技能）
	repo := gitRepoFilesFixture(t, map[string]string{
		"skills/a/SKILL.md": "---\nname: a\ndescription: 技能A\n---\n# a\nbody\n",
		"skills/b/SKILL.md": "---\nname: b\ndescription: 技能B\n---\n# b\nbody\n",
	})

	m.runtime(ws)

	// 保存市场
	m.dispatch(cmd(1, "market_add", map[string]any{"workspace": ws, "url": repo}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != true {
		t.Fatalf("market_add 应 ok: %v", resp)
	}
	// 幂等再存
	m.dispatch(cmd(2, "market_add", map[string]any{"workspace": ws, "url": repo}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != true {
		t.Fatalf("幂等 market_add 应 ok: %v", resp)
	}
	// market_list → 1 条
	m.dispatch(cmd(3, "market_list", map[string]any{"workspace": ws}))
	resp, _ := lastResp(t, buf)
	markets := resp["data"].(map[string]any)["markets"].([]any)
	if len(markets) != 1 {
		t.Fatalf("market_list 应有 1 条: %v", resp["data"])
	}
	// 市场全装（market-skill + extra）
	m.dispatch(cmd(4, "skill_install", map[string]any{"workspace": ws, "url": repo, "scope": "global"}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != true {
		t.Fatalf("市场安装应 ok: %v", resp)
	}
	// 按市场更新 → 更新 2 个
	m.dispatch(cmd(5, "skill_update_market", map[string]any{"workspace": ws, "url": repo}))
	resp, _ = lastResp(t, buf)
	if resp["ok"] != true {
		t.Fatalf("skill_update_market 应 ok: %v", resp)
	}
	updated := resp["data"].(map[string]any)["updated"].([]any)
	if len(updated) != 2 {
		t.Fatalf("应更新 2 技能: %v", resp["data"])
	}
	// 移除市场
	m.dispatch(cmd(6, "market_remove", map[string]any{"workspace": ws, "url": repo}))
	if resp, ok := lastResp(t, buf); !ok || resp["ok"] != true {
		t.Fatalf("market_remove 应 ok: %v", resp)
	}
	m.dispatch(cmd(7, "market_list", map[string]any{"workspace": ws}))
	resp, _ = lastResp(t, buf)
	if len(resp["data"].(map[string]any)["markets"].([]any)) != 0 {
		t.Fatalf("移除后 market_list 应空: %v", resp["data"])
	}
}
