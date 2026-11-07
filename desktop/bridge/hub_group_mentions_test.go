package main

// P3b-02：结构化 mentions / mention_all（group_say + hub_group_send）与消息持久化。
//
// 全部用确定性 fake provider：唤醒链路只在进程内跑，不碰网络。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

// p3bGroup 建群 + 一条人类种子消息（重置 agent 轮次上限），并返回群。
func p3bGroup(t *testing.T, m *manager, buf *syncBuf, cmdID int, name string, policy map[string]any, members ...struct {
	id   agenthub.AgentID
	role string
}) *agenthub.Group {
	t.Helper()
	raw := make([]map[string]any, 0, len(members))
	for _, mem := range members {
		raw = append(raw, map[string]any{"agent_id": string(mem.id), "role": mem.role})
	}
	payload := map[string]any{"name": name, "members": raw}
	if policy != nil {
		payload["policy"] = policy
	}
	ok, errMsg, data := hubCmd(t, m, buf, cmdID, "hub_group_create", payload)
	if !ok {
		t.Fatalf("create group: %s", errMsg)
	}
	g := groupFromData(t, data)
	if ok, errMsg, _ := hubCmd(t, m, buf, cmdID+1, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "开始",
	}); !ok {
		t.Fatalf("human seed: %s", errMsg)
	}
	return g
}

// p3bGroupSay 以某智能体的群工具调用 group_say（与生产一致：参数走 JSON）。
func p3bGroupSay(t *testing.T, m *manager, agentID string, args map[string]any) (map[string]any, error) {
	t.Helper()
	bs, err := m.hubDMSession(agentID)
	if err != nil {
		t.Fatal(err)
	}
	tool, err := bs.hubEngine.GetTool(context.Background(), "group_say")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(args)
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Call(context.Background(), "group_say", string(raw))
	if err != nil {
		return nil, err
	}
	var res map[string]any
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("group_say 结果不是 JSON: %s (%v)", out, err)
	}
	return res, nil
}

// p3bWireFake 给某成员的群会话装一个确定性 loop（必须在被唤醒前完成）。
func p3bWireFake(t *testing.T, m *manager, g *agenthub.Group, agentID, reply string) {
	t.Helper()
	bs, err := m.hubGroupSession(agentID, string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	setGroupFakeLoop(t, m, bs, g, agentID, &evalProvider{reply: reply})
}

func p3bFindMessage(msgs []agenthub.Message, id string) *agenthub.Message {
	for i := range msgs {
		if msgs[i].ID == id {
			return &msgs[i]
		}
	}
	return nil
}

// toStringIDs 把 JSON 解码后的 targets/mentions（[]any 或 []agenthub.AgentID）转成 id 列表。
func toStringIDs(v any) []agenthub.AgentID {
	switch ids := v.(type) {
	case []agenthub.AgentID:
		return ids
	case []any:
		out := make([]agenthub.AgentID, 0, len(ids))
		for _, x := range ids {
			if s, ok := x.(string); ok {
				out = append(out, agenthub.AgentID(s))
			}
		}
		return out
	}
	return nil
}

func p3bSameIDs(got []agenthub.AgentID, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if string(got[i]) != want[i] {
			return false
		}
	}
	return true
}

// p3bSameSet 用于 @all 目标：顺序由 JoinedAt/AgentID 决定，测试只关心集合。
func p3bSameSet(got []agenthub.AgentID, want ...string) bool {
	if len(got) != len(want) {
		return false
	}
	seen := make(map[agenthub.AgentID]int, len(got))
	for _, id := range got {
		seen[id]++
	}
	for _, id := range want {
		if seen[agenthub.AgentID(id)] != 1 {
			return false
		}
	}
	return true
}

