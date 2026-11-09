package subagent

// 注册表不再无界增长（2026-11-09 事故的修复目标）。
//
// 事故：注册表的任务表只增不减（全包没有一处 delete）→ 进程长跑时涨到 65 万条
// （bridge 内存 2.5GB），且 List/TotalUsage/AbandonAll 每次遍历全表（还在持有
// r.mu 时遍历 → 别的路径全排队；实测有 goroutine 在 r.mu 上等了 75 分钟，
// 整条 dispatch 停摆 → 桌面端「所有工作区会话数都是 0、无法切会话」）。
//
// 修复：已结束任务的对象被回收（终态摘要进 finished，用量累计进 accumulatedUsage），
// 完整内容按 task id 从 journal 还原（宿主注入的回落器）。

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
)

// TestFinishedTasksAreReclaimed 验证「已结束任务的对象被回收」，
// 且**信息不丢**（终态摘要 + 用量累计）。
func TestFinishedTasksAreReclaimed(t *testing.T) {
	r := NewRegistry()

	const n = 500
	for i := 0; i < n; i++ {
		task, err := r.Start(context.Background(), fmt.Sprintf("t%d", i), func(ctx context.Context, t *Task) (string, core.Usage, error) {
			return "done", core.Usage{Input: 10, Output: 20}, nil
		})
		if err != nil {
			t.Fatalf("Start 失败: %v", err)
		}
		select {
		case <-task.done:
		case <-time.After(2 * time.Second):
			t.Fatalf("任务 %d 未在 2s 内结束", i)
		}
	}

	r.mu.Lock()
	live := len(r.tasks)
	finished := len(r.finished)
	acc := r.accumulatedUsage
	r.mu.Unlock()

	t.Logf("起 %d 个已结束任务 → 仍在跑 %d 个，终态摘要 %d 条，累计用量 in=%d out=%d",
		n, live, finished, acc.Input, acc.Output)

	if live != 0 {
		t.Fatalf("已结束任务的对象未被回收：tasks 里还剩 %d 个（应为空）", live)
	}
	if finished != n {
		t.Fatalf("终态摘要应有 %d 条，得 %d", n, finished)
	}
	// 用量不丢账：回收后 TotalUsage 仍要等于所有任务的用量之和。
	if acc.Input != n*10 || acc.Output != n*20 {
		t.Fatalf("回收丢账：累计用量 in=%d out=%d，期望 in=%d out=%d",
			acc.Input, acc.Output, n*10, n*20)
	}
	total := r.TotalUsage()
	if total.Input != n*10 || total.Output != n*20 {
		t.Fatalf("TotalUsage 不含已回收任务：in=%d out=%d，期望 in=%d out=%d",
			total.Input, total.Output, n*10, n*20)
	}
	// 回收后仍能按 id 查到（终态摘要）——List 里也要能看到。
	infos := r.List()
	if len(infos) != n {
		t.Fatalf("List 应返回 %d 条（终态摘要代替对象），得 %d", n, len(infos))
	}
}
