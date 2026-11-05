package main

// P1-05：记忆隔离 + 浏览/编辑（记忆面板命令）。

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

func hubMemoryCmd(t *testing.T, m *manager, buf *syncBuf, id int, typ string, payload map[string]any) (bool, string, map[string]any) {
	t.Helper()
	return hubCmd(t, m, buf, id, typ, payload)
}

// 新智能体：清单为空；读不存在 = ok + exists=false；写入后可读回。
func TestHubMemoryBrowseReadWrite(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Mem")
	agentID := string(p.ID)

	ok, errMsg, data := hubMemoryCmd(t, m, buf, 2, "hub_agent_memory", map[string]any{"agent_id": agentID})
	if !ok {
		t.Fatalf("list memory failed: %s", errMsg)
	}
	if entries, _ := data["entries"].([]any); len(entries) != 0 {
		t.Fatalf("new agent should have no memory entries: %v", entries)
	}

	ok, _, data = hubMemoryCmd(t, m, buf, 3, "hub_agent_memory", map[string]any{
		"agent_id": agentID, "path": "MEMORY.md",
	})
	if !ok || data["exists"] != false || data["content"] != "" {
		t.Fatalf("missing memory read = %v, want ok+exists=false", data)
	}

	ok, errMsg, data = hubMemoryCmd(t, m, buf, 4, "hub_agent_memory_write", map[string]any{
		"agent_id": agentID, "path": "MEMORY.md", "content": "- 用户叫小王\n",
	})
	if !ok {
		t.Fatalf("write failed: %s", errMsg)
	}
	if data["bytes"] != float64(len("- 用户叫小王\n")) {
		t.Fatalf("bytes = %v", data["bytes"])
	}
	if data["backup"] != "" {
		t.Fatalf("first write must not create a backup, got %v", data["backup"])
	}
	if data["effective"] != "next_run" {
		t.Fatalf("effective = %v, want next_run", data["effective"])
	}

	ok, _, data = hubMemoryCmd(t, m, buf, 5, "hub_agent_memory", map[string]any{
		"agent_id": agentID, "path": "MEMORY.md",
	})
	if !ok || data["exists"] != true || !strings.Contains(data["content"].(string), "小王") {
		t.Fatalf("read back = %v", data)
	}
	ok, _, data = hubMemoryCmd(t, m, buf, 6, "hub_agent_memory", map[string]any{"agent_id": agentID})
	if !ok {
		t.Fatal("list failed")
	}
	entries, _ := data["entries"].([]any)
	if len(entries) != 1 {
		t.Fatalf("entries = %v, want 1", entries)
	}
	if first, _ := entries[0].(map[string]any); first["name"] != "MEMORY.md" {
		t.Fatalf("entry name = %v", first["name"])
	}
}

// 覆盖写必须留备份（旧内容可找回）。
func TestHubMemoryWriteBacksUpOldContent(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Backup")
	agentID := string(p.ID)
	if ok, errMsg, _ := hubMemoryCmd(t, m, buf, 2, "hub_agent_memory_write", map[string]any{
		"agent_id": agentID, "path": "MEMORY.md", "content": "old",
	}); !ok {
		t.Fatalf("first write failed: %s", errMsg)
	}
	ok, errMsg, data := hubMemoryCmd(t, m, buf, 3, "hub_agent_memory_write", map[string]any{
		"agent_id": agentID, "path": "MEMORY.md", "content": "new",
	})
	if !ok {
		t.Fatalf("second write failed: %s", errMsg)
	}
	backup, _ := data["backup"].(string)
	if backup == "" {
		t.Fatal("overwrite must return a backup name")
	}
	root := filepath.Join(appDataDir(), "hub")
	dir, err := agenthub.MemoryAgentDir(root, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, ".backups", backup))
	if err != nil {
		t.Fatalf("backup unreadable: %v", err)
	}
	if string(body) != "old" {
		t.Fatalf("backup content = %q, want old", body)
	}
}

// A/B 隔离：A 写的记忆 B 看不到（清单与读取都隔离）。
func TestHubMemoryIsolatedBetweenAgents(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "Alpha")
	b := createHubAgent(t, m, buf, 2, "Beta")
	if ok, errMsg, _ := hubMemoryCmd(t, m, buf, 3, "hub_agent_memory_write", map[string]any{
		"agent_id": string(a.ID), "path": "MEMORY.md", "content": "A only",
	}); !ok {
		t.Fatalf("A write failed: %s", errMsg)
	}
	ok, _, data := hubMemoryCmd(t, m, buf, 4, "hub_agent_memory", map[string]any{"agent_id": string(b.ID)})
	if !ok {
		t.Fatal("B list failed")
	}
	if entries, _ := data["entries"].([]any); len(entries) != 0 {
		t.Fatalf("B sees A's memory: %v", entries)
	}
	ok, _, data = hubMemoryCmd(t, m, buf, 5, "hub_agent_memory", map[string]any{
		"agent_id": string(b.ID), "path": "MEMORY.md",
	})
	if !ok || data["exists"] != false {
		t.Fatalf("B read = %v, want empty", data)
	}
}

