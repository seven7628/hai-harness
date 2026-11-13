package main

// P3b-01a：群会话懒装配 / @ 唤醒 / final hub_message / 游标推进。

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

func groupWithMembers(t *testing.T, m *manager, buf *syncBuf, cmdID int, name string, agents ...struct {
	id   agenthub.AgentID
	role string
}) *agenthub.Group {
	t.Helper()
	members := make([]map[string]any, 0, len(agents))
	for _, a := range agents {
		members = append(members, map[string]any{"agent_id": string(a.id), "role": a.role})
	}
	ok, errMsg, data := hubCmd(t, m, buf, cmdID, "hub_group_create", map[string]any{
		"name": name, "members": members,
	})
	if !ok {
		t.Fatalf("create group: %s", errMsg)
	}
	return groupFromData(t, data)
}

func setGroupFakeLoop(t *testing.T, m *manager, bs *bridgeSession, g *agenthub.Group, agentID string, fake provider.Provider) {
	t.Helper()
	setGroupFakeLoopIters(t, m, bs, g, agentID, fake, 1)
}

// setGroupFakeLoopIters 同上，但给 loop 留 maxIter 轮（工具轮 + 收尾正文需要 ≥2；
// P3b-08 评测 8 的「A 生产 assign_task 工具调用」走这条）。
func setGroupFakeLoopIters(t *testing.T, m *manager, bs *bridgeSession, g *agenthub.Group, agentID string, fake provider.Provider, maxIter int) {
	t.Helper()
	if maxIter <= 0 {
		maxIter = 1
	}
	loaded, _, err := m.hubLoadAgent(agentID)
	if err != nil {
		t.Fatal(err)
	}
	members, err := m.hubGroupMemberProfiles(context.Background(), mustHubStore(t, m), g)
	if err != nil {
		t.Fatal(err)
	}
	ws := m.hubRuntime(agentID)
	spec := m.hubSpecFor(loaded, agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID}, ws.path)
	spec.Group = g
	spec.Members = members
	opts := m.hubLoopOptions(ws, bs.id, spec, bs.hubEngine)
	opts = append(opts,
		agents.WithProvider(fake),
		agents.WithModel("group-fake"),
		agents.WithMaxIterations(maxIter),
	)
	bs.s.SetLoop(agents.NewAgentLoop(opts...))
	bs.loop = bs.s.Loop()
}

