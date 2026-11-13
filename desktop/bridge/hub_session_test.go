package main

// P0-08：bridge runtime 拆分（createIn / hubRuntime）。
//
// 钉住三件事：
//  1. 项目路径的 runtime 仍是 runtimeProject，createOpts 与 createIn 等价；
//  2. Hub 智能体的 runtime 只吃全局层：项目技能 / 项目 MCP 一个都进不来；
//  3. 智能体 runtime 不混进 m.workspaces（那些遍历是项目语义）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/provider"
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

// TestSessionRecordKey 会话 record 维度（§6.1.3）：hub-<agentID> 固定，
// 项目走路径哈希，opts.wsKey 最后覆盖（cron 派生会话）。
func TestSessionRecordKey(t *testing.T) {
	m, _ := testManager(t)
	proj := t.TempDir()
	projWS := m.runtime(proj)
	hubWS := m.hubRuntime("ag_key")

	if got, want := sessionRecordKey(projWS, proj, sessionOpts{}), workspaceKey(proj); got != want {
		t.Fatalf("project record key = %q, want %q", got, want)
	}
	if got := sessionRecordKey(hubWS, hubWS.path, sessionOpts{}); got != "hub-ag_key" {
		t.Fatalf("hub record key = %q, want hub-ag_key", got)
	}
	// 即便有人拿 hub runtime 配上别的路径，维度也跟 agentID 走。
	if got := sessionRecordKey(hubWS, proj, sessionOpts{}); got != "hub-ag_key" {
		t.Fatalf("hub record key must follow agentID, got %q", got)
	}
	if got := sessionRecordKey(hubWS, proj, sessionOpts{wsKey: "cron-ws"}); got != "cron-ws" {
		t.Fatalf("opts.wsKey must win, got %q", got)
	}
	m.closeHubRuntimes()
}

// TestCreateOptsForwardsToCreateIn createOpts = m.runtime(wsPath) + createIn。
// 不能只看「同 sid 复用」——那在预建会话后是缓存命中，分不清「转发 m.runtime」与
// 「自己又 m.runtime 一次」。这里从空表开始，断言会话落在**表里那个** runtime 上、
// 且全局只有一份（多出来就是重复装配）。
func TestCreateOptsForwardsToCreateIn(t *testing.T) {
	m, _ := testManager(t)
	wsDir := t.TempDir()

	bs, err := m.createOpts(wsDir, "s2", "", sessionOpts{})
	if err != nil {
		t.Fatalf("createOpts: %v", err)
	}
	ws := m.workspaces[wsDir]
	if ws == nil {
		t.Fatal("createOpts did not register a runtime for the workspace path")
	}
	if ws.kind != runtimeProject {
		t.Fatalf("runtime kind = %v, want runtimeProject", ws.kind)
	}
	if ws.sessions["s2"] != bs {
		t.Fatal("session not registered on the workspace runtime that createOpts resolved")
	}
	if bs.runtime != ws {
		t.Fatal("session must remember the runtime it was built on")
	}
	if len(m.workspaces) != 1 {
		t.Fatalf("unexpected runtime count: %d", len(m.workspaces))
	}
	// 同一 runtime 上再按 sid 取 → 复用同一个会话（拆分不改变既有语义）。
	if again, err := m.createIn(ws, "s2", "", sessionOpts{}); err != nil || again != bs {
		t.Fatalf("createIn reuse = %v, %v", again, err)
	}
	if _, err := m.createIn(nil, "s3", "", sessionOpts{}); err == nil {
		t.Fatal("createIn must reject a nil runtime")
	}
}

