package main

// P3a-10：评测 7（方案 §10.3）——群组 @、历史、私有上下文、流式一致性。
//
//  1. @all 唤醒除发送者外的全部成员，每个被唤醒的成员各写一条 final；hub_round 事件的
//     payload（round.id/trigger_seq/targets）与响应一致；
//  2. @A 只唤醒 A：B/C 没有 agent 消息（群唤醒发号仍为 0），游标也不动；hub_round 事件 targets 只有 A；
//  3. 无 @：只写日志，不写路由回执、不发 hub_round、不唤醒（响应里的 round 仍存在，targets 为空）；
//  4. 群历史能翻到早期消息（命令分页 + group_history 工具的 before_seq/search）；
//  5. owner-only 与 approval_stub 不进非 owner 的快照，也不进真实唤醒请求；
//  6. 两个成员同时流式：草稿各自一个 message_id，final 与 delta 同 ID、正文一致，
//     每条 final 只发一次事件，且事件 payload 与落库 final 逐字段一致（id/text/seq/
//     round_id/group_id/sender），最后一段 delta 必须先于 final 事件。P3a-09 的 80ms
//     合并/顺序/迟到 delta 由 hub_events_test.go 与 scripts/test-hub-stream.mjs 覆盖，
//     这里只补评测级断言。
//
// 全部走确定性 fake provider（不碰网络）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

// eval7GroupFixture 建 n 个智能体（第一个 owner，其余 member）和一个群。
func eval7GroupFixture(t *testing.T, m *manager, buf *syncBuf, cmdID int, groupName string, names ...string) ([]agenthub.AgentProfile, *agenthub.Group) {
	t.Helper()
	agents := make([]agenthub.AgentProfile, 0, len(names))
	for i, name := range names {
		agents = append(agents, createHubAgent(t, m, buf, cmdID+i, name))
	}
	members := make([]struct {
		id   agenthub.AgentID
		role string
	}, 0, len(agents))
	for i, a := range agents {
		role := "member"
		if i == 0 {
			role = "owner"
		}
		members = append(members, struct {
			id   agenthub.AgentID
			role string
		}{a.ID, role})
	}
	return agents, groupWithMembers(t, m, buf, cmdID+len(agents), groupName, members...)
}

// eval7WireGroup 预装配成员的群会话并换成确定性 fake loop。
// 不预装配的话唤醒路径会建真实 provider loop，评测就会碰网络。
func eval7WireGroup(t *testing.T, m *manager, g *agenthub.Group, agentID agenthub.AgentID, fake provider.Provider) *bridgeSession {
	t.Helper()
	bs, err := m.hubGroupSession(string(agentID), string(g.ID))
	if err != nil {
		t.Fatalf("group session for %s: %v", agentID, err)
	}
	setGroupFakeLoop(t, m, bs, g, string(agentID), fake)
	return bs
}

func eval7AgentMessages(msgs []agenthub.Message, id agenthub.AgentID) []agenthub.Message {
	var out []agenthub.Message
	for _, msg := range msgs {
		if msg.Sender.Kind == agenthub.SenderAgent && msg.Sender.AgentID == id {
			out = append(out, msg)
		}
	}
	return out
}

func eval7ReadGroup(t *testing.T, m *manager, gid agenthub.GroupID) []agenthub.Message {
	t.Helper()
	msgs, err := mustHubStore(t, m).ReadMessages(context.Background(), gid, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

func eval7RoundOf(t *testing.T, data map[string]any) (id string, mentionAll bool, targets []agenthub.AgentID) {
	t.Helper()
	raw, err := json.Marshal(data["round"])
	if err != nil {
		t.Fatalf("round 字段不可序列化: %v", err)
	}
	var round struct {
		ID         string             `json:"id"`
		MentionAll bool               `json:"mention_all"`
		Targets    []agenthub.AgentID `json:"targets"`
	}
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("round payload = %s: %v", raw, err)
	}
	return round.ID, round.MentionAll, round.Targets
}

func eval7HistoryPage(t *testing.T, data map[string]any) ([]agenthub.Message, bool) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("history payload 不可序列化: %v", err)
	}
	var page struct {
		Messages []agenthub.Message `json:"messages"`
		HasMore  bool               `json:"has_more"`
	}
	if err := json.Unmarshal(raw, &page); err != nil {
		t.Fatalf("history payload = %s: %v", raw, err)
	}
	return page.Messages, page.HasMore
}

func eval7HasText(msgs []agenthub.Message, text string) bool {
	for _, msg := range msgs {
		if strings.Contains(msg.Text, text) {
			return true
		}
	}
	return false
}

// eval7EventMessage 取出 hub_message 事件的 message 对象（必须存在且非 null）。
// 前端直接消费 ev.message：payload 缺失时不该被当成「没有这条事件」静默跳过。
func eval7EventMessage(t *testing.T, ev map[string]any) map[string]any {
	t.Helper()
	msg, ok := ev["message"].(map[string]any)
	if !ok || msg == nil {
		t.Fatalf("hub_message 事件缺 message 对象: %+v", ev)
	}
	return msg
}

