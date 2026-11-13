package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/subagent"
)

// —— Phase 4：bridge journal 路径接线与清理钩子（docs/SUBAGENT_OUTPUT_JOURNAL_TASKOUTPUT.md §3.6 / §6 P4）——
// 路径不变量（§4 验收红线）：journal 收敛在 sessions/<wsKey>/<sid>/agents/ 命名空间内，
// 不污染主 Session jsonl，不出现在 listSessions 枚举，删除会话连带清理。

// TestJournalPathForLayout 路径布局：<storeDir>/<sid>/agents/<taskId>.jsonl，父目录自动创建。
func TestJournalPathForLayout(t *testing.T) {
	store := t.TempDir()
	p, err := journalPathFor(store, "s-123", "task-abc123fg")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(store, "s-123", "agents", "task-abc123fg.jsonl")
	if p != want {
		t.Fatalf("path = %q, want %q", p, want)
	}
	if fi, err := os.Stat(filepath.Dir(p)); err != nil || !fi.IsDir() {
		t.Fatalf("parent dir not created: %v", err)
	}
}

// TestJournalPathForRejectsBadTaskID 防穿越双保险之二（其一在 SDK ValidTaskID 同规则）。
func TestJournalPathForRejectsBadTaskID(t *testing.T) {
	for _, bad := range []string{"../x", "a/b", "tooltask-1", "", "task-", "s-1", "task-1234567", "task-abcdefghij"} {
		if _, err := journalPathFor(t.TempDir(), "s-1", bad); err == nil {
			t.Errorf("task id %q 应被拒绝", bad)
		}
	}
}

// TestJournalPathForRejectsBadSessionID sid 含路径穿越符时拒绝（防 <sid>/agents 逃逸）。
func TestJournalPathForRejectsBadSessionID(t *testing.T) {
	for _, bad := range []string{"../x", "a/b", "a\\b", "", ".."} {
		if _, err := journalPathFor(t.TempDir(), bad, "task-abc123fg"); err == nil {
			t.Errorf("session id %q 应被拒绝", bad)
		}
	}
}

