package main

// hub_taskboard.go：P2-06 任务板 + Needs you。
//
// 任务板直接读账本（agenthub.Ledger/FileStore），取消/重试走 Queue 与 Session.Interrupt；
// Needs you 把审批/提问事件落成 need（锚 = 调用 id / 批次 id），决策复用
// Session.Approve / Session.AnswerQuestions（§6.10.2），失败只标记已读。

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/events"
)

// —— 任务板 ——

func (m *manager) hubTaskList(c command) {
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	f := agenthub.TaskFilter{Status: agenthub.TaskStatus(strings.TrimSpace(str(c.Payload, "status")))}
	if v := strings.TrimSpace(str(c.Payload, "agent_id")); v != "" {
		f.AgentID = agenthub.AgentID(v)
	}
	if v := strings.TrimSpace(str(c.Payload, "group_id")); v != "" {
		f.GroupID = agenthub.GroupID(v)
	}
	tasks, err := st.ListTasks(context.Background(), f)
	if err != nil {
		m.resp(c, false, "读取任务失败: "+err.Error(), nil)
		return
	}
	limit := int(numOr(c.Payload, "limit", 100))
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	if len(tasks) > limit {
		tasks = tasks[:limit]
	}
	m.resp(c, true, "", map[string]any{"tasks": tasks, "count": len(tasks)})
}

func (m *manager) hubTaskGet(c command) {
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	id := agenthub.TaskID(strings.TrimSpace(str(c.Payload, "task_id")))
	if id == "" {
		m.resp(c, false, "task_id 不能为空", nil)
		return
	}
	t, err := st.LoadTask(context.Background(), id)
	if err != nil {
		m.resp(c, false, "读取任务失败: "+err.Error(), nil)
		return
	}
	evs, _ := st.ReadTaskEvents(context.Background(), id)
	m.resp(c, true, "", map[string]any{"task": t, "events": evs})
}

func (m *manager) hubTaskCancel(c command) {
	ctx := context.Background()
	id := agenthub.TaskID(strings.TrimSpace(str(c.Payload, "task_id")))
	if id == "" {
		m.resp(c, false, "task_id 不能为空", nil)
		return
	}
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	t, err := st.LoadTask(ctx, id)
	if err != nil {
		m.resp(c, false, "读取任务失败: "+err.Error(), nil)
		return
	}
	respTask := func(t *agenthub.Task, note string) {
		m.resp(c, true, "", map[string]any{"task": t, "note": note})
	}
	if t.Status.Terminal() {
		respTask(t, "任务已结束，无需取消")
		return
	}
	if t.Status == agenthub.TaskQueued {
		q, err := m.hubQueueFor()
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		canceled, err := q.CancelQueued(ctx, id)
		if err != nil {
			m.resp(c, false, "取消失败: "+err.Error(), nil)
			return
		}
		m.emitHubTaskUpdate(canceled)
		respTask(canceled, "已取消（未开始）")
		return
	}
	// dispatched/running：先中断会话；空闲兜底与 Pump 的派发窗口在 q.mu 内
	// 串行判定，避免「已回已取消、Dispatch 仍把指令 Ask 进会话」（P2-06 复审 B-2）。
	q, err := m.hubQueueFor()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	bs := m.sessionByID(t.Target.SessionID)
	if bs == nil {
		canceled, did, cerr := q.CancelDispatchedIfIdle(ctx, id, "目标会话不存在，取消任务", nil)
		if cerr != nil {
			m.resp(c, false, "取消失败: "+cerr.Error(), nil)
			return
		}
		if did {
			m.emitHubTaskUpdate(canceled)
			respTask(canceled, "目标会话不存在，已取消")
			return
		}
		respTask(canceled, "任务已结束，无需取消")
		return
	}
	bs.s.Interrupt() // 幂等：空闲会话无操作；终态由 AgentEnd 结算
	canceled, did, cerr := q.CancelDispatchedIfIdle(ctx, id, "会话空闲，取消未结束的任务", func() bool {
		return bs.running.Load() || bs.hubDispatchPending.Load()
	})
	if cerr != nil {
		m.resp(c, false, "取消失败: "+cerr.Error(), nil)
		return
	}
	if did {
		m.emitHubTaskUpdate(canceled)
		respTask(canceled, "会话空闲，已取消")
		return
	}
	if canceled.Status.Terminal() {
		respTask(canceled, "任务已结束，无需取消")
		return
	}
	respTask(canceled, "已请求中断，等待运行结束")
}

