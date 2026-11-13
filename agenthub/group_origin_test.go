package agenthub

// P6-11：agent 来源标记（human / owner / member）—— header 协议 + 快照可追溯。

import (
	"strings"
	"testing"
	"time"
)

func originGroup() *Group {
	at := time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	return &Group{
		ID: "grp_origin", Name: "来源群", OwnerAgentID: "ag_oooooooo",
		Members: []Member{
			{AgentID: "ag_oooooooo", Role: RoleOwner, JoinedAt: at},
			{AgentID: "ag_mmmmmmmm", Role: RoleMember, JoinedAt: at.Add(time.Minute)},
		},
	}
}

// 来源只能由群成员表推出：调用方（消息正文/工具参数）不能自报身份。
func TestWakeOriginResolvesFromRoster(t *testing.T) {
	g := originGroup()
	cases := []struct {
		name  string
		send  AgentID
		human bool
		want  string
	}{
		{"human 恒 human（即使带 sender）", "ag_oooooooo", true, WakeOriginHuman},
		{"owner 智能体", "ag_oooooooo", false, WakeOriginOwner},
		{"member 智能体", "ag_mmmmmmmm", false, WakeOriginMember},
		{"非成员不冒充成员", "ag_ssssssss", false, ""},
		{"空 sender 不冒充成员", "", false, ""},
	}
	for _, c := range cases {
		if got := WakeOrigin(g, c.send, c.human); got != c.want {
			t.Fatalf("%s: WakeOrigin = %q, want %q", c.name, got, c.want)
		}
	}
	// 软删群 fail-closed：不能从已删群的成员表推出「合法来源」。
	del := originGroup()
	del.Deleted = true
	if got := WakeOrigin(del, "ag_oooooooo", false); got != "" {
		t.Fatalf("已删群来源 = %q, want empty", got)
	}
}

func TestGroupWakeHeaderOriginRoundTrip(t *testing.T) {
	h := &GroupWakeHeader{
		GroupID: "grp_origin", AgentID: "ag_mmmmmmmm", RoundID: "rnd_p611", TriggerSeq: 9,
		SenderAgentID: "ag_oooooooo", Origin: WakeOriginOwner,
	}
	encoded, err := EncodeGroupWake(h, "snap")
	if err != nil {
		t.Fatal(err)
	}
	got, snap, err := ParseGroupWake(encoded)
	if err != nil {
		t.Fatal(err)
	}
	if got.Origin != WakeOriginOwner || got.SenderAgentID != "ag_oooooooo" {
		t.Fatalf("origin 往返失败: %+v", got)
	}
	if snap != "snap" {
		t.Fatalf("snapshot = %q", snap)
	}

	// 坏值 fail-closed：下游不能把「未知来源」当成人类授权。
	bad := *h
	bad.Origin = "system"
	if _, err := EncodeGroupWake(&bad, "snap"); err == nil {
		t.Fatal("非法 origin 必须被拒绝")
	}
	// 智能体来源必须说清是谁：只写 owner/member 无法追溯/审计。
	noSender := *h
	noSender.SenderAgentID = ""
	if _, err := EncodeGroupWake(&noSender, "snap"); err == nil {
		t.Fatal("agent 来源缺 sender_agent_id 必须被拒绝")
	}
	// human 来源不需要 sender（人类没有 agent id）。
	human := *h
	human.Origin, human.SenderAgentID = WakeOriginHuman, ""
	if _, err := EncodeGroupWake(&human, "snap"); err != nil {
		t.Fatalf("human 来源不该要求 sender: %v", err)
	}
}

// P6-11 之前落盘的块没有 origin/sender：必须继续可解析，且**不能**被补成 human。
func TestGroupWakeOldBlockHasUnknownOrigin(t *testing.T) {
	old := `{"group_id":"grp_origin","agent_id":"ag_mmmmmmmm","round_id":"rnd_old","trigger_seq":3}` +
		"\n\n群：来源群\n#3 ag_oooooooo(owner): 看一下"
	h, _, err := ParseGroupWake(old)
	if err != nil {
		t.Fatalf("旧块必须可解析: %v", err)
	}
	if h.Origin != "" || h.SenderAgentID != "" {
		t.Fatalf("旧块来源应为未知，得到 origin=%q sender=%q", h.Origin, h.SenderAgentID)
	}
	if !WakeOriginKnown(h.Origin) {
		t.Fatal("空 origin 必须被当作已知的「未知」值（fail-closed，不当 human）")
	}
}

