package main

// 惰性解压（P5）的端到端不变式。
//
// ## 这条测试在守什么
//
// 压缩块在磁盘上是 zlib 字节，而块摘要（payloadDigest）算的是**逻辑内容**。
// 于是「磁盘字节」与「逻辑字节」必须分开记（session.BlockSpan）：
//
//   - 记混了 → 摘要校验必错（拿压缩字节去比逻辑摘要）；
//   - 提前解压 → 每开一个会话白付 **259MB**（实测 20 会话 5.06GB，占堆 64%），
//     而首屏只要 500 块 —— 97% 的解压是白付。
//
// 所以这里同时验三件事：
//
//  1. 读回的 span **确实是压缩的**（惰性路径真的在跑，不是碰巧没压上）；
//  2. 窗口解码出来的内容**与原文逐字节相同**（解压对了）；
//  3. 摘要校验**按逻辑内容算**（没报错就是过了 —— 拿压缩字节比必错）。
//
// 反向的回归（最该怕的那种）：把 session.BlockSpan.Logical() 改成直接返回
// Raw，本测试第 1、2 条会红。

import (
	"context"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/session"
)

// bigText 造一段「可压缩且够大」的正文（>512B 才会走压缩路径）。
func bigText(n int) string {
	return strings.Repeat("惰性解压要省的是没被读的那部分。", n)
}

