package agenthub

// P6-12：observer 角色删除 + 旧数据迁移。
//
// 三条不变量：
//  1. 旧 JSON 里的 observer 在**加载时**迁成 member —— 不删人、不丢历史；
//  2. 新写入带 observer 一律拒绝（否则迁移永远做不完）；
//  3. 角色只剩 owner/member，@all 覆盖全部活跃成员（除发送者）。

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// legacyObserverGroup 是 P6-12 之前的落盘形状：owner + member + observer。
func legacyObserverGroup() *Group {
	at := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	return &Group{
		ID: "grp_legacy", Name: "旧群", OwnerAgentID: "ag_aaaaaaaa",
		Members: []Member{
			{AgentID: "ag_aaaaaaaa", Role: RoleOwner, JoinedAt: at},
			{AgentID: "ag_bbbbbbbb", Role: RoleMember, JoinedAt: at.Add(time.Minute)},
			{AgentID: "ag_cccccccc", Role: MemberRole(legacyRoleObserver), JoinedAt: at.Add(2 * time.Minute)},
		},
		Policy:    GroupPolicy{Mode: GroupModeAsk, MaxMentions: 3},
		CreatedAt: at, UpdatedAt: at,
	}
}

func TestLoadGroupMigratesLegacyObserver(t *testing.T) {
	ctx := context.Background()
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	old := legacyObserverGroup()
	// 直接写盘（绕过 SaveGroup 的新写入拒绝）：模拟升级前就存在的 group.json。
	if err := writeJSONAtomic(st.groupPath(old.ID), old); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(st.groupPath(old.ID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), legacyRoleObserver) {
		t.Fatalf("fixture 没写进 observer: %s", raw)
	}

	// 历史：迁移前后都要读得到（迁移不能丢历史）。
	msg := &Message{GroupID: old.ID, Kind: KindText, Sender: Sender{Kind: SenderHuman}, Text: "历史消息"}
	if _, err := st.AppendMessage(ctx, msg); err != nil {
		t.Fatal(err)
	}

	got, err := st.LoadGroup(ctx, old.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Members) != 3 {
		t.Fatalf("迁移删了成员: %+v", got.Members)
	}
	roles := map[AgentID]MemberRole{}
	for _, m := range got.Members {
		roles[m.AgentID] = m.Role
	}
	if roles["ag_cccccccc"] != RoleMember {
		t.Fatalf("observer 没迁成 member: %+v", got.Members)
	}
	if roles["ag_aaaaaaaa"] != RoleOwner || roles["ag_bbbbbbbb"] != RoleMember {
		t.Fatalf("迁移改动了其它角色: %+v", got.Members)
	}
	if got.OwnerAgentID != "ag_aaaaaaaa" {
		t.Fatalf("迁移改动了 owner: %s", got.OwnerAgentID)
	}
	// 成员顺序与 JoinedAt 原样保留（审计字段不能被迁移重写）。
	if got.Members[2].AgentID != "ag_cccccccc" || !got.Members[2].JoinedAt.Equal(old.Members[2].JoinedAt) {
		t.Fatalf("迁移改动了成员顺序/JoinedAt: %+v", got.Members[2])
	}
	msgs, err := st.ReadMessages(ctx, old.ID, 0, 10)
	if err != nil || len(msgs) != 1 || msgs[0].Text != "历史消息" {
		t.Fatalf("迁移后历史读不到: %+v (%v)", msgs, err)
	}

	// ListGroups 同口径（左栏列表不能显示 observer）。
	groups, err := st.ListGroups(ctx)
	if err != nil || len(groups) != 1 {
		t.Fatalf("ListGroups: %+v (%v)", groups, err)
	}
	for _, m := range groups[0].Members {
		if m.Role != RoleOwner && m.Role != RoleMember {
			t.Fatalf("ListGroups 仍有非法角色: %+v", groups[0].Members)
		}
	}

	// 游标写是「读整份→改字段→写整份」：不能把 observer 又写回盘里。
	if _, err := st.MarkGroupRead(ctx, old.ID, 1); err != nil {
		t.Fatal(err)
	}
	raw, err = os.ReadFile(st.groupPath(old.ID))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), `"role": "`+legacyRoleObserver+`"`) {
		t.Fatalf("MarkGroupRead 把 observer 写回了盘里: %s", raw)
	}
	if !strings.Contains(string(raw), `"role": "member"`) {
		t.Fatalf("迁移后的角色没落盘: %s", raw)
	}
}