func (m *manager) hubTaskRetry(c command) {
	ctx := context.Background()
	id := agenthub.TaskID(strings.TrimSpace(str(c.Payload, "task_id")))
	if id == "" {
		m.resp(c, false, "task_id 不能为空", nil)
		return
	}
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	old, err := st.LoadTask(ctx, id)
	if err != nil {
		m.resp(c, false, "读取任务失败: "+err.Error(), nil)
		return
	}
	if !old.Status.Terminal() {
		m.resp(c, false, "任务还在进行中，先取消再重试", nil)
		return
	}
	instruction := strings.TrimSpace(str(c.Payload, "instruction"))
	if instruction == "" {
		instruction = old.Instruction
	}
	newID, err := st.AllocateID(ctx, agenthub.PrefixTask)
	if err != nil {
		m.resp(c, false, "分配任务 ID 失败: "+err.Error(), nil)
		return
	}
	chain := append(append([]agenthub.TaskID(nil), old.Chain...), agenthub.TaskID(newID))
	next := &agenthub.Task{
		ID:           agenthub.TaskID(newID),
		RootTaskID:   old.RootTaskID,
		ParentTaskID: old.ID,
		Chain:        chain,
		Origin:       old.Origin,
		Target:       old.Target,
		Title:        firstLine(instruction),
		Instruction:  instruction,
		Callback:     old.Callback,
		Attempt:      old.Attempt + 1,
	}
	q, err := m.hubQueueFor()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	created, err := q.Submit(ctx, next)
	if err != nil {
		m.resp(c, false, "重试失败: "+err.Error(), nil)
		return
	}
	m.emitHubTaskUpdate(created)
	m.resp(c, true, "", map[string]any{"task": created})
}

// —— Needs you ——

func (m *manager) hubNeedList(c command) {
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	status := strings.TrimSpace(str(c.Payload, "status"))
	if status == "" {
		status = "pending"
	}
	needs, err := st.ListNeeds(context.Background(), agenthub.NeedFilter{Status: status})
	if err != nil {
		m.resp(c, false, "读取 Needs you 失败: "+err.Error(), nil)
		return
	}
	sort.Slice(needs, func(i, j int) bool { return needs[i].CreatedAt.After(needs[j].CreatedAt) })
	m.resp(c, true, "", map[string]any{"needs": needs, "count": len(needs)})
}

func (m *manager) hubNeedResolve(c command) {
	ctx := context.Background()
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	id := agenthub.NeedID(strings.TrimSpace(str(c.Payload, "need_id")))
	if id == "" {
		m.resp(c, false, "need_id 不能为空", nil)
		return
	}
	m.hubNeedMu.Lock()
	defer m.hubNeedMu.Unlock()
	n, err := st.LoadNeed(ctx, id)
	if err != nil {
		m.resp(c, false, "读取 Need 失败: "+err.Error(), nil)
		return
	}
	if n.Status != "pending" {
		m.resp(c, true, "", map[string]any{"need": n, "already": true})
		return
	}
	resolution := strings.TrimSpace(str(c.Payload, "resolution"))
	if resolution == "" {
		resolution = "acknowledged"
	}
	bs := m.sessionByID(n.SessionID)
	switch n.Kind {
	case "approval":
		if bs == nil {
			m.resp(c, false, "会话已关闭，无法处理审批", nil)
			return
		}
		approve := resolution == "approve" || resolution == "approved" || resolution == "true"
		if err := bs.s.Approve(n.Anchor, approve); err != nil {
			m.resp(c, false, "审批失败: "+err.Error(), nil)
			return
		}
	case "question":
		if bs == nil {
			m.resp(c, false, "会话已关闭，无法回答问题", nil)
			return
		}
		answers, err := hubNeedAnswers(c.Payload["answers"])
		if err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
		if err := bs.s.AnswerQuestions(n.Anchor, answers); err != nil {
			m.resp(c, false, "回答失败: "+err.Error(), nil)
			return
		}
	case "failure", "budget", "handoff":
		// failure/budget 只标记已读；handoff 的接受/拒绝归 P3 群组。
	default:
		m.resp(c, false, "未知的 Need 类型: "+n.Kind, nil)
		return
	}
	now := time.Now().UTC()
	n.Status = "resolved"
	n.ResolvedAt = &now
	if err := st.SaveNeed(ctx, n); err != nil {
		m.resp(c, false, "保存 Need 失败: "+err.Error(), nil)
		return
	}
	m.emitHubNeed(n)
	m.resp(c, true, "", map[string]any{"need": n, "resolution": resolution})
}

