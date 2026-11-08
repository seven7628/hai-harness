package agenthub

// P3b-07a：人类来源的 agent 目标任务（Origin.Kind=human，没有发起智能体）。
//
// 与 P3b-03 的「群内智能体互派」共用同一条 SubmitAgent/pumpAgent 路径，差异只有
// 两条按发起智能体的检查：同对在途上限、不能派给自己（人没有 agent id）。

import (
	"context"
	"errors"
	"testing"
)

// humanAssignTask 建一条「人在群里派给 B」的任务。
func humanAssignTask(to AgentID, text string) *Task {
	return &Task{
		Origin:      TaskOrigin{Kind: OriginHuman, GroupID: "grp_assign"},
		Target:      TaskTarget{Kind: TargetAgent, AgentID: to, GroupID: "grp_assign", SessionID: "hub." + string(to) + ".g.grp_assign"},
		Title:       "人类指派",
		Instruction: text,
		Callback:    CallbackAgentEnd,
	}
}

func TestQueueSubmitAgentAllowsHumanOrigin(t *testing.T) {
	q, st, ops := newAssignQueue(t)
	ctx := context.Background()

	created, err := q.SubmitAgent(ctx, humanAssignTask("ag_b", "把价格表整理成一页"), nil)
	if err != nil {
		t.Fatalf("人类来源应被接受: %v", err)
	}
	if created.Status != TaskQueued {
		t.Fatalf("status = %q, want queued（SubmitAgent 不派发）", created.Status)
	}
	if created.Origin.Kind != OriginHuman || created.Origin.GroupID != "grp_assign" || created.Origin.AgentID != "" {
		t.Fatalf("origin = %+v, want human/grp_assign/no agent", created.Origin)
	}
	if q.QueuedCountFor(created.Target) != 1 {
		t.Fatalf("queued = %d, want 1", q.QueuedCountFor(created.Target))
	}
	// 同对上限对人不适用：同一目标连派 3 条（> MaxAgentPairInFlight）都应成功。
	for i := 0; i < MaxAgentPairInFlight; i++ {
		if _, err := q.SubmitAgent(ctx, humanAssignTask("ag_b", "第 "+string(rune('1'+i))+" 件"), nil); err != nil {
			t.Fatalf("人类来源不应受同对在途上限约束（第 %d 条）: %v", i+1, err)
		}
	}
	if got := q.QueuedCountFor(created.Target); got != MaxAgentPairInFlight+1 {
		t.Fatalf("queued = %d, want %d", got, MaxAgentPairInFlight+1)
	}

	// 派发：OriginAgent 为空（没有发起者），任务照常投给目标。
	dispatched, err := q.Pump(ctx, created.Target)
	if err != nil {
		t.Fatal(err)
	}
	if dispatched == nil || dispatched.ID != created.ID || dispatched.Status != TaskDispatched {
		t.Fatalf("pump = %+v", dispatched)
	}
	sent := ops.sent()
	if len(sent) != 1 || sent[0].OriginAgent != "" || sent[0].Target.AgentID != "ag_b" {
		t.Fatalf("dispatch req = %+v", sent)
	}
	replayed, err := NewLedger(st).Replay(ctx, created.ID)
	if err != nil || replayed.Origin.Kind != OriginHuman || replayed.Origin.AgentID != "" {
		t.Fatalf("replay origin = %+v, %v", replayed, err)
	}
}

func TestQueueSubmitAgentRejectsInvalidHumanOrigin(t *testing.T) {
	q, _, _ := newAssignQueue(t)
	ctx := context.Background()

	agentless := humanAssignTask("ag_b", "没有群")
	agentless.Origin.GroupID = ""

	withAgent := humanAssignTask("ag_b", "人却带了 agent id")
	withAgent.Origin.AgentID = "ag_a"

	crossGroup := humanAssignTask("ag_b", "跨群")
	crossGroup.Origin.GroupID = "grp_other"

	emptyText := humanAssignTask("ag_b", "   ")

	dm := humanAssignTask("ag_b", "DM 来源")
	dm.Origin = TaskOrigin{Kind: OriginDM, AgentID: "ag_a"}
	dm.Origin.GroupID = "grp_assign"

	// 群来源但缺发起智能体（agentless non-human）：以前/现在都必须拒绝。
	agentlessGroup := assignTask("", "ag_b", "群来源缺 agent")
	agentlessGroup.Origin.GroupID = "grp_assign"

	for name, task := range map[string]*Task{
		"human 缺 group":     agentless,
		"human 带了 agent id": withAgent,
		"human 跨群":          crossGroup,
		"human 空指令":         emptyText,
		"DM 来源":             dm,
		"群来源缺发起智能体":         agentlessGroup,
	} {
		if _, err := q.SubmitAgent(ctx, task, nil); !errors.Is(err, ErrInvalid) {
			t.Fatalf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if n := q.QueuedCountFor(humanAssignTask("ag_b", "x").Target); n != 0 {
		t.Fatalf("拒绝路径不应入队，queued = %d", n)
	}
}

// TestGuardSkipAgentScope：人派任务的护栏只看全局/群预算与链长；
// 按发起智能体的并发/去重/智能体日预算三项跳过（否则所有人类任务会共享
// 同一个空 agent 桶，第三条就被「并发上限」挡住）。
func TestGuardSkipAgentScope(t *testing.T) {
	st, l := newGuardStore(t)
	ctx := context.Background()
	// 三条在途任务（不同发起者）+ 一条超预算的智能体日成本。
	for _, text := range []string{"a", "b", "c"} {
		if _, err := l.Create(ctx, guardTask(text)); err != nil {
			t.Fatal(err)
		}
	}
	addTask(t, l, "已花 5 元", 5, true)

	human := humanAssignTask("ag_b", "a") // 与在途任务同指令：去重窗口也不该命中
	human.Target.AgentID = "ag_q"
	g := &Guard{Limits: GuardLimits{MaxConcurrent: 3, DailyBudgetUSD: 1, SkipAgentScope: true}}
	if err := g.Check(ctx, st, human); err != nil {
		t.Fatalf("SkipAgentScope 应跳过并发/去重/智能体日预算: %v", err)
	}

	// 全局/群预算仍然生效。
	g.Limits.GlobalDailyBudgetUSD = 4
	if err := g.Check(ctx, st, human); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("全局预算 err = %v, want budget exceeded", err)
	}
	g.Limits.GlobalDailyBudgetUSD = 0
	g.Limits.GroupDailyBudgetUSD = 4
	if err := g.Check(ctx, st, human); err != nil {
		t.Fatalf("群预算在没有群成本时不应命中: %v", err)
	}

	// 对照组：同样的任务在 agent 作用域下会被并发/去重挡住（证明上面不是空转）。
	agentTask := assignTask("ag_q", "ag_b", "a")
	agentTask.Origin.AgentID = "ag_q"
	g = &Guard{Limits: GuardLimits{MaxConcurrent: 3, DailyBudgetUSD: 1}}
	if err := g.Check(ctx, st, agentTask); !errors.Is(err, ErrTooConcurrent) {
		t.Fatalf("agent 作用域 err = %v, want too concurrent", err)
	}
}
