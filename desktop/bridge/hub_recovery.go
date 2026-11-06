package main

// hub_recovery.go：崩溃/重启恢复（§6.12，P2-08）。
//
// 口径：
//   - queued：放回内存队列（Restore），不立即派发；下一条根 AgentEnd/下次 Pump 再走闸门。
//   - dispatched/running 但 Dispatch.RunID 为空：没有真实 run，标 interrupted + 告警（不自动重跑，
//     避免副作用执行两次）；结果按失败回推发起者。
//   - completed/failed/interrupted 且未 notified：重推结果（MarkNotified 幂等，重复恢复不双推）。
//   - 不自动 retry 失败任务（retry 必须是人/智能体显式动作）。

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// ensureHubRecovery 首次 hub_open 做一次恢复（进程内只在成功后闩住）。
// 失败不能闩：一个半写 task.json 就让整轮恢复中止，如果不重试，本进程后续
// hub_open 全部 no-op，queued 永不派发、未回推终态永不重推（P2-08 复审 B-2）。
func (m *manager) ensureHubRecovery() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.hubRecovered || m.hubRecovering {
		m.mu.Unlock()
		return
	}
	m.hubRecovering = true
	m.mu.Unlock()
	var err error
	func() {
		// defer：recoverHubTasks panic 时也必须放开 hubRecovering，否则本进程
		// 后续 hub_open 全部 no-op（P2-08 非阻塞）。
		defer func() {
			m.mu.Lock()
			m.hubRecovering = false
			if err == nil {
				m.hubRecovered = true
			}
			m.mu.Unlock()
		}()
		err = m.recoverHubTasks()
	}()
	if err != nil {
		m.emitHubTaskAlert("", "recovery_failed", err.Error())
	}
}

// recoverHubTasks 扫描账本做一次性恢复；可重复调用（幂等）。
//
// P2.5-05：不再全程持 hubFinishMu —— 扫描/排队在锁外，只有「单任务重读 →
// 判定 → 回推 → MarkNotified」这一小段持锁。恢复 1000 个任务时，正常结算
// 不会被整轮恢复阻塞；每个任务处理前重读账本，仍与 finishHubTask 串行。
func (m *manager) recoverHubTasks() error {
	if m == nil {
		return nil
	}
	st, err := m.hubStore()
	if err != nil {
		return fmt.Errorf("打开 Hub 账本: %w", err)
	}
	ctx := context.Background()
	tasks, err := st.ListTasks(ctx, agenthub.TaskFilter{})
	if err != nil {
		return fmt.Errorf("扫描任务账本: %w", err)
	}
	// ListTasks 按 UpdatedAt 降序；直接 Restore 会让重启后的排队变 LIFO。
	// 按 CreatedAt 升序恢复，保持用户提交顺序（P2-08 复审 B-1）。
	var q *agenthub.Queue
	for _, t := range queuedInCreatedOrder(tasks) {
		// 扫描后可能已被并发 Pump 派走：Restore 前再读一次，避免把 dispatched
		// 任务重新塞回 pending（Queue.Restore 只认传入快照的状态）。
		cur, lerr := st.LoadTask(ctx, t.ID)
		if lerr != nil || cur == nil || cur.Status != agenthub.TaskQueued {
			continue
		}
		if q == nil {
			q, err = m.hubQueueFor()
			if err != nil {
				return fmt.Errorf("打开 Hub 队列: %w", err)
			}
		}
		q.Restore(cur)
	}
	ledger := agenthub.NewLedger(st)
	for _, snap := range tasks {
		if snap == nil || snap.Status == agenthub.TaskQueued {
			continue // 已按顺序 Restore
		}
		m.hubFinishMu.Lock()
		t, lerr := st.LoadTask(ctx, snap.ID)
		if lerr != nil {
			m.emitHubTaskAlert(snap.ID, "recovery_failed", lerr.Error())
			m.hubFinishMu.Unlock()
			continue
		}
		m.recoverOneTaskLocked(ctx, ledger, t)
		m.hubFinishMu.Unlock()
	}
	return nil
}

