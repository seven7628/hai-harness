package main

// hub_task_result.go：智能体任务（target=agent）的群内结果卡与派发者回推（P3b-05）。
//
// 方案 §6.5.7 的三层感知在这里落地：
//   - 实时：hub_task_update 更新群内任务卡（emitHubTaskUpdate）；
//   - 持久：群 task_result 消息写进 log.jsonl（appendAgentTaskResultCard）；
//   - 唤醒：只回推**派发者**（pushAgentTaskResult → Session.PushTaskResultUsage）。
//
// 结果卡不是对话消息：不带 mentions、不写 routing 回执、不触发任何唤醒。摘要里
// 即使出现 @Name 也不能把别人叫醒（§6.7.9 防循环：要找人必须显式 group_say）。

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
)

// agentTaskResultClientID 是结果卡的确定性幂等键：同一任务永远只有一张卡。
func agentTaskResultClientID(id agenthub.TaskID) string {
	return "task_result:" + string(id)
}

// notifyAgentTaskResult = 持久层（群内结果卡）+ 唤醒层（只派发者）。
// 卡写失败不阻断回推：结论在账本里，派发者必须知道结果（失败只留一条告警）。
func (m *manager) notifyAgentTaskResult(ledger *agenthub.Ledger, task *agenthub.Task) {
	if m == nil || task == nil || task.Target.Kind != agenthub.TargetAgent {
		return
	}
	if _, err := m.appendAgentTaskResultCard(task); err != nil {
		m.emitHubTaskAlert(task.ID, "task_result_card_failed", err.Error())
	}
	m.pushAgentTaskResult(ledger, task)
}

// notifyCanceledAgentTask 智能体任务**真的**转成 canceled（未开始/会话空闲兜底）时
// 通知派发者：结果卡 + 回推，一次。中断语义（running → interrupted，由 AgentEnd
// 结算）不走这里，避免同一次取消发出两条终态。
func (m *manager) notifyCanceledAgentTask(task *agenthub.Task) {
	if m == nil || task == nil || task.Target.Kind != agenthub.TargetAgent ||
		task.Status != agenthub.TaskCanceled {
		return
	}
	st, err := m.hubStore()
	if err != nil {
		m.emitHubTaskAlert(task.ID, "notify_failed", err.Error())
		return
	}
	m.notifyAgentTaskResult(agenthub.NewLedger(st), task)
}

// appendAgentTaskResultCard 写一张群 task_result 卡（幂等）。
//
// 幂等靠确定性 client_msg_id：重复 AgentEnd / 取消与迟到结算并发时，先扫
// FindGroupMessageByClientID，命中就不再追加。「查重 → 追加」必须在
// hubGroupSendMu 内完成，否则两个结算方会同时看不到对方的卡。
func (m *manager) appendAgentTaskResultCard(task *agenthub.Task) (*agenthub.Message, error) {
	if m == nil || task == nil || task.Target.Kind != agenthub.TargetAgent {
		return nil, nil
	}
	gid := task.Target.GroupID
	if gid == "" {
		gid = task.Origin.GroupID
	}
	if gid == "" {
		return nil, fmt.Errorf("%w: 智能体任务没有群作用域，无法写结果卡", agenthub.ErrInvalid)
	}
	st, err := m.hubStore()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	clientID := agentTaskResultClientID(task.ID)
	msg := &agenthub.Message{
		GroupID: gid, TaskID: task.ID, Kind: agenthub.KindTaskResult,
		Sender: agenthub.Sender{Kind: agenthub.SenderAgent, AgentID: task.Target.AgentID},
		Text:   truncateRunes(agentTaskCardText(task), 4000),
		// Mentions 恒为空：结果卡不参与 @ 路由，也不唤醒任何人。
		Visibility: agenthub.VisibilityGroup,
		Refs: map[string]any{
			"client_msg_id": clientID,
			"task_id":       string(task.ID),
			"status":        string(task.Status),
			"agent_id":      string(task.Target.AgentID),
			"title":         task.Title,
			"duration_ms":   taskResultDurationMS(task),
			"usage":         taskResultUsageSnapshot(task),
			"artifacts":     taskResultArtifacts(task),
			"error":         taskResultError(task),
		},
		CreatedAt: time.Now().UTC(),
	}
	m.hubGroupSendMu.Lock()
	existing, ferr := st.FindGroupMessageByClientID(ctx, gid, clientID)
	if ferr == nil && existing != nil {
		m.hubGroupSendMu.Unlock()
		return existing, nil
	}
	if ferr != nil {
		m.hubGroupSendMu.Unlock()
		return nil, ferr
	}
	// round=nil：卡片不走路由、不写回执（结果卡不是触发消息）。
	seq, _, aerr := agenthub.AppendGroupMessageRouted(ctx, st, msg, nil)
	m.hubGroupSendMu.Unlock()
	if aerr != nil {
		return nil, aerr
	}
	m.emitHubGroupMessage(gid, seq, msg)
	return msg, nil
}

