package session

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// TestLoadRecordForViewCarriesKinds 映射路径必须带回每块的 kind，
// 否则窗口恢复无法在不全量解码的前提下捞出卡片块。
func TestLoadRecordForViewCarriesKinds(t *testing.T) {
	store, err := NewFileStorage(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	wsKey, sid := "ws", "s1"
	blocks := []string{
		`{"kind":"assistant","id":1,"text":"a"}`,
		`{"kind":"agent","id":2,"run_id":"sub"}`,
		`{"kind":"async_task","id":3,"task_id":"t"}`,
		`{"kind":"divider","id":4}`,
	}
	raw := []byte(fmt.Sprintf(`{"schema_version":1,"blocks":[%s],"run_nodes":[],"turns":[],"usage":{},"todos":[]}`,
		joinStrings(blocks)))
	if err := store.CommitRecord(context.Background(), NewSessionRecord(wsKey, sid, 1, 1, 0, State{}, raw)); err != nil {
		t.Fatal(err)
	}
	rec, err := store.LoadRecordForView(context.Background(), wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.ReleaseView()
	if len(rec.LoadedBlockKinds) != len(blocks) {
		t.Fatalf("kinds=%d want %d", len(rec.LoadedBlockKinds), len(blocks))
	}
	want := []uint8{blockKindCodeAssistant, blockKindCodeAgent, blockKindCodeAsyncTask, blockKindCodeDivider}
	for i, w := range want {
		if rec.LoadedBlockKinds[i] != w {
			t.Fatalf("块 %d kind want %d got %d", i, w, rec.LoadedBlockKinds[i])
		}
	}
	// 卡片类判定可直接用于「窗口外也要解码」的筛选
	if !hasCardKind(rec.LoadedBlockKinds[1]) || !hasCardKind(rec.LoadedBlockKinds[2]) {
		t.Fatal("agent/async_task 应判为卡片类")
	}
	if hasCardKind(rec.LoadedBlockKinds[0]) || hasCardKind(rec.LoadedBlockKinds[3]) {
		t.Fatal("assistant/divider 不应判为卡片类")
	}
	// BodyView 是引用形态（blocks 是对象，不是数组）—— 正是窗口恢复的输入形态。
	if !strings.Contains(string(rec.BodyView), "$blocks_ref") {
		t.Fatal("BodyView 应保持 sidecar 引用形态")
	}
}

func joinStrings(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ","
		}
		out += p
	}
	return out
}