func mustHubStore(t *testing.T, m *manager) agenthub.Store {
	t.Helper()
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func waitGroupMessages(t *testing.T, m *manager, gid agenthub.GroupID, pred func([]agenthub.Message) bool) []agenthub.Message {
	t.Helper()
	st := mustHubStore(t, m)
	deadline := time.Now().Add(20 * time.Second)
	var msgs []agenthub.Message
	var lastErr error
	for time.Now().Before(deadline) {
		msgs, lastErr = st.ReadMessages(context.Background(), gid, 0, 500)
		if lastErr == nil && pred(msgs) {
			return msgs
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("group messages not in expected state: %v (last=%v)", lastErr, msgs)
	return nil
}

func TestHubGroupSessionLazySingleton(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "GroupSessionA")
	g := groupWithMembers(t, m, buf, 2, "会话群", struct {
		id   agenthub.AgentID
		role string
	}{a.ID, "owner"})

	bs, err := m.hubGroupSession(string(a.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	wantSID := hubSessionID(string(a.ID), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID})
	if bs.id != wantSID {
		t.Fatalf("group session id = %q, want %q", bs.id, wantSID)
	}
	again, err := m.hubGroupSession(string(a.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	if again != bs {
		t.Fatal("hubGroupSession 返回了另一个会话对象：每群一会话的单例语义不成立")
	}
	ws := m.hubRuntime(string(a.ID))
	ws.mu.Lock()
	count := len(ws.sessions)
	ws.mu.Unlock()
	if count != 1 {
		t.Fatalf("ws.sessions = %d, want only the group session", count)
	}
	// 非成员不能装配群会话。
	b := createHubAgent(t, m, buf, 3, "Outsider")
	if _, err := m.hubGroupSession(string(b.ID), string(g.ID)); err == nil {
		t.Fatal("non-member must not get a group session")
	}
}

func TestHubGroupSendWakesTargetAndWritesFinalMessage(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "OwnerA")
	b := createHubAgent(t, m, buf, 2, "MemberB")
	g := groupWithMembers(t, m, buf, 3, "唤醒群",
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"},
		struct {
			id   agenthub.AgentID
			role string
		}{b.ID, "member"},
	)
	bs, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	fake := &evalProvider{reply: "B 在群里看到了你的消息，这是最终回复。"}
	setGroupFakeLoop(t, m, bs, g, string(b.ID), fake)

	ok, errMsg, data := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id": string(g.ID),
		"text":     "@MemberB 看一下这条",
	})
	if !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	round := data["round"].(map[string]any)
	roundID, _ := round["id"].(string)
	triggerSeq := int64(numOr(data, "seq", 0))
	if roundID == "" || triggerSeq == 0 {
		t.Fatalf("send payload = %+v", data)
	}

	msgs := waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		for _, msg := range msgs {
			if msg.Sender.Kind == agenthub.SenderAgent && msg.Sender.AgentID == b.ID &&
				strings.Contains(msg.Text, "最终回复") {
				return true
			}
		}
		return false
	})
	var final *agenthub.Message
	for i := range msgs {
		if msgs[i].Sender.AgentID == b.ID && strings.Contains(msgs[i].Text, "最终回复") {
			final = &msgs[i]
			break
		}
	}
	if final == nil {
		t.Fatal("no final group message")
	}
	if final.RoundID != roundID {
		t.Fatalf("final round_id = %q, want %q", final.RoundID, roundID)
	}
	if !strings.HasPrefix(final.ID, "msg_") || final.Seq <= triggerSeq {
		t.Fatalf("final message = %+v", final)
	}
	// 游标只推进到触发消息：运行期间别人的新消息留给下一次唤醒。
	// 消息先落、游标后写，二者不在一个事务里 —— 等游标，避免读在两次写之间。
	st := mustHubStore(t, m)
	deadline := time.Now().Add(5 * time.Second)
	var cur *agenthub.Cursor
	for time.Now().Before(deadline) {
		cur, err = st.LoadCursor(context.Background(), g.ID, b.ID)
		if err == nil && cur.AgentContextSeq == triggerSeq {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if cur == nil || cur.AgentContextSeq != triggerSeq {
		t.Fatalf("agent_context_seq = %+v, want trigger %d", cur, triggerSeq)
	}
	deadline = time.Now().Add(5 * time.Second)
	for bs.hubGroupBusy.Load() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if bs.hubGroupBusy.Load() {
		t.Fatal("group session still busy after final reply")
	}
	// final 事件与落盘消息同 id，前端据此替换草稿。
	var finalEvents int
	for _, ev := range hubEvents(t, buf, "hub_message") {
		if ev["group_id"] != string(g.ID) {
			continue
		}
		msg, _ := ev["message"].(map[string]any)
		if msg != nil && msg["id"] == final.ID {
			finalEvents++
		}
	}
	if finalEvents != 1 {
		t.Fatalf("hub_message events for final id %s = %d, want 1", final.ID, finalEvents)
	}
}

func TestHubGroupEmptyReplyAdvancesCursorWithoutMessage(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "EmptyOwner")
	b := createHubAgent(t, m, buf, 2, "EmptyB")
	g := groupWithMembers(t, m, buf, 3, "空回复群",
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"},
		struct {
			id   agenthub.AgentID
			role string
		}{b.ID, "member"},
	)
	bs, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	setGroupFakeLoop(t, m, bs, g, string(b.ID), &evalProvider{reply: ""})

	ok, errMsg, data := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id": string(g.ID),
		"text":     "@EmptyB 这个不用发言",
	})
	if !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	triggerSeq := int64(numOr(data, "seq", 0))
	st := mustHubStore(t, m)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		cur, err := st.LoadCursor(context.Background(), g.ID, b.ID)
		if err == nil && cur.AgentContextSeq == triggerSeq {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	msgs, err := st.ReadMessages(context.Background(), g.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, msg := range msgs {
		if msg.Sender.Kind == agenthub.SenderAgent && msg.Sender.AgentID == b.ID {
			t.Fatalf("empty reply must not write a group message: %+v", msg)
		}
	}
	if bs.hubGroupBusy.Load() {
		t.Fatal("empty reply must clear busy")
	}
}

func TestFinishHubRoundDedupesGroupSay(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "DedupOwner")
	b := createHubAgent(t, m, buf, 2, "DedupB")
	g := groupWithMembers(t, m, buf, 3, "去重群",
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"},
		struct {
			id   agenthub.AgentID
			role string
		}{b.ID, "member"},
	)
	st := mustHubStore(t, m)
	trigger, err := st.AppendMessage(context.Background(), &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindText,
		Sender: agenthub.Sender{Kind: agenthub.SenderHuman}, Text: "@DedupB 汇报一下",
	})
	if err != nil {
		t.Fatal(err)
	}
	const report = "这是完整的汇报正文，足够长以避免短句误判为重复。"
	if _, err := st.AppendMessage(context.Background(), &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindText, RoundID: "rnd_dedup",
		Sender: agenthub.Sender{Kind: agenthub.SenderAgent, AgentID: b.ID}, Text: report,
	}); err != nil {
		t.Fatal(err)
	}
	loaded, _, err := m.hubLoadAgent(string(b.ID))
	if err != nil {
		t.Fatal(err)
	}
	spec := m.hubSpecFor(loaded, agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID}, m.hubRuntime(string(b.ID)).path)
	spec.Group = g
	wake := &agenthub.GroupWakeHeader{
		GroupID: g.ID, AgentID: b.ID, RoundID: "rnd_dedup", TriggerSeq: trigger,
	}
	m.finishHubRound(spec, wake, report, nil)
	msgs, err := st.ReadMessages(context.Background(), g.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	agentMsgs := 0
	for _, msg := range msgs {
		if msg.Sender.AgentID == b.ID {
			agentMsgs++
		}
	}
	if agentMsgs != 1 {
		t.Fatalf("agent messages = %d, want 1 (group_say final must be deduped)", agentMsgs)
	}
	cur, err := st.LoadCursor(context.Background(), g.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.AgentContextSeq != trigger {
		t.Fatalf("agent_context_seq = %d, want %d", cur.AgentContextSeq, trigger)
	}
}

func TestHubGroupFinalMentionRoutesNextRound(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "RouteOwner")
	b := createHubAgent(t, m, buf, 2, "RouteB")
	c := createHubAgent(t, m, buf, 3, "RouteC")
	g := groupWithMembers(t, m, buf, 4, "路由群",
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"},
		struct {
			id   agenthub.AgentID
			role string
		}{b.ID, "member"},
		struct {
			id   agenthub.AgentID
			role string
		}{c.ID, "member"},
	)
	bsB, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	setGroupFakeLoop(t, m, bsB, g, string(b.ID), &evalProvider{reply: "收到，交给 @RouteC 处理。"})
	bsC, err := m.hubGroupSession(string(c.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	setGroupFakeLoop(t, m, bsC, g, string(c.ID), &evalProvider{reply: "C 已接手。"})

	ok, errMsg, data := hubCmd(t, m, buf, 5, "hub_group_send", map[string]any{
		"group_id": string(g.ID),
		"text":     "@RouteB 转给 C",
	})
	if !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	firstRound, _ := data["round"].(map[string]any)
	firstRoundID, _ := firstRound["id"].(string)
	msgs := waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		for _, msg := range msgs {
			if msg.Sender.AgentID == c.ID && strings.Contains(msg.Text, "C 已接手") {
				return true
			}
		}
		return false
	})
	var bMsg, cMsg *agenthub.Message
	for i := range msgs {
		switch msgs[i].Sender.AgentID {
		case b.ID:
			bMsg = &msgs[i]
		case c.ID:
			cMsg = &msgs[i]
		}
	}
	if bMsg == nil || cMsg == nil {
		t.Fatalf("missing routed replies: b=%v c=%v", bMsg, cMsg)
	}
	if bMsg.RoundID != firstRoundID {
		t.Fatalf("B final round_id = %q, want %q", bMsg.RoundID, firstRoundID)
	}
	if cMsg.RoundID == "" || cMsg.RoundID == firstRoundID {
		t.Fatalf("C reply round_id = %q, want a new round", cMsg.RoundID)
	}
	// 新轮必须有 routing 回执（@ 解析成功且落了 receipt）。
	var newReceipt bool
	for _, msg := range msgs {
		if msg.Kind == agenthub.KindRouting && msg.RoundID == cMsg.RoundID {
			newReceipt = true
			break
		}
	}
	if !newReceipt {
		t.Fatalf("no routing receipt for new round %s", cMsg.RoundID)
	}
}

