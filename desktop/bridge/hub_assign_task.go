package main

// hub_assign_task.go：assign_task —— 群内智能体互相派**可追踪**任务（P3b-03，§6.7.9）。
//
// 与 @ 的区别：@ 只是对话（没有 task_id、没有完成回调、不进任务板）；assign_task
// 在**同一本任务账本**里建一条 target.kind=agent 的任务，写一条 intent=assign 的群
// 消息（mentions=[B]），并把目标群会话以 hub_task header + instruction 唤醒。
//
// 边界（刻意留给后续任务）：
//   - 目标忙 → 任务留队，不投第二份快照；P3b-04 的对话控制器在目标回合结束后泵；
//   - AgentEnd 结算 / 群内结果卡 / 回推发起者 → P3b-05（finishAgentTask）。
//
// 群作用域：group_id 来自**当前会话**（工具注册时注入），不是模型参数 —— 模型
// 无法用 assign_task 往别的群派任务。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/tools"
)

// hubAssignMessageHook 仅测试用：写指派消息之前的确定性屏障（生产恒为 nil）。
// 返回错误时 assign 走「消息没写成 → 取消已建任务」的回滚路径（P3b-03 R2）；
// 钩子拿到 task id 也能模拟「写消息窗口里任务被别的路径结算」。
var hubAssignMessageHook func(taskID agenthub.TaskID) error

// hubAssignMessageBarrier 读钩子时取一次快照（同 hubGroupAssembleBarrier 惯例）。
func hubAssignMessageBarrier(taskID agenthub.TaskID) error {
	if hook := hubAssignMessageHook; hook != nil {
		return hook(taskID)
	}
	return nil
}

func registerHubAssignTaskTool(eng *tools.Engine, m *manager, agentID agenthub.AgentID, groupID agenthub.GroupID) {
	if eng == nil || m == nil || agentID == "" || groupID == "" {
		return
	}
	eng.RegisterTool(context.Background(), newHubAssignTaskTool(m, agentID, groupID))
}

type hubAssignTaskTool struct {
	tools.BaseTool
	m       *manager
	agentID agenthub.AgentID
	groupID agenthub.GroupID
}

func newHubAssignTaskTool(m *manager, agentID agenthub.AgentID, groupID agenthub.GroupID) *hubAssignTaskTool {
	return &hubAssignTaskTool{BaseTool: tools.BaseTool{
		Name_: "assign_task",
		Description_: "Assign a tracked task to another member of THIS group: it creates a HubTask " +
			"(target=agent) in the shared ledger, posts an intent=assign group message mentioning the " +
			"target, and wakes their group session with a hub_task header. Use it when you need a " +
			"deliverable and a completion callback; use @Name (group_say) for discussion. " +
			"The group is fixed to the current session; to_agent accepts an agent id or a unique member name. " +
			"You cannot assign to yourself, and at most 2 tasks per pair may be in flight.",
		Params_: tools.Obj(map[string]any{
			"to_agent":      tools.Str("Target member: agent id, or a unique member name"),
			"instruction":   tools.Str("What the target must do; passed to them as the task body"),
			"title":         tools.Str("Optional short title for the task card (defaults to the first line)"),
			"artifact_ids":  tools.ArrayStr("Optional artifact ids from this group to attach"),
			"client_msg_id": tools.Str("Optional idempotency key: the same value never creates a second task"),
		}, "to_agent", "instruction"),
		CanParallel_: false, ReadOnly_: false,
	}, m: m, agentID: agentID, groupID: groupID}
}

func (t *hubAssignTaskTool) ValidParams(_ context.Context, _, arguments string) error {
	_, err := parseHubAssignArgs(arguments)
	return err
}

type hubAssignArgs struct {
	ToAgent     string   `json:"to_agent"`
	Instruction string   `json:"instruction"`
	Title       string   `json:"title"`
	ArtifactIDs []string `json:"artifact_ids"`
	ClientMsgID string   `json:"client_msg_id"`
}

