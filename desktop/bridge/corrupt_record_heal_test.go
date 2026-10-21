package main

// record 损坏时的自愈（2026-10-21 用户报障：刷新后会话与左栏子智能体全没了）。
//
// 事故链条（三段，任何一段单独存在都不致命，串起来就是数据"消失"）：
//   1. reducer 的 blocks 集合会变小（normalize 丢弃空壳 assistant 块 / 新 run 清 error
//      块 / 清空上下文），而 blocks sidecar 是 append-only —— 被丢弃的块留在索引里；
//   2. 读取端把「$blocks_ref.count == 索引块数」当完整性信号 → 整条 record 判 corrupt
//      → create 的 reducer 是空的；
//   3. 打开会话只看 LoadRecordMetadata（只读头部，永远成功）→ 直接回一份 **0 块**
//      checkpoint → UI 显示空白会话 + 左栏树空。
//
// 修复分三处：写端裁剪落删除记录（session/block_store.go pruneTo，另一条测试钉住）、
// 打开路径改走事件重放自愈、空 reducer 期间禁止提交。这里钉住后两条 —— 用一份
// **真正损坏**的 record（sidecar 里多出一个不被引用的块）走完整个 new_session 命令。

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"os"
	"path/filepath"
	"testing"

	"github.com/seven7628/hai-harness/session"
)

// appendStaleBlock 往 sidecar 末尾追加一条「不再被引用」的块记录（制造 count mismatch）。
func appendStaleBlock(t *testing.T, path, id string, payload []byte) {
	t.Helper()
	buf := make([]byte, 0, 32+len(payload))
	buf = append(buf, 1) // blockRecPut
	var n [4]byte
	binary.BigEndian.PutUint32(n[:], uint32(len(id)))
	buf = append(buf, n[:]...)
	buf = append(buf, id...)
	binary.BigEndian.PutUint32(n[:], uint32(len(payload)))
	buf = append(buf, n[:]...)
	buf = append(buf, payload...)
	sum := sha256.Sum256(payload)
	buf = append(buf, sum[:8]...)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := f.Write(buf); err != nil {
		t.Fatal(err)
	}
}

