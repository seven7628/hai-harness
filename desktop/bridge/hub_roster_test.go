package main

// P3b-01：名册稳定层 + 实时状态走工具 + 名册变化重建群 loop。
//
// 覆盖：
//   - group_members() 行里的状态来自真实会话忙闲（idle/running + 忙在哪个作用域）；
//   - agent_status() 的成员/共享群权限（不确认陌生 id 是否存在）；
//   - 加/删成员、群改名、owner 转让、智能体改名/软删之后，**其他成员**的群会话
//     system prompt 立刻换新名册；正在跑的回合不被腰斩。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
)

func callSessionTool(t *testing.T, bs *bridgeSession, name, args string) (string, error) {
	t.Helper()
	if bs == nil || bs.hubEngine == nil {
		t.Fatalf("session %v has no hub engine", bs)
	}
	tool, err := bs.hubEngine.GetTool(context.Background(), name)
	if err != nil {
		t.Fatalf("tool %s not registered: %v", name, err)
	}
	return tool.Call(context.Background(), name, args)
}

func mustSessionTool(t *testing.T, bs *bridgeSession, name, args string) string {
	t.Helper()
	out, err := callSessionTool(t, bs, name, args)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return out
}

type hubMemberRows struct {
	GroupID string         `json:"group_id"`
	Name    string         `json:"name"`
	Mode    string         `json:"mode"`
	Members []hubMemberRow `json:"members"`
}

func memberRows(t *testing.T, out string) []hubMemberRow {
	t.Helper()
	var got hubMemberRows
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("group_members output is not JSON: %v\n%s", err, out)
	}
	return got.Members
}

func memberRowByID(t *testing.T, rows []hubMemberRow, id agenthub.AgentID) hubMemberRow {
	t.Helper()
	for _, row := range rows {
		if row.AgentID == string(id) {
			return row
		}
	}
	t.Fatalf("member %s missing from %+v", id, rows)
	return hubMemberRow{}
}