func parseHubAssignArgs(arguments string) (hubAssignArgs, error) {
	var a hubAssignArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return a, fmt.Errorf("解析参数失败: %w", err)
	}
	if strings.TrimSpace(a.ToAgent) == "" {
		return a, fmt.Errorf("%w: to_agent 不能为空", agenthub.ErrInvalid)
	}
	if strings.TrimSpace(a.Instruction) == "" {
		return a, fmt.Errorf("%w: instruction 不能为空", agenthub.ErrInvalid)
	}
	return a, nil
}

func (t *hubAssignTaskTool) Call(ctx context.Context, _, arguments string) (string, error) {
	a, err := parseHubAssignArgs(arguments)
	if err != nil {
		return "", err
	}
	task, seq, dispatched, err := t.m.assignHubTask(ctx, hubAssignRequest{
		GroupID:     t.groupID,
		From:        t.agentID,
		To:          a.ToAgent,
		Instruction: a.Instruction,
		Title:       a.Title,
		ArtifactIDs: a.ArtifactIDs,
		ClientMsgID: a.ClientMsgID,
	})
	if err != nil {
		return "", err
	}
	return marshalHubToolResult(hubAssignResult(task, seq, dispatched))
}

// hubAssignResult 是 assign_task 工具与 hub_group_assign 命令共用的响应体
// （P3b-07a）：同一条业务路径必须给同一个形状，否则模型与前端各猜一套字段。
func hubAssignResult(task *agenthub.Task, seq int64, dispatched bool) map[string]any {
	out := map[string]any{
		"task": task, "task_id": string(task.ID), "status": task.Status,
		"to_agent": string(task.Target.AgentID), "seq": seq, "dispatched": dispatched,
	}
	// 说明按**最终状态**给，不能一律说「已入队」：泵可能已经把任务落成
	// canceled/failed（目标被移出群、目标侧去重、落账失败），说成 queued 会让
	// 模型以为对方迟早会收到（P3b-05 B-1）。
	if note := assignTaskNote(task); note != "" {
		out["note"] = note
	}
	return out
}

// hubHumanAssignGuard 是「人在群 UI 里派任务」的护栏（P3b-07a）：没有发起智能体，
// 所以按 agent 的并发/去重/智能体日预算三项都不适用（SkipAgentScope）；
// 全局/群日预算与链长仍然生效。
func (m *manager) hubHumanAssignGuard() *agenthub.Guard {
	return &agenthub.Guard{
		Limits: agenthub.GuardLimits{
			GlobalDailyBudgetUSD: agenthub.DefaultGlobalDailyBudgetUSD,
			SkipAgentScope:       true,
		},
		GroupDailyBudget: m.hubGroupDailyBudgetHook(),
	}
}

// assignTaskNote 是 assign_task 给模型的补充说明（空 = 不需要说明）。
// 只有 queued 才是「目标忙、等对方/控制器」；canceled/failed/declined 必须说清
// 任务已终止且不会再派发，并带上原因。
func assignTaskNote(task *agenthub.Task) string {
	if task == nil {
		return ""
	}
	switch task.Status {
	case agenthub.TaskQueued:
		return "目标当前正忙：任务已入队（queued），对方回合结束后由对话控制器派发；不要重复派同一件事"
	case agenthub.TaskCanceled, agenthub.TaskFailed, agenthub.TaskDeclined:
		reason := strings.TrimSpace(task.Error)
		if reason == "" && task.Result != nil {
			reason = strings.TrimSpace(task.Result.Error)
		}
		note := fmt.Sprintf("任务未能派发，已终止（%s）", task.Status)
		if reason != "" {
			note += "：" + reason
		}
		return note + "；不要重复派同一件事，先确认目标仍在群里且可用"
	}
	return ""
}

// hubAssignRequest 是 manager 层的指派输入（工具只做 JSON 解析，权限/账本都在这里）。
type hubAssignRequest struct {
	GroupID agenthub.GroupID
	// From 是发起智能体；FromHuman=true 时必须为空（人没有 agent id）。
	From agenthub.AgentID
	// FromHuman 人类从群 UI 派任务（P3b-07a）：Origin=human、群消息 Sender=human，
	// 跳过「发起者成员/派发权限/同对在途上限」三条按智能体的检查。
	FromHuman   bool
	To          string
	Instruction string
	Title       string
	ArtifactIDs []string
	ClientMsgID string
}