func hubNeedAnswers(v any) ([]events.QuestionAnswer, error) {
	if v == nil {
		return nil, fmt.Errorf("缺少 answers")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("answers 不可序列化: %w", err)
	}
	var answers []events.QuestionAnswer
	if err := json.Unmarshal(raw, &answers); err == nil && len(answers) > 0 {
		return answers, nil
	}
	var byID map[string]string
	if err := json.Unmarshal(raw, &byID); err != nil {
		return nil, fmt.Errorf("answers 格式错误: 需要 [{question_id,answer}] 或 {question_id: answer}")
	}
	keys := make([]string, 0, len(byID))
	for k := range byID {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		answers = append(answers, events.QuestionAnswer{QuestionId: k, Answer: byID[k]})
	}
	if len(answers) == 0 {
		return nil, fmt.Errorf("answers 不能为空")
	}
	return answers, nil
}

// recordHubNeed 在审批/提问事件到达时落一条 need（幂等：同一 anchor 只建一次）。
// 只对 Hub 智能体或正在执行 hub_task 的会话生效，不改普通工作区会话的行为。
func (m *manager) recordHubNeed(bs *bridgeSession, e events.Event) {
	if m == nil || bs == nil || bs.runtime == nil {
		return
	}
	isHub := bs.runtime.kind == runtimeHubAgent
	isTask := len(bs.hubTaskIDs) > 0
	if !isHub && !isTask {
		return
	}
	var kind, anchor, title, body string
	switch ev := e.(type) {
	case *events.ToolApprovalRequested:
		kind, anchor = "approval", ev.Id
		title = "工具审批：" + ev.Name
		body = truncateRunes(ev.Arguments, 2000)
	case *events.AskUserQuestion:
		kind, anchor = "question", ev.Id
		title = "需要你回答问题"
		if raw, err := json.Marshal(ev.Questions); err == nil {
			body = truncateRunes(string(raw), 4000)
		}
	default:
		return
	}
	if anchor == "" {
		return
	}
	st, err := m.hubStore()
	if err != nil {
		return
	}
	sum := sha1.Sum([]byte(kind + "\x00" + bs.id + "\x00" + anchor))
	id := agenthub.NeedID("need_" + hex.EncodeToString(sum[:])[:16])
	ctx := context.Background()
	if existing, err := st.LoadNeed(ctx, id); err == nil && existing != nil {
		return // 事件重放/重复渲染：已经在列表里（含已处理）
	}
	n := &agenthub.Need{
		ID: id, Kind: kind, Status: "pending",
		AgentID:   agenthub.AgentID(bs.runtime.agentID),
		TaskID:    firstHubTaskID(bs),
		SessionID: bs.id, Workspace: bs.workspace, Anchor: anchor,
		Title: title, Body: body, CreatedAt: time.Now().UTC(),
	}
	if err := st.SaveNeed(ctx, n); err != nil {
		return
	}
	m.emitHubNeed(n)
}

func firstHubTaskID(bs *bridgeSession) agenthub.TaskID {
	if bs == nil || len(bs.hubTaskIDs) == 0 {
		return ""
	}
	return bs.hubTaskIDs[0]
}

func (m *manager) emitHubNeed(n *agenthub.Need) {
	if m == nil || m.out == nil || n == nil {
		return
	}
	m.out.writeLine(map[string]any{"event_type": "hub_need", "need": n})
}

// expireSessionNeeds 根 run 结束时把该会话仍挂起的审批/提问标 expired。
func (m *manager) expireSessionNeeds(bs *bridgeSession) {
	if m == nil || bs == nil {
		return
	}
	st, err := m.hubStore()
	if err != nil {
		return
	}
	ctx := context.Background()
	needs, err := st.ListNeeds(ctx, agenthub.NeedFilter{Status: "pending"})
	if err != nil {
		return
	}
	now := time.Now().UTC()
	for _, candidate := range needs {
		if candidate.SessionID != bs.id || (candidate.Kind != "approval" && candidate.Kind != "question") {
			continue
		}
		m.hubNeedMu.Lock()
		// 锁内重读：resolve 可能刚刚把它改成 resolved；不能覆盖用户决策。
		n, err := st.LoadNeed(ctx, candidate.ID)
		if err != nil || n == nil || n.Status != "pending" || n.SessionID != bs.id {
			m.hubNeedMu.Unlock()
			continue
		}
		n.Status = "expired"
		n.ResolvedAt = &now
		if err := st.SaveNeed(ctx, n); err == nil {
			m.hubNeedMu.Unlock()
			m.emitHubNeed(n)
		} else {
			m.hubNeedMu.Unlock()
		}
	}
}

// hubClear 清空 Hub DM 的上下文（保留 system；会话内 /clear 同一实现）。
func (m *manager) hubClear(c command) {
	_, agentID, err := hubTarget(c.Payload)
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	bs, err := m.hubDMSession(agentID)
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	if err := bs.s.Command(context.Background(), "clear", ""); err != nil {
		m.resp(c, false, "清空上下文失败: "+err.Error(), nil)
		return
	}
	m.resp(c, true, "", map[string]any{"session_id": bs.id})
}
