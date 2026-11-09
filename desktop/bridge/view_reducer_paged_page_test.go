package main

import (
	"testing"
)

// 窗口恢复（paged）时 ReadBlockPage 不得越界。
//
// 回归背景（2026-11-08 实测）：bridge 崩溃
//
//	index out of range
//	main.(*ViewReducer).ReadBlockPage.func1  view_reducer.go:4288
//	sort.Search(...)
//
// 根因与 MarshalTail 同一家族：窗口恢复时 total 是**历史总块数**（10294），
// 而 view 只装窗口内的块（496）。二分查找按 total 取范围 → 谓词索引 view[i]
// 越界 → bridge 直接死 → 桌面端表现为「新建会话无反应、历史会话发不出消息」
// （命令无人应答）。
//
// 触发条件：paged != nil 且游标落在**窗口内部**（beforeID > 窗口最早块的 id），
// 此时不走 readPagedPageLocked（那只管「游标早于窗口」），而是走 sort.Search 那条。
func TestReadBlockPagePagedWindowNoOverflow(t *testing.T) {
	const (
		windowBlocks = 496
		historyTotal = 10294
	)

	r := NewViewReducer()
	cp := &Checkpoint{
		BlocksTotal: historyTotal,
		PagedBlocks: &PagedBlocks{IDs: []uint64{1, 2, 3}}, // 窗口外的旧块（占位即可）
	}
	for i := 0; i < windowBlocks; i++ {
		cp.Blocks = append(cp.Blocks, Block{
			Kind: "assistant",
			ID:   uint64(i + 1),
			Text: "x",
		})
	}
	if err := r.Restore(cp); err != nil {
		t.Fatalf("Restore 失败: %v", err)
	}

	// 游标 250 落在窗口内部 → 走 sort.Search 分支（窗口外分支不接管）。
	blocks, _, hasMore, total, err := r.ReadBlockPage(250, 50)
	if err != nil {
		t.Fatalf("ReadBlockPage 报错: %v", err)
	}
	if len(blocks) != 50 {
		t.Fatalf("本页应有 50 块，得 %d", len(blocks))
	}
	if total != historyTotal {
		t.Fatalf("total 应为历史总块数 %d，得 %d", historyTotal, total)
	}
	if !hasMore {
		t.Fatal("hasMore 应为 true（窗口之前还有历史）")
	}
}
