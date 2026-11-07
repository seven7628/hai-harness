package main

// hub_group_commands.go：群组 CRUD + 成员/角色/策略（P3a-01，§6.5/§7.1.3）。
//
// 领域校验在 agenthub.Group.Normalize/Validate（成员上限 12、角色、唯一 owner、
// 策略默认 ask、显示名不重复）；这里只做命令编排与 owner 转让的成对改写。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

func (m *manager) hubGroupList(c command) {
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	all, err := st.ListGroups(context.Background())
	if err != nil {
		m.resp(c, false, "读取群组失败: "+err.Error(), nil)
		return
	}
	out := make([]*agenthub.Group, 0, len(all))
	for _, g := range all {
		if g != nil && !g.Deleted {
			out = append(out, g)
		}
	}
	m.resp(c, true, "", map[string]any{"groups": out, "count": len(out)})
}

func (m *manager) hubGroupGet(c command) {
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	id := agenthub.GroupID(strings.TrimSpace(str(c.Payload, "group_id")))
	if id == "" {
		m.resp(c, false, "group_id 不能为空", nil)
		return
	}
	g, err := st.LoadGroup(context.Background(), id)
	if err != nil {
		m.resp(c, false, "读取群组失败: "+err.Error(), nil)
		return
	}
	if g.Deleted {
		m.resp(c, false, "群组已删除", nil)
		return
	}
	m.resp(c, true, "", map[string]any{"group": g})
}

func (m *manager) hubGroupCreate(c command) {
	ctx := context.Background()
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	var req struct {
		Name         string               `json:"name"`
		OwnerAgentID agenthub.AgentID     `json:"owner_agent_id"`
		MemberIDs    []agenthub.AgentID   `json:"member_ids"`
		Members      []agenthub.Member    `json:"members"`
		Policy       agenthub.GroupPolicy `json:"policy"`
		Pinned       bool                 `json:"pinned"`
	}
	if err := decodeGroupPayload(c.Payload, &req); err != nil {
		m.resp(c, false, "参数格式错误: "+err.Error(), nil)
		return
	}
	if len(req.Members) > 0 && len(req.MemberIDs) > 0 {
		m.resp(c, false, "members 与 member_ids 只能给一个", nil)
		return
	}
	req.OwnerAgentID = agenthub.AgentID(strings.TrimSpace(string(req.OwnerAgentID)))
	for i := range req.MemberIDs {
		req.MemberIDs[i] = agenthub.AgentID(strings.TrimSpace(string(req.MemberIDs[i])))
		if req.MemberIDs[i] == "" {
			m.resp(c, false, "member_ids 不能包含空 id", nil)
			return
		}
	}
	// §7.1.3：兼容 member_ids 简写（角色默认 member），owner 缺省取第一个成员。
	// 显式 owner_agent_id 时把对应成员的角色升为 owner。
	members := req.Members
	if len(members) == 0 && len(req.MemberIDs) > 0 {
		members = make([]agenthub.Member, 0, len(req.MemberIDs))
		for _, id := range req.MemberIDs {
			members = append(members, agenthub.Member{AgentID: id, Role: agenthub.RoleMember})
		}
	}
	owner := req.OwnerAgentID
	if owner == "" && len(req.Members) == 0 && len(members) > 0 {
		owner = members[0].AgentID
	}
	if owner != "" {
		for i := range members {
			if members[i].AgentID == owner {
				members[i].Role = agenthub.RoleOwner
			}
		}
	}
	g := &agenthub.Group{Name: req.Name, OwnerAgentID: owner, Members: members, Policy: req.Policy, Pinned: req.Pinned}
	now := time.Now().UTC()
	for i := range g.Members {
		g.Members[i].JoinedAt = now
	}
	if err := m.validateGroupAgents(ctx, g, nil); err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	groups, err := st.ListGroups(ctx)
	if err != nil {
		m.resp(c, false, "读取群组失败: "+err.Error(), nil)
		return
	}
	if err := agenthub.ValidateGroupNameUnique(g.Name, groups, ""); err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	id, err := st.AllocateID(ctx, agenthub.PrefixGroup)
	if err != nil {
		m.resp(c, false, "分配群组 ID 失败: "+err.Error(), nil)
		return
	}
	g.ID = agenthub.GroupID(id)
	g.CreatedAt, g.UpdatedAt = now, now
	// P2.5-04/D1：新群默认日预算 $50；显式 0 = 不限额（与 UI 文案一致）。
	if policy, ok := c.Payload["policy"].(map[string]any); !ok {
		g.Policy.DailyBudgetUSD = agenthub.DefaultGroupDailyBudgetUSD
	} else if _, ok := policy["daily_budget_usd"]; !ok {
		g.Policy.DailyBudgetUSD = agenthub.DefaultGroupDailyBudgetUSD
	}
	if err := g.Normalize(); err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	if err := st.SaveGroup(ctx, g); err != nil {
		m.resp(c, false, "保存群组失败: "+err.Error(), nil)
		return
	}
	m.resp(c, true, "", map[string]any{"group": g})
}