// assignHubTask 是 assign_task / hub_group_assign 的完整业务路径：校验 → 建账 →
// 写群消息 → 绑定 message seq → 派发（目标空闲时）。任何校验失败都在**写之前**返回，
// 不留任务/消息。人类来源与智能体来源共用这条路径，只有 Origin、群消息发送者与
// 「按发起智能体」的护栏不同（P3b-07a）。
func (m *manager) assignHubTask(ctx context.Context, req hubAssignRequest) (*agenthub.Task, int64, bool, error) {
	if req.GroupID == "" {
		return nil, 0, false, fmt.Errorf("%w: assign 需要群", agenthub.ErrInvalid)
	}
	if req.FromHuman {
		if req.From != "" {
			return nil, 0, false, fmt.Errorf("%w: 人类来源不能带发起智能体", agenthub.ErrInvalid)
		}
	} else if req.From == "" {
		return nil, 0, false, fmt.Errorf("%w: assign 需要发起者", agenthub.ErrInvalid)
	}
	instruction := strings.TrimSpace(req.Instruction)
	if instruction == "" {
		return nil, 0, false, fmt.Errorf("%w: instruction 不能为空", agenthub.ErrInvalid)
	}
	st, err := m.hubStore()
	if err != nil {
		return nil, 0, false, err
	}
	g, err := st.LoadGroup(ctx, req.GroupID)
	if err != nil {
		return nil, 0, false, err
	}
	if g == nil || g.Deleted {
		return nil, 0, false, fmt.Errorf("%w: 群组 %s 不存在", agenthub.ErrNotFound, req.GroupID)
	}
	// 鉴权顺序：先确认发起者是成员，再看权限，最后才解析目标（不向非成员泄漏成员名）。
	// 人类来源没有发起智能体：这两步不适用（人就是群的所有者/使用者），
	// 但目标校验（成员/存活/非自己）照旧。
	var from *agenthub.AgentProfile
	if !req.FromHuman {
		if _, member := groupMemberOf(g, req.From); !member {
			return nil, 0, false, fmt.Errorf("%w: 智能体不是该群成员", agenthub.ErrInvalid)
		}
		from, _, err = m.hubLoadAgent(string(req.From))
		if err != nil || from == nil {
			return nil, 0, false, fmt.Errorf("%w: 发起者智能体不可用", agenthub.ErrNotFound)
		}
		if !from.Policy.CanDispatch {
			return nil, 0, false, fmt.Errorf("%w: 没有派发权限（policy.can_dispatch=false）", agenthub.ErrForbidden)
		}
	}
	to, err := resolveAssignTarget(ctx, st, g, req.From, req.To)
	if err != nil {
		return nil, 0, false, err
	}
	// artifact_ids 只能挂已登记且自己在群里可见的产物（与 handoff 同口径）。
	for _, raw := range req.ArtifactIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			return nil, 0, false, fmt.Errorf("%w: artifact_id 不能为空", agenthub.ErrInvalid)
		}
		art, aerr := st.LoadArtifact(ctx, agenthub.ArtifactID(id))
		if aerr != nil {
			return nil, 0, false, fmt.Errorf("%w: 产物 %s 不存在", agenthub.ErrInvalid, id)
		}
		if art.GroupID != "" && art.GroupID != g.ID {
			return nil, 0, false, fmt.Errorf("%w: 产物 %s 不属于本群", agenthub.ErrInvalid, id)
		}
		if !agenthub.ArtifactVisibleTo(art, req.From, req.FromHuman) {
			return nil, 0, false, fmt.Errorf("%w: 无权引用产物 %s", agenthub.ErrInvalid, id)
		}
	}
	title := assignTaskTitle(req.Title, instruction)
	target := agenthub.TaskTarget{
		Kind: agenthub.TargetAgent, AgentID: to, GroupID: g.ID,
		// 确定性会话 ID（同一 (agent, group) 只有一个群会话），P3b-05 据此结算。
		SessionID: hubSessionID(string(to), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID}),
	}
	clientID := assignClientMsgID(req.From, to, req.ClientMsgID)
	ledger := agenthub.NewLedger(st)
	// 重试快路径：同一 (from,to,client_msg_id) 的非终态任务直接返回，不再写消息/派发。
	// 终态（failed/canceled/...）不短路：交给 Queue.SubmitAgent 决定是重投成新 attempt
	// （可重试终态）还是返回旧任务（completed/delivered），与 session_send 口径一致。
	if existing, ferr := ledger.FindByClientMsgID(ctx, target, clientID); ferr != nil {
		return nil, 0, false, ferr
	} else if existing != nil && !existing.Status.Terminal() {
		return existing, existing.Origin.MessageSeq, existing.Status == agenthub.TaskDispatched ||
			existing.Status == agenthub.TaskRunning, nil
	}
	q, err := m.hubQueueFor()
	if err != nil {
		return nil, 0, false, err
	}
	var origin agenthub.TaskOrigin
	var guard *agenthub.Guard
	if req.FromHuman {
		// 人没有 agent id：Origin 只带群。护栏也只有全局/群日预算与链长
		// （并发/去重/智能体日预算按发起智能体算，人没有这一层）。
		origin = agenthub.TaskOrigin{Kind: agenthub.OriginHuman, GroupID: g.ID}
		guard = m.hubHumanAssignGuard()
	} else {
		origin = agenthub.TaskOrigin{
			Kind: agenthub.OriginGroup, GroupID: g.ID, AgentID: req.From, AgentName: from.Name,
		}
		guard = m.hubWorkspaceGuard(*from)
	}
	created, err := q.SubmitAgent(ctx, &agenthub.Task{
		Origin:      origin,
		Target:      target,
		Title:       title,
		Instruction: instruction,
		ArtifactIDs: append([]string(nil), req.ArtifactIDs...),
		Callback:    agenthub.CallbackAgentEnd,
		ClientMsgID: clientID,
	}, guard)
	if err != nil {
		return nil, 0, false, err
	}
	// 群消息：kind=text + intent=assign + task_id + mentions=[B]（结构化，不是正文 @）。
	// 只用结构化 mentions 路由：instruction 里的 @ 是给 B 的正文，不该把 C 也拉进来。
	seq, deduped, err := m.appendAssignMessage(ctx, st, g, created, instruction)
	if err != nil {
		// 消息没写成：任务还没派发，取消它 —— 不能留一条「有任务、无消息」的指派。
		cancelCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
		_, cerr := q.CancelQueued(cancelCtx, created.ID)
		cancel()
		if cerr != nil {
			// 回滚失败不能静默：任务可能已经被别的路径派发/结算，调用方却只看到
			// 「写消息失败」。留一条任务级告警给任务板（P3b-03 N3）。
			m.emitHubTaskAlert(created.ID, "assign_rollback_failed",
				"写指派消息失败后取消任务也失败（任务可能已被派发）: "+cerr.Error())
		}
		return nil, 0, false, fmt.Errorf("写指派消息失败: %w", err)
	}
	if req.FromHuman && !deduped && seq > 0 {
		// 人类派任务也是一条人类消息：必须开新对话/清接龙队列（§6.5.6 规则 1/5）。
		// 否则人在派活之后，上一段自动对话会继续接龙，目标带着旧对话的预算/深度
		// 继续跑（P3b-08 P2）。deduped 重试不再重复占首轮预算。
		m.beginHubConversation(g, seq, 1)
	}
	if seq > 0 && created.Origin.MessageSeq != seq {
		if bound, berr := ledger.BindOriginMessageSeq(ctx, created.ID, seq); berr == nil {
			created = bound
		} else {
			// seq 只用于展示/关联；绑定失败不影响任务本身，但要留痕。
			m.emitHubTaskAlert(created.ID, "assign_meta_failed", berr.Error())
		}
	}
	// 事件顺序：消息（人看到的）→ 任务 queued → 任务 dispatched。重试命中已存在
	// 的消息（deduped）时不再重发 hub_message，避免前端重复气泡。
	dispatchedTask := created
	dispatched := false
	if !deduped || created.Status == agenthub.TaskQueued {
		m.emitHubTaskUpdate(created)
	}
	if pumped, perr := m.pumpAgentTask(ctx, created.Target); perr != nil {
		// 任务仍是 queued（没投出去）或已被泵成终态：指派本身已成立，把失败如实
		// 回给模型与任务板（终态的卡/回推由 pumpAgentTask 负责，P3b-05 B-1）。
		m.emitHubTaskAlert(created.ID, "dispatch_failed", perr.Error())
	} else if pumped != nil && pumped.ID == created.ID {
		dispatchedTask = pumped
		dispatched = pumped.Status == agenthub.TaskDispatched || pumped.Status == agenthub.TaskRunning
		if dispatched && !deduped {
			m.emitHubTaskUpdate(pumped)
		}
	}
	if latest, rerr := ledger.Replay(ctx, created.ID); rerr == nil && latest != nil {
		dispatchedTask = latest
		dispatched = latest.Status == agenthub.TaskDispatched || latest.Status == agenthub.TaskRunning
	}
	return dispatchedTask, seq, dispatched, nil
}

