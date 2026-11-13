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
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// hubGroupAssembleHook 仅测试用：群会话装配在**持有 agent 锁**、createIn 之前的确定性
// 屏障（生产恒为 nil）。用来复现「名册重建与装配并发」的 TOCTOU（P3b-01b N1）。
var hubGroupAssembleHook func(agentID string)

// hubGroupAssembleBarrier 在读钩子时取一次快照：调用方可能并发清空钩子。
func hubGroupAssembleBarrier(agentID string) {
	if hook := hubGroupAssembleHook; hook != nil {
		hook(agentID)
	}
}

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
		if existing.ctx.Err() != nil {
			// 已关闭的会话不能再接新一轮：顺手清闩，避免它把智能体永久标忙。
			// hubDispatchPending 同口径：Ask 成功后会话关掉（AgentStart 不会再来）
			// 会让忙判据永久为真（P3b-03 N6）。
			existing.hubGroupBusy.Store(false)
			existing.hubGroupOwner.Store(0)
			existing.hubDispatchPending.Store(false)
			return nil, fmt.Errorf("%w: 群会话 %s 已关闭", agenthub.ErrInvalid, sid)
		}
		return existing, nil
	}

	spec := m.hubSpecFor(p, agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID}, ws.path)
	if err := m.enrichHubGroupSpec(ctx, st, spec, g.ID); err != nil {
		return nil, err
	}
	// 名册快照已取、会话尚未发布：这里是「装配在飞」的窗口（测试屏障，生产空操作）。
	hubGroupAssembleBarrier(agentID)
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