// 结构化 mentions 能在正文没有 @Name 时唤醒目标，并把生效的 mentions 落到消息上。
func TestHubGroupSayStructuredMentionWakes(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "MentionA")
	b := createHubAgent(t, m, buf, 2, "MentionB")
	g := p3bGroup(t, m, buf, 3, "结构化群", nil,
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"},
		struct {
			id   agenthub.AgentID
			role string
		}{b.ID, "member"},
	)
	p3bWireFake(t, m, g, string(b.ID), "B 收到结构化点名。")

	res, err := p3bGroupSay(t, m, string(a.ID), map[string]any{
		"group_id": string(g.ID),
		"text":     "看一下日志（正文里没有 @）",
		"mentions": []string{string(b.ID)},
	})
	if err != nil {
		t.Fatalf("group_say: %v", err)
	}
	if !p3bSameIDs(toStringIDs(res["targets"]), string(b.ID)) {
		t.Fatalf("targets = %v, want [%s]", res["targets"], b.ID)
	}
	if !p3bSameIDs(toStringIDs(res["mentions"]), string(b.ID)) || res["mention_all"] != false {
		t.Fatalf("mentions/mention_all = %v/%v", res["mentions"], res["mention_all"])
	}

	msgs := waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		for _, msg := range msgs {
			if msg.Sender.AgentID == b.ID && strings.Contains(msg.Text, "B 收到结构化点名") {
				return true
			}
		}
		return false
	})
	trigger := p3bFindMessage(msgs, res["message_id"].(string))
	if trigger == nil || trigger.Sender.AgentID != a.ID {
		t.Fatalf("trigger message missing: %+v", msgs)
	}
	if !p3bSameIDs(trigger.Mentions, string(b.ID)) || trigger.MentionAll {
		t.Fatalf("stored trigger mentions = %v/%v, want [%s]/false", trigger.Mentions, trigger.MentionAll, b.ID)
	}
}

// 正文 @ 与结构化 mentions 并集、去重、self 过滤，并共用 policy 的 3 人上限。
func TestHubGroupSayMentionUnionAndCap(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "UnionA")
	b := createHubAgent(t, m, buf, 2, "UnionB")
	c := createHubAgent(t, m, buf, 3, "UnionC")
	d := createHubAgent(t, m, buf, 4, "UnionD")
	g := p3bGroup(t, m, buf, 5, "并集群", map[string]any{"max_mentions_per_message": 2},
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
		struct {
			id   agenthub.AgentID
			role string
		}{d.ID, "member"},
	)
	p3bWireFake(t, m, g, string(b.ID), "B 看到了。")
	p3bWireFake(t, m, g, string(c.ID), "C 看到了。")
	p3bWireFake(t, m, g, string(d.ID), "D 不该被点名。")

	// 正文 @B @C + 结构化 [D, C, A]：C 去重、A 是 self，前 2 个留下，D 记 dropped。
	res, err := p3bGroupSay(t, m, string(a.ID), map[string]any{
		"group_id": string(g.ID),
		"text":     "@UnionB @UnionC 一起看",
		"mentions": []string{string(d.ID), string(c.ID), string(a.ID)},
	})
	if err != nil {
		t.Fatalf("group_say: %v", err)
	}
	if !p3bSameIDs(toStringIDs(res["targets"]), string(b.ID), string(c.ID)) {
		t.Fatalf("targets = %v, want [B C]", res["targets"])
	}
	if !p3bSameIDs(toStringIDs(res["mentions"]), string(b.ID), string(c.ID)) {
		t.Fatalf("mentions = %v, want [B C]", res["mentions"])
	}
	if res["dropped"] != float64(1) {
		t.Fatalf("dropped = %v, want 1", res["dropped"])
	}

	msgs := waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		return p3bFindMessage(msgs, res["message_id"].(string)) != nil
	})
	trigger := p3bFindMessage(msgs, res["message_id"].(string))
	if trigger == nil || !p3bSameIDs(trigger.Mentions, string(b.ID), string(c.ID)) {
		t.Fatalf("stored mentions = %+v, want [B C]", trigger)
	}
}

// 非成员/未知/空结构化 id 必须让 group_say 失败，且不留下消息或回执。
func TestHubGroupSayStructuredMentionRejects(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "RejectA")
	b := createHubAgent(t, m, buf, 2, "RejectB")
	outsider := createHubAgent(t, m, buf, 3, "RejectOutsider")
	g := p3bGroup(t, m, buf, 4, "拒绝群", nil,
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
	before := mustReadLog(t, m, g.ID)

	for name, mentions := range map[string][]string{
		"non member": {string(outsider.ID)},
		"unknown":    {"ag_zzzzzzzz"},
		"empty":      {""},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := p3bGroupSay(t, m, string(a.ID), map[string]any{
				"group_id": string(g.ID), "text": "看下", "mentions": mentions,
			})
			if err == nil {
				t.Fatalf("mentions %v must fail", mentions)
			}
			if msgs := mustReadLog(t, m, g.ID); len(msgs) != len(before) {
				t.Fatalf("rejected mention wrote a message: before=%d after=%d", len(before), len(msgs))
			}
			if acts, err := st.ReadActivity(context.Background(), g.ID, 0, 50); err != nil || len(acts) != 0 {
				t.Fatalf("rejected mention wrote a receipt: %+v, %v", acts, err)
			}
		})
	}
}