func TestCorruptRecordHealsFromEventLog(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	sid := "s-heal-1"
	wsKey := workspaceKey(ws)
	ctx := context.Background()

	// ① 先落一份合法 record（否则连"损坏"都无从谈起）
	store, err := session.NewSessionRecordStorage(recordRoot())
	if err != nil {
		t.Fatal(err)
	}
	view := session.ViewCheckpoint(`{"schema_version":1,"blocks":[` +
		`{"id":1,"kind":"user","text":"旧消息"},` +
		`{"id":2,"kind":"assistant","text":"旧回复"}` +
		`],"run_nodes":[],"turns":[],"usage":{},"cost_usd":0,"todos":[],"degraded":false}`)
	rec := session.NewSessionRecord(wsKey, sid, 1, 10, 0, session.State{}, view)
	if err := store.CommitRecord(ctx, rec); err != nil {
		t.Fatal(err)
	}
	// ② 制造现场：sidecar 里多出一个不被引用的块（等价于「块集合变小过」）
	path, err := store.RecordPath(wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	appendStaleBlock(t, path+".blocks", "999", []byte(`{"id":999,"kind":"assistant","text":"陈旧"}`))
	if _, err := store.LoadRecord(ctx, wsKey, sid); err == nil {
		t.Fatal("前置条件不成立：这份 record 应当读不出来（否则测的不是自愈路径）")
	}

	// ③ 事件日志是完整历史源（自愈靠它重放）
	// 目录用 wsKey 拼：t.TempDir() 在 macOS 上是 /var/…（真身 /private/var/…），
	// wsEventsDir 会解析软链，这里直接用同一个 key 对齐，避免写到另一条路径上。
	evDir := filepath.Join(appDataDir(), "events", wsKey)
	if err := os.MkdirAll(evDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// 事件日志里的 timestamp 是 RFC3339 字符串（events 包的字段类型是 time.Time），
	// 不是发给前端的 epoch 毫秒 —— 写成数字会被 parseLegacyEvent 整行跳过。
	const t0 = "2026-10-21T00:00:00.000000+08:00"
	lines := []string{
		`{"event_type":"user_inputs_consumed","session_id":"s-heal-1","timestamp":"` + t0 + `","user_texts":["帮我查一下"]}`,
		`{"event_type":"agent_start","session_id":"s-heal-1","timestamp":"` + t0 + `","run_id":"main-1","index":0,"content":"主任务","model":"m"}`,
		`{"event_type":"llm_start","session_id":"s-heal-1","timestamp":"` + t0 + `","run_id":"main-1","request_id":"r1","model":"m"}`,
		`{"event_type":"llm_end","session_id":"s-heal-1","timestamp":"` + t0 + `","run_id":"main-1","request_id":"r1","content":"旧回复","model":"m","usage":{"input":5,"output":7}}`,
		`{"event_type":"agent_start","session_id":"s-heal-1","timestamp":"` + t0 + `","run_id":"sub-1","parent_run_id":"main-1","depth":1,"task_id":"task-1","name":"历史子智能体","content":"历史子智能体"}`,
		`{"event_type":"llm_start","session_id":"s-heal-1","timestamp":"` + t0 + `","run_id":"sub-1","request_id":"r2","model":"m"}`,
		`{"event_type":"llm_end","session_id":"s-heal-1","timestamp":"` + t0 + `","run_id":"sub-1","request_id":"r2","content":"子结论","model":"m","usage":{"input":1,"output":2}}`,
		`{"event_type":"agent_end","session_id":"s-heal-1","timestamp":"` + t0 + `","run_id":"sub-1","status":"completed"}`,
	}
	body := ""
	for _, l := range lines {
		body += l + "\n"
	}
	if err := os.WriteFile(filepath.Join(evDir, sid+".jsonl"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	// ④ 打开会话（带 blocks_limit：同时验「窗口外卡片单独带回」没被自愈路径丢掉）
	before := len(readLines(buf))
	m.dispatch(cmd(1, "new_session", map[string]any{"workspace": ws, "id": sid, "blocks_limit": 2}))
	resp := lastOK(t, readLines(buf)[before:])
	data, _ := resp["data"].(map[string]any)
	if data == nil {
		t.Fatalf("new_session 无 data: %v", resp)
	}
	if got, _ := data["restored"].(string); got != "checkpoint" {
		t.Fatalf("自愈后应回 checkpoint（实际 %q，说明走了别的路径）", got)
	}
	cpDoc, _ := data["view_checkpoint"].(map[string]any)
	if cpDoc == nil {
		t.Fatal("缺 view_checkpoint")
	}
	blocks, _ := cpDoc["blocks"].([]any)
	if len(blocks) == 0 {
		t.Fatal("record 损坏时必须从事件日志重建出视图，不能回 0 块（用户看到的就是「全没了」）")
	}
	// 子智能体卡片必须在响应里活着：要么在 blocks 首屏里，要么由 agents 字段带回
	//（两种形态取决于它落在窗口内还是窗口外，投影规则本身由
	//  TestMarshalTailCarriesAgentsOutsideWindow 单独钉住）。
	agents, _ := data["agents"].([]any)
	foundAgent := false
	for _, a := range agents {
		am, _ := a.(map[string]any)
		if am["kind"] == "agent" && am["task_id"] == "task-1" {
			foundAgent = true
		}
	}
	for _, b := range blocks {
		bm, _ := b.(map[string]any)
		if bm["kind"] == "agent" && bm["task_id"] == "task-1" {
			foundAgent = true
		}
	}
	if !foundAgent {
		t.Fatalf("重放出来的子智能体卡片丢了（blocks=%d 张，agents=%d 张）—— 左栏树就靠它",
			len(blocks), len(agents))
	}

	// ⑤ 自愈必须落盘成一份自洽的 record：下次打开不该再进这条路径
	for _, l := range readLines(buf) {
		if l["event_type"] == "session_persist_error" {
			t.Logf("持久化错误: %v", l["error"])
		}
	}
	if _, err := store.LoadRecord(ctx, wsKey, sid); err != nil {
		t.Fatalf("自愈后 record 必须可读（否则每次打开都要重放一遍）: %v", err)
	}

	// 收尾：停掉会话（避免后台 goroutine 影响 TempDir 清理）
	if bs := m.get(ws, sid); bs != nil {
		_ = bs.s.Shutdown(ctx)
		bs.cancel()
	}
}

// lastOK 取最后一条 ok=true 的响应行。
func lastOK(t *testing.T, lines []map[string]any) map[string]any {
	t.Helper()
	for i := len(lines) - 1; i >= 0; i-- {
		if lines[i]["ok"] == true {
			return lines[i]
		}
	}
	if len(lines) > 0 {
		t.Fatalf("响应都不 ok: %v", lines[len(lines)-1])
	}
	t.Fatal("没有收到响应")
	return nil
}