// refreshHubGroupRoster 名册变化（加入/移除成员、角色/owner 转让、群改名）后，重建该群
// 每个**已装配**群会话的 loop —— 名册在 system prompt L2 里，不换 loop 的话下一次唤醒
// 看到的还是旧名册。
//
// 只换 loop（SetLoop 的快照语义：正在跑的 Run 继续用旧 loop 收尾，下一次 Run 用新的），
// 不重建引擎、不打断在飞的回合，也不动没装配过的成员（首次 wake 时按当前群渲染）。
func (m *manager) refreshHubGroupRoster(groupID agenthub.GroupID) error {
	if m == nil || groupID == "" {
		return nil
	}
	ctx := context.Background()
	st, err := m.hubStore()
	if err != nil {
		return err
	}
	g, err := st.LoadGroup(ctx, groupID)
	if err != nil {
		return err
	}
	if g == nil || g.Deleted {
		return nil // 已删除的群不会再被唤醒，没有可重建的名册
	}
	members, err := m.hubGroupMemberProfiles(ctx, st, g)
	if err != nil {
		return err
	}
	byID := make(map[agenthub.AgentID]*agenthub.AgentProfile, len(members))
	for _, p := range members {
		if p != nil {
			byID[p.ID] = p
		}
	}
	var firstErr error
	for _, member := range g.Members {
		p := byID[member.AgentID]
		if p == nil {
			continue // 档案缺失/软删：名册里是显式 fallback，没有 loop 可重建
		}
		if err := m.refreshHubGroupMemberLoop(g, p); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// refreshHubGroupMemberLoop 重建单个成员的群会话 loop（名册变化路径）。
//
// 锁序与 hubGroupSession 完全一致：先取 runtime（m.mu），再拿 agent 装配锁，最后 ws.mu。
// 「查表 → 重建」必须在 agent 锁内：装配在 lockHubAgent 里完成「取名册快照 → createIn
// 发布」，不持锁的话名册重建会在会话还没发布时读到空表而跳过，随后装配用**变化前**的
// 快照发布，旧名册静默胜出（P3b-01b N1 TOCTOU）。锁内必须二次查表。
//
// 不能持 agent 锁再调 hubRuntimeIfExists：那会变成 agent 锁 → m.mu，与 lockHubAgent 的
// m.mu → agent 锁反向。所以先取 ws，再上锁。
func (m *manager) refreshHubGroupMemberLoop(g *agenthub.Group, p *agenthub.AgentProfile) error {
	if g == nil || p == nil || p.ID == "" {
		return nil
	}
	agentID := string(p.ID)
	ws := m.hubRuntimeIfExists(agentID)
	if ws == nil {
		return nil // 没装配过：零副作用（下一次 wake 按当前群渲染）
	}
	sid := hubSessionID(agentID, agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID})
	unlock := m.lockHubAgent(agentID)
	defer unlock()
	ws.mu.Lock()
	bs := ws.sessions[sid]
	ws.mu.Unlock()
	if bs == nil || bs.ctx.Err() != nil || bs.hubEngine == nil {
		return nil
	}
	if err := m.applyHubSessionLoop(bs, p, ws); err != nil {
		return fmt.Errorf("会话 %s: %w", sid, err)
	}
	return nil
}

// refreshHubAgentRosters 智能体资料变化（改名/职责/能力/软删/恢复）后，重建它所在
// **每个群**的所有成员会话 loop：名册行是给别人看的，改名影响的是别人的 system prompt。
func (m *manager) refreshHubAgentRosters(agentID string) error {
	if m == nil || strings.TrimSpace(agentID) == "" {
		return nil
	}
	st, err := m.hubStore()
	if err != nil {
		return err
	}
	groups, err := st.ListGroups(context.Background())
	if err != nil {
		return err
	}
	var firstErr error
	for _, g := range groups {
		if g == nil || g.Deleted || !groupHasMember(g, agenthub.AgentID(agentID)) {
			continue
		}
		if err := m.refreshHubGroupRoster(g.ID); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// wakeGroupRound 把**人类** @ 消息并行投递给每个 target 的群会话（首轮）。
// 返回每个 target 的错误（不阻断其他 target；调用方决定告警/回执文案）。
//
// 智能体之间的 @ 不在这里：那条路径要串行 + 冷却 + 护栏（hub_conversation.go）。
// 人类消息是「首轮」，可以并行 —— 人发一句话就应当同时叫醒被点名的几个人。
//
// 忙目标**不排队**：群会话一次只处理一轮，硬塞第二个快照会让两个 round 共用一次
// AgentEnd（final 归属错轮）。目标仍在群里，下一次被 @ 时游标未推进会重新看到这条
// 消息；智能体之间的排队由对话控制器负责（P3b-04）。
func (m *manager) wakeGroupRound(g *agenthub.Group, round agenthub.Round, trigger *agenthub.Message, depth, turn int) []string {
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
		w := agenthub.Wake{
			GroupID: g.ID, ConversationID: m.hubConversationView(g.ID).ID,
			Sender: trigger.Sender.AgentID, Human: trigger.Sender.Kind == agenthub.SenderHuman,
			Target: target, Text: trigger.Text, Depth: depth, Turn: turn,
			// 附件随 wake 走：群会话要在 hidden 快照之后把图片拼进模型上下文。
			Attachments: trigger.Attachments,
			TriggerSeq:  trigger.Seq, RoundID: round.ID, EnqueuedAt: time.Now(),
		}
		if err := m.wakeGroupAgent(ctx, st, g, w); err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", target, err))
		}
	}
	return errs
}

// wakeGroupAgent 把一条唤醒投给 target 的群会话（hidden 快照 + 触发消息正文）。
// 忙 → errHubGroupTargetBusy（可 errors.Is）；调用方据此决定「留队重试」还是「报错」。
func (m *manager) wakeGroupAgent(ctx context.Context, st agenthub.Store, g *agenthub.Group, w agenthub.Wake) error {
	target := w.Target
	if target == "" {
		return fmt.Errorf("%w: 群唤醒缺 target", agenthub.ErrInvalid)
	}
	bs, err := m.hubGroupSession(string(target), string(g.ID))
	if err != nil {
		return err
	}
	// 忙：不投递第二快照。CAS 防止两个发送路径同时看到空闲。
	if !bs.hubGroupBusy.CompareAndSwap(false, true) {
		return fmt.Errorf("%w: %w: 该智能体上一轮群回复尚未结束", agenthub.ErrInvalid, errHubGroupTargetBusy)
	}
	seq := bs.hubGroupSeq.Add(1)
	bs.hubGroupOwner.Store(seq)
	ok := false
	defer func() {
		if !ok {
			bs.clearHubGroupBusy(seq)
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
	// 快照只用到 round 的 id/trigger_seq：从 wake 复原一个最小 round，
	// 这样对话队列只需保存协议字段，不必把 Round/Message 指针一直攥着。
	round := agenthub.Round{GroupID: g.ID, ID: w.RoundID, TriggerSeq: w.TriggerSeq, Targets: []agenthub.AgentID{target}}
	snap := agenthub.BuildGroupSnapshot(g, target, msgs, cursor.AgentContextSeq, round, agenthub.SnapshotOptions{})
	header := &agenthub.GroupWakeHeader{
		GroupID: g.ID, AgentID: target, RoundID: w.RoundID, TriggerSeq: w.TriggerSeq,
		Depth: w.Depth, Turn: w.Turn, ConversationID: w.ConversationID,
	}
	hidden, err := agenthub.EncodeGroupWake(header, snap.Text)
	if err != nil {
		return err
	}
	text := strings.TrimSpace(w.Text)
	if text == "" {
		text = "（群消息）"
	}
	// 隐藏快照块 → 补发此前未送达的图片 → 触发消息正文 → 触发消息图片。
	//
	// 补发是必须的：目标忙时 wake 直接返回 busy（不排队），无 @ 的纯图消息也
	// 不唤醒任何人；这两种情况下图片从未进过该智能体的模型上下文。快照只有
	// 文本，模型只会看到 `[图片]` 占位 —— 必须在下一次 wake 把积压的 image
	// 块一起带上，而不是假装它已经看过。
	//
	// 补发窗口是**游标之后的全部消息**（分页从新往旧翻），不是快照那一页：
	// 快照只渲染最近 40 条正文，但图片漏一张就永远补不回来，所以这里单独翻页
	// 找图；msgs 作为最近一页直接复用，省掉一次重复读。快照自身的 500 条窗口
	// 语义不变（BuildGroupSnapshot 只取最近 40 条正文，窗口大小与正文无关）。
	pending, err := hubPendingImagesFromStore(ctx, st, g.ID, msgs, cursor.AgentContextSeq, w.TriggerSeq)
	if err != nil {
		return err
	}
	blocks := []core.Content{
		{Type: core.ContentTypeHubGroupWake, Content: hidden},
	}
	blocks = append(blocks, hubPendingImageBlocks(pending)...)
	blocks = append(blocks, core.Content{Type: core.ContentTypeText, Content: text})
	// 文件附件不进上下文：前端已把 `@路径` 拼进正文，模型按正文自行读取
	// （与主会话 @ 文件同语义）。
	blocks = append(blocks, hubAttachmentImageBlocks(w.Attachments)...)
	msg := core.NewUserMessage(blocks...)
	if err := bs.s.Ask(ctx, msg); err != nil {
		return err
	}
	ok = true
	bs.lastActive.Store(time.Now().UnixMilli())
	bs.wakeRun()
	return nil
}

// finishHubReply 根 AgentEnd 的统一收尾：DM 写 DM 索引，群写 final group message。
//
// segs 是按 LLM 轮次（RequestId）分段的正文（已由 hubDeltaSink.finish 收口并
// flush 完 delta）；空段在落账前过滤，全空时回退 AgentEnd.Content 的单条行为。
// final 必须先于对应 delta 是不可能的（finish 已经 flush），但 delta 必须先于
// 本条 final：前端按 message_id 用 final 替换草稿，先 final 后 delta 会被当迟到丢弃。
//
// 群会话还有第三种 run（Review 场景 3 N1）：**任务结果回推**唤起的 run 没有
// hub_group_wake（AgentStart 用 task_result 正文认领，见 claimHubTaskResultPush）。
// 它的正文照样要落成发起者的一条气泡 —— 否则「收到项目结果，我来总结」只留在
// 会话历史里，群里只剩一张结果卡。
func (m *manager) finishHubReply(bs *bridgeSession, spec *hubSessionSpec, ae *events.AgentEnd, segs []hubSegment) {
	if spec == nil || ae == nil {
		return
	}
	if spec.Scope.Kind != agenthub.ScopeGroup {
		m.appendHubReply(spec, ae.Content, segs)
		return
	}
	wake := bs.hubGroupWake
	bs.setHubGroupWake(nil)
	// 一次性标记：无论这一轮怎么收尾都要取走，不能留给下一个 run。
	taskResult := bs.takeHubTaskResultTask()
	runSeq := bs.hubGroupRunSeq.Load()
	if runSeq == 0 {
		runSeq = bs.hubGroupOwner.Load()
	}
	// 先完成 final 落账/路由，再放下一轮进来；无论是否有 AgentEnd 绑定都要清闩。
	// 注意：这里只清闩。自动对话的下一条 / 忙目标任务泵挂在 emit 的 hubOnRunEnd
	// （running 也放开之后）—— 这个时点 running 还是 true，泵会被判成「目标忙」。
	defer func() {
		bs.hubGroupRunSeq.Store(0)
		bs.clearHubGroupBusy(runSeq)
	}()
	if wake == nil {
		// 回推触发的 run：落一条不路由的群气泡（Sender = 发起者，round = nil）。
		// 其余无 wake 的 run（普通 API 调用、群任务批次）保持静默：不往群里写空气泡。
		if taskResult != "" {
			m.finishHubTaskResultReply(spec, ae, segs)
		}
		return
	}
	if wake.GroupID != spec.Scope.GroupID || wake.AgentID != spec.Agent.ID {
		m.hubAlertf("group_round_mismatch",
			"群回复 round 归属不匹配：wake=%s/%s spec=%s/%s",
			wake.GroupID, wake.AgentID, spec.Scope.GroupID, spec.Agent.ID)
		return
	}
	// 智能体任务批次（hub_task 绑定）：结果由群内 task_result 卡承载（§6.7.1/§6.7.9），
	// 最终正文不再写第二条普通气泡，否则「结果卡 + AgentEnd 正文」＝同一条回复出现两次。
	// 运行中显式 group_say 已单独落账，不受影响。游标仍要推进，避免这条触发消息
	// 下次被重复快照（任务批次正常不带 group_wake，这里只是防御混合批）。
	if bs.currentHubTaskID() != "" {
		m.advanceHubGroupCursor(wake.GroupID, spec.Agent.ID, wake.TriggerSeq)
		return
	}
	m.finishHubRound(spec, wake, ae.Content, segs)
}

// finishHubTaskResultReply 把「Hub 任务结果回推唤起」的群 run 正文落成发起者的
// 一条群气泡（Review 场景 3 N1：总结正文曾经被直接丢弃，群里只剩结果卡）。
//
// 与 finishHubRound 的差别全是**去掉**：没有 wake 就没有 round、没有触发消息的
// seq、没有可推进的游标。所以这里 SkipRoute（不解析 @、不填 mentions）、RoundID
// 空、Context 空、不 offer wakes（不唤醒群里任何人、不启动/续跑自动对话），也不写
// 结果卡（回推那一步已经写过，这里再写就是第二张）。
//
// 落账口径与群 final 一致：非空段各一条，全空回退 AgentEnd.Content。
// 空正文 = 模型选择不输出 → 一条都不写（只留结果卡，不造空气泡）；
// 被中断/失败/护栏截断的 run → 不写半截总结（FinishReason != stop）。
func (m *manager) finishHubTaskResultReply(spec *hubSessionSpec, ae *events.AgentEnd, segs []hubSegment) {
	if m == nil || spec == nil || ae == nil {
		return
	}
	if ae.FinishReason != core.FinishReasonStop {
		return
	}
	texts := hubNonEmptySegments(segs)
	if len(texts) == 0 {
		if text := strings.TrimSpace(ae.Content); text != "" {
			texts = []hubSegment{{text: text}}
		}
	}
	if len(texts) == 0 {
		return
	}
	st, err := m.hubStore()
	if err != nil {
		m.hubAlertf("group_reply_failed", "群回复落账失败：%v", err)
		return
	}
	ctx := context.Background()
	// 与 group_say / 人类发送 / 群 final 共用同一临界区：查重与落盘不能被插队。
	m.hubGroupSendMu.Lock()
	g, err := st.LoadGroup(ctx, spec.Scope.GroupID)
	if err != nil || g == nil || g.Deleted {
		m.hubGroupSendMu.Unlock()
		m.hubAlertf("group_reply_failed", "群回复落账失败：群 %s 不存在", spec.Scope.GroupID)
		return
	}
	msgs, err := st.ReadMessages(ctx, g.ID, 0, 500)
	if err != nil {
		m.hubGroupSendMu.Unlock()
		m.hubAlertf("group_reply_failed", "读取群日志失败：%v", err)
		return
	}
	// 去重与群 final 同口径：模型可能在本轮先用 group_say 汇报过（那条已经落账），
	// AgentEnd 的最后一段是同一段收尾时不再写第二条。没有触发 seq，只看该智能体
	// 最近一条发言。
	lastText := ""
	if last := lastAgentMessageInGroup(msgs, spec.Agent.ID); last != nil {
		lastText = last.Text
	}
	for i, seg := range texts {
		if i == len(texts)-1 && sameGroupReply(lastText, seg.text) {
			break
		}
		w := agentGroupWrite{
			Text: seg.text, MessageID: seg.messageID,
			// 不路由：这条气泡不对应任何 round，@ 不应唤醒任何人。
			SkipRoute: true,
		}
		if _, _, _, err := m.appendAgentGroupMessageLocked(ctx, st, g, spec.Agent.ID, w); err != nil {
			m.hubGroupSendMu.Unlock()
			m.hubAlertf("group_reply_failed", "群回复落账失败：%v", err)
			return
		}
	}
	m.hubGroupSendMu.Unlock()
}

// lastAgentMessageInGroup 取该智能体在群日志里最近一条非空发言（不按 seq 过滤：
// 结果回推触发的 run 没有触发消息，去重只能对着「上一条我说的话」）。
func lastAgentMessageInGroup(msgs []agenthub.Message, agentID agenthub.AgentID) *agenthub.Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		m := msgs[i]
		if m.Sender.Kind == agenthub.SenderAgent && m.Sender.AgentID == agentID && strings.TrimSpace(m.Text) != "" {
			return &msgs[i]
		}
	}
	return nil
}

// finishHubRound 把群会话最终正文写入群日志，并推进 agent_context_seq。
// 每一段非空正文（一次 LLM 调用）各写一条 final 消息；空回复/最后一段与
// group_say 重复：不写那一条（全部为空时只推进游标，方案 §6.5.7）。
//
// 为什么只有最后一段参与路由：同一轮里模型可能先铺垫正文、再调工具、再说结论。
// 中间段的 @ 不是最终意图（且常是复述），让它们各自 @ 会把同一批目标唤醒多次。
// hubFinishRoundHook 仅测试用：final 落账入口的确定性停点（生产恒 nil）。
// 用来证明「停止标记已落地时，正在收尾的 fast run 一定在闸门前被拦下」。
var hubFinishRoundHook func(gid agenthub.GroupID)

// hubFinishRoundBarrier 读钩子时取一次快照（同其它 barrier 惯例）。
func hubFinishRoundBarrier(gid agenthub.GroupID) {
	if hook := hubFinishRoundHook; hook != nil {
		hook(gid)
	}
}

func (m *manager) finishHubRound(spec *hubSessionSpec, wake *agenthub.GroupWakeHeader, content string, segs []hubSegment) {
	if spec == nil || wake == nil {
		return
	}
	hubFinishRoundBarrier(wake.GroupID)
	// Stop P1：人类已停止自动对话时，停止窗口里跑完的 fast run 不能再把 final
	// 写进群（先是「停止」系统消息、后面又冒出一条智能体发言）。
	//
	// 状态检查 + 落账都持群 dispatch 闸门，而 hubConversationStop 是**先标记
	// stopped、再抢闸门**：所以这里读到的 stopped 一定是权威顺序 —— 要么 final
	// 在停止之前已经写完（闸门先到），要么被这里抑制（标记先到）。人类插话开的
	// 新对话是 running（不是 stopped），旧 run 允许正常收尾。
	unlock := m.hubConvDispatch.lock(wake.GroupID)
	defer unlock()
	if m.hubConversationView(wake.GroupID).Status == agenthub.ConversationStopped {
		st, serr := m.hubStore()
		if serr != nil {
			m.hubAlertf("group_cursor_failed", "群游标推进失败：%v", serr)
			return
		}
		if err := m.advanceGroupAgentCursor(context.Background(), st, wake.GroupID, spec.Agent.ID, wake.TriggerSeq); err != nil {
			m.hubAlertf("group_cursor_failed", "群游标推进失败：%v", err)
		}
		return
	}
	st, err := m.hubStore()
	if err != nil {
		m.hubAlertf("group_reply_failed", "群回复落账失败：%v", err)
		return
	}
	ctx := context.Background()
	// 本轮要落账的正文：按 LLM 轮次分段的非空段各一条气泡（顺序 = seq 顺序）。
	// 一段都没有（老链路没有 LLM 事件 / 段全空）才回退 AgentEnd.Content 的单条行为。
	texts := hubNonEmptySegments(segs)
	if len(texts) == 0 {
		if text := strings.TrimSpace(content); text != "" {
			texts = []hubSegment{{messageID: wake.MessageID, text: text}}
		}
	}
	if len(texts) == 0 {
		// 纯工具轮/空回复：不写气泡，但这一轮已经处理过，推进游标。
		if err := m.advanceGroupAgentCursor(ctx, st, wake.GroupID, spec.Agent.ID, wake.TriggerSeq); err != nil {
			m.hubAlertf("group_cursor_failed", "群游标推进失败：%v", err)
		}
		return
	}

	// 与 group_say / 人类发送共用同一临界区：查重、路由、落盘不能被插队。
	m.hubGroupSendMu.Lock()
	g, err := st.LoadGroup(ctx, wake.GroupID)
	if err != nil || g == nil || g.Deleted {
		m.hubGroupSendMu.Unlock()
		m.hubAlertf("group_reply_failed", "群回复落账失败：群 %s 不存在", wake.GroupID)
		return
	}
	msgs, err := st.ReadMessages(ctx, g.ID, 0, 500)
	if err != nil {
		m.hubGroupSendMu.Unlock()
		m.hubAlertf("group_reply_failed", "读取群日志失败：%v", err)
		return
	}
	// 本轮回复的触发上下文（P3b-07 B6）：只算「该智能体上次读到 → 本次唤醒触发消息」
	// 之间的群消息，senders 只收智能体（人类没有 AgentRef）。没有多智能体来源 = nil，
	// 前端不会画 TurnContextLine。
	var turnCtx *agenthub.TurnContext
	if cur, cerr := st.LoadCursor(ctx, g.ID, spec.Agent.ID); cerr == nil {
		turnCtx = agenthub.BuildTurnContext(spec.Agent.ID, msgs, cur.AgentContextSeq, wake.TriggerSeq)
	}
	// 去重只看最后一段：中间段是铺垫，重复与否不影响「本轮回复是否已由 group_say 说过」。
	lastText := ""
	if last := lastAgentReplyInRound(msgs, spec.Agent.ID, wake.TriggerSeq); last != nil {
		lastText = last.Text
	}
	var lastMsg *agenthub.Message
	var lastRouted *agenthub.Round
	var lastRouteErr error
	for i, seg := range texts {
		last := i == len(texts)-1
		if last && sameGroupReply(lastText, seg.text) {
			// 模型先 group_say 做了完整汇报，AgentEnd 的最后一段是同一段收尾，
			// 不能再写一条重复气泡；本轮已处理，推进游标。
			break
		}
		w := agentGroupWrite{
			Text: seg.text, RoundID: wake.RoundID, MessageID: seg.messageID,
			SkipRoute: !last,
		}
		if last {
			w.Context = turnCtx
		}
		msg, routed, routeErr, err := m.appendAgentGroupMessageLocked(ctx, st, g, spec.Agent.ID, w)
		if err != nil {
			m.hubGroupSendMu.Unlock()
			m.hubAlertf("group_reply_failed", "群回复落账失败：%v", err)
			return
		}
		lastText = seg.text
		if last {
			lastMsg, lastRouted, lastRouteErr = msg, routed, routeErr
		}
	}
	m.hubGroupSendMu.Unlock()
	if lastRouteErr != nil {
		// @ 解析失败（越权 @all/保留名等）不能吞掉正文：正文已照发，只是不唤醒。
		m.hubAlertf("group_route_failed", "群回复 @ 路由失败：%v", lastRouteErr)
	}
	if err := m.advanceGroupAgentCursor(ctx, st, g.ID, spec.Agent.ID, wake.TriggerSeq); err != nil {
		m.hubAlertf("group_cursor_failed", "群游标推进失败：%v", err)
	}
	if lastRouted != nil && lastMsg != nil {
		// 智能体之间的 @ 走对话控制器：入队（串行 + 冷却 + 护栏）。投递由本轮 run
		// 真正收尾后的 hubOnRunEnd（running 与群轮闩都已放开）触发。
		depth := wake.Depth + 1
		if _, oerr := m.offerGroupWakes(g, *lastRouted, lastMsg, spec.Agent.ID, depth); oerr != nil &&
			!errors.Is(oerr, agenthub.ErrConversationExhausted) {
			m.hubAlertf("group_conversation_offer_failed", "群 %s 对话入队失败：%v", g.ID, oerr)
		}
	}
}

// agentGroupWrite 是一次「智能体群消息」的落账请求。group_say 可以带结构化
// mentions/mention_all（P3b-02）；final 回复只用正文，正文里的 @Name 仍由 Router 解析。
type agentGroupWrite struct {
	Text       string
	RoundID    string
	MessageID  string
	Mentions   []agenthub.AgentID
	MentionAll bool
	RouteFatal bool
	// SkipRoute 跳过 @ 路由：同一次 AgentEnd 里的**中间段**只落正文，不唤醒任何人
	// （路由只由最后一段决定；消息的 effective mentions 也不会被填）。
	SkipRoute bool
	// Context 是本轮回复的触发上下文（final 由 finishHubRound 从游标窗口算；
	// group_say 暂不填）。只进 UI，不参与路由/唤醒。
	Context *agenthub.TurnContext
}

// appendAgentGroupMessageLocked 是 group_say / final 共用的「路由 + 落账」路径。
// 调用方必须持有 hubGroupSendMu；wake 由调用方在解锁后做（会话装配可能很慢）。
// routeFatal=true：@ 解析失败就不落消息（group_say 的工具语义）；false：正文照发、
// 只把 routeErr 交回调用方告警（final 的语义）。
//
// 不再有「人类发言后最多 N 条」的闸门（用户口径：群策略限制取消）：
// 群策略 MaxTurnsPerHuman 字段保留只为兼容旧 JSON，不参与拦截。
func (m *manager) appendAgentGroupMessageLocked(ctx context.Context, st agenthub.Store, g *agenthub.Group, sender agenthub.AgentID, w agentGroupWrite) (msg *agenthub.Message, routed *agenthub.Round, routeErr error, err error) {
	var r *agenthub.Round
	if !w.SkipRoute {
		r, routeErr = m.routeGroupMentions(ctx, st, g, sender, false, agenthub.MentionRequest{
			Text: w.Text, Mentions: w.Mentions, MentionAll: w.MentionAll,
		})
		if routeErr != nil {
			if w.RouteFatal {
				return nil, nil, routeErr, nil
			}
			r = nil
		}
	}
	msg = &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindText, ID: w.MessageID,
		Sender: agenthub.Sender{Kind: agenthub.SenderAgent, AgentID: sender},
		Text:   w.Text, RoundID: w.RoundID, Context: w.Context,
	}
	if r != nil {
		routed = r
		// 持久化生效的 mentions（正文 @ 与结构化 id 的并集，已截断/去重）；
		// @all 时 Targets 是全体，不能拿它当 Mentions（P3b-02）。
		msg.Mentions = append([]agenthub.AgentID(nil), r.Mentions...)
		msg.MentionAll = r.MentionAll
	}
	seq, _, err := agenthub.AppendGroupMessageRouted(ctx, st, msg, routed)
	if err != nil {
		return nil, nil, routeErr, err
	}
	m.emitHubGroupMessage(g.ID, seq, msg)
	if routed != nil {
		routed.TriggerSeq = seq
		m.emitHubGroupRound(g.ID, *routed)
	}
	return msg, routed, routeErr, nil
}

