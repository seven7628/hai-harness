package main

// 增量 Marshal 回归测试。
//
// ViewReducer.Marshal 现在按 block 缓存序列化字节，只重算变化的部分。
// 风险在于「缓存命中错了 → 返回陈旧字节 → UI 显示旧内容」，且不会报错。
// 这里钉住：
//   1. 增量输出与整份 json.Marshal **逐字节相同**（任何内容变化下）；
//   2. 任何字段变化都必须使指纹失效（漏一个就会返回旧字节）；
//   3. item 追加 / 文本修改 / 状态变化都会反映到输出。

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/events"
)

// marshalFull 是基准：直接对快照做整份序列化。
func marshalFull(t *testing.T, r *ViewReducer) []byte {
	t.Helper()
	r.mu.Lock()
	defer r.mu.Unlock()
	b, err := json.Marshal(r.snapshotLocked())
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// TestIncrementalMarshalMatchesFull 冷/热缓存、以及各种变更后都必须逐字节一致。
func TestIncrementalMarshalMatchesFull(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now()

	// 造一个有内容的视图
	_ = r.ApplyPlain(&events.AgentStart{RunId: "root", Content: "主", Timestamp: ts, EventType: events.AgentStartType})
	_ = r.ApplyPlain(&events.ContentChunk{RunId: "root", Content: "hello", Timestamp: ts, EventType: events.ContentChunkType})
	_ = r.ApplyPlain(&events.ToolStart{RunId: "root", Id: "c1", Name: "bash", Arguments: `{}`, Timestamp: ts, EventType: events.ToolRunStartType})
	_ = r.ApplyPlain(&events.ToolResponse{RunId: "root", Id: "c1", Name: "bash", Result: "ok", Timestamp: ts, EventType: events.ToolRunEndType})

	check := func(label string) {
		t.Helper()
		got, err := r.Marshal()
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		want := marshalFull(t, r)
		if !bytes.Equal(got, want) {
			i := 0
			for i < len(got) && i < len(want) && got[i] == want[i] {
				i++
			}
			lo := i - 80
			if lo < 0 {
				lo = 0
			}
			hi := i + 80
			if hi > len(got) {
				hi = len(got)
			}
			hiw := i + 80
			if hiw > len(want) {
				hiw = len(want)
			}
			t.Fatalf("%s: 增量与全量不一致 @%d\n got: %s\nwant: %s", label, i, got[lo:hi], want[lo:hiw])
		}
	}

	check("初次（冷缓存）")
	check("再次（热缓存）")

	// 内容变化 → 必须反映
	_ = r.ApplyPlain(&events.ContentChunk{RunId: "root", Content: " world", Timestamp: ts, EventType: events.ContentChunkType})
	check("追加正文后")

	_ = r.ApplyPlain(&events.ReasoningChunk{RunId: "root", Content: "思考", Timestamp: ts, EventType: events.ReasoningChunkType})
	check("追加思考后")

	_ = r.ApplyPlain(&events.LLMStart{RunId: "root", RequestId: "r2", Timestamp: ts, EventType: events.LLMStartType})
	check("新块加入后")

	_ = r.ApplyPlain(&events.ToolStart{RunId: "root", Id: "c2", Name: "read_file", Arguments: `{"p":1}`, Timestamp: ts, EventType: events.ToolRunStartType})
	check("工具进行中")
	_ = r.ApplyPlain(&events.ToolResponse{RunId: "root", Id: "c2", Name: "read_file", Result: "内容", Timestamp: ts, EventType: events.ToolRunEndType})
	check("工具结束后")
}

// TestBlockCacheHonorsDirtyMarkers 字节缓存必须尊重写入方的脏标记。
//
// 这是本优化**唯一**的正确性前提：缓存按 block id 复用序列化字节，
// 由写入方（applyLocked 里的 r.blocks[idx]=b / 新建块）标脏来决定何时重算。
// 曾试过两种「不依赖标脏」的替代方案，都被证明会返回陈旧字节：
//   · 手写字段清单指纹 —— 漏了 Block.Result（被本文件的一致性测试抓到）；
//   · 「终态块内容永不变」假设 —— 工具 running→done 后仍会被原地补内容
//     （被 session 包的 TestSidecarRewritesLiveBlocks 抓到）。
func TestBlockCacheHonorsDirtyMarkers(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now()
	_ = r.ApplyPlain(&events.AgentStart{RunId: "root", Content: "x", Timestamp: ts, EventType: events.AgentStartType})
	_ = r.ApplyPlain(&events.ContentChunk{RunId: "root", Content: "第一版", Timestamp: ts, EventType: events.ContentChunkType})

	first, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(first, []byte("第一版")) {
		t.Fatal("首轮序列化应含正文")
	}

	// 内容变化后必须反映（脏标记被 Apply 设置）
	_ = r.ApplyPlain(&events.ContentChunk{RunId: "root", Content: "追加", Timestamp: ts, EventType: events.ContentChunkType})
	second, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(second, []byte("第一版追加")) {
		t.Fatalf("内容变化未反映到输出: %s", string(second))
	}

	// 再与全量对拍，确保增量路径没有引入差异
	if want := marshalFull(t, r); !bytes.Equal(second, want) {
		t.Fatal("增量与全量不一致")
	}
}

// TestIncrementalMarshalRestoreEquivalence 恢复后（NormalizeForRestore 等路径）也必须一致。
func TestIncrementalMarshalRestoreEquivalence(t *testing.T) {
	src := NewViewReducer()
	ts := time.Now()
	_ = src.ApplyPlain(&events.AgentStart{RunId: "root", Content: "x", Timestamp: ts, EventType: events.AgentStartType})
	_ = src.ApplyPlain(&events.ContentChunk{RunId: "root", Content: "正文", Timestamp: ts, EventType: events.ContentChunkType})

	snap := src.Snapshot()
	dst := NewViewReducer()
	if err := dst.Restore(snap); err != nil {
		t.Fatal(err)
	}
	got, err := dst.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	want := marshalFull(t, dst)
	if !bytes.Equal(got, want) {
		t.Fatal("Restore 后增量与全量不一致")
	}

	// NormalizeForRestore 会直接改内部状态（绕过 Apply）→ 缓存必须失效
	dst.NormalizeForRestore()
	got, _ = dst.Marshal()
	want = marshalFull(t, dst)
	if !bytes.Equal(got, want) {
		t.Fatal("NormalizeForRestore 后增量与全量不一致（缓存未失效）")
	}
}
