package agenthub

// task_index_test.go：P2.5-02 索引与回退口径。

import (
	"context"
	"errors"
	"testing"
	"time"
)

func indexTestTask(agent AgentID, instruction, clientMsgID string) *Task {
	return &Task{
		Origin:      TaskOrigin{Kind: OriginDM, AgentID: agent, GroupID: "grp_idx"},
		Target:      TaskTarget{Kind: TargetWorkspace, Workspace: "/tmp/idx/", SessionID: " s1 "},
		Title:       instruction,
		Instruction: instruction,
		ClientMsgID: clientMsgID,
	}
}

func TestTaskIndexClientActiveAndCost(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	led := NewLedger(st)
	now := time.Now().UTC()

	created, err := led.Create(ctx, indexTestTask("ag_idx", "do it", "c-1"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, ok := st.FindByClientMsgID(TaskTarget{Kind: TargetWorkspace, Workspace: "/tmp/idx", SessionID: "s1"}, "c-1")
	if !ok || got == nil || got.ID != created.ID {
		t.Fatalf("FindByClientMsgID = %v, %v; want %s", got, ok, created.ID)
	}
	if n := st.ActiveCount("ag_idx"); n != 1 {
		t.Fatalf("active = %d, want 1", n)
	}

	// 结算后有价用量进入当日成本；取消则退出 active。
	if _, err := led.Transition(ctx, created.ID, TaskCanceled, TransitionOptions{At: now}); err != nil {
		t.Fatalf("cancel: %v", err)
	}
	if n := st.ActiveCount("ag_idx"); n != 0 {
		t.Fatalf("active after cancel = %d, want 0", n)
	}

	done, err := led.Create(ctx, indexTestTask("ag_idx", "spend", "c-2"))
	if err != nil {
		t.Fatalf("create 2: %v", err)
	}
	if _, err := led.Transition(ctx, done.ID, TaskDispatched, TransitionOptions{At: now}); err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	if _, err := led.Transition(ctx, done.ID, TaskCompleted, TransitionOptions{
		At:     now,
		Result: &TaskResult{Usage: Usage{CostPriced: true, CostUSD: 2.5}},
	}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	agentCost, groupCost, globalCost := st.DailyCost("ag_idx", "grp_idx", now)
	if agentCost != 2.5 || groupCost != 2.5 || globalCost != 2.5 {
		t.Fatalf("daily cost = %v/%v/%v, want 2.5 each", agentCost, groupCost, globalCost)
	}

	// 换成无价用量：旧成本必须从索引里扣掉，不能继续算钱。
	if _, err := led.Transition(ctx, done.ID, TaskCompleted, TransitionOptions{
		At:     now.Add(time.Minute),
		Result: &TaskResult{Usage: Usage{CostPriced: false, CostUSD: 99}},
	}); err != nil {
		t.Fatalf("unpriced update: %v", err)
	}
	agentCost, groupCost, globalCost = st.DailyCost("ag_idx", "grp_idx", now)
	if agentCost != 0 || groupCost != 0 || globalCost != 0 {
		t.Fatalf("daily cost after unpriced = %v/%v/%v, want 0", agentCost, groupCost, globalCost)
	}
}

func TestTaskIndexDuplicateAndClientRetry(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	led := NewLedger(st)
	now := time.Now().UTC()

	old, err := led.Create(ctx, indexTestTask("ag_dup", "same", "c-old"))
	if err != nil {
		t.Fatalf("create old: %v", err)
	}
	probe := indexTestTask("ag_dup", "same", "c-new")
	if id, ok := st.FindDuplicate(probe, now.Add(-10*time.Minute)); !ok || id != old.ID {
		t.Fatalf("FindDuplicate = %s, %v; want %s", id, ok, old.ID)
	}
	// 同 client_msg_id 是显式重投，不算重复。
	retryProbe := indexTestTask("ag_dup", "same", "c-old")
	if id, ok := st.FindDuplicate(retryProbe, now.Add(-10*time.Minute)); ok {
		t.Fatalf("same client id should be skipped, got %s", id)
	}
	// 窗口外不再命中。
	if id, ok := st.FindDuplicate(probe, now.Add(time.Minute)); ok {
		t.Fatalf("out-of-window duplicate = %s, want none", id)
	}
}

func TestFileStoreRebuildsTaskIndex(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	st, err := OpenFileStore(root)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	led := NewLedger(st)
	created, err := led.Create(ctx, indexTestTask("ag_file_idx", "persist", "c-file"))
	if err != nil {
		t.Fatalf("create: %v", err)
	}

	reopened, err := OpenFileStore(root)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	got, ok := reopened.FindByClientMsgID(TaskTarget{Kind: TargetWorkspace, Workspace: "/tmp/idx", SessionID: "s1"}, "c-file")
	if !ok || got == nil || got.ID != created.ID {
		t.Fatalf("rebuilt FindByClientMsgID = %v, %v; want %s", got, ok, created.ID)
	}
	if n := reopened.ActiveCount("ag_file_idx"); n != 1 {
		t.Fatalf("rebuilt active = %d, want 1", n)
	}
}

// countingListStore 嵌入 MemoryStore：TaskIndex 方法被提升，ListTasks 被计数。
type countingListStore struct {
	*MemoryStore
	listCalls int
}

func (s *countingListStore) ListTasks(ctx context.Context, filter TaskFilter) ([]*Task, error) {
	s.listCalls++
	return s.MemoryStore.ListTasks(ctx, filter)
}

func TestGuardUsesTaskIndexWithoutListTasks(t *testing.T) {
	ctx := context.Background()
	base := NewMemoryStore()
	st := &countingListStore{MemoryStore: base}
	led := NewLedger(st)
	now := time.Now().UTC()

	if _, err := led.Create(ctx, indexTestTask("ag_guard_idx", "same", "c-1")); err != nil {
		t.Fatalf("create: %v", err)
	}
	if got, err := led.FindByClientMsgID(ctx, TaskTarget{Kind: TargetWorkspace, Workspace: "/tmp/idx", SessionID: "s1"}, "c-1"); err != nil || got == nil {
		t.Fatalf("indexed FindByClientMsgID = %v, %v", got, err)
	}
	probe := indexTestTask("ag_guard_idx", "same", "c-2")
	probe.CreatedAt = now
	probe.UpdatedAt = now
	g := &Guard{Now: func() time.Time { return now }, Limits: GuardLimits{MaxConcurrent: 3}}
	err := g.Check(ctx, st, probe)
	if !errors.Is(err, ErrDuplicateTask) {
		t.Fatalf("Check err = %v, want ErrDuplicateTask", err)
	}
	if st.listCalls != 0 {
		t.Fatalf("Guard.Check called ListTasks %d times; index fast path not used", st.listCalls)
	}
}
