package agenthub

// P3a-04：群上下文快照（delta / 冻结本轮 / 预算折叠 / 确定性）。

import (
	"strings"
	"testing"
	"time"
)

func snapGroup() *Group {
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return &Group{
		ID: "grp_aaaaaa", Name: "快照群", OwnerAgentID: "ag_aaaaaaaa",
		Members: []Member{
			{AgentID: "ag_aaaaaaaa", Role: RoleOwner, JoinedAt: at},
			{AgentID: "ag_bbbbbbbb", Role: RoleMember, JoinedAt: at.Add(time.Hour)},
		},
	}
}

func snapMsgs(n int) []Message {
	out := make([]Message, 0, n)
	for i := 1; i <= n; i++ {
		kind := KindText
		sender := Sender{Kind: SenderAgent, AgentID: "ag_bbbbbbbb"}
		switch i % 4 {
		case 0:
			sender = Sender{Kind: SenderHuman}
		case 1:
			sender = Sender{Kind: SenderSystem}
		}
		out = append(out, Message{Seq: int64(i), GroupID: "grp_aaaaaa", Kind: kind, Sender: sender, Text: strings.Repeat("消息", i)})
	}
	return out
}

func TestSnapshotDeltaAndFreezeRound(t *testing.T) {
	g := snapGroup()
	msgs := snapMsgs(6)
	round := Round{ID: "rnd_1", GroupID: g.ID, TriggerSeq: 5, Targets: []AgentID{"ag_aaaaaaaa"}}
	snap := BuildGroupSnapshot(g, "ag_aaaaaaaa", msgs, 2, round, SnapshotOptions{})
	if snap.FromSeq != 2 || snap.TriggerSeq != 5 {
		t.Fatalf("snap = %+v", snap)
	}
	// 只要 seq 3,4,5；6 被冻结。
	if len(snap.Messages) != 3 || snap.Messages[0].Seq != 3 || snap.Messages[2].Seq != 5 {
		t.Fatalf("messages = %+v", snap.Messages)
	}
	if !strings.Contains(snap.Text, "本轮你被 @：是") {
		t.Fatalf("text = %s", snap.Text)
	}
	// 别人被 @ / 无 round：旁听。
	round.Targets = []AgentID{"ag_bbbbbbbb"}
	snap2 := BuildGroupSnapshot(g, "ag_aaaaaaaa", msgs, 2, round, SnapshotOptions{})
	if !strings.Contains(snap2.Text, "只读旁听") {
		t.Fatalf("text2 = %s", snap2.Text)
	}
}

func TestSnapshotBudgetFolding(t *testing.T) {
	g := snapGroup()
	msgs := snapMsgs(50)
	snap := BuildGroupSnapshot(g, "ag_aaaaaaaa", msgs, 0, Round{}, SnapshotOptions{})
	if len(snap.Messages) != 40 || snap.Folded != 10 {
		t.Fatalf("kept=%d folded=%d", len(snap.Messages), snap.Folded)
	}
	if !strings.Contains(snap.Text, "[已折叠 10 条较早消息]") {
		t.Fatalf("text = %s", snap.Text)
	}
	// rune 预算：4 条超长消息只留最新能装下的。
	long := make([]Message, 0, 4)
	for i := 0; i < 4; i++ {
		long = append(long, Message{Seq: int64(i + 1), Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: strings.Repeat("长", 100)})
	}
	snap = BuildGroupSnapshot(g, "ag_aaaaaaaa", long, 0, Round{}, SnapshotOptions{MaxRunes: 250})
	if len(snap.Messages) != 2 || snap.Folded != 2 {
		t.Fatalf("rune budget kept=%d folded=%d", len(snap.Messages), snap.Folded)
	}
	if len([]rune(snap.Text)) > 400 {
		t.Fatalf("text too long: %d", len([]rune(snap.Text)))
	}
}

func TestSnapshotExcludesRoundSummaryAndEmpty(t *testing.T) {
	g := snapGroup()
	msgs := []Message{
		{Seq: 1, Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: "正常"},
		{Seq: 2, Kind: KindRoundSummary, Sender: Sender{Kind: SenderSystem}, Text: "回执"},
		{Seq: 3, Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: "   "},
	}
	snap := BuildGroupSnapshot(g, "ag_aaaaaaaa", msgs, 0, Round{}, SnapshotOptions{})
	if len(snap.Messages) != 1 || snap.Messages[0].Seq != 1 {
		t.Fatalf("messages = %+v", snap.Messages)
	}
	if strings.Contains(snap.Text, "回执") {
		t.Fatalf("round summary leaked: %s", snap.Text)
	}
	// 同输入同输出。
	again := BuildGroupSnapshot(g, "ag_aaaaaaaa", msgs, 0, Round{}, SnapshotOptions{})
	if again.Text != snap.Text {
		t.Fatal("snapshot not deterministic")
	}
}
