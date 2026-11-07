package main

// hub_group_session.go：群内智能体会话与轮次收尾（P3b-01a，方案 §6.5.6）。
//
// 每 (group, agent) 一个确定性会话 hub.<agent>.g.<group>；群 @ 唤醒时构建
// agent_context_seq → trigger_seq 的增量快照，作为 hidden hub_group_wake 块交给
// 该会话。AgentEnd 由 finishHubRound 统一提交 final hub_message（与 P3a-09 的
// 流式 message_id 对齐），并把游标推进到 trigger_seq —— 只推进「已看过」的部分，
// 运行期间到达的他人消息留到下一次唤醒。

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
)

// hubGroupSession 取或首次装配某智能体在群里的会话。
// 与 hubDMSession 同一条懒创建路径，只是 sid/spec 带群作用域。
func (m *manager) hubGroupSession(agentID, groupID string) (*bridgeSession, error) {
	agentID = strings.TrimSpace(agentID)
	groupID = strings.TrimSpace(groupID)
	if agentID == "" || groupID == "" {
		return nil, fmt.Errorf("%w: 群会话需要 agent_id/group_id", agenthub.ErrInvalid)
	}
	st, err := m.hubStore()
	if err != nil {
		return nil, err
	}
	ctx := context.Background()
	g, err := st.LoadGroup(ctx, agenthub.GroupID(groupID))
	if err != nil {
		return nil, err
	}
	if g == nil || g.Deleted {
		return nil, fmt.Errorf("%w: 群组 %s 不存在", agenthub.ErrNotFound, groupID)
	}
	p, _, err := m.hubLoadAgent(agentID)
	if err != nil {
		return nil, err
	}
	if p == nil || p.Deleted {
		return nil, fmt.Errorf("%w: 智能体 %s 不存在", agenthub.ErrNotFound, agentID)
	}
	if !groupHasMember(g, p.ID) {
		return nil, fmt.Errorf("%w: %s 不是群 %s 的成员", agenthub.ErrInvalid, agentID, groupID)
	}
	ws := m.hubRuntime(agentID)
	if ws == nil {
		return nil, fmt.Errorf("%w: hub runtime 不可用（正在关停）", agenthub.ErrInvalid)
	}
	sid := hubSessionID(agentID, agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID})
	unlock := m.lockHubAgent(agentID)
	defer unlock()
	ws.mu.Lock()
	existing := ws.sessions[sid]
	ws.mu.Unlock()
	if existing != nil {
		return existing, nil
	}

	spec := m.hubSpecFor(p, agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID}, ws.path)
	if err := m.enrichHubGroupSpec(ctx, st, spec, g.ID); err != nil {
		return nil, err
	}
	bs, err := m.createIn(ws, sid, p.Effort, sessionOpts{hub: spec})
	if err != nil {
		return nil, err
	}
	return bs, nil
}

func groupHasMember(g *agenthub.Group, agentID agenthub.AgentID) bool {
	if g == nil {
		return false
	}
	for _, m := range g.Members {
		if m.AgentID == agentID {
			return true
		}
	}
	return false
}

// hubGroupMemberProfiles 群成员档案（快照名册/提示词用）；已软删的成员跳过。
func (m *manager) hubGroupMemberProfiles(ctx context.Context, st agenthub.Store, g *agenthub.Group) ([]*agenthub.AgentProfile, error) {
	if g == nil {
		return nil, nil
	}
	agents, err := st.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[agenthub.AgentID]*agenthub.AgentProfile, len(agents))
	for _, a := range agents {
		if a != nil {
			byID[a.ID] = a
		}
	}
	out := make([]*agenthub.AgentProfile, 0, len(g.Members))
	for _, mem := range g.Members {
		if p := byID[mem.AgentID]; p != nil && !p.Deleted {
			out = append(out, p)
		}
	}
	return out, nil
}

