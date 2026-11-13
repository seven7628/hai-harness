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
	"errors"
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
	t, note, err := m.cancelHubTask(ctx, id)
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	m.resp(c, true, "", map[string]any{"task": t, "note": note})
}

// cancelHubTask 是任务板命令与 task_cancel 模型工具的共同取消入口。
// 返回最新快照与给用户的说明；取消成功时已发 hub_task_update。
func (m *manager) cancelHubTask(ctx context.Context, id agenthub.TaskID) (*agenthub.Task, string, error) {
	st, err := m.hubStore()
	if err != nil {
		return nil, "", err
	}
	t, err := st.LoadTask(ctx, id)
	if err != nil {
		return nil, "", fmt.Errorf("读取任务失败: %w", err)
	}
	if t.Status.Terminal() {
		return t, "任务已结束，无需取消", nil
	}
	if t.Status == agenthub.TaskQueued {
		q, err := m.hubQueueFor()
		if err != nil {
			return nil, "", err
		}
		canceled, err := q.CancelQueued(ctx, id)
		if err == nil {
			m.emitHubTaskUpdate(canceled)
			m.notifyCanceledAgentTask(canceled)
			return canceled, "已取消（未开始）", nil
		}
		// 读到 queued 之后 Pump 可能已在 q.mu 内把它派走（Retry/并发取消也会改状态）。
		// 这不是失败：重读后落回下面的 dispatched/running 分支，走中断而不是报错。
		if !errors.Is(err, agenthub.ErrInvalid) {
			return nil, "", fmt.Errorf("取消失败: %w", err)
		}
		t, err = st.LoadTask(ctx, id)
		if err != nil {
			return nil, "", fmt.Errorf("读取任务失败: %w", err)
		}
		if t.Status.Terminal() {
			return t, "任务已结束，无需取消", nil
		}
	}
	// dispatched/running：先做空闲兜底判定（与 Pump 的派发窗口在 q.mu 内串行），
	// 没抢到空闲再中断会话 —— Interrupt 放在判定之后，避免在「Ask 未发生」的窗口
	// 空转后仍回「已请求中断」却让 run 跑到完成（P2-06 复审 N-1）。
	q, err := m.hubQueueFor()
	if err != nil {
		return nil, "", err
	}
	bs := m.sessionByID(t.Target.SessionID)
	if bs == nil {
		canceled, did, cerr := q.CancelDispatchedIfIdle(ctx, id, "目标会话不存在，取消任务", nil)
		if cerr != nil {
			return nil, "", fmt.Errorf("取消失败: %w", cerr)
		}
		if did {
			m.emitHubTaskUpdate(canceled)
			m.notifyCanceledAgentTask(canceled)
			return canceled, "目标会话不存在，已取消", nil
		}
		return canceled, "任务已结束，无需取消", nil
	}
	canceled, did, cerr := q.CancelDispatchedIfIdle(ctx, id, "会话空闲，取消未结束的任务", func() bool {
		// 统一忙判据（P3b-03 N6）：只认 running/dispatchPending/settling 会把
		// 「群任务已投、AgentEnd 未到」（hubGroupBusy）误判空闲并直接取消。
		return hubSessionBusy(bs)
	})
	if cerr != nil {
		return nil, "", fmt.Errorf("取消失败: %w", cerr)
	}
	if did {
		m.emitHubTaskUpdate(canceled)
		m.notifyCanceledAgentTask(canceled)
		return canceled, "会话空闲，已取消", nil
	}
	if canceled.Status.Terminal() {
		return canceled, "任务已结束，无需取消", nil
	}
	if bs.hubSettling.Load() {
		// 根 AgentEnd 已到、finishHubTask 未完成：Interrupt 对已结束的 run 是 no-op，
		// 回「已请求中断」会让人以为取消生效，实际任务会按 completed 结算。
		return canceled, "任务正在结算，结果保留", nil
	}
	bs.s.Interrupt() // 已 Ask 进会话（running）：中断幂等，终态由 AgentEnd 结算
	return canceled, "已请求中断，等待运行结束", nil
}