func TestEnrichHubGroupSpecKeepsRosterOnRebuild(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "RosterA")
	b := createHubAgent(t, m, buf, 2, "RosterB")
	g := groupWithMembers(t, m, buf, 3, "名册群",
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"},
		struct {
			id   agenthub.AgentID
			role string
		}{b.ID, "member"},
	)
	loaded, _, err := m.hubLoadAgent(string(b.ID))
	if err != nil {
		t.Fatal(err)
	}
	spec := m.hubSpecFor(loaded, agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID}, m.hubRuntime(string(b.ID)).path)
	if spec.Group != nil {
		t.Fatal("fixture: spec should start without Group")
	}
	if err := m.enrichHubGroupSpec(context.Background(), mustHubStore(t, m), spec, g.ID); err != nil {
		t.Fatal(err)
	}
	if spec.Group == nil || spec.Group.ID != g.ID || len(spec.Members) != 2 {
		t.Fatalf("enriched spec = group=%+v members=%d", spec.Group, len(spec.Members))
	}
}

func TestHubGroupSendSkipsBusyTargetWithoutCorruptingRound(t *testing.T) {
	m, buf := testManager(t)
	convLimits(m, agenthub.ConversationLimits{Cooldown: -1})
	a := createHubAgent(t, m, buf, 1, "BusyOwner")
	b := createHubAgent(t, m, buf, 2, "BusyB")
	g := groupWithMembers(t, m, buf, 3, "忙群",
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"},
		struct {
			id   agenthub.AgentID
			role string
		}{b.ID, "member"},
	)
	bs, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	// 门闩式 fake：补投之后 B 会真的跑一轮并卡住，好断言「投出去了」。
	fb := newConvProvider("B 补投后的回复")
	release := fb.gated()
	defer close(release)
	setGroupFakeLoop(t, m, bs, g, string(b.ID), fb)
	bs.hubGroupBusy.Store(true) // 模拟上一轮还没 AgentEnd
	defer bs.hubGroupBusy.Store(false)

	ok, errMsg, data := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id": string(g.ID),
		"text":     "@BusyB 第二条",
	})
	if !ok {
		t.Fatalf("message itself must still be accepted: %s", errMsg)
	}
	// P6-01：目标忙 → 账本记 queued(target_busy)，wake 留队重试；不再是 wake_errors。
	d0 := mustDelivery(t, data, b.ID)
	if d0.Status != "queued" || d0.Reason != "target_busy" {
		t.Fatalf("busy target must be queued/target_busy, got %+v", d0)
	}
	// 群消息必须已经落日志（投递没出去也不回滚）。
	st := mustHubStore(t, m)
	msgs, err := st.ReadMessages(context.Background(), g.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 {
		t.Fatal("group message missing after queued wake")
	}
	if n := fb.callCount(); n != 0 {
		t.Fatalf("忙目标不该跑模型，实际调用 %d 次", n)
	}
	// 目标这一轮结束后（忙闩放开 + 泵），排队的 wake 必须真的投出去 —— 不丢，
	// 而且是**同一条 wake id**（幂等键不变）。
	bs.hubGroupBusy.Store(false)
	m.scheduleHubConversationPump(g.ID)
	fb.waitStarted(t)
	d1, found := m.hubConversation().Delivery(g.ID, d0.WakeID)
	if !found || d1.Status != "running" {
		t.Fatalf("补投后的记录 = %+v (found=%v), want running", d1, found)
	}
	if d1.Attempts < 2 {
		t.Fatalf("attempts = %d, want >= 2（忙一次 + 补投一次）", d1.Attempts)
	}
}

