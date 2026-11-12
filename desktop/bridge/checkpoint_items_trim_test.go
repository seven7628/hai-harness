package main

// agent block 的 items 裁剪（maxAgentItemsPerBlock）回归测试。
// 裁剪把 record 从 128MB 压到 17MB（commit 6.0s → 0.7s），但它动的是
// **已落盘的历史展示数据**，一旦裁错方向就是「用户重启后看不到子智能体记录」。
// 这里钉住：终态后裁剪生效、被裁条数被记录、终态后迟到的事件不丢。

import (
	"testing"
	"time"

	"github.com/seven7628/hai-harness/events"
)

// agent_end 之后仍可能有 items 到达（异步交付/迟到事件）：
// 裁剪后新条目必须能被正常追加，且不能因为 items 被裁导致去重失效而重复。
func TestAgentItemsAfterTrim(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now()
	runID := "sub-1"
	_ = r.ApplyPlain(&events.AgentStart{RunId: "root", Content: "主任务", Timestamp: ts, EventType: events.AgentStartType})
	_ = r.ApplyPlain(&events.AgentStart{RunId: runID, Content: "子任务", ParentRunId: "root", Depth: 1,
		Timestamp: ts, EventType: events.AgentStartType})

	// 造 > maxAgentItemsPerBlock 个 tool 对
	total := maxAgentItemsPerBlock() + 50
	for i := 0; i < total; i++ {
		id := "call-" + itoa(i)
		_ = r.ApplyPlain(&events.ToolStart{RunId: runID, Id: id, Name: "read_file",
			Arguments: `{}`, Timestamp: ts, EventType: events.ToolRunStartType})
		_ = r.ApplyPlain(&events.ToolResponse{RunId: runID, Id: id, Name: "read_file",
			Result: "r", Timestamp: ts, EventType: events.ToolRunEndType})
	}
	_ = r.ApplyPlain(&events.AgentEnd{RunId: runID, Content: "done", Timestamp: ts, EventType: events.AgentEndType})

	cp := r.Snapshot()
	var agentBlk *Block
	for i := range cp.Blocks {
		if cp.Blocks[i].Kind == "agent" {
			agentBlk = &cp.Blocks[i]
		}
	}
	if agentBlk == nil {
		t.Fatal("找不到 agent block")
	}
	if len(agentBlk.Items) > maxAgentItemsPerBlock() {
		t.Fatalf("裁剪失败：仍有 %d 条 items（上限 %d）", len(agentBlk.Items), maxAgentItemsPerBlock())
	}
	if agentBlk.ItemsTrimmed == 0 {
		t.Fatal("应记录被裁条数")
	}

	// 终态后仍来一个 tool：必须能追加，且不会被误裁成更少
	_ = r.ApplyPlain(&events.ToolStart{RunId: runID, Id: "late-call", Name: "bash",
		Arguments: `{}`, Timestamp: ts, EventType: events.ToolRunStartType})
	cp2 := r.Snapshot()
	for i := range cp2.Blocks {
		if cp2.Blocks[i].Kind == "agent" {
			agentBlk = &cp2.Blocks[i]
		}
	}
	found := false
	for _, it := range agentBlk.Items {
		if it.ToolID == "late-call" {
			found = true
		}
	}
	if !found {
		t.Fatal("终态后到达的 item 丢失了")
	}
	t.Logf("裁剪后 items=%d, trimmed=%d, 迟到 item 保留=%v", len(agentBlk.Items), agentBlk.ItemsTrimmed, found)
}

