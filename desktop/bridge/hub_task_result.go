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

// notifyAgentTaskResult = 持久层（群内结果卡，同步、store-only）+ 唤醒层（异步）。
//
// 卡写失败不阻断回推：结论在账本里，派发者必须知道结果（失败只留一条告警）。
// 唤醒**不能**在这里同步做：调用方可能处在 emit（持 dispatchMu）/hubFinishMu 里
// （结算、取消、ack 都在这些窗口），同步 PushTaskResultUsage 会跨会话拿对方的
// dispatchMu，A↔B 互派即死锁（P3b-08 B1，见 hub_notify.go）。
func (m *manager) notifyAgentTaskResult(task *agenthub.Task) {
	if m == nil || task == nil || task.Target.Kind != agenthub.TargetAgent {
		return
	}
	m.ensureTaskResultCard(task)
	m.scheduleTaskResultNotification(task.ID)
}

// notifyCanceledAgentTask 智能体任务**真的**转成 canceled（未开始/会话空闲兜底）时
// 通知派发者：结果卡 + 回推，一次。中断语义（running → interrupted，由 AgentEnd
// 结算）不走这里，避免同一次取消发出两条终态。
func (m *manager) notifyCanceledAgentTask(task *agenthub.Task) {
	if m == nil || task == nil || task.Target.Kind != agenthub.TargetAgent ||
		task.Status != agenthub.TaskCanceled {
		return
	}
	m.notifyAgentTaskResult(task)
}

// appendAgentTaskResultCard 写一张群 task_result 卡（target=agent，幂等）。
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
	return m.appendTaskResultCard(task, gid,
		agenthub.Sender{Kind: agenthub.SenderAgent, AgentID: task.Target.AgentID},
		agentTaskCardText(task))
}

// appendWorkspaceTaskResultCard 写一张群 task_result 卡（group 来源的 Project 任务）。
//
// 署名用**发起**智能体而不是「执行者」（那里没有智能体，只有一个工作区会话）：
// 它在群里，UI 能画头像，读者也知道该找谁追这一条。归属（哪个 workspace / 哪个
// session）写在 refs 与正文抬头里 —— 群历史里没有别的地方能看到它。
func (m *manager) appendWorkspaceTaskResultCard(task *agenthub.Task) (*agenthub.Message, error) {
	if m == nil || task == nil || task.Target.Kind != agenthub.TargetWorkspace ||
		task.Origin.Kind != agenthub.OriginGroup {
		return nil, nil
	}
	if task.Origin.GroupID == "" {
		return nil, fmt.Errorf("%w: 群来源的 Project 任务没有 group_id，无法写结果卡", agenthub.ErrInvalid)
	}
	return m.appendTaskResultCard(task, task.Origin.GroupID,
		agenthub.Sender{Kind: agenthub.SenderAgent, AgentID: task.Origin.AgentID},
		workspaceTaskCardText(task))
}

