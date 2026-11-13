package main

// P6-12：observer 删除后的桥接层行为。
//
//   - 新写入显式 observer 必须被拒绝（create / member_add），不能静默变成 member；
//   - 旧 JSON 的 observer 在加载时迁成 member，迁移后能正常发言/交接/被 @all 唤醒。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

// TestHubGroupCreateRejectsObserverRole：显式写 observer 是「新写入」，必须报错。
func TestHubGroupCreateRejectsObserverRole(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "LegacyA")
	b := createHubAgent(t, m, buf, 2, "LegacyB")
	ok, errMsg, _ := hubCmd(t, m, buf, 3, "hub_group_create", map[string]any{
		"name": "旧角色群",
		"members": []map[string]any{
			{"agent_id": string(a.ID), "role": "owner"},
			{"agent_id": string(b.ID), "role": "observer"},
		},
	})
	if ok {
		t.Fatal("显式 observer 必须被拒绝")
	}
	if !strings.Contains(errMsg, "owner/member") || !strings.Contains(errMsg, "readonly") {
		t.Fatalf("错误要说明角色只剩 owner/member 并指出只读用群模式: %q", errMsg)
	}
	// member_add 同口径（否则可以绕过 create 的校验加进一个 observer）。
	g := groupModeTestGroup(t, m, buf, 4, "ask", struct {
		id   agenthub.AgentID
		role string
	}{a.ID, "owner"})
	if ok, _, _ := hubCmd(t, m, buf, 5, "hub_group_member_add", map[string]any{
		"group_id": string(g.ID), "agent_id": string(b.ID), "role": "observer",
	}); ok {
		t.Fatal("member_add 显式 observer 必须被拒绝")
	}
	// 正常加人仍然可用（没有被误伤）。
	if ok, msg, _ := hubCmd(t, m, buf, 6, "hub_group_member_add", map[string]any{
		"group_id": string(g.ID), "agent_id": string(b.ID),
	}); !ok {
		t.Fatalf("普通 member_add 失败: %s", msg)
	}
}

// TestMigratedObserverMemberCanSpeakAndHandoff：旧 group.json 里的 observer 迁移成
// member 之后，原来被特判挡住的动作（发言 / 交接 / 被 @all 唤醒）全部正常。
func TestMigratedObserverMemberCanSpeakAndHandoff(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "MigrateOwner")
	b := createHubAgent(t, m, buf, 2, "MigrateObserver")
	g := groupModeTestGroup(t, m, buf, 3, "auto", struct {
		id   agenthub.AgentID
		role string
	}{a.ID, "owner"}, struct {
		id   agenthub.AgentID
		role string
	}{b.ID, "member"})

	// 把落盘数据改写成 P6-12 之前的形状（observer），模拟升级前的存量群。
	st, err := m.hubStore() // *agenthub.FileStore：需要 Root() 定位 group.json
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(st.Root(), "groups", string(g.ID), "group.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	legacy := strings.Replace(string(raw), `"role": "member"`, `"role": "observer"`, 1)
	if legacy == string(raw) {
		t.Fatalf("fixture 没找到 member 角色: %s", raw)
	}
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatal(err)
	}

	// 加载 = 迁移：observer → member，成员没少。
	loaded, err := st.LoadGroup(context.Background(), g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Members) != 2 {
		t.Fatalf("迁移丢了成员: %+v", loaded.Members)
	}
	for _, mem := range loaded.Members {
		if mem.Role != agenthub.RoleOwner && mem.Role != agenthub.RoleMember {
			t.Fatalf("加载后仍有非法角色: %+v", loaded.Members)
		}
	}

	// 发言：不再有 observer 特判（迁移后 group_say 必须成功）。
	if _, err := p3bGroupSay(t, m, string(b.ID), map[string]any{
		"group_id": string(g.ID), "text": "迁移后我可以说话了",
	}); err != nil {
		t.Fatalf("迁移后的成员不能发言: %v", err)
	}
	// 交接：双方都是成员即可（不再有 observer 拒绝）。
	if err := agenthub.CheckHandoff(loaded, b.ID, a.ID, 0, nil, time.Now().UTC()); err != nil {
		t.Fatalf("迁移后的成员不能发起交接: %v", err)
	}
	if err := agenthub.CheckHandoff(loaded, a.ID, b.ID, 0, nil, time.Now().UTC()); err != nil {
		t.Fatalf("迁移后的成员不能接收交接: %v", err)
	}

	// @all 覆盖全部活跃成员（除发送者）：迁移成员必须被唤醒。
	profiles, err := m.hubGroupMemberProfiles(context.Background(), st, loaded)
	if err != nil {
		t.Fatal(err)
	}
	router, err := agenthub.NewGroupRouter(loaded, profiles)
	if err != nil {
		t.Fatal(err)
	}
	round, err := router.Route("@all 开会", a.ID, false, 1, time.Now().UTC())
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, id := range round.Targets {
		if id == b.ID {
			found = true
		}
		if id == a.ID {
			t.Fatalf("@all 不该包含发送者: %+v", round.Targets)
		}
	}
	if !found {
		t.Fatalf("@all 没覆盖迁移成员: %+v", round.Targets)
	}
}