// 路径逃逸与非 md 文件必须被拒（面板不能借写接口碰到别的目录）。
func TestHubMemoryRejectsEscapes(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Guard")
	for _, bad := range []string{"../x.md", "/etc/passwd.md", `..\x.md`, "notes.txt"} {
		ok, errMsg, _ := hubMemoryCmd(t, m, buf, 2, "hub_agent_memory_write", map[string]any{
			"agent_id": string(p.ID), "path": bad, "content": "x",
		})
		if ok {
			t.Fatalf("path %q must be rejected", bad)
		}
		if !strings.Contains(errMsg, "invalid") && !strings.Contains(errMsg, "md") {
			t.Fatalf("path %q error = %q", bad, errMsg)
		}
	}
}

// 生效时机：写记忆后该智能体已存在的 DM 会话 loop 必须被重建（下一个 Run 生效）。
func TestHubMemoryWriteRebuildsLoop(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Live")
	agentID := string(p.ID)
	bs, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	before := bs.loop
	if before == nil {
		t.Fatal("DM session has no loop")
	}
	if ok, errMsg, _ := hubMemoryCmd(t, m, buf, 2, "hub_agent_memory_write", map[string]any{
		"agent_id": agentID, "path": "MEMORY.md", "content": "- 记住：用户叫小王\n",
	}); !ok {
		t.Fatalf("write failed: %s", errMsg)
	}
	if bs.loop == before {
		t.Fatal("memory write must rebuild the agent's loop (下一个 Run 生效)")
	}
	// 只比 bs.loop 指针挡不住「重建了但没 SetLoop」（评审 mutation C：删掉 SetLoop
	// 后 5 个 HubMemory 测试仍全绿）。会话才是「下一次 Run」的事实源。
	if got := bs.s.Loop(); got != bs.loop {
		t.Fatal("session still runs the OLD loop: 下一个 Run 看不到新记忆")
	}
	// 记忆真的进了注入文本（装配期算一次）；空记忆时同一函数返回空。
	inj := m.hubMemoryInjection(agentID, m.hubRuntime(agentID).path)
	if !strings.Contains(inj, "小王") {
		t.Fatalf("memory injection missing new content: %q", inj)
	}
	// 且会话将要用的那个 loop 里量的就是这段注入（内容一路到「下一次请求」）。
	bd := bs.s.Loop().ContextBreakdown(context.Background(), nil, nil)
	var memChars int64 = -1
	for _, part := range bd.Parts {
		if part.Key == agents.ContextPartMemory {
			memChars = part.Chars
		}
	}
	if memChars != int64(len(inj)) {
		t.Fatalf("next Run's memory part = %d chars, want injection length %d", memChars, len(inj))
	}
}

// 会话级 model/effort 优先于 Profile：写记忆触发的 loop 重建不能把人刚切的
// switch_model/switch_effort 静默改回默认值（P1-05 复核 B1）。
func TestHubMemoryWriteKeepsSessionModelAndEffort(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "Switch")
	agentID := string(p.ID)
	bs, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	if p.Model == "" || p.Effort != "" {
		t.Fatalf("test profile expectation changed: model=%q effort=%q", p.Model, p.Effort)
	}
	if got := bs.loop.Model(); got != p.Model {
		t.Fatalf("assembled loop model = %q, want profile %q", got, p.Model)
	}
	// 模拟 switch_model / switch_effort：只改会话状态（与命令做的事相同）。
	bs.model, bs.provider, bs.effort = "deepseek-v4-pro", "deepseek", "high"

	if ok, errMsg, _ := hubMemoryCmd(t, m, buf, 2, "hub_agent_memory_write", map[string]any{
		"agent_id": agentID, "path": "MEMORY.md", "content": "- 偏好：短句\n",
	}); !ok {
		t.Fatalf("write failed: %s", errMsg)
	}
	if got := bs.loop.Model(); got != "deepseek-v4-pro" {
		t.Fatalf("rebuilt loop model = %q, want session model deepseek-v4-pro (profile 不得覆盖会话选择)", got)
	}
	if got := bs.loop.ReasoningEffort(); got != provider.ReasoningEffortLevelHigh {
		t.Fatalf("rebuilt loop effort = %q, want high", got)
	}
	// 重建不得回写会话状态（否则面板与实际行为分叉）。
	if bs.model != "deepseek-v4-pro" || bs.provider != "deepseek" || bs.effort != "high" {
		t.Fatalf("session fields drifted: model=%q provider=%q effort=%q", bs.model, bs.provider, bs.effort)
	}
}
