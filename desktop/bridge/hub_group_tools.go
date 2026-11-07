package main

// hub_group_tools.go：Hub 智能体的群工具（P3a-05）。
//
// group_history / group_members / group_say 都显式带 group_id（P3a 还没有群会话，
// 模型从系统提示词/人给的任务里知道群 id）。权限：
//   - 必须是群成员（非成员一律拒绝，不泄漏群存在性以外的信息）；
//   - group_history 走 Redacted(false)：owner-only 消息只显示占位；
//   - observer 可以读，不能 group_say；
//   - group_say 与人类一样走严格 @ 路由（@all 只有 owner 可以）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/tools"
)

type hubGroupToolBase struct {
	tools.BaseTool
	m       *manager
	agentID agenthub.AgentID
}

func registerHubGroupTools(eng *tools.Engine, m *manager, agentID agenthub.AgentID) {
	if eng == nil || m == nil || agentID == "" {
		return
	}
	for _, tl := range []tools.Tool{
		newHubGroupHistoryTool(m, agentID),
		newHubGroupMembersTool(m, agentID),
		newHubGroupSayTool(m, agentID),
	} {
		eng.RegisterTool(context.Background(), tl)
	}
	registerHubHandoffTools(eng, m, agentID)
}

// groupAccess 读群 + 校验成员；返回成员角色。
func (t *hubGroupToolBase) groupAccess(ctx context.Context, groupID string) (*agenthub.FileStore, *agenthub.Group, agenthub.MemberRole, error) {
	st, err := t.m.hubStore()
	if err != nil {
		return nil, nil, "", err
	}
	gid := agenthub.GroupID(strings.TrimSpace(groupID))
	if gid == "" {
		return nil, nil, "", fmt.Errorf("%w: group_id 不能为空", agenthub.ErrInvalid)
	}
	g, err := st.LoadGroup(ctx, gid)
	if err != nil {
		return nil, nil, "", err
	}
	if g.Deleted {
		return nil, nil, "", fmt.Errorf("%w: 群组已删除", agenthub.ErrNotFound)
	}
	for _, member := range g.Members {
		if member.AgentID == t.agentID {
			return st, g, member.Role, nil
		}
	}
	return nil, nil, "", fmt.Errorf("%w: 智能体不是该群成员", agenthub.ErrInvalid)
}

// —— group_history ——

type hubGroupHistoryTool struct{ hubGroupToolBase }

func newHubGroupHistoryTool(m *manager, agentID agenthub.AgentID) *hubGroupHistoryTool {
	return &hubGroupHistoryTool{hubGroupToolBase{BaseTool: tools.BaseTool{
		Name_: "group_history",
		Description_: "Read a Hub group's message history (paginated, newest first). " +
			"Only members can read; owner-only messages are redacted. " +
			"Use query to search text, before_seq to page older messages.",
		Params_: tools.Obj(map[string]any{
			"group_id":   tools.Str("Group id from the task/system context"),
			"query":      tools.Str("Optional text search"),
			"before_seq": tools.Int("Return messages older than this seq (0 = latest page)"),
			"limit":      tools.Int("Page size 1-100 (default 30)"),
		}, "group_id"),
		CanParallel_: true, ReadOnly_: true,
	}, m: m, agentID: agentID}}
}

type hubGroupHistoryArgs struct {
	GroupID   string `json:"group_id"`
	Query     string `json:"query"`
	BeforeSeq int64  `json:"before_seq"`
	Limit     int    `json:"limit"`
}

func (t *hubGroupHistoryTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a hubGroupHistoryArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	st, _, _, err := t.groupAccess(ctx, a.GroupID)
	if err != nil {
		return "", err
	}
	limit := a.Limit
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	var msgs []agenthub.Message
	if strings.TrimSpace(a.Query) != "" {
		msgs, err = st.SearchMessages(ctx, agenthub.GroupID(a.GroupID), a.Query, a.BeforeSeq, limit)
	} else {
		msgs, err = agenthub.ReadGroupLog(ctx, st, agenthub.GroupID(a.GroupID), a.BeforeSeq, limit)
	}
	if err != nil {
		return "", err
	}
	out := make([]agenthub.Message, 0, len(msgs))
	for _, m := range msgs {
		if !m.AgentContextKind() {
			continue // 审批占位/回执不进 agent 的工具视图（P3a-07 B3）
		}
		out = append(out, m.Redacted(false))
	}
	return marshalHubToolResult(map[string]any{"messages": out, "count": len(out)})
}

// —— group_members ——

type hubGroupMembersTool struct{ hubGroupToolBase }

func newHubGroupMembersTool(m *manager, agentID agenthub.AgentID) *hubGroupMembersTool {
	return &hubGroupMembersTool{hubGroupToolBase{BaseTool: tools.BaseTool{
		Name_: "group_members",
		Description_: "List a Hub group's members with roles and whether their profile still exists. " +
			"Only members can read.",
		Params_:      tools.Obj(map[string]any{"group_id": tools.Str("Group id")}, "group_id"),
		CanParallel_: true, ReadOnly_: true,
	}, m: m, agentID: agentID}}
}

