package main

// hub_group_tools.go：Hub 智能体的群工具（P3a-05）。
//
// group_history / group_members / group_say 都带 group_id。**群作用域会话把群钉死**：
// 装配时绑定 spec.Scope.GroupID，参数缺省用绑定群、给了别的群直接拒绝 —— 与
// assign_task/task_ack 同口径。否则同一智能体在多个群时，A 群会话能借参数读写 B 群
// （跨群越权）。缺省能力必须同时落在两处：schema 的 required（模型/编排层看到的
// 工具签名）与 ValidParams（引擎在 Call 之前的必填校验）—— 只改一处的后果是模型
// 以为必填，或引擎先拦下省略 group_id 的合法调用。权限：
//   - 必须是群成员（非成员一律拒绝，不泄漏群存在性以外的信息）；
//   - group_history 走 Redacted(false)：owner-only 消息只显示占位；
//   - 群成员（owner/member）都能读、都能 group_say（P6-12 删除了 observer 角色：
//     「只读」是群策略 readonly 的事，不是角色的事）；
//   - group_say 与人类一样走严格 @ 路由（@all 只有 owner 可以）。

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/tools"
)

type hubGroupToolBase struct {
	tools.BaseTool
	m       *manager
	agentID agenthub.AgentID
	// groupID 是装配时绑定的群（群作用域会话 = spec.Scope.GroupID）。
	// 空 = 未绑定（直造工具的老路径/测试），此时沿用「按参数」的旧行为。
	groupID agenthub.GroupID
}

func registerHubGroupTools(eng *tools.Engine, m *manager, agentID agenthub.AgentID, groupID agenthub.GroupID) {
	if eng == nil || m == nil || agentID == "" {
		return
	}
	for _, tl := range []tools.Tool{
		newHubGroupHistoryTool(m, agentID, groupID),
		newHubGroupMembersTool(m, agentID, groupID),
		newHubAgentStatusTool(m, agentID, groupID),
		newHubGroupSayTool(m, agentID, groupID),
	} {
		eng.RegisterTool(context.Background(), tl)
	}
	registerHubHandoffTools(eng, m, agentID, groupID)
}

// pinnedGroupID 解析工具参数里的 group_id，并把群作用域钉在装配时绑定的群上：
//   - 已绑定（群作用域会话）：缺省用绑定群，给了别的群直接拒绝；
//   - 未绑定（测试/老装配路径）：沿用按参数的行为（空参数仍报错）。
func (t *hubGroupToolBase) pinnedGroupID(argsGroupID string) (agenthub.GroupID, error) {
	bound := agenthub.GroupID(strings.TrimSpace(string(t.groupID)))
	gid := agenthub.GroupID(strings.TrimSpace(argsGroupID))
	if bound == "" {
		if gid == "" {
			return "", fmt.Errorf("%w: group_id 不能为空", agenthub.ErrInvalid)
		}
		return gid, nil
	}
	if gid == "" {
		return bound, nil
	}
	if gid != bound {
		// 不回显别的群的任何信息（只报本群与参数），越权探测没有额外收益。
		return "", fmt.Errorf("%w: 群作用域会话只能操作本群（本群 %s，参数 %s）", agenthub.ErrInvalid, bound, gid)
	}
	return bound, nil
}

// hubGroupIDParam 是 group_id 参数的 schema 描述：绑定群的会话（群作用域）里参数可
// 省略 —— 缺省就是本群；未绑定（直造工具的测试/老装配路径）仍要模型自己给。
func hubGroupIDParam(bound agenthub.GroupID, base string) map[string]any {
	if strings.TrimSpace(string(bound)) != "" {
		return tools.Str(base + " (defaults to this session's group)")
	}
	return tools.Str(base)
}

