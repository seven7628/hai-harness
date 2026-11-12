package session

// P5'（history 行的处置）的回归（2026-11-11）。
//
// 三条不变量：
//  1. **默认仍全写** —— `Store.AppendHistory` 是公开契约，零值不能变；
//  2. 开了 `WithHistoryLogTitleOnly()` 后**只写第一条**（标题来源）；
//  3. 恢复不受影响（尾扫最后一条 state 行）。

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// countRows 统计会话文件里各类行的条数。
func countRows(t *testing.T, p string) map[string]int {
	t.Helper()
	out := map[string]int{}
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var rec struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			continue
		}
		out[rec.Type]++
	}
	return out
}

// TestHistoryLogDefaultStillWritesEverything 零值必须保持 Store 契约：全写。
func TestHistoryLogDefaultStillWritesEverything(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := store.AppendHistory(context.Background(), "s1", []core.Message{{
			Role: core.User, Content: []core.Content{{Type: core.ContentTypeText, Content: "消息"}},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(dir, "s1.jsonl")
	rows := countRows(t, p)
	if rows["history"] != 5 {
		t.Fatalf("默认应写全部 history 行，实际 %d 条", rows["history"])
	}
}

// TestHistoryLogTitleOnly 只写第一条（标题来源），之后的 history 行不落盘。
func TestHistoryLogTitleOnly(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir, WithHistoryLogTitleOnly())
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		if err := store.AppendHistory(context.Background(), "s1", []core.Message{{
			Role: core.User, Content: []core.Content{{Type: core.ContentTypeText, Content: "第 " + string(rune('0'+i)) + " 条"}},
		}}); err != nil {
			t.Fatal(err)
		}
		// 每轮也落一条 state 行（真实形态）。
		if err := store.SaveState(context.Background(), "s1", &State{
			Messages: []core.Message{{Role: core.User, Content: []core.Content{{Type: core.ContentTypeText, Content: "x"}}}},
		}); err != nil {
			t.Fatal(err)
		}
	}
	p := filepath.Join(dir, "s1.jsonl")
	rows := countRows(t, p)
	if rows["history"] != 1 {
		t.Fatalf("应只写 1 条 history 行，实际 %d 条", rows["history"])
	}
	if rows["state"] != 5 {
		t.Fatalf("state 行应全写，实际 %d 条", rows["state"])
	}
	// 首行必须是 history 行且带用户文本 —— 会话标题的冷磁盘回退读它。
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	first := strings.SplitN(string(data), "\n", 2)[0]
	var rec struct {
		Type     string         `json:"type"`
		Messages []core.Message `json:"messages"`
	}
	if err := json.Unmarshal([]byte(first), &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Type != "history" {
		t.Fatalf("首行应是 history 行，实际 %q", rec.Type)
	}
	if len(rec.Messages) == 0 || rec.Messages[0].Role != core.User {
		t.Fatalf("首行应带用户消息（标题来源）: %+v", rec.Messages)
	}
	if c := rec.Messages[0].Content[0].Content; !strings.Contains(c, "第 0 条") {
		t.Fatalf("首行应是**第一条**消息，实际 %q", c)
	}
}

// TestHistoryLogTitleOnlyKeepsRecovery 恢复（尾扫最后一条 state 行）不受影响。
func TestHistoryLogTitleOnlyKeepsRecovery(t *testing.T) {
	dir := t.TempDir()
	store, err := NewFileStore(dir, WithHistoryLogTitleOnly())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := store.AppendHistory(ctx, "s1", []core.Message{{
		Role: core.User, Content: []core.Content{{Type: core.ContentTypeText, Content: "开场"}},
	}}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		msg := core.Message{Role: core.Assistant, Content: []core.Content{{Type: core.ContentTypeText, Content: "回答"}}}
		if err := store.AppendHistory(ctx, "s1", []core.Message{msg}); err != nil {
			t.Fatal(err)
		}
		if err := store.SaveState(ctx, "s1", &State{Messages: []core.Message{msg}}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := store.Load(ctx, "s1")
	if err != nil {
		t.Fatal(err)
	}
	if data == nil || data.State == nil {
		t.Fatal("恢复失败")
	}
	if len(data.State.Messages) != 1 {
		t.Fatalf("恢复出的消息数=%d want 1", len(data.State.Messages))
	}
}