// appendAssignMessage 落一条 intent=assign 的群消息 + 路由回执。
// 返回的 seq 是触发消息自己的 seq（回执在其后）。
//
// 发送者从任务 Origin 推导（不另收参数）：智能体来源 = SenderAgent + agent id，
// 人类来源 = SenderHuman（P3b-07a）—— 两者必须始终与账本里的 Origin 一致，
// 否则群日志会出现「人派的任务署了某智能体的名」。
//
// 刻意不持 hubGroupSendMu：那把锁保护的是「轮次上限检查 → 消息+回执」的原子性，
// 而指派不参与接龙限流（P3b-04 负责循环/预算）；消息+回执的原子性由
// AppendMessageWithRound 的存储锁保证，锁范围不必扩大到会话装配。
func (m *manager) appendAssignMessage(ctx context.Context, st agenthub.Store, g *agenthub.Group, task *agenthub.Task, instruction string) (int64, bool, error) {
	if g == nil || task == nil {
		return 0, false, fmt.Errorf("%w: assign 消息参数为空", agenthub.ErrInvalid)
	}
	human := task.Origin.Kind == agenthub.OriginHuman
	if !human && task.Origin.Kind != agenthub.OriginGroup {
		return 0, false, fmt.Errorf("%w: 指派消息来源只有群内智能体/群内人类（得到 %q）", agenthub.ErrInvalid, task.Origin.Kind)
	}
	from := task.Origin.AgentID
	if !human && from == "" {
		return 0, false, fmt.Errorf("%w: 指派消息缺少发起智能体", agenthub.ErrInvalid)
	}
	// 写消息前的确定性屏障（测试注入；生产恒为 nil）：用来复现「任务已建、消息没写成」
	// 的回滚路径，而不是靠 sleep/关磁盘（P3b-03 R2）。
	if err := hubAssignMessageBarrier(task.ID); err != nil {
		return 0, false, err
	}
	// 正文是 instruction（给 B 的内容），不参与 @ 路由：只传结构化 mentions。
	routed, err := m.routeGroupMentions(ctx, st, g, from, human, agenthub.MentionRequest{
		Mentions: []agenthub.AgentID{task.Target.AgentID},
	})
	if err != nil {
		return 0, false, err
	}
	sender := agenthub.Sender{Kind: agenthub.SenderAgent, AgentID: from}
	if human {
		sender = agenthub.Sender{Kind: agenthub.SenderHuman}
	}
	msg := &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindText,
		Sender: sender,
		Text:   instruction, Intent: agenthub.IntentAssign, TaskID: task.ID,
		Visibility: agenthub.VisibilityGroup,
	}
	if task.ClientMsgID != "" {
		// 消息侧幂等键按 (调用方键, task_id) 作用域：同一次调用的重试命中同一条消息；
		// 失败后重投出的新 attempt 会写自己的 assign 消息（不会把旧消息挂到新任务上）。
		msg.Refs = map[string]any{"client_msg_id": task.ClientMsgID + "#" + string(task.ID)}
	}
	if routed != nil {
		msg.Mentions = append([]agenthub.AgentID(nil), routed.Mentions...)
		msg.MentionAll = routed.MentionAll
	} else {
		msg.Mentions = []agenthub.AgentID{task.Target.AgentID}
	}
	seq, deduped, err := agenthub.AppendGroupMessageRouted(ctx, st, msg, routed)
	if err != nil {
		return 0, false, err
	}
	if !deduped {
		m.emitHubGroupMessage(g.ID, seq, msg)
		if routed != nil {
			m.emitHubGroupRound(g.ID, *routed)
		}
	}
	return seq, deduped, nil
}

