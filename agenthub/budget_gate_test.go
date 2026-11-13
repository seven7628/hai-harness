package agenthub

// P6-03：预跑预算门的三级判定（agent / group / global）。

import (
	"strings"
	"testing"
	"time"
)

// TestOverDailyBudgetThreeLevels 锁住「群/agent/全局三级」：任何一级到顶都拒绝，
// 说明文案要指出是哪一级（会写进群系统消息，用户据此知道该去调哪个预算）。
func TestOverDailyBudgetThreeLevels(t *testing.T) {
	limits := GuardLimits{DailyBudgetUSD: 20, GroupDailyBudgetUSD: 50, GlobalDailyBudgetUSD: 200}

	if over, _ := limits.OverDailyBudget(0, 0, 0); over {
		t.Fatal("零成本不该拦")
	}
	// agent 级：>= 即到顶（与 Guard.Check 同口径）。
	over, msg := limits.OverDailyBudget(20, 0, 0)
	if !over || !strings.Contains(msg, "智能体") {
		t.Fatalf("agent 级 = %v %q, want 拒绝且说明智能体预算", over, msg)
	}
	// group 级。
	over, msg = limits.OverDailyBudget(0, 50, 0)
	if !over || !strings.Contains(msg, "本群") {
		t.Fatalf("group 级 = %v %q, want 拒绝且说明群预算", over, msg)
	}
	// global 级。
	over, msg = limits.OverDailyBudget(0, 0, 200)
	if !over || !strings.Contains(msg, "全局") {
		t.Fatalf("global 级 = %v %q, want 拒绝且说明全局预算", over, msg)
	}
	// 0 = 不限额：三级都为 0 时永不拦。
	none := GuardLimits{}
	if over, _ := none.OverDailyBudget(1e9, 1e9, 1e9); over {
		t.Fatal("零值预算 = 不限额，不该拦")
	}
	// 只有一级限额：另外两级再大也不影响这一级。
	onlyAgent := GuardLimits{DailyBudgetUSD: 5}
	if over, _ := onlyAgent.OverDailyBudget(1, 1e9, 1e9); over {
		t.Fatal("未配置的级别不该生效")
	}
	if over, _ := onlyAgent.OverDailyBudget(5, 0, 0); !over {
		t.Fatal("配置了的 agent 级必须生效")
	}
}

// TestSkipWakeMarksBudgetReason：预算门命中时账本记 skipped(budget) 并出队 ——
// 人能看到「这条没跑，因为预算」，而不是队列悄悄不动。
func TestSkipWakeMarksBudgetReason(t *testing.T) {
	c, _ := newTestController(t, ConversationLimits{Cooldown: -1, PairLimit: 100, MaxDepth: 100})
	gid := GroupID("g_budget_gate")
	c.BeginHuman(gid, 1, time.Time{})
	c.StartHumanRound(gid, 2, time.Time{})
	wakes := mustOffer(t, c, gid, "agent_a", []AgentID{"agent_b", "agent_c"}, 1)
	if !c.SkipWake(gid, wakes[0].ID, DeliveryReasonBudget, time.Time{}) {
		t.Fatal("SkipWake 必须生效")
	}
	if d := deliveryByID(t, c, gid, wakes[0].ID); d.Status != DeliverySkipped || d.Reason != DeliveryReasonBudget {
		t.Fatalf("跳过的记录 = %+v, want skipped/budget", d)
	}
	if v := c.View(gid); v.QueueLen != 1 {
		t.Fatalf("queue = %d, want 1（只跳过命中的那条）", v.QueueLen)
	}
	// 已终结的记录不能再 skip（幂等）。
	if c.SkipWake(gid, wakes[0].ID, DeliveryReasonBudget, time.Time{}) {
		t.Fatal("终态记录不该再被 skip")
	}
}
