package session

// 脏块快路径（1a）：只重写调用方标脏的块，不再对每个块做 sha256。
//
// 这是「单次提交 1.3s → 0.4s」里最大的一刀：30,755 块 / 185MB 的全量哈希
// 占 sidecar 阶段的主体。安全网是调用方周期性传 nil 走全量比对（bridge 每
// 32 次提交一次，以及失败/首次提交后一次）。

import (
	"context"
	"strings"
	"testing"
)

func TestDirtyBlocksSkipHashing(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionRecordStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	ws, sid := "ws-dirty", "s-dirty"
	ctx := context.Background()
	b1, b2 := testBlock(1, "one"), testBlock(2, "two")
	view := testView(b1, b2)

	spans := testSpans(t, view)
	commit := func(rev uint64, dirty map[uint64]struct{}) CommitPhaseTiming {
		t.Helper()
		r := NewSessionRecord(ws, sid, rev, rev, 0, State{}, view)
		r.BlockSpans = spans
		r.DirtyBlocks = dirty
		ct, err := store.CommitRecordTraced(ctx, r)
		if err != nil {
			t.Fatalf("提交失败: %v", err)
		}
		return ct
	}

	// 第一次：空库，写出两块（没有可比对的内容，因此不哈希）。
	if ct := commit(1, nil); ct.BlocksWrote != 2 {
		t.Fatalf("首次提交应写 2 块，实际 %d", ct.BlocksWrote)
	}
	// 第二次：全量路径（nil），两块都要哈希比对。
	ct := commit(2, nil)
	if ct.HashedBytes == 0 {
		t.Fatal("全量路径没有哈希任何 payload")
	}
	// 第三次：都已知未变 → 不哈希、不写块。
	ct = commit(3, map[uint64]struct{}{})
	if ct.HashedBytes != 0 {
		t.Fatalf("脏块快路径仍在哈希: %d 字节", ct.HashedBytes)
	}
	if ct.BlocksWrote != 0 {
		t.Fatalf("没有变化却写了 %d 个块", ct.BlocksWrote)
	}
	// 第四次：周期校验（nil）必须重新哈希。
	ct = commit(4, nil)
	if ct.HashedBytes == 0 {
		t.Fatal("周期性全量校验没有发生")
	}
}

func TestDirtyBlocksRewriteChangedBlock(t *testing.T) {
	root := t.TempDir()
	store, err := NewSessionRecordStorage(root)
	if err != nil {
		t.Fatal(err)
	}
	ws, sid := "ws-dirty2", "s-dirty2"
	ctx := context.Background()

	v1 := testView(testBlock(1, "one"), testBlock(2, "two"))
	if err := store.CommitRecord(ctx, NewSessionRecord(ws, sid, 1, 1, 0, State{}, v1)); err != nil {
		t.Fatal(err)
	}
	// 只有块 2 变了 → 标脏它，块 1 不标。
	v2 := testView(testBlock(1, "one"), testBlock(2, "two CHANGED"))
	r := NewSessionRecord(ws, sid, 2, 2, 0, State{}, v2)
	r.BlockSpans = testSpans(t, v2)
	r.DirtyBlocks = map[uint64]struct{}{2: {}}
	ct, err := store.CommitRecordTraced(ctx, r)
	if err != nil {
		t.Fatal(err)
	}
	if ct.BlocksWrote != 1 {
		t.Fatalf("应当只写 1 个块，实际 %d", ct.BlocksWrote)
	}
	if ct.HashedBytes != 0 {
		t.Fatalf("脏块快路径不应哈希，实际 %d 字节", ct.HashedBytes)
	}
	// 读回来必须是新内容。
	got, err := store.LoadRecord(ctx, ws, sid)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got.ViewCheckpoint), "two CHANGED") {
		t.Fatalf("脏块内容没有落盘: %s", truncateForLog(got.ViewCheckpoint))
	}
}

// testSpans 从 view 里切出每个 block 的字节（等价于 reducer 的 spans）。
func testSpans(t *testing.T, view []byte) [][]byte {
	t.Helper()
	raw, ok := splitViewBlocks(view)
	if !ok {
		t.Fatalf("view 没有 blocks 字段")
	}
	spans, ok := splitBlocksArray(raw)
	if !ok {
		t.Fatalf("blocks 不是数组")
	}
	return spans
}