// recoverOneTaskLocked 处理单条任务；调用方必须持有 hubFinishMu。
func (m *manager) recoverOneTaskLocked(ctx context.Context, ledger *agenthub.Ledger, t *agenthub.Task) {
	if t == nil {
		return
	}
	switch t.Status {
	case agenthub.TaskQueued:
		return
	case agenthub.TaskDispatched, agenthub.TaskRunning:
		if t.Dispatch.RunID != "" {
			return // 有真实 run：由事件流正常收尾
		}
		updated, terr := ledger.Transition(ctx, t.ID, agenthub.TaskInterrupted, agenthub.TransitionOptions{
			At: time.Now().UTC(), Error: "进程重启：没有对应的运行记录",
		})
		if terr != nil {
			m.emitHubTaskAlert(t.ID, "recovery_failed", terr.Error())
			return
		}
		m.emitHubTaskUpdate(updated)
		m.emitHubTaskAlert(t.ID, "recovered_interrupted", "进程重启：任务没有对应的运行记录，已标中断")
		m.openRecoveryNeed(updated)
		// 结果回推发起者；已通知过则跳过。
		if !alreadyNotified(updated, "recovery") {
			m.pushHubTaskResult(ledger, updated, recoveryAgentEnd(updated))
		}
	case agenthub.TaskCompleted, agenthub.TaskFailed, agenthub.TaskInterrupted, agenthub.TaskBlockedBudget:
		runID := t.Dispatch.RunID
		if runID == "" {
			runID = "recovery"
		}
		if alreadyNotified(t, runID) {
			return
		}
		ae := recoveryAgentEnd(t)
		ae.RunId = runID // 用真实 run 占位，避免恢复写 "recovery" 后真实 run 迟到重放（P2-05 NB-3）
		m.pushHubTaskResult(ledger, t, ae)
	}
}

// queuedInCreatedOrder 恢复 queued 任务时的 FIFO 顺序。ListTasks 按 UpdatedAt
// 降序返回，不能直接拿来做 Restore；CreatedAt 相同（旧数据/同毫秒）用 ID 兜底。
func queuedInCreatedOrder(tasks []*agenthub.Task) []*agenthub.Task {
	out := make([]*agenthub.Task, 0, len(tasks))
	for _, t := range tasks {
		if t != nil && t.Status == agenthub.TaskQueued {
			out = append(out, t)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// recoveryAgentEnd 用账本里已有的结果合成 AgentEnd（只用于回推 usage/摘要）。
func recoveryAgentEnd(t *agenthub.Task) *events.AgentEnd {
	ae := &events.AgentEnd{RunId: "recovery", EventType: events.AgentEndType, Timestamp: time.Now()}
	if t == nil || t.Result == nil {
		return ae
	}
	ae.Content = t.Result.Summary
	ae.FinishReason = core.FinishReason(t.Result.FinishReason)
	ae.Error = t.Result.Error
	u := t.Result.Usage
	ae.Usage = &core.Usage{
		Input: u.Input, Output: u.Output, CacheRead: u.CacheRead,
		CacheWrite: u.CacheWrite, CacheWrite1h: u.CacheWrite1h, Reasoning: u.Reasoning,
		Cost: core.Cost{Priced: u.CostPriced, Total: int64(u.CostUSD * 1e6)},
	}
	return ae
}

// openRecoveryNeed 恢复失败进 Needs you（kind=failure，锚 = 任务 id）。
func (m *manager) openRecoveryNeed(t *agenthub.Task) {
	if m == nil || t == nil {
		return
	}
	st, err := m.hubStore()
	if err != nil {
		return
	}
	id := agenthub.NeedID("need_recovery_" + string(t.ID))
	ctx := context.Background()
	if _, err := st.LoadNeed(ctx, id); err == nil {
		return
	}
	n := &agenthub.Need{
		ID: id, Kind: "failure", Status: "pending",
		AgentID: t.Origin.AgentID, TaskID: t.ID,
		SessionID: t.Target.SessionID, Workspace: t.Target.Workspace,
		Title:     fmt.Sprintf("任务中断：%s", t.Title),
		Body:      "进程重启时没有找到对应的运行记录，任务已标记为中断。需要重试请手动执行 hub_task_retry。",
		CreatedAt: time.Now().UTC(),
	}
	if err := st.SaveNeed(ctx, n); err == nil {
		m.emitHubNeed(n)
	}
}
