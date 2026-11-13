package main

// 提交路径的 marshal 成本（2026-11-12，P4 归因）的基准。
//
// ## 为什么有这个基准
//
// 实测慢提交里 `marshal=5.898s`（23.4s 那次的最慢项）。归因过程：
//
//   1. 实验台（HAI_LAB_PHASE=commit）跑真实会话的提交只要 **72–363ms**
//      —— 但实验台**不走 reducer**（spans 是从加载出来的 view 解析的），
//      所以它量不到 marshal ⇒ 差异就在 reducer 这段；
//   2. reducer 的 bodyOnly 路径（提交路径）里有一处**每次提交都克隆全部块**
//      的动作：`cloneBlocksForEncode`。
//
// ## 根因
//
// `cloneBlocksForEncode` 会复制**每张 agent 卡**的 items（normalize 唯一会
// 写的字段就在那儿）。真实会话 40,232 块里有 ~1,600 张 agent 卡、每张 50 条
// item ⇒ 每次提交白分配 ~20MB ⇒ 在 3GB 堆上触发 GC ⇒ 秒级。
//
// 但 normalize **只对「正在压缩中」的那一项写** ⇒ 真正需要复制的只有
// 此刻正在压缩的那几张卡。
//
// 跑法：
//
//	go test ./desktop/bridge/ -run XXX -bench BenchmarkMarshalBody -benchmem -count=3

import (
	"fmt"
	"strings"
	"testing"
)

// buildBenchCheckpoint 造一个真实形态的 checkpoint：
// nBlocks 个块，其中 nAgents 张 agent 卡（每张 itemsPerCard 条 item），
// 其余是 assistant 块。
func buildBenchCheckpoint(nBlocks, nAgents, itemsPerCard int) *Checkpoint {
	cp := &Checkpoint{SchemaVersion: checkpointSchemaVersion}
	itemText := strings.Repeat("工具输出的一段文字", 40)
	for i := 1; i <= nAgents; i++ {
		items := make([]AgentItem, 0, itemsPerCard)
		for j := 0; j < itemsPerCard; j++ {
			items = append(items, AgentItem{
				Kind: "tool",
				Text: itemText,
				Name: "bash",
			})
		}
		cp.Blocks = append(cp.Blocks, Block{
			Kind:  blockKindAgent,
			ID:    uint64(i),
			Label: fmt.Sprintf("agent-%d", i),
			Items: items,
		})
	}
	for i := nAgents + 1; i <= nBlocks; i++ {
		cp.Blocks = append(cp.Blocks, Block{
			Kind: blockKindAssistant,
			ID:   uint64(i),
			Text: "正文",
		})
	}
	return cp
}

// benchReducer 造一个装了大 checkpoint 的 reducer。
func benchReducer(b *testing.B, nBlocks, nAgents, itemsPerCard int) *ViewReducer {
	b.Helper()
	r := NewViewReducer()
	if err := r.Restore(buildBenchCheckpoint(nBlocks, nAgents, itemsPerCard)); err != nil {
		b.Fatal(err)
	}
	return r
}

// BenchmarkMarshalBodyBigSession 真实规模会话（40,232 块 / 1,600 张 agent 卡）
// 的提交路径 marshal 成本。
//
// 每次迭代都让缓存失效（模拟「新事件到达 ⇒ 缓存作废」），否则第二次起
// 会命中 bodyCache，量到的是 0。
func BenchmarkMarshalBodyBigSession(b *testing.B) {
	r := benchReducer(b, 40232, 1600, 50)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.invalidateMarshalCache()
		if _, err := r.MarshalBodyWithSpans(); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkMarshalBodySmallSession 小会话（对照）：clone 的代价与块数成正比，
// 但 agent 卡少 ⇒ 差异不明显。
func BenchmarkMarshalBodySmallSession(b *testing.B) {
	r := benchReducer(b, 500, 20, 50)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		r.invalidateMarshalCache()
		if _, err := r.MarshalBodyWithSpans(); err != nil {
			b.Fatal(err)
		}
	}
}