// group_members 的状态必须是真实现场：会话忙（run/派发/群轮）→ running，
// 并说清忙在哪个作用域；都不忙 → idle。
func TestHubGroupMembersLiveStatus(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "StatusA")
	b := createHubAgent(t, m, buf, 2, "StatusB")
	g := groupWithMembers(t, m, buf, 3, "状态群",
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"},
		struct {
			id   agenthub.AgentID
			role string
		}{b.ID, "member"},
	)
	bsA, err := m.hubGroupSession(string(a.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	bsB, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	membersArg := `{"group_id":"` + string(g.ID) + `"}`

	rows := memberRows(t, mustSessionTool(t, bsA, "group_members", membersArg))
	if len(rows) != 2 {
		t.Fatalf("members = %+v", rows)
	}
	// 顺序 = 群成员表顺序（不是 map 迭代），且初始状态是 idle。
	if rows[0].AgentID != string(a.ID) || rows[1].AgentID != string(b.ID) {
		t.Fatalf("member order = %+v", rows)
	}
	for _, row := range rows {
		if row.Status != hubToolStatusIdle || row.Where != "" {
			t.Fatalf("idle member row = %+v", row)
		}
	}
	if rows[0].Role != "owner" || rows[1].Role != "member" {
		t.Fatalf("roles = %q/%q, want owner/member", rows[0].Role, rows[1].Role)
	}
	// 职责（profile description）也随行返回：模型问「谁能干什么」时不用再翻名册。
	if ok, errMsg, _ := hubCmd(t, m, buf, 4, "hub_agent_update", map[string]any{
		"agent_id": string(b.ID), "patch": map[string]any{"description": "负责状态校验"},
	}); !ok {
		t.Fatalf("set description: %s", errMsg)
	}
	row := memberRowByID(t, memberRows(t, mustSessionTool(t, bsA, "group_members", membersArg)), b.ID)
	if row.Description != "负责状态校验" {
		t.Fatalf("member description = %q, want the profile description", row.Description)
	}

	// B 正在跑本群回合。
	bsB.running.Store(true)
	row = memberRowByID(t, memberRows(t, mustSessionTool(t, bsA, "group_members", membersArg)), b.ID)
	if row.Status != hubToolStatusRunning || row.Where != "group" {
		t.Fatalf("running row = %+v, want running/group", row)
	}
	bsB.running.Store(false)

	// 群轮已投递、AgentEnd 还没到（busy 闩）也必须算 running。
	bsB.hubGroupBusy.Store(true)
	row = memberRowByID(t, memberRows(t, mustSessionTool(t, bsA, "group_members", membersArg)), b.ID)
	if row.Status != hubToolStatusRunning || row.Where != "group" {
		t.Fatalf("busy-latch row = %+v, want running/group", row)
	}
	bsB.hubGroupBusy.Store(false)

	// 已投递但根 run 未开始（hubDispatchPending）同样是 running。
	bsB.hubDispatchPending.Store(true)
	row = memberRowByID(t, memberRows(t, mustSessionTool(t, bsA, "group_members", membersArg)), b.ID)
	if row.Status != hubToolStatusRunning {
		t.Fatalf("dispatch-pending row = %+v, want running", row)
	}
	bsB.hubDispatchPending.Store(false)

	// A 在私聊里忙：本群视角必须显示 running + dm（不用别的群 id 泄漏作用域）。
	bsADM, err := m.hubDMSession(string(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	bsADM.running.Store(true)
	defer bsADM.running.Store(false)
	row = memberRowByID(t, memberRows(t, mustSessionTool(t, bsA, "group_members", membersArg)), a.ID)
	if row.Status != hubToolStatusRunning || row.Where != "dm" {
		t.Fatalf("dm-busy row = %+v, want running/dm", row)
	}

	// 回到空闲：状态必须自己落回去，不能粘住。
	bsADM.running.Store(false)
	for _, row := range memberRows(t, mustSessionTool(t, bsA, "group_members", membersArg)) {
		if row.Status != hubToolStatusIdle || row.Where != "" {
			t.Fatalf("row did not return to idle: %+v", row)
		}
	}
}

// agent_status：默认查自己；查别人必须有共享群；带 group_id 时目标必须是该群成员；
// 非成员不能借工具探测群的成员表。
func TestHubAgentStatusToolMembership(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "StatA")
	b := createHubAgent(t, m, buf, 2, "StatB")
	d := createHubAgent(t, m, buf, 3, "Outsider")
	g := groupWithMembers(t, m, buf, 4, "状态权限群",
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"},
		struct {
			id   agenthub.AgentID
			role string
		}{b.ID, "member"},
	)
	bsB, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	bsD, err := m.hubDMSession(string(d.ID))
	if err != nil {
		t.Fatal(err)
	}

	// 默认查自己。
	out := mustSessionTool(t, bsB, "agent_status", `{}`)
	var self struct {
		Agent hubMemberRow `json:"agent"`
	}
	if err := json.Unmarshal([]byte(out), &self); err != nil {
		t.Fatalf("agent_status output: %v\n%s", err, out)
	}
	if self.Agent.AgentID != string(b.ID) || self.Agent.Status != hubToolStatusIdle {
		t.Fatalf("self status = %+v", self.Agent)
	}
	if self.Agent.Role != "" {
		t.Fatalf("group-less self status must not invent a role: %+v", self.Agent)
	}

	// 同群成员 + group_id：拿得到角色。
	out = mustSessionTool(t, bsB, "agent_status", `{"group_id":"`+string(g.ID)+`","agent_id":"`+string(a.ID)+`"}`)
	var inGroup struct {
		GroupID  string       `json:"group_id"`
		YourRole string       `json:"your_role"`
		Agent    hubMemberRow `json:"agent"`
	}
	if err := json.Unmarshal([]byte(out), &inGroup); err != nil {
		t.Fatalf("agent_status output: %v\n%s", err, out)
	}
	if inGroup.GroupID != string(g.ID) || inGroup.YourRole != "member" || inGroup.Agent.Role != "owner" {
		t.Fatalf("group status = %+v", inGroup)
	}

	// 不带 group_id 查同群成员：允许（有共享群）。
	if _, err := callSessionTool(t, bsB, "agent_status", `{"agent_id":"`+string(a.ID)+`"}`); err != nil {
		t.Fatalf("shared-group status: %v", err)
	}

	// 非成员不能按 group_id 读群成员。
	if _, err := callSessionTool(t, bsD, "agent_status", `{"group_id":"`+string(g.ID)+`","agent_id":"`+string(a.ID)+`"}`); err == nil {
		t.Fatal("non-member must not read group member status")
	}
	// 无共享群：拒绝，且不能因为 id 不存在而给出不同的错误（不确认存在性）。
	if _, err := callSessionTool(t, bsD, "agent_status", `{"agent_id":"`+string(a.ID)+`"}`); err == nil {
		t.Fatal("agent outside every shared group must be rejected")
	}
	if _, err := callSessionTool(t, bsD, "agent_status", `{"agent_id":"ag_does_not_exist"}`); err == nil {
		t.Fatal("unknown agent id must be rejected the same way")
	}
	// 目标不是该群成员：明确拒绝，不返回半截信息。
	if _, err := callSessionTool(t, bsB, "agent_status", `{"group_id":"`+string(g.ID)+`","agent_id":"`+string(d.ID)+`"}`); err == nil {
		t.Fatal("agent_status must reject a target outside the group")
	}
	// 自己永远可以查自己（哪怕还没进任何群）。
	if _, err := callSessionTool(t, bsD, "agent_status", `{"agent_id":"`+string(d.ID)+`"}`); err != nil {
		t.Fatalf("self status outside groups: %v", err)
	}
}

// 名册变化必须重建**其他成员**的群 loop：加人/删人/群改名/owner 转让/智能体改名/
// 软删都让下一个 Run 看到新名册；正在跑的回合不被腰斩。
func TestHubGroupRosterRebuildsOnRosterChange(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "Alpha")
	b := createHubAgent(t, m, buf, 2, "Bravo")
	c := createHubAgent(t, m, buf, 3, "Charlie")
	g := groupWithMembers(t, m, buf, 4, "名册重建群",
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"},
		struct {
			id   agenthub.AgentID
			role string
		}{b.ID, "member"},
	)
	bsA, err := m.hubGroupSession(string(a.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	bsB, err := m.hubGroupSession(string(b.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	bsC, err := m.hubGroupSession(string(c.ID), string(g.ID))
	if err == nil || bsC != nil {
		t.Fatal("non-member must not get a group session")
	}

	initial := bsA.loop
	initialPrompt := initial.SystemPromptText()
	if !strings.Contains(initialPrompt, "- Bravo (member, "+string(b.ID)+")") {
		t.Fatalf("initial roster missing B:\n%s", initialPrompt)
	}
	if strings.Contains(initialPrompt, "Charlie") {
		t.Fatalf("initial roster already has the outsider:\n%s", initialPrompt)
	}

	// 1) 加成员：A/B 的 loop 都要换，且名册出现 C（observer）。
	if ok, errMsg, _ := hubCmd(t, m, buf, 5, "hub_group_member_add", map[string]any{
		"group_id": string(g.ID), "agent_id": string(c.ID), "role": "observer",
	}); !ok {
		t.Fatalf("add member: %s", errMsg)
	}
	if bsA.loop == initial {
		t.Fatal("add member did not rebuild A's group loop")
	}
	added := bsA.loop.SystemPromptText()
	if !strings.Contains(added, "- Charlie (observer, "+string(c.ID)+")") {
		t.Fatalf("rebuilt roster missing C:\n%s", added)
	}
	if !strings.Contains(bsB.loop.SystemPromptText(), "Charlie (observer") {
		t.Fatalf("other member's roster not rebuilt:\n%s", bsB.loop.SystemPromptText())
	}

	// C 自己也有群会话（加入后装配）：删除后它必须能收尾，但不能再被重建/唤醒。
	bsC, err = m.hubGroupSession(string(c.ID), string(g.ID))
	if err != nil {
		t.Fatal(err)
	}
	cLoopBeforeRemove := bsC.loop

	// 2) 删成员：剩下的成员名册立刻少一行，被删者自己的旧 loop 不动（在跑的回合能收尾）。
	if ok, errMsg, _ := hubCmd(t, m, buf, 6, "hub_group_member_remove", map[string]any{
		"group_id": string(g.ID), "agent_id": string(c.ID),
	}); !ok {
		t.Fatalf("remove member: %s", errMsg)
	}
	removed := bsA.loop.SystemPromptText()
	if strings.Contains(removed, "Charlie") {
		t.Fatalf("removed member still in A's roster:\n%s", removed)
	}
	if !strings.Contains(bsB.loop.SystemPromptText(), "Bravo") {
		t.Fatalf("B's roster lost itself after removal:\n%s", bsB.loop.SystemPromptText())
	}
	if bsC.loop != cLoopBeforeRemove {
		t.Fatal("removed member's own loop must not be rebuilt (it should just finish)")
	}
	if _, err := m.hubGroupSession(string(c.ID), string(g.ID)); err == nil {
		t.Fatal("removed member must not get a group session again")
	}

	// 3) 群改名：名册第一行换掉。
	if ok, errMsg, _ := hubCmd(t, m, buf, 7, "hub_group_update", map[string]any{
		"group_id": string(g.ID), "patch": map[string]any{"name": "名册改名群"},
	}); !ok {
		t.Fatalf("rename group: %s", errMsg)
	}
	if got := bsA.loop.SystemPromptText(); !strings.Contains(got, "## Group: 名册改名群") {
		t.Fatalf("group rename not in roster:\n%s", got)
	}

	// 4) owner 转让：角色成对改写（A→member, B→owner），两个会话都要换。
	if ok, errMsg, _ := hubCmd(t, m, buf, 8, "hub_group_update", map[string]any{
		"group_id": string(g.ID), "patch": map[string]any{"owner_agent_id": string(b.ID)},
	}); !ok {
		t.Fatalf("transfer owner: %s", errMsg)
	}
	transferred := bsA.loop.SystemPromptText()
	if !strings.Contains(transferred, "You are Alpha (member, "+string(a.ID)+")") {
		t.Fatalf("A did not become member:\n%s", transferred)
	}
	if !strings.Contains(transferred, "- Bravo (owner, "+string(b.ID)+")") {
		t.Fatalf("B did not become owner in A's roster:\n%s", transferred)
	}

	// 5) 智能体改名：别人 prompt 里的那一行必须跟着变。
	const renamed = "BravoRenamed"
	if ok, errMsg, _ := hubCmd(t, m, buf, 9, "hub_agent_update", map[string]any{
		"agent_id": string(b.ID), "patch": map[string]any{"name": renamed},
	}); !ok {
		t.Fatalf("agent rename: %s", errMsg)
	}
	afterRename := bsA.loop.SystemPromptText()
	if !strings.Contains(afterRename, "- "+renamed+" (owner, "+string(b.ID)+")") {
		t.Fatalf("agent rename not reflected in A's roster:\n%s", afterRename)
	}
	if strings.Contains(afterRename, "- Bravo (owner") {
		t.Fatalf("old name still in A's roster:\n%s", afterRename)
	}

	// 6) 在跑的回合不能被腰斩：忙时加成员只换 loop，running/ctx 不动。
	bsA.running.Store(true)
	beforeAdd := bsA.loop
	if ok, errMsg, _ := hubCmd(t, m, buf, 10, "hub_group_member_add", map[string]any{
		"group_id": string(g.ID), "agent_id": string(c.ID), "role": "member",
	}); !ok {
		t.Fatalf("re-add member: %s", errMsg)
	}
	if bsA.loop == beforeAdd {
		t.Fatal("busy session's loop was not rebuilt (next run would see a stale roster)")
	}
	if !bsA.running.Load() {
		t.Fatal("rebuilding the roster must not clear the in-flight run flag")
	}
	if err := bsA.ctx.Err(); err != nil {
		t.Fatalf("rebuilding the roster cancelled the session: %v", err)
	}
	bsA.running.Store(false)

	// 7) 软删成员（现在是 owner）：名册里留显式 fallback，而不是静默消失或继续用旧名。
	if ok, errMsg, _ := hubCmd(t, m, buf, 11, "hub_agent_delete", map[string]any{
		"agent_id": string(b.ID),
	}); !ok {
		t.Fatalf("soft delete owner: %s", errMsg)
	}
	deleted := bsA.loop.SystemPromptText()
	if !strings.Contains(deleted, "- unknown member (owner, "+string(b.ID)+") — profile unavailable") {
		t.Fatalf("deleted agent must stay an explicit fallback:\n%s", deleted)
	}
	if strings.Contains(deleted, renamed) {
		t.Fatalf("deleted agent's name leaked into the roster:\n%s", deleted)
	}

	// 名册渲染逐字节稳定：没有任何变更时重复重建不能抖动（prompt cache 依赖它）。
	if err := m.refreshHubGroupRoster(g.ID); err != nil {
		t.Fatal(err)
	}
	once := bsA.loop.SystemPromptText()
	if err := m.refreshHubGroupRoster(g.ID); err != nil {
		t.Fatal(err)
	}
	if twice := bsA.loop.SystemPromptText(); twice != once {
		t.Fatalf("roster rebuild is not byte-stable:\n--- once ---\n%s\n--- twice ---\n%s", once, twice)
	}
}

// 起会话后再改成员时不能顺手把「没装配过的成员」的 runtime 建出来（工具调用要零副作用）。
func TestHubGroupRosterRebuildDoesNotAssembleSessions(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "LazyA")
	b := createHubAgent(t, m, buf, 2, "LazyB")
	g := groupWithMembers(t, m, buf, 3, "惰性群",
		struct {
			id   agenthub.AgentID
			role string
		}{a.ID, "owner"},
		struct {
			id   agenthub.AgentID
			role string
		}{b.ID, "member"},
	)
	if ws := m.hubRuntimeIfExists(string(b.ID)); ws != nil {
		t.Fatal("fixture: B runtime should not exist yet")
	}
	bs, err := m.hubDMSession(string(a.ID))
	if err != nil {
		t.Fatal(err)
	}
	bs.running.Store(true) // 让状态查询有东西可读
	defer bs.running.Store(false)
	if out := mustSessionTool(t, bs, "group_members", `{"group_id":"`+string(g.ID)+`"}`); !strings.Contains(out, "LazyB") {
		t.Fatalf("members = %s", out)
	}
	if ws := m.hubRuntimeIfExists(string(b.ID)); ws != nil {
		t.Fatal("group_members/agent_status must not assemble another agent's runtime")
	}
	if _, err := callSessionTool(t, bs, "agent_status", `{"group_id":"`+string(g.ID)+`","agent_id":"`+string(b.ID)+`"}`); err != nil {
		t.Fatal(err)
	}
	if ws := m.hubRuntimeIfExists(string(b.ID)); ws != nil {
		t.Fatal("agent_status assembled another agent's runtime")
	}
}
