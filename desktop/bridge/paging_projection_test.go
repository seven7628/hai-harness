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

// TestMarshalTailCarriesAgentsOutsideWindow 窗口外的子智能体卡片必须随投影一起回给 UI。
//
// 用户报障（2026-10）：「我的子智能体左侧侧边栏怎么看不到了？？？刷新后近期的也看不到了」。
// 真因：左栏的树是**从 blocks 投影出来**的（lib/agentTree.agentTreeFromView），而打开
// 会话只回最近 200 块 —— 真实会话实测（487 块）6 张卡片全在窗口之外，于是刷新一次
// 树就空一次。run_nodes 也救不了：新顶层 run 会把 runNodes 重置成 {主 run}。
//
// 这条测试钉住三件事：
//  1. 窗口外的 agent/async_task 卡片进 pv.Agents（树有东西可画）；
//  2. 有 task_id 的卡片只发骨架（items 清空 + items_trimmed 记账），否则「首屏 200 块」
//     省下来的传输量又被卡片 item 吐回去（实测单卡 100–185KB）；
//  3. 没有 task_id 的同步子智能体整份带上 —— 它没有 journal 可回落，Items 是唯一副本。
func TestMarshalTailCarriesAgentsOutsideWindow(t *testing.T) {
	r := NewViewReducer()
	cp := &Checkpoint{SchemaVersion: checkpointSchemaVersion}
	add := func(b Block) { cp.Blocks = append(cp.Blocks, b) }
	items := func(n int) []AgentItem {
		out := make([]AgentItem, n)
		for i := range out {
			out[i] = AgentItem{Kind: agentItemText, Text: "记录"}
		}
		return out
	}
	add(Block{Kind: blockKindUser, ID: 1, Text: "开始"})
	// ① 后台子智能体：有 task_id → 骨架（items 裁掉，条数记账）
	add(Block{Kind: blockKindAgent, ID: 2, RunID: "run-bg", ParentRunID: "run-main", Label: "后台子智能体",
		Status: statusCompleted, SpawnedAt: 10, TaskID: "task-bg", Items: items(30)})
	// ② 同步子智能体：没有 task_id → 整份带上（没有 journal 可回落）
	add(Block{Kind: blockKindAgent, ID: 3, RunID: "run-sync", ParentRunID: "run-main", Label: "同步子智能体",
		Status: statusCompleted, SpawnedAt: 20, Items: items(4)})
	// ③ 后台任务占位
	add(Block{Kind: blockKindAsyncTask, ID: 4, Label: "后台任务", Status: statusRunning, TaskID: "task-async"})
	// ④ 被摘离的工具任务（树投影也认它）
	add(Block{Kind: blockKindTool, ID: 5, Name: "bash", Status: statusDone, TaskID: "task-tool", Promoted: true})
	// ⑤ 普通工具块：不进 Agents（它在对话流里，由「加载更早」取回）
	add(Block{Kind: blockKindTool, ID: 6, Name: "bash", Status: statusDone})
	// 填充到窗口之外
	for i := 7; i <= 120; i++ {
		add(Block{Kind: blockKindAssistant, ID: uint64(i), Text: "正文"})
	}
	// ⑥ 窗口**内**的卡片：已经在 blocks 里，不能再重复进 Agents
	add(Block{Kind: blockKindAgent, ID: 121, RunID: "run-recent", ParentRunID: "run-main", Label: "最近的",
		Status: statusRunning, SpawnedAt: 30, TaskID: "task-recent"})
	if err := r.Restore(cp); err != nil {
		t.Fatal(err)
	}

	pv, err := r.MarshalTail(20)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[uint64]Block{}
	for _, b := range pv.Agents {
		byID[b.ID] = b
	}
	if len(byID) != 4 {
		t.Fatalf("窗口外卡片应有 4 张（agent×2/async_task/工具任务），实际 %d 张: %v", len(byID), keysOf(byID))
	}
	bg, ok := byID[2]
	if !ok {
		t.Fatal("后台子智能体卡片必须回给 UI（否则左栏树刷新后就是空的）")
	}
	if len(bg.Items) != 0 || bg.ItemsTrimmed != 30 {
		t.Fatalf("有 task_id 的卡片应只发骨架: items=%d trimmed=%d（want 0/30）", len(bg.Items), bg.ItemsTrimmed)
	}
	if bg.TaskID != "task-bg" || bg.RunID != "run-bg" || bg.ParentRunID != "run-main" || bg.Label != "后台子智能体" {
		t.Fatalf("骨架必须保留身份字段（树的父子连线靠它）: %+v", bg)
	}
	sync, ok := byID[3]
	if !ok || len(sync.Items) != 4 {
		t.Fatalf("没有 task_id 的同步子智能体必须整份带上（无 journal 可回落）: %+v", sync)
	}
	if _, ok := byID[5]; !ok {
		t.Fatal("promoted 工具任务在树里也是一个节点，必须带上")
	}
	if _, has := byID[6]; has {
		t.Fatal("普通工具块不属于树，不该进 Agents")
	}
	if _, has := byID[121]; has {
		t.Fatal("窗口内的卡片已在 blocks 里，不能重复进 Agents（会出现两个同名节点）")
	}
	// 未截断时没有窗口外卡片
	small := buildPagingReducer(t, 10)
	pv2, err := small.MarshalTail(200)
	if err != nil {
		t.Fatal(err)
	}
	if len(pv2.Agents) != 0 {
		t.Fatalf("未截断时不该有 Agents，实际 %d", len(pv2.Agents))
	}
}

func keysOf(m map[uint64]Block) []uint64 {
	out := make([]uint64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