func mustReadLog(t *testing.T, m *manager, gid agenthub.GroupID) []agenthub.Message {
	t.Helper()
	st := mustHubStore(t, m)
	msgs, err := st.ReadMessages(context.Background(), gid, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	return msgs
}

// mention_all：owner/人类可以；member 被拒绝且不降级（不落消息）。
func TestHubGroupSayMentionAllPermissions(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "AllA")
	b := createHubAgent(t, m, buf, 2, "AllB")
	c := createHubAgent(t, m, buf, 3, "AllC")
	g := p3bGroup(t, m, buf, 4, "@all 群", nil,
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
	// 空回复：只验证路由/鉴权，不写 final（避免异步 final 干扰后续日志计数）。
	p3bWireFake(t, m, g, string(b.ID), "")
	p3bWireFake(t, m, g, string(c.ID), "")

	res, err := p3bGroupSay(t, m, string(a.ID), map[string]any{
		"group_id": string(g.ID), "text": "开会", "mention_all": true,
	})
	if err != nil {
		t.Fatalf("owner mention_all: %v", err)
	}
	if res["mention_all"] != true || !p3bSameSet(toStringIDs(res["targets"]), string(b.ID), string(c.ID)) {
		t.Fatalf("owner mention_all = %+v", res)
	}

	before := mustReadLog(t, m, g.ID)
	if _, err := p3bGroupSay(t, m, string(b.ID), map[string]any{
		"group_id": string(g.ID), "text": "@AllC 看下", "mention_all": true,
	}); err == nil {
		t.Fatal("member mention_all must be rejected")
	}
	if msgs := mustReadLog(t, m, g.ID); len(msgs) != len(before) {
		t.Fatalf("forbidden mention_all wrote a message: before=%d after=%d", len(before), len(msgs))
	}
}

// 人类 hub_group_send 的结构化 mentions/mention_all：并集、校验、落盘、幂等。
func TestHubGroupSendStructuredMentions(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "HumanA")
	b := createHubAgent(t, m, buf, 2, "HumanB")
	outsider := createHubAgent(t, m, buf, 3, "HumanOutsider")
	g := p3bGroup(t, m, buf, 4, "人类结构化群", nil,
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"},
		struct {
			id   agenthub.AgentID
			role string
		}{b.ID, "member"},
	)
	// 空回复：本测试只关心路由/落盘，不等待成员 final。
	p3bWireFake(t, m, g, string(a.ID), "")
	p3bWireFake(t, m, g, string(b.ID), "")

	ok, errMsg, data := hubCmd(t, m, buf, 10, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "请看一下（正文没有 @）",
		"mentions": []any{string(b.ID)}, "client_msg_id": "p3b-human-1",
	})
	if !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	if !p3bSameIDs(toStringIDs(data["targets"]), string(b.ID)) {
		t.Fatalf("human targets = %v", data["targets"])
	}
	msgID := data["message_id"].(string)
	msgs := waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		return p3bFindMessage(msgs, msgID) != nil
	})
	msg := p3bFindMessage(msgs, msgID)
	if msg == nil || !p3bSameIDs(msg.Mentions, string(b.ID)) || msg.MentionAll {
		t.Fatalf("human message mentions = %+v", msg)
	}
	// hub_message 事件同样带 mentions（前端回显依赖同一对象）。
	found := 0
	for _, ev := range hubEvents(t, buf, "hub_message") {
		raw, _ := ev["message"].(map[string]any)
		if raw == nil || raw["id"] != msgID {
			continue
		}
		found++
		if !p3bSameIDs(toStringIDs(raw["mentions"]), string(b.ID)) {
			t.Fatalf("hub_message mentions = %v", raw["mentions"])
		}
		if ev["seq"] != float64(msg.Seq) {
			t.Fatalf("hub_message seq = %v, want %d", ev["seq"], msg.Seq)
		}
	}
	if found != 1 {
		t.Fatalf("hub_message events for %s = %d, want 1", msgID, found)
	}

	// client_msg_id 幂等：同 id 重试不双写、不再发事件。
	beforeCount := len(mustReadLog(t, m, g.ID))
	beforeEvents := strings.Count(buf.String(), `"event_type":"hub_message"`)
	ok, errMsg, data = hubCmd(t, m, buf, 11, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "请看一下（正文没有 @）",
		"mentions": []any{string(b.ID)}, "client_msg_id": "p3b-human-1",
	})
	if !ok || data["deduplicated"] != true {
		t.Fatalf("retry = %v, %s, %v", ok, errMsg, data)
	}
	if count := len(mustReadLog(t, m, g.ID)); count != beforeCount {
		t.Fatalf("retry wrote a duplicate message: %d -> %d", beforeCount, count)
	}
	if count := strings.Count(buf.String(), `"event_type":"hub_message"`); count != beforeEvents {
		t.Fatalf("retry emitted hub_message: %d -> %d", beforeEvents, count)
	}

	// mention_all：人类可用；目标是全体成员。
	ok, errMsg, data = hubCmd(t, m, buf, 12, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "全体开会", "mention_all": true,
	})
	if !ok {
		t.Fatalf("human mention_all: %s", errMsg)
	}
	if !p3bSameSet(toStringIDs(data["targets"]), string(a.ID), string(b.ID)) {
		t.Fatalf("human mention_all targets = %v (A=%s B=%s)", data["targets"], a.ID, b.ID)
	}
	allMsg := p3bFindMessage(mustReadLog(t, m, g.ID), data["message_id"].(string))
	if allMsg == nil || !allMsg.MentionAll {
		t.Fatalf("human mention_all message = %+v", allMsg)
	}

	// @all + 显式 mentions：targets 扩成全体，但 mentions 只记显式点名的人。
	// 落盘/事件写成 round.Targets 就会把「全体」当成「点名」（M2）。
	ok, errMsg, data = hubCmd(t, m, buf, 16, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "全体开会，HumanB 重点看", "mention_all": true,
		"mentions": []any{string(b.ID)}, "client_msg_id": "p3b-human-all-2",
	})
	if !ok {
		t.Fatalf("human mention_all + mentions: %s", errMsg)
	}
	if !p3bSameSet(toStringIDs(data["targets"]), string(a.ID), string(b.ID)) {
		t.Fatalf("mention_all+mentions targets = %v (A=%s B=%s)", data["targets"], a.ID, b.ID)
	}
	allID := data["message_id"].(string)
	bothMsg := p3bFindMessage(mustReadLog(t, m, g.ID), allID)
	if bothMsg == nil || !bothMsg.MentionAll || !p3bSameIDs(bothMsg.Mentions, string(b.ID)) {
		t.Fatalf("mention_all+mentions message = %+v, want mentions [%s] / mention_all", bothMsg, b.ID)
	}
	bothEvents := 0
	for _, ev := range hubEvents(t, buf, "hub_message") {
		raw, _ := ev["message"].(map[string]any)
		if raw == nil || raw["id"] != allID {
			continue
		}
		bothEvents++
		if !p3bSameIDs(toStringIDs(raw["mentions"]), string(b.ID)) || raw["mention_all"] != true {
			t.Fatalf("mention_all+mentions event = %v/%v, want [%s]/true", raw["mentions"], raw["mention_all"], b.ID)
		}
	}
	if bothEvents != 1 {
		t.Fatalf("hub_message events for %s = %d, want 1", allID, bothEvents)
	}

	// 非成员/未知 id：失败且不落消息。
	beforeCount = len(mustReadLog(t, m, g.ID))
	ok, _, _ = hubCmd(t, m, buf, 13, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "非法点名", "mentions": []any{string(outsider.ID)},
	})
	if ok {
		t.Fatal("non-member mention must fail")
	}
	if len(mustReadLog(t, m, g.ID)) != beforeCount {
		t.Fatal("failed human mention wrote a message")
	}
	// 类型不对也要报错，不能静默丢。
	ok, _, _ = hubCmd(t, m, buf, 14, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "类型错误", "mentions": "not-an-array",
	})
	if ok {
		t.Fatal("malformed mentions must fail")
	}

	// 纯文本 @ 的既有行为不变。
	ok, errMsg, data = hubCmd(t, m, buf, 15, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@HumanB 还是正文点名",
	})
	if !ok || !p3bSameIDs(toStringIDs(data["targets"]), string(b.ID)) {
		t.Fatalf("text-only send = %v, %s, %v", ok, errMsg, data)
	}
}

