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
		Members      []agenthub.Member    `json:"members"`
		Policy       agenthub.GroupPolicy `json:"policy"`
		Pinned       bool                 `json:"pinned"`
	}
	if err := decodeGroupPayload(c.Payload, &req); err != nil {
		m.resp(c, false, "参数格式错误: "+err.Error(), nil)
		return
	}
	g := &agenthub.Group{Name: req.Name, OwnerAgentID: req.OwnerAgentID, Members: req.Members, Policy: req.Policy, Pinned: req.Pinned}
	now := time.Now().UTC()
	for i := range g.Members {
		g.Members[i].JoinedAt = now
	}
	if err := m.validateGroupAgents(ctx, g); err != nil {
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
	var patch struct {
		Name         *string               `json:"name"`
		OwnerAgentID *agenthub.AgentID     `json:"owner_agent_id"`
		Policy       *agenthub.GroupPolicy `json:"policy"`
		Pinned       *bool                 `json:"pinned"`
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
		next.Policy = *patch.Policy
	}
	if patch.Pinned != nil {
		next.Pinned = *patch.Pinned
	}
	if patch.OwnerAgentID != nil && *patch.OwnerAgentID != "" && *patch.OwnerAgentID != g.OwnerAgentID {
		next.OwnerAgentID = *patch.OwnerAgentID
		// owner 转让是成对改写：旧 owner 降 member、新 owner 升 owner。
		found := false
		for i := range next.Members {
			if next.Members[i].AgentID == g.OwnerAgentID {
				next.Members[i].Role = agenthub.RoleMember
			}
			if next.Members[i].AgentID == next.OwnerAgentID {
				next.Members[i].Role = agenthub.RoleOwner
				found = true
			}
		}
		if !found {
			m.resp(c, false, "新 owner 必须是群成员", nil)
			return
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
	if err := m.validateGroupAgents(ctx, &next); err != nil {
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
	m.resp(c, true, "", map[string]any{"group": &next})
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
	g.Deleted = true
	g.UpdatedAt = time.Now().UTC()
	if err := st.SaveGroup(ctx, g); err != nil {
		m.resp(c, false, "删除群组失败: "+err.Error(), nil)
		return
	}
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
	if err := m.validateGroupAgents(ctx, &next); err != nil {
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
	m.resp(c, true, "", map[string]any{"group": &next})
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
	m.resp(c, true, "", map[string]any{"group": &next})
}

// validateGroupAgents 成员必须是非删除的 Hub 智能体，且显示名不重复（§5.9）。
func (m *manager) validateGroupAgents(ctx context.Context, g *agenthub.Group) error {
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
	return agenthub.ValidateGroupMemberNames(g, profiles)
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

// hubGroupHistory 群日志分页（before_seq=0 最新页）。
func (m *manager) hubGroupHistory(c command) {
	st, err := m.hubStore()
	if err != nil {
		m.resp(c, false, err.Error(), nil)
		return
	}
	gid := agenthub.GroupID(strings.TrimSpace(str(c.Payload, "group_id")))
	msgs, err := agenthub.ReadGroupLog(context.Background(), st, gid, numOr(c.Payload, "before_seq", 0), int(numOr(c.Payload, "limit", 50)))
	if err != nil {
		m.resp(c, false, "读取群日志失败: "+err.Error(), nil)
		return
	}
	m.resp(c, true, "", map[string]any{"messages": msgs, "has_more": len(msgs) >= int(numOr(c.Payload, "limit", 50))})
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
