package main

import (
	"encoding/json"
	"strconv"
	"testing"
)

// 主对话分页投影（2026-10-20）的不变量测试。
//
// 这些测试钉住「不能因为按需加载导致历史消息看不到」这条红线：
// 首屏只给尾部 N 块，但**逐页向前翻必须能拿回全部块，且逐字节一致、不重不漏**。

// buildPagingReducer 造一个确定性 reducer：n 个 assistant 块 + 少量 turns/diffs。
func buildPagingReducer(t *testing.T, n int) *ViewReducer {
	t.Helper()
	r := NewViewReducer()
	cp := &Checkpoint{SchemaVersion: checkpointSchemaVersion}
	for i := 1; i <= n; i++ {
		cp.Blocks = append(cp.Blocks, Block{
			Kind: blockKindAssistant,
			ID:   uint64(i),
			Text: "内容" + strconv.Itoa(i),
		})
	}
	for i := 0; i < n; i++ {
		cp.Turns = append(cp.Turns, Turn{RequestID: "req-" + strconv.Itoa(i), Output: int64(i)})
	}
	if err := r.Restore(cp); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestMarshalTailIsSuffixOfFull 尾部投影必须是全量 blocks 的精确后缀。
func TestMarshalTailIsSuffixOfFull(t *testing.T) {
	r := buildPagingReducer(t, 500)

	full, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	pv, err := r.MarshalTail(50)
	if err != nil {
		t.Fatal(err)
	}

	var fullDoc, tailDoc struct {
		Blocks []json.RawMessage `json:"blocks"`
	}
	if err := json.Unmarshal(full, &fullDoc); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(pv.View, &tailDoc); err != nil {
		t.Fatal(err)
	}
	if len(tailDoc.Blocks) != 50 {
		t.Fatalf("应返回 50 块，实际 %d", len(tailDoc.Blocks))
	}
	off := len(fullDoc.Blocks) - 50
	for i := range tailDoc.Blocks {
		if string(tailDoc.Blocks[i]) != string(fullDoc.Blocks[off+i]) {
			t.Fatalf("第 %d 块与全量不一致（必须逐字节相同）", i)
		}
	}
	if !pv.HasMore || pv.Total != 500 {
		t.Fatalf("游标态不符: hasMore=%v total=%d", pv.HasMore, pv.Total)
	}
}

// TestPagingRecoversEveryBlock 逐页向前翻必须覆盖全部块，不重不漏。
//
// 这是「加载更早」的正确性底线：少一块就是历史丢失，多一块就是重复消息。
func TestPagingRecoversEveryBlock(t *testing.T) {
	const n = 1000
	r := buildPagingReducer(t, n)

	pv, err := r.MarshalTail(100)
	if err != nil {
		t.Fatal(err)
	}
	var first struct {
		Blocks       []json.RawMessage `json:"blocks"`
		BlocksCursor uint64            `json:"blocks_cursor"`
	}
	if err := json.Unmarshal(pv.View, &first); err != nil {
		t.Fatal(err)
	}

	seen := map[uint64]bool{}
	idOf := func(raw []byte) uint64 {
		var h struct {
			ID uint64 `json:"id"`
		}
		_ = json.Unmarshal(raw, &h)
		return h.ID
	}
	for _, b := range first.Blocks {
		seen[idOf(b)] = true
	}

	cur := first.BlocksCursor
	pages := 0
	for {
		blks, next, more, total, err := r.ReadBlockPage(cur, 100)
		if err != nil {
			t.Fatal(err)
		}
		if total != n {
			t.Fatalf("total=%d want %d", total, n)
		}
		if len(blks) == 0 {
			break
		}
		for _, b := range blks {
			id := idOf(b)
			if seen[id] {
				t.Fatalf("重复块 id=%d（重复插入会让用户看到重复消息）", id)
			}
			seen[id] = true
		}
		cur = next
		pages++
		if !more {
			break
		}
		if pages > n {
			t.Fatal("翻页未终止")
		}
	}
	if len(seen) != n {
		t.Fatalf("历史丢失：翻出 %d 块，应为 %d", len(seen), n)
	}
	for i := 1; i <= n; i++ {
		if !seen[uint64(i)] {
			t.Fatalf("缺块 id=%d", i)
		}
	}
}

// TestMarshalTailTrimsTurnsAndDiffs turns/diffs 与 blocks 同步裁剪，且给真实总数。
func TestMarshalTailTrimsTurnsAndDiffs(t *testing.T) {
	r := buildPagingReducer(t, 800)
	pv, err := r.MarshalTail(100)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Turns       []json.RawMessage `json:"turns"`
		TurnsTotal  int               `json:"turns_total"`
		BlocksTotal int               `json:"blocks_total"`
	}
	if err := json.Unmarshal(pv.View, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Turns) != 100 {
		t.Fatalf("turns 应裁到 100，实际 %d", len(doc.Turns))
	}
	// Statusbar 显示回合数用 turns_total，不能是「已加载条数」。
	if doc.TurnsTotal != 800 {
		t.Fatalf("turns_total=%d want 800（否则长会话会把回合数显示成已加载条数）", doc.TurnsTotal)
	}
	if doc.BlocksTotal != 800 {
		t.Fatalf("blocks_total=%d want 800", doc.BlocksTotal)
	}
}

