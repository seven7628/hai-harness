package main

// hub_notify.go：任务终态回推的**异步**通知簿（P3b-08 B1）。
//
// 为什么必须异步：finishHubTask 在 bs.emit 里被调用，而 emit 持着该会话的
// dispatchMu。PushTaskResultUsage 不只是入队 —— 它还会同步 emit 一个
// TaskResultDelivered 事件，也就是去拿**发起者会话**的 dispatchMu。两个会话互为
// 上下游（A↔B 互派任务）时就是死锁：
//
//	A.emit 持 A.dispatchMu → 结算持 hubFinishMu → 等 B.dispatchMu
//	B.emit 持 B.dispatchMu → 结算等 hubFinishMu
//
// 所以结算窗口里只做「账本 Finish / 产物 / 结果卡（store-only）/ 事件投影」，
// 任何跨会话动作（PushTaskResultUsage / Ask）一律排到后台 goroutine，并且后台
// 不再持 hubFinishMu。恢复路径同理：它也可能与正在 emit 的会话并发。
//
// 去重与幂等（多层，缺一不可）：
//   - 通知簿按 task_id 串行（refcount 回收，不无界增长）：两个调度方（结算 +
//     恢复 + 取消）不会同时看到「还没回推」各推一份；
//   - 推之前仍查接收端 HasTaskResult（上次推成功但 MarkNotified 失败/进程崩溃）；
//   - 只有推成功才 MarkNotified；失败保留未通知状态，恢复时重推。
//
// 排队本身不去重（同一任务可能被排两次）：两次执行都会在通知簿里串行，
// 第二次看到 HasTaskResult/alreadyNotified 后只补标记与快照，不会重复入上下文。

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/seven7628/hai-harness/agenthub"
)

// hubNotifier 是 per-task 串行锁表 + 在途计数。
//
// inflight 在 schedule 时同步 +1、后台 goroutine 退出时 -1（关停拒绝也要回退），
// 所以「schedule 返回后 inflight==0」= 这次回推已经被处理完：测试据它做确定性
// 等待，业务侧也用它判断关停前是否还有未落地的唤醒。
type hubNotifier struct {
	mu       sync.Mutex
	per      map[agenthub.TaskID]*notifySlot
	inflight atomic.Int64
}

type notifySlot struct {
	mu   sync.Mutex
	refs int
}

// lock 取得某任务的串行权，返回幂等的解锁函数。
func (n *hubNotifier) lock(id agenthub.TaskID) func() {
	n.mu.Lock()
	if n.per == nil {
		n.per = make(map[agenthub.TaskID]*notifySlot)
	}
	slot := n.per[id]
	if slot == nil {
		slot = &notifySlot{}
		n.per[id] = slot
	}
	slot.refs++
	n.mu.Unlock()

	slot.mu.Lock()
	var once sync.Once
	return func() {
		slot.mu.Unlock()
		once.Do(func() {
			n.mu.Lock()
			slot.refs--
			if slot.refs <= 0 {
				delete(n.per, id)
			}
			n.mu.Unlock()
		})
	}
}

// scheduleTaskResultNotification 把「回推某任务终态」排到后台。
// 关停后 spawnHubBackground 直接拒绝：那时任何唤醒都没有意义，账本里
// 未通知的状态会由下次启动的恢复重推（P2-08）。
func (m *manager) scheduleTaskResultNotification(id agenthub.TaskID) {
	if m == nil || id == "" {
		return
	}
	n := &m.hubNotify
	n.inflight.Add(1)
	if !m.spawnHubBackground(func(ctx context.Context) {
		defer n.inflight.Add(-1)
		m.deliverTaskResult(ctx, id)
	}) {
		n.inflight.Add(-1) // 关停拒绝：没有 goroutine 会来还这笔账
	}
}

// deliverTaskResult 读回账本、按 target 种类回推。调用方是后台 goroutine。
func (m *manager) deliverTaskResult(ctx context.Context, id agenthub.TaskID) {
	if m == nil || id == "" {
		return
	}
	unlock := m.hubNotify.lock(id)
	defer unlock()
	if ctx.Err() != nil || m.hubShutdown.Load() {
		return
	}
	st, err := m.hubStore()
	if err != nil {
		m.emitHubTaskAlert(id, "push_failed", err.Error())
		return
	}
	task, err := st.LoadTask(ctx, id)
	if err != nil {
		m.emitHubTaskAlert(id, "push_failed", err.Error())
		return
	}
	// 排队到执行之间任务可能被重试/改派：只回推**终态**快照，避免把新一轮
	// 还在跑的任务当成已完成推给发起者。
	if task == nil || !task.Status.Terminal() {
		return
	}
	// 同一次结算可能排了多条通知（第二次 AgentEnd/恢复与首推并发）：
	// 已回推过的直接退出，别再用 delivered 快照刷一遍 UI。
	runID := task.Dispatch.RunID
	if runID == "" {
		runID = "recovery" // 没有真实 run（恢复合成）：MarkNotified 的占位键
	}
	if alreadyNotified(task, runID) {
		return
	}
	ledger := agenthub.NewLedger(st)
	if task.Target.Kind == agenthub.TargetAgent {
		m.pushAgentTaskResult(ledger, task)
		return
	}
	m.pushHubTaskResult(ledger, task, runID)
}

// ensureAgentTaskResultCard 写群内结果卡（幂等）。
// store-only：可以在 hubFinishMu / emit 的 dispatchMu 内调用，不碰其它会话。
func (m *manager) ensureAgentTaskResultCard(task *agenthub.Task) {
	if m == nil || task == nil || task.Target.Kind != agenthub.TargetAgent {
		return
	}
	if _, err := m.appendAgentTaskResultCard(task); err != nil {
		m.emitHubTaskAlert(task.ID, "task_result_card_failed", err.Error())
	}
}
