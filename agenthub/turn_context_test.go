package agenthub

// P3b-07 B6：群回复触发上下文的纯函数单测（TurnContextLine 的数据源）。

import (
	"testing"
	"time"
)

func ctxMsg(seq int64, sender Sender, text string, kind MessageKind, visibility Visibility) Message {
	return Message{
		Seq: seq, Sender: sender, Text: text, Kind: kind, Visibility: visibility,
		CreatedAt: time.Now().UTC(),
	}
}

func TestBuildTurnContextMultiAgentWindow(t *testing.T) {
	a, b, c := AgentID("ag_aaaa"), AgentID("ag_bbbb"), AgentID("ag_cccc")
	human := Sender{Kind: SenderHuman}
	msgs := []Message{
		ctxMsg(1, human, "@B 看一下", KindText, VisibilityGroup),
		ctxMsg(2, Sender{Kind: SenderAgent, AgentID: c}, "C 的背景", KindText, VisibilityGroup),
		ctxMsg(3, Sender{Kind: SenderAgent, AgentID: a}, "@B 复核", KindText, VisibilityGroup),
		ctxMsg(4, Sender{Kind: SenderAgent, AgentID: b}, "B 自己之前的话", KindText, VisibilityGroup),
		ctxMsg(5, human, "targets=ag_a", KindRouting, VisibilityGroup),          // UI-only
		ctxMsg(6, Sender{Kind: SenderAgent, AgentID: a}, "owner-only", KindText, VisibilityOwner), // 不可见
		ctxMsg(7, Sender{Kind: SenderAgent, AgentID: c}, "   ", KindText, VisibilityGroup),        // 空正文
		ctxMsg(8, Sender{Kind: SenderAgent, AgentID: a}, "窗口外", KindText, VisibilityGroup),
	}
	ctx := BuildTurnContext(b, msgs, 0, 3)
	if ctx == nil {
		t.Fatal("multi-agent window must produce context")
	}
	if len(ctx.Senders) != 2 || ctx.Senders[0].AgentID != c || ctx.Senders[1].AgentID != a {
		t.Fatalf("senders = %+v, want C then A", ctx.Senders)
	}
	if len(ctx.TriggerSeqs) != 2 || ctx.TriggerSeqs[0] != 2 || ctx.TriggerSeqs[1] != 3 {
		t.Fatalf("trigger_seqs = %v, want [2 3]", ctx.TriggerSeqs)
	}
	if ctx.Reason != "multi_agent" {
		t.Fatalf("reason = %q, want multi_agent", ctx.Reason)
	}

	// 游标推进：只看到 A 的 @，C 的消息已读过 → 只剩 A。
	one := BuildTurnContext(b, msgs, 2, 3)
	if one == nil || len(one.Senders) != 1 || one.Senders[0].AgentID != a {
		t.Fatalf("fromSeq=2 context = %+v, want only A", one)
	}
	if one.Reason != "" {
		t.Fatalf("single sender reason = %q, want empty", one.Reason)
	}
}

func TestBuildTurnContextSkipsHumanAndSelf(t *testing.T) {
	a, b := AgentID("ag_aaaa"), AgentID("ag_bbbb")
	msgs := []Message{
		ctxMsg(1, Sender{Kind: SenderHuman}, "@B 你好", KindText, VisibilityGroup),
		ctxMsg(2, Sender{Kind: SenderAgent, AgentID: b}, "B 自己的旧回复", KindText, VisibilityGroup),
	}
	if ctx := BuildTurnContext(b, msgs, 0, 2); ctx != nil {
		t.Fatalf("human/self only context = %+v, want nil", ctx)
	}
	// 空的窗口也必须 nil，而不是空结构体。
	if ctx := BuildTurnContext(a, msgs, 2, 2); ctx != nil {
		t.Fatalf("empty window = %+v, want nil", ctx)
	}
}
