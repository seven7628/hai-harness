package agenthub

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// 反假开关（P6-13）：GroupPolicy 里**还留着的**每个字段，改它 → 行为必须跟着变。
//
// 为什么值得一条独立测试：max_turns_per_human 曾经「字段在、默认值在、校验在、UI 在」，
// 唯独没有任何判定读它 —— 只断言「字段能存能读」的测试完全挡不住这种死旋钮。
// 这里的每个子用例都走**真实判定函数**（CheckHandoff / Router.Route / Guard.Check），
// 断言的是行为，不是字段值。
//
// Mode 的真实闸门在 bridge（hubGroupModeFor → agents.Mode 的 Allow/Approve），
// 见 desktop/bridge 的 TestHubGroupModeForFollowsPolicy；本文件只钉它在这里的
// 归一化/校验边界。
func TestGroupPolicyFieldsEffective(t *testing.T) {
	now := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC)

	t.Run("MaxHandoffDepth", func(t *testing.T) {
		g := &Group{
			ID: "grp_eff_depth", Name: "深度群", OwnerAgentID: "ag_aaaaaaaa",
			Members: []Member{
				{AgentID: "ag_aaaaaaaa", Role: RoleOwner},
				{AgentID: "ag_bbbbbbbb", Role: RoleMember},
			},
		}
		// depth=2：上限 2 → 拒绝（2 >= 2），上限 4 → 放行。改字段 = 改行为。
		g.Policy.MaxHandoffDepth = 2
		if err := CheckHandoff(g, "ag_aaaaaaaa", "ag_bbbbbbbb", 2, nil, now); !errors.Is(err, ErrHandoffTooDeep) {
			t.Fatalf("max_handoff_depth=2 时 depth=2 必须被拒: %v", err)
		}
		g.Policy.MaxHandoffDepth = 4
		if err := CheckHandoff(g, "ag_aaaaaaaa", "ag_bbbbbbbb", 2, nil, now); err != nil {
			t.Fatalf("max_handoff_depth=4 时 depth=2 必须放行: %v", err)
		}
	})

	t.Run("MaxMentions", func(t *testing.T) {
		g, r := routerFixture(t)
		text := "@Bob @Cara @Dan @Eve 四个都来"
		// 上限 3（默认）→ 3 个目标 + 1 个被丢弃；上限 1 → 1 个目标 + 3 个丢弃。
		g.Policy.MaxMentions = DefaultGroupMaxMentions
		round, err := r.Route(text, "ag_aaaaaaaa", false, 1, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(round.Targets) != 3 || round.Dropped != 1 {
			t.Fatalf("max_mentions=%d round = %+v（want 3 targets / 1 dropped）", DefaultGroupMaxMentions, round)
		}
		g.Policy.MaxMentions = 1
		round, err = r.Route(text, "ag_aaaaaaaa", false, 2, now)
		if err != nil {
			t.Fatal(err)
		}
		if len(round.Targets) != 1 || round.Dropped != 3 {
			t.Fatalf("max_mentions=1 round = %+v（want 1 target / 3 dropped）", round)
		}
	})

	t.Run("DailyBudgetUSD", func(t *testing.T) {
		ctx := context.Background()
		st, ledger := newGuardStore(t)
		const gid = GroupID("grp_eff_budget")
		save := func(budget float64) {
			t.Helper()
			if err := st.SaveGroup(ctx, &Group{
				ID: gid, Name: "预算群", Policy: GroupPolicy{DailyBudgetUSD: budget},
				CreatedAt: now, UpdatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
		}
		// 先花掉 $6（已结算），再用同一份 Group.Policy 判「下一个 run 能不能起」。
		spent := guardTask("已花群预算")
		spent.Origin.Kind = OriginGroup
		spent.Origin.GroupID = gid
		created, err := ledger.Create(ctx, spent)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ledger.Transition(ctx, created.ID, TaskDispatched, TransitionOptions{At: now}); err != nil {
			t.Fatal(err)
		}
		if _, err := ledger.Finish(ctx, FinishInput{
			TaskID: created.ID, RunID: "r-eff", Status: TaskCompleted,
			Result: &TaskResult{Usage: Usage{CostPriced: true, CostUSD: 6}},
			At:     now,
		}); err != nil {
			t.Fatal(err)
		}
		g := &Guard{
			Now:    func() time.Time { return now },
			Limits: GuardLimits{MaxConcurrent: 10},
			// 与生产同一条接线：初始派发时从 Store 读 Group.Policy.DailyBudgetUSD。
			GroupDailyBudget: func(ctx context.Context, got GroupID) (float64, error) {
				grp, err := st.LoadGroup(ctx, got)
				if err != nil {
					return 0, err
				}
				return grp.Policy.DailyBudgetUSD, nil
			},
		}
		next := guardTask("下一个群任务")
		next.Origin.Kind = OriginGroup
		next.Origin.GroupID = gid
		save(5) // $5 < 已花 $6 → 拒绝
		if err := g.Check(ctx, st, next); !errors.Is(err, ErrBudgetExceeded) {
			t.Fatalf("daily_budget_usd=5 且已花 6 必须拒绝: %v", err)
		}
		save(10) // 改字段 → 放行
		if err := g.Check(ctx, st, next); err != nil {
			t.Fatalf("daily_budget_usd=10 必须放行: %v", err)
		}
		save(0) // 0 = 不限额（UI 文案同口径）
		if err := g.Check(ctx, st, next); err != nil {
			t.Fatalf("daily_budget_usd=0 表示不限额，必须放行: %v", err)
		}
	})

	t.Run("Mode", func(t *testing.T) {
		// 这里能钉的是「值域 + 零值归一」：非法值必须被拒，零值必须归一到 ask，
		// 否则 bridge 的闸门会拿到一个它认不出的模式（见 TestHubGroupModeForFollowsPolicy）。
		for _, bad := range []string{"read-only", "AUTO", "yolo"} {
			if err := (GroupPolicy{Mode: bad}).Validate(); err == nil {
				t.Fatalf("mode=%q 必须被拒", bad)
			}
		}
		for _, good := range []string{GroupModeAuto, GroupModeAsk, GroupModeReadonly} {
			if err := (GroupPolicy{Mode: good}).Validate(); err != nil {
				t.Fatalf("mode=%q 必须被接受: %v", good, err)
			}
			if got := (GroupPolicy{Mode: good}).EffectiveMode(); got != good {
				t.Fatalf("EffectiveMode(%q) = %q", good, got)
			}
		}
		if got := (GroupPolicy{}).EffectiveMode(); got != GroupModeAsk {
			t.Fatalf("零值必须归一到 ask，got %q", got)
		}
	})
}

// TestGroupPolicyIgnoresRemovedFields：已删的旋钮出现在旧 JSON 里必须被**忽略**
// 而不是报错（P6-02 删了 max_parallel_runs，P6-13 删了 max_turns_per_human）。
// 存量群文件/旧客户端不需要迁移脚本，也不能因为多一个键就读不出群。
func TestGroupPolicyIgnoresRemovedFields(t *testing.T) {
	raw := `{
	  "id":"grp_legacy","name":"旧群","owner_agent_id":"ag_aaaaaaaa",
	  "members":[{"agent_id":"ag_aaaaaaaa","role":"owner"}],
	  "policy":{"mode":"ask","max_handoff_depth":5,"max_parallel_runs":7,
	            "max_turns_per_human":8,"max_mentions_per_message":2,"daily_budget_usd":9}
	}`
	var g Group
	if err := json.Unmarshal([]byte(raw), &g); err != nil {
		t.Fatalf("旧 JSON 带已删字段必须能读（忽略而不是报错）: %v", err)
	}
	if g.Policy.MaxHandoffDepth != 5 || g.Policy.MaxMentions != 2 || g.Policy.DailyBudgetUSD != 9 || g.Policy.Mode != GroupModeAsk {
		t.Fatalf("合法字段被旧键连累: %+v", g.Policy)
	}
	// 归一化/校验同样不能因为旧键失败（否则整个群加载不了）。
	if err := g.Normalize(); err != nil {
		t.Fatalf("Normalize 不能因为已删字段失败: %v", err)
	}
	out, err := json.Marshal(g)
	if err != nil {
		t.Fatal(err)
	}
	// 再写回磁盘时不能把死旋钮写回去 —— 否则「字段已删」只是表面功夫。
	for _, dead := range []string{"max_turns_per_human", "max_parallel_runs"} {
		if strings.Contains(string(out), dead) {
			t.Fatalf("序列化后仍带已删字段 %q: %s", dead, out)
		}
	}
}