// 恢复历史 record 时，agent 卡片的 items 也要按同一上限收敛。
//
// 背景：裁剪只在 agent_end 当刻生效，早于该特性落盘的 record 里 items 仍是大份。
// 用户重启后每次 checkpoint 都会把这 85% 的老历史反复重写（实测 91MB）。
// NormalizeForRestore 是一次性收敛点。
//
// **2026-11-11 修正**：原实现只裁「恢复前就是终态」的卡片，running 的卡片被
// 跳过 —— 理由是「running 卡片的 items 之后还要按 tool_id 追加，裁掉会让
// hasAgentToolItem 查不到而重复插入」。这个理由不成立（见 view_reducer.go
// NormalizeForRestore 里的三条论证）：恢复后 agentBlockID 为空，同一个
// run_id 再来 agent_start 会新建一张卡，tool 事件根本找不到旧卡。
//
// 真实代价（实测 s-1792660803279727000-3）：4 张 abandoned 卡片的 items 达
// 301/270/264/150 条且 items_trimmed=0（**从未裁过**），每被重写一次就是
// 几百 KB 的写放大。
func TestRestoreTrimsOnlySettledAgentBlocks(t *testing.T) {
	limit := maxAgentItemsPerBlock()
	many := make([]AgentItem, limit+40)
	for i := range many {
		many[i] = AgentItem{Kind: "text", Text: "历史条目 " + itoa(i)}
	}

	cp := &Checkpoint{Blocks: []Block{
		{ID: 1, Kind: blockKindAgent, Status: statusDone, Items: append([]AgentItem(nil), many...)},
		{ID: 2, Kind: blockKindAgent, Status: statusFailed, Items: append([]AgentItem(nil), many...)},
		{ID: 3, Kind: blockKindAgent, Status: statusRunning, Items: append([]AgentItem(nil), many...)},
		{ID: 4, Kind: blockKindUser, Items: []AgentItem{{Kind: "text", Text: "hi"}}},
	}}

	r := NewViewReducer()
	if err := r.Restore(cp); err != nil {
		t.Fatal(err)
	}
	r.NormalizeForRestore()
	got := r.Snapshot()

	byID := map[uint64]*Block{}
	for i := range got.Blocks {
		byID[got.Blocks[i].ID] = &got.Blocks[i]
	}
	for _, id := range []uint64{1, 2} {
		b := byID[id]
		if b == nil {
			t.Fatalf("block %d 丢了", id)
		}
		if len(b.Items) != limit {
			t.Fatalf("block %d 应收敛到 %d 条，实为 %d", id, limit, len(b.Items))
		}
		if b.ItemsTrimmed != 40 {
			t.Fatalf("block %d 的 ItemsTrimmed 应为 40，实为 %d", id, b.ItemsTrimmed)
		}
		// 保留的必须是**最新**的 40+limit 里最后 limit 条
		if b.Items[limit-1].Text != many[len(many)-1].Text {
			t.Fatalf("block %d 裁掉了最新条目", id)
		}
	}
	// running → abandoned 的卡片**也要裁**：恢复后它收不到任何 tool 事件
	// （agentBlockID 为空），不会被追加，也不会重复插入 item。
	if b := byID[3]; b == nil {
		t.Fatal("block 3 丢了")
	} else {
		if b.Status != statusAbandoned {
			t.Fatalf("恢复后 running 应收敛为 abandoned，实为 %q", b.Status)
		}
		if len(b.Items) != limit {
			t.Fatalf("恢复后收敛的卡片应收敛到 %d 条，实为 %d", limit, len(b.Items))
		}
		if b.ItemsTrimmed != 40 {
			t.Fatalf("block 3 的 ItemsTrimmed 应为 40，实为 %d", b.ItemsTrimmed)
		}
	}
	if b := byID[4]; b == nil || len(b.Items) != 1 {
		t.Fatal("非 agent 块的 items 不应被裁剪")
	}
}

// TestRestoredAbandonedCardDoesNotDuplicateToolItems 钉住「恢复后旧卡不会被
// tool 事件追加」—— 这正是「恢复时裁剪 running 卡片」得以安全的前提。
//
// 背景（2026-11-11）：NormalizeForRestore 原来跳过 running 卡片，理由是
// 「裁掉后 hasAgentToolItem 查不到 → 同一次 tool 被重复插入」。该修复把
// running→abandoned 的卡片**也**裁了，前提是「旧卡恢复后收不到 tool 事件」。
//
// 机制：恢复不重建 agentBlockID（它只在 applyAgentStartLocked 里填），
// 于是 tool_start / tool_end 按 agentBlockID 找卡 → 找不到 → 对旧卡无操作。
//
// 若这个前提被破坏（例如将来恢复时重建 agentBlockID），裁剪就会导致重复插入
// —— 本测试就是那个前提的守卫。
func TestRestoredAbandonedCardDoesNotDuplicateToolItems(t *testing.T) {
	limit := maxAgentItemsPerBlock()
	many := make([]AgentItem, limit+40)
	for i := range many {
		many[i] = AgentItem{Kind: "text", Text: "历史条目 " + itoa(i)}
	}
	const runID = "sub-restored"
	cp := &Checkpoint{Blocks: []Block{
		{ID: 1, Kind: blockKindAgent, Status: statusRunning, RunID: runID,
			Items: append([]AgentItem(nil), many...)},
	}}

	r := NewViewReducer()
	if err := r.Restore(cp); err != nil {
		t.Fatal(err)
	}
	r.NormalizeForRestore()

	// 恢复后：旧卡被收敛为 abandoned 且已裁剪。
	got := r.Snapshot()
	if len(got.Blocks) == 0 {
		t.Fatal("恢复后没有块")
	}
	card := &got.Blocks[0]
	if card.Status != statusAbandoned {
		t.Fatalf("running 应收敛为 abandoned，实为 %q", card.Status)
	}
	if len(card.Items) != limit {
		t.Fatalf("应收敛到 %d 条，实为 %d", limit, len(card.Items))
	}

	// 旧 run 的 tool 事件到达：不得往旧卡追加（否则裁剪会导致 tool_id 查不到
	// → 重复插入）。
	ts := time.Now()
	_ = r.ApplyPlain(&events.ToolStart{RunId: runID, Id: "old-call", Name: "bash",
		Arguments: `{}`, Timestamp: ts, EventType: events.ToolRunStartType})
	_ = r.ApplyPlain(&events.ToolResponse{RunId: runID, Id: "old-call", Name: "bash",
		Result: "ok", Timestamp: ts, EventType: events.ToolRunEndType})

	got2 := r.Snapshot()
	for i := range got2.Blocks {
		b := &got2.Blocks[i]
		if b.Kind != blockKindAgent {
			continue
		}
		if b.RunID != runID {
			continue
		}
		for _, it := range b.Items {
			if it.ToolID == "old-call" {
				t.Fatalf("恢复出来的旧卡被追加了 tool item（%d 条）——"+
					"裁剪与 hasAgentToolItem 去重的前提被破坏", len(b.Items))
			}
		}
		if len(b.Items) != limit {
			t.Fatalf("旧卡的 items 数变了：%d（期望 %d）", len(b.Items), limit)
		}
	}
}
