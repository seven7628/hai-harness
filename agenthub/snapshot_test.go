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
		{Seq: 4, Kind: KindApprovalStub, Sender: Sender{Kind: SenderSystem}, Text: "审批占位"},
		{Seq: 5, Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: "私密", Visibility: VisibilityOwner},
	}
	snap := BuildGroupSnapshot(g, "ag_aaaaaaaa", msgs, 0, Round{}, SnapshotOptions{})
	if len(snap.Messages) != 1 || snap.Messages[0].Seq != 1 {
		t.Fatalf("messages = %+v", snap.Messages)
	}
	if strings.Contains(snap.Text, "回执") || strings.Contains(snap.Text, "审批占位") || strings.Contains(snap.Text, "私密") {
		t.Fatalf("receipt/approval/owner-only leaked: %s", snap.Text)
	}
	// 同输入同输出。
	again := BuildGroupSnapshot(g, "ag_aaaaaaaa", msgs, 0, Round{}, SnapshotOptions{})
	if again.Text != snap.Text {
		t.Fatal("snapshot not deterministic")
	}
}

// P3a-04 非阻塞：快照返回的 Messages 必须是深拷贝，调用方改动不能污染群日志。
func TestSnapshotMessagesAreDeepCopied(t *testing.T) {
	g := snapGroup()
	msg := Message{
		Seq: 1, GroupID: g.ID, Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: "hi",
		Mentions: []AgentID{"ag_bbbbbbbb"},
		Context: &TurnContext{
			TriggerSeqs: []int64{1},
			Senders:     []AgentRef{{AgentID: "ag_bbbbbbbb", Name: "b"}},
			Reason:      "multi_agent",
		},
		Refs: map[string]any{"k": "v"},
	}
	snap := BuildGroupSnapshot(g, "ag_aaaaaaaa", []Message{msg}, 0, Round{}, SnapshotOptions{})
	if len(snap.Messages) != 1 {
		t.Fatalf("messages = %+v", snap.Messages)
	}
	snap.Messages[0].Mentions[0] = "ag_mutated"
	snap.Messages[0].Context.TriggerSeqs[0] = 99
	snap.Messages[0].Context.Senders[0].Name = "mutated"
	snap.Messages[0].Refs["k"] = "mutated"

	if msg.Mentions[0] != "ag_bbbbbbbb" || msg.Context.TriggerSeqs[0] != 1 ||
		msg.Context.Senders[0].Name != "b" || msg.Refs["k"] != "v" {
		t.Fatalf("snapshot aliased source message: %+v", msg)
	}
}

// P3a-04 复审 B1：单条消息本身超预算时也必须截断，12k 是硬上限。
func TestSnapshotSingleOversizeMessageIsHardCapped(t *testing.T) {
	g := snapGroup()
	msgs := []Message{{
		Seq: 1, GroupID: g.ID, Kind: KindText,
		Sender: Sender{Kind: SenderHuman}, Text: strings.Repeat("巨", 20000),
	}}
	snap := BuildGroupSnapshot(g, "ag_aaaaaaaa", msgs, 0, Round{}, SnapshotOptions{})
	if !snap.Truncated {
		t.Fatal("oversize single message must set Truncated")
	}
	if got := len([]rune(snap.Text)); got > DefaultSnapshotRunes {
		t.Fatalf("snapshot text = %d runes, want <= %d", got, DefaultSnapshotRunes)
	}
	if len(snap.Messages) != 1 || !strings.HasSuffix(snap.Messages[0].Text, "…") {
		t.Fatalf("message not truncated: %+v", snap.Messages)
	}
	// 多条的估算也必须按真实行头计，最终文本不得超预算。
	many := make([]Message, 0, 10)
	for i := 0; i < 10; i++ {
		many = append(many, Message{Seq: int64(i + 1), GroupID: g.ID, Kind: KindText,
			Sender: Sender{Kind: SenderAgent, AgentID: "ag_bbbbbbbb"}, Text: strings.Repeat("边", 1192)})
	}
	snap = BuildGroupSnapshot(g, "ag_aaaaaaaa", many, 0, Round{}, SnapshotOptions{})
	if got := len([]rune(snap.Text)); got > DefaultSnapshotRunes {
		t.Fatalf("boundary snapshot text = %d runes, want <= %d", got, DefaultSnapshotRunes)
	}
	// 块头（群/成员行）也计入预算：超额大于最旧正文时不能整段跳过截断。
	header := []Message{
		{Seq: 1, GroupID: g.ID, Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: "同意"},
		{Seq: 2, GroupID: g.ID, Kind: KindText, Sender: Sender{Kind: SenderAgent, AgentID: "ag_bbbbbbbb"},
			Text: strings.Repeat("长", 11940)},
	}
	snap = BuildGroupSnapshot(g, "ag_aaaaaaaa", header, 0, Round{}, SnapshotOptions{})
	if got := len([]rune(snap.Text)); got > DefaultSnapshotRunes {
		t.Fatalf("header overflow text = %d runes, want <= %d", got, DefaultSnapshotRunes)
	}
	if len(snap.Messages) != 1 || snap.Messages[0].Seq != 2 || snap.Folded != 1 {
		t.Fatalf("oldest must be folded, newest kept: msgs=%+v folded=%d truncated=%v", snap.Messages, snap.Folded, snap.Truncated)
	}
	// 超额小于最旧正文时截断最旧、保留最新（不能整条丢，也不能截错人）。
	mix := []Message{
		{Seq: 1, GroupID: g.ID, Kind: KindText, Sender: Sender{Kind: SenderAgent, AgentID: "ag_bbbbbbbb"},
			Text: strings.Repeat("旧", 9000)},
		{Seq: 2, GroupID: g.ID, Kind: KindText, Sender: Sender{Kind: SenderAgent, AgentID: "ag_bbbbbbbb"},
			Text: strings.Repeat("新", 4000)},
	}
	snap = BuildGroupSnapshot(g, "ag_aaaaaaaa", mix, 0, Round{}, SnapshotOptions{})
	if len(snap.Messages) != 2 || snap.Folded != 0 {
		t.Fatalf("mix should keep both and truncate oldest: msgs=%d folded=%d", len(snap.Messages), snap.Folded)
	}
	if got := len([]rune(snap.Messages[1].Text)); got != 4000 {
		t.Fatalf("newest message truncated: %d runes, want 4000", got)
	}
	if got := len([]rune(snap.Messages[0].Text)); got >= 9000 {
		t.Fatalf("oldest not truncated: %d runes", got)
	}
	// 极小预算连块头都放不下：仍必须硬切到上限。
	tiny := BuildGroupSnapshot(g, "ag_aaaaaaaa", header, 0, Round{}, SnapshotOptions{MaxRunes: 20})
	if got := len([]rune(tiny.Text)); got > 20 {
		t.Fatalf("tiny budget text = %d, want <= 20", got)
	}
	if !strings.HasSuffix(tiny.Text, "…") {
		t.Fatalf("tiny hard cut must keep ellipsis: %q", tiny.Text)
	}
}