// TestMarshalTailFullWhenUnderLimit 块数不超过 limit 时不标 partial（小会话行为不变）。
func TestMarshalTailFullWhenUnderLimit(t *testing.T) {
	r := buildPagingReducer(t, 10)
	pv, err := r.MarshalTail(200)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(pv.View, &doc); err != nil {
		t.Fatal(err)
	}
	if _, has := doc["blocks_partial"]; has {
		t.Fatal("未截断时不该标 blocks_partial")
	}
	if pv.HasMore {
		t.Fatal("未截断时不该声称还有更早的")
	}
	// 仍必须能当普通 checkpoint 用（字段齐全）
	var cp Checkpoint
	if err := json.Unmarshal(pv.View, &cp); err != nil {
		t.Fatalf("投影必须是合法 checkpoint: %v", err)
	}
	if len(cp.Blocks) != 10 {
		t.Fatalf("块数不对: %d", len(cp.Blocks))
	}
}

// TestReadBlockPageDropsEmptyAssistantShells 分页必须与 Marshal 同一套归一化语义。
//
// 背景：恢复时空壳 assistant 块（llm_start 无 llm_end、无正文无思考、未结算）
// 会被 normalizeDanglingState 丢弃。真实会话实测有 451 个这样的块。
// 分页若基于 r.blocks 而非归一化快照，就会返回 Marshal 里根本不存在的块 ——
// 用户翻历史时会看到「幽灵消息」。
func TestReadBlockPageDropsEmptyAssistantShells(t *testing.T) {
	r := NewViewReducer()
	cp := &Checkpoint{SchemaVersion: checkpointSchemaVersion}
	for i := 1; i <= 40; i++ {
		cp.Blocks = append(cp.Blocks, Block{
			Kind: blockKindAssistant,
			ID:   uint64(i),
			Text: "", // 全空壳 → 归一化时应被丢弃
		})
	}
	if err := r.Restore(cp); err != nil {
		t.Fatal(err)
	}

	blks, _, _, total, err := r.ReadBlockPage(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if total != 0 || len(blks) != 0 {
		t.Fatalf("空壳块应被丢弃，实际 total=%d 返回 %d 块", total, len(blks))
	}

	// 有正文的块必须保留
	r2 := NewViewReducer()
	cp2 := &Checkpoint{SchemaVersion: checkpointSchemaVersion}
	for i := 1; i <= 5; i++ {
		cp2.Blocks = append(cp2.Blocks, Block{Kind: blockKindAssistant, ID: uint64(i), Text: "有内容"})
	}
	if err := r2.Restore(cp2); err != nil {
		t.Fatal(err)
	}
	blks2, _, _, total2, err := r2.ReadBlockPage(0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if total2 != 5 || len(blks2) != 5 {
		t.Fatalf("正常块不该被丢弃: total=%d n=%d", total2, len(blks2))
	}
}
