package main

// hub_handoff.go：群内交接卡、落盘与 ack（P3a-06，§6.8/§6.9）。
//
// handoff 工具：
//   - 从 g.Members 解析双方角色（不信任调用方），拒非成员/observer/软删群；
//   - 链深度由历史推导（0 = 首跳），同无序对 10 分钟最多 2 次；
//   - 写 handoffs/<id>.json + KindHandoff 卡片，并开一条 handoff Needs you。
//
// handoff_ack 工具：只有接收方能 ack；pending_ack → accepted/declined 幂等，
// 更新卡片状态并把对应 Need 置 resolved。

import (
	"context"
	"crypto/sha1"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/tools"
)

func registerHubHandoffTools(eng *tools.Engine, m *manager, agentID agenthub.AgentID) {
	if eng == nil || m == nil || agentID == "" {
		return
	}
	for _, tl := range []tools.Tool{
		newHubHandoffTool(m, agentID),
		newHubHandoffAckTool(m, agentID),
	} {
		eng.RegisterTool(context.Background(), tl)
	}
}

// —— handoff ——

type hubHandoffTool struct{ hubGroupToolBase }

func newHubHandoffTool(m *manager, agentID agenthub.AgentID) *hubHandoffTool {
	return &hubHandoffTool{hubGroupToolBase{BaseTool: tools.BaseTool{
		Name_: "handoff",
		Description_: "Transfer ownership of work to another group member (not the same as @mention). " +
			"The recipient must acknowledge; observers cannot hand off. " +
			"Guard: chain depth 3 and at most 2 handoffs per pair in 10 minutes.",
		Params_: tools.Obj(map[string]any{
			"group_id":     tools.Str("Group id"),
			"to_agent_id":  tools.Str("Recipient agent id (must be a group member)"),
			"instruction":  tools.Str("What ownership is being transferred, with enough context"),
			"artifact_ids": tools.ArrayStr("Optional artifact id"),
		}, "group_id", "to_agent_id", "instruction"),
		CanParallel_: false, ReadOnly_: false,
	}, m: m, agentID: agentID}}
}