// final 回复的 mentions/mention_all 落盘进 hub_message，且沿用 P3a-09 的预分配流式 ID。
func TestHubGroupFinalMessageMentions(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "FinalA")
	b := createHubAgent(t, m, buf, 2, "FinalB")
	c := createHubAgent(t, m, buf, 3, "FinalC")
	g := p3bGroup(t, m, buf, 4, "final 群", nil,
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
	p3bWireFake(t, m, g, string(b.ID), "交给 @FinalC 处理。")
	p3bWireFake(t, m, g, string(c.ID), "C 收到。")

	ok, errMsg, _ := hubCmd(t, m, buf, 10, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@FinalB 请处理",
	})
	if !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	msgs := waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		var sawB, sawC bool
		for _, msg := range msgs {
			if msg.Sender.AgentID == b.ID && msg.Text == "交给 @FinalC 处理。" {
				sawB = true
			}
			if msg.Sender.AgentID == c.ID && msg.Text == "C 收到。" {
				sawC = true
			}
		}
		return sawB && sawC
	})
	var bFinal, cFinal *agenthub.Message
	for i := range msgs {
		switch {
		case msgs[i].Sender.AgentID == b.ID && msgs[i].Text == "交给 @FinalC 处理。":
			bFinal = &msgs[i]
		case msgs[i].Sender.AgentID == c.ID && msgs[i].Text == "C 收到。":
			cFinal = &msgs[i]
		}
	}
	if bFinal == nil || cFinal == nil {
		t.Fatalf("final messages missing: %+v", msgs)
	}
	// P3a-09：final 复用 AgentStart 预分配的流式 ID，不是 msg_<seq>。
	if !strings.HasPrefix(bFinal.ID, "msg_") || bFinal.ID == agenthub.MessageID(bFinal.Seq) || bFinal.RoundID == "" {
		t.Fatalf("final id/round = %q / %q (seq %d)", bFinal.ID, bFinal.RoundID, bFinal.Seq)
	}
	if !p3bSameIDs(bFinal.Mentions, string(c.ID)) || bFinal.MentionAll {
		t.Fatalf("final mentions = %v/%v, want [C]/false", bFinal.Mentions, bFinal.MentionAll)
	}
	// 无 @ 的 C 回复不能继承 B 的 mentions（同一 round 字段串台）。
	if len(cFinal.Mentions) != 0 || cFinal.MentionAll || cFinal.ID == bFinal.ID {
		t.Fatalf("C final = %+v (B id %s)", cFinal, bFinal.ID)
	}

	eventsFor := func(id string) []map[string]any {
		var out []map[string]any
		for _, ev := range hubEvents(t, buf, "hub_message") {
			raw, _ := ev["message"].(map[string]any)
			if raw != nil && raw["id"] == id {
				out = append(out, ev)
			}
		}
		return out
	}
	evB := eventsFor(bFinal.ID)
	if len(evB) != 1 {
		t.Fatalf("hub_message events for B final = %d, want 1", len(evB))
	}
	rawB, _ := evB[0]["message"].(map[string]any)
	if !p3bSameIDs(toStringIDs(rawB["mentions"]), string(c.ID)) || rawB["mention_all"] != nil {
		t.Fatalf("final event mentions = %v/%v", rawB["mentions"], rawB["mention_all"])
	}
	if rawB["round_id"] != bFinal.RoundID {
		t.Fatalf("final event round_id = %v, want %s", rawB["round_id"], bFinal.RoundID)
	}
	if evB[0]["seq"] != float64(bFinal.Seq) {
		t.Fatalf("final event seq = %v, want %d", evB[0]["seq"], bFinal.Seq)
	}
	evC := eventsFor(cFinal.ID)
	if len(evC) != 1 {
		t.Fatalf("hub_message events for C final = %d, want 1", len(evC))
	}
	rawC, _ := evC[0]["message"].(map[string]any)
	if len(toStringIDs(rawC["mentions"])) != 0 {
		t.Fatalf("C final event inherited mentions: %v", rawC["mentions"])
	}
	// 流式 delta 若存在必须与 final 同一个 message_id（P3a-09）。
	for _, ev := range hubEvents(t, buf, "hub_message_delta") {
		if ev["group_id"] != string(g.ID) {
			continue
		}
		if ev["message_id"] != bFinal.ID && ev["message_id"] != cFinal.ID {
			t.Fatalf("delta message_id %v 不属于任何 final", ev["message_id"])
		}
	}
}

