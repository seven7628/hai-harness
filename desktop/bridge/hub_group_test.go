package main

// P3a-01：群组 CRUD + 成员/角色/策略。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

func groupFromData(t *testing.T, data map[string]any) *agenthub.Group {
	t.Helper()
	raw, _ := json.Marshal(data)
	var got struct {
		Group *agenthub.Group `json:"group"`
	}
	if err := json.Unmarshal(raw, &got); err != nil || got.Group == nil {
		t.Fatalf("group payload = %s (%v)", raw, err)
	}
	return got.Group
}

func TestHubGroupCRUDAndRoles(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "GroupA")
	b := createHubAgent(t, m, buf, 2, "GroupB")
	ok, errMsg, data := hubCmd(t, m, buf, 3, "hub_group_create", map[string]any{
		"name": "复盘群",
		"members": []map[string]any{
			{"agent_id": string(a.ID), "role": "owner"},
			{"agent_id": string(b.ID), "role": "member"},
		},
	})
	if !ok {
		t.Fatalf("create: %s", errMsg)
	}
	g := groupFromData(t, data)
	if g.Policy.Mode != agenthub.GroupModeAsk {
		t.Fatalf("default mode = %q, want ask", g.Policy.Mode)
	}
	if len(g.Members) != 2 || g.OwnerAgentID != a.ID {
		t.Fatalf("group = %+v", g)
	}
	// 重复成员：领域校验拒绝（同时保持 owner 唯一）。
	ok, errMsg, _ = hubCmd(t, m, buf, 4, "hub_group_create", map[string]any{
		"name": "坏群",
		"members": []map[string]any{
			{"agent_id": string(a.ID), "role": "owner"},
			{"agent_id": string(a.ID), "role": "member"},
		},
	})
	if ok {
		t.Fatal("duplicate member must be rejected")
	}
	// 成员上限 12（13 个不同智能体；用不存在的 id 也应先在数量上被拒）。
	members := make([]map[string]any, 0, 13)
	for i := 0; i < 13; i++ {
		members = append(members, map[string]any{"agent_id": fmt.Sprintf("ag_%06d", i+100), "role": "member"})
	}
	members[0]["role"] = "owner"
	ok, errMsg, _ = hubCmd(t, m, buf, 5, "hub_group_create", map[string]any{"name": "大群", "members": members})
	if ok || errMsg == "" {
		t.Fatalf("13 members must be rejected: %s", errMsg)
	}
	// 观察者角色 + 成员移除。
	ok, errMsg, data = hubCmd(t, m, buf, 6, "hub_group_member_add", map[string]any{
		"group_id": string(g.ID), "agent_id": string(b.ID), "role": "observer",
	})
	if ok {
		t.Fatalf("adding existing member must fail: %s", errMsg)
	}
	if ok, errMsg, data = hubCmd(t, m, buf, 7, "hub_group_member_remove", map[string]any{
		"group_id": string(g.ID), "agent_id": string(b.ID),
	}); !ok {
		t.Fatalf("remove member: %s", errMsg)
	}
	g = groupFromData(t, data)
	if len(g.Members) != 1 {
		t.Fatalf("members after remove = %d", len(g.Members))
	}
	// owner 不能被移除。
	ok, _, _ = hubCmd(t, m, buf, 8, "hub_group_member_remove", map[string]any{
		"group_id": string(g.ID), "agent_id": string(a.ID),
	})
	if ok {
		t.Fatal("owner remove must fail")
	}
	// 加回 b 并转让 owner：角色成对改写。
	if ok, errMsg, _ := hubCmd(t, m, buf, 9, "hub_group_member_add", map[string]any{
		"group_id": string(g.ID), "agent_id": string(b.ID), "role": "member",
	}); !ok {
		t.Fatalf("re-add member: %s", errMsg)
	}
	ok, errMsg, data = hubCmd(t, m, buf, 10, "hub_group_update", map[string]any{
		"group_id": string(g.ID), "patch": map[string]any{"owner_agent_id": string(b.ID), "pinned": true},
	})
	if !ok {
		t.Fatalf("transfer owner: %s", errMsg)
	}
	g = groupFromData(t, data)
	if g.OwnerAgentID != b.ID || !g.Pinned {
		t.Fatalf("after transfer = %+v", g)
	}
	roles := map[agenthub.AgentID]agenthub.MemberRole{}
	for _, m := range g.Members {
		roles[m.AgentID] = m.Role
	}
	if roles[a.ID] != agenthub.RoleMember || roles[b.ID] != agenthub.RoleOwner {
		t.Fatalf("roles = %+v", roles)
	}
	// 删除后 list 不再出现。
	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_group_delete", map[string]any{"group_id": string(g.ID)}); !ok {
		t.Fatalf("delete: %s", errMsg)
	}
	ok, errMsg, data = hubCmd(t, m, buf, 12, "hub_group_list", map[string]any{})
	if !ok {
		t.Fatalf("list: %s", errMsg)
	}
	var listed struct {
		Groups []*agenthub.Group `json:"groups"`
	}
	raw, _ := json.Marshal(data)
	_ = json.Unmarshal(raw, &listed)
	for _, lg := range listed.Groups {
		if lg.ID == g.ID {
			t.Fatal("deleted group still listed")
		}
	}
}