// 用户口径「群策略限制取消」：人类之后已有 8 条智能体发言，final 仍然要追加。
// （P6-13：那个旋钮已经从 GroupPolicy 删掉，这里用字面量 8 表示「远超旧默认值」。）
func TestFinishHubRoundIgnoresRemovedTurnsGate(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "LimitOwner")
	b := createHubAgent(t, m, buf, 2, "LimitB")
	g := groupWithMembers(t, m, buf, 3, "上限群",
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"},
		struct {
			id   agenthub.AgentID
			role string
		}{b.ID, "member"},
	)
	st := mustHubStore(t, m)
	trigger, err := st.AppendMessage(context.Background(), &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindText,
		Sender: agenthub.Sender{Kind: agenthub.SenderHuman}, Text: "@LimitB 开始",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 人类之后已有 8 条智能体发言（旧默认上限就是 8，但那个闸门已删）。
	for i := 0; i < 8; i++ {
		if _, err := st.AppendMessage(context.Background(), &agenthub.Message{
			GroupID: g.ID, Kind: agenthub.KindText, RoundID: "rnd_limit",
			Sender: agenthub.Sender{Kind: agenthub.SenderAgent, AgentID: b.ID},
			Text:   "第 " + string(rune('A'+i)) + " 条",
		}); err != nil {
			t.Fatal(err)
		}
	}
	loaded, _, err := m.hubLoadAgent(string(b.ID))
	if err != nil {
		t.Fatal(err)
	}
	spec := m.hubSpecFor(loaded, agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID}, m.hubRuntime(string(b.ID)).path)
	spec.Group = g
	wake := &agenthub.GroupWakeHeader{GroupID: g.ID, AgentID: b.ID, RoundID: "rnd_limit", TriggerSeq: trigger}
	m.finishHubRound(spec, wake, "再补一条", nil)
	msgs, err := st.ReadMessages(context.Background(), g.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	agentMsgs := 0
	for _, msg := range msgs {
		if msg.Sender.AgentID == b.ID {
			agentMsgs++
		}
	}
	if want := 9; agentMsgs != want {
		t.Fatalf("agent messages = %d, want %d（第 9 条必须落盘）", agentMsgs, want)
	}
	cur, err := st.LoadCursor(context.Background(), g.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.AgentContextSeq != trigger {
		t.Fatalf("agent_context_seq = %d, want %d", cur.AgentContextSeq, trigger)
	}
}

func TestClearHubGroupBusyOnlyOwnEpoch(t *testing.T) {
	bs := &bridgeSession{}
	bs.hubGroupBusy.Store(true)
	bs.hubGroupOwner.Store(1)
	bs.clearHubGroupBusy(1)
	if bs.hubGroupBusy.Load() {
		t.Fatal("owning run must clear the group latch")
	}
	// 代号单调递增：下一轮是 2，旧 run 手里的 1 不能清它（ABA）。
	bs.hubGroupBusy.Store(true)
	bs.hubGroupOwner.Store(2)
	bs.clearHubGroupBusy(1)
	if !bs.hubGroupBusy.Load() {
		t.Fatal("stale run must not clear a newer group latch (ABA)")
	}
	bs.clearHubGroupBusy(2)
	if bs.hubGroupBusy.Load() {
		t.Fatal("owning run must clear the group latch")
	}
}

func TestHubGroupSessionClosedClearsBusy(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "ClosedA")
	g := groupWithMembers(t, m, buf, 2, "关闭群", struct {
		id   agenthub.AgentID
		role string
	}{a.ID, "owner"})
	bs, err := m.hubGroupSession(string(a.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	bs.hubGroupBusy.Store(true)
	bs.hubGroupOwner.Store(9)
	bs.cancel()
	if _, err := m.hubGroupSession(string(a.ID), string(g.ID)); err == nil {
		t.Fatal("closed group session must not accept a new wake")
	}
	if bs.hubGroupBusy.Load() || bs.hubGroupOwner.Load() != 0 {
		t.Fatal("closed group session must release the busy latch")
	}
}

func TestClearHubGroupBusyAfterRunKeepsPendingRound(t *testing.T) {
	bs := &bridgeSession{}
	bs.hubGroupBusy.Store(true)
	bs.hubGroupOwner.Store(3)
	bs.clearHubGroupBusyAfterRun(1)
	if !bs.hubGroupBusy.Load() {
		t.Fatal("pending input must keep the group latch")
	}
	bs.clearHubGroupBusyAfterRun(0)
	if bs.hubGroupBusy.Load() {
		t.Fatal("empty inbox must release the group latch")
	}
}

// P3b-07 B6：final 群回复把「游标窗口里的智能体发送者」写进 Message.Context。
func TestHubGroupFinalCarriesTurnContext(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "CtxA")
	b := createHubAgent(t, m, buf, 2, "CtxB")
	c := createHubAgent(t, m, buf, 3, "CtxC")
	g := groupWithMembers(t, m, buf, 4, "上下文群",
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"},
		struct {
			id   agenthub.AgentID
			role string
		}{b.ID, "member"},
		struct {
			id   agenthub.AgentID
			role string
		}{c.ID, "member"},
	)
	st := mustHubStore(t, m)
	ctx := context.Background()
	if _, err := st.AppendMessage(ctx, &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindText,
		Sender: agenthub.Sender{Kind: agenthub.SenderHuman}, Text: "@CtxB 看一下",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.AppendMessage(ctx, &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindText,
		Sender: agenthub.Sender{Kind: agenthub.SenderAgent, AgentID: c.ID}, Text: "C 的背景",
	}); err != nil {
		t.Fatal(err)
	}
	triggerSeq, err := st.AppendMessage(ctx, &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindText,
		Sender: agenthub.Sender{Kind: agenthub.SenderAgent, AgentID: a.ID}, Text: "@CtxB 复核一下",
	})
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := st.ReadMessages(ctx, g.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	turnCtx := agenthub.BuildTurnContext(b.ID, msgs, 0, triggerSeq)
	if turnCtx == nil || len(turnCtx.Senders) != 2 {
		t.Fatalf("precondition context = %+v, want A+C", turnCtx)
	}
	loaded, _, err := m.hubLoadAgent(string(b.ID))
	if err != nil {
		t.Fatal(err)
	}
	spec := m.hubSpecFor(loaded, agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID}, m.hubRuntime(string(b.ID)).path)
	spec.Group = g
	wake := &agenthub.GroupWakeHeader{
		GroupID: g.ID, AgentID: b.ID, RoundID: "rnd_ctx", TriggerSeq: triggerSeq,
	}
	// finishHubRound 自己从游标窗口重算 Context；这里不依赖 wake.Context。
	m.finishHubRound(spec, wake, "B 的最终回复", nil)

	msgs, err = st.ReadMessages(ctx, g.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	var reply *agenthub.Message
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Sender.Kind == agenthub.SenderAgent && msgs[i].Sender.AgentID == b.ID {
			reply = &msgs[i]
			break
		}
	}
	if reply == nil {
		t.Fatal("B 的 final 回复未落账")
	}
	if reply.Context == nil {
		t.Fatal("final 回复缺 Context")
	}
	got := map[agenthub.AgentID]bool{}
	for _, s := range reply.Context.Senders {
		got[s.AgentID] = true
	}
	if !got[a.ID] || !got[c.ID] || got[b.ID] {
		t.Fatalf("Context.Senders = %+v, want A+C (not self)", reply.Context.Senders)
	}
	if len(reply.Context.TriggerSeqs) != 2 {
		t.Fatalf("TriggerSeqs = %v, want 2 entries", reply.Context.TriggerSeqs)
	}
}

