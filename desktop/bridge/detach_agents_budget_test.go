package main

// detachAgents 的字节预算（2026-11-12，P3）的回归。
//
// ## 为什么钉
//
// 同步子智能体（**没有 task_id**）在窗口外时会被**整份带上 items**
// （实测单卡 100–185KB）⇒ 一次 `new_session` 的响应就能带回**上百 MB**。
// 实测渲染进程 footprint 2.08 GB（峰值 3.13 GB），而 V8 的堆上限是**崩溃线**
// ⇒ 内存尖峰发生在前端任何降级机制之前，降级根本来不及救。
//
// 三条不变量：
//  1. 响应里的窗口外卡片**总字节**有界；
//  2. 丢的是**最老**的（最近的保留）—— 树默认只显示最近的 5 个根；
//  3. **有 task_id** 的卡片仍然只发骨架（items 裁掉、条数记 ItemsTrimmed）——
//     那是 UI 打开它时按 task_id 从 journal 取首屏的依据。

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// buildDetachReducer 造一个 reducer：n 张 agent 卡（每张 itemBytes 大小的转录）
// + 末尾 10 个 assistant 块（构成尾部窗口）。
func buildDetachReducer(t *testing.T, nCards, itemBytes int, withTaskID bool) *ViewReducer {
	t.Helper()
	r := NewViewReducer()
	cp := &Checkpoint{SchemaVersion: checkpointSchemaVersion}
	for i := 1; i <= nCards; i++ {
		b := Block{
			Kind:  blockKindAgent,
			ID:    uint64(i),
			Label: "agent-" + strconv.Itoa(i),
			Items: []AgentItem{{Kind: "text", Text: strings.Repeat("x", itemBytes)}},
		}
		if withTaskID {
			b.TaskID = "task-" + strconv.Itoa(i)
		}
		cp.Blocks = append(cp.Blocks, b)
	}
	for i := 0; i < 10; i++ {
		cp.Blocks = append(cp.Blocks, Block{
			Kind: blockKindAssistant,
			ID:   uint64(nCards + 1 + i),
			Text: "tail-" + strconv.Itoa(i),
		})
	}
	if err := r.Restore(cp); err != nil {
		t.Fatal(err)
	}
	return r
}

// TestDetachAgentsRespectsByteBudget 窗口外卡片的总字节有界。
func TestDetachAgentsRespectsByteBudget(t *testing.T) {
	// 200 张卡 × 100KB = 20 MB（远超预算）。
	r := buildDetachReducer(t, 200, 100*1024, false)
	pv, err := r.MarshalTail(10)
	if err != nil {
		t.Fatal(err)
	}
	blob, err := json.Marshal(pv.Agents)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(blob)) > detachAgentsBudgetBytes {
		t.Fatalf("窗口外卡片 %d 字节 > 预算 %d 字节", len(blob), detachAgentsBudgetBytes)
	}
	if len(pv.Agents) == 0 {
		t.Fatal("不该一张都不给（预算内的最近卡片要保留）")
	}
	t.Logf("200 张卡（无上界 20MB）→ 带出 %d 张 / %d 字节", len(pv.Agents), len(blob))
}

// TestDetachAgentsKeepsNewest 丢的是**最老**的，最近的保留。
func TestDetachAgentsKeepsNewest(t *testing.T) {
	r := buildDetachReducer(t, 200, 100*1024, false)
	pv, err := r.MarshalTail(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Agents) == 0 {
		t.Fatal("没有带出卡片")
	}
	last := pv.Agents[len(pv.Agents)-1]
	if last.ID != 200 {
		t.Fatalf("最后一张应是**最近**的（id=200），实际 id=%d", last.ID)
	}
	// 顺序保持原样（升序），不是倒序。
	for i := 1; i < len(pv.Agents); i++ {
		if pv.Agents[i].ID <= pv.Agents[i-1].ID {
			t.Fatalf("卡片顺序乱了：%d 在 %d 之后", pv.Agents[i].ID, pv.Agents[i-1].ID)
		}
	}
}

// TestDetachAgentsTaskIDCardsStaySkeleton 有 task_id 的卡片只发骨架。
func TestDetachAgentsTaskIDCardsStaySkeleton(t *testing.T) {
	r := buildDetachReducer(t, 20, 100*1024, true)
	pv, err := r.MarshalTail(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Agents) == 0 {
		t.Fatal("没有带出卡片")
	}
	for _, b := range pv.Agents {
		if len(b.Items) != 0 {
			t.Fatalf("卡片 %d 仍带着 %d 条 item（有 task_id 的应只发骨架）", b.ID, len(b.Items))
		}
		if b.ItemsTrimmed == 0 {
			t.Fatalf("卡片 %d 裁了 items 却没记 ItemsTrimmed —— UI 会以为它本来就没有记录", b.ID)
		}
	}
}

// TestDetachAgentsSmallSessionUntouched 小会话（本来就在预算内）一个都不该丢。
func TestDetachAgentsSmallSessionUntouched(t *testing.T) {
	r := buildDetachReducer(t, 5, 1024, false)
	pv, err := r.MarshalTail(10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pv.Agents) != 5 {
		t.Fatalf("小会话应原样带出 5 张，实际 %d", len(pv.Agents))
	}
	for _, b := range pv.Agents {
		if len(b.Items) != 1 {
			t.Fatalf("卡片 %d 的 items 被裁了（小会话不该裁）", b.ID)
		}
	}
}
