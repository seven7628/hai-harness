package main

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/session"
)

// newManagerWithAppData 构造 manager 并固定 APP 级数据目录（模拟跨进程重启共享磁盘）。
// 与 testManager 不同：不重置 HOME/dataDir（同一测试内多次调用共享持久化）。
func newManagerWithAppData(t *testing.T, dataDir string) (*manager, *syncBuf) {
	t.Helper()
	// loadProviderConfig() 会把**真实** settings.json 的 provider[].prices 写进
	// 进程级 userPriceTable 且不还原。本 helper 不封闭 HOME（要复用本机 provider
	// 配置），因此必须自己隔离，否则用户钉的旧价会泄漏给后续按内置价断言的
	// metrics/reducer 测试（见 isolateUserPrices 注释里的实测）。
	assertNoUserPrices(t)
	isolateUserPrices(t)
	appDataDirOverride = dataDir
	t.Cleanup(func() { appDataDirOverride = "" })
	buf := &syncBuf{}
	m := &manager{
		workspaces: map[string]*workspaceRuntime{},
		provCfg:    loadProviderConfig(),
		out:        newOut(buf),
	}
	return m, buf
}

// waitRecord 轮询等待 record 落盘（commit 为异步 worker）。
func waitRecord(t *testing.T, wsKey, id string, timeout time.Duration) *session.SessionRecord {
	t.Helper()
	store, err := newRecordStore()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if rec, err := store.LoadRecord(context.Background(), wsKey, id); err == nil {
			return rec
		}
		time.Sleep(15 * time.Millisecond)
	}
	t.Fatalf("record %s/%s not committed within %v", wsKey, id, timeout)
	return nil
}

// TestCheckpointCommitOnBoundary 语义边界（llm_end）触发 record 原子落盘：
// revision/reducer_seq/view_checkpoint/sdk_state 均为有效值。
func TestCheckpointCommitOnBoundary(t *testing.T) {
	assertNoUserPrices(t)
	home := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("HOME", home)
	m, _ := newManagerWithAppData(t, dataDir)
	ws := t.TempDir()

	bs, err := m.create(ws, "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	if bs.coord == nil || bs.reducer == nil {
		t.Fatal("bridgeSession 应装配 reducer + coordinator")
	}
	ts := time.Now().Add(-time.Minute)
	bs.emit(&events.LLMEnd{
		RunId: "r1", RequestId: "q1", Model: "deepseek-v4-flash",
		Content: "ok", Usage: &core.Usage{Input: 100, Output: 20},
		FinishReason: core.FinishReasonStop, Timestamp: ts, EventType: events.LLMEndType,
	})
	// 非边界事件（内容流）不提交
	bs.emit(&events.ContentChunk{RunId: "r1", RequestId: "q1", Index: 0, Content: "x", Timestamp: ts, EventType: events.ContentChunkType})

	wsKey := workspaceKey(ws)
	rec := waitRecord(t, wsKey, "s1", 5*time.Second)
	if rec.Revision == 0 {
		t.Fatalf("revision should start at 1, got %d", rec.Revision)
	}
	if rec.ReducerSeq == 0 {
		t.Fatalf("reducer_seq should be > 0, got %d", rec.ReducerSeq)
	}
	cp, err := parseCheckpoint(rec.ViewCheckpoint)
	if err != nil {
		t.Fatalf("view_checkpoint 解析失败: %v", err)
	}
	if len(cp.Turns) != 1 || cp.Turns[0].Input != 100 || cp.Turns[0].Output != 20 {
		t.Fatalf("turn 未按 llm_end 结算: %+v", cp.Turns)
	}
	if cp.CostUSD <= 0 {
		t.Fatalf("cost 应按价表结算: %f", cp.CostUSD)
	}
	if rec.SDKState.UpdatedAt.IsZero() {
		t.Fatal("sdk_state 应带 updated_at")
	}
}