// P3b-07 B7：hub_group_get 一次返回 conversation + 人类已读游标。
func TestHubGroupGetHydratesConversationAndCursor(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "HydA")
	g := convGroupWith(t, m, buf, 2, "水合群", nil, a)
	p3bWireFake(t, m, g, string(a.ID), "收到")
	ok, errMsg, data := hubCmd(t, m, buf, 3, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@HydA 看一下",
	})
	if !ok {
		t.Fatalf("send: %s", errMsg)
	}
	seq := int64(0)
	if v, ok := data["seq"].(float64); ok {
		seq = int64(v)
	}
	if seq <= 0 {
		t.Fatalf("send seq = %v", data["seq"])
	}
	ok, errMsg, data = hubCmd(t, m, buf, 4, "hub_group_get", map[string]any{"group_id": string(g.ID)})
	if !ok {
		t.Fatalf("group_get: %s", errMsg)
	}
	conv, _ := data["conversation"].(map[string]any)
	if conv == nil {
		t.Fatalf("group_get missing conversation: %+v", data)
	}
	if conv["status"] != string(agenthub.ConversationRunning) {
		t.Fatalf("conversation.status = %v, want running", conv["status"])
	}
	if conv["turn_budget"] != float64(agenthub.UnlimitedTurnBudget) || conv["max_depth"] != float64(agenthub.DefaultConversationMaxDepth) {
		t.Fatalf("conversation limits = %+v", conv)
	}
	cursor, _ := data["cursor"].(map[string]any)
	if cursor == nil || int64(cursor["human_read_seq"].(float64)) != seq {
		t.Fatalf("cursor = %+v, want human_read_seq=%d", cursor, seq)
	}
}