// hubGroupRequired 过滤 schema 的 required：绑定群的会话里 group_id 可省略（由
// pinnedGroupID 补上本群），未绑定时保持必填。required 若继续硬编码 "group_id"，
// tools/engine.go 会在 Call 之前的必填校验（ValidParams + missingFieldsHint）就把
// 「省略 group_id」的合法调用拒掉 —— 所以必需项必须跟着装配时的绑定一起变。
func hubGroupRequired(bound agenthub.GroupID, required ...string) []string {
	if strings.TrimSpace(string(bound)) == "" {
		return required
	}
	out := make([]string, 0, len(required))
	for _, name := range required {
		if name != "group_id" {
			out = append(out, name)
		}
	}
	return out
}

// groupAccess 先钉死群作用域、再读群 + 校验成员；返回成员角色。
func (t *hubGroupToolBase) groupAccess(ctx context.Context, groupID string) (*agenthub.FileStore, *agenthub.Group, agenthub.MemberRole, error) {
	gid, err := t.pinnedGroupID(groupID)
	if err != nil {
		return nil, nil, "", err
	}
	st, err := t.m.hubStore()
	if err != nil {
		return nil, nil, "", err
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

func newHubGroupHistoryTool(m *manager, agentID agenthub.AgentID, groupID agenthub.GroupID) *hubGroupHistoryTool {
	return &hubGroupHistoryTool{hubGroupToolBase{BaseTool: tools.BaseTool{
		Name_: "group_history",
		Description_: "Read a Hub group's message history (paginated, newest first). " +
			"Only members can read; owner-only messages are redacted. " +
			"Use query to search text, before_seq to page older messages.",
		Params_: tools.Obj(map[string]any{
			"group_id":   hubGroupIDParam(groupID, "Group id from the task/system context"),
			"query":      tools.Str("Optional text search"),
			"before_seq": tools.Int("Return messages older than this seq (0 = latest page)"),
			"limit":      tools.Int("Page size 1-100 (default 30)"),
		}, hubGroupRequired(groupID, "group_id")...),
		CanParallel_: true, ReadOnly_: true,
	}, m: m, agentID: agentID, groupID: groupID}}
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
	// 读用 groupAccess 解析后的群：参数缺省时 a.GroupID 是空串，不能直接拿去读。
	st, g, _, err := t.groupAccess(ctx, a.GroupID)
	if err != nil {
		return "", err
	}
	limit := a.Limit
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	var msgs []agenthub.Message
	if strings.TrimSpace(a.Query) != "" {
		msgs, err = st.SearchMessages(ctx, g.ID, a.Query, a.BeforeSeq, limit)
	} else {
		msgs, err = agenthub.ReadGroupLog(ctx, st, g.ID, a.BeforeSeq, limit)
	}
	if err != nil {
		return "", err
	}
	out := make([]agenthub.Message, 0, len(msgs))
	for _, m := range msgs {
		if !m.AgentContextKind() {
			continue // 审批占位/回执不进 agent 的工具视图（P3a-07 B3）
		}
		out = append(out, m.AgentToolView())
	}
	return marshalHubToolResult(map[string]any{"messages": out, "count": len(out)})
}

// —— group_members / agent_status ——
//
// 名册（谁是谁、职责/能力）在 system prompt L2 里；**实时状态**只在这里给：
// idle/running + 忙在哪个作用域。状态不落盘、不进 system prompt，每次调用现算。

// hubMemberRow 是 group_members()/agent_status() 的共同行：稳定身份 + 实时状态。
// 已有字段（agent_id/name/role/deleted）保持不变，新增 status/where/missing 为增量。
type hubMemberRow struct {
	AgentID     string `json:"agent_id"`
	Name        string `json:"name,omitempty"`
	Role        string `json:"role,omitempty"` // 群内角色：owner/member/observer
	Description string `json:"description,omitempty"`
	Status      string `json:"status"`          // idle | running
	Where       string `json:"where,omitempty"` // running 时：group（本群/未指定群作用域）/ dm / other_group
	Deleted     bool   `json:"deleted,omitempty"`
	Missing     bool   `json:"missing,omitempty"` // 档案缺失（软删/purge）：名册里是显式 fallback
}

// effectiveGroupRole 群内**有效**角色：owner 以 Group.OwnerAgentID 为准（转让 owner 时
// 老数据可能只改了 owner 字段没改 Member.Role），其次 Member.Role，最后缺省 member。
// 名册行与 agent_status 的 your_role 都走这里，两处不能各判一套（P3b-01b N3）。
func effectiveGroupRole(g *agenthub.Group, member agenthub.Member) agenthub.MemberRole {
	if g != nil && member.AgentID != "" && member.AgentID == g.OwnerAgentID {
		return agenthub.RoleOwner
	}
	if member.Role != "" {
		return member.Role
	}
	return agenthub.RoleMember
}

func (m *manager) hubMemberRowOf(g *agenthub.Group, member agenthub.Member, p *agenthub.AgentProfile) hubMemberRow {
	role := string(effectiveGroupRole(g, member))
	var groupID agenthub.GroupID
	if g != nil {
		groupID = g.ID
	}
	row := hubMemberRow{AgentID: string(member.AgentID), Role: role}
	if p != nil {
		row.Name, row.Description, row.Deleted = p.Name, p.Description, p.Deleted
	} else {
		row.Missing = true
	}
	// 状态来自真实运行态（会话忙闲），不是 profile 里的静态字段。
	row.Status, row.Where = m.hubAgentLiveStatus(string(member.AgentID), groupID)
	return row
}

func groupMemberOf(g *agenthub.Group, id agenthub.AgentID) (agenthub.Member, bool) {
	if g == nil {
		return agenthub.Member{}, false
	}
	for _, member := range g.Members {
		if member.AgentID == id {
			return member, true
		}
	}
	return agenthub.Member{}, false
}

type hubGroupMembersTool struct{ hubGroupToolBase }

func newHubGroupMembersTool(m *manager, agentID agenthub.AgentID, groupID agenthub.GroupID) *hubGroupMembersTool {
	return &hubGroupMembersTool{hubGroupToolBase{BaseTool: tools.BaseTool{
		Name_: "group_members",
		Description_: "List a Hub group's members: role (owner/member/observer) and live status " +
			"(idle/running, plus where they are busy). Roles/responsibilities are also in your system " +
			"prompt; the status here is realtime. Missing profiles are reported explicitly. " +
			"Only members can read.",
		Params_:      tools.Obj(map[string]any{"group_id": hubGroupIDParam(groupID, "Group id")}, hubGroupRequired(groupID, "group_id")...),
		CanParallel_: true, ReadOnly_: true,
	}, m: m, agentID: agentID, groupID: groupID}}
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
	rows := make([]hubMemberRow, 0, len(g.Members))
	for _, member := range g.Members {
		rows = append(rows, t.m.hubMemberRowOf(g, member, byID[member.AgentID]))
	}
	return marshalHubToolResult(map[string]any{"group_id": string(g.ID), "name": g.Name, "mode": g.Policy.EffectiveMode(), "members": rows})
}

// —— agent_status ——

type hubAgentStatusTool struct{ hubGroupToolBase }

func newHubAgentStatusTool(m *manager, agentID agenthub.AgentID, groupID agenthub.GroupID) *hubAgentStatusTool {
	return &hubAgentStatusTool{hubGroupToolBase{BaseTool: tools.BaseTool{
		Name_: "agent_status",
		Description_: "Live status (idle/running) for you or a colleague. Pass agent_id to ask about " +
			"someone else and group_id to check their role/status in that group (membership is " +
			"verified: you can only see yourself or agents you share a group with). Status is " +
			"realtime and never included in the system prompt.",
		Params_: tools.Obj(map[string]any{
			"group_id": tools.Str("Optional group id: verify membership and return the role in that group"),
			"agent_id": tools.Str("Target agent id (default: you)"),
		}),
		CanParallel_: true, ReadOnly_: true,
	}, m: m, agentID: agentID, groupID: groupID}}
}

type hubAgentStatusArgs struct {
	GroupID string `json:"group_id"`
	AgentID string `json:"agent_id"`
}

func (t *hubAgentStatusTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var a hubAgentStatusArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return "", err
	}
	target := agenthub.AgentID(strings.TrimSpace(a.AgentID))
	if target == "" {
		target = t.agentID // 缺省查自己
	}
	st, err := t.m.hubStore()
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
	if gid := strings.TrimSpace(a.GroupID); gid != "" {
		// 群作用域：调用者必须是成员（groupAccess），目标也必须是成员。
		// 目标不是成员时明确拒绝，不泄露「这个 id 是否存在」。
		_, g, _, err := t.groupAccess(ctx, gid)
		if err != nil {
			return "", err
		}
		member, ok := groupMemberOf(g, target)
		if !ok {
			return "", fmt.Errorf("%w: %s 不是该群成员，无法查看状态", agenthub.ErrInvalid, target)
		}
		// your_role 用**有效**角色（owner 转让后的老数据里 Member.Role 可能还是 member），
		// 与 hubMemberRowOf 输出的角色行必须一致（P3b-01b N3）。
		caller, ok := groupMemberOf(g, t.agentID)
		if !ok { // groupAccess 已校验过；防御性兜底，不给一个编造的角色
			return "", fmt.Errorf("%w: 智能体不是该群成员", agenthub.ErrInvalid)
		}
		return marshalHubToolResult(map[string]any{
			"group_id":  string(g.ID),
			"your_role": string(effectiveGroupRole(g, caller)),
			"agent":     t.m.hubMemberRowOf(g, member, byID[target]),
		})
	}
	// 无群作用域：查自己随便查；查别人必须至少共享一个群（不看群就不知道谁能看谁）。
	if target != t.agentID {
		groups, err := st.ListGroups(ctx)
		if err != nil {
			return "", err
		}
		shared := false
		for _, g := range groups {
			if g == nil || g.Deleted {
				continue
			}
			if groupHasMember(g, t.agentID) && groupHasMember(g, target) {
				shared = true
				break
			}
		}
		if !shared {
			return "", fmt.Errorf("%w: 只能查看自己或同群成员", agenthub.ErrInvalid)
		}
	}
	row := hubMemberRow{AgentID: string(target), Role: ""}
	if p := byID[target]; p != nil {
		row.Name, row.Description, row.Deleted = p.Name, p.Description, p.Deleted
	} else {
		row.Missing = true
	}
	row.Status, row.Where = t.m.hubAgentLiveStatus(string(target), "")
	return marshalHubToolResult(map[string]any{"agent": row})
}

// ValidParams 只做 JSON 可解析；成员/共享群权限在 Call 里判（要读存储）。
func (t *hubAgentStatusTool) ValidParams(_ context.Context, _, arguments string) error {
	var a hubAgentStatusArgs
	return json.Unmarshal([]byte(arguments), &a)
}

// —— group_say ——

type hubGroupSayTool struct{ hubGroupToolBase }

func newHubGroupSayTool(m *manager, agentID agenthub.AgentID, groupID agenthub.GroupID) *hubGroupSayTool {
	return &hubGroupSayTool{hubGroupToolBase{BaseTool: tools.BaseTool{
		Name_: "group_say",
		Description_: "Say something in a Hub group (appends to the group log and routes @mentions). " +
			"@Name in text and the structured mentions list are unioned, deduped and capped by the group policy. " +
			"mention_all is only allowed for the group owner (humans sending from the app are always allowed).",
		Params_: tools.Obj(map[string]any{
			"group_id":    hubGroupIDParam(groupID, "Group id"),
			"text":        tools.Str("Message text (use @Name to wake members)"),
			"mentions":    tools.ArrayStr("Optional agent ids to wake explicitly; unioned with @Name in text"),
			"mention_all": tools.Bool("Wake every group member (owner agents only; a rejected mention_all fails the call)"),
		}, hubGroupRequired(groupID, "group_id", "text")...),
		CanParallel_: false, ReadOnly_: false,
	}, m: m, agentID: agentID, groupID: groupID}}
}

func (t *hubGroupSayTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var args struct {
		GroupID    string   `json:"group_id"`
		Text       string   `json:"text"`
		Mentions   []string `json:"mentions"`
		MentionAll bool     `json:"mention_all"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", err
	}
	st, g, _, err := t.groupAccess(ctx, args.GroupID)
	if err != nil {
		return "", err
	}
	text := strings.TrimSpace(args.Text)
	if text == "" {
		return "", fmt.Errorf("%w: text 不能为空", agenthub.ErrInvalid)
	}
	mentions := make([]agenthub.AgentID, 0, len(args.Mentions))
	for _, id := range args.Mentions {
		mentions = append(mentions, agenthub.AgentID(strings.TrimSpace(id)))
	}
	// 与人类发送 / final 共用一个临界区：路由到消息+回执落盘不能被并发工具调用插队。
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
	msg, routed, routeErr, err := t.m.appendAgentGroupMessageLocked(ctx, st, g, t.agentID, agentGroupWrite{
		Text: text, Mentions: mentions, MentionAll: args.MentionAll, RouteFatal: true,
	})
	t.m.hubGroupSendMu.Unlock()
	if err != nil {
		return "", err
	}
	if routeErr != nil {
		// 结构化 id 非法/非成员、越权 @all：整条消息不落盘（routeFatal）。
		return "", routeErr
	}
	result := map[string]any{"seq": msg.Seq, "message_id": msg.ID, "targets": []agenthub.AgentID{}}
	if routed != nil {
		result["targets"] = routed.Targets
		result["mentions"] = routed.Mentions
		result["mention_all"] = routed.MentionAll
		if routed.Dropped > 0 {
			result["dropped"] = routed.Dropped
		}
		// 智能体之间的 @ 走对话控制器（P3b-04）：预算/深度/踢皮球/同内容护栏 +
		// 串行队列。首条立即尝试（新对话的第一条永远立即可跑）；后续条等本轮 run
		// 结束、冷却到期后由泵放行。
		depth := t.m.hubGroupNextDepth(string(t.agentID), g)
		wakes, oerr := t.m.offerGroupWakes(g, *routed, msg, t.agentID, depth)
		if oerr != nil {
			if errors.Is(oerr, agenthub.ErrConversationExhausted) {
				// 护栏命中：控制器已把状态推到 exhausted，系统消息/事件已由
				// notifyHubConversation 写一条。把视图交给模型，它能解释「为什么没人被叫醒」。
				result["conversation"] = t.m.hubConversationView(g.ID)
			} else {
				t.m.hubAlertf("group_conversation_offer_failed", "群 %s 对话入队失败：%v", g.ID, oerr)
				result["wake_errors"] = []string{oerr.Error()}
			}
		} else if len(wakes) > 0 {
			t.m.pumpHubConversation(g.ID)
		}
	}
	return marshalHubToolResult(result)
}

// ValidParams 只做「JSON 可解析 + group_id 合法」：绑定群的会话里 group_id 可省略
// （pinnedGroupID 补本群），显式传别的群在这里就拒；成员权限在 Call 里判。
func (t *hubGroupHistoryTool) ValidParams(_ context.Context, _, arguments string) error {
	var a hubGroupHistoryArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	_, err := t.pinnedGroupID(a.GroupID)
	return err
}

func (t *hubGroupMembersTool) ValidParams(_ context.Context, _, arguments string) error {
	var a hubGroupHistoryArgs
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	_, err := t.pinnedGroupID(a.GroupID)
	return err
}

func (t *hubGroupSayTool) ValidParams(_ context.Context, _, arguments string) error {
	var a struct {
		GroupID    string   `json:"group_id"`
		Text       string   `json:"text"`
		Mentions   []string `json:"mentions"`    // 只为校验数组元素类型；目标校验在 Router
		MentionAll bool     `json:"mention_all"` // 同上：鉴权在 Call/Router
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	if _, err := t.pinnedGroupID(a.GroupID); err != nil {
		return err
	}
	if strings.TrimSpace(a.Text) == "" {
		return fmt.Errorf("%w: text 不能为空", agenthub.ErrInvalid)
	}
	return nil
}