// pushAgentTaskResult 只回推派发者（origin=group + agent_id），DM/人工来源不回推。
// 成功才 MarkNotified（completed/failed/interrupted/blocked_budget 允许；
// canceled/declined 没有 delivered 出边，靠接收端 HasTaskResult 保持恰好一次）。
func (m *manager) pushAgentTaskResult(ledger *agenthub.Ledger, task *agenthub.Task) {
	if m == nil || task == nil {
		return
	}
	if task.Origin.Kind != agenthub.OriginGroup || task.Origin.AgentID == "" {
		return // 人/工作区来源：结果卡已足够，不唤醒任何智能体（§6.5.7）
	}
	gid := task.Origin.GroupID
	if gid == "" {
		gid = task.Target.GroupID
	}
	if gid == "" {
		m.emitHubTaskAlert(task.ID, "push_failed", "群来源任务缺少 group_id，无法回推派发者")
		return
	}
	origin, err := m.hubGroupSession(string(task.Origin.AgentID), string(gid))
	if err != nil || origin == nil {
		m.emitHubTaskAlert(task.ID, "push_failed", fmt.Sprintf("无法打开发起会话: %v", err))
		return
	}
	// 接收端幂等：结果已在发起者上下文（上次 push 成功但 MarkNotified 失败/崩溃）
	// 就不再推第二份，只补标记与终态快照（前端「已回推」收敛）。
	if origin.s.HasTaskResult(string(task.ID)) {
		m.markAgentTaskNotified(ledger, task)
		if latest, lerr := ledger.Replay(context.Background(), task.ID); lerr == nil && latest != nil {
			m.emitHubTaskUpdate(latest)
		}
		return
	}
	summary, pushErr := agentTaskPushSummary(task)
	if err := origin.s.PushTaskResultUsage(string(task.ID), task.Title, summary,
		string(task.Status), coreUsageOfTaskResult(task), pushErr); err != nil {
		m.emitHubTaskAlert(task.ID, "push_failed", err.Error())
		return
	}
	m.markAgentTaskNotified(ledger, task)
	if latest, lerr := ledger.Replay(context.Background(), task.ID); lerr == nil && latest != nil {
		m.emitHubTaskUpdate(latest)
	}
}

// markAgentTaskNotified 记录「已回推」。状态机没有出边时（canceled/declined）跳过。
func (m *manager) markAgentTaskNotified(ledger *agenthub.Ledger, task *agenthub.Task) {
	if m == nil || ledger == nil || task == nil {
		return
	}
	runID := task.Dispatch.RunID
	switch task.Status {
	case agenthub.TaskCompleted, agenthub.TaskFailed, agenthub.TaskInterrupted,
		agenthub.TaskBlockedBudget, agenthub.TaskDelivered:
	default:
		return // canceled/declined/queued/... 没有 delivered 出边
	}
	if runID == "" {
		// 没有真实 run（例如恢复合成）：MarkNotified 的幂等键就是 run_id，
		// 空串不能写进 notified_run_ids（会把后续真实 run 全挡掉）。
		return
	}
	if _, err := ledger.MarkNotified(context.Background(), task.ID, runID); err != nil {
		m.emitHubTaskAlert(task.ID, "notify_failed", err.Error())
	}
}

