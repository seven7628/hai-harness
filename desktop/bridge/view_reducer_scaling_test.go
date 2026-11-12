package main

// reducer 规模回归的**金丝雀**（2026-11-11，CHECKPOINT_V3_DESIGN §6 P3-0）。
//
// ## 为什么要有这个测试
//
// 2026-11-11 在做 P3（视图从事件流投影）时实测发现：reducer 的事件应用路径是
// **O(N²)** 的。三处叠加：
//
//  1. `filterBlocks`（挂在每个 llm_end / agent_start 上）每次都 make + 逐块**按值**
//     复制整个块数组；
//  2. 它把切片的 cap 重置成 len → `append(r.blocks, …)` 的富余容量被反复丢弃
//     → 每来一个工具块就整数组扩容一次；
//  3. `isSubRunLocked`（每个 tool_start/tool_end 都问一次）线性扫**全部块**，
//     而且是按值迭代（Block 是 ~1KB 的大结构 → duffcopy）。
//
// 实测 205MB / 28,491 事件的真实会话：filterBlocks 分配 19.6GB、append 分配
// 24.1GB，合计 48GB；重放耗时 6.4s（apply 4.7s）。
//
// 修复后同一会话：**2.0GB / 2.6s（apply 0.93s）**。
//
// 这个测试的判据是**分配量**（不是墙钟）：分配量确定、不受机器负载影响，
// 而 O(N²) 在分配量上表现为指数级增长 —— 阈值卡在两者之间。
//
// ## 阈值怎么定的
//
// 旧实现：2000 轮 × 2000 块 × ~1KB ≈ **4GB**。
// 新实现：O(N) —— 实测同规模约 **20MB**（含 JSON 解析与块对象本身）。
// 阈值取 512MB：比新实现宽 25 倍（抗环境噪声），比旧实现小 8 倍（抓住回归）。

import (
	"fmt"
	"runtime"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/events"
)

// reducerScalingCanaryMaxAlloc 是分配量上限（字节）。
const reducerScalingCanaryMaxAlloc = 512 << 20

// TestReducerApplyDoesNotScaleQuadratically 钉住「事件应用不随历史长度平方增长」。
//
// 事件序列刻意复现真实形态：**每轮一次 llm_end + 若干 tool_start/tool_run_end**。
// llm_end 是关键 —— 它会触发 filterBlocks（清 error 块），旧实现在这里复制整份块数组。
func TestReducerApplyDoesNotScaleQuadratically(t *testing.T) {
	const rounds = 2000

	red := NewViewReducer()
	// 先热身（分配器/GC 的第一次开销不算进来）。
	applyScalingRound(red, 0, 20)

	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)

	for i := 0; i < rounds; i++ {
		applyScalingRound(red, i, 1)
	}

	runtime.ReadMemStats(&after)
	alloc := after.TotalAlloc - before.TotalAlloc

	t.Logf("rounds=%d blocks=%d 分配量=%.1f MB（上限 %.0f MB）",
		rounds, red.BlockCount(), float64(alloc)/(1<<20), float64(reducerScalingCanaryMaxAlloc)/(1<<20))

	if alloc > reducerScalingCanaryMaxAlloc {
		t.Fatalf("分配量 %.1f MB 超上限 %.0f MB —— reducer 又退回 O(N²) 了？",
			float64(alloc)/(1<<20), float64(reducerScalingCanaryMaxAlloc)/(1<<20))
	}
}

// applyScalingRound 造一轮「llm_start → tool_start → tool_run_end → llm_end」。
func applyScalingRound(red *ViewReducer, round, tools int) {
	runID := fmt.Sprintf("run-%d", round)
	reqID := fmt.Sprintf("req-%d", round)
	now := time.Now()
	for t := 0; t < tools; t++ {
		toolID := fmt.Sprintf("call-%d-%d", round, t)
		_ = red.Apply(&events.ToolStart{
			Id: toolID, Name: "bash", Arguments: `{"cmd":"ls"}`,
			RunId: runID, RequestId: reqID, Timestamp: now,
		}, EventMeta{})
		_ = red.Apply(&events.ToolResponse{
			Id: toolID, Name: "bash", Result: "ok",
			RunId: runID, RequestId: reqID, Timestamp: now,
		}, EventMeta{})
	}
	_ = red.Apply(&events.LLMStart{
		Model: "test-model", RunId: runID, RequestId: reqID, Timestamp: now,
	}, EventMeta{})
	_ = red.Apply(&events.LLMEnd{
		Model: "test-model", Content: fmt.Sprintf("第 %d 轮的回答", round),
		RunId: runID, RequestId: reqID, Timestamp: now,
	}, EventMeta{})
}

