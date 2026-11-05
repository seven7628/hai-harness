package agenthub

// P3a-03：@ 路由 + round + 回执。

import (
	"context"
	"errors"
	"testing"
	"time"
)

func routerFixture(t *testing.T) (*Group, *Router) {
	t.Helper()
	profiles := []*AgentProfile{
		{ID: "ag_aaaaaaaa", Name: "Alice"},
		{ID: "ag_bbbbbbbb", Name: "Bob"},
		{ID: "ag_cccccccc", Name: "Cara"},
		{ID: "ag_dddddddd", Name: "Dan"},
		{ID: "ag_eeeeeeee", Name: "Eve"},
	}
	g := &Group{
		ID: "grp_aaaaaa", Name: "路由群", OwnerAgentID: "ag_aaaaaaaa",
		Members: []Member{
			{AgentID: "ag_aaaaaaaa", Role: RoleOwner, JoinedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)},
			{AgentID: "ag_bbbbbbbb", Role: RoleMember, JoinedAt: time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC)},
			{AgentID: "ag_cccccccc", Role: RoleMember, JoinedAt: time.Date(2026, 1, 3, 0, 0, 0, 0, time.UTC)},
			{AgentID: "ag_dddddddd", Role: RoleMember, JoinedAt: time.Date(2026, 1, 4, 0, 0, 0, 0, time.UTC)},
			{AgentID: "ag_eeeeeeee", Role: RoleObserver, JoinedAt: time.Date(2026, 1, 5, 0, 0, 0, 0, time.UTC)},
		},
	}
	r, err := NewGroupRouter(g, profiles)
	if err != nil {
		t.Fatal(err)
	}
	return g, r
}

func TestRouterNoMentionNoWake(t *testing.T) {
	_, r := routerFixture(t)
	round, err := r.Route("大家好，今天先同步一下进度", "ag_aaaaaaaa", false, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(round.Targets) != 0 {
		t.Fatalf("targets = %v, want none", round.Targets)
	}
}

func TestRouterExplicitMentionsAndSelf(t *testing.T) {
	_, r := routerFixture(t)
	round, err := r.Route("@Bob 看一下日志；@Alice 自己也提一下；@Cara 复核", "ag_aaaaaaaa", false, 2, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(round.Targets) != 2 || round.Targets[0] != "ag_bbbbbbbb" || round.Targets[1] != "ag_cccccccc" {
		t.Fatalf("targets = %v", round.Targets)
	}
	// 未知 @ 不唤醒。
	round, err = r.Route("@Nobody 在吗", "ag_aaaaaaaa", false, 3, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(round.Targets) != 0 || len(round.Unknown) != 1 || round.Unknown[0] != "Nobody" {
		t.Fatalf("round = %+v", round)
	}
}

func TestRouterMentionAllAuthorization(t *testing.T) {
	_, r := routerFixture(t)
	// owner 可以 @all：目标是除自己外全部成员（按 JoinedAt/ID 确定）。
	round, err := r.Route("@all 开始", "ag_aaaaaaaa", false, 4, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if !round.MentionAll || len(round.Targets) != 4 || round.Targets[0] != "ag_bbbbbbbb" || round.Targets[3] != "ag_eeeeeeee" {
		t.Fatalf("owner @all = %+v", round)
	}
	// 普通成员 @all：拒绝且不降级。
	if _, err := r.Route("@all 开始", "ag_bbbbbbbb", false, 5, time.Now()); !errors.Is(err, ErrMentionAllForbidden) {
		t.Fatalf("member @all err = %v, want ErrMentionAllForbidden", err)
	}
	// 人类可以 @all（sender 空）。
	round, err = r.Route("@all 开会", "", true, 6, time.Now())
	if err != nil || len(round.Targets) != 5 {
		t.Fatalf("human @all = %+v, %v", round, err)
	}
}

func TestRouterMentionLimitAndReceipt(t *testing.T) {
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	g, r := routerFixture(t)
	g.Policy.MaxMentions = 2
	if err := g.Normalize(); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveGroup(context.Background(), g); err != nil {
		t.Fatal(err)
	}
	round, err := r.Route("@Bob @Cara @Dan @Eve 四个都来", "ag_aaaaaaaa", false, 7, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(round.Targets) != 2 || round.Dropped != 2 {
		t.Fatalf("limit round = %+v", round)
	}
	if err := AppendRoundReceipt(context.Background(), st, round); err != nil {
		t.Fatal(err)
	}
	acts, err := st.ReadActivity(context.Background(), g.ID, 0, 10)
	if err != nil || len(acts) != 1 {
		t.Fatalf("activity = %+v, %v", acts, err)
	}
	if acts[0].Kind != "routing" || acts[0].AfterSeq != 7 || acts[0].RunID != round.ID {
		t.Fatalf("receipt = %+v", acts[0])
	}
	if acts[0].Summary != "targets=ag_bbbbbbbb,ag_cccccccc dropped=2" {
		t.Fatalf("summary = %q", acts[0].Summary)
	}
	if _, err := round.TargetsOf(); err != nil {
		t.Fatal(err)
	}
	dup := Round{Targets: []AgentID{"ag_aaaaaaaa", "ag_aaaaaaaa"}}
	if _, err := dup.TargetsOf(); err == nil {
		t.Fatal("duplicate targets must be rejected")
	}
}
