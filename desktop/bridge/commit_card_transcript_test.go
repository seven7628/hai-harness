package main

// 「窗口外卡片的转录」在**提交**后必须原样还在（2026-11-19，骨架级优化的安全网）。
//
// ## 为什么要有这条
//
// 骨架级优化把窗口外的卡片从 reducer 的堆里赶走了（只留头部）。风险是
// **提交路径**：reducer 的 blocks 是 checkpoint 的来源 ⇒ 如果提交时把「瘦卡片」
// 写回 sidecar，就会用「没有 items 的版本」覆盖完整转录 ⇒ **不可逆丢数据**。
//
// 本次实现的防线是：
//   · 卡片**不进** reducer 的 blocks ⇒ 不在提交�� spans 里 ⇒ 不会被重写；
//   · 卡片在 paged 的**引用全集**里 ⇒ 提交时 allIDs 含它 ⇒ 不被 pruneTo 删掉。
//
// 这条测试把这两条防线**端到端**验一遍：打开 → 提交 → 重读 sidecar ⇒
// 卡片的 stored 字节必须与提交前**逐字节相同**。

import (
	"bytes"
	"context"
	"testing"

	"github.com/seven7628/hai-harness/session"
)

// cardStoredBytes 取 paged 集合里某个 id 的**存储内容**（解压后的逻辑内容）。
func cardStoredBytes(t *testing.T, pb *PagedBlocks, id uint64) []byte {
	t.Helper()
	if pb == nil {
		t.Fatal("PagedBlocks 为 nil")
	}
	for i, got := range pb.IDs {
		if got != id {
			continue
		}
		if i >= len(pb.Spans) {
			t.Fatalf("paged 集合里 id=%d 的 span 缺失", id)
		}
		var out []byte
		if err := pb.Spans[i].LogicalTransient(func(raw []byte) error {
			out = bytes.Clone(raw)
			return nil
		}); err != nil {
			t.Fatalf("读 id=%d 的 stored 内容失败: %v", id, err)
		}
		return out
	}
	t.Fatalf("paged 集合里找不到 id=%d", id)
	return nil
}

// TestCommitKeepsWindowOutsideCardTranscript：提交不得改掉窗口外卡片的转录。
func TestCommitKeepsWindowOutsideCardTranscript(t *testing.T) {
	newManagerWithAppData(t, t.TempDir())
	store, err := newRecordStore()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	wsKey, sid := "ws", "s-card-commit"

	// 一个 agent 卡片（带 items = 转录正文）+ 几个 assistant 块。
	card := `{"kind":"agent","id":1,"run_id":"sub","label":"调查",` +
		`"items":[{"kind":"text","text":"转录正文-第一段"},{"kind":"text","text":"转录正文-第二段"}]}`
	raw := []byte(`{"schema_version":1,"blocks":[` + card + `,` +
		`{"kind":"assistant","id":2,"text":"m2"},` +
		`{"kind":"assistant","id":3,"text":"m3"},` +
		`{"kind":"assistant","id":4,"text":"m4"}],` +
		`"run_nodes":[],"turns":[],"usage":{},"todos":[]}`)
	if err := store.CommitRecord(ctx, session.NewSessionRecord(wsKey, sid, 1, 1, 0, session.State{}, raw)); err != nil {
		t.Fatal(err)
	}

	// ——— 打开（新路径：只解尾部，卡片在 paged 引用集里）———
	rec, err := store.LoadRecordForView(ctx, wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	cp, err := parseCheckpointFromRecordWindow(rec, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(cp.DetachedCards) != 1 {
		t.Fatalf("应有 1 张窗口外卡片的头，得 %d", len(cp.DetachedCards))
	}
	before := cardStoredBytes(t, cp.PagedBlocks, 1)
	if !bytes.Contains(before, []byte("转录正文-第二段")) {
		t.Fatalf("提交前的卡片内容里就没有转录正文，测试前提不成立: %s", before)
	}

	// ——— 提交（走真实的提交路径：reducer 的 body+spans → store）———
	red := NewViewReducer()
	if err := red.Restore(cp); err != nil {
		t.Fatal(err)
	}
	red.NormalizeForRestore()
	bs, err := red.MarshalBodyWithSpans()
	if err != nil {
		t.Fatal(err)
	}
	// 卡片的 id 必须在引用全集里（⇒ 不被 pruneTo 删），但**不能**在 spans 里
	// （⇒ 不会被重写）。
	foundInAll := false
	for _, id := range bs.AllIDs {
		if id == 1 {
			foundInAll = true
			break
		}
	}
	if !foundInAll {
		t.Fatalf("卡片的 id 不在引用全集里（⇒ 提交时会被 pruneTo 删掉）: %v", bs.AllIDs)
	}
	for _, id := range bs.IDs {
		if id == 1 {
			t.Fatalf("卡片出现在提交的 spans 里（⇒ 会被重写 ⇒ 转录被抹）")
		}
	}
	rev := uint64(2)
	record := session.NewSessionRecord(wsKey, sid, rev, red.LastSeq(), red.ClearGeneration(), session.State{}, bs.Body)
	record.BlockSpans = bs.Spans
	record.BlockIDs = bs.IDs
	record.AllBlockIDs = bs.AllIDs
	record.DirtyBlocks = bs.Dirty
	if err := store.CommitRecord(ctx, record); err != nil {
		t.Fatal(err)
	}

	// ——— 重读 sidecar：卡片的 stored 字节必须原样 ———
	rec2, err := store.LoadRecordForView(ctx, wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	defer rec2.ReleaseView()
	cp2, err := parseCheckpointFromRecordWindow(rec2, 2)
	if err != nil {
		t.Fatal(err)
	}
	after := cardStoredBytes(t, cp2.PagedBlocks, 1)
	if !bytes.Equal(before, after) {
		t.Fatalf("提交把窗口外卡片的转录改了：\n 提交前 %s\n 提交后 %s", before, after)
	}
	t.Logf("✅ 提交后卡片转录原样保留（%d 字节）：引用全集含它、spans 不含它 ⇒ 不会被重写也不会被删",
		len(after))
}
