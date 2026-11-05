package main

// P3a-01：群组 CRUD + 成员/角色/策略。

import (
	"encoding/json"
	"fmt"
	"testing"

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