// hubGuardForTask 用任务发起者的 Profile 与群策略构造重试护栏。
func (m *manager) hubGuardForTask(old *agenthub.Task) *agenthub.Guard {
	limits := agenthub.GuardLimits{GlobalDailyBudgetUSD: agenthub.DefaultGlobalDailyBudgetUSD}
	if old != nil && old.Origin.AgentID != "" {
		if p, _, err := m.hubLoadAgent(string(old.Origin.AgentID)); err == nil && p != nil {
			limits.MaxTaskCostUSD = p.Policy.MaxTaskCostUSD
			limits.DailyBudgetUSD = p.Policy.DailyBudgetUSD
		}
	}
	if old != nil && old.Origin.GroupID != "" {
		if st, err := m.hubStore(); err == nil {
			if g, err := st.LoadGroup(context.Background(), old.Origin.GroupID); err == nil && g != nil {
				limits.GroupDailyBudgetUSD = g.Policy.DailyBudgetUSD
			}
		}
	}
	return &agenthub.Guard{Limits: limits}
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
	if old.Target.Kind == agenthub.TargetAgent {
		// agent 目标的重试要走 assign 路径（群消息 + 目标校验 + 同对上限 + 派发），
		// 这里直接 SubmitGuarded 会让任务静默排队到永远（没有对话控制器泵它）。
		// 接线归 P3b-04/05：在那之前显式拒绝，而不是假装成功。
		m.resp(c, false, "智能体目标任务暂不支持重试（重试接线在 P3b-04/05）：请用 assign_task 重新指派，或取消后重派", nil)
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
	// 重试也是派发：必须走同一套护栏（预算/链长/并发），不能开旁路。
	created, err := q.SubmitGuarded(ctx, next, m.hubGuardForTask(old))
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
	// 惰性过期：审批/提问超过 30 分钟未处理就从 pending 收掉（P3a-07 B4）。
	m.expireStaleHubNeeds(context.Background(), st)
	needs, err := st.ListNeeds(context.Background(), agenthub.NeedFilter{
		Status:  status,
		GroupID: agenthub.GroupID(strings.TrimSpace(str(c.Payload, "group_id"))),
		AgentID: agenthub.AgentID(strings.TrimSpace(str(c.Payload, "agent_id"))),
	})
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
	// 决策前也扫一次 TTL：列表没打开时也不能批准已过期的审批。
	m.expireStaleHubNeeds(ctx, st)
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
	case "handoff":
		// 只有明确的接受/拒绝才改交接状态；「标记已读」不能把 pending_ack
		// 不可逆地判成 declined（P3a-06 复审 B-01）。未知 resolution fail-closed，
		// 避免拼写错误既不改状态又把 need 静默消费掉。
		accept := resolution == "accept" || resolution == "accepted" ||
			resolution == "approve" || resolution == "approved" || resolution == "true"
		decline := resolution == "decline" || resolution == "declined" || resolution == "deny" ||
			resolution == "denied" || resolution == "reject" || resolution == "rejected" || resolution == "false"
		read := resolution == "acknowledged" || resolution == "ack" || resolution == "read"
		switch {
		case accept || decline:
			if _, err := m.ackHubHandoff(ctx, n.Anchor, accept, str(c.Payload, "reason"), "", true); err != nil {
				m.resp(c, false, "交接处理失败: "+err.Error(), nil)
				return
			}
		case read:
			// 只读：handoff 保持 pending_ack，接收方仍可决策。
		default:
			m.resp(c, false, "未知的交接处理方式: "+resolution, nil)
			return
		}
	case "failure", "budget":
		// failure/budget 只标记已读。
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
// 归属（场景2）：Hub 会话记会话自身；Project 会话从绑定的 hub_task 反推 origin
// （哪个 Hub 智能体/群派来的）与 target（本 Project workspace/session），
// 供 Hub 单聊/群内联展示；RunID/Depth 取事件自带值。
func (m *manager) recordHubNeed(bs *bridgeSession, e events.Event) {
	if m == nil || bs == nil || bs.runtime == nil {
		return
	}
	isHub := bs.runtime.kind == runtimeHubAgent
	isTask := bs.hubTaskCount() > 0
	if !isHub && !isTask {
		return
	}
	var kind, anchor, title, body string
	runID, depth := "", 0
	switch ev := e.(type) {
	case *events.ToolApprovalRequested:
		kind, anchor = "approval", ev.Id
		runID, depth = ev.RunId, ev.Depth
		title = "工具审批：" + ev.Name
		body = truncateRunes(ev.Arguments, 2000)
	case *events.AskUserQuestion:
		kind, anchor = "question", ev.Id
		runID = ev.RunId // 提问事件没有 Depth（不是嵌套调用）
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
	taskID := firstHubTaskID(bs)
	// 任务快照缺失（被清理/脏绑定）只让归属留空：need 仍要落库、仍要能 resolve。
	var task *agenthub.Task
	if taskID != "" {
		task, _ = st.LoadTask(ctx, taskID)
	}
	n := &agenthub.Need{
		ID: id, Kind: kind, Status: "pending",
		TaskID:    taskID,
		SessionID: bs.id, Workspace: bs.workspace, Anchor: anchor,
		RunID: runID, RunName: bs.hubRunName(runID), Depth: depth,
		Title: title, Body: body, CreatedAt: time.Now().UTC(),
	}
	if isHub {
		// Hub 会话保持原语义：等的人就是会话自己的智能体；群从 sid 推出（DM 为空）。
		n.AgentID = agenthub.AgentID(bs.runtime.agentID)
		n.GroupID = agenthub.GroupID(hubGroupIDFromSID(bs.id))
	} else if task != nil {
		// Project 会话：AgentID/GroupID 记 origin（hub_need_list 按它们能查到），
		// SessionID/Workspace 仍是**被阻塞的** Project 会话（resolve 按它找会话）。
		n.AgentID = task.Origin.AgentID
		n.GroupID = task.Origin.GroupID
		n.OriginAgentID = task.Origin.AgentID
		// session_send 建账只带 agent_id，不带名字（N2）：直接抄账本就是空串，
		// 前端只能回落到 ag_xxx。按 profile 补名字，与结果卡 refs 同一回落。
		n.OriginAgentName = m.hubOriginAgentName(task)
		n.OriginKind = task.Origin.Kind
	}
	if hubNeedTaskOwnsSession(task, bs) {
		n.TargetKind = task.Target.Kind
		n.TargetWorkspace = task.Target.Workspace
		n.TargetSessionID = task.Target.SessionID
	}
	if err := st.SaveNeed(ctx, n); err != nil {
		return
	}
	m.emitHubNeed(n)
}

// hubNeedTaskOwnsSession 判断 task 的目标是不是本会话：钉了 session_id 就比 id
// （Hub 智能体可能拿到绑在不同会话上的任务）；只钉了 workspace（还没建会话就
// 派发）时按 workspace 相同算本会话。
func hubNeedTaskOwnsSession(task *agenthub.Task, bs *bridgeSession) bool {
	if task == nil || bs == nil {
		return false
	}
	if task.Target.SessionID != "" {
		return task.Target.SessionID == bs.id
	}
	return task.Target.Workspace != "" && task.Target.Workspace == bs.workspace
}

func firstHubTaskID(bs *bridgeSession) agenthub.TaskID {
	if bs == nil {
		return ""
	}
	return bs.currentHubTaskID()
}

// expireStaleHubNeeds 把 30 分钟未处理的审批/提问置 expired。
// 锁内重读，避免覆盖刚刚 resolve 的决策；只处理 approval/question，交接/失败不自动过期。
func (m *manager) expireStaleHubNeeds(ctx context.Context, st agenthub.Store) {
	if st == nil {
		return
	}
	needs, err := st.ListNeeds(ctx, agenthub.NeedFilter{Status: "pending"})
	if err != nil {
		return
	}
	now := time.Now().UTC()
	for _, cand := range needs {
		if cand == nil || (cand.Kind != "approval" && cand.Kind != "question") {
			continue
		}
		if now.Sub(cand.CreatedAt) < agenthub.DefaultNeedTTL {
			continue
		}
		m.hubNeedMu.Lock()
		n, err := st.LoadNeed(ctx, cand.ID)
		if err != nil || n == nil || n.Status != "pending" ||
			(n.Kind != "approval" && n.Kind != "question") ||
			now.Sub(n.CreatedAt) < agenthub.DefaultNeedTTL {
			m.hubNeedMu.Unlock()
			continue
		}
		n.Status = "expired"
		expiredAt := now
		n.ResolvedAt = &expiredAt
		saveErr := st.SaveNeed(ctx, n)
		m.hubNeedMu.Unlock()
		if saveErr == nil {
			m.emitHubNeed(n)
		}
	}
}

func (m *manager) emitHubNeed(n *agenthub.Need) {
	if m == nil || m.out == nil || n == nil {
		return
	}
	m.out.writeLine(map[string]any{"event_type": "hub_need", "need": n})
}

// expireSessionNeeds 根 run 结束时把该会话仍挂起的审批/提问标 expired。
// 同步路径保留给测试/诊断；生产走 scheduleHubExpire（可取消、可等待）。
func (m *manager) expireSessionNeeds(bs *bridgeSession) {
	if m == nil || bs == nil {
		return
	}
	ctx, ok := m.hubBackgroundContext()
	if !ok {
		return
	}
	m.expireSessionNeedsCtx(ctx, bs)
}

func (m *manager) expireSessionNeedsCtx(ctx context.Context, bs *bridgeSession) {
	if m == nil || bs == nil || ctx == nil || ctx.Err() != nil {
		return
	}
	st, err := m.hubStore()
	if err != nil {
		return
	}
	needs, err := st.ListNeeds(ctx, agenthub.NeedFilter{Status: "pending"})
	if err != nil {
		return
	}
	now := time.Now().UTC()
	for _, candidate := range needs {
		if ctx.Err() != nil {
			return
		}
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

// scheduleHubExpire 是生产入口：登记到 Hub 后台 WaitGroup 后再过期。
func (m *manager) scheduleHubExpire(bs *bridgeSession) {
	m.spawnHubBackground(func(ctx context.Context) {
		m.expireSessionNeedsCtx(ctx, bs)
	})
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