// eval7FinalEvent 断言某个 final 恰好一条 hub_message 事件，并把 payload 与落库的
// final 逐字段对齐（id/text/seq/round_id/group_id/sender）。
//
// 为什么不能只数条数：前端 useHubStore 用 ev.message 把草稿转成最终消息、并按 seq
// 排序展示，事件正文或序号被改坏时「事件数 == 1」仍然是绿的，用户却看到错正文/乱序。
func eval7FinalEvent(t *testing.T, buf *syncBuf, gid agenthub.GroupID, roundID string, agentID agenthub.AgentID, final *agenthub.Message) {
	t.Helper()
	var found map[string]any
	var envelope map[string]any
	events := 0
	for _, ev := range hubEvents(t, buf, "hub_message") {
		msg := eval7EventMessage(t, ev)
		if msg["id"] != final.ID {
			continue
		}
		events++
		envelope, found = ev, msg
	}
	if events != 1 {
		t.Fatalf("%s final %s 的 hub_message 事件 = %d，want 1（前端按 id 替换草稿，重复会插两条）", agentID, final.ID, events)
	}
	// 事件信封的 seq 与 message.seq 都要是落库序号：前端按 message.seq 排序展示，
	// 事件信封的 seq 是同一份数据的另一处出口，任何一处归零都说明 payload 没对齐。
	if got := numOr(envelope, "seq", -1); got != final.Seq {
		t.Fatalf("%s final 事件 seq = %v，落库 = %d", agentID, envelope["seq"], final.Seq)
	}
	if found["text"] != final.Text {
		t.Fatalf("%s final 事件 text = %v，落库 = %q", agentID, found["text"], final.Text)
	}
	if got := numOr(found, "seq", -1); got != final.Seq {
		t.Fatalf("%s final 事件 message.seq = %v，落库 = %d", agentID, found["seq"], final.Seq)
	}
	if final.RoundID != roundID {
		t.Fatalf("%s 落库 final round_id = %q，want %q", agentID, final.RoundID, roundID)
	}
	if found["round_id"] != roundID {
		t.Fatalf("%s final 事件 round_id = %v，want %q", agentID, found["round_id"], roundID)
	}
	if found["group_id"] != string(gid) {
		t.Fatalf("%s final 事件 group_id = %v，want %s", agentID, found["group_id"], gid)
	}
	sender, ok := found["sender"].(map[string]any)
	if !ok || sender == nil {
		t.Fatalf("%s final 事件 sender 不是对象: %v", agentID, found["sender"])
	}
	// 字面量 "agent"：这是 stdout 上的 wire 形状，前端按它分支，不能随内部常量漂移。
	if sender["kind"] != "agent" || sender["agent_id"] != string(agentID) {
		t.Fatalf("%s final 事件 sender = %+v，want kind=agent agent_id=%s", agentID, sender, agentID)
	}
}

// eval7RoundEvent 断言 hub_round 事件恰好一条，并核对 group/round/trigger 归属。
// 前端看事件里的 round（targets 为空就不提示「谁被唤醒」），事件与响应是两套 payload。
func eval7RoundEvent(t *testing.T, buf *syncBuf, gid agenthub.GroupID, roundID string, triggerSeq int64) agenthub.Round {
	t.Helper()
	evs := hubEvents(t, buf, "hub_round")
	if len(evs) != 1 {
		t.Fatalf("hub_round 事件 = %d 条，want 1: %+v", len(evs), evs)
	}
	if evs[0]["group_id"] != string(gid) {
		t.Fatalf("hub_round group_id = %v，want %s", evs[0]["group_id"], gid)
	}
	raw, err := json.Marshal(evs[0]["round"])
	if err != nil {
		t.Fatalf("hub_round.round 不可序列化: %v", err)
	}
	var round agenthub.Round
	if err := json.Unmarshal(raw, &round); err != nil {
		t.Fatalf("hub_round.round = %s: %v", raw, err)
	}
	if round.ID != roundID || round.GroupID != gid || round.TriggerSeq != triggerSeq {
		t.Fatalf("hub_round payload = %+v，want id %s group %s trigger %d", round, roundID, gid, triggerSeq)
	}
	return round
}

// eval7ToolHistory 解 group_history 工具的 JSON 输出（{"messages":[...],"count":N}）。
// 用 count 字段断言条数：字符串匹配 "count":1 会让 count:10 也满足。
func eval7ToolHistory(t *testing.T, out string) ([]agenthub.Message, int) {
	t.Helper()
	var page struct {
		Messages []agenthub.Message `json:"messages"`
		Count    int                `json:"count"`
	}
	if err := json.Unmarshal([]byte(out), &page); err != nil {
		t.Fatalf("group_history 输出不是 JSON: %v\n%s", err, out)
	}
	return page.Messages, page.Count
}

