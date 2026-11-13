package main

// P3b-01b：名册重建与状态一致性（N1 装配/重建并发、N2 重建失败不静默、
// N3 your_role 有效角色、N4 无群作用域时的 where、N5 边界行情景）。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

// memberRef 是 groupWithMembers 的入参简写（与既有测试同构，避免重复匿名结构）。
type memberRef = struct {
	id   agenthub.AgentID
	role string
}

// N1：名册重建必须与群会话装配串行（lockHubAgent）。
//
// 交错：装配已取**旧**名册快照、持 agent 锁停在 createIn 之前 → 名册变化 → 重建。
// 不持锁的话，重建会在这时读到空会话表而跳过，随后装配用旧快照发布，旧名册静默胜出；
// 持锁的话，重建等到发布完成再二次查表，把新名册换上去。
func TestHubGroupRosterRefreshWaitsForInFlightAssembly(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "RaceA")
	b := createHubAgent(t, m, buf, 2, "RaceB")
	g := groupWithMembers(t, m, buf, 3, "并发名册群", memberRef{a.ID, "owner"}, memberRef{b.ID, "member"})

	entered := make(chan struct{}, 1)
	release := make(chan struct{})
	hubGroupAssembleHook = func(agentID string) {
		if agentID != string(a.ID) {
			return
		}
		hubGroupAssembleHook = nil // 只挡这一次装配
		entered <- struct{}{}
		<-release
	}
	t.Cleanup(func() { hubGroupAssembleHook = nil })

	assembled := make(chan error, 1)
	go func() {
		_, err := m.hubGroupSession(string(a.ID), string(g.ID))
		assembled <- err
	}()
	<-entered // 装配已持锁、已取旧名册快照

	// 名册变化：B 改名。直达存储（与 hub_agent_update 的「先落盘、再刷新」同序）：
	// 走命令的话它自己会先触发一次刷新，轮不到这里复现交错。
	st := mustHubStore(t, m)
	pb, _, err := m.hubLoadAgent(string(b.ID))
	if err != nil {
		t.Fatal(err)
	}
	pb.Name = "RaceB2"
	pb.UpdatedAt = time.Now().UTC()
	if err := st.SaveAgent(context.Background(), pb); err != nil {
		t.Fatal(err)
	}

	refreshed := make(chan error, 1)
	go func() { refreshed <- m.refreshHubGroupRoster(g.ID) }()

	// 持锁：刷新拿不到 agent 锁，只能等装配发布后二次查表（此处超时是**预期**）。
	// 不持锁（mutation）：刷新立刻读完空表返回，下面的断言会失败。
	refreshedEarly := false
	select {
	case err := <-refreshed:
		refreshedEarly = true
		if err != nil {
			t.Fatalf("refresh: %v", err)
		}
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-assembled; err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if !refreshedEarly {
		select {
		case err := <-refreshed:
			if err != nil {
				t.Fatalf("refresh: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Fatal("roster refresh did not finish after the assembly was released")
		}
	}

	ws := m.hubRuntimeIfExists(string(a.ID))
	if ws == nil {
		t.Fatal("assembly did not register the agent runtime")
	}
	sid := hubSessionID(string(a.ID), agenthub.Scope{Kind: agenthub.ScopeGroup, GroupID: g.ID})
	ws.mu.Lock()
	bs := ws.sessions[sid]
	ws.mu.Unlock()
	if bs == nil || bs.loop == nil {
		t.Fatalf("group session not assembled: %+v", bs)
	}
	prompt := bs.loop.SystemPromptText()
	if !strings.Contains(prompt, "- RaceB2 (member, "+string(b.ID)+")") {
		t.Fatalf("in-flight assembly won with the stale roster (N1 TOCTOU):\n%s", prompt)
	}
	if strings.Contains(prompt, "- RaceB (member") {
		t.Fatalf("old roster survived the concurrent change:\n%s", prompt)
	}
}

// N2：重建失败必须带回错误（命令层据此告警），不能静默保留旧 loop 还报「已生效」。
func TestHubRosterLoopRebuildReportsFailure(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "WarnA")
	b := createHubAgent(t, m, buf, 2, "WarnB")
	g := groupWithMembers(t, m, buf, 3, "告警群", memberRef{a.ID, "owner"}, memberRef{b.ID, "member"})
	bs, err := m.hubGroupSession(string(a.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	before := bs.loop

	// 群被删除：群会话留在内存里只为收尾，但名册已经读不到了。
	if ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_group_delete", map[string]any{"group_id": string(g.ID)}); !ok {
		t.Fatalf("delete group: %s", errMsg)
	}
	if err := m.refreshHubLoops(string(a.ID)); err == nil {
		t.Fatal("refreshHubLoops swallowed the unreadable group (want non-nil error)")
	}
	if bs.loop != before {
		t.Fatal("a failed rebuild must keep the previous loop (never swap in a DM-context one)")
	}

	ok, errMsg, data := hubCmd(t, m, buf, 5, "hub_agent_update", map[string]any{
		"agent_id": string(a.ID), "patch": map[string]any{"instructions": "新的行事规则"},
	})
	if !ok {
		t.Fatalf("update agent: %s", errMsg)
	}
	w, _ := data["warning"].(string)
	if !strings.Contains(w, "重建会话 loop 失败") {
		t.Fatalf("warning = %q, want the loop-rebuild warning", w)
	}
	if data["effective"] != "next_session" {
		t.Fatalf("effective = %v, want next_session after a failed rebuild", data["effective"])
	}
	if bs.loop != before {
		t.Fatal("failed rebuild still swapped the loop")
	}
}

// 告警是追加而不是覆盖：前一条（例如工具策略变更）不能被后一条抹掉。
func TestAddHubWarningPreservesExisting(t *testing.T) {
	data := map[string]any{"effective": "next_session"}
	addHubWarning(data, "第一条")
	addHubWarning(data, "第二条")
	if got, _ := data["warning"].(string); got != "第一条；第二条" {
		t.Fatalf("warning = %q, want both messages", got)
	}
	addHubWarning(data, "") // 空消息不落一条空告警
	if got, _ := data["warning"].(string); got != "第一条；第二条" {
		t.Fatalf("empty warning changed the message: %q", got)
	}
}

// N3：your_role 走**有效**角色（owner 以 Group.OwnerAgentID 为准）。
// 老数据里 owner 转让可能只改了 owner 字段、成员表仍是 member ——
// 工具不能对着 owner 说「你是 member」，且与名册行/提示词里的角色必须一致。
func TestHubAgentStatusYourRoleUsesOwnerOverride(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "LegacyOwner")
	b := createHubAgent(t, m, buf, 2, "LegacyMember")
	g := groupWithMembers(t, m, buf, 3, "旧数据群", memberRef{a.ID, "owner"}, memberRef{b.ID, "member"})

	// 老数据：owner 字段指向 A，但成员表里 A 还是 member（Store 不做归一化）。
	st := mustHubStore(t, m)
	legacy, err := st.LoadGroup(context.Background(), g.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := range legacy.Members {
		if legacy.Members[i].AgentID == a.ID {
			legacy.Members[i].Role = agenthub.RoleMember
		}
	}
	if err := st.SaveGroup(context.Background(), legacy); err != nil {
		t.Fatal(err)
	}

	bsA, err := m.hubGroupSession(string(a.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	args := `{"group_id":"` + string(g.ID) + `"}`
	var got struct {
		GroupID  string       `json:"group_id"`
		YourRole string       `json:"your_role"`
		Agent    hubMemberRow `json:"agent"`
	}
	if err := json.Unmarshal([]byte(mustSessionTool(t, bsA, "agent_status", args)), &got); err != nil {
		t.Fatal(err)
	}
	if got.YourRole != "owner" {
		t.Fatalf("your_role = %q, want owner (Group.OwnerAgentID wins over Member.Role)", got.YourRole)
	}
	if got.Agent.Role != "owner" {
		t.Fatalf("agent row role = %q, want owner (must agree with your_role)", got.Agent.Role)
	}
	rows := memberRows(t, mustSessionTool(t, bsA, "group_members", args))
	if row := memberRowByID(t, rows, a.ID); row.Role != "owner" {
		t.Fatalf("group_members role = %q, want owner", row.Role)
	}
	// 提示词里的名册行同口径（有效角色）。
	if err := m.refreshHubGroupRoster(g.ID); err != nil {
		t.Fatal(err)
	}
	if prompt := bsA.loop.SystemPromptText(); !strings.Contains(prompt, "You are LegacyOwner (owner, "+string(a.ID)+")") {
		t.Fatalf("roster self line disagrees with your_role:\n%s", prompt)
	}
}

// N4：没有请求群作用域时，群会话忙就是 "group"；只有**指定了别的群**才是 "other_group"。
func TestHubAgentStatusWhereLabels(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "WhereA")
	b := createHubAgent(t, m, buf, 2, "WhereB")
	g1 := groupWithMembers(t, m, buf, 3, "状态群一", memberRef{a.ID, "owner"}, memberRef{b.ID, "member"})
	g2 := groupWithMembers(t, m, buf, 4, "状态群二", memberRef{b.ID, "owner"}, memberRef{a.ID, "member"})
	bsA1, err := m.hubGroupSession(string(a.ID), string(g1.ID))
	if err != nil {
		t.Fatal(err)
	}
	bsA2, err := m.hubGroupSession(string(a.ID), string(g2.ID))
	if err != nil {
		t.Fatal(err)
	}
	bsB1, err := m.hubGroupSession(string(b.ID), string(g1.ID))
	if err != nil {
		t.Fatal(err)
	}
	statusRow := func(args string) hubMemberRow {
		t.Helper()
		var out struct {
			Agent hubMemberRow `json:"agent"`
		}
		if err := json.Unmarshal([]byte(mustSessionTool(t, bsB1, "agent_status", args)), &out); err != nil {
			t.Fatalf("agent_status %s: %v", args, err)
		}
		return out.Agent
	}
	noScope := `{"agent_id":"` + string(a.ID) + `"}`
	scoped1 := `{"group_id":"` + string(g1.ID) + `","agent_id":"` + string(a.ID) + `"}`
	defer func() {
		bsA1.running.Store(false)
		bsA2.running.Store(false)
	}()

	// A 在本群忙：无作用域查询不能再报 other_group。
	bsA1.running.Store(true)
	if row := statusRow(noScope); row.Status != hubToolStatusRunning || row.Where != "group" {
		t.Fatalf("no-scope row = %+v, want running/group", row)
	}
	if row := statusRow(scoped1); row.Status != hubToolStatusRunning || row.Where != "group" {
		t.Fatalf("same-group row = %+v, want running/group", row)
	}

	// 只忙在**另一个**群：指定 g1 时必须说 other_group（不给那个群 id）。
	bsA1.running.Store(false)
	bsA2.running.Store(true)
	if row := statusRow(scoped1); row.Status != hubToolStatusRunning || row.Where != "other_group" {
		t.Fatalf("other-group row = %+v, want running/other_group", row)
	}
	if row := statusRow(noScope); row.Status != hubToolStatusRunning || row.Where != "group" {
		t.Fatalf("no-scope other-group row = %+v, want running/group", row)
	}

	// 私聊忙：仍然是 dm（群会话空闲）。
	bsA2.running.Store(false)
	bsADM, err := m.hubDMSession(string(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	bsADM.running.Store(true)
	defer bsADM.running.Store(false)
	if row := statusRow(scoped1); row.Status != hubToolStatusRunning || row.Where != "dm" {
		t.Fatalf("dm row = %+v, want running/dm", row)
	}
}

// N5：软删的行标 Deleted（名字还在），物理 purge 后标 Missing（名册里是显式 fallback）。
func TestHubGroupMembersMissingAndDeletedRows(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "RowsA")
	b := createHubAgent(t, m, buf, 2, "RowsB")
	g := groupWithMembers(t, m, buf, 3, "行状态群", memberRef{a.ID, "owner"}, memberRef{b.ID, "member"})
	bsA, err := m.hubGroupSession(string(a.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	args := `{"group_id":"` + string(g.ID) + `"}`

	if ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_agent_delete", map[string]any{"agent_id": string(b.ID)}); !ok {
		t.Fatalf("soft delete: %s", errMsg)
	}
	row := memberRowByID(t, memberRows(t, mustSessionTool(t, bsA, "group_members", args)), b.ID)
	if !row.Deleted || row.Missing {
		t.Fatalf("soft-deleted row = %+v, want deleted (profile still readable)", row)
	}
	if row.Name != "RowsB" {
		t.Fatalf("soft-deleted row name = %q, want the retained profile name", row.Name)
	}

	if ok, errMsg, _ := hubCmd(t, m, buf, 5, "hub_agent_delete", map[string]any{
		"agent_id": string(b.ID), "purge": true,
	}); !ok {
		t.Fatalf("purge: %s", errMsg)
	}
	row = memberRowByID(t, memberRows(t, mustSessionTool(t, bsA, "group_members", args)), b.ID)
	if !row.Missing || row.Deleted || row.Name != "" {
		t.Fatalf("purged row = %+v, want missing with no name", row)
	}
	// agent_status 对缺失档案的目标同样显式标 missing（不编造名字）。
	var got struct {
		Agent hubMemberRow `json:"agent"`
	}
	if err := json.Unmarshal([]byte(mustSessionTool(t, bsA, "agent_status",
		`{"group_id":"`+string(g.ID)+`","agent_id":"`+string(b.ID)+`"}`)), &got); err != nil {
		t.Fatal(err)
	}
	if !got.Agent.Missing {
		t.Fatalf("agent_status row = %+v, want missing", got.Agent)
	}
}

// N5：删除群不重建成员 loop（旧会话只用于收尾），之后的「名册重建」是空操作。
func TestHubGroupDeleteDoesNotRebuildLoops(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "DelA")
	b := createHubAgent(t, m, buf, 2, "DelB")
	g := groupWithMembers(t, m, buf, 3, "待删群", memberRef{a.ID, "owner"}, memberRef{b.ID, "member"})
	bsA, err := m.hubGroupSession(string(a.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	bsB, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	loopA, loopB := bsA.loop, bsB.loop

	ok, errMsg, data := hubCmd(t, m, buf, 4, "hub_group_delete", map[string]any{"group_id": string(g.ID)})
	if !ok {
		t.Fatalf("delete group: %s", errMsg)
	}
	if w, _ := data["warning"].(string); w != "" {
		t.Fatalf("group delete is not a failed rebuild, got warning %q", w)
	}
	if bsA.loop != loopA || bsB.loop != loopB {
		t.Fatal("group delete rebuilt member loops (should only let in-flight rounds finish)")
	}
	if err := m.refreshHubGroupRoster(g.ID); err != nil {
		t.Fatalf("refresh after delete: %v (deleted groups are a no-op, not an error)", err)
	}
	if bsA.loop != loopA || bsB.loop != loopB {
		t.Fatal("refresh after delete touched a deleted group's loops")
	}
}
