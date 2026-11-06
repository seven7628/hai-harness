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
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// ensureHubRecovery 首次 hub_open 做一次恢复（进程内只跑一次）。
func (m *manager) ensureHubRecovery() {
	if m == nil {
		return
	}
	m.mu.Lock()
	if m.hubRecovered {
		m.mu.Unlock()
		return
	}
	m.hubRecovered = true
	m.mu.Unlock()
	m.recoverHubTasks()
}

// recoverHubTasks 扫描账本做一次性恢复；可重复调用（幂等）。
func (m *manager) recoverHubTasks() {
	if m == nil {
		return
	}
	st, err := m.hubStore()
	if err != nil {
		return
	}
	q, qerr := m.hubQueueFor()
	ctx := context.Background()
	tasks, err := st.ListTasks(ctx, agenthub.TaskFilter{})
	if err != nil {
		return
	}
	ledger := agenthub.NewLedger(st)
	for _, t := range tasks {
		if t == nil {
			continue
		}
		switch t.Status {
		case agenthub.TaskQueued:
			if qerr == nil && q != nil {
				q.Restore(t)
			}
		case agenthub.TaskDispatched, agenthub.TaskRunning:
			if t.Dispatch.RunID != "" {
				continue // 有真实 run：由事件流正常收尾
			}
			updated, terr := ledger.Transition(ctx, t.ID, agenthub.TaskInterrupted, agenthub.TransitionOptions{
				At: time.Now().UTC(), Error: "进程重启：没有对应的运行记录",
			})
			if terr != nil {
				m.emitHubTaskAlert(t.ID, "recovery_failed", terr.Error())
				continue
			}
			m.emitHubTaskUpdate(updated)
			m.emitHubTaskAlert(t.ID, "recovered_interrupted", "进程重启：任务没有对应的运行记录，已标中断")
			m.openRecoveryNeed(updated)
			// 结果回推发起者；已通知过则跳过。
			if !alreadyNotified(updated, "recovery") {
				m.pushHubTaskResult(ledger, updated, recoveryAgentEnd(updated))
			}
		case agenthub.TaskCompleted, agenthub.TaskFailed, agenthub.TaskInterrupted:
			runID := t.Dispatch.RunID
			if runID == "" {
				runID = "recovery"
			}
			if alreadyNotified(t, runID) {
				continue
			}
			ae := recoveryAgentEnd(t)
			ae.RunId = runID // 用真实 run 占位，避免恢复写 "recovery" 后真实 run 迟到重放（P2-05 NB-3）
			m.pushHubTaskResult(ledger, t, ae)
		}
	}
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
