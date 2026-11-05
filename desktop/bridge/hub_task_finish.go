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
	st, err := m.hubStore()
	if err != nil {
		m.emitHubTaskAlert(ids[0], "settle_failed", err.Error())
		return
	}
	ledger := agenthub.NewLedger(st)
	status := agenthub.TaskCompleted
	switch {
	case ae.ErrorKind == "canceled" || ae.ErrorKind == "aborted":
		status = agenthub.TaskInterrupted
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
	settled, err := ledger.Finish(ctx, agenthub.FinishInput{
		TaskID: first, RunID: ae.RunId, Status: status, Result: res, Error: ae.Error,
		At: time.Now().UTC(),
	})
	if err != nil {
		m.emitHubTaskAlert(first, "settle_failed", err.Error())
	} else {
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
	if err := origin.s.PushTaskResultUsage(string(task.ID), task.Title, summary,
		string(task.Status), usage, pushErr); err != nil {
		m.emitHubTaskAlert(task.ID, "push_failed", err.Error())
		return
	}
	if _, err := ledger.MarkNotified(context.Background(), task.ID, ae.RunId); err != nil {
		m.emitHubTaskAlert(task.ID, "notify_failed", err.Error())
	}
}

// emitHubTaskUpdate 任务快照变化（任务板/卡片；P2-06 接前端）。
func (m *manager) emitHubTaskUpdate(t *agenthub.Task) {
	if m == nil || m.out == nil || t == nil {
		return
	}
	m.out.writeLine(map[string]any{
		"event_type": "hub_task_update",
		"task":       t,
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
