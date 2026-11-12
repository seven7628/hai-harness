package main

// 分页的**顺序不变量**（2026-11-11，CHECKPOINT_V3_DESIGN §6 P4）。
//
// ## 背景：blocks 不是 id 有序的
//
// reducer 的 `insertBlockAtLocked` 按**事件时刻**插入（任务回执/产出这类
// 「晚到但事件时刻更早」的块要落在它该在的位置），于是**最大 id 的块会排到
// 数组中间** —— 真实会话实测 3 个会话分别有 9 / 3 / 5 处逆序。
//
// `ReadBlockPage` 原先用 `sort.Search` 找页边界，谓词是
// `view[i].ID >= beforeID` —— 有逆序时**不单调**，Go 文档未定义其行为。
// 实测在特定游标上与线性扫描差 2~3 个位置 ⇒ 可能**跳块**（历史丢失）或**重复**。
//
// 修法：游标改成**数组位置锚点**（上一页第一块的 id → 在数组里定位 → 取它
// 之前的 limit 个）。页与页首尾相接 ⇒ 不重不漏。
//
// ## 为什么这个测试之前测不出来
//
// 既有的分页测试（`paging_projection_test.go` 等）都用 `Restore` 造
// **id 严格递增**的数组 —— 而逆序只由 `insertBlockAtLocked`（实时路径）产生，
// 所以那些测试永远走不到坏分支。

import (
	"encoding/json"
	"sort"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/events"
)

// jsonUnmarshal 是测试里的小工具。
func jsonUnmarshal(raw []byte, v any) error { return json.Unmarshal(raw, v) }

// TestPagingWithInvertedIDsLosesNothing 有逆序时，翻页仍要不重不漏。
func TestPagingWithInvertedIDsLosesNothing(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)

	// ① 造一处逆序：交付通知（id 更大）排在工具块（id 更小）之前。
	mustApply(t, r, &events.TaskStarted{TaskId: "task-inv", Name: "bg", Async: true, Timestamp: ts, EventType: events.TaskStartedType})
	mustApply(t, r, &events.ToolStart{Id: "call-late", Name: "bash", Arguments: "{}", RunId: "run-2", Timestamp: ts.Add(10 * time.Second), EventType: events.ToolRunStartType})
	mustApply(t, r, &events.TaskResultDelivered{SessionId: "s1", TaskId: "task-inv", Status: "completed", Result: "done", Timestamp: ts.Add(5 * time.Second), EventType: events.TaskResultDeliveredType})

	// ② 再补一些普通块，保证够翻几页。
	for i := 0; i < 20; i++ {
		mustApply(t, r, &events.ToolStart{
			Id: "call-" + itoa(i), Name: "bash", Arguments: "{}", RunId: "run-2",
			Timestamp: ts.Add(time.Duration(20+i) * time.Second),
		})
	}

	// ③ 确认确实有逆序（否则这个测试测不到任何东西）。
	cp := r.Snapshot()
	inverted := false
	for i := 1; i < len(cp.Blocks); i++ {
		if cp.Blocks[i].ID < cp.Blocks[i-1].ID {
			inverted = true
			break
		}
	}
	if !inverted {
		t.Fatal("构造没造出逆序 —— 测试的前提不成立")
	}

	// ④ 逐页往前翻，收集所有返回的块 id。
	seen := map[uint64]int{}
	order := []uint64{}
	cursor := uint64(0)
	for page := 0; page < 200; page++ {
		blocks, next, hasMore, _, err := r.ReadBlockPage(cursor, 3)
		if err != nil {
			t.Fatalf("第 %d 页: %v", page, err)
		}
		for _, raw := range blocks {
			var b struct {
				ID uint64 `json:"id"`
			}
			if err := jsonUnmarshal(raw, &b); err != nil {
				t.Fatal(err)
			}
			seen[b.ID]++
			order = append(order, b.ID)
		}
		if !hasMore || next == 0 {
			break
		}
		cursor = next
	}

	// ⑤ 全量块 id（归一化后 —— 空壳 assistant 块会被丢弃）。
	want := map[uint64]bool{}
	for _, b := range cp.Blocks {
		want[b.ID] = true
	}
	for id, n := range seen {
		if n > 1 {
			t.Fatalf("块 %d 被返回了 %d 次（重复消息）", id, n)
		}
		delete(want, id)
	}
	if len(want) > 0 {
		t.Fatalf("有 %d 个块从未被翻到（历史丢失）：%v", len(want), sortedKeys(want))
	}
}

// sortedKeys 取 map 的键并升序（测试报错用）。
func sortedKeys(m map[uint64]bool) []uint64 {
	out := make([]uint64, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}