// routeGroupMentions 只对完整正文做 @ 路由（streaming partial 绝不路由），并把结构化
// mentions 一起交给 Router（@all 鉴权、成员校验、上限都在 agenthub 里）。
// 智能体正文/群内指派传 isHuman=false，人派任务的指派消息传 true（P3b-07a）。
// 失败（越权 @all/保留名/成员已删/非成员 id）返回错误，调用方决定是否仍落正文。
func (m *manager) routeGroupMentions(ctx context.Context, st agenthub.Store, g *agenthub.Group, sender agenthub.AgentID, isHuman bool, in agenthub.MentionRequest) (*agenthub.Round, error) {
	if g == nil || (strings.TrimSpace(in.Text) == "" && len(in.Mentions) == 0 && !in.MentionAll) {
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
	round, err := router.RouteMentions(in, sender, isHuman, 0, time.Now())
	if err != nil {
		return nil, err
	}
	if !round.MentionAll && len(round.Targets) == 0 {
		return nil, nil
	}
	return &round, nil
}

func (m *manager) emitHubGroupRound(gid agenthub.GroupID, round agenthub.Round) {
	if m == nil || m.out == nil {
		return
	}
	m.out.writeLine(map[string]any{"event_type": "hub_round", "group_id": string(gid), "round": round})
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
	// 必须走字段级原子推进：整行 SaveCursor 会覆盖人类已读游标（两个独立写者）。
	return st.AdvanceAgentContextSeq(ctx, gid, agentID, seq)
}

// advanceHubGroupCursor 只推进「已看过」的游标（没有会话侧 store 句柄时用）。
func (m *manager) advanceHubGroupCursor(gid agenthub.GroupID, agentID agenthub.AgentID, seq int64) {
	if m == nil || gid == "" || agentID == "" {
		return
	}
	st, err := m.hubStore()
	if err != nil {
		m.hubAlertf("group_cursor_failed", "群游标推进失败：%v", err)
		return
	}
	if err := m.advanceGroupAgentCursor(context.Background(), st, gid, agentID, seq); err != nil {
		m.hubAlertf("group_cursor_failed", "群游标推进失败：%v", err)
	}
}

// hubAlertf 是 bridge 内需要格式化时的薄封装；保持 hub_alert 单一出口不变。
func (m *manager) hubAlertf(code, format string, args ...any) {
	if m == nil || m.out == nil {
		return
	}
	m.emitHubAlert(hubAlertEvent{Code: code, Message: fmt.Sprintf(format, args...)})
}
