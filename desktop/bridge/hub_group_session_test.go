package main

// P3b-01a：群会话懒装配 / @ 唤醒 / final hub_message / 游标推进。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/agents"
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

func setGroupFakeLoop(t *testing.T, m *manager, bs *bridgeSession, g *agenthub.Group, agentID string, fake *evalProvider) {
	t.Helper()
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
		agents.WithMaxIterations(1),
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
	m.finishHubRound(spec, wake, report)
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
	bs.hubGroupBusy.Store(true) // 模拟上一轮还没 AgentEnd
	defer bs.hubGroupBusy.Store(false)

	ok, errMsg, data := hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id": string(g.ID),
		"text":     "@BusyB 第二条",
	})
	if !ok {
		t.Fatalf("message itself must still be accepted: %s", errMsg)
	}
	errs, _ := data["wake_errors"].([]any)
	if len(errs) == 0 {
		t.Fatal("busy target must be reported in wake_errors")
	}
	// 群消息必须已经落日志（唤醒失败不回滚）。
	st := mustHubStore(t, m)
	msgs, err := st.ReadMessages(context.Background(), g.ID, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) == 0 {
		t.Fatal("group message missing after wake failure")
	}
}

func TestFinishHubRoundTurnLimitStopsAutoChain(t *testing.T) {
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
	// 人类之后已有 8 条智能体发言（默认 MaxTurnsPerHuman=8）。
	for i := 0; i < agenthub.DefaultGroupMaxTurnsPerHuman; i++ {
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
	m.finishHubRound(spec, wake, "再补一条")
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
	if agentMsgs != agenthub.DefaultGroupMaxTurnsPerHuman {
		t.Fatalf("agent messages = %d, want %d (over-limit final must not append)", agentMsgs, agenthub.DefaultGroupMaxTurnsPerHuman)
	}
	cur, err := st.LoadCursor(context.Background(), g.ID, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cur.AgentContextSeq != trigger {
		t.Fatalf("agent_context_seq = %d, want %d even when limited", cur.AgentContextSeq, trigger)
	}
}

func TestClearHubGroupBusyOnlyOwnEpoch(t *testing.T) {
	bs := &bridgeSession{}
	bs.hubGroupBusy.Store(true)
	bs.hubGroupBusySeq.Store(2)
	bs.clearHubGroupBusy(1) // 旧 run 的兜底清理
	if !bs.hubGroupBusy.Load() {
		t.Fatal("stale run must not clear a newer group latch")
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
	bs.hubGroupBusySeq.Store(9)
	bs.cancel()
	if _, err := m.hubGroupSession(string(a.ID), string(g.ID)); err == nil {
		t.Fatal("closed group session must not accept a new wake")
	}
	if bs.hubGroupBusy.Load() || bs.hubGroupBusySeq.Load() != 0 {
		t.Fatal("closed group session must release the busy latch")
	}
}
