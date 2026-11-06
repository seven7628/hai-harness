package agenthub

// P2-07：预算/并发/去重/链长护栏。

import (
	"context"
	"errors"
	"testing"
	"time"
)

func guardTask(text string) *Task {
	t := queueTask(text)
	t.ID = ""
	t.Instruction = text
	return t
}

func newGuardStore(t *testing.T) (*FileStore, *Ledger) {
	t.Helper()
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return st, NewLedger(st)
}

func addTask(t *testing.T, l *Ledger, text string, cost float64, priced bool) *Task {
	t.Helper()
	ctx := context.Background()
	task := guardTask(text)
	created, err := l.Create(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l.Transition(ctx, created.ID, TaskDispatched, TransitionOptions{At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	_, err = l.Finish(ctx, FinishInput{
		TaskID: created.ID, RunID: "r1", Status: TaskCompleted,
		Result: &TaskResult{Summary: "done", Usage: Usage{CostPriced: priced, CostUSD: cost}},
		At:     time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := l.Replay(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestGuardChainTooDeep(t *testing.T) {
	st, _ := newGuardStore(t)
	task := guardTask("接力")
	for i := 0; i < 7; i++ {
		task.Chain = append(task.Chain, TaskID("task_step"+string(rune('a'+i))))
	}
	g := &Guard{Limits: GuardLimits{MaxChainSteps: 6}}
	if err := g.Check(context.Background(), st, task); !errors.Is(err, ErrChainTooDeep) {
		t.Fatalf("err = %v, want chain too deep", err)
	}
	task.Chain = task.Chain[:6]
	if err := g.Check(context.Background(), st, task); err != nil {
		t.Fatalf("6 steps should pass: %v", err)
	}
	// 重试 attempt 的追加 id 不消耗链深：第 9 次 attempt 仍应按 1 跳计。
	retry := guardTask("接力")
	retry.Chain = []TaskID{"task_root", "task_a1", "task_a2", "task_a3", "task_a4", "task_a5", "task_a6", "task_a7", "task_a8"}
	retry.Attempt = 9
	if got := retry.ChainSteps(); got != 1 {
		t.Fatalf("retry chain steps = %d, want 1", got)
	}
	if err := g.Check(context.Background(), st, retry); err != nil {
		t.Fatalf("retry attempt must not hit chain limit: %v", err)
	}
}

func TestGuardDuplicateWindow(t *testing.T) {
	st, l := newGuardStore(t)
	addTask(t, l, "同一件事", 0, false)
	now := time.Now().UTC()
	g := &Guard{Now: func() time.Time { return now }, Limits: GuardLimits{DuplicateWindow: 10 * time.Minute, MaxConcurrent: 10}}
	if err := g.Check(context.Background(), st, guardTask("同一件事")); !errors.Is(err, ErrDuplicateTask) {
		t.Fatalf("err = %v, want duplicate", err)
	}
	g.Now = func() time.Time { return now.Add(11 * time.Minute) }
	if err := g.Check(context.Background(), st, guardTask("同一件事")); err != nil {
		t.Fatalf("after window: %v", err)
	}
}

func TestGuardConcurrency(t *testing.T) {
	st, l := newGuardStore(t)
	for _, text := range []string{"a", "b", "c"} {
		if _, err := l.Create(context.Background(), guardTask(text)); err != nil {
			t.Fatal(err)
		}
	}
	g := &Guard{Limits: GuardLimits{MaxConcurrent: 3}}
	if err := g.Check(context.Background(), st, guardTask("d")); !errors.Is(err, ErrTooConcurrent) {
		t.Fatalf("err = %v, want too concurrent", err)
	}
	// 不同发起者不共享并发额度。
	other := guardTask("d")
	other.Origin.AgentID = "ag_other"
	if err := g.Check(context.Background(), st, other); err != nil {
		t.Fatalf("other agent: %v", err)
	}
}

func TestGuardBudgets(t *testing.T) {
	st, l := newGuardStore(t)
	addTask(t, l, "已花 5 元", 5, true)
	addTask(t, l, "免费任务", 0, true)
	addTask(t, l, "未知成本", 99, false) // 不能被当成 99 元
	base := guardTask("新任务")
	now := time.Now().UTC()
	g := &Guard{Now: func() time.Time { return now }, Limits: GuardLimits{DailyBudgetUSD: 4, MaxConcurrent: 10}}
	if err := g.Check(context.Background(), st, base); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("daily err = %v, want budget exceeded", err)
	}
	g.Limits.DailyBudgetUSD = 0
	g.Limits.GlobalDailyBudgetUSD = 4
	if err := g.Check(context.Background(), st, base); !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("global err = %v, want budget exceeded", err)
	}
	g.Limits.GlobalDailyBudgetUSD = 0
	g.Limits.GroupDailyBudgetUSD = 4
	grouped := guardTask("群任务")
	grouped.Origin.GroupID = "grp_1"
	// 现有任务没有 group：群预算不命中。
	if err := g.Check(context.Background(), st, grouped); err != nil {
		t.Fatalf("group without prior cost: %v", err)
	}
}

func TestGuardOverTaskBudget(t *testing.T) {
	l := GuardLimits{MaxTaskCostUSD: 2}
	if over, _ := l.OverTaskBudget(Usage{CostPriced: true, CostUSD: 2.5}); !over {
		t.Fatal("2.5 > 2 should be over")
	}
	if over, _ := l.OverTaskBudget(Usage{CostPriced: false, CostUSD: 99}); over {
		t.Fatal("unknown cost must not be treated as over/zero")
	}
	if over, _ := l.OverTaskBudget(Usage{CostPriced: true, CostUSD: 1.9}); over {
		t.Fatal("1.9 < 2 should pass")
	}
}
