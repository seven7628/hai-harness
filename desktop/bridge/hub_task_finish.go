package main

// hub_task_finish.go：Hub 派发任务的结算（方案 §6.7，P2-04）。
//
// 相关性由 hub_task 内容块给出（派发时写、UserInputsConsumed 时绑定），
// 这里只做 AgentEnd 结算：账本终态 + 用量/产物快照。结果回推是 P2-05。
//
// 只在**根 run**（ParentRunId=="" && Depth==0）结算：子 agent 的 AgentEnd 也走
// 同一个 emit，但那是内部步骤，不是任务产出。

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// finishHubTask 把本批 hub_task 结算进账本。
func (m *manager) finishHubTask(bs *bridgeSession, ae *events.AgentEnd) {
	if m == nil || bs == nil || ae == nil {
		return
	}
	ids := bs.hubTaskIDs
	bs.hubTaskIDs = nil
	if len(ids) == 0 {
		return
	}
	humanIntervened := bs.hubHumanIntervened.Swap(false)
	m.hubFinishMu.Lock()
	defer m.hubFinishMu.Unlock()
	st, err := m.hubStore()
	if err != nil {
		m.emitHubTaskAlert(ids[0], "settle_failed", err.Error())
		return
	}
	ledger := agenthub.NewLedger(st)
	status := agenthub.TaskCompleted
	switch {
	case ae.FinishReason == core.FinishReasonAbort || ae.ErrorKind == "canceled" || ae.ErrorKind == "aborted":
		status = agenthub.TaskInterrupted
	case ae.FinishReason == core.FinishReasonMaxIterations:
		status = agenthub.TaskFailed // 轮数护栏截断：没有最终答复，不能算完成
	case ae.FinishReason == core.FinishReasonError || ae.Error != "":
		status = agenthub.TaskFailed
	}
	res := &agenthub.TaskResult{
		FinishReason: string(ae.FinishReason),
		Summary:      truncateRunes(ae.Content, 4000),
		Usage:        taskUsageOf(ae.Usage),
		Error:        ae.Error,
		Artifacts:    taskArtifactsOf(ae.Artifacts),
	}
	ctx := context.Background()
	first := ids[0]
	// 重放（同 run 已回推）：静默返回，不再 Finish（delivered→completed 会报假告警）。
	if cur, lerr := st.LoadTask(ctx, first); lerr == nil && alreadyNotified(cur, ae.RunId) {
		return
	}
	// 单任务预算：成本只有在结算时才知道；超限落 blocked_budget + hub_alert（§6.9）。
	if originTask, terr := st.LoadTask(ctx, first); terr == nil && originTask.Origin.AgentID != "" {
		if p, _, perr := m.hubLoadAgent(string(originTask.Origin.AgentID)); perr == nil && p != nil {
			limits := agenthub.GuardLimits{MaxTaskCostUSD: p.Policy.MaxTaskCostUSD}
			if over, msg := limits.OverTaskBudget(res.Usage); over {
				status = agenthub.TaskBlockedBudget
				res.Error = msg
				m.emitHubTaskAlert(first, "blocked_budget", msg)
			}
		}
	}
	settled, err := ledger.Finish(ctx, agenthub.FinishInput{
		TaskID: first, RunID: ae.RunId, Status: status, Result: res, Error: ae.Error,
		HumanIntervened: humanIntervened,
		At:              time.Now().UTC(),
	})
	if err != nil {
		// 人已取消：AgentEnd 迟到不能再结算，也不要制造 settle_failed 假告警。
		if cur, lerr := st.LoadTask(ctx, first); lerr == nil && cur.Status == agenthub.TaskCanceled {
			return
		}
		m.emitHubTaskAlert(first, "settle_failed", err.Error())
	} else {
		// 产物登记：AgentEnd 是 Hub 产物的唯一生产入口，版本分配由
		// agenthub.RegisterArtifact 串行化（P3a-07 复审 B1）。
		if err := registerTaskArtifacts(ctx, st, settled, ae.Artifacts); err != nil {
			m.emitHubTaskAlert(settled.ID, "artifact_register_failed", err.Error())
		}
		m.emitHubTaskUpdate(settled)
		if !alreadyNotified(settled, ae.RunId) {
			m.pushHubTaskResult(ledger, settled, ae)
		}
	}
	// 一个 batch 出现多个 hub_task = 空闲闸门失效（§6.7.2 防御）：
	// 第一个正常结算，其余按 batch_conflict 失败并告警。
	for _, extra := range ids[1:] {
		_, _ = ledger.Finish(ctx, agenthub.FinishInput{
			TaskID: extra, RunID: ae.RunId, Status: agenthub.TaskFailed,
			Error: "batch_conflict: 同一批出现多个 hub_task（空闲闸门 bug）",
			At:    time.Now().UTC(),
		})
		if t, lerr := st.LoadTask(ctx, extra); lerr == nil {
			m.emitHubTaskUpdate(t)
		}
		m.emitHubTaskAlert(extra, "batch_conflict", "同一批出现多个 hub_task（空闲闸门 bug）")
	}
}

