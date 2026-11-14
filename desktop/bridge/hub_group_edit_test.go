package main

// P7-01（I3）：群组编辑原子命令 hub_group_edit。
//
// 契约：一次 LoadGroup → 全量校验 → 一次 SaveGroup → 一次名册重建；任一校验失败时
// 磁盘上的群逐字段不变（连文件字节都不变）。旧命令（update/member_add/member_remove）
// 必须继续可用。

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
)

// memberSpec 是 groupWithMembers 的匿名结构体元素的别名（调用点写起来短一点）。
type memberSpec = struct {
	id   agenthub.AgentID
	role string
}

// countRosterRefresh 统计名册重建入口被调用了几次（生产恒 nil，测试里临时挂上）。
func countRosterRefresh(t *testing.T) *int32 {
	t.Helper()
	var n int32
	hubRosterRefreshHook = func(gid agenthub.GroupID) { atomic.AddInt32(&n, 1) }
	t.Cleanup(func() { hubRosterRefreshHook = nil })
	return &n
}

func loadGroupForTest(t *testing.T, m *manager, gid agenthub.GroupID) *agenthub.Group {
	t.Helper()
	st, err := m.hubStore()
	if err != nil {
		t.Fatalf("hubStore: %v", err)
	}
	g, err := st.LoadGroup(context.Background(), gid)
	if err != nil || g == nil {
		t.Fatalf("load group %s: %v", gid, err)
	}
	return g
}

// groupFileBytes 读群 JSON 的原始字节：原子性反证要证明「连一次写都没发生」。
func groupFileBytes(t *testing.T, gid agenthub.GroupID) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(appDataDir(), "hub", "groups", string(gid), "group.json"))
	if err != nil {
		t.Fatalf("read group file: %v", err)
	}
	return raw
}

func groupMemberIDs(g *agenthub.Group) []string {
	out := make([]string, 0, len(g.Members))
	for _, m := range g.Members {
		out = append(out, string(m.AgentID))
	}
	return out
}

func groupRoleOf(g *agenthub.Group, id agenthub.AgentID) agenthub.MemberRole {
	role, ok := g.RoleOf(id)
	if !ok {
		return ""
	}
	return role
}

// 成功路径：一条命令同时改 name/mode/owner + 增删成员，名册只重建一次。
func TestHubGroupEditAtomicAllInOne(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "EditOwner")
	b := createHubAgent(t, m, buf, 2, "EditKeep")
	c := createHubAgent(t, m, buf, 3, "EditDrop")
	d := createHubAgent(t, m, buf, 4, "EditAdd")
	g := groupWithMembers(t, m, buf, 5, "原子群",
		memberSpec{a.ID, "owner"},
		memberSpec{b.ID, "member"},
		memberSpec{c.ID, "member"},
	)
	// 装配 owner 的群会话：名册重建要真的换掉它的 loop（否则「一次重建」只是空跑）。
	bs, err := m.hubGroupSession(string(a.ID), string(g.ID))
	if err != nil {
		t.Fatalf("group session: %v", err)
	}
	beforeLoop := bs.loop
	refreshes := countRosterRefresh(t)

	ok, errMsg, data := hubCmd(t, m, buf, 6, "hub_group_edit", map[string]any{
		"group_id":       string(g.ID),
		"name":           "原子群（改）",
		"mode":           "readonly",
		"owner":          string(b.ID),
		"add_members":    []string{string(d.ID)},
		"remove_members": []string{string(c.ID)},
	})
	if !ok {
		t.Fatalf("hub_group_edit: %s", errMsg)
	}
	got := groupFromData(t, data)
	if got.Name != "原子群（改）" {
		t.Fatalf("name = %q", got.Name)
	}
	if got.Policy.Mode != agenthub.GroupModeReadonly {
		t.Fatalf("mode = %q, want readonly", got.Policy.Mode)
	}
	if got.OwnerAgentID != b.ID {
		t.Fatalf("owner = %s, want %s", got.OwnerAgentID, b.ID)
	}
	if want := []string{string(a.ID), string(b.ID), string(d.ID)}; !reflect.DeepEqual(groupMemberIDs(got), want) {
		t.Fatalf("members = %v, want %v（移除 c、加入 d 必须同一条命令生效）", groupMemberIDs(got), want)
	}
	if role := groupRoleOf(got, b.ID); role != agenthub.RoleOwner {
		t.Fatalf("new owner role = %q", role)
	}
	if role := groupRoleOf(got, a.ID); role != agenthub.RoleMember {
		t.Fatalf("old owner role = %q, want member", role)
	}
	// 落盘的就是返回的那份：重新加载逐字段相等。
	if reloaded := loadGroupForTest(t, m, g.ID); !reflect.DeepEqual(reloaded, got) {
		t.Fatalf("reloaded group != response:\nreloaded=%+v\nresponse=%+v", reloaded, got)
	}
	if n := atomic.LoadInt32(refreshes); n != 1 {
		t.Fatalf("roster rebuild = %d, want exactly 1（不能逐成员循环重建）", n)
	}
	if bs.loop == beforeLoop {
		t.Fatal("roster loop 没换：下一次唤醒会看到旧群名/旧成员")
	}
	if prompt := bs.loop.SystemPromptText(); !strings.Contains(prompt, "原子群（改）") {
		t.Fatalf("新群名没进名册：\n%s", prompt)
	}
}