// resolveAssignTarget 把 to_agent 解析成**活跃**群成员：先按 agent id 精确匹配，
// 再按成员名大小写不敏感唯一匹配（与 @ 解析器同一 fold 规则）。
func resolveAssignTarget(ctx context.Context, st agenthub.Store, g *agenthub.Group, from agenthub.AgentID, raw string) (agenthub.AgentID, error) {
	name := strings.TrimSpace(raw)
	name = strings.TrimPrefix(name, "@")
	if name == "" {
		return "", fmt.Errorf("%w: to_agent 不能为空", agenthub.ErrInvalid)
	}
	member, ok := groupMemberOf(g, agenthub.AgentID(name))
	if !ok {
		agents, err := st.ListAgents(ctx)
		if err != nil {
			return "", err
		}
		byID := make(map[agenthub.AgentID]*agenthub.AgentProfile, len(agents))
		for _, p := range agents {
			if p != nil {
				byID[p.ID] = p
			}
		}
		matches := make([]agenthub.AgentID, 0, 1)
		for _, mem := range g.Members {
			p := byID[mem.AgentID]
			if p == nil || p.Deleted {
				continue
			}
			if strings.EqualFold(strings.TrimSpace(p.Name), name) {
				matches = append(matches, mem.AgentID)
			}
		}
		switch len(matches) {
		case 0:
			return "", fmt.Errorf("%w: %q 不是群内成员（用 agent_id 或成员名）", agenthub.ErrInvalid, name)
		case 1:
			member, _ = groupMemberOf(g, matches[0])
		default:
			return "", fmt.Errorf("%w: 名称 %q 匹配多个成员，请改用 agent_id", agenthub.ErrInvalid, name)
		}
	}
	if member.AgentID == from {
		return "", fmt.Errorf("%w: 不能把任务派给自己（用 todo/直接做）", agenthub.ErrInvalid)
	}
	p, err := st.LoadAgent(ctx, member.AgentID)
	if err != nil {
		return "", fmt.Errorf("%w: 目标智能体不存在", agenthub.ErrNotFound)
	}
	if p.Deleted {
		return "", fmt.Errorf("%w: 目标智能体已删除", agenthub.ErrInvalid)
	}
	return member.AgentID, nil
}

// assignClientMsgID 把幂等键限制在同一 (发起者, 目标) 内：两个智能体各自用
// "retry-1" 给同一个目标派任务时不能互相命中（与账本/消息两条去重键一致）。
// 人类来源没有 agent id，用固定前缀 "human" 占位 —— 与任何 ag_* 发起者的键空间
// 不相交，同时保持「同一客户端键 + 同一目标 = 同一条任务」的幂等语义（P3b-07a）。
func assignClientMsgID(from, to agenthub.AgentID, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	scope := string(from)
	if scope == "" {
		scope = "human"
	}
	return scope + ":" + string(to) + ":" + raw
}

// assignTaskTitle 缺省标题取 instruction 首行（任务板需要非空标题）。
func assignTaskTitle(title, instruction string) string {
	title = strings.TrimSpace(title)
	if title != "" {
		return truncateRunes(title, 80)
	}
	line := strings.TrimSpace(strings.SplitN(instruction, "\n", 2)[0])
	if line == "" {
		return "群内指派"
	}
	return truncateRunes(line, 40)
}