// TestCheckpointColdRestoreCheckpointFirst 磁盘有合法 record 时，new_session
// 冷恢复直接返回 view_checkpoint（restored=checkpoint），不再返回 legacy snapshot。
func TestCheckpointColdRestoreCheckpointFirst(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("HOME", home)
	ws := t.TempDir()

	m1, _ := newManagerWithAppData(t, dataDir)
	bs1, err := m1.create(ws, "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(-time.Minute)
	bs1.emit(&events.AgentStart{RunId: "root-1", Content: "任务", Model: "deepseek-v4-flash", Timestamp: ts, EventType: events.AgentStartType})
	bs1.emit(&events.ToolStart{RunId: "root-1", Id: "call-1", Name: "write_file", Arguments: `{"path":"a.txt"}`, Timestamp: ts.Add(time.Second), EventType: events.ToolRunStartType})
	bs1.emit(&events.ToolResponse{RunId: "root-1", Id: "call-1", Name: "write_file", Result: "ok", Timestamp: ts.Add(2 * time.Second), EventType: events.ToolRunEndType})
	waitRecord(t, workspaceKey(ws), "s1", 5*time.Second)

	// 模拟重启：新 manager 共享同一数据目录
	m2, buf2 := newManagerWithAppData(t, dataDir)
	m2.dispatch(cmd(0, "new_session", map[string]any{"workspace": ws, "id": "s1", "activate": true}))
	var resp map[string]any
	for _, l := range readLines(buf2) {
		if l["event_type"] == "command_response" && l["id"] == float64(0) {
			resp = l
			break
		}
	}
	if resp == nil {
		t.Fatal("new_session 无响应")
	}
	data, _ := resp["data"].(map[string]any)
	if data["restored"] != "checkpoint" {
		t.Fatalf("restored = %v, want checkpoint", data["restored"])
	}
	if _, ok := data["snapshot"]; ok {
		t.Fatal("checkpoint 路径不应带 legacy snapshot.events")
	}
	raw, _ := data["view_checkpoint"].(json.RawMessage)
	if len(raw) == 0 {
		raw, _ = json.Marshal(data["view_checkpoint"])
	}
	var cp Checkpoint
	if err := json.Unmarshal(raw, &cp); err != nil {
		t.Fatalf("view_checkpoint 解析失败: %v", err)
	}
	if len(cp.Blocks) < 1 || cp.Blocks[0].Kind != "tool" || cp.Blocks[0].Status != "done" {
		t.Fatalf("checkpoint 未含收敛的工具块: %+v", cp.Blocks)
	}
	// 冷恢复时若有 running 主 run → NormalizeForRestore 追加 shutdown divider（横线隔离）
	for _, b := range cp.Blocks {
		if b.Kind == "divider" && b.DividerReason != "shutdown" {
			t.Fatalf("divider reason 应为 shutdown: %+v", b)
		}
	}
	// 进程内 reducer 也被 hydrate（后续事件继续累积）
	bs2 := m2.get(ws, "s1")
	if bs2 == nil || bs2.reducer == nil {
		t.Fatal("重启后 reducer 应被 hydrate")
	}
	if v := data["revision"]; v != float64(1) && v != float64(2) {
		t.Fatalf("revision = %v, want >= 1", v)
	}
}

// TestCheckpointDeleteSessionRemovesRecord delete_session 连 record + .bak 一并删除，
// 且异步 commit 不会在删除后把 record 写回。
func TestCheckpointDeleteSessionRemovesRecord(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("HOME", home)
	m, buf := newManagerWithAppData(t, dataDir)
	ws := t.TempDir()

	bs, err := m.create(ws, "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	bs.emit(&events.LLMEnd{RunId: "r1", RequestId: "q1", Model: "deepseek-v4-flash", Usage: &core.Usage{Input: 10}, FinishReason: core.FinishReasonStop, Timestamp: time.Now(), EventType: events.LLMEndType})
	wsKey := workspaceKey(ws)
	waitRecord(t, wsKey, "s1", 5*time.Second)

	m.dispatch(cmd(1, "delete_session", map[string]any{"workspace": ws, "session_id": "s1"}))
	if !lastRespOK(t, buf, 1) {
		t.Fatal("delete_session 失败")
	}
	sessDir := filepath.Join(dataDir, "sessions", wsKey)
	// 等待异步 commit 稳定后仍应不存在（stopSession 已等待 coordinator drain）
	time.Sleep(100 * time.Millisecond)
	for _, name := range []string{"s1.record.json", "s1.record.json.bak", "s1.jsonl"} {
		if _, err := os.Stat(filepath.Join(sessDir, name)); !os.IsNotExist(err) {
			t.Fatalf("删除后 %s 仍存在（err=%v）", name, err)
		}
	}
}

// TestCheckpointClearGenerationCommitted clear 命中的 commit 带 clear_generation=1，
// checkpoint 只保留清空提示块；lifetime 对账值保留。
func TestCheckpointClearGenerationCommitted(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("HOME", home)
	m, _ := newManagerWithAppData(t, dataDir)
	ws := t.TempDir()

	bs, err := m.create(ws, "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Add(-time.Minute)
	bs.emit(&events.LLMEnd{RunId: "r1", RequestId: "q1", Model: "deepseek-v4-flash", Usage: &core.Usage{Input: 100}, FinishReason: core.FinishReasonStop, Timestamp: ts, EventType: events.LLMEndType})
	bs.emit(&events.CommandResult{Name: "clear", Result: "context cleared", Timestamp: ts.Add(time.Second), EventType: events.CommandResultType})

	wsKey := workspaceKey(ws)
	// 等两个 commit：llm_end 边界（gen=0）与 clear 边界（gen=1）；轮询直到目标代际。
	store, err := newRecordStore()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	var rec *session.SessionRecord
	for time.Now().Before(deadline) {
		if r, lerr := store.LoadRecord(context.Background(), wsKey, "s1"); lerr == nil && r.ClearGeneration == 1 {
			rec = r
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if rec == nil {
		t.Fatalf("clear 边界 commit 未落盘（clear_generation=1）")
	}
	if rec.ClearGeneration != 1 {
		t.Fatalf("clear_generation = %d, want 1", rec.ClearGeneration)
	}
	cp, err := parseCheckpoint(rec.ViewCheckpoint)
	if err != nil {
		t.Fatal(err)
	}
	if len(cp.Blocks) != 1 || cp.Blocks[0].Kind != "divider" || cp.Blocks[0].DividerReason != "clear" {
		t.Fatalf("clear 后 checkpoint 应为 divider(clear) 横线: %+v", cp.Blocks)
	}
	if len(cp.Turns) != 0 {
		t.Fatalf("clear 后 turns 应清空")
	}
	if cp.Usage.Input != 0 {
		t.Fatalf("clear 后展示 usage 应归零: %+v", cp.Usage)
	}
	if cp.LifetimeUsage.Input != 100 {
		t.Fatalf("clear 后 lifetime 对账值应保留: %+v", cp.LifetimeUsage)
	}
}

// TestCheckpointRestoreIgnoresSubagentJournals 重启边界：主 session 从 record 恢复时
// 不读取/不删除 <sid>/agents journal；checkpoint 中未完成任务摘要归一化为 abandoned；
// journal 文件与主 record 各自独立（namespace 隔离，docs §3.1.2）。
func TestCheckpointRestoreIgnoresSubagentJournals(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("HOME", home)
	ws := t.TempDir()
	wsKey := workspaceKey(ws)
	timestamp := time.Now().Add(-time.Hour)

	// 构造含 running 子 agent / async_task / run 摘要的 checkpoint（crash 前最后提交）
	cp := &Checkpoint{
		SchemaVersion: checkpointSchemaVersion,
		Blocks: []Block{
			{Kind: blockKindAgent, ID: 1, RunID: "sub-1", Label: "调查", Status: statusRunning, SpawnedAt: timestamp.UnixMilli(), TaskID: "task-abc123fg", Items: []AgentItem{}},
			{Kind: blockKindAsyncTask, ID: 2, TaskID: "task-hjk34mn", Label: "后台", Status: statusRunning, StartedAt: timestamp.UnixMilli()},
		},
		RunNodes: []RunNode{{ID: "root-1", Label: "主会话", Status: statusRunning, Depth: 0, Kind: runNodeKindRun}},
		Turns:    []Turn{},
		Usage:    Usage{},
		Todos:    []TodoItem{},
	}
	raw, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	// newManagerWithAppData 设置 appDataDirOverride（必须先于 newRecordStore）
	m, _ := newManagerWithAppData(t, dataDir)
	_ = m
	store, err := newRecordStore()
	if err != nil {
		t.Fatal(err)
	}
	rec := session.NewSessionRecord(wsKey, "s1", 1, 5, 0, session.State{}, raw)
	if err := store.CommitRecord(context.Background(), rec); err != nil {
		t.Fatal(err)
	}

	// 伪造该会话的 SubAgent journal（<sid>/agents namespace）
	jp, err := journalPathFor(filepath.Join(dataDir, "sessions"), "s1", "task-abc123fg")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(jp, []byte(`{"type":"start","task_id":"task-abc123fg","ts":"2026-08-26T10:00:00Z"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// 再次冷恢复（新 manager 实例；record 已在磁盘）
	m2, _ := newManagerWithAppData(t, dataDir)
	bs, err := m2.create(ws, "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	if bs.reducer == nil {
		t.Fatal("reducer 应被 hydrate")
	}
	got := bs.reducer.Snapshot()
	if len(got.Blocks) != 3 {
		t.Fatalf("blocks = %d, want 3（2 任务块 + shutdown divider）", len(got.Blocks))
	}
	byKind := map[string]Block{}
	for _, b := range got.Blocks {
		byKind[b.Kind] = b
	}
	// 顶层 run running → 冷恢复追加 shutdown divider（横线隔离）
	if dv, ok := byKind[blockKindDivider]; !ok || dv.DividerReason != "shutdown" {
		t.Fatalf("冷恢复应追加 shutdown divider: %+v", byKind[blockKindDivider])
	}
	if byKind[blockKindAgent].Status != statusAbandoned {
		t.Fatalf("子 agent 重启后应为 abandoned: %+v", byKind[blockKindAgent])
	}
	if byKind[blockKindAsyncTask].Status != statusAbandoned {
		t.Fatalf("async_task 重启后应为 abandoned: %+v", byKind[blockKindAsyncTask])
	}
	if len(got.RunNodes) != 1 || got.RunNodes[0].Status != statusDone {
		t.Fatalf("run 节点重启后应为 done: %+v", got.RunNodes)
	}
	// journal 存在且未被主恢复消费/删除
	if _, err := os.Stat(jp); err != nil {
		t.Fatalf("SubAgent journal 不应被主恢复删除: %v", err)
	}
	// 主 record 独立可读（revision 基线不受 journal 影响）
	loaded, err := store.LoadRecord(context.Background(), wsKey, "s1")
	if err != nil {
		t.Fatalf("主 record 应可读: %v", err)
	}
	if loaded.Revision != 1 {
		t.Fatalf("revision = %d, want 1", loaded.Revision)
	}
}

// TestCheckpointPendingInRecordAndRecoveryPriority #1/#2：sdk_state.pending 被填充；
// 冷恢复时 record.sdk_state（含 pending）作为 SDK 恢复源，积压重新入队。
func TestCheckpointPendingInRecordAndRecoveryPriority(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("HOME", home)
	ws := t.TempDir()
	wsKey := workspaceKey(ws)

	m1, _ := newManagerWithAppData(t, dataDir)
	bs1, err := m1.create(ws, "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	// 注入一条未消费的 inbox 消息（模拟用户输入积压）
	if err := bs1.s.Ask(context.Background(), core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "pending-msg"})); err != nil {
		t.Fatal(err)
	}
	bs1.emit(&events.LLMEnd{RunId: "r1", RequestId: "q1", Model: "deepseek-v4-flash", Usage: &core.Usage{Input: 10}, FinishReason: core.FinishReasonStop, Timestamp: time.Now(), EventType: events.LLMEndType})
	rec := waitRecord(t, wsKey, "s1", 5*time.Second)
	if len(rec.SDKState.Pending) != 1 || rec.SDKState.Pending[0].Content[0].Content != "pending-msg" {
		t.Fatalf("sdk_state.pending 应含积压输入: %+v", rec.SDKState.Pending)
	}

	// 模拟重启：新 manager 冷恢复 → Session 从 record.sdk_state 恢复（含 pending 入队）
	m2, _ := newManagerWithAppData(t, dataDir)
	bs2, err := m2.create(ws, "s1", "")
	if err != nil {
		t.Fatal(err)
	}
	got := bs2.s.PendingSnapshot()
	if len(got) != 1 || got[0].Content[0].Content != "pending-msg" {
		t.Fatalf("重启后 pending 应重新入队: %+v", got)
	}
	// record.sdk_state.messages 在测试场景中为空（llm_end 直接 emit 未进 Session 上下文）；
	// 关键断言是 pending 走 record 恢复（legacy FileStore 此时无 state 行，pending 只有
	// record 能提供——若走了 legacy 分支，pending 必为空）。
	// 补充：验证恢复分支确实使用 record（todo/usage 等字段一致）
	if bs2.s.Cost().Total != 0 {
		t.Fatalf("成本应从 record.sdk_state 恢复: %+v", bs2.s.Cost())
	}
}

// TestCheckpointNoRecordFallsBackToLegacy 无 record（新会话/旧数据）时 new_session
// 保持 legacy snapshot 路径（现状不破坏）。
func TestCheckpointNoRecordFallsBackToLegacy(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("HOME", home)
	m, buf := newManagerWithAppData(t, dataDir)
	ws := t.TempDir()

	bs, err := m.create(ws, "s2", "")
	if err != nil {
		t.Fatal(err)
	}
	bs.emit(&events.AgentStart{RunId: "r1", Content: "任务", Timestamp: time.Now(), EventType: events.AgentStartType})

	// 无 record：deleteSession 场景之外的普通新会话（record 只在边界提交后产生；
	// AgentStart 非边界 → 不落盘）
	if _, err := newRecordStore(); err != nil {
		t.Fatal(err)
	}
	m.dispatch(cmd(0, "new_session", map[string]any{"workspace": ws, "id": "s2"}))
	var resp map[string]any
	for _, l := range readLines(buf) {
		if l["event_type"] == "command_response" && l["id"] == float64(0) {
			resp = l
			break
		}
	}
	data, _ := resp["data"].(map[string]any)
	if data["restored"] != "snapshot" {
		t.Fatalf("无 record 时应回退 legacy snapshot，got %v", data["restored"])
	}
}