func TestHubGroupRejectsDeletedMember(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "LiveA")
	b := createHubAgent(t, m, buf, 2, "GoneB")
	if ok, errMsg, _ := hubCmd(t, m, buf, 3, "hub_agent_delete", map[string]any{"agent_id": string(b.ID)}); !ok {
		t.Fatalf("delete agent: %s", errMsg)
	}
	ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_group_create", map[string]any{
		"name": "带删除成员",
		"members": []map[string]any{
			{"agent_id": string(a.ID), "role": "owner"},
			{"agent_id": string(b.ID), "role": "member"},
		},
	})
	if ok || errMsg == "" {
		t.Fatal("deleted agent must not join a group")
	}
}

func TestHubGroupHistoryAndRoundSummary(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "HistA")
	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_group_create", map[string]any{
		"name": "历史群", "members": []map[string]any{{"agent_id": string(a.ID), "role": "owner"}},
	})
	if !ok {
		t.Fatalf("create: %s", errMsg)
	}
	g := groupFromData(t, data)
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for i := 0; i < 3; i++ {
		if _, err := agenthub.AppendGroupMessage(ctx, st, &agenthub.Message{
			GroupID: g.ID, Sender: agenthub.Sender{Kind: agenthub.SenderHuman}, Text: "历史",
		}); err != nil {
			t.Fatal(err)
		}
	}
	summary := agenthub.BuildRoundSummary(g.ID, "rnd_1", []agenthub.RoundItem{
		{AgentID: a.ID, Tool: "bash", Status: "done", Summary: "ok"},
	}, time.Now())
	if _, err := agenthub.AppendGroupMessage(ctx, st, &summary); err != nil {
		t.Fatal(err)
	}
	ok, errMsg, data = hubCmd(t, m, buf, 3, "hub_group_history", map[string]any{"group_id": string(g.ID), "limit": 2})
	if !ok {
		t.Fatalf("history: %s", errMsg)
	}
	var page struct {
		Messages []agenthub.Message `json:"messages"`
		HasMore  bool               `json:"has_more"`
	}
	raw, _ := json.Marshal(data)
	_ = json.Unmarshal(raw, &page)
	if len(page.Messages) != 2 || !page.HasMore {
		t.Fatalf("page = %s", raw)
	}
	if page.Messages[len(page.Messages)-1].Kind != agenthub.KindRoundSummary {
		t.Fatalf("latest message kind = %s, want round_summary", page.Messages[len(page.Messages)-1].Kind)
	}
	if !strings.Contains(page.Messages[len(page.Messages)-1].Text, "bash") {
		t.Fatalf("round summary text = %q", page.Messages[len(page.Messages)-1].Text)
	}
	// limit=0 会被 store 钳成默认值；has_more 只能用生效分页判断，不能拿 0 比较。
	ok, errMsg, data = hubCmd(t, m, buf, 4, "hub_group_history", map[string]any{"group_id": string(g.ID), "limit": 0})
	if !ok {
		t.Fatalf("history limit=0: %s", errMsg)
	}
	raw, _ = json.Marshal(data)
	page = struct {
		Messages []agenthub.Message `json:"messages"`
		HasMore  bool               `json:"has_more"`
	}{}
	_ = json.Unmarshal(raw, &page)
	if len(page.Messages) == 0 || page.HasMore {
		t.Fatalf("limit=0 page = %s, want non-empty and has_more=false", raw)
	}
	// 请求超过 store 上限（500）不能被误判成「还有更早页」。
	ok, errMsg, data = hubCmd(t, m, buf, 5, "hub_group_history", map[string]any{"group_id": string(g.ID), "limit": 1000})
	if !ok {
		t.Fatalf("history limit=1000: %s", errMsg)
	}
	raw, _ = json.Marshal(data)
	page = struct {
		Messages []agenthub.Message `json:"messages"`
		HasMore  bool               `json:"has_more"`
	}{}
	_ = json.Unmarshal(raw, &page)
	if len(page.Messages) != 4 || page.HasMore {
		t.Fatalf("limit=1000 page = %s, want 4 and has_more=false", raw)
	}
}