func (m *manager) hubGroupUpdate(c command) {
	ctx := context.Background()
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	id := agenthub.GroupID(strings.TrimSpace(str(c.Payload, "group_id")))
	if id == "" {
		m.resp(c, false, "group_id 不能为空", nil)
		return
	}
	g, err := st.LoadGroup(ctx, id)
	if err != nil {
		m.resp(c, false, "读取群组失败: "+err.Error(), nil)
		return
	}
	if g.Deleted {
		m.resp(c, false, "群组已删除", nil)
		return
	}
	var patch struct {
		Name         *string           `json:"name"`
		OwnerAgentID *agenthub.AgentID `json:"owner_agent_id"`
		Policy       *struct {
			Mode             *string  `json:"mode"`
			MaxParallelRuns  *int     `json:"max_parallel_runs"`
			MaxHandoffDepth  *int     `json:"max_handoff_depth"`
			MaxTurnsPerHuman *int     `json:"max_turns_per_human"`
			MaxMentions      *int     `json:"max_mentions_per_message"`
			DailyBudgetUSD   *float64 `json:"daily_budget_usd"`
		} `json:"policy"`
		Pinned *bool `json:"pinned"`
	}
	if err := decodeGroupPayload(c.Payload["patch"], &patch); err != nil {
		m.resp(c, false, "patch 格式错误: "+err.Error(), nil)
		return
	}
	next := *g
	next.Members = append([]agenthub.Member(nil), g.Members...)
	if patch.Name != nil {
		next.Name = *patch.Name
	}
	if patch.Policy != nil {
		// 字段级 patch：未传的字段保留原值，避免改 mode 时把预算/并发静默重置。
		pp := patch.Policy
		if pp.Mode != nil {
			next.Policy.Mode = *pp.Mode
		}
		if pp.MaxParallelRuns != nil {
			next.Policy.MaxParallelRuns = *pp.MaxParallelRuns
		}
		if pp.MaxHandoffDepth != nil {
			next.Policy.MaxHandoffDepth = *pp.MaxHandoffDepth
		}
		if pp.MaxTurnsPerHuman != nil {
			next.Policy.MaxTurnsPerHuman = *pp.MaxTurnsPerHuman
		}
		if pp.MaxMentions != nil {
			next.Policy.MaxMentions = *pp.MaxMentions
		}
		if pp.DailyBudgetUSD != nil {
			next.Policy.DailyBudgetUSD = *pp.DailyBudgetUSD
		}
	}
	if patch.Pinned != nil {
		next.Pinned = *patch.Pinned
	}
	if patch.OwnerAgentID != nil {
		want := agenthub.AgentID(strings.TrimSpace(string(*patch.OwnerAgentID)))
		if want == "" {
			m.resp(c, false, "owner_agent_id 不能为空", nil)
			return
		}
		if want != g.OwnerAgentID {
			// 新 owner 必须存活；旧 owner 可以是已删除成员（群记录保留）。
			if err := m.validateHubAgentActive(ctx, want); err != nil {
				m.resp(c, false, err.Error(), nil)
				return
			}
			next.OwnerAgentID = want
			// owner 转让是成对改写：旧 owner 降 member、新 owner 升 owner。
			found := false
			for i := range next.Members {
				if next.Members[i].AgentID == g.OwnerAgentID {
					next.Members[i].Role = agenthub.RoleMember
				}
				if next.Members[i].AgentID == want {
					next.Members[i].Role = agenthub.RoleOwner
					found = true
				}
			}
			if !found {
				m.resp(c, false, "新 owner 必须是群成员", nil)
				return
			}
		}
	}
	if !strings.EqualFold(next.Name, g.Name) {
		groups, err := st.ListGroups(ctx)
		if err != nil {
			m.resp(c, false, "读取群组失败: "+err.Error(), nil)
			return
		}
		if err := agenthub.ValidateGroupNameUnique(next.Name, groups, g.ID); err != nil {
			m.resp(c, false, err.Error(), nil)
			return
		}
	}
	if err := m.validateGroupAgents(ctx, &next, groupMemberSet(g)); err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	next.ID, next.CreatedAt = g.ID, g.CreatedAt
	next.UpdatedAt = time.Now().UTC()
	if err := next.Normalize(); err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	if err := st.SaveGroup(ctx, &next); err != nil {
		m.resp(c, false, "保存群组失败: "+err.Error(), nil)
		return
	}
	// 群名/owner（角色成对改写）进群名册（system prompt L2）：重建成员会话 loop，
	// 下一个 Run 就能看到新名册。只换 loop，不打断在跑的回合。
	rosterChanged := next.Name != g.Name || next.OwnerAgentID != g.OwnerAgentID
	m.respGroupChanged(c, &next, rosterChanged)
}