// enrichHubGroupSpec 给群作用域补上 Group/Members。重建 loop 的路径也必须走这里，
// 否则 refreshHubLoops 会把群会话的 L2 名册/群工具上下文悄悄换回 DM 版本。
func (m *manager) enrichHubGroupSpec(ctx context.Context, st agenthub.Store, spec *hubSessionSpec, groupID agenthub.GroupID) error {
	if spec == nil || groupID == "" {
		return fmt.Errorf("%w: 群作用域缺 group_id", agenthub.ErrInvalid)
	}
	g, err := st.LoadGroup(ctx, groupID)
	if err != nil {
		return err
	}
	if g == nil || g.Deleted {
		return fmt.Errorf("%w: 群组 %s 不存在", agenthub.ErrNotFound, groupID)
	}
	members, err := m.hubGroupMemberProfiles(ctx, st, g)
	if err != nil {
		return err
	}
	spec.Group = g
	spec.Members = members
	return nil
}

// wakeGroupRound 把人/智能体的 @ 消息投递给每个 target 的群会话。
// 返回每个 target 的错误（不阻断其他 target；调用方决定告警/回执文案）。
//
// 忙目标**不排队**：群会话一次只处理一轮，硬塞第二个快照会让两个 round 共用一次
// AgentEnd（final 归属错轮）。目标仍在群里，下一次被 @ 时游标未推进会重新看到这条
// 消息；P3b 的群调度器再补真正的排队。
func (m *manager) wakeGroupRound(g *agenthub.Group, round agenthub.Round, trigger *agenthub.Message) []string {
	if m == nil || g == nil || trigger == nil {
		return nil
	}
	st, err := m.hubStore()
	if err != nil {
		return []string{err.Error()}
	}
	ctx := context.Background()
	var errs []string
	for _, target := range round.Targets {
		if target == "" || target == trigger.Sender.AgentID {
			continue
		}
		if err := m.wakeGroupAgent(ctx, st, g, round, trigger, target); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", target, err))
		}
	}
	return errs
}

func (m *manager) wakeGroupAgent(ctx context.Context, st agenthub.Store, g *agenthub.Group, round agenthub.Round, trigger *agenthub.Message, target agenthub.AgentID) error {
	bs, err := m.hubGroupSession(string(target), string(g.ID))
	if err != nil {
		return err
	}
	// 忙：不投递第二快照。CAS 防止两个发送路径同时看到空闲。
	if !bs.hubGroupBusy.CompareAndSwap(false, true) {
		return fmt.Errorf("%w: 该智能体上一轮群回复尚未结束", agenthub.ErrInvalid)
	}
	ok := false
	defer func() {
		if !ok {
			bs.hubGroupBusy.Store(false)
		}
	}()

	cursor, err := st.LoadCursor(ctx, g.ID, target)
	if err != nil {
		return err
	}
	msgs, err := st.ReadMessages(ctx, g.ID, 0, 500)
	if err != nil {
		return err
	}
	snap := agenthub.BuildGroupSnapshot(g, target, msgs, cursor.AgentContextSeq, round, agenthub.SnapshotOptions{})
	header := &agenthub.GroupWakeHeader{
		GroupID: g.ID, AgentID: target, RoundID: round.ID, TriggerSeq: trigger.Seq,
	}
	hidden, err := agenthub.EncodeGroupWake(header, snap.Text)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(trigger.Text)
	if text == "" {
		text = "（群消息）"
	}
	msg := core.NewUserMessage(
		core.Content{Type: core.ContentTypeHubGroupWake, Content: hidden},
		core.Content{Type: core.ContentTypeText, Content: text},
	)
	if err := bs.s.Ask(ctx, msg); err != nil {
		return err
	}
	ok = true
	bs.lastActive.Store(time.Now().UnixMilli())
	bs.wakeRun()
	return nil
}

