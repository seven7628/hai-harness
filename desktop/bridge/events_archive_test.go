package main

// events 日志 gzip 封存的行为测试（2026-11-19，爬 4）。
//
// 核心不变量：**封存不改变任何读侧看到的内容**。
// ⇒ 端到端验：封存前后的 snapshot（buildSnapshotFromPath）必须**逐字节相同**
// —— 那条路径同时被「会话详情页恢复」与「cron 快照归档」复用。

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/eventslog"
)

// archiveTestEvents 造一批事件行（含 llm_end / tool_run_end / agent_start）。
func archiveTestEvents() string {
	var sb strings.Builder
	sb.WriteString(`{"session_id":"s-1","event_type":"agent_start","run_id":"r1","index":0,"timestamp":"2026-01-01T00:00:00Z"}` + "\n")
	sb.WriteString(`{"session_id":"s-1","event_type":"llm_end","run_id":"r1","content":"回答正文","reasoning":"思考","tool_calls":[],"finish_reason":"stop","usage":{}}` + "\n")
	for i := 0; i < 50; i++ {
		sb.WriteString(`{"session_id":"s-1","event_type":"tool_run_end","run_id":"r1","id":"c` + itoa(i) + `","name":"bash","result":"输出` + strings.Repeat("内容", i+1) + `","is_error":false}` + "\n")
	}
	sb.WriteString(`{"session_id":"s-1","event_type":"agent_end","run_id":"r1","content":"收尾","finish_reason":"stop"}` + "\n")
	return sb.String()
}

// TestArchiveEventsFilesKeepsSnapshotIdentical：封存前后 snapshot 逐字节相同。
func TestArchiveEventsFilesKeepsSnapshotIdentical(t *testing.T) {
	root := t.TempDir()
	wsDir := filepath.Join(root, "ws-a")
	if err := os.MkdirAll(wsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	evPath := filepath.Join(wsDir, "s-1.jsonl")
	content := archiveTestEvents()
	if err := os.WriteFile(evPath, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// 老文件（40 天前）⇒ 该被封存
	old := time.Now().Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(evPath, old, old); err != nil {
		t.Fatal(err)
	}

	snapBefore, okBefore := buildSnapshotFromPath(evPath)
	if !okBefore {
		t.Fatal("封存前读不出 snapshot")
	}
	beforeJSON, _ := json.Marshal(snapBefore)

	res, err := archiveEventsFiles(root, 30*24*time.Hour, 0, 0)
	if err != nil {
		t.Fatalf("封存失败: %v", err)
	}
	if res.Files != 1 {
		t.Fatalf("应封存 1 个文件，得 %d（跳过 %d 放弃 %d）", res.Files, res.Skipped, res.Aborted)
	}
	if _, err := os.Stat(evPath); !os.IsNotExist(err) {
		t.Fatalf("明文还在")
	}
	if _, err := os.Stat(evPath + eventslog.GzSuffix); err != nil {
		t.Fatalf("gz 没生成: %v", err)
	}

	// 封存后：**同一个路径**（没有 .gz 后缀）读出来的 snapshot 必须一样
	snapAfter, okAfter := buildSnapshotFromPath(evPath)
	if !okAfter {
		t.Fatal("封存后读不出 snapshot（读侧没走透明解压）")
	}
	afterJSON, _ := json.Marshal(snapAfter)
	if string(beforeJSON) != string(afterJSON) {
		t.Fatalf("封存前后 snapshot 不同：\n前 %s\n后 %s", beforeJSON, afterJSON)
	}
	t.Logf("✅ 封存前后 snapshot 逐字节相同（%d 字节）；%.1fKB → %.1fKB",
		len(beforeJSON), float64(len(content))/1e3, float64(res.After)/1e3)
}

// TestArchiveEventsFilesSkipsRecent：太新的不封存。
func TestArchiveEventsFilesSkipsRecent(t *testing.T) {
	root := t.TempDir()
	wsDir := filepath.Join(root, "ws-a")
	_ = os.MkdirAll(wsDir, 0o700)
	evPath := filepath.Join(wsDir, "s-2.jsonl")
	if err := os.WriteFile(evPath, []byte(archiveTestEvents()), 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := archiveEventsFiles(root, 30*24*time.Hour, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 0 {
		t.Fatalf("新文件不该被封存: %d", res.Files)
	}
	if _, err := os.Stat(evPath); err != nil {
		t.Fatalf("明文被误删: %v", err)
	}
}

// TestArchiveEventsFilesSkipsWhenGzExists：已有 .gz ⇒ 明文是更新的那份，不碰。
func TestArchiveEventsFilesSkipsWhenGzExists(t *testing.T) {
	root := t.TempDir()
	wsDir := filepath.Join(root, "ws-a")
	_ = os.MkdirAll(wsDir, 0o700)
	evPath := filepath.Join(wsDir, "s-3.jsonl")
	if err := os.WriteFile(evPath, []byte(archiveTestEvents()), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(evPath+eventslog.GzSuffix, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-40 * 24 * time.Hour)
	_ = os.Chtimes(evPath, old, old)

	res, err := archiveEventsFiles(root, 30*24*time.Hour, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if res.Files != 0 {
		t.Fatalf("两者都在时不该封存明文: %d", res.Files)
	}
	if res.Skipped != 1 {
		t.Fatalf("应记 1 个跳过，得 %d", res.Skipped)
	}
}

// TestLogEventLineRestoresArchivedLog：封存的会话被重新激活 ⇒ 事件**追加到还原后的明文**。
//
// 这是「事件流被劈成两半」那个坑的防线：
// 若不还原，logEventLine 会 O_CREATE 出一个**空的** .jsonl ⇒ 旧事件留在 .gz 里、
// 新事件进了空文件 ⇒ trace 面板只看得到后半截。
func TestLogEventLineRestoresArchivedLog(t *testing.T) {
	dir := t.TempDir()
	evPath := filepath.Join(dir, "s-9.jsonl")
	orig := `{"session_id":"s-9","event_type":"agent_start","run_id":"r1"}` + "\n"
	if err := os.WriteFile(evPath, []byte(orig), 0o600); err != nil {
		t.Fatal(err)
	}
	// 封存掉
	ok, err := eventslog.Archive(evPath, eventslog.DefaultLevel)
	if err != nil || !ok {
		t.Fatalf("封存失败: ok=%v err=%v", ok, err)
	}
	if _, err := os.Stat(evPath); !os.IsNotExist(err) {
		t.Fatalf("封存后明文还在")
	}

	// 重新激活：写一条新事件
	bs := &bridgeSession{id: "s-9", eventsDir: dir}
	bs.logEventLine([]byte(`{"session_id":"s-9","event_type":"agent_end","run_id":"r1"}`))
	if bs.eventsF != nil {
		_ = bs.eventsF.Close()
	}

	// 明文应被还原，且**含新旧两条**
	got, err := os.ReadFile(evPath)
	if err != nil {
		t.Fatalf("明文没被还原: %v", err)
	}
	s := string(got)
	if !strings.Contains(s, "agent_start") {
		t.Fatalf("旧事件丢了（没还原）: %s", s)
	}
	if !strings.Contains(s, "agent_end") {
		t.Fatalf("新事件没写进去: %s", s)
	}
	// .gz 应被删（两者都在 = 占双份空间）
	if _, err := os.Stat(evPath + eventslog.GzSuffix); !os.IsNotExist(err) {
		t.Fatalf("gz 没被删")
	}
	t.Logf("✅ 封存的会话重新激活后：旧事件 + 新事件都在明文里（%d 字节）", len(got))
}
