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
	}
	ctx := context.Background()
	first := ids[0]
	if _, err := ledger.Finish(ctx, agenthub.FinishInput{
		TaskID: first, RunID: ae.RunId, Status: status, Result: res, Error: ae.Error,
		At: time.Now().UTC(),
	}); err != nil {
		m.emitHubTaskAlert(first, "settle_failed", err.Error())
	}
	// 一个 batch 出现多个 hub_task = 空闲闸门失效（§6.7.2 防御）：
	// 第一个正常结算，其余按 batch_conflict 失败并告警。
	for _, extra := range ids[1:] {
		_, _ = ledger.Finish(ctx, agenthub.FinishInput{
			TaskID: extra, RunID: ae.RunId, Status: agenthub.TaskFailed,
			Error: "batch_conflict: 同一批出现多个 hub_task（空闲闸门 bug）",
			At:    time.Now().UTC(),
		})
		m.emitHubTaskAlert(extra, "batch_conflict", "同一批出现多个 hub_task（空闲闸门 bug）")
	}
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