func TestApplyDefaultsMigratesLegacyObserver(t *testing.T) {
	g := legacyObserverGroup()
	g.ApplyDefaults()
	if g.Members[2].Role != RoleMember {
		t.Fatalf("ApplyDefaults 没迁移 observer: %+v", g.Members[2])
	}
	// 归一化之后 Validate 必须通过（否则旧群连保存都做不到）。
	if err := g.Validate(); err != nil {
		t.Fatalf("迁移后 Validate: %v", err)
	}
	// 大小写/空白也要认（旧数据可能是手写/别的版本落盘的）。
	g2 := legacyObserverGroup()
	g2.Members[2].Role = "  Observer "
	if err := g2.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if g2.Members[2].Role != RoleMember {
		t.Fatalf("大小写变体没迁移: %q", g2.Members[2].Role)
	}
}

func TestSaveGroupRejectsLegacyObserver(t *testing.T) {
	ctx := context.Background()
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g := legacyObserverGroup()
	if err := st.SaveGroup(ctx, g); !errors.Is(err, ErrInvalid) {
		t.Fatalf("新写入 observer 必须被拒绝，得到 %v", err)
	}
	if _, err := st.LoadGroup(ctx, g.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("被拒绝的写入不该落盘: %v", err)
	}
	// 内存实现同口径。
	mem := NewMemoryStore()
	if err := mem.SaveGroup(ctx, g); !errors.Is(err, ErrInvalid) {
		t.Fatalf("MemoryStore 也必须拒绝 observer，得到 %v", err)
	}

	// 归一化（= 迁移）之后可以保存：旧群因此能继续被编辑。
	g.MigrateLegacyMemberRoles()
	if err := g.Validate(); err != nil {
		t.Fatalf("迁移后 Validate: %v", err)
	}
	if err := st.SaveGroup(ctx, g); err != nil {
		t.Fatalf("迁移后保存: %v", err)
	}
}

func TestValidateRejectsLegacyObserver(t *testing.T) {
	g := legacyObserverGroup()
	// 不经过 Normalize：Validate 必须拒绝（角色只剩两种）。
	if err := g.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("未迁移的 observer 必须 Validate 失败，得到 %v", err)
	}
	if !strings.Contains(errString(g.Validate()), "invalid role") {
		t.Fatalf("错误应说明角色非法: %v", g.Validate())
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

// @all 覆盖全部活跃成员（除发送者）：迁移成 member 的旧 observer 也必须在内。
func TestMentionAllCoversAllActiveMembersExceptSender(t *testing.T) {
	g := legacyObserverGroup()
	g.MigrateLegacyMemberRoles()
	profiles := []*AgentProfile{
		{ID: "ag_aaaaaaaa", Name: "Owner"},
		{ID: "ag_bbbbbbbb", Name: "Member"},
		{ID: "ag_cccccccc", Name: "Migrated"},
	}
	router, err := NewGroupRouter(g, profiles)
	if err != nil {
		t.Fatal(err)
	}
	round, err := router.Route("@all 开会", "ag_aaaaaaaa", false, 1, time.Date(2026, 3, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	got := map[AgentID]bool{}
	for _, id := range round.Targets {
		got[id] = true
	}
	if len(round.Targets) != 2 || !got["ag_bbbbbbbb"] || !got["ag_cccccccc"] {
		t.Fatalf("@all targets = %+v, want 除发送者外的全部活跃成员", round.Targets)
	}
	if got["ag_aaaaaaaa"] {
		t.Fatal("@all 不该包含发送者自己")
	}
}