// isolationGateProvider 把模型调用卡在闸门上：用来把 DM 与群两个 run **同时按住在飞**，
// 而不是拿 sleep 猜时序。release 之后才回 LLM 结束帧。
type isolationGateProvider struct {
	mu        sync.Mutex
	entered   int
	gate      chan struct{}
	enteredCh chan struct{}
	content   string
}

func newIsolationGateProvider(content string) *isolationGateProvider {
	return &isolationGateProvider{
		gate:      make(chan struct{}),
		enteredCh: make(chan struct{}, 4),
		content:   content,
	}
}

func (p *isolationGateProvider) Stream(ctx context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	p.mu.Lock()
	p.entered++
	p.mu.Unlock()
	select {
	case p.enteredCh <- struct{}{}:
	default:
	}
	select {
	case <-p.gate:
	case <-ctx.Done():
		return ctx.Err()
	}
	model := ""
	if req != nil {
		model = req.Model
	}
	return h(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: p.content, Model: model})
}

func (p *isolationGateProvider) waitEntered(t *testing.T) {
	t.Helper()
	select {
	case <-p.enteredCh:
	case <-time.After(20 * time.Second):
		t.Fatal("provider 没被调用：run 没起来")
	}
}

func (p *isolationGateProvider) release() string {
	close(p.gate)
	return p.content
}

// 场景 1-3：DM 与群并发唤醒同一智能体，互不抢 hubGroupBusy / hubGroupOwner /
// hubDispatchPending / run 状态；各自 AgentEnd 只写自己的索引/群日志。
func TestHubDMAndGroupRunsDoNotStealEachOthersLocks(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "ConcurrentIso")
	g := groupWithMembers(t, m, buf, 2, "并发隔离群", memberRef{a.ID, "owner"})
	agentID := string(a.ID)

	dm, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	grp, err := m.hubGroupSession(agentID, string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	dmGate := newIsolationGateProvider("DM-ONLY-并发回复")
	grpGate := newIsolationGateProvider("GROUP-ONLY-并发回复")
	setDMFakeLoop(t, m, dm, agentID, dmGate)
	setGroupFakeLoop(t, m, grp, g, agentID, grpGate)

	// 1) DM 先跑到模型里卡住。
	if ok, errMsg, _ := hubCmd(t, m, buf, 3, "hub_send", map[string]any{
		"target": map[string]any{"kind": "dm", "agent_id": agentID},
		"text":   "DM-ONLY-并发提问",
	}); !ok {
		t.Fatalf("dm send: %s", errMsg)
	}
	dmGate.waitEntered(t)
	if !dm.running.Load() {
		t.Fatal("DM run 未在跑")
	}
	// DM 的 run 不许碰群闩：群闩是「群任务已投、AgentEnd 未到」的每会话闩。
	if dm.hubGroupBusy.Load() || dm.hubGroupOwner.Load() != 0 || dm.hubGroupSeq.Load() != 0 {
		t.Fatalf("DM run 抢了群闩: busy=%v owner=%d seq=%d",
			dm.hubGroupBusy.Load(), dm.hubGroupOwner.Load(), dm.hubGroupSeq.Load())
	}
	if grp.hubGroupBusy.Load() {
		t.Fatal("群闩在 DM 运行期间就被占上了")
	}

	// 2) DM 还在飞的时候唤醒同一智能体的群会话：必须能进（群闩按会话，不按 agent）。
	if ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "GROUP-ONLY-并发提问", "mentions": []string{agentID},
	}); !ok {
		t.Fatalf("group send while dm run in flight: %s", errMsg)
	}
	grpGate.waitEntered(t)

	// 3) 两个 run 同时在飞，状态各自持有。
	if !grp.running.Load() {
		t.Fatal("群 run 未在跑")
	}
	if !grp.hubGroupBusy.Load() || grp.hubGroupOwner.Load() == 0 {
		t.Fatalf("群 run 没占自己的群闩: busy=%v owner=%d",
			grp.hubGroupBusy.Load(), grp.hubGroupOwner.Load())
	}
	if grp.hubGroupSeq.Load() == 0 || grp.hubGroupOwner.Load() > grp.hubGroupSeq.Load() {
		t.Fatalf("群闩代号异常: seq=%d owner=%d", grp.hubGroupSeq.Load(), grp.hubGroupOwner.Load())
	}
	// hub_send / hub_group_send 都不走「任务已 Ask 未开跑」的 pending 窗口。
	if dm.hubDispatchPending.Load() || grp.hubDispatchPending.Load() {
		t.Fatalf("hubDispatchPending 被发送路径占用: dm=%v grp=%v",
			dm.hubDispatchPending.Load(), grp.hubDispatchPending.Load())
	}
	// 正对照：两个 provider 都真的在飞（一个 release 之前谁都没结束）。
	if dmGate.enteredCount() != 1 || grpGate.enteredCount() != 1 {
		t.Fatalf("provider 调用次数 dm=%d grp=%d, want 1/1", dmGate.enteredCount(), grpGate.enteredCount())
	}

	// 4) 只放 DM：DM 落自己的索引，且不能顺手清掉群的闩。
	dmContent := dmGate.release()
	waitHubIndex(t, m, a.ID, 2)
	if !grp.hubGroupBusy.Load() {
		t.Fatal("DM 的 AgentEnd 把群会话的 busy 清掉了（跨会话清闩）")
	}
	if grpGate.enteredCount() != 1 {
		t.Fatal("DM 收尾期间群 run 被重启/重入了")
	}

	// 5) 放群：群 final 落群日志，闩自行释放。
	grpContent := grpGate.release()
	waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		for _, msg := range msgs {
			if msg.Sender.Kind == agenthub.SenderAgent && strings.Contains(msg.Text, grpContent) {
				return true
			}
		}
		return false
	})
	p404aWaitGroupIdle(t, grp)
	if grp.hubGroupOwner.Load() != 0 {
		t.Fatalf("群 run 结束后 owner = %d, want 0", grp.hubGroupOwner.Load())
	}

	// 6) 落账归属：各写各的索引/群日志，内容不互相串。
	fst, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	dmMsgs, err := fst.ReadDirectMessages(context.Background(), a.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(dmMsgs) != 2 {
		t.Fatalf("DM 索引 = %d 条, want 2（提问 + 回复）: %+v", len(dmMsgs), dmMsgs)
	}
	if dmMsgs[0].Text != "DM-ONLY-并发提问" || dmMsgs[1].Text != dmContent {
		t.Fatalf("DM 索引内容错位: %+v", dmMsgs)
	}
	for _, msg := range dmMsgs {
		if strings.Contains(msg.Text, "GROUP-ONLY") {
			t.Fatalf("群内容进了 DM 索引: %+v", msg)
		}
	}
	grpMsgs, err := fst.ReadMessages(context.Background(), g.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	var agentMsgs []agenthub.Message
	for _, msg := range grpMsgs {
		if msg.Sender.Kind == agenthub.SenderAgent {
			agentMsgs = append(agentMsgs, msg)
		}
		if strings.Contains(msg.Text, "DM-ONLY") {
			t.Fatalf("DM 内容进了群日志: %+v", msg)
		}
	}
	if len(agentMsgs) != 1 || agentMsgs[0].Text != grpContent {
		t.Fatalf("群日志智能体消息 = %+v, want 恰好一条 %q", agentMsgs, grpContent)
	}
	if !dm.running.Load() == false {
		t.Fatal("DM run 结束后 running 仍为真")
	}
}

// enteredCount 读 provider 已进入次数（并发断言用）。
func (p *isolationGateProvider) enteredCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.entered
}