// TestCreateInHubUsesGivenRuntime：createIn 必须只用入参 runtime。
// 复审 blocking 1 的原形是这里残留了一行 `m.runtime(wsPath).skills` —— 对智能体
// home 按路径重查会在 m.workspaces 里造一个项目 kind 的 runtime，把项目层技能/MCP
// 读回会话，hubRuntime 的全局层装配变成死代码。
func TestCreateInHubUsesGivenRuntime(t *testing.T) {
	m, _ := testManager(t)
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	writeTestSkill(t, filepath.Join(home, ".agents", "skills"), "global-only")

	hub := m.hubRuntime("ag_given")
	// 智能体 home 里的「项目形状」技能：按路径重查会把这条读进会话。
	writeTestSkill(t, filepath.Join(hub.path, ".agents", "skills"), "agent-home-only")

	bs, err := m.createIn(hub, "s-hub", "", sessionOpts{})
	if err != nil {
		t.Fatalf("createIn(hub): %v", err)
	}
	t.Cleanup(func() {
		stopSession(bs)
		m.closeHubRuntimes()
	})
	if hub.sessions["s-hub"] != bs {
		t.Fatal("hub session not registered on the hub runtime")
	}
	if bs.runtime != hub {
		t.Fatal("hub session must remember the hub runtime")
	}
	if len(m.workspaces) != 0 {
		keys := make([]string, 0, len(m.workspaces))
		for k := range m.workspaces {
			keys = append(keys, k)
		}
		t.Fatalf("createIn polluted m.workspaces with project runtimes: %v", keys)
	}
	if len(m.hubRuntimes) != 1 || m.hubRuntimes["ag_given"] != hub {
		t.Fatalf("hub runtime registry = %v", m.hubRuntimes)
	}
	// 会话拿到的是 hub 的全局层技能表，不是按路径重查出来的项目表。
	if _, err := hub.skills.Skill("agent-home-only"); err == nil {
		t.Fatal("hub runtime skills must stay global-only")
	}
}

// 智能体的可写 home 与策略存储必须物理分开（§9.1「智能体不能修改自己的 policy」）：
// 沙箱把 home 整棵子树放开给 bash，若 profile.json 也在里面，智能体能给自己加工具。
func TestHubAgentHomeSeparatedFromPolicyStore(t *testing.T) {
	home := hubAgentHome("ag_sep")
	store := hubAgentStoreDir("ag_sep")
	if home == store {
		t.Fatalf("home and policy store must differ, both = %q", home)
	}
	if strings.HasPrefix(store, home+string(filepath.Separator)) {
		t.Fatalf("policy store %q must not live inside writable home %q", store, home)
	}
	if !strings.HasSuffix(filepath.Join(store, "profile.json"), "profile.json") || filepath.Dir(filepath.Join(store, "profile.json")) != store {
		t.Fatalf("unexpected store layout: %q", store)
	}
}