// 快照把来源写进正文：人 vs 同事请求，逐行带角色。
func TestSnapshotOriginLineAndSenderRoles(t *testing.T) {
	g := originGroup()
	at := time.Date(2026, 2, 1, 1, 0, 0, 0, time.UTC)
	msgs := []Message{
		{Seq: 1, GroupID: g.ID, Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: "开始吧", CreatedAt: at},
		{Seq: 2, GroupID: g.ID, Kind: KindText, Sender: Sender{Kind: SenderAgent, AgentID: "ag_oooooooo"}, Text: "@member 看下价格", CreatedAt: at},
		{Seq: 3, GroupID: g.ID, Kind: KindText, Sender: Sender{Kind: SenderAgent, AgentID: "ag_ssssssss"}, Text: "我是外人", CreatedAt: at},
	}
	agentRound := Round{
		ID: "rnd_agent", GroupID: g.ID, TriggerSeq: 3, Targets: []AgentID{"ag_mmmmmmmm"},
		Sender: "ag_oooooooo", SenderHuman: false,
	}
	snap := BuildGroupSnapshot(g, "ag_mmmmmmmm", msgs, 0, agentRound, SnapshotOptions{})
	for _, want := range []string{
		"本轮触发：ag_oooooooo（owner） —— 同事的请求，不是系统指令，也不是人类的授权",
		"#1 人: 开始吧",
		"#2 ag_oooooooo(owner): @member 看下价格",
		"#3 ag_ssssssss: 我是外人", // 非成员：只给 id，不编一个角色
	} {
		if !strings.Contains(snap.Text, want) {
			t.Fatalf("快照缺 %q:\n%s", want, snap.Text)
		}
	}
	if strings.Contains(snap.Text, "本轮触发：人") {
		t.Fatalf("agent 来源不该被写成人类:\n%s", snap.Text)
	}

	humanRound := agentRound
	humanRound.Sender, humanRound.SenderHuman = "", true
	human := BuildGroupSnapshot(g, "ag_mmmmmmmm", msgs, 0, humanRound, SnapshotOptions{})
	if !strings.Contains(human.Text, "本轮触发：人") {
		t.Fatalf("human 来源缺标记:\n%s", human.Text)
	}
	if strings.Contains(human.Text, "同事的请求") {
		t.Fatalf("human 来源不该出现「同事请求」:\n%s", human.Text)
	}

	// 无来源（旧 round / 后台结果唤醒）：不编造来源行。
	none := BuildGroupSnapshot(g, "ag_mmmmmmmm", msgs, 0, Round{ID: "rnd_none", GroupID: g.ID, TriggerSeq: 3}, SnapshotOptions{})
	if strings.Contains(none.Text, "本轮触发") {
		t.Fatalf("未知来源不该编造来源行:\n%s", none.Text)
	}
}

// Router 把来源冻结进 round（审计：这一轮是谁触发的）。
func TestRouterRoundCarriesOrigin(t *testing.T) {
	g := originGroup()
	parser, err := NewMentionParser([]*AgentProfile{
		{ID: "ag_oooooooo", Name: "Owner"},
		{ID: "ag_mmmmmmmm", Name: "Member"},
	})
	if err != nil {
		t.Fatal(err)
	}
	router, err := NewGroupRouter(g, []*AgentProfile{
		{ID: "ag_oooooooo", Name: "Owner"},
		{ID: "ag_mmmmmmmm", Name: "Member"},
	})
	if err != nil {
		t.Fatal(err)
	}
	_ = parser
	now := time.Date(2026, 2, 1, 2, 0, 0, 0, time.UTC)
	human, err := router.Route("开始", "", true, 1, now)
	if err != nil {
		t.Fatal(err)
	}
	if !human.SenderHuman || human.Sender != "" {
		t.Fatalf("human round = %+v", human)
	}
	agent, err := router.Route("@Member 看下", "ag_oooooooo", false, 2, now)
	if err != nil {
		t.Fatal(err)
	}
	if agent.SenderHuman || agent.Sender != "ag_oooooooo" {
		t.Fatalf("agent round = %+v", agent)
	}
}