// TestRemoveSubagentJournals 删除目标会话的 journal 目录；其他会话不受影响；幂等。
func TestRemoveSubagentJournals(t *testing.T) {
	store := t.TempDir()
	for _, sid := range []string{"s-a", "s-b"} {
		p, err := journalPathFor(store, sid, "task-abc123fg")
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte("{\"type\":\"start\"}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	removeSubagentJournals(store, "s-a")
	if _, err := os.Stat(filepath.Join(store, "s-a", "agents")); !os.IsNotExist(err) {
		t.Fatalf("s-a journal dir should be gone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(store, "s-b", "agents", "task-abc123fg.jsonl")); err != nil {
		t.Fatalf("s-b journal must survive: %v", err)
	}
	removeSubagentJournals(store, "s-a") // 幂等
}

// TestDeleteSessionRemovesJournalDir delete_session 钩子：删会话连代清理 <sid>/agents（A7）。
func TestDeleteSessionRemovesJournalDir(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	sid := newSessionID(t, m, buf, ws)
	rt := m.runtime(ws)

	// 伪造该会话的 journal 文件
	jp, err := journalPathFor(rt.storeDir, sid, "task-abc123fg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jp, []byte("{\"type\":\"start\"}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	m.dispatch(cmd(0, "delete_session", map[string]any{"workspace": ws, "session_id": sid}))

	if _, err := os.Stat(filepath.Join(rt.storeDir, sid, "agents")); !os.IsNotExist(err) {
		t.Fatalf("<sid>/agents should be removed after delete_session: %v", err)
	}
	if _, err := os.Stat(filepath.Join(rt.storeDir, sid+".jsonl")); !os.IsNotExist(err) {
		t.Fatalf("session file should be removed: %v", err)
	}
}

// TestListSessionsIgnoresSubagentDir 会话枚举不因 journal 目录/文件出现幻影条目（A8）。
func TestListSessionsIgnoresSubagentDir(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	sid := newSessionID(t, m, buf, ws)
	rt := m.runtime(ws)

	// journal 目录内塞伪会话文件 + 另一个真实冷磁盘会话作对照
	sub := filepath.Join(rt.storeDir, sid, "agents")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "task-abc123fg.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rt.storeDir, "s-cold.jsonl"), []byte("{}\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	list := m.listSessions(ws)
	ids := map[string]bool{}
	for _, item := range list {
		if id, ok := item["id"].(string); ok {
			ids[id] = true
		}
	}
	if ids[sid] && !liveSession(m, ws, sid) {
		// 活跃会话本身会出现（in-memory 分支），此处只断言「无幻影」：
		_ = ids // 占位，真实断言在下方
	}
	if ids["task-abc123fg"] {
		t.Fatalf("journal 文件不得被列为会话: %v", ids)
	}
	if len(list) < 2 { // sid(活跃) + s-cold(冷磁盘)
		t.Fatalf("应含活跃会话与冷磁盘会话: %v", ids)
	}
	if !ids["s-cold"] {
		t.Fatalf("冷磁盘会话丢失: %v", ids)
	}
}

func liveSession(m *manager, ws, sid string) bool {
	rt := m.runtime(ws)
	rt.mu.Lock()
	defer rt.mu.Unlock()
	_, ok := rt.sessions[sid]
	return ok
}

// TestSubagentsBaseNaming 命名空间约定：<sid>/agents 子目录（与 record 平级、listSessions 跳过）。
func TestSubagentsBaseNaming(t *testing.T) {
	got := subagentsBase("/store", "s-1")
	if !strings.HasSuffix(got, "/s-1/agents") && !strings.HasSuffix(got, "\\s-1\\agents") {
		t.Fatalf("base = %q, want suffix <sid>/agents", got)
	}
}

// TestSubagentStateStoreWritesSessionFormat 会话级恢复的落盘契约：宿主注入的
// subagent.StateStore 适配器把子任务状态写进 `<sid>/agents/<taskId>.jsonl`，且**格式与
// 主会话逐字段同构**（history / state 行，复用 session.FileStore）。这是「子智能体的文件
// 日志与主智能体一样，只是目录不同」的装配断言。
func TestSubagentStateStoreWritesSessionFormat(t *testing.T) {
	ws := t.TempDir()
	storeDir := wsSessionsDir(ws)
	const sid, taskID = "s-state", "task-abc123fg"

	st := subagentStateStore(ws, sid)
	if !st.Enabled() {
		t.Fatal("适配器应三个方法齐备（Enabled=真）")
	}

	msgs := []core.Message{
		core.NewUserMessage(core.Content{Type: "text", Content: "原始任务"}),
		core.NewAssistantMessage(nil, "", nil),
	}
	ctx := context.Background()
	if err := st.AppendHistory(ctx, taskID, msgs); err != nil {
		t.Fatal(err)
	}
	if err := st.Save(ctx, taskID, &subagent.RunState{
		Messages: msgs,
		Usage:    core.Usage{Input: 11, Output: 7, Cost: core.Cost{Total: 123}},
	}); err != nil {
		t.Fatal(err)
	}

	// 路径：<storeDir>/<sid>/agents/<taskId>.jsonl（与 journal 同目录同文件）。
	p := filepath.Join(storeDir, sid, "agents", taskID+".jsonl")
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("状态应落在 %s: %v", p, err)
	}
	// 格式与主会话同构：出现 history 与 state 两种记录类型行。
	if !strings.Contains(string(raw), `"type":"history"`) || !strings.Contains(string(raw), `"type":"state"`) {
		t.Fatalf("应为会话级 jsonl（history+state 行）:\n%s", raw)
	}

	// Load 读回：上下文与用量逐字段一致（恢复路径据此续跑）。
	got, err := st.Load(ctx, taskID)
	if err != nil || got == nil {
		t.Fatalf("Load: %v %+v", err, got)
	}
	if len(got.Messages) != len(msgs) || got.Messages[0].Content[0].Content != "原始任务" {
		t.Fatalf("Messages 不一致: %+v", got.Messages)
	}
	if got.Usage.Input != 11 || got.Usage.Output != 7 || got.Usage.Cost.Total != 123 {
		t.Fatalf("Usage 不一致: %+v", got.Usage)
	}

	// 未知任务 → (nil, nil)：调用方按「无历史可续」处理，不是错误。
	if st2, err := st.Load(ctx, "task-zzzzzzzz"); err != nil || st2 != nil {
		t.Fatalf("未知任务应返回 (nil,nil)，got %+v err=%v", st2, err)
	}
	// 与 journal 同文件：journal 路径解析器指向同一路径。
	jp, err := journalPathFor(storeDir, sid, taskID)
	if err != nil {
		t.Fatal(err)
	}
	if jp != p {
		t.Fatalf("状态文件应与 journal 同路径：%s vs %s", p, jp)
	}
}