// finishHubReply 根 AgentEnd 的统一收尾：DM 写 DM 索引，群写 final group message。
func (m *manager) finishHubReply(bs *bridgeSession, spec *hubSessionSpec, content string) {
	if spec == nil {
		return
	}
	if spec.Scope.Kind != agenthub.ScopeGroup {
		m.appendHubReply(spec, content)
		return
	}
	wake := bs.hubGroupWake
	bs.hubGroupWake = nil
	bs.hubGroupBusy.Store(false)
	if wake == nil {
		// 没有绑定到轮次（例如进程内其它消息触发的 run）：不往群里写空气泡。
		return
	}
	if wake.GroupID != spec.Scope.GroupID || wake.AgentID != spec.Agent.ID {
		m.hubAlertf("group_round_mismatch",
			"群回复 round 归属不匹配：wake=%s/%s spec=%s/%s",
			wake.GroupID, wake.AgentID, spec.Scope.GroupID, spec.Agent.ID)
		return
	}
	m.finishHubRound(spec, wake, content)
}

// finishHubRound 把群会话最终正文写入群日志，并推进 agent_context_seq。
// 空回复/与最后一条 group_say 重复：不写 final，只推进游标（方案 §6.5.7）。
func (m *manager) finishHubRound(spec *hubSessionSpec, wake *agenthub.GroupWakeHeader, content string) {
	if spec == nil || wake == nil {
		return
	}
	st, err := m.hubStore()
	if err != nil {
		m.hubAlertf("group_reply_failed", "群回复落账失败：%v", err)
		return
	}
	ctx := context.Background()
	g := spec.Group
	if g == nil {
		// 冷恢复/旧构造路径可能没有带 Group 指针：按 header 重新取。
		g, err = st.LoadGroup(ctx, wake.GroupID)
		if err != nil || g == nil || g.Deleted {
			m.hubAlertf("group_reply_failed", "群回复落账失败：群 %s 不存在", wake.GroupID)
			return
		}
		spec.Group = g
	}
	gid := g.ID
	text := strings.TrimSpace(content)
	advanced := false
	if text != "" {
		msgs, err := st.ReadMessages(ctx, gid, 0, 500)
		if err != nil {
			m.hubAlertf("group_reply_failed", "读取群日志失败：%v", err)
			return
		}
		if last := lastAgentReplyInRound(msgs, spec.Agent.ID, wake.TriggerSeq); last != nil && sameGroupReply(last.Text, text) {
			// 模型先 group_say 做了完整汇报，AgentEnd 的 Content 是同一段收尾，
			// 不能再写一条重复气泡。
			advanced = true
		} else {
			msg := &agenthub.Message{
				GroupID: gid,
				Kind:    agenthub.KindText,
				Sender:  agenthub.Sender{Kind: agenthub.SenderAgent, AgentID: spec.Agent.ID},
				Text:    text,
				RoundID: wake.RoundID,
			}
			routed, routeErr := m.routeGroupFinal(ctx, st, g, spec.Agent.ID, text)
			if routeErr != nil {
				// @ 解析失败（越权 @all/保留名等）不能吞掉正文：正文照发，只是不唤醒。
				m.hubAlertf("group_route_failed", "群回复 @ 路由失败：%v", routeErr)
			}
			seq, _, err := agenthub.AppendGroupMessageRouted(ctx, st, msg, routed)
			if err != nil {
				m.hubAlertf("group_reply_failed", "群回复落账失败：%v", err)
				return
			}
			m.emitHubGroupMessage(gid, seq, msg)
			advanced = true
			if routed != nil {
				routed.TriggerSeq = seq
				m.emitHubGroupRound(gid, *routed)
				for _, we := range m.wakeGroupRound(g, *routed, msg) {
					m.hubAlertf("group_wake_failed", "群 %s 唤醒失败：%s", gid, we)
				}
			}
		}
	} else {
		advanced = true
	}
	if advanced {
		if err := m.advanceGroupAgentCursor(ctx, st, gid, spec.Agent.ID, wake.TriggerSeq); err != nil {
			m.hubAlertf("group_cursor_failed", "群游标推进失败：%v", err)
		}
	}
}