func (m *manager) hubGroupDelete(c command) {
	ctx := context.Background()
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	id := agenthub.GroupID(strings.TrimSpace(str(c.Payload, "group_id")))
	g, err := st.LoadGroup(ctx, id)
	if err != nil {
		m.resp(c, false, "读取群组失败: "+err.Error(), nil)
		return
	}
	if g.Deleted {
		m.resp(c, true, "", map[string]any{"group": g})
		return
	}
	g.Deleted = true
	g.UpdatedAt = time.Now().UTC()
	if err := st.SaveGroup(ctx, g); err != nil {
		m.resp(c, false, "删除群组失败: "+err.Error(), nil)
		return
	}
	// 不重建成员 loop：已删除的群不会再被唤醒（发送/唤醒路径都拒绝 deleted 群），
	// 旧会话留在内存里只为让在跑的回合正常收尾。
	m.resp(c, true, "", map[string]any{"group": g})
}

func (m *manager) hubGroupMemberAdd(c command) {
	ctx := context.Background()
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	id := agenthub.GroupID(strings.TrimSpace(str(c.Payload, "group_id")))
	agentID := agenthub.AgentID(strings.TrimSpace(str(c.Payload, "agent_id")))
	role := agenthub.MemberRole(strings.ToLower(strings.TrimSpace(str(c.Payload, "role"))))
	if role == "" {
		role = agenthub.RoleMember
	}
	g, err := st.LoadGroup(ctx, id)
	if err != nil {
		m.resp(c, false, "读取群组失败: "+err.Error(), nil)
		return
	}
	if g.Deleted {
		m.resp(c, false, "群组已删除", nil)
		return
	}
	for _, member := range g.Members {
		if member.AgentID == agentID {
			m.resp(c, false, "该智能体已在群里", nil)
			return
		}
	}
	next := *g
	next.Members = append(append([]agenthub.Member(nil), g.Members...), agenthub.Member{
		AgentID: agentID, Role: role, JoinedAt: time.Now().UTC(),
	})
	if err := m.validateGroupAgents(ctx, &next, groupMemberSet(g)); err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	next.UpdatedAt = time.Now().UTC()
	if err := next.Normalize(); err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	if err := st.SaveGroup(ctx, &next); err != nil {
		m.resp(c, false, "保存群组失败: "+err.Error(), nil)
		return
	}
	// 新成员要进每个老成员的群名册（system prompt L2）：重建现有成员会话 loop。
	m.respGroupChanged(c, &next, true)
}

