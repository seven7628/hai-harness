package main

import (
	"encoding/json"
	"testing"
)

// 窗口恢复（paged）时 MarshalTail 不得越界。
//
// 回归背景（2026-11-08 实测）：bridge 启动即 panic
//
//	slice bounds out of range [10294:496]
//	main.(*ViewReducer).MarshalTail  view_reducer.go:4110
//
// 根因：窗口恢复时 r.blocks 只装了解码过的窗口尾部（496 块），而 r.blocksTotal
// 是**历史总块数**（10294）。MarshalTail 原来按 total 算切点 start = total - limit，
// 再切 blocks[start:] → start(10294) 远超 len(blocks)(496) → panic。
//
// 切点必须相对「实际拿到的块」算：blocks 本身就是最新的一段，取其后 limit 个；
// 非窗口模式下 total == len(blocks)，行为与旧写法等价。
func TestMarshalTailPagedWindowNoOverflow(t *testing.T) {
	const (
		windowBlocks = 496 // 实际解码进内存的窗口块数（对应实测的 496）
		historyTotal = 10294
		limit        = 200
	)

	r := NewViewReducer()
	cp := &Checkpoint{
		BlocksTotal: historyTotal,
		PagedBlocks: &PagedBlocks{IDs: []uint64{1, 2, 3}},
	}
	for i := 0; i < windowBlocks; i++ {
		cp.Blocks = append(cp.Blocks, Block{
			Kind: "assistant",
			ID:   uint64(i + 1), // id 严格递增（reducer 的约定），最新的在末尾
			Text: "x",
		})
	}
	if err := r.Restore(cp); err != nil {
		t.Fatalf("Restore 失败: %v", err)
	}

	pv, err := r.MarshalTail(limit)
	if err != nil {
		t.Fatalf("MarshalTail 报错: %v", err)
	}
	if pv == nil {
		t.Fatal("MarshalTail 返回 nil")
	}
	if pv.Returned != limit {
		t.Fatalf("返回块数应为 %d，得 %d", limit, pv.Returned)
	}
	if pv.Total != historyTotal {
		t.Fatalf("Total 应为历史总块数 %d，得 %d", historyTotal, pv.Total)
	}
	// 窗口里还有更早的块 → UI 必须能继续往前翻。
	if !pv.HasMore {
		t.Fatal("HasMore 应为 true（窗口内仍有更早的块）")
	}
	// 尾部块必须是窗口里最新的 limit 个。
	var view struct {
		Blocks []Block `json:"blocks"`
	}
	if err := json.Unmarshal(pv.View, &view); err != nil {
		t.Fatalf("视图反序列化失败: %v", err)
	}
	if len(view.Blocks) != limit {
		t.Fatalf("视图内块数应为 %d，得 %d", limit, len(view.Blocks))
	}
	newest := view.Blocks[len(view.Blocks)-1].ID
	if newest != uint64(windowBlocks) {
		t.Fatalf("尾部最后一块应是窗口最新的 id=%d，得 %d", windowBlocks, newest)
	}
}
