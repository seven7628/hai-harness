package agenthub

// P2-03：任务账本 + 空闲闸门 + 队列。

import (
	"context"
	"errors"
	"testing"
)

type queueFakeOps struct {
	running     bool
	pending     int
	dispatched  []DispatchReq
	dispatchErr error
}

func (f *queueFakeOps) ListWorkspaces(context.Context) ([]WorkspaceInfo, error) { return nil, nil }

func (f *queueFakeOps) CreateWorkspace(context.Context, CreateWorkspaceReq) (WorkspaceInfo, error) {
	return WorkspaceInfo{}, nil
}

func (f *queueFakeOps) ListSessions(context.Context, string) ([]SessionInfo, error) { return nil, nil }

func (f *queueFakeOps) SessionStatus(context.Context, string, string) (SessionInfo, error) {
	return SessionInfo{ID: "s1", Workspace: "/w", Running: f.running, Pending: f.pending}, nil
}

func (f *queueFakeOps) CreateSession(context.Context, CreateSessionReq) (SessionInfo, error) {
	return SessionInfo{}, nil
}

func (f *queueFakeOps) Dispatch(_ context.Context, req DispatchReq) (DispatchResult, error) {
	if f.dispatchErr != nil {
		return DispatchResult{}, f.dispatchErr
	}
	f.dispatched = append(f.dispatched, req)
	return DispatchResult{SessionID: req.SessionID, Accepted: true}, nil
}

func (f *queueFakeOps) InterruptSession(context.Context, string) error { return nil }

func newTestQueue(t *testing.T, ops WorkspaceOps) (*Queue, *FileStore) {
	t.Helper()
	st, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return NewQueue(NewLedger(st), ops), st
}

func queueTask(text string) *Task {
	return &Task{
		Origin:      TaskOrigin{Kind: OriginDM, AgentID: "ag_q"},
		Target:      TaskTarget{Kind: TargetWorkspace, Workspace: "/w", SessionID: "s1"},
		Title:       "t",
		Instruction: text,
		Callback:    CallbackAgentEnd,
	}
}

func TestQueueIdleDispatch(t *testing.T) {
	ops := &queueFakeOps{}
	q, st := newTestQueue(t, ops)
	created, err := q.Submit(context.Background(), queueTask("做一遍对比"))
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != TaskDispatched {
		t.Fatalf("status = %q, want dispatched", created.Status)
	}
	if created.ID == "" || created.RootTaskID != created.ID {
		t.Fatalf("ledger id/root = %q/%q", created.ID, created.RootTaskID)
	}
	if len(ops.dispatched) != 1 || ops.dispatched[0].Text != "做一遍对比" ||
		ops.dispatched[0].AgentID != "ag_q" || ops.dispatched[0].ClientMsgID != string(created.ID) {
		t.Fatalf("dispatch = %+v", ops.dispatched)
	}
	// 账本可回放，且状态已落盘。
	got, err := NewLedger(st).Replay(context.Background(), created.ID)
	if err != nil || got.Status != TaskDispatched {
		t.Fatalf("replay = %+v, %v", got, err)
	}
	if q.QueuedCount("/w", "s1") != 0 {
		t.Fatalf("queue not empty after idle dispatch")
	}
}

func TestQueueBusyStaysQueuedThenPumpsOne(t *testing.T) {
	ops := &queueFakeOps{running: true}
	q, _ := newTestQueue(t, ops)
	first, err := q.Submit(context.Background(), queueTask("one"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := q.Submit(context.Background(), queueTask("two"))
	if err != nil {
		t.Fatal(err)
	}
	if first.Status != TaskQueued || second.Status != TaskQueued {
		t.Fatalf("busy submit status = %q/%q, want queued", first.Status, second.Status)
	}
	if len(ops.dispatched) != 0 || q.QueuedCount("/w", "s1") != 2 {
		t.Fatalf("busy target must not dispatch: calls=%d queued=%d", len(ops.dispatched), q.QueuedCount("/w", "s1"))
	}
	// 目标空闲后一次 Pump 只派发一个（不能把两个任务塞进同一批）。
	ops.running = false
	got, err := q.Pump(context.Background(), TaskTarget{Kind: TargetWorkspace, Workspace: "/w", SessionID: "s1"})
	if err != nil || got == nil || got.ID != first.ID {
		t.Fatalf("first pump = %+v, %v", got, err)
	}
	if len(ops.dispatched) != 1 || q.QueuedCount("/w", "s1") != 1 {
		t.Fatalf("pump must dispatch exactly one: calls=%d queued=%d", len(ops.dispatched), q.QueuedCount("/w", "s1"))
	}
	got, err = q.Pump(context.Background(), TaskTarget{Kind: TargetWorkspace, Workspace: "/w", SessionID: "s1"})
	if err != nil || got == nil || got.ID != second.ID {
		t.Fatalf("second pump = %+v, %v", got, err)
	}
	if q.QueuedCount("/w", "s1") != 0 {
		t.Fatal("queue should be empty")
	}
}

func TestQueuePendingGatesDispatch(t *testing.T) {
	ops := &queueFakeOps{pending: 1}
	q, _ := newTestQueue(t, ops)
	task, err := q.Submit(context.Background(), queueTask("wait"))
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != TaskQueued || len(ops.dispatched) != 0 {
		t.Fatalf("pending>0 must queue: status=%q calls=%d", task.Status, len(ops.dispatched))
	}
	ops.pending = 0
	if _, err := q.Pump(context.Background(), task.Target); err != nil {
		t.Fatal(err)
	}
	if len(ops.dispatched) != 1 {
		t.Fatalf("dispatch after pending drained = %d", len(ops.dispatched))
	}
}

func TestQueueDispatchFailureMarksFailed(t *testing.T) {
	ops := &queueFakeOps{dispatchErr: errors.New("目标会话已关闭")}
	q, st := newTestQueue(t, ops)
	task, err := q.Submit(context.Background(), queueTask("boom"))
	if err == nil {
		t.Fatal("dispatch failure must surface")
	}
	got, rerr := NewLedger(st).Replay(context.Background(), task.ID)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if got.Status != TaskFailed || got.Error == "" {
		t.Fatalf("failed task = %+v", got)
	}
	if q.QueuedCount("/w", "s1") != 0 {
		t.Fatal("failed task must leave the queue")
	}
}

func TestQueueValidation(t *testing.T) {
	q, _ := newTestQueue(t, &queueFakeOps{})
	if _, err := (*Queue)(nil).Submit(context.Background(), queueTask("x")); !errors.Is(err, ErrInvalid) {
		t.Fatalf("nil queue = %v", err)
	}
	bad := []*Task{
		nil,
		{Origin: TaskOrigin{Kind: OriginDM}, Target: TaskTarget{Kind: TargetWorkspace}, Instruction: "x"},
		{Origin: TaskOrigin{Kind: OriginDM}, Target: TaskTarget{Kind: TargetWorkspace, Workspace: "/w"}, Instruction: "x"},
		{Origin: TaskOrigin{Kind: OriginDM}, Target: TaskTarget{Kind: TargetWorkspace, Workspace: "/w", SessionID: "s1"}, Instruction: "  "},
		{Origin: TaskOrigin{Kind: OriginDM}, Target: TaskTarget{Kind: TargetAgent, Workspace: "/w", SessionID: "s1"}, Instruction: "x"},
	}
	for i, task := range bad {
		if _, err := q.Submit(context.Background(), task); !errors.Is(err, ErrInvalid) {
			t.Errorf("case %d = %v, want ErrInvalid", i, err)
		}
	}
}
