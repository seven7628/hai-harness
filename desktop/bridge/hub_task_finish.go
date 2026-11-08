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
	if m == nil || m.hubShutdown.Load() {
		return // 关停后的迟到结算不能复活 hub runtime/写 TempDir
	}
	if bs == nil || ae == nil {
		return
	}
	ids := bs.takeHubTasks()
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
	ctx := context.Background()
	first := ids[0]
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
		DurationMS:   ae.DurationMS,
		Error:        ae.Error,
		Artifacts:    taskArtifactsOf(ae.Artifacts),
	}
	finishErr := ae.Error
	// 运行中预算硬停：LLMEnd 已 Interrupt，AgentEnd 会以 aborted 收尾；这里把
	// 终态改回 blocked_budget，保留人/模型能看懂的原因。
	if bs.hubBudgetStop.Swap(false) {
		reason, _ := bs.hubBudgetReason.Load().(string)
		if reason == "" {
			reason = "日预算或单任务预算超限，运行已中止"
		}
		status = agenthub.TaskBlockedBudget
		res.Error = reason
		finishErr = reason
		m.emitHubTaskAlert(first, "blocked_budget", reason)
	}
	bs.hubTaskSpentMicro.Store(0)
	bs.hubBudgetReason.Store("")
	// 重放（同 run 已回推）：静默返回，不再 Finish（delivered→completed 会报假告警）。
	if cur, lerr := st.LoadTask(ctx, first); lerr == nil && alreadyNotified(cur, ae.RunId) {
		return
	}
	originTask, _ := st.LoadTask(ctx, first)
	// 已取消/已拒绝的任务没有回到工作态的边：迟到的 AgentEnd 不能把它改回
	// completed/failed（那会把终态覆盖成假结果），也不再写第二张卡/第二次回推。
	// 与下面 Finish 失败的 canceled 兜底同口径，只是提前判定、更安静。
	if originTask != nil &&
		(originTask.Status == agenthub.TaskCanceled || originTask.Status == agenthub.TaskDeclined) {
		return
	}
	// 单任务预算：成本只有在结算时才知道；超限落 blocked_budget + hub_alert（§6.9）。
	if originTask != nil && originTask.Origin.AgentID != "" {
		if p, _, perr := m.hubLoadAgent(string(originTask.Origin.AgentID)); perr == nil && p != nil {
			limits := agenthub.GuardLimits{MaxTaskCostUSD: p.Policy.MaxTaskCostUSD}
			if over, msg := limits.OverTaskBudget(res.Usage); over {
				status = agenthub.TaskBlockedBudget
				res.Error = msg
				finishErr = msg
				m.emitHubTaskAlert(first, "blocked_budget", msg)
			}
		}
	}
	// 无价用量：预算无法计入，必须在第一次见到时提醒（之后按智能体去重，避免刷屏）。
	if originTask != nil && !res.Usage.CostPriced && usageHasTokens(res.Usage) {
		m.warnUnpricedUsageOnce(first, originTask.Origin.AgentID)
	}
	settled, err := ledger.Finish(ctx, agenthub.FinishInput{
		TaskID: first, RunID: ae.RunId, Status: status, Result: res, Error: finishErr,
		HumanIntervened: humanIntervened,
		At:              time.Now().UTC(),
	})
	if err != nil {
		// 人已取消/目标已拒绝：AgentEnd 迟到不能再结算，也不要制造 settle_failed 假告警。
		if cur, lerr := st.LoadTask(ctx, first); lerr == nil &&
			(cur.Status == agenthub.TaskCanceled || cur.Status == agenthub.TaskDeclined) {
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
		if settled.Status == agenthub.TaskBlockedBudget {
			m.recordBudgetNeed(settled, res.Error)
		}
		switch settled.Target.Kind {
		case agenthub.TargetAgent:
			// 智能体任务：结果卡进群（持久感知）+ 只唤醒派发者（§6.5.7/§6.7.9）。
			m.notifyAgentTaskResult(ledger, settled)
		default:
			if !alreadyNotified(settled, ae.RunId) {
				m.pushHubTaskResult(ledger, settled, ae)
			}
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

// recordBudgetNeed 把预算阻断放进 Needs you（P2-07 N5）。同一 task 幂等。
func (m *manager) recordBudgetNeed(t *agenthub.Task, reason string) {
	if m == nil || t == nil {
		return
	}
	st, err := m.hubStore()
	if err != nil {
		return
	}
	id := agenthub.NeedID("need_budget_" + string(t.ID))
	ctx := context.Background()
	if existing, err := st.LoadNeed(ctx, id); err == nil && existing != nil {
		return
	}
	n := &agenthub.Need{
		ID: id, Kind: "budget", Status: "pending",
		AgentID: t.Origin.AgentID, GroupID: t.Origin.GroupID, TaskID: t.ID,
		Title: "任务预算阻断", Body: truncateRunes(reason, 2000),
		CreatedAt: time.Now().UTC(),
	}
	if err := st.SaveNeed(ctx, n); err == nil {
		m.emitHubNeed(n)
	}
}

// warnUnpricedUsageOnce 每个智能体只发一次「无价用量」告警。
func (m *manager) warnUnpricedUsageOnce(taskID agenthub.TaskID, agentID agenthub.AgentID) {
	if m == nil || agentID == "" {
		return
	}
	m.mu.Lock()
	if m.hubUnpricedWarned == nil {
		m.hubUnpricedWarned = map[agenthub.AgentID]bool{}
	}
	seen := m.hubUnpricedWarned[agentID]
	m.hubUnpricedWarned[agentID] = true
	m.mu.Unlock()
	if seen {
		return
	}
	m.emitHubTaskAlert(taskID, "usage_unpriced",
		"该智能体的用量没有价格（provider 未上报且价表无此模型），预算无法计入；请补价表或改用有价模型")
}

func usageHasTokens(u agenthub.Usage) bool {
	return u.Input > 0 || u.Output > 0 || u.CacheRead > 0 || u.CacheWrite > 0 || u.Reasoning > 0
}

// hubBudgetCheck 是目标会话 LLMEnd 的硬停判定：已结算日成本 + 本任务已花
// 是否越过智能体/群/全局日预算或单任务上限。只对当前 hub_task 生效。
func (m *manager) hubBudgetCheck(taskID agenthub.TaskID, spentUSD float64) (bool, string) {
	if m == nil || taskID == "" {
		return false, ""
	}
	st, err := m.hubStore()
	if err != nil {
		return false, ""
	}
	ctx := context.Background()
	t, err := st.LoadTask(ctx, taskID)
	if err != nil || t == nil || t.Origin.AgentID == "" {
		return false, ""
	}
	p, _, err := m.hubLoadAgent(string(t.Origin.AgentID))
	if err != nil || p == nil {
		return false, ""
	}
	limits := agenthub.GuardLimits{
		MaxTaskCostUSD:       p.Policy.MaxTaskCostUSD,
		DailyBudgetUSD:       p.Policy.DailyBudgetUSD,
		GlobalDailyBudgetUSD: agenthub.DefaultGlobalDailyBudgetUSD,
	}
	if t.Origin.GroupID != "" {
		if g, gerr := st.LoadGroup(ctx, t.Origin.GroupID); gerr == nil && g != nil {
			limits.GroupDailyBudgetUSD = g.Policy.DailyBudgetUSD
		}
	}
	if over, msg := limits.OverTaskBudget(agenthub.Usage{CostPriced: true, CostUSD: spentUSD}); over {
		return true, msg
	}
	agentDay, groupDay, globalDay := st.DailyCost(t.Origin.AgentID, t.Origin.GroupID, time.Now().UTC())
	if limits.GlobalDailyBudgetUSD > 0 && globalDay+spentUSD >= limits.GlobalDailyBudgetUSD {
		return true, fmt.Sprintf("全局日预算 $%.2f 已用满，运行已中止", limits.GlobalDailyBudgetUSD)
	}
	if limits.GroupDailyBudgetUSD > 0 && t.Origin.GroupID != "" && groupDay+spentUSD >= limits.GroupDailyBudgetUSD {
		return true, fmt.Sprintf("群日预算 $%.2f 已用满，运行已中止", limits.GroupDailyBudgetUSD)
	}
	if limits.DailyBudgetUSD > 0 && agentDay+spentUSD >= limits.DailyBudgetUSD {
		return true, fmt.Sprintf("智能体日预算 $%.2f 已用满，运行已中止", limits.DailyBudgetUSD)
	}
	return false, ""
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

// hubAlertEvent 是 hub_alert 的唯一构造参数（P2.5-05 D3-A：统一 envelope）。
// 所有生产点必须走 emitHubAlert，避免同一 code 在结算路径/护栏路径出现
// 不同的 severity 或缺 scope（P2.5-05 复审 B2）。
type hubAlertEvent struct {
	Scope    string // hub | task（空 = 按 TaskID 推导）
	Code     string
	Severity string // info | warning | error（空 = 按 Code 映射）
	TaskID   agenthub.TaskID
	AgentID  agenthub.AgentID
	GroupID  agenthub.GroupID
	Message  string
	At       time.Time
}

// hubAlertSeverity 显式映射；未知 code 默认 warning，但新 code 应在这里登记。
func hubAlertSeverity(code string) string {
	switch code {
	case "recovery_failed", "blocked_budget", "push_failed", "settle_failed",
		"artifact_register_failed", "batch_conflict", "notify_failed", "injected_conflict",
		"task_result_card_failed", "bad_header":
		return "error"
	case "usage_unpriced", "recovered_interrupted", "queue_unavailable",
		"dispatch_denied", "concurrency_limit", "duplicate_task", "chain_too_deep",
		"hub_background_timeout":
		return "warning"
	}
	return "warning"
}

// emitHubAlert 是 hub_alert 的唯一出口。
func (m *manager) emitHubAlert(a hubAlertEvent) {
	if m == nil || m.out == nil {
		return
	}
	if a.Code == "" {
		a.Code = "hub_alert"
	}
	scope := a.Scope
	if scope == "" {
		scope = "hub"
		if a.TaskID != "" {
			scope = "task"
		}
	}
	severity := a.Severity
	if severity == "" {
		severity = hubAlertSeverity(a.Code)
	}
	at := a.At
	if at.IsZero() {
		at = time.Now().UTC()
	}
	ev := map[string]any{
		"event_type": "hub_alert",
		"scope":      scope,
		"code":       a.Code,
		"kind":       a.Code, // 兼容旧消费者
		"severity":   severity,
		"message":    a.Message,
		"at":         at,
	}
	if a.TaskID != "" {
		ev["task_id"] = string(a.TaskID)
	}
	if a.AgentID != "" {
		ev["agent_id"] = string(a.AgentID)
	}
	if a.GroupID != "" {
		ev["group_id"] = string(a.GroupID)
	}
	m.out.writeLine(ev)
}

// emitHubTaskAlert 任务级告警的便捷入口：补 agent/group（旧调用方保持兼容）。
func (m *manager) emitHubTaskAlert(id agenthub.TaskID, kind, msg string) {
	a := hubAlertEvent{TaskID: id, Code: kind, Message: msg}
	if id != "" {
		if st, err := m.hubStore(); err == nil {
			if t, err := st.LoadTask(context.Background(), id); err == nil && t != nil {
				a.AgentID = t.Origin.AgentID
				a.GroupID = t.Origin.GroupID
			}
		}
	}
	m.emitHubAlert(a)
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