// 评测 7-1：@all 唤醒全部其他成员；每个成员一条 final；人只有触发消息。
func TestEval7MentionAllWakesEveryOtherMember(t *testing.T) {
	m, buf := testManager(t)
	agents, g := eval7GroupFixture(t, m, buf, 1, "评测7全群", "Eval7AllA", "Eval7AllB", "Eval7AllC")
	replies := []string{"A 收到", "B 收到", "C 收到"}
	for i, a := range agents {
		eval7WireGroup(t, m, g, a.ID, &evalProvider{reply: replies[i]})
	}

	ok, errMsg, data := hubCmd(t, m, buf, 40, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@all 每人回一句",
	})
	if !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	roundID, mentionAll, targets := eval7RoundOf(t, data)
	if roundID == "" || !mentionAll {
		t.Fatalf("round = %+v，want @all round", data["round"])
	}
	triggerSeq := numOr(data, "seq", 0)
	if len(targets) != len(agents) {
		t.Fatalf("@all targets = %v，want 全部 %d 名成员", targets, len(agents))
	}
	hit := map[agenthub.AgentID]bool{}
	for _, id := range targets {
		hit[id] = true
	}
	for _, a := range agents {
		if !hit[a.ID] {
			t.Fatalf("@all targets = %v，缺少 %s", targets, a.ID)
		}
	}

	msgs := waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		for i, a := range agents {
			if !eval7HasText(eval7AgentMessages(msgs, a.ID), replies[i]) {
				return false
			}
		}
		return true
	})
	for i, a := range agents {
		own := eval7AgentMessages(msgs, a.ID)
		if len(own) != 1 {
			t.Fatalf("%s final 数 = %d，want 1（@all 每人只回一条）", a.ID, len(own))
		}
		if own[0].Text != replies[i] || own[0].RoundID != roundID {
			t.Fatalf("%s final = %+v，want text %q round %q", a.ID, own[0], replies[i], roundID)
		}
	}
	// 触发者是人：日志里只有一条 human 消息（触发本身），没有以 human 身份冒出的 agent 回复。
	var human int
	for _, msg := range msgs {
		if msg.Sender.Kind == agenthub.SenderHuman {
			human++
		}
	}
	if human != 1 {
		t.Fatalf("human 消息数 = %d，want 1（发送者不该被当成 agent 回复）", human)
	}
	// 人触发 + 路由回执 + 3 条 final：多出来的就是重复 final 或幽灵发送者。
	if len(msgs) != 1+1+len(agents) {
		t.Fatalf("群日志 %d 条，want %d: %+v", len(msgs), 2+len(agents), msgs)
	}
	// 每个成员的游标都只推进到本轮触发消息：@all 的唤醒 header 不能串到别人身上。
	// 消息先落、游标后写（不在一个事务里），所以等游标而不是立刻读。
	st := mustHubStore(t, m)
	if triggerSeq == 0 {
		t.Fatal("send response 缺 seq")
	}
	for _, a := range agents {
		var cur *agenthub.Cursor
		var err error
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if cur, err = st.LoadCursor(context.Background(), g.ID, a.ID); err == nil && cur.AgentContextSeq == triggerSeq {
				break
			}
			time.Sleep(5 * time.Millisecond)
		}
		if err != nil || cur.AgentContextSeq != triggerSeq {
			t.Fatalf("%s 游标 = %+v (%v)，want trigger %d（唤醒归属串台）", a.ID, cur, err, triggerSeq)
		}
	}
	// hub_round 事件 payload 也要对：前端展示「谁被唤醒」读的是事件里的 round，
	// 只对响应对不上事件就是两套事实（曾漏过只改事件的变异）。
	evRound := eval7RoundEvent(t, buf, g.ID, roundID, triggerSeq)
	if !evRound.MentionAll {
		t.Fatalf("hub_round 事件 mention_all = false，want true: %+v", evRound)
	}
	// 事件与响应的 targets 必须是同一份（条数相等 + 无响应之外的成员 = 集合相等）。
	if len(evRound.Targets) != len(targets) {
		t.Fatalf("hub_round 事件 targets = %v，响应 = %v（两套 payload 必须一致）", evRound.Targets, targets)
	}
	for _, id := range evRound.Targets {
		if !hit[id] {
			t.Fatalf("hub_round 事件 targets = %v，含响应之外的 %s", evRound.Targets, id)
		}
	}
}

