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

// 恢复历史 record 时，**终态** agent 卡片的 items 也要按同一上限收敛。
//
// 背景：裁剪只在 agent_end 当刻生效，早于该特性落盘的 record 里 items 仍是大份。
// 用户重启后每次 checkpoint 都会把这 85% 的老历史反复重写（实测 91MB）。
// NormalizeForRestore 是一次性收敛点，但**绝不能裁运行中的卡片** ——
// 它们的 items 之后还要按 tool_id 追加。
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
	if b := byID[3]; b == nil || len(b.Items) != len(many) {
		t.Fatalf("运行中卡片不应被裁剪（实为 %d 条，期望 %d）", len(b.Items), len(many))
	}
	if b := byID[4]; b == nil || len(b.Items) != 1 {
		t.Fatal("非 agent 块的 items 不应被裁剪")
	}
}