// alreadyNotified 同一 run 已回推过（幂等：重复 AgentEnd/事件重放不重复推）。
func alreadyNotified(t *agenthub.Task, runID string) bool {
	if t == nil || runID == "" {
		return false
	}
	for _, r := range t.NotifiedRuns {
		if r == runID {
			return true
		}
	}
	return false
}

// pushHubTaskResult 把终态回推给发起智能体的 DM 会话（P2-05）。
//
// 复用 Session.PushTaskResultUsage：入队 task_result 消息（模型续跑）+ TaskResultDelivered
// 事件（UI 任务卡终态收敛）。回推成功才 MarkNotified（幂等：同一 run 只推一次）；
// 失败保留未通知状态并告警，崩溃恢复（P2-08）据此重推。
func (m *manager) pushHubTaskResult(ledger *agenthub.Ledger, task *agenthub.Task, ae *events.AgentEnd) {
	if task == nil || task.Origin.Kind != agenthub.OriginDM || task.Origin.AgentID == "" {
		return // 群/人工来源的回推归 P3
	}
	origin, err := m.hubDMSession(string(task.Origin.AgentID))
	if err != nil || origin == nil {
		m.emitHubTaskAlert(task.ID, "push_failed", fmt.Sprintf("无法打开发起会话: %v", err))
		return
	}
	runID := ""
	if ae != nil {
		runID = ae.RunId
	}
	// 接收端幂等：若 id 已进入发起者 history（上次 push 成功但 MarkNotified
	// 失败/进程在这两步之间崩溃），不再推第二份；只补标记并重发 delivered
	// 快照（P2-05 复审 B-1）。
	// checkpoint-first 重启只恢复 messages、不恢复 history（session.go 的 record
	// 分支），所以两处都要查；否则崩溃窗口会在模型上下文里出现第二份结果。
	if origin.s.HasTaskResult(string(task.ID)) {
		if _, err := ledger.MarkNotified(context.Background(), task.ID, runID); err != nil {
			m.emitHubTaskAlert(task.ID, "notify_failed", err.Error())
			return
		}
		if latest, lerr := ledger.Replay(context.Background(), task.ID); lerr == nil {
			m.emitHubTaskUpdate(latest)
		}
		return
	}
	var usage core.Usage
	if ae != nil && ae.Usage != nil {
		usage = *ae.Usage
	}
	var pushErr error
	if task.Status == agenthub.TaskFailed || task.Status == agenthub.TaskInterrupted {
		pushErr = errors.New(task.Error)
	}
	summary := ""
	if task.Result != nil {
		summary = task.Result.Summary
	}
	// 预算阻断不是失败：正文仍是任务产出，但必须把「为什么停」带给模型。
	// 不能塞 pushErr（那会用错误文本整段替换摘要），也不能只说「完成」——
	// 模型会被指示继续，而重试又绕过 Guard（P2-07 复审 N1）。
	if task.Status == agenthub.TaskBlockedBudget {
		reason := task.Error
		if reason == "" && task.Result != nil {
			reason = task.Result.Error
		}
		if reason != "" {
			if summary != "" {
				summary += "\n\n"
			}
			summary += "已阻断：" + reason
		}
	}
	// §6.7.5：人在任务运行中介入过，回推必须让模型先对齐人的最新意图，
	// 而不是按原计划自动继续。
	if task.HumanIntervened {
		if summary != "" {
			summary += "\n\n"
		}
		summary += "—— 人类在该任务运行中介入了。先确认用户的最新意图，不要自动继续原计划。"
	}
	if err := origin.s.PushTaskResultUsage(string(task.ID), task.Title, summary,
		string(task.Status), usage, pushErr); err != nil {
		m.emitHubTaskAlert(task.ID, "push_failed", err.Error())
		return
	}
	if _, err := ledger.MarkNotified(context.Background(), task.ID, runID); err != nil {
		m.emitHubTaskAlert(task.ID, "notify_failed", err.Error())
		return
	}
	// delivered 是 completed/failed/interrupted 的权威终态：再发一次快照，前端才能
	// 区分「已回推」与「未送达」。blocked_budget 保留自己的状态（状态机无出边），
	// 已回推与否看 notified_run_ids（P2-07 复审 N2）。
	if latest, lerr := ledger.Replay(context.Background(), task.ID); lerr == nil {
		m.emitHubTaskUpdate(latest)
	}
}