func TestGroupHistoryHasMoreUsesEffectivePage(t *testing.T) {
	if groupHistoryHasMore(nil) {
		t.Fatal("empty page must not claim more")
	}
	if groupHistoryHasMore([]agenthub.Message{{Seq: 1}, {Seq: 2}}) {
		t.Fatal("page starting at seq 1 has no older page")
	}
	if !groupHistoryHasMore([]agenthub.Message{{Seq: 501}, {Seq: 502}}) {
		t.Fatal("page starting after seq 1 must report older pages")
	}
}

func TestHubGroupSendRoutesAndReceipts(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "SendA")
	b := createHubAgent(t, m, buf, 2, "SendB")
	ok, errMsg, data := hubCmd(t, m, buf, 3, "hub_group_create", map[string]any{
		"name": "发言群", "members": []map[string]any{
			{"agent_id": string(a.ID), "role": "owner"},
			{"agent_id": string(b.ID), "role": "member"},
		},
	})
	if !ok {
		t.Fatalf("create: %s", errMsg)
	}
	g := groupFromData(t, data)
	buf.Reset()
	ok, errMsg, data = hubCmd(t, m, buf, 4, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@SendB 看一下日志", "client_msg_id": "human-1",
	})
	if !ok {
		t.Fatalf("send: %s", errMsg)
	}
	var sent struct {
		Seq     int64              `json:"seq"`
		Targets []agenthub.AgentID `json:"targets"`
		Round   agenthub.Round     `json:"round"`
	}
	raw, _ := json.Marshal(data)
	_ = json.Unmarshal(raw, &sent)
	if sent.Seq != 1 || len(sent.Targets) != 1 || sent.Targets[0] != b.ID {
		t.Fatalf("sent = %s", raw)
	}
	if !strings.Contains(buf.String(), `"hub_round"`) || !strings.Contains(buf.String(), `"hub_message"`) {
		t.Fatalf("events = %s", buf.String())
	}
	// 日志 = 人这条 + 一条 routing_receipt 系统消息（P3a-03 B-1）。
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	msgs, err := agenthub.ReadGroupLog(context.Background(), st, g.ID, 0, 10)
	if err != nil || len(msgs) != 2 || msgs[0].Sender.Kind != agenthub.SenderHuman ||
		msgs[1].Kind != agenthub.KindRouting || msgs[1].RoundID != sent.Round.ID {
		t.Fatalf("msgs = %+v, %v", msgs, err)
	}
	acts, err := st.ReadActivity(context.Background(), g.ID, 0, 10)
	if err != nil || len(acts) != 1 || acts[0].Kind != "routing" || acts[0].AfterSeq != 2 {
		t.Fatalf("activity = %+v, %v", acts, err)
	}
	// 用「最新消息 seq」当活动游标：回执 activity 不能被跳过（B-4 的游标语义）。
	if page, err := st.ReadActivity(context.Background(), g.ID, msgs[len(msgs)-1].Seq, 10); err != nil || len(page) != 0 {
		t.Fatalf("activity after latest = %+v, %v", page, err)
	}
	// 同 client_msg_id 重试：不双写、不再发事件（B-2）。
	buf.Reset()
	ok, errMsg, data = hubCmd(t, m, buf, 41, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "@SendB 看一下日志", "client_msg_id": "human-1",
	})
	if !ok {
		t.Fatalf("dedupe resend: %s", errMsg)
	}
	var again struct {
		Seq          int64 `json:"seq"`
		Deduplicated bool  `json:"deduplicated"`
	}
	raw, _ = json.Marshal(data)
	_ = json.Unmarshal(raw, &again)
	if again.Seq != 1 || !again.Deduplicated {
		t.Fatalf("dedupe response = %s", raw)
	}
	// 重试不能返回本次新 Route 的 round：落盘回执属于首次请求（P3a-03 复审 NB-2）。
	if strings.Contains(string(raw), `"round"`) || strings.Contains(string(raw), `"targets"`) {
		t.Fatalf("dedupe response carries fresh round: %s", raw)
	}
	if strings.Contains(buf.String(), `"hub_message"`) || strings.Contains(buf.String(), `"hub_round"`) {
		t.Fatalf("dedupe emitted events: %s", buf.String())
	}
	if msgs, err = agenthub.ReadGroupLog(context.Background(), st, g.ID, 0, 10); err != nil || len(msgs) != 2 {
		t.Fatalf("dedupe wrote log: %+v, %v", msgs, err)
	}
	// 无 @：不唤醒任何人，也不发 hub_round（否则前端误报「无人被唤醒」）。
	buf.Reset()
	if ok, errMsg, data = hubCmd(t, m, buf, 5, "hub_group_send", map[string]any{
		"group_id": string(g.ID), "text": "只同步，不点名",
	}); !ok {
		t.Fatalf("send2: %s", errMsg)
	}
	var sent2 struct {
		Targets []agenthub.AgentID `json:"targets"`
	}
	raw, _ = json.Marshal(data)
	_ = json.Unmarshal(raw, &sent2)
	if len(sent2.Targets) != 0 {
		t.Fatalf("no-mention targets = %v", sent2.Targets)
	}
	if strings.Contains(buf.String(), `"hub_round"`) {
		t.Fatalf("no-mention emitted hub_round: %s", buf.String())
	}
	// 无 @ 只写消息，不写回执（设计 §6.6.3；bridge 判定行必须有回归）。
	if msgs, err = agenthub.ReadGroupLog(context.Background(), st, g.ID, 0, 10); err != nil || len(msgs) != 3 {
		t.Fatalf("no-mention log = %+v, %v", msgs, err)
	}
	if acts, err = st.ReadActivity(context.Background(), g.ID, 0, 10); err != nil || len(acts) != 1 {
		t.Fatalf("no-mention wrote receipt: %+v, %v", acts, err)
	}
}

