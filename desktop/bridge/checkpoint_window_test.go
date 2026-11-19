package main

import (
	"context"
	"testing"

	"github.com/seven7628/hai-harness/session"
)

// TestParseCheckpointFromRecordWindow 窗口解析的不变量（2026-11-19 骨架级优化后更新）。
//
// ## 语义变化（有意为之）
//
// 改造前：窗口 = 尾部 limit 块 + **窗口外的全部卡片块**，**全部解码**
//
//	（卡片正文因此进堆；实测某会话 2017 张卡片 = 119.9MB，而对话尾部只有 1.7MB）。
//
// 改造后：窗口 = 尾部 limit 块（只解这些）+ 窗口外卡片的**头部**
//
//	（正文不进内存；卡片留在 paged 的**引用全集**里 ⇒ 提交时不被 pruneTo 删掉，
//	  但**不进页集** ⇒ UI 的翻页结果里没有卡片）。
//
// 四条不变量：
//  1. cp.Blocks = 尾部窗口（只有它被解码）；
//  2. cp.DetachedCards = 窗口外卡片的头部（items 只留条数）；
//  3. cp.PagedBlocks.IDs = 窗口外的**全部**未解码块（含卡片 ⇒ 引用全集）；
//     cp.PagedBlocks.PageIDs = 只有非卡片块（UI 翻页用）；
//  4. cp.BlocksTotal = 历史真实总数。
//
// 交付等价性（agents 字节逐字节相同）由 detach_agents_equivalence_test.go 守着。
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

	// ① 只解码尾部窗口（2 块：5,6）
	if len(cp.Blocks) != 2 {
		t.Fatalf("应只解码尾部 2 块，得 %d", len(cp.Blocks))
	}
	if cp.Blocks[0].ID != 5 || cp.Blocks[1].ID != 6 {
		t.Fatalf("窗口块应为 5,6，得 %d,%d", cp.Blocks[0].ID, cp.Blocks[1].ID)
	}

	// ② 窗口外的卡片以「头部」形式单独带出
	if len(cp.DetachedCards) != 1 {
		t.Fatalf("窗口外卡片头应为 1 张，得 %d", len(cp.DetachedCards))
	}
	if cp.DetachedCards[0].ID != 1 || cp.DetachedCards[0].Kind != blockKindAgent {
		t.Fatalf("卡片头不对: %+v", cp.DetachedCards[0])
	}
	if cp.DetachedCards[0].Label != "调查" {
		t.Fatalf("卡片头的头部字段丢了: %+v", cp.DetachedCards[0])
	}

	// ③ 历史总数
	if cp.BlocksTotal != 6 {
		t.Fatalf("BlocksTotal 应为 6，得 %d", cp.BlocksTotal)
	}

	// ④ paged 分两栏：引用全集（含卡片）/ 页集（不含卡片）
	if cp.PagedBlocks == nil {
		t.Fatal("PagedBlocks 不应为 nil")
	}
	if len(cp.PagedBlocks.IDs) != 4 {
		t.Fatalf("引用全集应为 4 个（1,2,3,4），得 %+v", cp.PagedBlocks.IDs)
	}
	wantAll := []uint64{1, 2, 3, 4}
	for i, w := range wantAll {
		if cp.PagedBlocks.IDs[i] != w {
			t.Fatalf("引用全集 id 错位: want %v got %v", wantAll, cp.PagedBlocks.IDs)
		}
	}
	if len(cp.PagedBlocks.PageIDs) != 3 {
		t.Fatalf("页集应为 3 个（2,3,4），得 %+v", cp.PagedBlocks.PageIDs)
	}
	wantPage := []uint64{2, 3, 4}
	for i, w := range wantPage {
		if cp.PagedBlocks.PageIDs[i] != w {
			t.Fatalf("页集 id 错位: want %v got %v", wantPage, cp.PagedBlocks.PageIDs)
		}
	}

	// limit 覆盖全部 ⇒ 走完整解析（无 paged、无卡片头）
	cp2, err := parseCheckpointFromRecordWindow(rec, 99)
	if err != nil {
		t.Fatal(err)
	}
	if cp2.PagedBlocks != nil || len(cp2.Blocks) != 6 {
		t.Fatalf("limit 覆盖全部时应完整解析，得 blocks=%d paged=%v", len(cp2.Blocks), cp2.PagedBlocks != nil)
	}
}