// 原子性反证：任何一条校验失败，群逐字段（name/mode/owner/members）与文件字节都不变。
func TestHubGroupEditFailuresLeaveGroupUntouched(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "AtomicOwner")
	b := createHubAgent(t, m, buf, 2, "AtomicKeep")
	c := createHubAgent(t, m, buf, 3, "AtomicDrop")
	d := createHubAgent(t, m, buf, 4, "AtomicOutsider")
	small := groupWithMembers(t, m, buf, 5, "原子性小群",
		memberSpec{a.ID, "owner"},
		memberSpec{b.ID, "member"},
		memberSpec{c.ID, "member"},
	)

	// 12 人群（上限正好占满）+ 一个不在群里的第 13 人。
	specs := make([]memberSpec, 0, agenthub.MaxGroupMembers)
	bigIDs := make([]agenthub.AgentID, 0, agenthub.MaxGroupMembers)
	for i := 0; i < agenthub.MaxGroupMembers; i++ {
		p := createHubAgent(t, m, buf, 100+i, fmt.Sprintf("Big%02d", i))
		bigIDs = append(bigIDs, p.ID)
		role := "member"
		if i == 0 {
			role = "owner"
		}
		specs = append(specs, memberSpec{p.ID, role})
	}
	big := groupWithMembers(t, m, buf, 200, "12 人群", specs...)
	extra := createHubAgent(t, m, buf, 201, "BigExtra")

	// 软删的智能体不能再被加进群（旧成员可以留，新成员必须存活）。
	gone := createHubAgent(t, m, buf, 202, "AtomicGone")
	if ok, errMsg, _ := hubCmd(t, m, buf, 203, "hub_agent_delete", map[string]any{
		"agent_id": string(gone.ID),
	}); !ok {
		t.Fatalf("soft delete: %s", errMsg)
	}

	refreshes := countRosterRefresh(t)
	cmdID := 300
	cases := []struct {
		name    string
		gid     agenthub.GroupID
		payload map[string]any
		wantErr string
	}{
		{
			name:    "add 不存在的 agent",
			gid:     small.ID,
			payload: map[string]any{"add_members": []string{"ag_doesnotexist"}},
			wantErr: "not found",
		},
		{
			// 旧实现（remove 命令先落盘、add 命令后失败）会留下「删了一半」的群。
			name:    "移除合法但添加失败",
			gid:     small.ID,
			payload: map[string]any{"remove_members": []string{string(c.ID)}, "add_members": []string{"ag_doesnotexist"}},
			wantErr: "not found",
		},
		{
			name:    "add 已软删的 agent",
			gid:     small.ID,
			payload: map[string]any{"add_members": []string{string(gone.ID)}},
			wantErr: "智能体已删除",
		},
		{
			name:    "移除 owner",
			gid:     small.ID,
			payload: map[string]any{"remove_members": []string{string(a.ID)}},
			wantErr: "不能移除群 owner",
		},
		{
			name:    "超过 12 人",
			gid:     big.ID,
			payload: map[string]any{"add_members": []string{string(extra.ID)}},
			wantErr: "群成员上限 12 人",
		},
		{
			name:    "重复添加已在群的人",
			gid:     small.ID,
			payload: map[string]any{"add_members": []string{string(b.ID)}},
			wantErr: "该智能体已在群里",
		},
		{
			name:    "移除不在群的人",
			gid:     small.ID,
			payload: map[string]any{"remove_members": []string{string(d.ID)}},
			wantErr: "该智能体不在群里",
		},
		{
			name:    "同一个人同时加入和移除",
			gid:     small.ID,
			payload: map[string]any{"add_members": []string{string(d.ID)}, "remove_members": []string{string(d.ID)}},
			wantErr: "不能同时加入和移除",
		},
		{
			name:    "owner 不是群成员",
			gid:     small.ID,
			payload: map[string]any{"owner": string(d.ID)},
			wantErr: "群主必须是群成员",
		},
		{
			name:    "空群名",
			gid:     small.ID,
			payload: map[string]any{"name": "   "},
			wantErr: "group.name must be 1-32 characters",
		},
		{
			name:    "同名群",
			gid:     small.ID,
			payload: map[string]any{"name": "12 人群"},
			wantErr: "already used by",
		},
		{
			name:    "非法群模式",
			gid:     small.ID,
			payload: map[string]any{"mode": "yolo"},
			wantErr: "group.policy.mode must be auto, ask or readonly",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := loadGroupForTest(t, m, tc.gid)
			fileBefore := groupFileBytes(t, tc.gid)
			atomic.StoreInt32(refreshes, 0)
			cmdID++

			payload := map[string]any{"group_id": string(tc.gid)}
			for k, v := range tc.payload {
				payload[k] = v
			}
			ok, errMsg, _ := hubCmd(t, m, buf, cmdID, "hub_group_edit", payload)
			if ok {
				t.Fatalf("必须失败，却成功了（payload=%v）", payload)
			}
			if !strings.Contains(errMsg, tc.wantErr) {
				t.Fatalf("error = %q, want 含 %q", errMsg, tc.wantErr)
			}
			after := loadGroupForTest(t, m, tc.gid)
			if !reflect.DeepEqual(before, after) {
				t.Fatalf("失败后群被改了：\nbefore=%+v\nafter =%+v", before, after)
			}
			if fileAfter := groupFileBytes(t, tc.gid); !bytes.Equal(fileBefore, fileAfter) {
				t.Fatalf("失败后群文件被重写：\nbefore=%s\nafter =%s", fileBefore, fileAfter)
			}
			if n := atomic.LoadInt32(refreshes); n != 0 {
				t.Fatalf("失败也重建了 %d 次名册（应当零副作用）", n)
			}
		})
	}
}