// P3a-01 复审 B1–B4：软删群写保护、policy 字段级 patch、删除 owner 后可转让、
// create 的 member_ids/默认 owner 契约。
func TestHubGroupPatchDeleteAndCreateContracts(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "ContractA")
	b := createHubAgent(t, m, buf, 2, "ContractB")
	c := createHubAgent(t, m, buf, 3, "ContractC")
	d := createHubAgent(t, m, buf, 4, "ContractD")

	// B4：member_ids 简写 + 默认第一个成员为 owner。
	ok, errMsg, data := hubCmd(t, m, buf, 10, "hub_group_create", map[string]any{
		"name": "契约群", "member_ids": []string{string(a.ID), string(b.ID)},
	})
	if !ok {
		t.Fatalf("member_ids create: %s", errMsg)
	}
	g := groupFromData(t, data)
	if g.OwnerAgentID != a.ID || len(g.Members) != 2 {
		t.Fatalf("member_ids group = %+v", g)
	}
	for _, member := range g.Members {
		if member.AgentID == a.ID && member.Role != agenthub.RoleOwner {
			t.Fatalf("first member must become owner: %+v", g.Members)
		}
	}
	// 显式 owner + 无 role 的 members：owner 角色要自动补齐。
	ok, errMsg, data = hubCmd(t, m, buf, 11, "hub_group_create", map[string]any{
		"name": "显式owner群", "owner_agent_id": string(b.ID),
		"members": []map[string]any{{"agent_id": string(a.ID)}, {"agent_id": string(b.ID)}},
	})
	if !ok {
		t.Fatalf("explicit owner create: %s", errMsg)
	}
	if got := groupFromData(t, data); got.OwnerAgentID != b.ID {
		t.Fatalf("explicit owner = %+v", got)
	}

	// B2：policy 字段级 patch，未传字段不得重置。
	ok, errMsg, data = hubCmd(t, m, buf, 12, "hub_group_create", map[string]any{
		"name": "策略群",
		"members": []map[string]any{
			{"agent_id": string(a.ID), "role": "owner"},
			{"agent_id": string(b.ID), "role": "member"},
		},
		"policy": map[string]any{"mode": "ask", "max_parallel_runs": 5, "max_turns_per_human": 6, "daily_budget_usd": 10},
	})
	if !ok {
		t.Fatalf("policy group create: %s", errMsg)
	}
	policyGroup := groupFromData(t, data)
	ok, errMsg, data = hubCmd(t, m, buf, 13, "hub_group_update", map[string]any{
		"group_id": string(policyGroup.ID), "patch": map[string]any{"policy": map[string]any{"mode": "auto"}},
	})
	if !ok {
		t.Fatalf("policy patch: %s", errMsg)
	}
	gp := groupFromData(t, data)
	if gp.Policy.Mode != agenthub.GroupModeAuto || gp.Policy.MaxParallelRuns != 5 ||
		gp.Policy.MaxTurnsPerHuman != 6 || gp.Policy.DailyBudgetUSD != 10 {
		t.Fatalf("policy patch reset fields: %+v", gp.Policy)
	}

	// B3：删除 owner 后仍能改名/加成员/转让，转让后再移除旧 owner。
	ok, errMsg, data = hubCmd(t, m, buf, 14, "hub_group_create", map[string]any{
		"name": "孤儿群",
		"members": []map[string]any{
			{"agent_id": string(a.ID), "role": "owner"},
			{"agent_id": string(b.ID), "role": "member"},
		},
	})
	if !ok {
		t.Fatalf("orphan group create: %s", errMsg)
	}
	orphan := groupFromData(t, data)
	if ok, errMsg, _ := hubCmd(t, m, buf, 15, "hub_agent_delete", map[string]any{"agent_id": string(a.ID)}); !ok {
		t.Fatalf("delete owner: %s", errMsg)
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 16, "hub_group_update", map[string]any{
		"group_id": string(orphan.ID), "patch": map[string]any{"name": "改名群"},
	}); !ok {
		t.Fatalf("rename with deleted owner: %s", errMsg)
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 17, "hub_group_member_add", map[string]any{
		"group_id": string(orphan.ID), "agent_id": string(c.ID), "role": "observer",
	}); !ok {
		t.Fatalf("add member with deleted owner: %s", errMsg)
	}
	ok, errMsg, data = hubCmd(t, m, buf, 18, "hub_group_update", map[string]any{
		"group_id": string(orphan.ID), "patch": map[string]any{"owner_agent_id": string(b.ID)},
	})
	if !ok {
		t.Fatalf("transfer from deleted owner: %s", errMsg)
	}
	transferred := groupFromData(t, data)
	if transferred.OwnerAgentID != b.ID {
		t.Fatalf("owner after transfer = %+v", transferred)
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 19, "hub_group_member_remove", map[string]any{
		"group_id": string(orphan.ID), "agent_id": string(a.ID),
	}); !ok {
		t.Fatalf("remove old deleted owner: %s", errMsg)
	}

	// B1：软删群只能读列表/幂等删除，get/update/member_add/remove 全部拒绝。
	ok, errMsg, data = hubCmd(t, m, buf, 20, "hub_group_create", map[string]any{
		"name": "待删群",
		"members": []map[string]any{
			{"agent_id": string(d.ID), "role": "owner"},
			{"agent_id": string(b.ID), "role": "member"},
		},
	})
	if !ok {
		t.Fatalf("delete group create: %s", errMsg)
	}
	dead := groupFromData(t, data)
	if ok, errMsg, _ := hubCmd(t, m, buf, 21, "hub_group_delete", map[string]any{"group_id": string(dead.ID)}); !ok {
		t.Fatalf("delete group: %s", errMsg)
	}
	if ok, _, _ := hubCmd(t, m, buf, 22, "hub_group_get", map[string]any{"group_id": string(dead.ID)}); ok {
		t.Fatal("get deleted group must fail")
	}
	if ok, _, _ := hubCmd(t, m, buf, 23, "hub_group_update", map[string]any{
		"group_id": string(dead.ID), "patch": map[string]any{"name": "复活群"},
	}); ok {
		t.Fatal("update deleted group must fail")
	}
	if ok, _, _ := hubCmd(t, m, buf, 24, "hub_group_member_add", map[string]any{
		"group_id": string(dead.ID), "agent_id": string(c.ID),
	}); ok {
		t.Fatal("member_add on deleted group must fail")
	}
	if ok, _, _ := hubCmd(t, m, buf, 25, "hub_group_member_remove", map[string]any{
		"group_id": string(dead.ID), "agent_id": string(b.ID),
	}); ok {
		t.Fatal("member_remove on deleted group must fail")
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 26, "hub_group_delete", map[string]any{"group_id": string(dead.ID)}); !ok {
		t.Fatalf("second delete must be idempotent: %s", errMsg)
	}
}

