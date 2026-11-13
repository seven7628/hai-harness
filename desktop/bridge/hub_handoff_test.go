package main

// P3a-06：handoff 交接卡 + 落盘 + ack + Needs you。

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
)

type handoffMember struct {
	Name string
	Role string
}

func handoffGroupFixture(t *testing.T, roles []handoffMember) (*manager, *syncBuf, []agenthub.AgentProfile, *agenthub.Group) {
	t.Helper()
	m, buf := testManager(t)
	agents := make([]agenthub.AgentProfile, 0, len(roles))
	members := make([]map[string]any, 0, len(roles))
	for i, r := range roles {
		a := createHubAgent(t, m, buf, i+1, r.Name)
		agents = append(agents, a)
		members = append(members, map[string]any{"agent_id": string(a.ID), "role": r.Role})
	}
	ok, errMsg, data := hubCmd(t, m, buf, 50, "hub_group_create", map[string]any{
		"name": "交接群", "members": members,
	})
	if !ok {
		t.Fatalf("create group: %s", errMsg)
	}
	return m, buf, agents, groupFromData(t, data)
}

// callHubToolForAgent 在**群作用域会话**里调群工具。场景1 隔离后 handoff/handoff_ack
// 只在群会话注册（DM 里注册等于让私聊往群里发起交接），所以会话必须按 groupID 取群会话。
func callHubToolForAgent(t *testing.T, m *manager, agentID agenthub.AgentID, groupID, name, args string) (string, error) {
	t.Helper()
	bs, err := m.hubGroupSession(string(agentID), groupID)
	if err != nil {
		t.Fatal(err)
	}
	tool, err := bs.hubEngine.GetTool(context.Background(), name)
	if err != nil {
		return "", err
	}
	return tool.Call(context.Background(), name, args)
}

func TestHubHandoffCardAckAndNeed(t *testing.T) {
	m, buf, agents, g := handoffGroupFixture(t, []handoffMember{
		{"HandA", "owner"}, {"HandB", "member"}, {"HandC", "observer"},
	})
	a, b, c := agents[0], agents[1], agents[2]
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	args := `{"group_id":"` + string(g.ID) + `","to_agent_id":"` + string(b.ID) + `","instruction":"把定价表写成两页简报"}`
	out, err := callHubToolForAgent(t, m, a.ID, string(g.ID), "handoff", args)
	if err != nil {
		t.Fatalf("handoff: %v", err)
	}
	var created struct {
		Handoff agenthub.Handoff `json:"handoff"`
		Seq     int64            `json:"seq"`
		NeedID  string           `json:"need_id"`
	}
	if err := json.Unmarshal([]byte(out), &created); err != nil {
		t.Fatalf("result = %s: %v", out, err)
	}
	h := created.Handoff
	if h.ID == "" || h.Status != agenthub.HandoffPendingAck || h.ChainDepth != 0 ||
		h.FromAgentID != a.ID || h.ToAgentID != b.ID {
		t.Fatalf("handoff = %+v", h)
	}
	if _, err := st.LoadHandoff(ctx, h.ID); err != nil {
		t.Fatalf("handoff not persisted: %v", err)
	}
	msgs, err := st.ReadMessages(ctx, g.ID, 0, 20)
	if err != nil || len(msgs) != 1 || msgs[0].Kind != agenthub.KindHandoff {
		t.Fatalf("card = %+v, %v", msgs, err)
	}
	if id, _ := msgs[0].Refs["handoff_id"].(string); id != h.ID {
		t.Fatalf("card refs = %+v", msgs[0].Refs)
	}
	need, err := st.LoadNeed(ctx, agenthub.NeedID(created.NeedID))
	if err != nil || need.Kind != "handoff" || need.Status != "pending" || need.Anchor != h.ID || need.AgentID != b.ID {
		t.Fatalf("need = %+v, %v", need, err)
	}
	// 拼写错误的 resolution 必须 fail-closed，不能既不改状态又把 need 消费掉。
	if ok, _, _ := hubCmd(t, m, buf, 61, "hub_need_resolve", map[string]any{
		"need_id": created.NeedID, "resolution": "acceptt",
	}); ok {
		t.Fatal("unknown handoff resolution must fail")
	}
	// 人类点「标记已读」（acknowledged）不得把 pending_ack 判成 declined；
	// 接收方之后仍必须能 accept（P3a-06 复审 B-01）。
	if ok, errMsg, _ := hubCmd(t, m, buf, 60, "hub_need_resolve", map[string]any{
		"need_id": created.NeedID, "resolution": "acknowledged",
	}); !ok {
		t.Fatalf("mark read: %s", errMsg)
	}
	if got, err := st.LoadHandoff(ctx, h.ID); err != nil || got.Status != agenthub.HandoffPendingAck {
		t.Fatalf("mark read changed handoff: %+v, %v", got, err)
	}
	// observer 不能发起；非接收方不能 ack。
	if _, err := callHubToolForAgent(t, m, c.ID, string(g.ID), "handoff", `{"group_id":"`+string(g.ID)+`","to_agent_id":"`+string(b.ID)+`","instruction":"x"}`); err == nil {
		t.Fatal("observer handoff must fail")
	}
	if _, err := callHubToolForAgent(t, m, c.ID, string(g.ID), "handoff_ack", `{"handoff_id":"`+h.ID+`","accept":true}`); err == nil {
		t.Fatal("non-recipient ack must fail")
	}
	// 接收方接受，卡片状态更新，Need resolved。
	out, err = callHubToolForAgent(t, m, b.ID, string(g.ID), "handoff_ack", `{"handoff_id":"`+h.ID+`","accept":true}`)
	if err != nil || !strings.Contains(out, string(agenthub.HandoffAccepted)) {
		t.Fatalf("ack = %s, %v", out, err)
	}
	got, err := st.LoadHandoff(ctx, h.ID)
	if err != nil || got.Status != agenthub.HandoffAccepted || got.AckedAt == nil {
		t.Fatalf("acked = %+v, %v", got, err)
	}
	need, _ = st.LoadNeed(ctx, agenthub.NeedID(created.NeedID))
	if need.Status != "resolved" || need.ResolvedAt == nil {
		t.Fatalf("need not resolved: %+v", need)
	}
	msgs, _ = st.ReadMessages(ctx, g.ID, 0, 20)
	if len(msgs) != 2 || msgs[1].Kind != agenthub.KindHandoff {
		t.Fatalf("ack card = %+v", msgs)
	}
	if status, _ := msgs[1].Refs["status"].(string); status != string(agenthub.HandoffAccepted) {
		t.Fatalf("ack card status = %+v", msgs[1].Refs)
	}
	// 幂等：重复 ack 不追加卡片。
	if _, err := callHubToolForAgent(t, m, b.ID, string(g.ID), "handoff_ack", `{"handoff_id":"`+h.ID+`","accept":true}`); err != nil {
		t.Fatalf("repeat ack: %v", err)
	}
	if msgs, _ = st.ReadMessages(ctx, g.ID, 0, 20); len(msgs) != 2 {
		t.Fatalf("repeat ack wrote card: %+v", msgs)
	}
}