func (m *manager) hubGroupMemberRemove(c command) {
	ctx := context.Background()
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	id := agenthub.GroupID(strings.TrimSpace(str(c.Payload, "group_id")))
	agentID := agenthub.AgentID(strings.TrimSpace(str(c.Payload, "agent_id")))
	g, err := st.LoadGroup(ctx, id)
	if err != nil {
		m.resp(c, false, "读取群组失败: "+err.Error(), nil)
		return
	}
	if g.Deleted {
		m.resp(c, false, "群组已删除", nil)
		return
	}
	if agentID == g.OwnerAgentID {
		m.resp(c, false, "不能移除群 owner，请先转让", nil)
		return
	}
	next := *g
	next.Members = make([]agenthub.Member, 0, len(g.Members))
	found := false
	for _, member := range g.Members {
		if member.AgentID == agentID {
			found = true
			continue
		}
		next.Members = append(next.Members, member)
	}
	if !found {
		m.resp(c, false, "该智能体不在群里", nil)
		return
	}
	next.UpdatedAt = time.Now().UTC()
	if err := next.Normalize(); err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	if err := st.SaveGroup(ctx, &next); err != nil {
		m.resp(c, false, "保存群组失败: "+err.Error(), nil)
		return
	}
	// 移除的成员从名册里消失：重建剩下成员的群 loop（被移除者自己不再被唤醒，
	// 旧会话不重建也不关闭 —— 在跑的回合要能正常收尾）。
	m.respGroupChanged(c, &next, true)
}

// respGroupChanged 群变更后的统一响应：名册变了就重建成员们的群会话 loop，
// 失败不静默 —— 把原因带回响应，UI 能提示「重启后生效」，而不是模型一直看旧名册。
func (m *manager) respGroupChanged(c command, g *agenthub.Group, rosterChanged bool) {
	data := map[string]any{"group": g}
	if rosterChanged {
		if err := m.refreshHubGroupRoster(g.ID); err != nil {
			addHubWarning(data, "群已保存，但重建成员会话提示词失败，下一次唤醒仍用旧名册: "+err.Error())
		}
	}
	m.resp(c, true, "", data)
}

// validateGroupAgents 成员必须存在；新成员/新 owner 必须存活，已在群里的
// 软删成员可以保留（否则删除 owner 后群会变成无法转让/改名/加人的孤儿）。
func (m *manager) validateGroupAgents(ctx context.Context, g *agenthub.Group, allowDeleted map[agenthub.AgentID]struct{}) error {
	st, err := m.hubStore()
	if err != nil {
		return err
	}
	all, err := st.ListAgents(ctx)
	if err != nil {
		return fmt.Errorf("读取智能体失败: %w", err)
	}
	profiles := make(map[agenthub.AgentID]*agenthub.AgentProfile, len(all))
	for _, p := range all {
		if p != nil {
			profiles[p.ID] = p
		}
	}
	if err := g.Normalize(); err != nil {
		return err
	}
	active := *g
	active.Members = make([]agenthub.Member, 0, len(g.Members))
	for _, member := range g.Members {
		p, ok := profiles[member.AgentID]
		if !ok || p == nil {
			// purge 会物理删 profile；已在群的旧成员不能因此把群变成孤儿。
			if _, keep := allowDeleted[member.AgentID]; keep {
				continue
			}
			return fmt.Errorf("%w: group member %s has no profile", agenthub.ErrInvalid, member.AgentID)
		}
		if p.Deleted {
			if _, keep := allowDeleted[member.AgentID]; keep {
				continue
			}
			return fmt.Errorf("%w: group member %s is deleted", agenthub.ErrInvalid, member.AgentID)
		}
		active.Members = append(active.Members, member)
	}
	return agenthub.ValidateGroupMemberNames(&active, profiles)
}