// P3a-01 复审后续：purge 物理删 owner 后群不能变孤儿；create 输入互斥/空 id。
func TestGroupPurgedOwnerDoesNotOrphanGroup(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "PurgeA")
	b := createHubAgent(t, m, buf, 2, "PurgeB")
	ok, errMsg, data := hubCmd(t, m, buf, 3, "hub_group_create", map[string]any{
		"name": "purge群",
		"members": []map[string]any{
			{"agent_id": string(a.ID), "role": "owner"},
			{"agent_id": string(b.ID), "role": "member"},
		},
	})
	if !ok {
		t.Fatalf("create: %s", errMsg)
	}
	g := groupFromData(t, data)
	if ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_agent_delete", map[string]any{"agent_id": string(a.ID), "purge": true}); !ok {
		t.Fatalf("purge owner: %s", errMsg)
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 5, "hub_group_update", map[string]any{
		"group_id": string(g.ID), "patch": map[string]any{"name": "purge改名"},
	}); !ok {
		t.Fatalf("rename after purge owner: %s", errMsg)
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 6, "hub_group_update", map[string]any{
		"group_id": string(g.ID), "patch": map[string]any{"owner_agent_id": string(b.ID)},
	}); !ok {
		t.Fatalf("transfer after purge owner: %s", errMsg)
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 7, "hub_group_member_remove", map[string]any{
		"group_id": string(g.ID), "agent_id": string(a.ID),
	}); !ok {
		t.Fatalf("remove purged owner: %s", errMsg)
	}
	// 互斥输入 / 空 member id 必须显式拒绝。
	if ok, _, _ := hubCmd(t, m, buf, 8, "hub_group_create", map[string]any{
		"name": "双输入", "member_ids": []string{string(a.ID)},
		"members": []map[string]any{{"agent_id": string(b.ID), "role": "owner"}},
	}); ok {
		t.Fatal("members + member_ids must be rejected")
	}
	if ok, _, _ := hubCmd(t, m, buf, 9, "hub_group_create", map[string]any{
		"name": "空id", "member_ids": []string{"  "},
	}); ok {
		t.Fatal("blank member_id must be rejected")
	}
}