// TestLazyDecompressKeepsContentIdentical 见文件头的说明。
func TestLazyDecompressKeepsContentIdentical(t *testing.T) {
	newManagerWithAppData(t, t.TempDir())
	store, err := newRecordStore()
	if err != nil {
		t.Fatal(err)
	}
	wsKey, sid := "ws", "s-lazy"
	text := bigText(200)
	raw := []byte(`{"schema_version":1,"blocks":[` +
		`{"kind":"assistant","id":1,"text":"` + text + `"},` +
		`{"kind":"assistant","id":2,"text":"短"}],` +
		`"run_nodes":[],"turns":[],"usage":{},"todos":[]}`)
	if err := store.CommitRecord(context.Background(), session.NewSessionRecord(wsKey, sid, 1, 1, 0, session.State{}, raw)); err != nil {
		t.Fatal(err)
	}

	rec, err := store.LoadRecordForView(context.Background(), wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.ReleaseView()

	if len(rec.LoadedBlockSpans) != 2 {
		t.Fatalf("spans=%d want 2", len(rec.LoadedBlockSpans))
	}
	// ① 大块应是压缩的（惰性路径在跑），小块不压缩（太小）。
	if !rec.LoadedBlockSpans[0].Compressed {
		t.Fatal("大块的 span 应标记 Compressed（惰性解压路径没生效？）")
	}
	if rec.LoadedBlockSpans[1].Compressed {
		t.Fatal("小块不该压缩（<512B）")
	}
	// Raw 是**磁盘字节**（压缩块）⇒ 比原文短。
	if len(rec.LoadedBlockSpans[0].Raw) >= len(text) {
		t.Fatalf("压缩块的 Raw 应小于原文：raw=%d 原文=%d", len(rec.LoadedBlockSpans[0].Raw), len(text))
	}
	// ② Logical() 必须还原出原文。
	logical, lerr := rec.LoadedBlockSpans[0].Logical()
	if lerr != nil {
		t.Fatal(lerr)
	}
	if !strings.Contains(string(logical), text[:40]) {
		t.Fatal("Logical() 没还原出原文")
	}

	// ③ 窗口解码：内容一致 + 摘要校验通过（不报错即通过）。
	cp, err := parseCheckpointFromRecordWindow(rec, 2)
	if err != nil {
		t.Fatalf("窗口解码失败: %v", err)
	}
	if len(cp.Blocks) != 2 {
		t.Fatalf("块数=%d want 2", len(cp.Blocks))
	}
	if cp.Blocks[0].Text != text {
		t.Fatalf("解码出的正文与原文不一致（%d vs %d 字节）", len(cp.Blocks[0].Text), len(text))
	}
	if cp.Blocks[1].Text != "短" {
		t.Fatalf("小块内容不对: %q", cp.Blocks[1].Text)
	}
}

// TestLazyDecompressPagedPage 分页路径也必须解压正确。
//
// 窗口外的块留在 PagedBlocks 里（**不解码**）⇒ 翻历史时才解压那一页。
// 这条测的是「翻到的那一页内容对不对」。
func TestLazyDecompressPagedPage(t *testing.T) {
	newManagerWithAppData(t, t.TempDir())
	store, err := newRecordStore()
	if err != nil {
		t.Fatal(err)
	}
	wsKey, sid := "ws", "s-lazy-page"
	text := bigText(200)
	var b strings.Builder
	b.WriteString(`{"schema_version":1,"blocks":[`)
	for i := 1; i <= 6; i++ {
		if i > 1 {
			b.WriteString(",")
		}
		txt := text
		if i == 6 {
			txt = "尾块"
		}
		b.WriteString(`{"kind":"assistant","id":` + itoa(i) + `,"text":"` + txt + `"}`)
	}
	b.WriteString(`],"run_nodes":[],"turns":[],"usage":{},"todos":[]}`)
	if err := store.CommitRecord(context.Background(), session.NewSessionRecord(wsKey, sid, 1, 1, 0, session.State{}, []byte(b.String()))); err != nil {
		t.Fatal(err)
	}

	rec, err := store.LoadRecordForView(context.Background(), wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	defer rec.ReleaseView()

	// 窗口只留 1 块 ⇒ 其余 5 块进 PagedBlocks（未解码）。
	cp, err := parseCheckpointFromRecordWindow(rec, 1)
	if err != nil {
		t.Fatal(err)
	}
	if cp.PagedBlocks == nil || len(cp.PagedBlocks.Spans) != 5 {
		t.Fatalf("分页块数不对: %+v", cp.PagedBlocks)
	}
	// 分页块里的压缩字节在翻页时才解压。
	compressed := 0
	for _, sp := range cp.PagedBlocks.Spans {
		if sp.Compressed {
			compressed++
		}
	}
	if compressed != 5 {
		t.Fatalf("5 个分页块都应是压缩的，实际 %d", compressed)
	}

	red := NewViewReducer()
	if err := red.Restore(cp); err != nil {
		t.Fatal(err)
	}
	// ① 游标在**窗口之前**（= 窗口首块 id）⇒ 走分页解码（解那一页的压缩块）。
	blocks, cursor, hasMore, total, err := red.ReadBlockPage(6, 2)
	if err != nil {
		t.Fatalf("分页读取失败: %v", err)
	}
	if len(blocks) != 2 || !hasMore {
		t.Fatalf("分页结果不对: blocks=%d hasMore=%v", len(blocks), hasMore)
	}
	if total != 6 {
		t.Fatalf("总数=%d want 6", total)
	}
	if cursor == 0 {
		t.Fatal("游标不应为 0")
	}
	if !strings.Contains(string(blocks[0]), "惰性解压") {
		t.Fatalf("分页内容不对: %s", truncate(string(blocks[0]), 80))
	}

	// ② 游标为 0（=「最新一页」）⇒ 取内存窗口的尾巴，**不许越界**。
	// 回归：窗口恢复时 total 含未加载的历史（6）而 view 只有 1 块 ⇒
	// 老代码 `view[start:end]` 用 total 当下标会 panic（2026-11-13 修）。
	latest, _, hasMore2, _, err := red.ReadBlockPage(0, 50)
	if err != nil {
		t.Fatalf("最新一页读取失败: %v", err)
	}
	if len(latest) != 1 {
		t.Fatalf("最新一页应返回窗口那 1 块，得 %d", len(latest))
	}
	if !hasMore2 {
		t.Fatal("窗口之外还有历史块 ⇒ has_more 应为 true")
	}
}
