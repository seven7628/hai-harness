package main

import (
	"context"
	"testing"

	"github.com/seven7628/hai-harness/session"
)

// TestParseCheckpointFromRecordWindow 窗口解析的三条不变量：
// 只解码尾部窗口 + 窗口外的卡片块；其余进 PagedBlocks；总数是历史真实值。
func TestParseCheckpointFromRecordWindow(t *testing.T) {
	newManagerWithAppData(t, t.TempDir())
	store, err := newRecordStore()
	if err != nil {
		t.Fatal(err)
	}
	wsKey, sid := "ws", "s-window"
	raw := []byte(`{"schema_version":1,"blocks":[` +
		`{"kind":"agent","id":1,"run_id":"sub","label":"调查"},` +
		`{"kind":"assistant","id":2,"text":"m2"},` +
		`{"kind":"assistant","id":3,"text":"m3"},` +
		`{"kind":"assistant","id":4,"text":"m4"},` +
		`{"kind":"assistant","id":5,"text":"m5"},` +
		`{"kind":"assistant","id":6,"text":"m6"}],` +
		`"run_nodes":[],"turns":[],"usage":{},"todos":[]}`)
	if err := store.CommitRecord(context.Background(), session.NewSessionRecord(wsKey, sid, 1, 1, 0, session.State{}, raw)); err != nil {
		t.Fatal(err)
	}
	rec, err := store.LoadRecordForView(context.Background(), wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.ReleaseView()

	cp, err := parseCheckpointFromRecordWindow(rec, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(cp.Blocks) != 3 {
		t.Fatalf("应解码 3 块（窗口 2 + 卡片 1），得 %d", len(cp.Blocks))
	}
	if cp.Blocks[0].ID != 1 || cp.Blocks[0].Kind != blockKindAgent {
		t.Fatalf("卡片块应排在最前: %+v", cp.Blocks[0])
	}
	if cp.Blocks[1].ID != 5 || cp.Blocks[2].ID != 6 {
		t.Fatalf("窗口块应为 5,6，得 %d,%d", cp.Blocks[1].ID, cp.Blocks[2].ID)
	}
	if cp.BlocksTotal != 6 {
		t.Fatalf("BlocksTotal 应为 6，得 %d", cp.BlocksTotal)
	}
	if cp.PagedBlocks == nil || len(cp.PagedBlocks.IDs) != 3 {
		t.Fatalf("未解码块应为 3 个（2,3,4），得 %+v", cp.PagedBlocks)
	}
	want := []uint64{2, 3, 4}
	for i, w := range want {
		if cp.PagedBlocks.IDs[i] != w {
			t.Fatalf("分页 id 错位: want %v got %v", want, cp.PagedBlocks.IDs)
		}
	}
	cp2, err := parseCheckpointFromRecordWindow(rec, 99)
	if err != nil {
		t.Fatal(err)
	}
	if cp2.PagedBlocks != nil || len(cp2.Blocks) != 6 {
		t.Fatalf("limit 覆盖全部时应完整解析，得 blocks=%d paged=%v", len(cp2.Blocks), cp2.PagedBlocks != nil)
	}
}