func (t *hubGroupMembersTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a hubGroupHistoryArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	st, g, _, err := t.groupAccess(ctx, a.GroupID)
	if err != nil {
		return "", err
	}
	agents, err := st.ListAgents(ctx)
	if err != nil {
		return "", err
	}
	byID := map[agenthub.AgentID]*agenthub.AgentProfile{}
	for _, p := range agents {
		if p != nil {
			byID[p.ID] = p
		}
	}
	type row struct {
		AgentID string `json:"agent_id"`
		Name    string `json:"name,omitempty"`
		Role    string `json:"role"`
		Deleted bool   `json:"deleted,omitempty"`
	}
	rows := make([]row, 0, len(g.Members))
	for _, member := range g.Members {
		r := row{AgentID: string(member.AgentID), Role: string(member.Role)}
		if p := byID[member.AgentID]; p != nil {
			r.Name, r.Deleted = p.Name, p.Deleted
		}
		rows = append(rows, r)
	}
	return marshalHubToolResult(map[string]any{"group_id": string(g.ID), "name": g.Name, "mode": g.Policy.EffectiveMode(), "members": rows})
}

// —— group_say ——

type hubGroupSayTool struct{ hubGroupToolBase }

func newHubGroupSayTool(m *manager, agentID agenthub.AgentID) *hubGroupSayTool {
	return &hubGroupSayTool{hubGroupToolBase{BaseTool: tools.BaseTool{
		Name_: "group_say",
		Description_: "Say something in a Hub group (appends to the group log and routes @mentions). " +
			"Observers cannot speak. @all is only allowed for the group owner; mention at most the policy limit.",
		Params_: tools.Obj(map[string]any{
			"group_id": tools.Str("Group id"),
			"text":     tools.Str("Message text (use @Name to wake members)"),
		}, "group_id", "text"),
		CanParallel_: false, ReadOnly_: false,
	}, m: m, agentID: agentID}}
}

func (t *hubGroupSayTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a hubGroupHistoryArgs
	var args struct {
		GroupID string `json:"group_id"`
		Text    string `json:"text"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", err
	}
	a.GroupID = args.GroupID
	st, g, role, err := t.groupAccess(ctx, args.GroupID)
	if err != nil {
		return "", err
	}
	if role == agenthub.RoleObserver {
		return "", fmt.Errorf("%w: observer 不能在群里发言", agenthub.ErrInvalid)
	}
	text := strings.TrimSpace(args.Text)
	if text == "" {
		return "", fmt.Errorf("%w: text 不能为空", agenthub.ErrInvalid)
	}
	// 与人类发送 / final 共用一个临界区：限流检查到消息+回执落盘不能被并发工具调用插队。
	t.m.hubGroupSendMu.Lock()
	g, err = st.LoadGroup(ctx, g.ID)
	if err != nil {
		t.m.hubGroupSendMu.Unlock()
		return "", err
	}
	if g == nil || g.Deleted {
		t.m.hubGroupSendMu.Unlock()
		return "", fmt.Errorf("%w: 群组已删除", agenthub.ErrNotFound)
	}
	msg, routed, limitHit, routeErr, err := t.m.appendAgentGroupMessageLocked(ctx, st, g, t.agentID, text, "", true)
	t.m.hubGroupSendMu.Unlock()
	if limitHit {
		return "", fmt.Errorf("%w: 本轮智能体发言已达上限 %d，等人类回复后再继续", agenthub.ErrInvalid, groupTurnLimit(g))
	}
	if err != nil {
		return "", err
	}
	if routeErr != nil {
		return "", routeErr
	}
	result := map[string]any{"seq": msg.Seq, "message_id": msg.ID, "targets": []agenthub.AgentID{}}
	if routed != nil {
		result["targets"] = routed.Targets
		// wake 在解锁后做：会话装配可能很慢，不能把群发送锁带进装配。
		if errs := t.m.wakeGroupRound(g, *routed, msg); len(errs) > 0 {
			result["wake_errors"] = errs
			for _, we := range errs {
				t.m.hubAlertf("group_wake_failed", "群 %s 唤醒失败：%s", g.ID, we)
			}
		}
	}
	return marshalHubToolResult(result)
}

// ValidParams 只做「JSON 可解析 + group_id 非空」；成员权限在 Call 里判。
func (t *hubGroupHistoryTool) ValidParams(_ context.Context, _, arguments string) error {
	var a hubGroupHistoryArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	if strings.TrimSpace(a.GroupID) == "" {
		return fmt.Errorf("%w: group_id 不能为空", agenthub.ErrInvalid)
	}
	return nil
}

func (t *hubGroupMembersTool) ValidParams(_ context.Context, _, arguments string) error {
	var a hubGroupHistoryArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	if strings.TrimSpace(a.GroupID) == "" {
		return fmt.Errorf("%w: group_id 不能为空", agenthub.ErrInvalid)
	}
	return nil
}

func (t *hubGroupSayTool) ValidParams(_ context.Context, _, arguments string) error {
	var a struct {
		GroupID string `json:"group_id"`
		Text    string `json:"text"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	if strings.TrimSpace(a.GroupID) == "" || strings.TrimSpace(a.Text) == "" {
		return fmt.Errorf("%w: group_id/text 不能为空", agenthub.ErrInvalid)
	}
	return nil
}