// owner 转让 + 成员增删在同一条命令里：新 owner 来自「本次新加的人」，旧 owner 同时被移除。
func TestHubGroupEditOwnerTransferWithAddRemove(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "TransferOld")
	b := createHubAgent(t, m, buf, 2, "TransferKeep")
	d := createHubAgent(t, m, buf, 3, "TransferNew")
	g := groupWithMembers(t, m, buf, 4, "转让群",
		memberSpec{a.ID, "owner"},
		memberSpec{b.ID, "member"},
	)
	refreshes := countRosterRefresh(t)

	ok, errMsg, data := hubCmd(t, m, buf, 5, "hub_group_edit", map[string]any{
		"group_id":       string(g.ID),
		"owner":          string(d.ID),
		"add_members":    []string{string(d.ID)},
		"remove_members": []string{string(a.ID)},
	})
	if !ok {
		t.Fatalf("hub_group_edit: %s", errMsg)
	}
	got := groupFromData(t, data)
	if got.OwnerAgentID != d.ID {
		t.Fatalf("owner = %s, want %s", got.OwnerAgentID, d.ID)
	}
	if want := []string{string(b.ID), string(d.ID)}; !reflect.DeepEqual(groupMemberIDs(got), want) {
		t.Fatalf("members = %v, want %v", groupMemberIDs(got), want)
	}
	if role := groupRoleOf(got, d.ID); role != agenthub.RoleOwner {
		t.Fatalf("new owner role = %q", role)
	}
	if n := atomic.LoadInt32(refreshes); n != 1 {
		t.Fatalf("roster rebuild = %d, want 1", n)
	}
}

// 兼容：旧命令（update / member_add / member_remove）在新增 edit 之后语义不变。
func TestHubGroupEditKeepsLegacyCommands(t *testing.T) {
	m, buf := testManager(t)
	a := createHubAgent(t, m, buf, 1, "LegacyOwner")
	b := createHubAgent(t, m, buf, 2, "LegacyKeep")
	c := createHubAgent(t, m, buf, 3, "LegacyJoin")
	g := groupWithMembers(t, m, buf, 4, "旧命令群",
		memberSpec{a.ID, "owner"},
		memberSpec{b.ID, "member"},
	)
	if ok, errMsg, _ := hubCmd(t, m, buf, 5, "hub_group_update", map[string]any{
		"group_id": string(g.ID), "patch": map[string]any{"name": "旧命令群（改名）"},
	}); !ok {
		t.Fatalf("hub_group_update: %s", errMsg)
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 6, "hub_group_member_add", map[string]any{
		"group_id": string(g.ID), "agent_id": string(c.ID), "role": "member",
	}); !ok {
		t.Fatalf("hub_group_member_add: %s", errMsg)
	}
	if ok, errMsg, _ := hubCmd(t, m, buf, 7, "hub_group_member_remove", map[string]any{
		"group_id": string(g.ID), "agent_id": string(b.ID),
	}); !ok {
		t.Fatalf("hub_group_member_remove: %s", errMsg)
	}
	got := loadGroupForTest(t, m, g.ID)
	if got.Name != "旧命令群（改名）" {
		t.Fatalf("name = %q", got.Name)
	}
	if want := []string{string(a.ID), string(c.ID)}; !reflect.DeepEqual(groupMemberIDs(got), want) {
		t.Fatalf("members = %v, want %v", groupMemberIDs(got), want)
	}
}