// appendTaskResultCard 是两种结果卡唯一的落盘路径：幂等查重 + 追加 + 事件投影。
// sender 与正文由调用方决定（agent 任务署名执行者，Project 任务署名发起者）。
func (m *manager) appendTaskResultCard(task *agenthub.Task, gid agenthub.GroupID,
	sender agenthub.Sender, text string) (*agenthub.Message, error) {
	st, err := m.hubStore()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	clientID := agentTaskResultClientID(task.ID)
	msg := &agenthub.Message{
		GroupID: gid, TaskID: task.ID, Kind: agenthub.KindTaskResult,
		Sender: sender,
		Text:   truncateRunes(text, 4000),
		// Mentions 恒为空：结果卡不参与 @ 路由，也不唤醒任何人。
		Visibility: agenthub.VisibilityGroup,
		Refs:       m.taskResultCardRefs(task, clientID),
		CreatedAt:  time.Now().UTC(),
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

// taskResultCardRefs 是结果卡的归属快照。sender 只说明「谁署名」，归属一律看 refs：
//   - agent 任务：sender = 执行者，origin_* = 派发者（B5）；
//   - group 来源的 Project 任务：sender = 发起者，target_* = 项目工作区/会话。
//
// 账本还没加载时（重启/历史）前端只能靠这些字段画「谁 → 哪里 → 结果」。
func (m *manager) taskResultCardRefs(task *agenthub.Task, clientID string) map[string]any {
	refs := map[string]any{
		"client_msg_id":     clientID,
		"task_id":           string(task.ID),
		"status":            string(task.Status),
		"title":             task.Title,
		"target_kind":       task.Target.Kind,
		"duration_ms":       taskResultDurationMS(task),
		"usage":             taskResultUsageSnapshot(task),
		"artifacts":         taskResultArtifacts(task),
		"error":             taskResultError(task),
		"origin_kind":       task.Origin.Kind,
		"origin_agent_id":   string(task.Origin.AgentID),
		"origin_agent_name": m.hubOriginAgentName(task),
	}
	switch task.Target.Kind {
	case agenthub.TargetAgent:
		refs["agent_id"] = string(task.Target.AgentID)
	case agenthub.TargetWorkspace:
		refs["target_workspace"] = task.Target.Workspace
		refs["target_session_id"] = task.Target.SessionID
	}
	return refs
}

// hubOriginAgentName 账本里的发起者名字可能是空的（session_send 建账只带 agent_id），
// 回落到 profile.Name；读不到就留空 —— 查名字失败不能让卡片写不出来。
func (m *manager) hubOriginAgentName(task *agenthub.Task) string {
	if name := strings.TrimSpace(task.Origin.AgentName); name != "" {
		return name
	}
	if task.Origin.AgentID == "" {
		return ""
	}
	if p, _, err := m.hubLoadAgent(string(task.Origin.AgentID)); err == nil && p != nil {
		return p.Name
	}
	return ""
}

// workspaceTaskCardText 是 Project 任务卡正文：先标明是哪个会话跑的，再是摘要。
// 口径与 agentTaskCardText 一致（终态兜底同源），只多一行归属 —— 群里没有任务详情
// 面板，读者必须在气泡里就能看出这段摘要来自哪个 workspace/session。
func workspaceTaskCardText(task *agenthub.Task) string {
	text := agentTaskCardText(task)
	where := make([]string, 0, 2)
	if ws := strings.TrimSpace(task.Target.Workspace); ws != "" {
		where = append(where, ws)
	}
	if sid := strings.TrimSpace(task.Target.SessionID); sid != "" {
		where = append(where, sid)
	}
	if len(where) == 0 {
		return text
	}
	return "〔Project " + strings.Join(where, " / ") + "〕\n" + text
}

// pushAgentTaskResult 只回推派发者（origin=group + agent_id），DM/人工来源不回推。
// 成功才 MarkNotified（completed/failed/interrupted/blocked_budget 允许；
// canceled/declined 没有 delivered 出边，靠接收端 HasTaskResult 保持恰好一次）。
func (m *manager) pushAgentTaskResult(ledger *agenthub.Ledger, task *agenthub.Task, runID string) {
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
		m.markAgentTaskNotified(ledger, task, runID)
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
	m.markAgentTaskNotified(ledger, task, runID)
	if latest, lerr := ledger.Replay(context.Background(), task.ID); lerr == nil && latest != nil {
		m.emitHubTaskUpdate(latest)
	}
}

// markAgentTaskNotified 记录「已回推」。状态机没有出边时（canceled/declined）跳过。
// runID 由调用方给（真实 run 或 recovery:<task_id> 占位）。
func (m *manager) markAgentTaskNotified(ledger *agenthub.Ledger, task *agenthub.Task, runID string) {
	if m == nil || ledger == nil || task == nil {
		return
	}
	if runID == "" {
		runID = task.Dispatch.RunID
	}
	switch task.Status {
	case agenthub.TaskCompleted, agenthub.TaskFailed, agenthub.TaskInterrupted,
		agenthub.TaskBlockedBudget, agenthub.TaskDelivered:
	default:
		return // canceled/declined/queued/... 没有 delivered 出边
	}
	if runID == "" {
		// 空 run 不能写进 notified_run_ids（会把后续真实 run 全挡掉）。
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
