package main

// P0-08：bridge runtime 拆分（createIn / hubRuntime）。
//
// 钉住三件事：
//  1. 项目路径的 runtime 仍是 runtimeProject，createOpts 与 createIn 等价；
//  2. Hub 智能体的 runtime 只吃全局层：项目技能 / 项目 MCP 一个都进不来；
//  3. 智能体 runtime 不混进 m.workspaces（那些遍历是项目语义）。

import (
	"os"
	"path/filepath"
	"testing"
)

// writeTestSkill 在 dir/<name>/SKILL.md 写一个最小可用技能。
func writeTestSkill(t *testing.T, dir, name string) {
	t.Helper()
	sd := filepath.Join(dir, name)
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatal(err)
	}
	body := "---\nname: " + name + "\ndescription: 测试技能\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(sd, "SKILL.md"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestHubRuntimeGlobalOnly 智能体运行时读全局层，但绝不读项目层。
func TestHubRuntimeGlobalOnly(t *testing.T) {
	m, _ := testManager(t)

	proj := t.TempDir()
	writeTestSkill(t, filepath.Join(proj, ".agents", "skills"), "proj-only")
	mcpCfg := `{"mcpServers":{"proj-srv":{"command":"/bin/true"}}}`
	if err := os.WriteFile(filepath.Join(proj, ".mcp.json"), []byte(mcpCfg), 0o600); err != nil {
		t.Fatal(err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	writeTestSkill(t, filepath.Join(home, ".agents", "skills"), "global-only")
	// 用户级 MCP（~/.go-code/settings.json）+ 项目级（{ws}/.mcp.json）各一台：
	// Hub 只该看到用户级那台（正对照 + 负对照都要，否则「都没读到」也能绿）。
	if err := os.MkdirAll(filepath.Join(home, ".go-code"), 0o755); err != nil {
		t.Fatal(err)
	}
	userCfg := `{"mcpServers":{"user-srv":{"command":"/bin/true"}}}`
	if err := os.WriteFile(filepath.Join(home, ".go-code", "settings.json"), []byte(userCfg), 0o600); err != nil {
		t.Fatal(err)
	}

	// 智能体 home 里放「项目形状」的技能与 MCP：如果谁把 hubRuntime 改成项目装配
	// （buildSkillsRegistry(home) / mcpConfigLayers(home)），这两个会立刻冒出来。
	// 每智能体自带技能是后续任务（P2）的显式能力，不在 P0-08 的全局层里。
	agentHome := hubAgentHome("ag_hubglobal")
	writeTestSkill(t, filepath.Join(agentHome, ".agents", "skills"), "agent-home-only")
	if err := os.WriteFile(filepath.Join(agentHome, ".mcp.json"),
		[]byte(`{"mcpServers":{"agent-home-srv":{"command":"/bin/true"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	hub := m.hubRuntime("ag_hubglobal")
	if hub.kind != runtimeHubAgent || hub.agentID != "ag_hubglobal" {
		t.Fatalf("hub runtime kind/agent = %v/%q", hub.kind, hub.agentID)
	}
	if hub.path != hubAgentHome("ag_hubglobal") {
		t.Fatalf("hub home = %q, want %q", hub.path, hubAgentHome("ag_hubglobal"))
	}

	got := map[string]bool{}
	for _, d := range hub.skills.All() {
		got[d.Name] = true
	}
	if !got["global-only"] {
		t.Fatalf("global skill missing from hub runtime: %v", got)
	}
	if got["proj-only"] || got["agent-home-only"] {
		t.Fatalf("project-shaped skill leaked into hub runtime: %v", got)
	}
	hubMCP := map[string]bool{}
	for _, st := range hub.mcp.Status() {
		hubMCP[st.Name] = true
	}
	if !hubMCP["user-srv"] {
		t.Fatalf("user-level MCP server missing from hub runtime: %v", hubMCP)
	}
	if hubMCP["proj-srv"] || hubMCP["agent-home-srv"] {
		t.Fatalf("project-shaped MCP server leaked into hub runtime: %v", hubMCP)
	}

	// 对照组：同一个 m 上的项目 runtime 仍能看到项目层（证明上面的断言不是「什么都没读到」）。
	projWS := m.runtime(proj)
	if projWS.kind != runtimeProject {
		t.Fatalf("project runtime kind = %v", projWS.kind)
	}
	projSkills := map[string]bool{}
	for _, d := range projWS.skills.All() {
		projSkills[d.Name] = true
	}
	if !projSkills["proj-only"] {
		t.Fatalf("project skill missing from project runtime: %v", projSkills)
	}
	foundProjMCP := false
	for _, st := range projWS.mcp.Status() {
		if st.Name == "proj-srv" {
			foundProjMCP = true
		}
	}
	if !foundProjMCP {
		t.Fatal("project MCP server missing from project runtime")
	}
}

// TestHubRuntimeRegistryIsolation 智能体运行时按 agentID 缓存、与项目运行时分表登记。
func TestHubRuntimeRegistryIsolation(t *testing.T) {
	m, _ := testManager(t)

	a := m.hubRuntime("ag_aaa")
	if got := m.hubRuntime("ag_aaa"); got != a {
		t.Fatal("hub runtime should be cached per agent")
	}
	b := m.hubRuntime("ag_bbb")
	if b == a || b.storeDir == a.storeDir || b.path == a.path {
		t.Fatalf("hub runtimes not isolated: %q vs %q", a.path, b.path)
	}
	if len(m.workspaces) != 0 {
		t.Fatalf("hub runtimes must not live in m.workspaces, got %d entries", len(m.workspaces))
	}
	if _, ok := m.hubRuntimes["ag_aaa"]; !ok {
		t.Fatal("hub runtime not registered")
	}

	// 关闭后重开必须是新实例（旧实例的 MCP 连接/探活已停）。
	m.closeHubRuntimes()
	if len(m.hubRuntimes) != 0 {
		t.Fatalf("closeHubRuntimes left %d entries", len(m.hubRuntimes))
	}
	if again := m.hubRuntime("ag_aaa"); again == a {
		t.Fatal("closed hub runtime should not be reused")
	}
	m.closeHubRuntimes()
}

// TestCreateOptsForwardsToCreateIn createOpts = m.runtime(wsPath) + createIn；
// 传同一个 runtime 时两次调用命中同一个会话（复用语义不因拆分改变）。
func TestCreateOptsForwardsToCreateIn(t *testing.T) {
	m, _ := testManager(t)
	wsDir := t.TempDir()

	ws := m.runtime(wsDir)
	if ws.kind != runtimeProject {
		t.Fatalf("runtime kind = %v, want runtimeProject", ws.kind)
	}
	bs, err := m.createIn(ws, "s1", "", sessionOpts{})
	if err != nil {
		t.Fatalf("createIn: %v", err)
	}
	if bs.workspace != wsDir {
		t.Fatalf("session workspace = %q, want %q", bs.workspace, wsDir)
	}
	if ws.sessions["s1"] != bs {
		t.Fatal("createIn did not register the session on the given runtime")
	}
	again, err := m.createOpts(wsDir, "s1", "", sessionOpts{})
	if err != nil {
		t.Fatalf("createOpts: %v", err)
	}
	if again != bs {
		t.Fatal("createOpts should reuse the session created via createIn on the same runtime")
	}
	if _, err := m.createIn(nil, "s2", "", sessionOpts{}); err == nil {
		t.Fatal("createIn must reject a nil runtime")
	}
}