// 评测 7-2：@A 只唤醒 A；其余成员没有 agent 消息（唤醒发号仍为 0）、游标不动。
func TestEval7MentionOneWakesOnlyThatMember(t *testing.T) {
	m, buf := testManager(t)
	agents, g := eval7GroupFixture(t, m, buf, 1, "评测7单点", "Eval7OneA", "Eval7OneB", "Eval7OneC")
	// 三个都装 fake loop：任何误唤醒都会留下可断言的 agent 消息，而不是静默失败。
	sessions := make([]*bridgeSession, len(agents))
	for i, a := range agents {
		sessions[i] = eval7WireGroup(t, m, g, a.ID, &evalProvider{reply: fmt.Sprintf("冒泡-%d", i)})
	}

	ok, errMsg, data := hubCmd(t, m, buf, 40, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@Eval7OneB 只回你",
	})
	if !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	roundID, mentionAll, targets := eval7RoundOf(t, data)
	if mentionAll || len(targets) != 1 || targets[0] != agents[1].ID {
		t.Fatalf("targets = %v (mention_all=%v)，want 只有 %s", targets, mentionAll, agents[1].ID)
	}
	triggerSeq := numOr(data, "seq", 0)
	if triggerSeq == 0 {
		t.Fatal("send response 缺 seq")
	}

	msgs := waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		return eval7HasText(eval7AgentMessages(msgs, agents[1].ID), "冒泡-1")
	})
	// 等 target 的 run 收口（闩清零 = final 已落库）：用它替代固定 300ms 静默窗口，
	// 既确定又不拖慢。未 @ 成员根本没有 run，下面的断言不需要窗口。
	waitHubGroupIdle(t, sessions[1])
	msgs = eval7ReadGroup(t, m, g.ID)
	for _, i := range []int{0, 2} {
		a := agents[i]
		// 确定性证据：wakeGroupAgent 在 send 返回前同步发号（永不回 0），
		// 所以 hubGroupSeq==0 就是「从未被唤醒」——不靠静默窗口猜。
		if got := sessions[i].hubGroupSeq.Load(); got != 0 {
			t.Fatalf("%s 未被 @，唤醒发号却 = %d（说明被唤醒过）", a.ID, got)
		}
		if got := eval7AgentMessages(msgs, a.ID); len(got) != 0 {
			t.Fatalf("%s 未被 @，却写了 agent 消息: %+v", a.ID, got)
		}
		cur, err := mustHubStore(t, m).LoadCursor(context.Background(), g.ID, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.AgentContextSeq != 0 {
			t.Fatalf("%s 未被 @，游标却推进到 %d（说明被唤醒过）", a.ID, cur.AgentContextSeq)
		}
		if sessions[i].hubGroupBusy.Load() {
			t.Fatalf("%s 未被 @，群会话却处于 busy", a.ID)
		}
	}
	if got := sessions[1].hubGroupSeq.Load(); got != 1 {
		t.Fatalf("target 唤醒发号 = %d，want 恰好 1 次", got)
	}
	own := eval7AgentMessages(msgs, agents[1].ID)
	if len(own) != 1 || own[0].Text != "冒泡-1" || own[0].RoundID != roundID {
		t.Fatalf("target final = %+v，want 1 条 text %q round %q", own, "冒泡-1", roundID)
	}
	if sessions[1].hubGroupBusy.Load() {
		t.Fatal("target 已 final，群会话仍 busy")
	}
	// 回执只写被唤醒的 A：其他成员不能被写成 target。
	acts, err := mustHubStore(t, m).ReadActivity(context.Background(), g.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 1 || !strings.Contains(acts[0].Summary, string(agents[1].ID)) {
		t.Fatalf("routing 回执 = %+v，want 只含 %s", acts, agents[1].ID)
	}
	if strings.Contains(acts[0].Summary, string(agents[0].ID)) || strings.Contains(acts[0].Summary, string(agents[2].ID)) {
		t.Fatalf("routing 回执把未 @ 的成员写进了 targets: %s", acts[0].Summary)
	}
	// 事件 payload 同样只能有 B：前端按 hub_round 里的 targets 提示唤醒对象。
	evRound := eval7RoundEvent(t, buf, g.ID, roundID, triggerSeq)
	if evRound.MentionAll || len(evRound.Targets) != 1 || evRound.Targets[0] != agents[1].ID {
		t.Fatalf("hub_round 事件 targets = %v (mention_all=%v)，want 只有 %s", evRound.Targets, evRound.MentionAll, agents[1].ID)
	}
}

// 评测 7-3：无 @ 只写日志，不产生回执/hub_round/唤醒，也没有 agent 消息。
func TestEval7NoMentionDoesNotWake(t *testing.T) {
	m, buf := testManager(t)
	agents, g := eval7GroupFixture(t, m, buf, 1, "评测7无at", "Eval7NoAtA", "Eval7NoAtB")
	sessions := make([]*bridgeSession, len(agents))
	for i, a := range agents {
		sessions[i] = eval7WireGroup(t, m, g, a.ID, &evalProvider{reply: "不该出现"})
	}

	ok, errMsg, data := hubCmd(t, m, buf, 40, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "只记录，不点名",
	})
	if !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	// 响应为了兼容前端仍带 round 字段（前端 roundHasAction 会过滤空 targets）：
	// 断言它**存在且非 null**，否则 eval7RoundOf 的零值会让「字段被删掉」也撞对。
	if rawRound, has := data["round"]; !has || rawRound == nil {
		t.Fatalf("无 @ 响应缺 round 字段（前端兼容依赖它）: %+v", data)
	}
	// 但本轮不该落回执、不该发 hub_round 事件、不该唤醒。
	roundID, mentionAll, targets := eval7RoundOf(t, data)
	if roundID == "" || mentionAll || len(targets) != 0 {
		t.Fatalf("无 @ round = %q (mention_all=%v targets=%v)，want 兼容用的空中继", roundID, mentionAll, targets)
	}
	if _, hasWakeErrors := data["wake_errors"]; hasWakeErrors {
		t.Fatalf("无 @ 不该尝试唤醒，response 却带 wake_errors: %+v", data["wake_errors"])
	}
	if evs := hubEvents(t, buf, "hub_round"); len(evs) != 0 {
		t.Fatalf("无 @ 产生了 hub_round 事件: %+v", evs)
	}
	msgs := eval7ReadGroup(t, m, g.ID)
	if len(msgs) != 1 || msgs[0].Sender.Kind != agenthub.SenderHuman || msgs[0].Text != "只记录，不点名" {
		t.Fatalf("群日志 = %+v，want 只有人的那一条（无路由回执）", msgs)
	}
	acts, err := mustHubStore(t, m).ReadActivity(context.Background(), g.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(acts) != 0 {
		t.Fatalf("无 @ 写了 routing activity: %+v", acts)
	}
	for i, a := range agents {
		// 确定性证据：任何唤醒都会在 send 返回前把 hubGroupSeq +1（永不回 0），
		// 0 即「没被唤醒过」——不再用 300ms 静默窗口兜底。
		if got := sessions[i].hubGroupSeq.Load(); got != 0 {
			t.Fatalf("%s 无 @ 却被唤醒（hubGroupSeq = %d）", a.ID, got)
		}
		if got := eval7AgentMessages(msgs, a.ID); len(got) != 0 {
			t.Fatalf("%s 无 @ 却发言: %+v", a.ID, got)
		}
		cur, err := mustHubStore(t, m).LoadCursor(context.Background(), g.ID, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if cur.AgentContextSeq != 0 {
			t.Fatalf("%s 无 @ 游标却动了: %d", a.ID, cur.AgentContextSeq)
		}
		if sessions[i].hubGroupBusy.Load() {
			t.Fatalf("%s 无 @ 却处于 busy", a.ID)
		}
	}
}

// 评测 7-4：群历史可检索早期消息，不是只拿得到最新一页。
func TestEval7HistoryReachesEarlyMessages(t *testing.T) {
	m, buf := testManager(t)
	agents, g := eval7GroupFixture(t, m, buf, 1, "评测7历史", "Eval7HistA")
	ctx := context.Background()
	const early = "EARLY-MARKER-项目起点"
	for i := 1; i <= 40; i++ {
		text := fmt.Sprintf("历史消息 %03d", i)
		if i == 1 {
			text = early
		}
		if _, err := agenthub.AppendGroupMessage(ctx, mustHubStore(t, m), &agenthub.Message{
			GroupID: g.ID, Kind: agenthub.KindText,
			Sender: agenthub.Sender{Kind: agenthub.SenderHuman}, Text: text,
		}); err != nil {
			t.Fatal(err)
		}
	}

	// ① 人的分页：最新页看不到第 1 条，靠 before_seq 一页页翻回去。
	ok, errMsg, data := hubCmd(t, m, buf, 10, "hub_group_history", map[string]any{
		"group_id": string(g.ID), "limit": 10,
	})
	if !ok {
		t.Fatalf("history: %s", errMsg)
	}
	page, hasMore := eval7HistoryPage(t, data)
	if len(page) != 10 || !hasMore {
		t.Fatalf("latest page = %d 条 (has_more=%v)，want 10 条且有更早", len(page), hasMore)
	}
	if eval7HasText(page, early) {
		t.Fatal("最新一页就包含第 1 条：断言退化，翻页没被验证")
	}
	found := false
	for i := 0; i < 8 && !found; i++ {
		before := page[0].Seq
		ok, errMsg, data = hubCmd(t, m, buf, 11+i, "hub_group_history", map[string]any{
			"group_id": string(g.ID), "limit": 10, "before_seq": before,
		})
		if !ok {
			t.Fatalf("history page %d: %s", i, errMsg)
		}
		page, _ = eval7HistoryPage(t, data)
		if len(page) == 0 {
			break
		}
		if page[0].Seq >= before {
			t.Fatalf("before_seq=%d 没往前翻（第一页 seq=%d）", before, page[0].Seq)
		}
		found = eval7HasText(page, early)
	}
	if !found {
		t.Fatal("before_seq 翻页始终到不了第 1 条群消息")
	}

	// ② 模型面：group_history 工具同样支持 before_seq 与 query 检索全量历史。
	bs, err := m.hubDMSession(string(agents[0].ID))
	if err != nil {
		t.Fatal(err)
	}
	call := func(args string) string {
		t.Helper()
		tool, err := bs.hubEngine.GetTool(context.Background(), "group_history")
		if err != nil {
			t.Fatalf("group_history tool: %v", err)
		}
		out, err := tool.Call(context.Background(), "group_history", args)
		if err != nil {
			t.Fatalf("group_history(%s): %v", args, err)
		}
		return out
	}
	out := call(`{"group_id":"` + string(g.ID) + `","before_seq":11,"limit":10}`)
	if !strings.Contains(out, early) {
		t.Fatalf("group_history before_seq 没翻到早期消息: %s", out)
	}
	out = call(`{"group_id":"` + string(g.ID) + `","query":"early-marker"}`)
	hitMsgs, count := eval7ToolHistory(t, out)
	if count != 1 || len(hitMsgs) != 1 || !strings.Contains(hitMsgs[0].Text, early) {
		t.Fatalf("group_history query: count=%d messages=%+v，want 只有第 1 条早期消息: %s", count, hitMsgs, out)
	}
}

// 评测 7-5：owner-only 与 approval_stub 不进非 owner 的群上下文（快照 + 真实唤醒请求）。
func TestEval7PrivateContentStaysOutOfGroupContext(t *testing.T) {
	m, buf := testManager(t)
	agents, g := eval7GroupFixture(t, m, buf, 1, "评测7隐私", "Eval7PrivA", "Eval7PrivB")
	member := agents[1]
	ctx := context.Background()
	const ownerSecret = "OWNER-ONLY-SECRET-8842 授权口令 alpha"
	const approvalSecret = "APPROVAL-SECRET-9931 rm -rf /srv/prod"
	const publicText = "公开消息：周会改到周四"
	st := mustHubStore(t, m)
	if _, err := agenthub.AppendGroupMessage(ctx, st, &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindText, Sender: agenthub.Sender{Kind: agenthub.SenderHuman},
		Text: ownerSecret, Visibility: agenthub.VisibilityOwner,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := agenthub.AppendGroupMessage(ctx, st, &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindApprovalStub, Sender: agenthub.Sender{Kind: agenthub.SenderSystem},
		Text: approvalSecret, Refs: map[string]any{"need_id": "need_priv"},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := agenthub.AppendGroupMessage(ctx, st, &agenthub.Message{
		GroupID: g.ID, Kind: agenthub.KindText, Sender: agenthub.Sender{Kind: agenthub.SenderHuman},
		Text: publicText,
	}); err != nil {
		t.Fatal(err)
	}

	fake := &evalProvider{reply: "隐私检查完成"}
	eval7WireGroup(t, m, g, member.ID, fake)
	ok, errMsg, data := hubCmd(t, m, buf, 10, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@Eval7PrivB 看公开消息",
	})
	if !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	triggerSeq := numOr(data, "seq", 0)
	waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		return eval7HasText(eval7AgentMessages(msgs, member.ID), "隐私检查完成")
	})

	// ① 快照正文：两条私有消息整条排除（owner-only 不是 redacted 占位），公开消息必须在
	// —— 否则「没泄漏」可能只是因为快照是空的。
	msgs := eval7ReadGroup(t, m, g.ID)
	snap := agenthub.BuildGroupSnapshot(g, member.ID, msgs, 0, agenthub.Round{
		ID: "rnd_eval7_priv", GroupID: g.ID, TriggerSeq: triggerSeq,
		Targets: []agenthub.AgentID{member.ID},
	}, agenthub.SnapshotOptions{})
	if strings.Contains(snap.Text, "OWNER-ONLY-SECRET") || strings.Contains(snap.Text, "APPROVAL-SECRET") {
		t.Fatalf("私有正文进了非 owner 快照:\n%s", snap.Text)
	}
	if strings.Contains(snap.Text, agenthub.OwnerOnlyPlaceholder) {
		t.Fatalf("owner-only 应当整条排除，快照里却出现占位符:\n%s", snap.Text)
	}
	if !strings.Contains(snap.Text, publicText) {
		t.Fatalf("快照没带公开消息（断言退化）:\n%s", snap.Text)
	}

	// ② 真实唤醒请求：模型看到的每一块（system / hidden wake / text）都不能有密文。
	req := fake.lastRequest()
	if req == nil {
		t.Fatal("provider 从未被调用：唤醒链路没走通")
	}
	var all strings.Builder
	var wakeSnapshot string
	wakes := 0
	for _, msg := range req.Messages {
		for _, c := range msg.Content {
			all.WriteString(c.Content)
			all.WriteByte('\n')
			if c.Type == core.ContentTypeHubGroupWake {
				wakes++
				if _, snapText, err := agenthub.ParseGroupWake(c.Content); err != nil {
					t.Fatalf("解析 hub_group_wake: %v", err)
				} else {
					wakeSnapshot = snapText
				}
			}
		}
	}
	if wakes != 1 {
		t.Fatalf("唤醒请求里的 hub_group_wake 块 = %d，want 1（否则隐私断言没覆盖真实上下文）", wakes)
	}
	text := all.String()
	if strings.Contains(text, "OWNER-ONLY-SECRET") || strings.Contains(text, "APPROVAL-SECRET") {
		t.Fatalf("私有正文进了真实唤醒请求:\n%s", text)
	}
	if !strings.Contains(wakeSnapshot, publicText) {
		t.Fatalf("唤醒的 hidden 快照没带公开消息（断言退化）:\n%s", wakeSnapshot)
	}
}

// eval7WaitTimeout 是「两个 provider 必须同时开流 / 群会话必须收口」的判定窗口，超时即失败。
const eval7WaitTimeout = 5 * time.Second

// eval7Barrier 让两个并发 provider 在开流前互等：如果发送路径是「跑完 A 再跑 B」，
// 「并行草稿」断言就退化成两条互不相干的单流。
// 超时必须被记成失败而不是静默放行：串行唤醒只会让先到的 provider 空等，
// 若等满 5s 就照旧往下跑，测试就永远绿——那正是要被抓住的变异。
type eval7Barrier struct {
	mu       sync.Mutex
	waiting  int
	timedOut bool
	ch       chan struct{}
}

func newEval7Barrier(n int) *eval7Barrier {
	return &eval7Barrier{waiting: n, ch: make(chan struct{})}
}

// wait 阻塞到所有 provider 到齐；超时记下 timedOut 后放行（不在这里报错：先让两条
// 流都跑完，失败信息由 await 指出「唤醒串行」，而不是退化成「缺 final」）。
func (b *eval7Barrier) wait() {
	b.mu.Lock()
	b.waiting--
	last := b.waiting == 0
	if last {
		close(b.ch)
	}
	b.mu.Unlock()
	select {
	case <-b.ch:
	case <-time.After(eval7WaitTimeout):
		b.mu.Lock()
		b.timedOut = true
		b.mu.Unlock()
	}
}

// await 在测试侧有界等待全部 provider 到齐，并拒绝「靠超时兜底才凑齐」的情形。
// 先到的 provider 超时放行 = 另一条流当时根本没在跑，并行断言已经退化。
func (b *eval7Barrier) await(t *testing.T) {
	t.Helper()
	select {
	case <-b.ch:
		b.mu.Lock()
		timedOut := b.timedOut
		b.mu.Unlock()
		if timedOut {
			t.Fatalf("有 provider 在 %s 内没等到另一条流：群唤醒是串行的（并行草稿断言已退化）", eval7WaitTimeout)
		}
	case <-time.After(eval7WaitTimeout):
		t.Fatalf("两个 provider 没有在 %s 内同时开流：群唤醒是串行的（并行草稿断言已退化）", eval7WaitTimeout)
	}
}

// waitHubGroupIdle 有界等群会话回到空闲。闩清零发生在 Run 返回之后（final 已落库），
// 所以它同时是「这一轮跑完了」的确定性信号，可以替代固定 300ms 静默窗口。
func waitHubGroupIdle(t *testing.T, bs *bridgeSession) {
	t.Helper()
	deadline := time.Now().Add(eval7WaitTimeout)
	for bs.hubGroupBusy.Load() {
		if time.Now().After(deadline) {
			t.Fatalf("群会话 %s 在 %s 后仍 busy（run 没有收口）", bs.id, eval7WaitTimeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// gatedStreamProvider 在屏障放行后分块流式，让两条流真正交错（并为 80ms 合并留出窗口）。
type gatedStreamProvider struct {
	chunks  []string
	reply   string
	barrier *eval7Barrier
}

func (p *gatedStreamProvider) Stream(_ context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	p.barrier.wait()
	const rid = "req_gated"
	if err := h(provider.LLMStartEvent{RequestId: rid, Model: req.Model, Timestamp: time.Now()}); err != nil {
		return err
	}
	for i, c := range p.chunks {
		if err := h(provider.LLMContentDeltaEvent{RequestId: rid, Delta: c, Index: int64(i)}); err != nil {
			return err
		}
		time.Sleep(30 * time.Millisecond)
	}
	return h(provider.LLMEndEvent{
		RequestId:    rid,
		FinishReason: core.FinishReasonStop,
		Content:      p.reply,
		Model:        req.Model,
		Usage:        core.Usage{Input: 3, Output: 2},
	})
}

// 评测 7-6：两个成员并行流式，草稿不串台；final 与 delta 同 ID、正文一致、事件不重复。
func TestEval7ConcurrentStreamsKeepDraftAndFinalConsistent(t *testing.T) {
	m, buf := testManager(t)
	agents, g := eval7GroupFixture(t, m, buf, 1, "评测7流式", "Eval7StreamA", "Eval7StreamB")
	barrier := newEval7Barrier(len(agents))
	replies := []string{"A 正在回复", "B 也在回复"}
	chunks := [][]string{{"A 正在", "回复"}, {"B 也", "在回复"}}
	sessions := make([]*bridgeSession, len(agents))
	for i, a := range agents {
		sessions[i] = eval7WireGroup(t, m, g, a.ID, &gatedStreamProvider{
			chunks: chunks[i], reply: replies[i], barrier: barrier,
		})
	}

	ok, errMsg, data := hubCmd(t, m, buf, 40, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@Eval7StreamA @Eval7StreamB 同时说一句",
	})
	if !ok {
		t.Fatalf("group send: %s", errMsg)
	}
	roundID, _, targets := eval7RoundOf(t, data)
	if len(targets) != 2 {
		t.Fatalf("targets = %v，want 两个并行成员", targets)
	}
	// 两个 provider 必须真的同时开流：串行唤醒时先到的那个会等满超时，
	// 这里直接判失败（否则后续断言只能证明「两条流先后存在」）。
	barrier.await(t)
	msgs := waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		for _, want := range replies {
			found := false
			for _, msg := range msgs {
				if msg.Sender.Kind == agenthub.SenderAgent && msg.Text == want {
					found = true
				}
			}
			if !found {
				return false
			}
		}
		return true
	})
	// 闩清零 = run 已收口（final 落库 + 事件已写），再断言事件流不是「还差一条」。
	waitHubGroupIdle(t, sessions[0])
	waitHubGroupIdle(t, sessions[1])
	msgs = eval7ReadGroup(t, m, g.ID)

	// final 不重复：每个成员恰好一条，且都挂在本轮。
	finals := map[agenthub.AgentID]*agenthub.Message{}
	for i := range msgs {
		msg := &msgs[i]
		if msg.Sender.Kind != agenthub.SenderAgent {
			continue
		}
		if _, dup := finals[msg.Sender.AgentID]; dup {
			t.Fatalf("%s 写了多条 final: %+v", msg.Sender.AgentID, eval7AgentMessages(msgs, msg.Sender.AgentID))
		}
		finals[msg.Sender.AgentID] = msg
	}
	if len(finals) != len(agents) {
		t.Fatalf("final 成员数 = %d，want %d", len(finals), len(agents))
	}
	for _, a := range agents {
		final := finals[a.ID]
		if final == nil || final.RoundID != roundID {
			t.Fatalf("%s final = %+v，want round %q", a.ID, final, roundID)
		}
	}

	// 草稿：两个不同的 message_id（并行草稿不合并/不串），每个 id 的 delta 拼起来等于对应 final。
	deltas := map[string][]map[string]any{}
	for _, ev := range hubEvents(t, buf, "hub_message_delta") {
		id, _ := ev["message_id"].(string)
		if id == "" {
			t.Fatalf("delta 缺 message_id: %+v", ev)
		}
		deltas[id] = append(deltas[id], ev)
	}
	if len(deltas) != len(agents) {
		t.Fatalf("草稿流 = %d，want %d（并行 agent 各自一条草稿）: %v", len(deltas), len(agents), deltas)
	}
	for _, a := range agents {
		final := finals[a.ID]
		evs := deltas[final.ID]
		if len(evs) == 0 {
			t.Fatalf("%s final id %s 没有对应 delta（草稿 ID 集合 = %v）", a.ID, final.ID, deltas)
		}
		var text strings.Builder
		for i, ev := range evs {
			if got := int(ev["index"].(float64)); got != i {
				t.Fatalf("%s delta index[%d] = %d，want 单调从 0 开始", a.ID, i, got)
			}
			if ev["agent_id"] != string(a.ID) {
				t.Fatalf("delta agent_id = %v，want %s（草稿串台）", ev["agent_id"], a.ID)
			}
			if ev["group_id"] != string(g.ID) {
				t.Fatalf("delta group_id = %v，want %s", ev["group_id"], g.ID)
			}
			text.WriteString(ev["delta"].(string))
		}
		if text.String() != final.Text {
			t.Fatalf("%s delta 拼接 = %q，final = %q", a.ID, text.String(), final.Text)
		}
		// final 事件恰好一次，且 payload 与落库 final 逐字段一致：前端直接消费
		// ev.message（替换草稿 + 按 seq 排序），只数事件条数会让正文/序号被改坏时全绿。
		eval7FinalEvent(t, buf, g.ID, roundID, a.ID, final)
		// 顺序：该 ID 的最后一段 delta 必须先于 final 事件写出（final 之后到达的 delta
		// 会被前端丢弃，尾部内容就永远看不到）。
		deltaAt, finalAt := waitEventOffsets(t, buf,
			func(ev map[string]any) bool {
				return ev["event_type"] == "hub_message_delta" && ev["message_id"] == final.ID
			},
			func(ev map[string]any) bool {
				msg, _ := ev["message"].(map[string]any)
				return ev["event_type"] == "hub_message" && msg != nil && msg["id"] == final.ID
			},
		)
		if finalAt < deltaAt {
			t.Fatalf("%s 顺序反了：final 事件偏移 %d 早于最后一段 delta 偏移 %d（delta 必须先写）", a.ID, finalAt, deltaAt)
		}
	}
}
