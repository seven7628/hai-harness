package agenthub

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func ledgerTask(id TaskID) *Task {
	return &Task{
		ID:          id,
		RootTaskID:  id,
		Chain:       []TaskID{id},
		Origin:      TaskOrigin{Kind: OriginGroup, GroupID: "grp_aaaaaa", AgentID: testAgentID('a')},
		Target:      TaskTarget{Kind: TargetAgent, AgentID: testAgentID('b'), GroupID: "grp_aaaaaa"},
		Title:       "写竞品报告",
		Instruction: "整理三家价格",
		Callback:    CallbackAgentEnd,
		Status:      TaskQueued,
		Attempt:     1,
		CreatedAt:   testTime(),
		UpdatedAt:   testTime(),
	}
}

func TestTaskStatusTable(t *testing.T) {
	statuses := []TaskStatus{TaskQueued, TaskDispatched, TaskRunning, TaskCompleted, TaskFailed,
		TaskInterrupted, TaskDelivered, TaskCanceled, TaskBlockedBudget, TaskDeclined}
	for _, s := range statuses {
		if !s.Valid() {
			t.Fatalf("%q should be valid", s)
		}
	}
	if TaskStatus("nope").Valid() {
		t.Fatal("unknown status is valid")
	}
	for _, s := range []TaskStatus{TaskCompleted, TaskFailed, TaskInterrupted, TaskDelivered, TaskCanceled, TaskBlockedBudget, TaskDeclined} {
		if !s.Terminal() {
			t.Fatalf("%q should be terminal", s)
		}
	}
	for _, s := range []TaskStatus{TaskQueued, TaskDispatched, TaskRunning} {
		if s.Terminal() {
			t.Fatalf("%q should not be terminal", s)
		}
	}

	tests := []struct {
		from, to TaskStatus
		want     bool
	}{
		{TaskQueued, TaskDispatched, true},
		{TaskQueued, TaskQueued, true},
		{TaskQueued, TaskRunning, false},
		{TaskDispatched, TaskRunning, true},
		{TaskDispatched, TaskDeclined, true},
		{TaskDispatched, TaskCompleted, true},
		{TaskDispatched, TaskDelivered, false},
		{TaskRunning, TaskCompleted, true},
		{TaskRunning, TaskCanceled, true},
		{TaskCompleted, TaskDelivered, true},
		{TaskCompleted, TaskRunning, false},
		{TaskDelivered, TaskCompleted, false},
		{TaskDelivered, TaskDelivered, true},
		{TaskCanceled, TaskQueued, false},
		{TaskBlockedBudget, TaskQueued, false},
		{TaskStatus("nope"), TaskQueued, false},
	}
	for _, tc := range tests {
		if got := tc.from.CanTransition(tc.to); got != tc.want {
			t.Fatalf("%q -> %q = %v, want %v", tc.from, tc.to, got, tc.want)
		}
	}
}

func TestTaskTransitionAndValidate(t *testing.T) {
	task := ledgerTask("task_aaaaaa")
	if err := task.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if err := task.Transition(TaskRunning, testTime()); !errors.Is(err, ErrInvalid) {
		t.Fatalf("queued->running err = %v", err)
	}
	if err := task.Transition(TaskDispatched, testTime()); err != nil {
		t.Fatal(err)
	}
	if task.Status != TaskDispatched {
		t.Fatalf("status = %q", task.Status)
	}
	if err := task.Transition(TaskRunning, testTime().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := task.Transition(TaskCompleted, testTime().Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := task.Transition(TaskRunning, testTime().Add(3*time.Second)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("completed->running err = %v", err)
	}
	if err := task.Transition(TaskDelivered, testTime().Add(4*time.Second)); err != nil {
		t.Fatalf("completed->delivered: %v", err)
	}
	if err := task.Transition(TaskCompleted, testTime().Add(5*time.Second)); !errors.Is(err, ErrInvalid) {
		t.Fatalf("delivered->completed err = %v", err)
	}

	bad := ledgerTask("task_bbbbbb")
	bad.Title = ""
	if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty title err = %v", err)
	}
	bad = ledgerTask("task_bbbbbb")
	bad.Chain = nil
	if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty chain err = %v", err)
	}
	bad = ledgerTask("task_bbbbbb")
	bad.Status = TaskStatus("nope")
	if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad status err = %v", err)
	}
	bad = ledgerTask("task_bbbbbb")
	bad.Target.Kind = "elsewhere"
	if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("bad target err = %v", err)
	}
}