func (m *manager) emitHubGroupRound(gid agenthub.GroupID, round agenthub.Round) {
	if m == nil || m.out == nil {
		return
	}
	m.out.writeLine(map[string]any{"event_type": "hub_round", "group_id": string(gid), "round": round})
}

// routeGroupFinal 只对 final 正文做 @ 路由（流式 partial 绝不路由）。
// 失败（越权 @all/保留名/成员已删）返回错误，调用方仍把正文落账。
func (m *manager) routeGroupFinal(ctx context.Context, st agenthub.Store, g *agenthub.Group, sender agenthub.AgentID, text string) (*agenthub.Round, error) {
	if g == nil || strings.TrimSpace(text) == "" {
		return nil, nil
	}
	agents, err := st.ListAgents(ctx)
	if err != nil {
		return nil, err
	}
	byID := make(map[agenthub.AgentID]*agenthub.AgentProfile, len(agents))
	for _, a := range agents {
		if a != nil {
			byID[a.ID] = a
		}
	}
	profiles := make([]*agenthub.AgentProfile, 0, len(g.Members))
	for _, mem := range g.Members {
		if p := byID[mem.AgentID]; p != nil && !p.Deleted {
			profiles = append(profiles, p)
		}
	}
	router, err := agenthub.NewGroupRouter(g, profiles)
	if err != nil {
		return nil, err
	}
	round, err := router.Route(text, sender, false, 0, time.Now())
	if err != nil {
		return nil, err
	}
	if !round.MentionAll && len(round.Targets) == 0 {
		return nil, nil
	}
	return &round, nil
}

func lastAgentReplyInRound(msgs []agenthub.Message, agentID agenthub.AgentID, triggerSeq int64) *agenthub.Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Seq <= triggerSeq {
			continue
		}
		if m.Sender.Kind == agenthub.SenderAgent && m.Sender.AgentID == agentID && strings.TrimSpace(m.Text) != "" {
			return &msgs[i]
		}
	}
	return nil
}

// sameGroupReply 只在「完全一致 / 一方是另一方的前缀且长度接近」时判重：
// 模型常把 group_say 的正文原样又说一遍，但「收到」这种短收尾不能吃掉正文。
func sameGroupReply(a, b string) bool {
	a = strings.TrimSpace(a)
	b = strings.TrimSpace(b)
	if a == "" || b == "" {
		return false
	}
	if a == b {
		return true
	}
	short, long := a, b
	if len([]rune(short)) > len([]rune(long)) {
		short, long = long, short
	}
	rs, rl := []rune(short), []rune(long)
	if len(rs) < 12 || float64(len(rs))/float64(len(rl)) < 0.9 {
		return false
	}
	return strings.HasPrefix(string(rl), string(rs))
}

func (m *manager) advanceGroupAgentCursor(ctx context.Context, st agenthub.Store, gid agenthub.GroupID, agentID agenthub.AgentID, seq int64) error {
	cur, err := st.LoadCursor(ctx, gid, agentID)
	if err != nil {
		return err
	}
	if cur == nil {
		cur = &agenthub.Cursor{GroupID: gid, AgentID: agentID}
	}
	if seq <= cur.AgentContextSeq {
		return nil
	}
	cur.AgentContextSeq = seq
	return st.SaveCursor(ctx, cur)
}

// hubAlertf 是 bridge 内需要格式化时的薄封装；保持 hub_alert 单一出口不变。
func (m *manager) hubAlertf(code, format string, args ...any) {
	if m == nil || m.out == nil {
		return
	}
	m.emitHubAlert(hubAlertEvent{Code: code, Message: fmt.Sprintf(format, args...)})
}
