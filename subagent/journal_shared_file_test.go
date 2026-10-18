package subagent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestJournalReadersIgnoreSessionStateRows 会话级恢复引入的共存契约：`<taskId>.jsonl`
// 同时承载 journal（start/turn/result）与会话级状态（history/state 行）。journal 读侧
// 必须只认前者 —— 否则 state 行携带的**完整上下文**会污染 TaskOutput 输出与
// total_lines/进展判据。
func TestJournalReadersIgnoreSessionStateRows(t *testing.T) {
	p := filepath.Join(t.TempDir(), "task-abc123fg.jsonl")
	// 文件序模拟真实交错：journal 行与会话级状态行混排（两个写者同文件追加）。
	lines := []string{
		`{"type":"start","task_id":"task-abc123fg","name":"n"}`,
		`{"type":"history","messages":[{"role":"user","content":[{"type":"text","content":"hi"}]}]}`,
		`{"type":"turn","task_id":"task-abc123fg","round":1,"text":"进展"}`,
		`{"type":"state","state":{"messages":[{"role":"user","content":[{"type":"text","content":"巨大上下文"}]}]}}`,
		`{"type":"result","task_id":"task-abc123fg","status":"completed","text":"结论"}`,
	}
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(l + "\n")
	}
	if err := os.WriteFile(p, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	// ReadTail：只返回 journal 记录（3 条），不掺 history/state。
	tail, err := ReadTail(p, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(tail.Records) != 3 {
		t.Fatalf("ReadTail 应只认 journal 记录（3 条），实际 %d 条: %+v", len(tail.Records), tail.Records)
	}
	for _, r := range tail.Records {
		if !journalRecordType(r.Type) {
			t.Fatalf("ReadTail 掺入非 journal 记录: %+v", r)
		}
	}
	if tail.Total != 3 {
		t.Fatalf("total_lines 应只计 journal 记录（3），实际 %d", tail.Total)
	}

	// ReadAll：同样只认 journal 记录（恢复重建交接简报用）。
	all, err := ReadAll(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("ReadAll 应只认 journal 记录（3 条），实际 %d 条: %+v", len(all), all)
	}
}
