package subagent

import (
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
)

// 注册表锁纪律回归：r.mu 绝不能在「等某个任务的 t.mu」期间被持有。
//
// 2026-11-09 实测事故：bridge 连续跑 18h 后，3 个 goroutine 卡在
// Registry.get → r.mu.Lock() 上 75 分钟，bridge 的串行 dispatch 停摆 →
// 桌面端「所有工作区会话数都是 0、无法切会话」。
//
// 根因：List()（以及 AbandonAll/TotalUsage）原来在**持有 r.mu** 的情况下
// 遍历全部任务并逐个 t.mu.Lock()。任务表只增不减（r.tasks 无 delete），
// 于是只要有一个任务的 t.mu 被占住，r.mu 就被长期持有 —— 而
// ParentTaskIDOf（宿主给终态事件标注归属的必经路径）走的正是 Registry.get。
//
// 本测试复刻那个场景：人为占住某个任务的 t.mu，然后确认
//   ① List() 会阻塞在那个任务上（这是预期的：它要读该任务的状态），但
//   ② **r.mu 必须是放开的** —— 另一个 goroutine 调 Registry.get 必须立刻返回。
// 旧实现下 ② 会超时（本测试失败）；新实现下 ② 立即成功。
func TestListDoesNotHoldRegistryLockWhileWaitingOnTask(t *testing.T) {
	r := NewRegistry()

	// 造一个任务并占住它的 t.mu（模拟「卡住的任务」）。
	task := &Task{ID: "t-stuck", Name: "stuck", Status: TaskRunning, done: make(chan struct{}), inbox: make(chan core.Message, 1)}
	r.mu.Lock()
	r.tasks[task.ID] = task
	r.mu.Unlock()

	task.mu.Lock() // 占住不放（末尾显式释放，见下）

	// List() 会卡在这个任务的 t.mu 上（等它释放）——这是预期的。
	listDone := make(chan struct{})
	go func() {
		defer close(listDone)
		_ = r.List()
	}()

	// 等 List 确实进去了（它已经在等 task.mu）
	time.Sleep(100 * time.Millisecond)

	// 关键断言：r.mu 没被 List 持有 → get 立刻返回。
	got := make(chan *Task, 1)
	go func() {
		tk, err := r.get(task.ID)
		if err == nil {
			got <- tk
		}
	}()

	select {
	case <-got:
		// 通过：List 等任务时没有占住注册表锁
	case <-time.After(2 * time.Second):
		t.Fatal("Registry.get 被 List 阻塞：List 持 r.mu 等 t.mu —— 锁纪律回归")
	}

	// 清理：释放任务锁，让 List 收尾
	task.mu.Unlock()
	select {
	case <-listDone:
	case <-time.After(2 * time.Second):
		t.Fatal("List 未在任务锁释放后返回")
	}
}