// taskResultInHistory 发起者 history 是否已有该任务的 task_result。
// task_result 只承载终态结果，任务 id 唯一，故 task_id 就是投递幂等键。
func taskResultInHistory(msgs []core.Message, id agenthub.TaskID) bool {
	prefix := "〔后台任务 " + string(id) + " "
	for _, msg := range msgs {
		for _, c := range msg.Content {
			if c.Type == core.ContentTypeTaskResult && strings.HasPrefix(c.Content, prefix) {
				return true
			}
		}
	}
	return false
}

// emitHubTaskUpdate 任务快照变化（任务板/卡片；P2-06 接前端）。
func (m *manager) emitHubTaskUpdate(t *agenthub.Task) {
	if m == nil || m.out == nil || t == nil {
		return
	}
	// §7.2/§7.3.3 契约：以 task_id 为 key 摊平字段；保留 task 供任务板整条渲染。
	m.out.writeLine(map[string]any{
		"event_type": "hub_task_update", "task_id": string(t.ID), "task": t,
		"status": t.Status, "target_kind": t.Target.Kind,
		"group_id": t.Target.GroupID, "agent_id": t.Origin.AgentID,
		"title": t.Title, "error": t.Error,
	})
}

// taskArtifactsOf core.Artifact → 账本产物快照。
func taskArtifactsOf(in []core.Artifact) []agenthub.TaskArtifact {
	if len(in) == 0 {
		return nil
	}
	out := make([]agenthub.TaskArtifact, 0, len(in))
	for _, a := range in {
		out = append(out, agenthub.TaskArtifact{
			Name: a.Name, Path: a.Path, Kind: a.Kind, Added: a.Added, Removed: a.Removed,
			Size: a.Size, Lines: a.Lines, SHA256: a.SHA256,
		})
	}
	return out
}

// emitHubTaskAlert 任务结算异常（前端 Needs you / 任务板用；P2-06 接）。
func (m *manager) emitHubTaskAlert(id agenthub.TaskID, kind, msg string) {
	if m == nil || m.out == nil {
		return
	}
	m.out.writeLine(map[string]any{
		"event_type": "hub_alert",
		"task_id":    string(id),
		"kind":       kind,
		"message":    msg,
	})
}

// taskUsageOf core.Usage → 账本 DTO（CostPriced 区分「不知道」与「免费」）。
func taskUsageOf(u *core.Usage) agenthub.Usage {
	if u == nil {
		return agenthub.Usage{}
	}
	return agenthub.Usage{
		Input: u.Input, Output: u.Output,
		CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, CacheWrite1h: u.CacheWrite1h,
		Reasoning:  u.Reasoning,
		CostPriced: u.Cost.Priced,
		CostUSD:    float64(u.Cost.Total) / 1e6,
	}
}

// failHubTasksByReason 把被注入消费/坏 header 的 hub_task 标 failed（不再静默）。
func (m *manager) failHubTasksByReason(ids []agenthub.TaskID, reason string) {
	if m == nil || len(ids) == 0 {
		return
	}
	st, err := m.hubStore()
	if err != nil {
		return
	}
	ledger := agenthub.NewLedger(st)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, id := range ids {
		t, err := ledger.Replay(ctx, id)
		if err != nil {
			m.emitHubTaskAlert(id, "settle_failed", err.Error())
			continue
		}
		if t.Status.Terminal() {
			continue
		}
		if _, err := ledger.Finish(ctx, agenthub.FinishInput{
			TaskID: id, RunID: "dispatch-conflict", Status: agenthub.TaskFailed, Error: reason,
		}); err != nil {
			// queued 不能直接 Finish：取消后再标失败。
			if _, cerr := ledger.Transition(ctx, id, agenthub.TaskCanceled, agenthub.TransitionOptions{At: time.Now().UTC(), Error: reason}); cerr != nil {
				m.emitHubTaskAlert(id, "settle_failed", err.Error())
				continue
			}
		}
		m.emitHubTaskAlert(id, "injected_conflict", reason)
		if latest, rerr := ledger.Replay(ctx, id); rerr == nil {
			m.emitHubTaskUpdate(latest)
		}
	}
}