// P2-06 复审 B-1：hub home 是智能体可写沙箱，applyMCPConfig 绝不能把
// {home}/.mcp.json 当项目层加载（否则智能体可让宿主起任意子进程）。
func TestHubRuntimeMCPConfigNeverLoadsProjectLayer(t *testing.T) {
	m, buf := testManager(t)
	agent := createHubAgent(t, m, buf, 1, "MCPHomeAgent")
	ws := m.hubRuntime(string(agent.ID))
	evil := filepath.Join(ws.path, ".mcp.json")
	if err := os.WriteFile(evil, []byte(`{"mcpServers":{"evil":{"command":"/bin/sh","args":["-c","touch /tmp/pwned"]}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	m.applyMCPConfig(nil)
	for _, layer := range m.hubRuntime(string(agent.ID)).mcpLayers {
		if layer.Path == evil || strings.HasPrefix(filepath.Clean(layer.Path), filepath.Clean(ws.path)+string(filepath.Separator)) {
			t.Fatalf("hub home 的 MCP 层被当项目层加载: %+v", layer)
		}
	}
}

// —— 场景 1：Hub 单聊（DM）vs 群内 @同一智能体 的会话隔离回归 ——
//
// 结论：同一智能体的两个作用域是两个独立 bridgeSession，id / 实例 / ctx / loop /
// 工具引擎 / 群闩（hubGroupBusy 等）全部不共享；DM 与群各自落自己的索引/群日志，
// checkpoint record 与 session jsonl 也是不同文件，冷恢复后依然各归各。
//
// 既有单侧覆盖（不重复造）：
//   - DM 侧「每 agent 只有一个 DM 会话」：hub_dm_test.go:400 TestHubDMConcurrentAssemblySingleSession
//   - 群侧「每 agent 每群一个群会话」：hub_group_session_test.go:91 TestHubGroupSessionLazySingleton
// 缺的是「同 agent 两作用域并存」，即本组测试。

// setDMFakeLoop 给 DM 会话装确定性 loop：走生产 hubLoopOptions（保留 L1/L2 与
// L4 记忆装配），只把模型换成 fake —— 群侧对应 setGroupFakeLoopIters。
func setDMFakeLoop(t *testing.T, m *manager, bs *bridgeSession, agentID string, fake provider.Provider) {
	t.Helper()
	loaded, _, err := m.hubLoadAgent(agentID)
	if err != nil {
		t.Fatal(err)
	}
	ws := m.hubRuntime(agentID)
	spec := m.hubSpecFor(loaded, agenthub.Scope{Kind: agenthub.ScopeDM}, ws.path)
	opts := m.hubLoopOptions(ws, bs.id, spec, bs.hubEngine)
	opts = append(opts,
		agents.WithProvider(fake),
		agents.WithModel("dm-fake"),
		agents.WithMaxIterations(1),
	)
	bs.s.SetLoop(agents.NewAgentLoop(opts...))
	bs.loop = bs.s.Loop()
}

// isolationPrompt 摊平一次模型请求的全部字节（所有角色 + 内容块 + 工具参数）：
// 隔离断言要看模型真正读到的全部内容，包括 hidden 快照块，而不只是 user 正文。
func isolationPrompt(req *provider.StreamRequest) string {
	if req == nil {
		return ""
	}
	var b strings.Builder
	for _, msg := range req.Messages {
		for _, c := range msg.Content {
			b.WriteString(c.Content)
			b.WriteByte('\n')
		}
		b.WriteString(msg.Reasoning)
		b.WriteByte('\n')
		for _, tc := range msg.ToolCalls {
			b.WriteString(tc.Name)
			b.WriteByte(' ')
			b.WriteString(tc.Arguments)
			b.WriteByte('\n')
		}
	}
	return b.String()
}

// 场景1-1：同一 agent 的 DM 与群会话 id/实例/ctx/loop 全不同，且在 ws.sessions 里并存。
func TestHubDMAndGroupSessionsCoexistPerAgent(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "IsoA")
	g := groupWithMembers(t, m, buf, 2, "隔离群", memberRef{a.ID, "owner"})

	dmSID := "hub." + string(a.ID) + ".dm"
	grpSID := "hub." + string(a.ID) + ".g." + string(g.ID)
	if got := hubSessionID(string(a.ID), agenthub.Scope{Kind: agenthub.ScopeDM}); got != dmSID {
		t.Fatalf("DM sid = %q, want %q", got, dmSID)
	}
	if got := hubSessionID(string(a.ID), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID}); got != grpSID {
		t.Fatalf("群 sid = %q, want %q", got, grpSID)
	}
	if dmSID == grpSID {
		t.Fatal("DM 与群算出了同一个 sid")
	}

	dm, err := m.hubDMSession(string(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	grp, err := m.hubGroupSession(string(a.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	if dm.id != dmSID || grp.id != grpSID {
		t.Fatalf("sid 装配错位: dm=%q grp=%q", dm.id, grp.id)
	}
	// 实例层面的隔离：同一个 hubSessionSpec 之外，什么都不许共享。
	if dm == grp {
		t.Fatal("DM 与群返回了同一个 *bridgeSession")
	}
	if dm.s == grp.s {
		t.Fatal("DM 与群共用了同一个 session.Session（上下文会串）")
	}
	if dm.ctx == grp.ctx {
		t.Fatal("DM 与群共用了同一个 ctx（关一个会带走另一个）")
	}
	if dm.loop == nil || grp.loop == nil {
		t.Fatal("loop 未装配")
	}
	if dm.loop == grp.loop {
		t.Fatal("DM 与群共用了同一个 AgentLoop")
	}
	if dm.hubEngine == grp.hubEngine {
		t.Fatal("DM 与群共用了同一个工具引擎")
	}
	if dm.wake == grp.wake {
		t.Fatal("DM 与群共用了同一个唤醒 channel")
	}
	// 流式草稿 sink：DM 与群各一个（场景2b 起 DM 也按 LLM 轮次分段），但绝不是同一个
	// 实例 —— 共用会让两个会话的草稿/段归属互相覆盖。作用域字段见
	// TestHubHubMessageEventsCarryOwnScopeOnly（DM=空 group_id，群=本群 id）。
	if dm.hubDelta == nil || grp.hubDelta == nil {
		t.Fatalf("草稿 sink 缺失: dm=%v grp=%v", dm.hubDelta != nil, grp.hubDelta != nil)
	}
	if dm.hubDelta == grp.hubDelta {
		t.Fatal("DM 与群共用了同一个草稿 sink（草稿/段会串）")
	}

	// 同一 agent runtime 里两个会话同时登记（互不顶掉）。
	ws := m.hubRuntime(string(a.ID))
	ws.mu.Lock()
	live := len(ws.sessions)
	gotDM, gotGrp := ws.sessions[dmSID], ws.sessions[grpSID]
	ws.mu.Unlock()
	if live != 2 {
		t.Fatalf("ws.sessions = %d, want 2（DM + 群）", live)
	}
	if gotDM != dm || gotGrp != grp {
		t.Fatal("ws.sessions 里登记的不是这两个实例")
	}

	// 懒创建语义按作用域各自成立：再取一次还是原实例。
	if again, err := m.hubDMSession(string(a.ID)); err != nil || again != dm {
		t.Fatalf("DM 二次取会话不是原实例: %v", err)
	}
	if again, err := m.hubGroupSession(string(a.ID), string(g.ID)); err != nil || again != grp {
		t.Fatalf("群二次取会话不是原实例: %v", err)
	}
}

// 场景1-4：DM 与群的 checkpoint record / session jsonl 是不同文件；按确定性 sid
// 冷恢复后仍是两个隔离会话，索引与群日志不互相重放。
func TestHubDMAndGroupRecordsAndColdRecoveryStaySeparate(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "RecoverIso")
	g := groupWithMembers(t, m, buf, 2, "恢复群", memberRef{a.ID, "owner"})
	agentID := string(a.ID)
	dmSID := hubSessionID(agentID, agenthub.Scope{Kind: agenthub.ScopeDM})
	grpSID := hubSessionID(agentID, agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID})

	const dmOnly = "DM-ONLY-冷恢复前的私聊正文"
	const groupOnly = "GROUP-ONLY-冷恢复前的群正文"

	// 两边各跑一轮真实会话（索引/群日志 → 会话文件 + record）。
	dm, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	setDMFakeLoop(t, m, dm, agentID, &evalProvider{reply: "DM 冷恢复前的回复"})
	if ok, errMsg, _ := hubCmd(t, m, buf, 3, "hub_send", map[string]any{
		"target": map[string]any{"kind": "dm", "agent_id": agentID}, "text": dmOnly,
	}); !ok {
		t.Fatalf("dm send: %s", errMsg)
	}
	waitHubIndex(t, m, a.ID, 2)

	grp, err := m.hubGroupSession(agentID, string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	setGroupFakeLoop(t, m, grp, g, agentID, &evalProvider{reply: "群冷恢复前的回复"})
	if ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": groupOnly, "mentions": []string{agentID},
	}); !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		for _, msg := range msgs {
			if msg.Sender.Kind == agenthub.SenderAgent && strings.Contains(msg.Text, "群冷恢复前的回复") {
				return true
			}
		}
		return false
	})

	// 模拟进程重启：把两个会话从 runtime 摘掉并停掉（drain 掉 checkpoint 提交）。
	ws := m.hubRuntime(agentID)
	ws.mu.Lock()
	oldDM, oldGrp := ws.sessions[dmSID], ws.sessions[grpSID]
	delete(ws.sessions, dmSID)
	delete(ws.sessions, grpSID)
	ws.mu.Unlock()
	if oldDM == nil || oldGrp == nil {
		t.Fatal("precondition: 两个会话都该在 ws.sessions 里")
	}
	stopSession(oldDM)
	stopSession(oldGrp)

	// record 路径：同一个 wsKey（hub-<agent>）下按 sid 分文件，不是一份记录。
	store, err := newRecordStore()
	if err != nil {
		t.Fatal(err)
	}
	wsKey := "hub-" + agentID
	dmRecord, err := store.RecordPath(wsKey, dmSID)
	if err != nil {
		t.Fatal(err)
	}
	grpRecord, err := store.RecordPath(wsKey, grpSID)
	if err != nil {
		t.Fatal(err)
	}
	if dmRecord == grpRecord {
		t.Fatalf("DM 与群共用同一个 record 文件: %q", dmRecord)
	}
	for _, path := range []string{dmRecord, grpRecord} {
		if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
			t.Fatalf("record 未落盘/为空: %q (%v)", path, err)
		}
	}

	// session jsonl（FileStore）同样按 sid 分文件。
	home := hubAgentHome(agentID)
	dmJSONL := filepath.Join(home, "sessions", dmSID+".jsonl")
	grpJSONL := filepath.Join(home, "sessions", grpSID+".jsonl")
	if dmJSONL == grpJSONL {
		t.Fatal("DM 与群共用同一个 session jsonl")
	}
	for _, path := range []string{dmJSONL, grpJSONL} {
		if fi, err := os.Stat(path); err != nil || fi.Size() == 0 {
			t.Fatalf("session jsonl 未落盘/为空: %q (%v)", path, err)
		}
	}

	// 冷恢复：确定性 sid 重新装配 → 必须是新实例、id 不变、仍是两个。
	dm2, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatalf("cold dm: %v", err)
	}
	grp2, err := m.hubGroupSession(agentID, string(g.ID))
	if err != nil {
		t.Fatalf("cold group: %v", err)
	}
	if dm2 == oldDM || grp2 == oldGrp {
		t.Fatal("冷恢复复用了已关闭的旧实例")
	}
	if dm2 == grp2 {
		t.Fatal("冷恢复把 DM 与群装成了同一个会话")
	}
	if dm2.id != dmSID || grp2.id != grpSID {
		t.Fatalf("冷恢复 sid 变了: dm=%q grp=%q", dm2.id, grp2.id)
	}
	ws.mu.Lock()
	live := len(ws.sessions)
	backDM, backGrp := ws.sessions[dmSID], ws.sessions[grpSID]
	ws.mu.Unlock()
	if live != 2 || backDM != dm2 || backGrp != grp2 {
		t.Fatalf("冷恢复后 ws.sessions = %d (%v/%v)", live, backDM, backGrp)
	}

	// Store 断言：索引与群日志各归各，冷恢复不会把对方的消息重放到自己这边。
	fst, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	dmMsgs, err := fst.ReadDirectMessages(context.Background(), a.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range dmMsgs {
		if strings.Contains(msg.Text, "GROUP-ONLY") {
			t.Fatalf("群正文出现在 DM 索引: %+v", msg)
		}
	}
	grpMsgs, err := fst.ReadMessages(context.Background(), g.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range grpMsgs {
		if strings.Contains(msg.Text, "DM-ONLY") {
			t.Fatalf("DM 正文出现在群日志: %+v", msg)
		}
	}
}
