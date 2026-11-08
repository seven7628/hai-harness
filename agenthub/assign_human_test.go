package agenthub

// P3b-07a：人类来源的 agent 目标任务（Origin.Kind=human，没有发起智能体）。
//
// 与 P3b-03 的「群内智能体互派」共用同一条 SubmitAgent/pumpAgent 路径，差异只有
// 两条按发起智能体的检查：同对在途上限、不能派给自己（人没有 agent id）。

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
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

// TestGuardSkipAgentScope：人派任务的护栏只看全局/群预算与链长。
//
// 关键点：所有人类来源的 Origin.AgentID 都是空串，所以「按发起智能体」的三项
// 额度（并发/去重/智能体日预算）会把**所有人**的任务算成同一个桶。这里用
// OriginHuman + 空 AgentID + 同群同目标的种子把三项都做成「必然命中」：
//   - 不开 SkipAgentScope：必须 ErrTooConcurrent / ErrDuplicateTask / ErrBudgetExceeded
//     （证明种子是有效的，跳过不是空转）；
//   - 开了：同样种子上必须全部放行；
//   - 链长与全局/群日预算：即使开了也必须继续生效。
//
// 两个 Store 实现都要覆盖：有内存索引的快路径（checkIndexed）与没有索引的全盘
// 扫描（ListTasks）。SkipAgentScope 在任一路径上被忽略，对应用例立刻变红。
func TestGuardSkipAgentScope(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()
	stores := map[string]func() Store{
		"indexed":  func() Store { return NewMemoryStore() },
		"fallback": func() Store { return &fallbackStore{Store: NewMemoryStore()} },
	}
	for name, mk := range stores {
		t.Run(name, func(t *testing.T) {
			st := mk()
			l := NewLedger(st)
			// 三条在途的人派任务：并发额度与去重键（同群同目标）的种子。
			for _, text := range []string{"在途一", "在途二", "在途三"} {
				if _, err := l.Create(ctx, humanAssignTask("ag_b", text)); err != nil {
					t.Fatalf("seed inflight %q: %v", text, err)
				}
			}
			// 一条已完成、花费 $5 的人派任务：智能体日/群/全局预算的种子。
			spent := humanAssignTask("ag_b", "已花钱")
			created, err := l.Create(ctx, spent)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := l.Transition(ctx, created.ID, TaskDispatched, TransitionOptions{At: now}); err != nil {
				t.Fatal(err)
			}
			if _, err := l.Finish(ctx, FinishInput{
				TaskID: created.ID, RunID: "r-scope", Status: TaskCompleted,
				Result: &TaskResult{Summary: "done", Usage: Usage{CostPriced: true, CostUSD: 5}},
				At:     now,
			}); err != nil {
				t.Fatal(err)
			}
			probe := func(text string) *Task {
				p := humanAssignTask("ag_b", text)
				p.CreatedAt, p.UpdatedAt = now, now
				return p
			}
			check := func(g *Guard, task *Task) error {
				g.Now = func() time.Time { return now }
				return g.Check(ctx, st, task)
			}

			// —— 不开 SkipAgentScope：三项按 agent 的额度都必须命中 ——
			if err := check(&Guard{Limits: GuardLimits{MaxConcurrent: 10, DuplicateWindow: 10 * time.Minute}}, probe("在途一")); !errors.Is(err, ErrDuplicateTask) {
				t.Fatalf("去重窗口 err = %v, want ErrDuplicateTask", err)
			}
			if err := check(&Guard{Limits: GuardLimits{MaxConcurrent: 3}}, probe("新任务")); !errors.Is(err, ErrTooConcurrent) {
				t.Fatalf("并发上限 err = %v, want ErrTooConcurrent", err)
			}
			if err := check(&Guard{Limits: GuardLimits{MaxConcurrent: 10, DailyBudgetUSD: 1}}, probe("新任务")); !errors.Is(err, ErrBudgetExceeded) ||
				!strings.Contains(err.Error(), "智能体日预算") {
				t.Fatalf("智能体日预算 err = %v, want ErrBudgetExceeded（智能体日预算）", err)
			}

			// —— 开 SkipAgentScope：同样的种子上三项全部放行 ——
			if err := check(&Guard{Limits: GuardLimits{MaxConcurrent: 10, DuplicateWindow: 10 * time.Minute, SkipAgentScope: true}}, probe("在途一")); err != nil {
				t.Fatalf("SkipAgentScope 应跳过去重窗口: %v", err)
			}
			if err := check(&Guard{Limits: GuardLimits{MaxConcurrent: 3, SkipAgentScope: true}}, probe("新任务")); err != nil {
				t.Fatalf("SkipAgentScope 应跳过并发上限: %v", err)
			}
			if err := check(&Guard{Limits: GuardLimits{MaxConcurrent: 10, DailyBudgetUSD: 1, SkipAgentScope: true}}, probe("新任务")); err != nil {
				t.Fatalf("SkipAgentScope 应跳过智能体日预算: %v", err)
			}

			// —— 链长仍然生效（与是否跳过 agent 额度无关）——
			deep := probe("接力")
			for i := 0; i < 7; i++ {
				deep.Chain = append(deep.Chain, TaskID("task_scope"+string(rune('a'+i))))
			}
			if err := check(&Guard{Limits: GuardLimits{MaxChainSteps: 6, SkipAgentScope: true}}, deep); !errors.Is(err, ErrChainTooDeep) {
				t.Fatalf("链长 err = %v, want ErrChainTooDeep", err)
			}

			// —— 全局/群日预算仍然生效（种子 $5）——
			if err := check(&Guard{Limits: GuardLimits{GroupDailyBudgetUSD: 4, SkipAgentScope: true}}, probe("新任务")); !errors.Is(err, ErrBudgetExceeded) ||
				!strings.Contains(err.Error(), "群日预算") {
				t.Fatalf("群日预算 err = %v, want ErrBudgetExceeded（群日预算）", err)
			}
			if err := check(&Guard{Limits: GuardLimits{GlobalDailyBudgetUSD: 4, SkipAgentScope: true}}, probe("新任务")); !errors.Is(err, ErrBudgetExceeded) ||
				!strings.Contains(err.Error(), "全局日预算") {
				t.Fatalf("全局日预算 err = %v, want ErrBudgetExceeded（全局日预算）", err)
			}
		})
	}
}