func (t *hubHandoffTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var args struct {
		GroupID     string   `json:"group_id"`
		ToAgentID   string   `json:"to_agent_id"`
		Instruction string   `json:"instruction"`
		ArtifactIDs []string `json:"artifact_ids"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", err
	}
	st, g, role, err := t.groupAccess(ctx, args.GroupID)
	if err != nil {
		return "", err
	}
	if role == agenthub.RoleObserver {
		return "", fmt.Errorf("%w: observer 不能发起交接", agenthub.ErrHandoffObserver)
	}
	instruction := strings.TrimSpace(args.Instruction)
	if instruction == "" {
		return "", fmt.Errorf("%w: instruction 不能为空", agenthub.ErrInvalid)
	}
	to := agenthub.AgentID(strings.TrimSpace(args.ToAgentID))
	if to == "" {
		return "", fmt.Errorf("%w: to_agent_id 不能为空", agenthub.ErrInvalid)
	}
	profile, err := st.LoadAgent(ctx, to)
	if err != nil {
		return "", fmt.Errorf("%w: 接收方不存在", agenthub.ErrInvalid)
	}
	if profile.Deleted {
		return "", fmt.Errorf("%w: 接收方已删除", agenthub.ErrInvalid)
	}

	// 护栏检查与落盘必须在同一临界区：并发 handoff 不能同时绕过同对频率上限。
	t.m.hubGroupSendMu.Lock()
	defer t.m.hubGroupSendMu.Unlock()

	history, err := st.ListHandoffs(ctx, g.ID)
	if err != nil {
		return "", err
	}
	flat := make([]agenthub.Handoff, 0, len(history))
	for _, h := range history {
		if h != nil {
			flat = append(flat, *h)
		}
	}
	now := time.Now().UTC()
	depth := agenthub.NextHandoffDepth(flat, t.agentID)
	if err := agenthub.CheckHandoff(g, t.agentID, to, depth, flat, now); err != nil {
		return "", err
	}
	id, err := st.AllocateID(ctx, agenthub.PrefixHandoff)
	if err != nil {
		return "", err
	}
	h := &agenthub.Handoff{
		ID: id, GroupID: g.ID, FromAgentID: t.agentID, ToAgentID: to,
		Instruction: instruction, ArtifactIDs: args.ArtifactIDs,
		Status: agenthub.HandoffPendingAck, ChainDepth: depth, CreatedAt: now,
	}
	if err := st.SaveHandoff(ctx, h); err != nil {
		return "", err
	}
	// 交接卡：正文只放摘要；完整指令进 handoff 文件与 Needs you 的私有正文。
	fromName := agentDisplayName(ctx, st, t.agentID)
	toName := profile.Name
	if strings.TrimSpace(toName) == "" {
		toName = string(to)
	}
	card := &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindHandoff, Intent: agenthub.IntentHandoff,
		Sender: agenthub.Sender{Kind: agenthub.SenderAgent, AgentID: t.agentID},
		Text:   fmt.Sprintf("%s → %s 交接：%s", fromName, toName, truncateRunes(instruction, 240)),
		Refs: map[string]any{
			"handoff_id": h.ID, "status": string(h.Status),
			"from_agent_id": string(t.agentID), "to_agent_id": string(to),
			"artifact_ids": h.ArtifactIDs,
		},
	}
	seq, _, err := agenthub.AppendGroupMessageRouted(ctx, st, card, nil)
	if err != nil {
		return "", err
	}
	need := handoffNeed(h, toName)
	if err := st.SaveNeed(ctx, need); err != nil {
		return "", err
	}
	t.m.emitHubGroupMessage(g.ID, seq, card)
	t.m.out.writeLine(map[string]any{
		"event_type": "hub_handoff", "group_id": string(g.ID), "handoff": h,
		"message_seq": seq, "need": need,
	})
	t.m.emitHubNeed(need)
	return marshalHubToolResult(map[string]any{
		"handoff": h, "seq": seq, "need_id": need.ID, "status": h.Status,
	})
}

// —— handoff_ack ——

type hubHandoffAckTool struct{ hubGroupToolBase }

func newHubHandoffAckTool(m *manager, agentID agenthub.AgentID) *hubHandoffAckTool {
	return &hubHandoffAckTool{hubGroupToolBase{BaseTool: tools.BaseTool{
		Name_: "handoff_ack",
		Description_: "Accept or decline a handoff addressed to you. " +
			"Only the recipient can acknowledge; repeated decisions are idempotent.",
		Params_: tools.Obj(map[string]any{
			"handoff_id": tools.Str("Handoff id from the handoff card"),
			"accept":     tools.Bool("true = accept ownership, false = decline"),
			"reason":     tools.Str("Optional reason when declining"),
		}, "handoff_id", "accept"),
		CanParallel_: false, ReadOnly_: false,
	}, m: m, agentID: agentID}}
}

func (t *hubHandoffAckTool) Call(ctx context.Context, _, arguments string) (string, error) {
	var args struct {
		HandoffID string `json:"handoff_id"`
		Accept    bool   `json:"accept"`
		Reason    string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", err
	}
	h, err := t.m.ackHubHandoff(ctx, strings.TrimSpace(args.HandoffID), args.Accept, args.Reason, t.agentID, false)
	if err != nil {
		return "", err
	}
	return marshalHubToolResult(map[string]any{"handoff": h, "status": h.Status})
}

// ackHubHandoff 是 agent 工具与人类 Needs you 共用的决策入口。
// actor 非空时必须是接收方；human=true 时允许群主/人代决策。
func (m *manager) ackHubHandoff(ctx context.Context, handoffID string, accept bool, reason string, actor agenthub.AgentID, human bool) (*agenthub.Handoff, error) {
	if strings.TrimSpace(handoffID) == "" {
		return nil, fmt.Errorf("%w: handoff_id 不能为空", agenthub.ErrInvalid)
	}
	st, err := m.hubStore()
	if err != nil {
		return nil, err
	}
	m.hubGroupSendMu.Lock()
	defer m.hubGroupSendMu.Unlock()
	h, err := st.LoadHandoff(ctx, handoffID)
	if err != nil {
		return nil, err
	}
	g, err := st.LoadGroup(ctx, h.GroupID)
	if err != nil {
		return nil, err
	}
	if g.Deleted {
		return nil, fmt.Errorf("%w: 群组已删除", agenthub.ErrNotFound)
	}
	if !human {
		if actor == "" || actor != h.ToAgentID {
			return nil, fmt.Errorf("%w: 只有交接接收方可以 ack", agenthub.ErrInvalid)
		}
		if _, ok := g.RoleOf(actor); !ok {
			return nil, fmt.Errorf("%w: 接收方不是群成员", agenthub.ErrInvalid)
		}
		if p, err := st.LoadAgent(ctx, actor); err != nil || p == nil || p.Deleted {
			return nil, fmt.Errorf("%w: 接收方已删除", agenthub.ErrInvalid)
		}
	}
	before := h.Status
	if err := h.Ack(accept, time.Now().UTC()); err != nil {
		return nil, err
	}
	if strings.TrimSpace(reason) != "" {
		h.Reason = strings.TrimSpace(reason)
	}
	if err := st.SaveHandoff(ctx, h); err != nil {
		return nil, err
	}
	// 幂等 ack 不重复写卡片/事件。
	if before != h.Status {
		sender := agenthub.Sender{Kind: agenthub.SenderAgent, AgentID: actor}
		if human {
			sender = agenthub.Sender{Kind: agenthub.SenderHuman}
		}
		text := "已接受交接"
		if h.Status == agenthub.HandoffDeclined {
			text = "已拒绝交接"
			if h.Reason != "" {
				text += "：" + truncateRunes(h.Reason, 160)
			}
		}
		card := &agenthub.Message{
			GroupID: h.GroupID, Kind: agenthub.KindHandoff, Intent: agenthub.IntentHandoff,
			Sender: sender, Text: text,
			Refs: map[string]any{"handoff_id": h.ID, "status": string(h.Status)},
		}
		seq, _, err := agenthub.AppendGroupMessageRouted(ctx, st, card, nil)
		if err != nil {
			return nil, err
		}
		m.emitHubGroupMessage(h.GroupID, seq, card)
		m.out.writeLine(map[string]any{
			"event_type": "hub_handoff", "group_id": string(h.GroupID), "handoff": h, "message_seq": seq,
		})
	}
	m.resolveHandoffNeeds(ctx, st, h)
	return h, nil
}

// resolveHandoffNeeds 把该 handoff 仍 pending 的 Need 置 resolved。
func (m *manager) resolveHandoffNeeds(ctx context.Context, st agenthub.Store, h *agenthub.Handoff) {
	needs, err := st.ListNeeds(ctx, agenthub.NeedFilter{Status: "pending"})
	if err != nil {
		return
	}
	now := time.Now().UTC()
	for _, n := range needs {
		if n == nil || n.Kind != "handoff" || n.Anchor != h.ID {
			continue
		}
		n.Status = "resolved"
		n.ResolvedAt = &now
		if err := st.SaveNeed(ctx, n); err == nil {
			m.emitHubNeed(n)
		}
	}
}

func handoffNeed(h *agenthub.Handoff, toName string) *agenthub.Need {
	sum := sha1.Sum([]byte("handoff\x00" + h.ID))
	return &agenthub.Need{
		ID:      agenthub.NeedID("need_" + hex.EncodeToString(sum[:])[:16]),
		Kind:    "handoff",
		Status:  "pending",
		AgentID: h.ToAgentID, GroupID: h.GroupID, Anchor: h.ID,
		Title:     fmt.Sprintf("交接等待 %s 确认", toName),
		Body:      truncateRunes(h.Instruction, 4000),
		CreatedAt: h.CreatedAt,
	}
}

func agentDisplayName(ctx context.Context, st agenthub.Store, id agenthub.AgentID) string {
	if p, err := st.LoadAgent(ctx, id); err == nil && p != nil && strings.TrimSpace(p.Name) != "" {
		return p.Name
	}
	return string(id)
}

func (t *hubHandoffTool) ValidParams(_ context.Context, _, arguments string) error {
	var a struct {
		GroupID     string `json:"group_id"`
		ToAgentID   string `json:"to_agent_id"`
		Instruction string `json:"instruction"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	if strings.TrimSpace(a.GroupID) == "" || strings.TrimSpace(a.ToAgentID) == "" || strings.TrimSpace(a.Instruction) == "" {
		return fmt.Errorf("%w: group_id/to_agent_id/instruction 不能为空", agenthub.ErrInvalid)
	}
	return nil
}

func (t *hubHandoffAckTool) ValidParams(_ context.Context, _, arguments string) error {
	var a struct {
		HandoffID string `json:"handoff_id"`
	}
	if err := json.Unmarshal([]byte(arguments), &a); err != nil {
		return err
	}
	if strings.TrimSpace(a.HandoffID) == "" {
		return fmt.Errorf("%w: handoff_id 不能为空", agenthub.ErrInvalid)
	}
	return nil
}
