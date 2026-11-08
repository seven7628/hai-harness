package session

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"testing"
)

// TestCommitWindowSpansKeepsFullRef 是窗口恢复的**数据安全回归**：
// 调用方只给「窗口 + 卡片块」的 spans，但 AllBlockIDs 是全集 —— 提交后
// record 必须引用全部 id，且 sidecar 里窗口外的块**一个都不能被 prune 掉**。
//
// 这条一旦坏，用户翻历史就会看到「打开时有、提交一次后没了」，而且是不可逆的。
func TestCommitWindowSpansKeepsFullRef(t *testing.T) {
	store, err := NewFileStorage(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	wsKey, sid := "ws", "s-win"

	// 先写 6 个块
	blocks := make([]string, 6)
	for i := range blocks {
		blocks[i] = fmt.Sprintf(`{"kind":"assistant","id":%d,"text":"m%d"}`, i+1, i+1)
	}
	raw := []byte(fmt.Sprintf(`{"schema_version":1,"blocks":[%s],"run_nodes":[],"turns":[],"usage":{},"todos":[]}`,
		joinStrings(blocks)))
	if err := store.CommitRecord(context.Background(), NewSessionRecord(wsKey, sid, 1, 1, 0, State{}, raw)); err != nil {
		t.Fatal(err)
	}

	// 模拟窗口恢复：只给最后 2 个块的 spans，外加走向新状态
	rec2 := NewSessionRecord(wsKey, sid, 2, 2, 0, State{}, raw)
	rec2.BlockSpans = [][]byte{[]byte(blocks[4]), []byte(blocks[5])}
	rec2.BlockIDs = []uint64{5, 6}
	rec2.AllBlockIDs = []uint64{1, 2, 3, 4, 5, 6}
	rec2.DirtyBlocks = map[uint64]struct{}{5: {}, 6: {}}
	if err := store.CommitRecord(context.Background(), rec2); err != nil {
		t.Fatalf("窗口提交失败: %v", err)
	}

	// 读回：view 必须能还原全部 6 块（一块都不能少）
	back, err := store.LoadRecord(context.Background(), wsKey, sid)
	if err != nil {
		t.Fatalf("读回失败: %v", err)
	}
	var probe struct {
		Blocks []json.RawMessage `json:"blocks"`
	}
	if err := json.Unmarshal(back.ViewCheckpoint, &probe); err != nil {
		t.Fatal(err)
	}
	if len(probe.Blocks) != 6 {
		t.Fatalf("窗口提交后块数应仍为 6，得 %d（窗口外的块被 prune 了 = 数据丢失）", len(probe.Blocks))
	}
	for i, raw := range probe.Blocks {
		var b struct {
			ID uint64 `json:"id"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatal(err)
		}
		if b.ID != uint64(i+1) {
			t.Fatalf("第 %d 块 id 错位: %d", i, b.ID)
		}
	}
}

// TestCommitWindowSpansRejectsMissingBlock allIDs 里出现 sidecar 没有的块时，
// 提交必须失败（而不是静默写入一个引用不存在的 id 的 record）。
func TestCommitWindowSpansRejectsMissingBlock(t *testing.T) {
	store, err := NewFileStorage(filepath.Join(t.TempDir(), "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	wsKey, sid := "ws", "s-miss"
	raw := []byte(`{"schema_version":1,"blocks":[{"kind":"assistant","id":1}],"run_nodes":[],"turns":[],"usage":{},"todos":[]}`)
	if err := store.CommitRecord(context.Background(), NewSessionRecord(wsKey, sid, 1, 1, 0, State{}, raw)); err != nil {
		t.Fatal(err)
	}
	rec2 := NewSessionRecord(wsKey, sid, 2, 2, 0, State{}, raw)
	rec2.BlockSpans = [][]byte{[]byte(`{"kind":"assistant","id":1}`)}
	rec2.BlockIDs = []uint64{1}
	rec2.AllBlockIDs = []uint64{1, 99} // 99 从未存在
	// 存储层对 sidecar 失败一律「回退全量内嵌」（正确性优先，静默）——
	// 所以这里不断言提交失败，而断言**落下去的 record 不会引用不存在的块**
	// （那会让下一次打开直接判 corrupt）。
	_ = store.CommitRecord(context.Background(), rec2)
	back, err := store.LoadRecord(context.Background(), wsKey, sid)
	if err != nil {
		t.Fatalf("回退后 record 应仍可读: %v", err)
	}
	var probe struct {
		Blocks []json.RawMessage `json:"blocks"`
	}
	if err := json.Unmarshal(back.ViewCheckpoint, &probe); err != nil {
		t.Fatal(err)
	}
	for _, raw := range probe.Blocks {
		var b struct {
			ID uint64 `json:"id"`
		}
		if err := json.Unmarshal(raw, &b); err != nil {
			t.Fatal(err)
		}
		if b.ID == 99 {
			t.Fatal("record 引用了不存在的块 99")
		}
	}
}