// TestFilterBlocksKeepsOrderAndCapacity 钉住 filterBlocks 的语义与「原地」性质。
//
// 原地过滤是性能改动的核心，必须钉住两件事：
//   · 语义：保留块的顺序、内容、id 完全一致；
//   · 性质：底层数组被复用（cap 不被重置）—— 这是 append 摊还成本的关键。
func TestFilterBlocksKeepsOrderAndCapacity(t *testing.T) {
	blocks := []Block{
		{Kind: blockKindAssistant, ID: 1, Text: "a"},
		{Kind: blockKindError, ID: 2, Text: "boom"},
		{Kind: blockKindTool, ID: 3, Name: "bash"},
		{Kind: blockKindError, ID: 4, Text: "boom2"},
		{Kind: blockKindUser, ID: 5, Text: "u"},
	}
	origCap := cap(blocks)
	origPtr := &blocks[0]

	out := filterBlocks(blocks, func(b *Block) bool { return b.Kind != blockKindError })

	if len(out) != 3 {
		t.Fatalf("保留块数=%d want 3", len(out))
	}
	want := []uint64{1, 3, 5}
	for i, id := range want {
		if out[i].ID != id {
			t.Fatalf("第 %d 块 id=%d want %d（顺序被打乱）", i, out[i].ID, id)
		}
	}
	if out[0].Text != "a" || out[1].Name != "bash" || out[2].Text != "u" {
		t.Fatalf("块内容被破坏: %+v", out)
	}
	if cap(out) != origCap {
		t.Fatalf("cap 被重置：%d → %d（append 的摊还成本会退回 O(N)）", origCap, cap(out))
	}
	if &out[0] != origPtr {
		t.Fatal("底层数组没有被复用（原地过滤失效）")
	}
}

// TestFilterBlocksNoDropKeepsSameSlice 没有要删的块时，返回的必须是**同一个切片**
// （旧实现每次都重新分配 —— 这是 19.6GB 分配的来源）。
func TestFilterBlocksNoDropKeepsSameSlice(t *testing.T) {
	blocks := []Block{{Kind: blockKindAssistant, ID: 1}, {Kind: blockKindTool, ID: 2}}
	out := filterBlocks(blocks, func(b *Block) bool { return b.Kind != blockKindError })
	if len(out) != 2 {
		t.Fatalf("块数=%d want 2", len(out))
	}
	if &out[0] != &blocks[0] {
		t.Fatal("没有要删的块时不该重新分配数组")
	}
}

// TestIsSubRunRecognizesRestoredAgentCard 钉住「恢复后仍认得出子 run」。
//
// Restore 会清空 agentBlockID（恢复后的旧卡片不再接收后续 tool 事件），
// 但卡片本身还在 —— 因此「这个 run 是子 run」的判定不能因为索引清空而失效。
// agentRunIdx 就是在 Restore 时按块重建的。
func TestIsSubRunRecognizesRestoredAgentCard(t *testing.T) {
	red := NewViewReducer()
	// 建一张子 agent 卡。
	_ = red.Apply(&events.AgentStart{
		RunId: "sub-1", ParentRunId: "main-1", Name: "干活",
		Timestamp: time.Now(),
	}, EventMeta{})

	cp := red.Snapshot()
	cp.Blocks = cloneBlocks(cp.Blocks)

	restored := NewViewReducer()
	if err := restored.Restore(cp); err != nil {
		t.Fatal(err)
	}
	// Restore 后 agentBlockID 是空的（设计如此），但 agentRunIdx 必须重建。
	if restored.agentBlockID["sub-1"] != 0 {
		t.Fatal("Restore 不该重建 agentBlockID（这条是既有设计，测试钉住它）")
	}
	if !restored.isSubRunLocked("sub-1") {
		t.Fatal("恢复后认不出子 run —— 工具行会挂到主会话去")
	}
	if restored.isSubRunLocked("main-1") {
		t.Fatal("顶层 run 不该被认成子 run")
	}
}