func TestHubHandoffGuardsDepthRateAndDeletedTarget(t *testing.T) {
	m, buf, agents, g := handoffGroupFixture(t, []handoffMember{
		{"GuardA", "owner"}, {"GuardB", "member"}, {"GuardC", "member"},
		{"GuardD", "member"}, {"GuardE", "member"},
	})
	a, b, c, d, e := agents[0], agents[1], agents[2], agents[3], agents[4]
	st, _ := m.hubStore()
	ctx := context.Background()
	handoff := func(from, to agenthub.AgentID) (agenthub.Handoff, error) {
		out, err := callHubToolForAgent(t, m, from, string(g.ID), "handoff",
			`{"group_id":"`+string(g.ID)+`","to_agent_id":"`+string(to)+`","instruction":"chain"}`)
		var r struct {
			Handoff agenthub.Handoff `json:"handoff"`
		}
		_ = json.Unmarshal([]byte(out), &r)
		return r.Handoff, err
	}
	// 不存在的 artifact_id 不能进交接卡。
	if _, err := callHubToolForAgent(t, m, a.ID, string(g.ID), "handoff",
		`{"group_id":"`+string(g.ID)+`","to_agent_id":"`+string(b.ID)+`","instruction":"x","artifact_ids":["art_missing"]}`); err == nil {
		t.Fatal("unknown artifact id must be rejected")
	}
	// A→B→C→D 后 D→E 是第 4 跳（depth=3），必须拒绝并给出可判定错误。
	for _, pair := range [][2]agenthub.AgentID{{a.ID, b.ID}, {b.ID, c.ID}, {c.ID, d.ID}} {
		if _, err := handoff(pair[0], pair[1]); err != nil {
			t.Fatalf("chain %s→%s: %v", pair[0], pair[1], err)
		}
	}
	if _, err := handoff(d.ID, e.ID); !errors.Is(err, agenthub.ErrHandoffTooDeep) {
		t.Fatalf("4th hop err = %v", err)
	}
	// 同无序对 10 分钟 2 次：A↔B 已有一次 A→B；B→A 算第二次，A→B 第三次拒绝。
	if _, err := handoff(b.ID, a.ID); err != nil {
		t.Fatalf("second pair handoff: %v", err)
	}
	if _, err := handoff(a.ID, b.ID); !errors.Is(err, agenthub.ErrHandoffRateLimited) {
		t.Fatalf("pair rate err = %v", err)
	}
	// 软删接收方 fail-closed。
	if err := st.SaveAgent(ctx, &agenthub.AgentProfile{ID: e.ID, Name: "GuardE", Deleted: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := handoff(c.ID, e.ID); err == nil {
		t.Fatal("deleted target must fail")
	}
	_ = buf
}

// P3a-06 三轮非阻塞：接收方在握手后被软删，ack 必须 fail-closed。
func TestHubHandoffAckRejectsDeletedRecipient(t *testing.T) {
	m, buf, agents, g := handoffGroupFixture(t, []handoffMember{
		{"DelA", "owner"}, {"DelB", "member"},
	})
	a, b := agents[0], agents[1]
	st, _ := m.hubStore()
	ctx := context.Background()
	bsB, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	out, err := callHubToolForAgent(t, m, a.ID, string(g.ID), "handoff",
		`{"group_id":"`+string(g.ID)+`","to_agent_id":"`+string(b.ID)+`","instruction":"x"}`)
	if err != nil {
		t.Fatal(err)
	}
	var r struct {
		Handoff agenthub.Handoff `json:"handoff"`
	}
	_ = json.Unmarshal([]byte(out), &r)
	if err := st.SaveAgent(ctx, &agenthub.AgentProfile{ID: b.ID, Name: "DelB", Deleted: true}); err != nil {
		t.Fatal(err)
	}
	tool, err := bsB.hubEngine.GetTool(ctx, "handoff_ack")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tool.Call(ctx, "handoff_ack", `{"handoff_id":"`+r.Handoff.ID+`","accept":true}`); err == nil {
		t.Fatal("deleted recipient must not ack")
	}
	_ = buf
}
