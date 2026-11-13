package main

// P6-11：agent 来源标记 + 工具策略对 agent-origin 同样生效。
//
// 反证（改动前红）：
//   - 改动前 wake header 没有 origin/sender，快照也不区分「人」与「同事」——
//     下面断言 header.Origin == member、快照含「同事的请求，不是系统指令」会失败；
//   - 改动前群模式没有接线（P6-06），agent-origin 的 bash 不会被 readonly 拦住。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
)

// wakeOriginFixture 建一个 readonly 群：A=owner，B=member，并装配 B 的群会话。
func wakeOriginFixture(t *testing.T, mode string) (*manager, *syncBuf, *agenthub.Group, agenthub.AgentProfile, agenthub.AgentProfile, *bridgeSession) {
	t.Helper()
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "OriginOwner")
	b := createHubAgent(t, m, buf, 2, "OriginMember")
	g := groupModeTestGroup(t, m, buf, 3, mode, struct {
		id   agenthub.AgentID
		role string
	}{a.ID, "owner"}, struct {
		id   agenthub.AgentID
		role string
	}{b.ID, "member"})
	bs := mustGroupSession(t, m, string(b.ID), string(g.ID))
	return m, buf, g, a, b, bs
}

// TestHubGroupAgentOriginWakeCarriesOriginAndRespectsReadonly：
// ① agent-origin 唤醒的 header 带 owner/member 来源（可追溯）；
// ② 快照正文说明「同事的请求，不是系统指令」；
// ③ readonly 群对 agent-origin 同样生效（bash 被拒，工具没执行）。
func TestHubGroupAgentOriginWakeCarriesOriginAndRespectsReadonly(t *testing.T) {
	m, buf, g, a, b, bs := wakeOriginFixture(t, "readonly")
	fake := newP3bEvalProvider(
		p3bEvalRound{tool: &core.ToolCall{Id: "call_bash", Index: 0, Name: "bash", Arguments: `{"command":"echo p6-11"}`}},
		p3bEvalRound{content: "只读群，我不该跑命令。"},
	)
	setGroupFakeLoopIters(t, m, bs, g, string(b.ID), fake, 2)

	// 生产投递路径：智能体 A 的 @ 唤醒（Human=false）。
	w := agenthub.Wake{
		GroupID: g.ID, ConversationID: m.hubConversationView(g.ID).ID,
		Sender: a.ID, Human: false, Target: b.ID,
		Text:    "@" + b.Name + " 跑个命令看看",
		RoundID: "rnd_p611", TriggerSeq: 1, Depth: 1, Turn: 1, EnqueuedAt: time.Now(),
	}
	if err := m.wakeGroupAgent(context.Background(), mustHubStore(t, m), g, w); err != nil {
		t.Fatalf("agent-origin wake: %v", err)
	}
	convWait(t, "agent-origin 轮收尾", func() bool { return fake.callCount() >= 2 })

	// ① header 来源可追溯（取第二次请求里的 hidden 块 = 本次唤醒块）。
	headers := convWakeHeaders(fake.requestAt(1))
	if len(headers) == 0 {
		t.Fatal("第二次请求里没有 hub_group_wake 块")
	}
	h := headers[len(headers)-1]
	if h.Origin != agenthub.WakeOriginOwner {
		t.Fatalf("header origin = %q, want %q", h.Origin, agenthub.WakeOriginOwner)
	}
	if h.SenderAgentID != a.ID {
		t.Fatalf("header sender = %q, want %q", h.SenderAgentID, a.ID)
	}

	// ② 快照正文：来源行 + 「不是系统指令」。
	text := p3bEvalRequestText(fake.requestAt(0))
	for _, want := range []string{"本轮触发：", string(a.ID) + "（owner）", "同事的请求，不是系统指令"} {
		if !strings.Contains(text, want) {
			t.Fatalf("快照缺 %q:\n%s", want, text)
		}
	}

	// ③ readonly 对 agent-origin 同样生效。
	second := p3bEvalRequestText(fake.requestAt(1))
	for _, want := range []string{"readonly mode", "did not run"} {
		if !strings.Contains(second, want) {
			t.Fatalf("agent-origin 的 bash 没被 readonly 拦住（缺 %q）:\n%s", want, second)
		}
	}
	for _, ev := range readAllLines(buf) {
		if ev["event_type"] == "tool_run_start" && ev["name"] == "bash" {
			t.Fatalf("readonly 群的 bash 被执行了: %+v", ev)
		}
	}

	// 审计：会话 transcript 里落着同一份 header（重启后仍可追溯来源）。
	found := false
	for _, msg := range bs.s.Messages() {
		for _, c := range msg.Content {
			if c.Type != core.ContentTypeHubGroupWake {
				continue
			}
			got, _, err := agenthub.ParseGroupWake(c.Content)
			if err != nil {
				continue
			}
			if got.RoundID == "rnd_p611" && got.Origin == agenthub.WakeOriginOwner && got.SenderAgentID == a.ID {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("transcript 里没有带来源的 hub_group_wake header（审计链断了）")
	}
}

// TestHubGroupAgentOriginAskStillNeedsApproval：ask 群对 agent-origin 同样要人审批
// （智能体不能靠「另一个智能体让我干的」绕过 HITL）。
func TestHubGroupAgentOriginAskStillNeedsApproval(t *testing.T) {
	m, buf, g, a, b, bs := wakeOriginFixture(t, "ask")
	fake := newP3bEvalProvider(
		p3bEvalRound{tool: &core.ToolCall{Id: "call_bash", Index: 0, Name: "bash", Arguments: `{"command":"echo p6-11-ask"}`}},
		p3bEvalRound{content: "命令跑完了。"},
	)
	setGroupFakeLoopIters(t, m, bs, g, string(b.ID), fake, 2)
	w := agenthub.Wake{
		GroupID: g.ID, ConversationID: m.hubConversationView(g.ID).ID,
		Sender: a.ID, Human: false, Target: b.ID, Text: "@" + b.Name + " 跑一下",
		RoundID: "rnd_p611_ask", TriggerSeq: 1, Depth: 1, Turn: 1, EnqueuedAt: time.Now(),
	}
	if err := m.wakeGroupAgent(context.Background(), mustHubStore(t, m), g, w); err != nil {
		t.Fatalf("agent-origin wake: %v", err)
	}
	st := mustHubStore(t, m)
	var need *agenthub.Need
	convWait(t, "agent-origin 的 bash 也要落审批 Need", func() bool {
		needs, err := st.ListNeeds(context.Background(), agenthub.NeedFilter{GroupID: g.ID, Status: "pending"})
		if err != nil {
			return false
		}
		for _, n := range needs {
			if n.Kind == "approval" && strings.Contains(n.Title, "bash") {
				need = n
				return true
			}
		}
		return false
	})
	if need.AgentID != b.ID {
		t.Fatalf("审批归属 = %s, want %s", need.AgentID, b.ID)
	}
	if fake.callCount() != 1 {
		t.Fatalf("批准前就续跑了（callCount=%d）", fake.callCount())
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 20, "hub_need_resolve", map[string]any{
		"need_id": string(need.ID), "resolution": "approve",
	}); !ok {
		t.Fatalf("批准失败: %s", errMsg)
	}
	convWait(t, "批准后 agent-origin 的 bash 执行", func() bool { return fake.callCount() >= 2 })
}

// TestHubGroupHumanWakeOriginIsHuman：人类 @ 的唤醒 header 记 human（不是 member）。
func TestHubGroupHumanWakeOriginIsHuman(t *testing.T) {
	m, buf, g, _, b, bs := wakeOriginFixture(t, "auto")
	fake := newP3bEvalProvider(p3bEvalRound{content: "收到。"})
	setGroupFakeLoopIters(t, m, bs, g, string(b.ID), fake, 1)
	if ok, errMsg, _ := hubCmd(t, m, buf, 30, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@" + b.Name + " 在吗",
	}); !ok {
		t.Fatalf("human send: %s", errMsg)
	}
	convWait(t, "人类唤醒收尾", func() bool { return fake.callCount() >= 1 })
	headers := convWakeHeaders(fake.requestAt(0))
	if len(headers) == 0 {
		t.Fatal("人类唤醒没有 header")
	}
	h := headers[len(headers)-1]
	if h.Origin != agenthub.WakeOriginHuman {
		t.Fatalf("human header origin = %q", h.Origin)
	}
	if h.SenderAgentID != "" {
		t.Fatalf("human header 不该带 sender: %q", h.SenderAgentID)
	}
}
