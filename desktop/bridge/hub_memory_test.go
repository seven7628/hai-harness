package main

// P1-05：记忆隔离 + 浏览/编辑（记忆面板命令）。

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
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
	// 记忆真的进了注入文本（装配期算一次）；空记忆时同一函数返回空。
	if inj := m.hubMemoryInjection(agentID, m.hubRuntime(agentID).path); !strings.Contains(inj, "小王") {
		t.Fatalf("memory injection missing new content: %q", inj)
	}
}
