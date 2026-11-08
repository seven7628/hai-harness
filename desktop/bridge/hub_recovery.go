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
	"strings"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
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
//
// queued 分支的 LoadTask 只是预筛：Queue.Restore 会在目标锁内再 Replay 一次，
// 只有仍为 queued 才入队（不要在 Restore 里依赖这里的快照，P2.5-03 B2）。
func (m *manager) recoverHubTasks() error {
	if m == nil || m.hubShutdown.Load() {
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
		// 锁内只结算/投影；回推排到锁外（P3b-08 B1：恢复也会与正在 emit 的
		// 会话并发，锁内同步 PushTaskResultUsage 就是和 dispatchMu 互等）。
		notify := m.recoverOneTaskLocked(ctx, ledger, t)
		m.hubFinishMu.Unlock()
		if notify != "" {
			m.scheduleTaskResultNotification(notify)
		}
	}
	return nil
}

// recoveryRunID 恢复期的确定性 run 占位：没有真实 run 时用 per-task 标记写进
// notified_run_ids，恢复重放才能幂等（P2；此前空 run 永远写不进账本，任务卡在
// completed 而不是 delivered）。真实 run 仍优先：避免真实 AgentEnd 迟到时重放
// （P2-05 NB-3）。deliverTaskResult 与这里必须用同一个推导。
func recoveryRunID(t *agenthub.Task) string {
	if t == nil {
		return ""
	}
	if t.Dispatch.RunID != "" {
		return t.Dispatch.RunID
	}
	return "recovery:" + string(t.ID)
}

// hubTaskSessionLive 报告任务的执行会话是否**仍在本进程里**跑着。
// 覆盖三种「还在路上」的形态：根 run 已启动（running）、已 Ask 但 root AgentStart
// 还没到（hubDispatchPending）、任务消息还在 inbox 里没被消费（PendingCount>0）。
// 只看消费端状态不够：恢复发生在启动早期，没装配的会话一律视为没有活 run。
func (m *manager) hubTaskSessionLive(t *agenthub.Task) bool {
	if m == nil || t == nil || t.Target.SessionID == "" {
		return false
	}
	bs := m.sessionByID(t.Target.SessionID)
	if bs == nil {
		return false
	}
	// 工作区会话的 sid 可能在不同工作区重复：必须连 workspace 一起对。
	if ws := strings.TrimSpace(t.Target.Workspace); ws != "" && bs.workspace != ws {
		return false
	}
	return bs.running.Load() || bs.hubDispatchPending.Load() || bs.s.PendingCount() > 0
}

// recoverOneTaskLocked 处理单条任务；调用方必须持有 hubFinishMu。
// 返回需要异步回推的任务 id（空 = 不需要）。
//
// 结果卡只在**终态确定之后**写（P1）：以前在 switch 之前就写，dispatched/running
// 的任务会先落一张非终态卡；卡的 client_msg_id 是确定性的（task_result:<id>），
// 之后真正的终态卡被去重，群里永远停在 dispatched/running。
func (m *manager) recoverOneTaskLocked(ctx context.Context, ledger *agenthub.Ledger, t *agenthub.Task) agenthub.TaskID {
	if t == nil {
		return ""
	}
	switch t.Status {
	case agenthub.TaskQueued:
		return ""
	case agenthub.TaskDispatched, agenthub.TaskRunning:
		// 本进程里还有活 run：交给事件流正常收尾（不能误标中断）。
		if m.hubTaskSessionLive(t) {
			return ""
		}
		reason := "进程重启：没有对应的运行记录"
		if t.Dispatch.RunID != "" {
			reason = "进程重启：运行记录已丢失（run " + t.Dispatch.RunID + "）"
		}
		updated, terr := ledger.Transition(ctx, t.ID, agenthub.TaskInterrupted, agenthub.TransitionOptions{
			At: time.Now().UTC(), Error: reason,
		})
		if terr != nil {
			m.emitHubTaskAlert(t.ID, "recovery_failed", terr.Error())
			return ""
		}
		m.emitHubTaskUpdate(updated)
		m.emitHubTaskAlert(t.ID, "recovered_interrupted", reason)
		m.openRecoveryNeed(updated)
		// 终态已确定：补一张 interrupted 结果卡（此前从此不写卡 → 群里停在旧状态）。
		m.ensureAgentTaskResultCard(updated)
		if alreadyNotified(updated, recoveryRunID(updated)) {
			return ""
		}
		return updated.ID
	case agenthub.TaskCompleted, agenthub.TaskFailed, agenthub.TaskInterrupted, agenthub.TaskBlockedBudget:
		m.ensureAgentTaskResultCard(t)
		if alreadyNotified(t, recoveryRunID(t)) {
			return ""
		}
		return t.ID
	default:
		// canceled/declined 等终态：卡可以补（幂等），但没有 delivered 出边，
		// 不重推（否则每次重启都唤醒一次派发者）。
		m.ensureAgentTaskResultCard(t)
	}
	return ""
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