// P3b-06：B 写的 task_result 卡对旁观者 C 可见（快照从日志来，不需要任何唤醒）。
// 路由回执/轮次摘要仍被排除 —— 它们只是人类 UI 的投影，正文可能含敏感信息。
func TestSnapshotIncludesTaskResultForNonDispatcher(t *testing.T) {
	g := &Group{
		ID: "grp_snaptask", Name: "任务群", OwnerAgentID: "ag_aaaaaaaa",
		Members: []Member{
			{AgentID: "ag_aaaaaaaa", Role: RoleOwner, JoinedAt: testTime()},
			{AgentID: "ag_bbbbbbbb", Role: RoleMember, JoinedAt: testTime()},
			{AgentID: "ag_cccccccc", Role: RoleMember, JoinedAt: testTime()},
		},
	}
	card := Message{
		Seq: 3, GroupID: g.ID, Kind: KindTaskResult,
		Sender:     Sender{Kind: SenderAgent, AgentID: "ag_bbbbbbbb"},
		Text:       "价格表出来了：三家",
		Visibility: VisibilityGroup, CreatedAt: testTime(),
		TaskID: "task_1",
		Refs:   map[string]any{"task_id": "task_1", "status": string(TaskCompleted)},
	}
	msgs := []Message{
		{Seq: 1, GroupID: g.ID, Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: "开始整理价格表",
			Visibility: VisibilityGroup, CreatedAt: testTime()},
		{Seq: 2, GroupID: g.ID, Kind: KindRouting, Sender: Sender{Kind: SenderSystem}, Text: "回执：唤醒 B",
			Visibility: VisibilityGroup, CreatedAt: testTime()},
		card,
		{Seq: 4, GroupID: g.ID, Kind: KindRoundSummary, Sender: Sender{Kind: SenderSystem}, Text: "本轮：B 完成",
			Visibility: VisibilityGroup, CreatedAt: testTime()},
	}
	// C 不是本轮被 @ 的人，也没有任何唤醒：快照只按游标 delta 构建。
	snap := BuildGroupSnapshot(g, "ag_cccccccc", msgs, 1, Round{}, SnapshotOptions{})
	if len(snap.Messages) != 1 || snap.Messages[0].Seq != 3 {
		t.Fatalf("snapshot messages = %+v, want only the task_result card", snap.Messages)
	}
	got := snap.Messages[0]
	if got.Kind != KindTaskResult || got.TaskID != "task_1" ||
		got.Refs["status"] != string(TaskCompleted) {
		t.Fatalf("task_result card lost fields: %+v", got)
	}
	// P6-11：发送者行带落盘角色（owner/member），来源逐行可追溯。
	if !strings.Contains(snap.Text, "#3 ag_bbbbbbbb(member): 价格表出来了：三家") {
		t.Fatalf("snapshot text = %q", snap.Text)
	}
	if strings.Contains(snap.Text, "回执") || strings.Contains(snap.Text, "本轮：B") {
		t.Fatalf("UI-only messages leaked into snapshot: %q", snap.Text)
	}
}