func TestLedgerCreateAndReplay(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	ledger := NewLedger(store)
	task := ledgerTask("")
	task.ID, task.RootTaskID, task.Chain = "", "", nil
	created, err := ledger.Create(ctx, task)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if created.ID == "" || created.RootTaskID != created.ID || len(created.Chain) != 1 {
		t.Fatalf("created defaults = %#v", created)
	}
	events, err := store.ReadTaskEvents(ctx, created.ID)
	if err != nil || len(events) != 1 || events[0].Type != TaskEventCreated {
		t.Fatalf("created events = %#v, %v", events, err)
	}
	if _, err := ledger.Create(ctx, created); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate create err = %v, want ErrConflict", err)
	}

	if _, err := ledger.Transition(ctx, created.ID, TaskDelivered, TransitionOptions{}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("queued->delivered err = %v", err)
	}
	if _, err := ledger.Transition(ctx, created.ID, TaskDispatched, TransitionOptions{RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	before, _ := store.ReadTaskEvents(ctx, created.ID)
	if _, err := ledger.Transition(ctx, created.ID, TaskDispatched, TransitionOptions{RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	after, _ := store.ReadTaskEvents(ctx, created.ID)
	if len(after) != len(before) {
		t.Fatalf("same-status transition appended events: %d -> %d", len(before), len(after))
	}
	if _, err := ledger.Transition(ctx, created.ID, TaskRunning, TransitionOptions{RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	finished, err := ledger.Finish(ctx, FinishInput{
		TaskID: created.ID, RunID: "run_1", Status: TaskCompleted,
		Result: &TaskResult{Summary: "done", Usage: Usage{Input: 10, Output: 5, CostPriced: true, CostUSD: 0.1}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if finished.Status != TaskCompleted || finished.Result == nil || finished.Dispatch.AgentEndAt == nil {
		t.Fatalf("finished = %#v", finished)
	}
	before, _ = store.ReadTaskEvents(ctx, created.ID)
	if _, err := ledger.Finish(ctx, FinishInput{TaskID: created.ID, RunID: "run_1", Status: TaskCompleted}); err != nil {
		t.Fatal(err)
	}
	after, _ = store.ReadTaskEvents(ctx, created.ID)
	if len(after) != len(before) {
		t.Fatalf("same-status finish appended events: %d -> %d", len(before), len(after))
	}

	ok, err := ledger.MarkNotified(ctx, created.ID, "run_1")
	if err != nil || !ok {
		t.Fatalf("MarkNotified first = %v, %v", ok, err)
	}
	ok, err = ledger.MarkNotified(ctx, created.ID, "run_1")
	if err != nil || ok {
		t.Fatalf("MarkNotified second = %v, %v", ok, err)
	}
	replayed, err := ledger.Replay(ctx, created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.Status != TaskDelivered || replayed.Result == nil || replayed.Result.Summary != "done" ||
		len(replayed.NotifiedRuns) != 1 || replayed.NotifiedRuns[0] != "run_1" {
		t.Fatalf("replayed = %#v", replayed)
	}
}

func TestLedgerConcurrentMarkNotified(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	ledger := NewLedger(store)
	task, err := ledger.Create(ctx, ledgerTask("task_cccccc"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(ctx, task.ID, TaskDispatched, TransitionOptions{RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(ctx, task.ID, TaskRunning, TransitionOptions{RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Finish(ctx, FinishInput{TaskID: task.ID, RunID: "run_1", Status: TaskFailed, Error: "boom"}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	first := 0
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ok, err := ledger.MarkNotified(ctx, task.ID, "run_1")
			if err != nil {
				t.Errorf("MarkNotified: %v", err)
				return
			}
			if ok {
				mu.Lock()
				first++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if first != 1 {
		t.Fatalf("first notifications = %d, want 1", first)
	}
}

func TestLedgerReplayWithoutSnapshot(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	store, err := OpenFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	ledger := NewLedger(store)
	task := ledgerTask("")
	task.ID, task.RootTaskID, task.Chain = "", "", nil
	created, err := ledger.Create(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(ctx, created.ID, TaskDispatched, TransitionOptions{RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(ctx, created.ID, TaskRunning, TransitionOptions{RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Finish(ctx, FinishInput{TaskID: created.ID, RunID: "run_1", Status: TaskInterrupted, Error: "human"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.MarkNotified(ctx, created.ID, "run_1"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(root, "tasks", string(created.ID), "task.json")); err != nil {
		t.Fatal(err)
	}
	replayed, err := ledger.Replay(ctx, created.ID)
	if err != nil {
		t.Fatalf("Replay without snapshot: %v", err)
	}
	if replayed.Status != TaskDelivered || replayed.Error != "human" || len(replayed.NotifiedRuns) != 1 {
		t.Fatalf("replayed = %#v", replayed)
	}

	if _, err := ledger.Replay(ctx, "task_missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Replay missing err = %v, want ErrNotFound", err)
	}
}

func TestLedgerMarkNotifiedRequiresSettled(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	ledger := NewLedger(store)
	task, err := ledger.Create(ctx, ledgerTask("task_dddddd"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(ctx, task.ID, TaskDispatched, TransitionOptions{RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(ctx, task.ID, TaskRunning, TransitionOptions{RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.MarkNotified(ctx, task.ID, "run_1"); !errors.Is(err, ErrConflict) {
		t.Fatalf("mark before finish err = %v, want ErrConflict", err)
	}
	if _, err := ledger.Finish(ctx, FinishInput{TaskID: task.ID, RunID: "run_1", Status: TaskCompleted}); err != nil {
		t.Fatal(err)
	}
	ok, err := ledger.MarkNotified(ctx, task.ID, "run_1")
	if err != nil || !ok {
		t.Fatalf("mark after finish = %v, %v", ok, err)
	}
}

func TestLedgerFinishRunIDAndReplayEquality(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	ledger := NewLedger(store)
	task, err := ledger.Create(ctx, ledgerTask("task_eeeeee"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(ctx, task.ID, TaskDispatched, TransitionOptions{RunID: "run_old"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(ctx, task.ID, TaskRunning, TransitionOptions{RunID: "run_old"}); err != nil {
		t.Fatal(err)
	}
	finished, err := ledger.Finish(ctx, FinishInput{
		TaskID: task.ID, RunID: "run_new", Status: TaskCompleted,
		Result: &TaskResult{Summary: "authoritative"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if finished.Dispatch.RunID != "run_new" || len(finished.SupersededRuns) != 1 || finished.SupersededRuns[0] != "run_old" {
		t.Fatalf("superseded = %#v, dispatch = %#v", finished.SupersededRuns, finished.Dispatch)
	}
	loaded, err := store.LoadTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := ledger.Replay(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, replayed) {
		t.Fatalf("Replay != LoadTask:\n loaded: %#v\nreplay: %#v", loaded, replayed)
	}
	if _, err := ledger.Finish(ctx, FinishInput{TaskID: task.ID, RunID: "", Status: TaskCompleted}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("empty run finish err = %v", err)
	}
}

func TestLedgerTwoInstancesMarkNotifiedOnce(t *testing.T) {
	ctx := context.Background()
	store := NewMemoryStore()
	l1 := NewLedger(store)
	l2 := NewLedger(store)
	task, err := l1.Create(ctx, ledgerTask("task_ffffff"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := l1.Transition(ctx, task.ID, TaskDispatched, TransitionOptions{RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l1.Transition(ctx, task.ID, TaskRunning, TransitionOptions{RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := l1.Finish(ctx, FinishInput{TaskID: task.ID, RunID: "run_1", Status: TaskCompleted}); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	var mu sync.Mutex
	first := 0
	for i := 0; i < 16; i++ {
		l := l1
		if i%2 == 1 {
			l = l2
		}
		wg.Add(1)
		go func(l *Ledger) {
			defer wg.Done()
			ok, err := l.MarkNotified(ctx, task.ID, "run_1")
			if err != nil {
				t.Errorf("MarkNotified: %v", err)
				return
			}
			if ok {
				mu.Lock()
				first++
				mu.Unlock()
			}
		}(l)
	}
	wg.Wait()
	if first != 1 {
		t.Fatalf("cross-ledger first notifications = %d, want 1", first)
	}
}

type failingAppendStore struct {
	Store
	fail   bool
	onFail func()
}

func (s *failingAppendStore) AppendTaskEvent(ctx context.Context, id TaskID, ev TaskEvent) (int64, error) {
	if s.fail {
		if s.onFail != nil {
			s.onFail()
		}
		return 0, errors.New("injected append failure")
	}
	return s.Store.AppendTaskEvent(ctx, id, ev)
}

func TestLedgerAppendFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	store := &failingAppendStore{Store: NewMemoryStore()}
	ledger := NewLedger(store)
	task, err := ledger.Create(ctx, ledgerTask("task_gggggg"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(ctx, task.ID, TaskDispatched, TransitionOptions{RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	store.fail = true
	if _, err := ledger.Transition(ctx, task.ID, TaskRunning, TransitionOptions{RunID: "run_1"}); err == nil {
		t.Fatal("expected injected append failure")
	}
	loaded, err := store.LoadTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != TaskDispatched {
		t.Fatalf("snapshot not rolled back: %q", loaded.Status)
	}
	store.fail = false
	if _, err := ledger.Transition(ctx, task.ID, TaskRunning, TransitionOptions{RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	replayed, err := ledger.Replay(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	loaded, err = store.LoadTask(ctx, task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(loaded, replayed) {
		t.Fatalf("Replay after rollback != LoadTask:\n loaded: %#v\nreplay: %#v", loaded, replayed)
	}
}

// P2.5-02 N-f：append 失败伴随 ctx 取消时，回滚仍必须落盘。
// 旧实现复用调用方 ctx，SaveTask 直接返回 context.Canceled，快照永久停在
// dispatched（既没派发、也没事件）。
func TestLedgerAppendFailureRollsBackAfterContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	store := &failingAppendStore{Store: NewMemoryStore()}
	ledger := NewLedger(store)
	task, err := ledger.Create(ctx, ledgerTask("task_cccccc"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(ctx, task.ID, TaskDispatched, TransitionOptions{RunID: "run_1"}); err != nil {
		t.Fatal(err)
	}
	store.fail = true
	store.onFail = cancel
	if _, err := ledger.Transition(ctx, task.ID, TaskRunning, TransitionOptions{RunID: "run_1"}); err == nil {
		t.Fatal("expected injected append failure")
	}
	store.fail = false
	loaded, err := store.LoadTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.Status != TaskDispatched {
		t.Fatalf("rollback lost after ctx cancel: %q", loaded.Status)
	}
}