// agentTaskCardText 结果卡正文 = 执行智能体的最终摘要；没有摘要的终态
// （取消/拒绝/预算阻断）用原因兜底，避免空卡在群里看不见。
func agentTaskCardText(task *agenthub.Task) string {
	if task == nil {
		return ""
	}
	text := ""
	if task.Result != nil {
		text = strings.TrimSpace(task.Result.Summary)
	}
	if text == "" {
		text = taskResultError(task)
	}
	if text == "" {
		text = fmt.Sprintf("任务 %s：%s", task.ID, task.Status)
	}
	return text
}

// agentTaskPushSummary 是回推正文：结果摘要 + 失败/阻断/取消/拒绝/人工介入说明。
// 口径与工作区任务（pushHubTaskResult）一致，只是卡片落在群里。
func agentTaskPushSummary(task *agenthub.Task) (string, error) {
	if task == nil {
		return "", nil
	}
	summary := ""
	if task.Result != nil {
		summary = strings.TrimSpace(task.Result.Summary)
	}
	reason := taskResultError(task)
	var pushErr error
	switch task.Status {
	case agenthub.TaskFailed, agenthub.TaskInterrupted:
		if reason != "" {
			pushErr = errors.New(reason)
		}
	case agenthub.TaskBlockedBudget:
		// 预算阻断不是失败：正文仍是产出，但必须把「为什么停」带给模型。
		if reason != "" {
			summary = appendTaskNote(summary, "已阻断："+reason)
		}
	case agenthub.TaskCanceled:
		// 取消/拒绝不塞 pushErr：core 的 task_result 文本在 err != nil 时只渲染
		// 错误正文，会把「状态 + 原因」一起吞掉。原因进摘要，状态走消息标签。
		note := "—— 任务已取消"
		if reason != "" {
			note += "：" + reason
		}
		summary = appendTaskNote(summary, note)
	case agenthub.TaskDeclined:
		note := "—— 任务已被执行者拒绝"
		if reason != "" {
			note += "：" + reason
		}
		summary = appendTaskNote(summary, note)
	}
	if summary == "" && pushErr != nil {
		summary = pushErr.Error()
	}
	// §6.7.5：人在任务运行中介入过，回推必须让模型先对齐人的最新意图。
	if task.HumanIntervened {
		summary = appendTaskNote(summary,
			"—— 人类在该任务运行中介入了。先确认用户的最新意图，不要自动继续原计划。")
	}
	return summary, pushErr
}

func appendTaskNote(summary, note string) string {
	if note == "" {
		return summary
	}
	if strings.TrimSpace(summary) == "" {
		return note
	}
	return summary + "\n\n" + note
}

func taskResultDurationMS(task *agenthub.Task) int64 {
	if task == nil || task.Result == nil {
		return 0
	}
	return task.Result.DurationMS
}

func taskResultUsageSnapshot(task *agenthub.Task) agenthub.Usage {
	if task == nil || task.Result == nil {
		return agenthub.Usage{}
	}
	return task.Result.Usage
}

func taskResultArtifacts(task *agenthub.Task) []agenthub.TaskArtifact {
	if task == nil || task.Result == nil {
		return nil
	}
	return task.Result.Artifacts
}

func taskResultError(task *agenthub.Task) string {
	if task == nil {
		return ""
	}
	if err := strings.TrimSpace(task.Error); err != "" {
		return err
	}
	if task.Result != nil {
		return strings.TrimSpace(task.Result.Error)
	}
	return ""
}

// coreUsageOfTaskResult 把账本用量还原成事件用量（任务卡 tokens/成本）。
// CostPriced=false 时不给金额：无价不等于免费。
func coreUsageOfTaskResult(task *agenthub.Task) core.Usage {
	u := taskResultUsageSnapshot(task)
	out := core.Usage{
		Input: u.Input, Output: u.Output,
		CacheRead: u.CacheRead, CacheWrite: u.CacheWrite, CacheWrite1h: u.CacheWrite1h,
		Reasoning: u.Reasoning,
	}
	if u.CostPriced {
		out.Cost = core.Cost{
			Total:  int64(math.Round(u.CostUSD * 1e6)),
			Priced: true,
			Source: core.CostSourceBuiltin,
		}
	}
	return out
}