func groupMemberSet(g *agenthub.Group) map[agenthub.AgentID]struct{} {
	if g == nil {
		return nil
	}
	out := make(map[agenthub.AgentID]struct{}, len(g.Members))
	for _, m := range g.Members {
		out[m.AgentID] = struct{}{}
	}
	return out
}

func (m *manager) validateHubAgentActive(ctx context.Context, id agenthub.AgentID) error {
	p, _, err := m.hubLoadAgent(string(id))
	if err != nil {
		return fmt.Errorf("读取智能体失败: %w", err)
	}
	if p == nil {
		return fmt.Errorf("%w: agent %s not found", agenthub.ErrNotFound, id)
	}
	if p.Deleted {
		return fmt.Errorf("%w: agent %s is deleted", agenthub.ErrInvalid, id)
	}
	return nil
}

func decodeGroupPayload(v any, out any) error {
	if v == nil {
		return fmt.Errorf("缺少参数")
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, out)
}

// groupHistoryHasMore 用生效分页的第一条 seq 判断是否还有更早消息。
// 不能拿请求 limit 与返回条数比较：store 会把 limit 钳到 1..500。
func groupHistoryHasMore(msgs []agenthub.Message) bool {
	return len(msgs) > 0 && msgs[0].Seq > 1
}

// hubGroupHistory 群日志分页（before_seq=0 最新页）。
func (m *manager) hubGroupHistory(c command) {
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	gid := agenthub.GroupID(strings.TrimSpace(str(c.Payload, "group_id")))
	limit := int(numOr(c.Payload, "limit", 50))
	msgs, err := agenthub.ReadGroupLog(context.Background(), st, gid, numOr(c.Payload, "before_seq", 0), limit)
	if err != nil {
		m.resp(c, false, "读取群日志失败: "+err.Error(), nil)
		return
	}
	if msgs == nil {
		msgs = []agenthub.Message{} // 空页必须是 []，前端直接 map
	}
	m.resp(c, true, "", map[string]any{"messages": msgs, "has_more": groupHistoryHasMore(msgs)})
}

// hubGroupActivity 群活动分页（独立流，不进快照）。
func (m *manager) hubGroupActivity(c command) {
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	gid := agenthub.GroupID(strings.TrimSpace(str(c.Payload, "group_id")))
	acts, err := agenthub.ReadGroupActivity(context.Background(), st, gid, numOr(c.Payload, "after_seq", 0), int(numOr(c.Payload, "limit", 100)))
	if err != nil {
		m.resp(c, false, "读取群活动失败: "+err.Error(), nil)
		return
	}
	m.resp(c, true, "", map[string]any{"activity": acts})
}