// 场景 1-5：hub_message / hub_message_delta 的作用域字段 —— 每条 hub_message 恰好
// 属于一个桶（DM 带 agent_id 不带 group_id；群带 group_id 不带 agent_id），
// 空 group_id 会把事件漏进前端 DM 分支（useHubStore.ts:1409），因此必须完全不出现。
func TestHubHubMessageEventsCarryOwnScopeOnly(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "EvtA")
	g := groupWithMembers(t, m, buf, 2, "事件隔离群", memberRef{a.ID, "owner"})
	agentID := string(a.ID)

	// DM 一轮（流式 fake：场景2b 起 DM 也按 LLM 轮次分段并发 delta）。
	const dmFinal = "DM-ONLY-事件回复"
	dm, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	setDMFakeLoop(t, m, dm, agentID, newP4DeltaProvider(dmFinal, "DM-ONLY-", "事件", "回复"))
	if ok, errMsg, _ := hubCmd(t, m, buf, 3, "hub_send", map[string]any{
		"target": map[string]any{"kind": "dm", "agent_id": agentID},
		"text":   "DM-ONLY-事件提问",
	}); !ok {
		t.Fatalf("dm send: %s", errMsg)
	}
	waitHubIndex(t, m, a.ID, 2)

	// 群一轮（流式 fake：产生 hub_message_delta）。
	grp, err := m.hubGroupSession(agentID, string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	const groupFinal = "GROUP-ONLY-流式最终正文"
	setGroupFakeLoop(t, m, grp, g, agentID, newP4DeltaProvider(groupFinal, "GROUP-ONLY-", "流式最终", "正文"))
	if ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "GROUP-ONLY-事件提问", "mentions": []string{agentID},
	}); !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		for _, msg := range msgs {
			if msg.Sender.Kind == agenthub.SenderAgent && msg.Text == groupFinal {
				return true
			}
		}
		return false
	})
	p404aWaitGroupIdle(t, grp)

	// hub_message：两个桶严格互斥。
	dmEvents, grpEvents := 0, 0
	for _, ev := range hubEvents(t, buf, "hub_message") {
		gid, _ := ev["group_id"].(string)
		aid, _ := ev["agent_id"].(string)
		switch {
		case gid == "" && aid == "":
			t.Fatalf("hub_message 既没有 group_id 也没有 agent_id，前端两个桶都进不去: %v", ev)
		case gid != "" && aid != "":
			t.Fatalf("hub_message 同时带 group_id/agent_id，前端会串桶: %v", ev)
		case gid != "":
			if gid != string(g.ID) {
				t.Fatalf("群 hub_message 的 group_id = %q, want %q", gid, g.ID)
			}
			grpEvents++
		default:
			if aid != agentID {
				t.Fatalf("DM hub_message 的 agent_id = %q, want %q", aid, agentID)
			}
			dmEvents++
		}
	}
	if dmEvents != 2 {
		t.Fatalf("DM hub_message = %d, want 2（人发 + agent 回复）", dmEvents)
	}
	if grpEvents < 2 {
		t.Fatalf("群 hub_message = %d, want ≥2（人发 + agent final）", grpEvents)
	}

	// hub_message_delta：两侧都有草稿流（场景2b：DM 也按 LLM 轮次分段），作用域字段
	// 严格二分 —— DM 的 group_id/round_id 是**空串**（不是缺字段），群是本群 id + 轮次 id。
	dmDelta, grpDelta := 0, 0
	var dmJoined, grpJoined strings.Builder
	dmDeltaIDs, grpDeltaIDs := map[string]bool{}, map[string]bool{}
	for _, ev := range hubEvents(t, buf, "hub_message_delta") {
		gid, _ := ev["group_id"].(string)
		aid, _ := ev["agent_id"].(string)
		rid, _ := ev["round_id"].(string)
		id := str(ev, "message_id")
		if aid != agentID {
			t.Fatalf("hub_message_delta 的 agent_id = %q, want %q: %v", aid, agentID, ev)
		}
		if id == "" {
			t.Fatalf("hub_message_delta 缺 message_id: %v", ev)
		}
		switch gid {
		case "":
			if rid != "" {
				t.Fatalf("DM delta 带了 round_id=%q（群轮次字段串进单聊）: %v", rid, ev)
			}
			dmDelta++
			dmDeltaIDs[id] = true
			dmJoined.WriteString(str(ev, "delta"))
		case string(g.ID):
			if rid == "" {
				t.Fatalf("群 delta 缺 round_id: %v", ev)
			}
			grpDelta++
			grpDeltaIDs[id] = true
			grpJoined.WriteString(str(ev, "delta"))
		default:
			t.Fatalf("delta 的 group_id = %q，既不是空（DM）也不是本群 %q", gid, g.ID)
		}
	}
	if dmDelta == 0 {
		t.Fatal("DM 没有 hub_message_delta（场景2b 起 DM 也要按轮次流式）")
	}
	if grpDelta == 0 {
		t.Fatal("群没有 hub_message_delta（流式草稿链路断了）")
	}
	if dmJoined.String() != dmFinal || grpJoined.String() != groupFinal {
		t.Fatalf("delta 拼接错位: dm=%q grp=%q, want %q / %q",
			dmJoined.String(), grpJoined.String(), dmFinal, groupFinal)
	}
	if len(dmDeltaIDs) != 1 || len(grpDeltaIDs) != 1 {
		t.Fatalf("delta 落点 = dm:%v grp:%v, want 各 1 条消息", dmDeltaIDs, grpDeltaIDs)
	}
	for id := range dmDeltaIDs {
		if grpDeltaIDs[id] {
			t.Fatalf("DM 与群的 delta 落在同一个 message_id %q：前端两条草稿会互相覆盖", id)
		}
	}
	// 草稿→final 的替换必须留在本桶：每个 delta 的 message_id 只能出现在同桶的
	// hub_message 里（跨桶出现就是串话）。
	for _, ev := range hubEvents(t, buf, "hub_message") {
		msg, _ := ev["message"].(map[string]any)
		if msg == nil {
			continue
		}
		id, _ := msg["id"].(string)
		if id == "" {
			continue
		}
		gid, _ := ev["group_id"].(string)
		if gid == "" {
			if grpDeltaIDs[id] {
				t.Fatalf("群的 delta message_id 出现在 DM hub_message: %v", ev)
			}
			if dmDeltaIDs[id] {
				dmDeltaIDs[id] = false // 标记「已被 DM final 替换」
			}
			continue
		}
		if dmDeltaIDs[id] {
			t.Fatalf("DM 的 delta message_id 出现在群 hub_message: %v", ev)
		}
		if grpDeltaIDs[id] {
			grpDeltaIDs[id] = false
		}
	}
	for id, pending := range dmDeltaIDs {
		if pending {
			t.Fatalf("DM delta 的 message_id %q 没有对应的 DM final hub_message（草稿永不被替换）", id)
		}
	}
	for id, pending := range grpDeltaIDs {
		if pending {
			t.Fatalf("群 delta 的 message_id %q 没有对应的群 final hub_message", id)
		}
	}
}