// p3bWaitAgentCursor 有界等某成员的 agent 游标推进到 seq：finishHubRound 在 Run 收口后
// 才推进游标，所以它是「那一轮群唤醒已经处理完」的确定性信号（替代固定静默窗口）。
func p3bWaitAgentCursor(t *testing.T, m *manager, gid agenthub.GroupID, agentID agenthub.AgentID, seq int64) {
	t.Helper()
	st := mustHubStore(t, m)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if c, err := st.LoadCursor(context.Background(), gid, agentID); err == nil && c.AgentContextSeq >= seq {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("agent %s 的群游标没推进到 seq %d", agentID, seq)
}

// P3b-02 M2（final 路径）：owner 的 final 正文 @all 时 targets 是全体，
// 落盘 mentions 只含正文里显式点名的人 —— 不能把 Targets 当成 Mentions。
func TestHubGroupFinalMentionAllKeepsExplicitMentions(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "AllFinalA")
	b := createHubAgent(t, m, buf, 2, "AllFinalB")
	c := createHubAgent(t, m, buf, 3, "AllFinalC")
	g := p3bGroup(t, m, buf, 4, "final @all 群", nil,
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
	// A 是 owner：final 正文里的 @all 合法。B/C 空回复，只验 A 的 final 落盘。
	const reply = "@AllFinalB @all 都看一下"
	p3bWireFake(t, m, g, string(a.ID), reply)
	p3bWireFake(t, m, g, string(b.ID), "")
	p3bWireFake(t, m, g, string(c.ID), "")

	if ok, errMsg, _ := hubCmd(t, m, buf, 10, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@AllFinalA 请处理",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	msgs := waitGroupMessages(t, m, g.ID, func(msgs []agenthub.Message) bool {
		for i := range msgs {
			if msgs[i].Sender.AgentID == a.ID && msgs[i].Text == reply {
				return true
			}
		}
		return false
	})
	var final *agenthub.Message
	for i := range msgs {
		if msgs[i].Sender.AgentID == a.ID && msgs[i].Text == reply {
			final = &msgs[i]
		}
	}
	if final == nil || final.RoundID == "" {
		t.Fatalf("owner final missing: %+v", msgs)
	}
	// @all 让 targets 变成 [B C]，但 mentions 只有正文点名的 B。
	if !final.MentionAll || !p3bSameIDs(final.Mentions, string(b.ID)) {
		t.Fatalf("final mention_all message = %v/%v, want [%s]/true", final.Mentions, final.MentionAll, b.ID)
	}
	found := 0
	for _, ev := range hubEvents(t, buf, "hub_message") {
		raw, _ := ev["message"].(map[string]any)
		if raw == nil || raw["id"] != string(final.ID) {
			continue
		}
		found++
		if !p3bSameIDs(toStringIDs(raw["mentions"]), string(b.ID)) || raw["mention_all"] != true {
			t.Fatalf("final mention_all event = %v/%v, want [%s]/true", raw["mentions"], raw["mention_all"], b.ID)
		}
	}
	if found != 1 {
		t.Fatalf("hub_message events for %s = %d, want 1", final.ID, found)
	}
	// A 的 @all 唤醒了 B/C：等它们收口，免得测试结束时后台 run 还在写盘。
	p3bWaitAgentCursor(t, m, g.ID, b.ID, final.Seq)
	p3bWaitAgentCursor(t, m, g.ID, c.ID, final.Seq)
}