// hubGroupSend 人发群消息：落日志 → @ 路由 → 回执 → hub_message/hub_round 事件。
// 智能体唤醒/群会话装配归 P3a-05+（本轮只保证日志、路由与回执是真的）。
func (m *manager) hubGroupSend(c command) {
	ctx := context.Background()
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	gid := agenthub.GroupID(strings.TrimSpace(str(c.Payload, "group_id")))
	text := strings.TrimSpace(str(c.Payload, "text"))
	if text == "" {
		m.resp(c, false, "text 不能为空", nil)
		return
	}
	// 与 group_say 串行：限流检查、client_msg_id 去重、消息+回执写入必须是一个临界区。
	m.hubGroupSendMu.Lock()
	defer m.hubGroupSendMu.Unlock()
	g, err := st.LoadGroup(ctx, gid)
	if err != nil {
		m.resp(c, false, "读取群组失败: "+err.Error(), nil)
		return
	}
	if g.Deleted {
		m.resp(c, false, "群组已删除", nil)
		return
	}
	msg := &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindText,
		Sender: agenthub.Sender{Kind: agenthub.SenderHuman}, Text: text,
	}
	if id := strings.TrimSpace(str(c.Payload, "client_msg_id")); id != "" {
		msg.Refs = map[string]any{"client_msg_id": id}
	}
	// 构造路由（成员 profile 用于严格 @ 解析）。先路由后落日志：保留名/解析失败
	// 不能留下没有回执的半条消息（P3a-05 B1）。
	agents, err := st.ListAgents(ctx)
	if err != nil {
		m.resp(c, false, "读取智能体失败: "+err.Error(), nil)
		return
	}
	byID := map[agenthub.AgentID]*agenthub.AgentProfile{}
	profiles := make([]*agenthub.AgentProfile, 0, len(g.Members))
	for _, a := range agents {
		if a != nil {
			byID[a.ID] = a
		}
	}
	for _, member := range g.Members {
		if p := byID[member.AgentID]; p != nil && !p.Deleted {
			profiles = append(profiles, p)
		}
	}
	router, err := agenthub.NewGroupRouter(g, profiles)
	if err != nil {
		m.resp(c, false, "构造路由失败: "+err.Error(), nil)
		return
	}
	round, err := router.Route(text, "", true, 0, time.Now())
	if err != nil {
		m.resp(c, false, "路由失败: "+err.Error(), nil)
		return
	}
	// 无 @ 只写日志，不产生回执（设计 §6.6.3）；有 @ 则消息与回执在同一事务内。
	var routed *agenthub.Round
	if round.MentionAll || len(round.Targets) > 0 {
		routed = &round
	}
	seq, deduped, err := agenthub.AppendGroupMessageRouted(ctx, st, msg, routed)
	if err != nil {
		m.resp(c, false, "写入群日志失败: "+err.Error(), nil)
		return
	}
	round.TriggerSeq = seq
	if deduped {
		// 断线重试：同 client_msg_id 不重复写日志，也不再发 hub_message/hub_round。
		// 不返回本次新 Route 的 round：落盘回执属于首次请求，返回它会让 round.ID 对不上（复审 NB-2）。
		m.resp(c, true, "", map[string]any{
			"seq": seq, "message_id": msg.ID, "deduplicated": true,
		})
		return
	}
	m.emitHubGroupMessage(g.ID, seq, msg)
	// 无 @ 只写日志（N-1）：不产生 hub_round，否则前端会误报「无人被唤醒」。
	var wakeErrors []string
	if routed != nil {
		m.out.writeLine(map[string]any{"event_type": "hub_round", "group_id": string(g.ID), "round": round})
		// 先落日志/回执，再唤醒；唤醒失败不能把已写入的群消息回滚（人已经看到了）。
		wakeErrors = m.wakeGroupRound(g, round, msg)
		for _, we := range wakeErrors {
			m.hubAlertf("group_wake_failed", "群 %s 唤醒失败：%s", g.ID, we)
		}
	}
	respData := map[string]any{"seq": seq, "message_id": msg.ID, "round": round, "targets": round.Targets}
	if len(wakeErrors) > 0 {
		respData["wake_errors"] = wakeErrors
	}
	m.resp(c, true, "", respData)
}

func (m *manager) emitHubGroupMessage(gid agenthub.GroupID, seq int64, msg *agenthub.Message) {
	if m == nil || m.out == nil || msg == nil {
		return
	}
	m.out.writeLine(map[string]any{
		"event_type": "hub_message", "group_id": string(gid), "seq": seq,
		"message": msg, "client_msg_id": msg.Refs["client_msg_id"],
	})
}
