package agenthub

// P2-03：任务账本 + 空闲闸门 + 队列。

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type queueFakeOps struct {
	running      bool
	pending      int
	dispatched   []DispatchReq
	dispatchErr  error
	statusErr    error
	duplicate    bool
	dispatchWait bool
	// dispatchStarted 在 Dispatch 入口非阻塞发信号（缓冲 1），用于把
	// 「已出队、正在投递」的窗口做成确定性的测试锚点。
	dispatchStarted chan struct{}
}

func (f *queueFakeOps) ListWorkspaces(context.Context) ([]WorkspaceInfo, error) { return nil, nil }

func (f *queueFakeOps) CreateWorkspace(context.Context, CreateWorkspaceReq) (WorkspaceInfo, error) {
	return WorkspaceInfo{}, nil
}

func (f *queueFakeOps) ListSessions(context.Context, string) ([]SessionInfo, error) { return nil, nil }

func (f *queueFakeOps) SessionStatus(context.Context, string, string) (SessionInfo, error) {
	if f.statusErr != nil {
		return SessionInfo{}, f.statusErr
	}
	return SessionInfo{ID: "s1", Workspace: "/w", Running: f.running, Pending: f.pending}, nil
}

func (f *queueFakeOps) CreateSession(context.Context, CreateSessionReq) (SessionInfo, error) {
	return SessionInfo{}, nil
}

func (f *queueFakeOps) Dispatch(ctx context.Context, req DispatchReq) (DispatchResult, error) {
	if f.dispatchStarted != nil {
		select {
		case f.dispatchStarted <- struct{}{}:
		default:
		}
	}
	if f.dispatchWait {
		<-ctx.Done()
		return DispatchResult{}, ctx.Err()
	}
	if f.dispatchErr != nil {
		return DispatchResult{}, f.dispatchErr
	}
	f.dispatched = append(f.dispatched, req)
	msg := ""
	if f.duplicate {
		msg = DispatchDuplicateMessage
	}
	return DispatchResult{SessionID: req.SessionID, Accepted: true, Message: msg}, nil
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

// failSaveStore 包一层 Store：第 failAt+1 次 SaveTask 起报错（模拟磁盘满/权限）。
type failSaveStore struct {
	Store
	failAt int
	saves  int
}

func (s *failSaveStore) SaveTask(ctx context.Context, t *Task) error {
	s.saves++
	if s.saves > s.failAt {
		return errors.New("save task: no space left on device")
	}
	return s.Store.SaveTask(ctx, t)
}

// P2-03 B2：有积压时 Submit 必须返回**自己**那条，而不是 Pump 泵走的队首。
func TestQueueSubmitWithBacklogReturnsOwnTask(t *testing.T) {
	ops := &queueFakeOps{running: true}
	q, _ := newTestQueue(t, ops)
	a, err := q.Submit(context.Background(), queueTask("任务A"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := q.Submit(context.Background(), queueTask("任务B"))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID == b.ID || a.Status != TaskQueued || b.Status != TaskQueued {
		t.Fatalf("a=%s/%s b=%s/%s", a.ID, a.Status, b.ID, b.Status)
	}
	// 闸门刚空闲（还没人泵）：Submit C 内部的 Pump 会派走队首 A。
	// 这个时点才能杀死「dispatched != nil 就返回」的旧实现（复审 B-2）。
	ops.running = false
	c, err := q.Submit(context.Background(), queueTask("任务C"))
	if err != nil {
		t.Fatal(err)
	}
	if c.ID == a.ID || c.ID == b.ID || c.Instruction != "任务C" || c.Status != TaskQueued {
		t.Fatalf("C 拿到别人的账本条目: id=%s text=%q status=%s", c.ID, c.Instruction, c.Status)
	}
	if len(ops.dispatched) != 1 || ops.dispatched[0].Text != "任务A" {
		t.Fatalf("pump should dispatch A, got %+v", ops.dispatched)
	}
	if got := q.QueuedCount("/w", "s1"); got != 2 {
		t.Fatalf("queued = %d, want 2", got)
	}
}

// P2-03 B3：Transition 失败时任务必须留在队列里（账本 queued 与内存一致）。
func TestQueueTransitionFailureKeepsQueued(t *testing.T) {
	ops := &queueFakeOps{running: true}
	base, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Submit 会 SaveTask 一次；让下一次（Pump 的 queued→dispatched）失败。
	store := &failSaveStore{Store: base, failAt: 1}
	q := NewQueue(NewLedger(store), ops)
	created, err := q.Submit(context.Background(), queueTask("落盘失败的任务"))
	if err != nil || created.Status != TaskQueued {
		t.Fatalf("submit = %+v, %v", created, err)
	}
	ops.running = false
	if _, err := q.Pump(context.Background(), created.Target); err == nil {
		t.Fatal("Transition 应失败")
	}
	if got := q.QueuedCount("/w", "s1"); got != 1 {
		t.Fatalf("Transition 失败后 queued = %d, want 1（不能先出队）", got)
	}
	got, err := NewLedger(base).Replay(context.Background(), created.ID)
	if err != nil {
		t.Fatalf("replay: %v", err)
	}
	if got.Status != TaskQueued {
		t.Fatalf("ledger status = %s, want queued", got.Status)
	}
	// 存储恢复后同一条任务仍能派出去（不是永久卡死）。
	store.failAt = 1 << 30
	if _, err := q.Pump(context.Background(), created.Target); err != nil {
		t.Fatal(err)
	}
	if len(ops.dispatched) != 1 || ops.dispatched[0].ClientMsgID != string(created.ID) {
		t.Fatalf("dispatched = %+v", ops.dispatched)
	}
}

// P2-03 N1：目标会话已不存在时队首转 canceled，不能永久堵住队列。
func TestQueueTargetGoneCancelsHead(t *testing.T) {
	ops := &queueFakeOps{running: true}
	q, st := newTestQueue(t, ops)
	created, err := q.Submit(context.Background(), queueTask("目标已删除"))
	if err != nil {
		t.Fatal(err)
	}
	ops.statusErr = ErrNotFound
	if _, err := q.Pump(context.Background(), created.Target); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pump err = %v, want not found", err)
	}
	if got := q.QueuedCount("/w", "s1"); got != 0 {
		t.Fatalf("queued = %d, want 0", got)
	}
	got, err := NewLedger(st).Replay(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskCanceled {
		t.Fatalf("ledger status = %s, want canceled", got.Status)
	}
}

// P2-02 复审 B2：Queue 模式必须保留调用方 client_msg_id，同 id 重试返回同一台账本任务。
func TestQueueSubmitClientMsgIDIdempotent(t *testing.T) {
	ops := &queueFakeOps{running: true}
	q, _ := newTestQueue(t, ops)
	mk := func(text string) *Task {
		t := queueTask(text)
		t.ClientMsgID = "client-key-1"
		return t
	}
	a, err := q.Submit(context.Background(), mk("第一次"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := q.Submit(context.Background(), mk("第二次（重试）"))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("same client id created two tasks: %s vs %s", a.ID, b.ID)
	}
	if got := q.QueuedCount("/w", "s1"); got != 1 {
		t.Fatalf("queued = %d, want 1", got)
	}
	ops.running = false
	if _, err := q.Pump(context.Background(), a.Target); err != nil {
		t.Fatal(err)
	}
	if len(ops.dispatched) != 1 || ops.dispatched[0].ClientMsgID != "client-key-1" {
		t.Fatalf("dispatch client id = %+v", ops.dispatched)
	}
	if ops.dispatched[0].Text != "第一次" {
		t.Fatalf("dispatched text = %q, want first submission", ops.dispatched[0].Text)
	}
}

// P2-06：取消队列里的任务（未开始）→ canceled 且离开内存队列。
func TestQueueCancelQueued(t *testing.T) {
	ops := &queueFakeOps{running: true}
	q, st := newTestQueue(t, ops)
	created, err := q.Submit(context.Background(), queueTask("待取消"))
	if err != nil || created.Status != TaskQueued {
		t.Fatalf("submit = %+v, %v", created, err)
	}
	got, err := q.CancelQueued(context.Background(), created.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != TaskCanceled {
		t.Fatalf("status = %s, want canceled", got.Status)
	}
	if n := q.QueuedCount("/w", "s1"); n != 0 {
		t.Fatalf("queued = %d, want 0", n)
	}
	led, err := NewLedger(st).Replay(context.Background(), created.ID)
	if err != nil || led.Status != TaskCanceled {
		t.Fatalf("ledger = %+v, %v", led, err)
	}
	// 已结束/不存在的任务不能再按 queued 取消。
	if _, err := q.CancelQueued(context.Background(), created.ID); !errors.Is(err, ErrInvalid) {
		t.Fatalf("second cancel err = %v, want ErrInvalid", err)
	}
}

// P2-03 复审 B-1：ErrNotFound 分支 Transition 失败时不能在出队（否则账本 queued、
// 内存没有、存储恢复后也泵不出）。
func TestQueueTargetGoneCancelFailureKeepsHead(t *testing.T) {
	ops := &queueFakeOps{running: true}
	base, err := OpenFileStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	// Submit 成功 SaveTask 一次；下一次（Pump 的 canceled）失败。
	store := &failSaveStore{Store: base, failAt: 1}
	q := NewQueue(NewLedger(store), ops)
	created, err := q.Submit(context.Background(), queueTask("目标消失"))
	if err != nil || created.Status != TaskQueued {
		t.Fatalf("submit = %+v, %v", created, err)
	}
	ops.statusErr = ErrNotFound
	if _, err := q.Pump(context.Background(), created.Target); err == nil {
		t.Fatal("cancel transition should fail")
	}
	if got := q.QueuedCount("/w", "s1"); got != 1 {
		t.Fatalf("queued after failed cancel = %d, want 1（保留队首）", got)
	}
	led, err := NewLedger(base).Replay(context.Background(), created.ID)
	if err != nil || led.Status != TaskQueued {
		t.Fatalf("ledger = %+v, %v", led, err)
	}
	// 存储恢复后 Pump：canceled 且出队。
	store.failAt = 1 << 30
	if _, err := q.Pump(context.Background(), created.Target); !errors.Is(err, ErrNotFound) {
		t.Fatalf("pump after recovery err = %v", err)
	}
	if got := q.QueuedCount("/w", "s1"); got != 0 {
		t.Fatalf("queued after recovery = %d, want 0", got)
	}
	led, _ = NewLedger(base).Replay(context.Background(), created.ID)
	if led.Status != TaskCanceled {
		t.Fatalf("ledger status = %s, want canceled", led.Status)
	}
}

// P2-02 复审 B1：命中失败终态时同 client_msg_id 必须是新 attempt，而不是
// 静默返回旧 failed 任务（否则用户重试永远不重投）。
func TestQueueSubmitRetryAfterFailedStartsNewAttempt(t *testing.T) {
	ops := &queueFakeOps{}
	q, st := newTestQueue(t, ops)
	first := queueTask("第一次")
	first.ClientMsgID = "retry-key"
	a, err := q.Submit(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.ledger.Transition(context.Background(), a.ID, TaskFailed, TransitionOptions{Error: "模拟失败"}); err != nil {
		t.Fatal(err)
	}
	retry := queueTask("第二次（重试）")
	retry.ClientMsgID = "retry-key"
	b, err := q.Submit(context.Background(), retry)
	if err != nil {
		t.Fatal(err)
	}
	if b.ID == a.ID {
		t.Fatalf("retry returned old failed task %s", a.ID)
	}
	if b.Status != TaskDispatched || b.Attempt != 2 || b.ParentTaskID != a.ID || b.RootTaskID != a.RootTaskID {
		t.Fatalf("retry task = id=%s status=%s attempt=%d parent=%s root=%s", b.ID, b.Status, b.Attempt, b.ParentTaskID, b.RootTaskID)
	}
	if len(ops.dispatched) != 2 || ops.dispatched[1].Text != "第二次（重试）" {
		t.Fatalf("retry not delivered: %+v", ops.dispatched)
	}
	// 新 attempt 用新 task id 作为目标侧去重键，避免被上次投递记录挡住。
	if got := ops.dispatched[1].ClientMsgID; got != string(b.ID) {
		t.Fatalf("retry dispatch client id = %q, want %q", got, b.ID)
	}
	all, err := st.ListTasks(context.Background(), TaskFilter{})
	if err != nil || len(all) != 2 {
		t.Fatalf("ledger tasks = %d, %v; want 2 (old + retry)", len(all), err)
	}
}

// P2-02 复审 B2：跨重启命中 queued 任务必须重新入队，否则返回 queued=true
// 但内存队列里没有它，永远不会被泵出去。
func TestQueueSubmitQueuedHitAfterRestartReenqueues(t *testing.T) {
	dir := t.TempDir()
	st1, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	busy := &queueFakeOps{running: true}
	q1 := NewQueue(NewLedger(st1), busy)
	task := queueTask("重启前排的队")
	task.ClientMsgID = "restart-key"
	first, err := q1.Submit(context.Background(), task)
	if err != nil || first.Status != TaskQueued {
		t.Fatalf("first submit = %+v, %v", first, err)
	}

	// 模拟重启：新 Store/Queue，内存 pending 为空。
	st2, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	idle := &queueFakeOps{}
	q2 := NewQueue(NewLedger(st2), idle)
	again := queueTask("重启后重发")
	again.ClientMsgID = "restart-key"
	got, err := q2.Submit(context.Background(), again)
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != first.ID {
		t.Fatalf("restart hit created a new task: %s vs %s", got.ID, first.ID)
	}
	if got.Status != TaskDispatched {
		t.Fatalf("queued hit status = %s, want dispatched (re-enqueued + pumped)", got.Status)
	}
	if len(idle.dispatched) != 1 {
		t.Fatalf("queued hit not re-enqueued/pumped: dispatches=%d", len(idle.dispatched))
	}
	if q2.QueuedCount("/w", "s1") != 0 {
		t.Fatalf("queue not empty after re-enqueue pump")
	}
}

// P2-02 复审 N1：workspace 尾斜杠必须归一到同一查重键，不能建两条任务
// 而目标侧只投递一条。
func TestQueueSubmitNormalizesWorkspaceForDedup(t *testing.T) {
	ops := &queueFakeOps{running: true}
	q, st := newTestQueue(t, ops)
	mk := func(ws string) *Task {
		t := queueTask("同一会话")
		t.Target.Workspace = ws
		t.ClientMsgID = "same-key"
		return t
	}
	a, err := q.Submit(context.Background(), mk("/w/"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := q.Submit(context.Background(), mk("/w"))
	if err != nil {
		t.Fatal(err)
	}
	if a.ID != b.ID {
		t.Fatalf("workspace spelling created two tasks: %s vs %s", a.ID, b.ID)
	}
	if got := q.QueuedCount("/w", "s1"); got != 1 {
		t.Fatalf("queued = %d, want 1", got)
	}
	all, err := st.ListTasks(context.Background(), TaskFilter{})
	if err != nil || len(all) != 1 {
		t.Fatalf("ledger tasks = %d, %v; want 1", len(all), err)
	}
}

// P2-02 复审 N1：目标侧返回 duplicate 时不能把任务当 dispatched（从未 Ask）。
func TestQueuePumpDuplicateIsNotSilentSuccess(t *testing.T) {
	ops := &queueFakeOps{dispatchErr: nil}
	ops.duplicate = true
	q, _ := newTestQueue(t, ops)
	created, err := q.Submit(context.Background(), queueTask("被判重"))
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != TaskCanceled {
		t.Fatalf("duplicate dispatch status = %s, want canceled", created.Status)
	}
	if created.Error == "" {
		t.Fatalf("duplicate dispatch must leave an error, got %+v", created)
	}
}

// P2-06 复审 B-3（REG-3）：cancel 的 busy 判定必须在目标锁内求值。
// 测试直接占住目标锁模拟 Pump 持锁派发：旧实现会立刻调用 busy 返回；修复后
// 必须等锁释放，才能与 Transition(queued→dispatched)→Dispatch 串行。
func TestQueueCancelDispatchedSerializedWithDispatch(t *testing.T) {
	ops := &queueFakeOps{}
	q, _ := newTestQueue(t, ops)
	created, err := q.Submit(context.Background(), queueTask("串行取消"))
	if err != nil || created.Status != TaskDispatched {
		t.Fatalf("submit = %+v, %v", created, err)
	}
	unlockTarget := q.targets.lock(queueKey("/w", "s1"))
	busyCalled := make(chan struct{})
	done := make(chan struct{})
	var got *Task
	var did bool
	var cancelErr error
	go func() {
		got, did, cancelErr = q.CancelDispatchedIfIdle(context.Background(), created.ID, "竞态取消", func() bool {
			close(busyCalled)
			return false
		})
		close(done)
	}()
	select {
	case <-busyCalled:
		unlockTarget()
		<-done
		t.Fatal("busy 在目标锁外求值：取消可与 Pump 的 Transition→Dispatch 交错")
	case <-time.After(200 * time.Millisecond):
	}
	unlockTarget()
	<-done
	if cancelErr != nil || !did || got == nil || got.Status != TaskCanceled {
		t.Fatalf("cancel = %+v did=%v err=%v; want canceled/true", got, did, cancelErr)
	}
}

// P2-06 复审测试债：CancelQueued 的 Replay/判状态/Transition 必须全程持目标锁。
// 占住目标锁模拟 Pump 派发窗口；旧实现会在锁外直接返回 canceled。
func TestQueueCancelQueuedSerializedWithPump(t *testing.T) {
	ops := &queueFakeOps{running: true}
	q, _ := newTestQueue(t, ops)
	created, err := q.Submit(context.Background(), queueTask("排队中取消"))
	if err != nil || created.Status != TaskQueued {
		t.Fatalf("submit = %+v, %v", created, err)
	}
	unlockTarget := q.targets.lock(queueKey("/w", "s1"))
	done := make(chan struct{})
	var got *Task
	var cancelErr error
	go func() {
		got, cancelErr = q.CancelQueued(context.Background(), created.ID)
		close(done)
	}()
	select {
	case <-done:
		unlockTarget()
		t.Fatal("CancelQueued 在目标锁外返回：Pump 可在窗口内派发")
	case <-time.After(200 * time.Millisecond):
	}
	unlockTarget()
	<-done
	if cancelErr != nil || got == nil || got.Status != TaskCanceled {
		t.Fatalf("cancel = %+v, %v; want canceled", got, cancelErr)
	}
	if n := q.QueuedCount("/w", "s1"); n != 0 {
		t.Fatalf("queued = %d, want 0", n)
	}
}

// P2-06 复审 B-2：取消已派发任务时 busy 在 q.mu 内求值；busy=true 不得 canceled。
func TestQueueCancelDispatchedIfIdleRespectsBusy(t *testing.T) {
	ops := &queueFakeOps{}
	q, _ := newTestQueue(t, ops)
	created, err := q.Submit(context.Background(), queueTask("已派发"))
	if err != nil || created.Status != TaskDispatched {
		t.Fatalf("submit = %+v, %v", created, err)
	}
	got, did, err := q.CancelDispatchedIfIdle(context.Background(), created.ID, "空闲误判", func() bool { return true })
	if err != nil || did || got.Status != TaskDispatched {
		t.Fatalf("busy cancel = status=%s did=%v err=%v; want dispatched/false", got.Status, did, err)
	}
	got, did, err = q.CancelDispatchedIfIdle(context.Background(), created.ID, "确认空闲", func() bool { return false })
	if err != nil || !did || got.Status != TaskCanceled {
		t.Fatalf("idle cancel = %+v did=%v err=%v; want canceled/true", got, did, err)
	}
}

// P2-02 N2：同一任务连续重试到第 8 次，不能因为 Chain 追加 attempt 而触发
// ErrChainTooDeep 永久锁死。
func TestQueueRetryDoesNotConsumeChainDepth(t *testing.T) {
	ops := &queueFakeOps{}
	q, _ := newTestQueue(t, ops)
	guard := &Guard{Limits: GuardLimits{MaxChainSteps: 6, MaxConcurrent: 20}}
	ctx := context.Background()
	first := queueTask("第一次")
	first.ClientMsgID = "retry-deep"
	cur, err := q.SubmitGuarded(ctx, first, guard)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := q.ledger.Transition(ctx, cur.ID, TaskFailed, TransitionOptions{Error: "模拟失败"}); err != nil {
			t.Fatal(err)
		}
		next := queueTask("重试")
		next.ClientMsgID = "retry-deep"
		cur, err = q.SubmitGuarded(ctx, next, guard)
		if err != nil {
			t.Fatalf("retry %d blocked: %v", i+1, err)
		}
	}
	if cur.Attempt != 9 {
		t.Fatalf("attempt = %d, want 9", cur.Attempt)
	}
	if cur.ChainSteps() != 1 {
		t.Fatalf("chain steps = %d, want 1", cur.ChainSteps())
	}
}

// saveCountingStore 统计真正落账的次数：验证 SubmitGuarded 的顺序是
// 查重 → Guard → Create，Guard 失败时不能先建账。
type saveCountingStore struct {
	*MemoryStore
	saves int
}

// P2.5-05 B1：恢复扫描在锁外用旧快照，任务可能已被 session_send 派发/结算。
// Restore 必须在目标锁内重读，只有仍 queued 才入队；否则 completed 会被塞回
// 队首并永久堵死该目标（PROBE-B）。
func TestQueueRestoreRechecksQueuedUnderTargetLock(t *testing.T) {
	ops := &queueFakeOps{}
	q, _ := newTestQueue(t, ops)
	ctx := context.Background()
	task := queueTask("恢复时已派发")
	created, err := q.ledger.Create(ctx, task)
	if err != nil {
		t.Fatal(err)
	}
	stale := *created // 调用方持有的旧快照：Status=queued
	if _, err := q.ledger.Transition(ctx, created.ID, TaskDispatched, TransitionOptions{At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	q.Restore(&stale)
	if n := q.QueuedCount("/w", "s1"); n != 0 {
		t.Fatalf("dispatched task restored into pending: queued=%d", n)
	}
	if _, err := q.ledger.Transition(ctx, created.ID, TaskCompleted, TransitionOptions{}); err != nil {
		t.Fatal(err)
	}
	stale2 := *created
	stale2.Status = TaskQueued
	q.Restore(&stale2)
	if n := q.QueuedCount("/w", "s1"); n != 0 {
		t.Fatalf("completed task restored into pending: queued=%d", n)
	}
}

// P2.5-07 N1：进程重启留下的 interrupted 任务，同 client_msg_id 重投
// 必须新建 attempt，不能永远返回同一 interrupted 快照。
func TestQueueInterruptedRetryStartsNewAttempt(t *testing.T) {
	ops := &queueFakeOps{}
	q, _ := newTestQueue(t, ops)
	ctx := context.Background()
	first := queueTask("重启后重投")
	first.ClientMsgID = "interrupted-retry"
	created, err := q.Submit(ctx, first)
	if err != nil || created.Status != TaskDispatched {
		t.Fatalf("first submit = %+v, %v", created, err)
	}
	if _, err := q.ledger.Transition(ctx, created.ID, TaskInterrupted, TransitionOptions{Error: "进程重启：没有运行记录"}); err != nil {
		t.Fatal(err)
	}
	again := queueTask("重启后重投")
	again.ClientMsgID = "interrupted-retry"
	retried, err := q.Submit(ctx, again)
	if err != nil {
		t.Fatal(err)
	}
	if retried.ID == created.ID || retried.Attempt != 2 || retried.Status != TaskDispatched {
		t.Fatalf("interrupted retry = %+v; want new task attempt 2 dispatched", retried)
	}
}

func (s *saveCountingStore) SaveTask(ctx context.Context, t *Task) error {
	s.saves++
	return s.MemoryStore.SaveTask(ctx, t)
}

// P2.5-07：SubmitGuarded 的顺序回归。guard 拒绝时账本不能先写。
func TestSubmitGuardedChecksGuardBeforeCreate(t *testing.T) {
	ops := &queueFakeOps{}
	st := &saveCountingStore{MemoryStore: NewMemoryStore()}
	q := NewQueue(NewLedger(st), ops)
	task := queueTask("会被链长拒绝")
	task.ID = "task_self"
	task.RootTaskID = "task_root"
	task.Chain = []TaskID{"task_root", "task_self"} // 账本合法且 ChainSteps=2 > 1
	guard := &Guard{Limits: GuardLimits{MaxChainSteps: 1, MaxConcurrent: 10}}

	if _, err := q.SubmitGuarded(context.Background(), task, guard); !errors.Is(err, ErrChainTooDeep) {
		t.Fatalf("SubmitGuarded err = %v, want ErrChainTooDeep", err)
	}
	if st.saves != 0 {
		t.Fatalf("guard rejected but SaveTask called %d times", st.saves)
	}
	if len(ops.dispatched) != 0 {
		t.Fatalf("guard rejected but dispatched %d", len(ops.dispatched))
	}
}

// P2.5-07：同 client_msg_id 命中已有任务时，查重必须排在 Guard 之前。
func TestSubmitGuardedReturnsExistingBeforeGuard(t *testing.T) {
	ops := &queueFakeOps{}
	st := &saveCountingStore{MemoryStore: NewMemoryStore()}
	q := NewQueue(NewLedger(st), ops)
	first := queueTask("原有任务")
	first.ClientMsgID = "same-key"
	created, err := q.Submit(context.Background(), first)
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}
	before := st.saves
	guard := &Guard{Limits: GuardLimits{MaxChainSteps: 1, MaxConcurrent: 10}}
	second := queueTask("重复投递")
	second.ClientMsgID = "same-key"
	second.ID = "task_self_dup"
	second.RootTaskID = "task_root_dup"
	second.Chain = []TaskID{"task_root_dup", "task_self_dup"} // 合法且 ChainSteps=2 > 1
	got, err := q.SubmitGuarded(context.Background(), second, guard)
	if err != nil {
		t.Fatalf("duplicate submit should return existing, got %v", err)
	}
	if got.ID != created.ID {
		t.Fatalf("duplicate returned %s, want %s", got.ID, created.ID)
	}
	if st.saves != before {
		t.Fatalf("duplicate submit wrote ledger: saves %d -> %d", before, st.saves)
	}
}

// P2.5-06 复审 N1：出队后 Dispatch 被 ctx 取消，收尾仍必须落 failed，
// 不能把任务永久留在 dispatched（只有重启恢复才能收）。
func TestQueueCancelAfterDequeueSettlesFailed(t *testing.T) {
	ops := &queueFakeOps{dispatchWait: true, dispatchStarted: make(chan struct{}, 1)}
	q, _ := newTestQueue(t, ops)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	task := queueTask("会被取消")
	task.ClientMsgID = "cancel-after-dequeue"
	done := make(chan error, 1)
	go func() { _, err := q.Submit(ctx, task); done <- err }()
	select {
	case <-ops.dispatchStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("Submit 未在 2s 内进入 Dispatch")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Submit did not return after cancel")
	}
	got, err := NewLedger(q.ledger.store).FindByClientMsgID(context.Background(), task.Target, task.ClientMsgID)
	if err != nil || got == nil {
		t.Fatalf("find task = %v, %v", got, err)
	}
	if got.Status != TaskFailed {
		t.Fatalf("status after cancel = %s, want failed", got.Status)
	}
}

// concurrentQueueOps：SessionStatus 恒空闲，Dispatch 可按 session 阻塞，
// 用来证明不同目标互不阻塞、同一目标严格串行（P2.5-03）。
type concurrentQueueOps struct {
	started  chan string
	mu       sync.Mutex
	releases map[string]chan struct{}
	calls    map[string]int
}

func (o *concurrentQueueOps) ListWorkspaces(context.Context) ([]WorkspaceInfo, error) {
	return nil, nil
}
func (o *concurrentQueueOps) CreateWorkspace(context.Context, CreateWorkspaceReq) (WorkspaceInfo, error) {
	return WorkspaceInfo{}, nil
}
func (o *concurrentQueueOps) ListSessions(context.Context, string) ([]SessionInfo, error) {
	return nil, nil
}
func (o *concurrentQueueOps) SessionStatus(_ context.Context, ws, sid string) (SessionInfo, error) {
	return SessionInfo{ID: sid, Workspace: ws}, nil
}
func (o *concurrentQueueOps) CreateSession(context.Context, CreateSessionReq) (SessionInfo, error) {
	return SessionInfo{}, nil
}
func (o *concurrentQueueOps) Dispatch(ctx context.Context, req DispatchReq) (DispatchResult, error) {
	o.started <- req.SessionID
	if ch := o.releases[req.SessionID]; ch != nil {
		select {
		case <-ch:
		case <-ctx.Done():
			return DispatchResult{}, ctx.Err()
		}
	}
	o.mu.Lock()
	o.calls[req.SessionID]++
	o.mu.Unlock()
	return DispatchResult{SessionID: req.SessionID, Accepted: true}, nil
}
func (o *concurrentQueueOps) InterruptSession(context.Context, string) error { return nil }

func (o *concurrentQueueOps) callCount(sid string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.calls[sid]
}

func TestQueueDifferentTargetsDoNotBlockEachOther(t *testing.T) {
	ops := &concurrentQueueOps{
		started:  make(chan string, 8),
		releases: map[string]chan struct{}{"s1": make(chan struct{})},
		calls:    map[string]int{},
	}
	q, _ := newTestQueue(t, ops)
	first := queueTask("慢会话")
	firstDone := make(chan error, 1)
	go func() { _, err := q.Submit(context.Background(), first); firstDone <- err }()
	select {
	case sid := <-ops.started:
		if sid != "s1" {
			t.Fatalf("first dispatch sid = %q", sid)
		}
	case <-time.After(time.Second):
		t.Fatal("first target never dispatched")
	}

	other := queueTask("另一个会话")
	other.Target.SessionID = "s2"
	otherDone := make(chan error, 1)
	go func() { _, err := q.Submit(context.Background(), other); otherDone <- err }()
	select {
	case sid := <-ops.started:
		if sid != "s2" {
			t.Fatalf("second dispatch sid = %q, want s2", sid)
		}
	case <-time.After(time.Second):
		t.Fatal("s1 的 Dispatch 阻塞了 s2：目标锁仍互相影响")
	}
	close(ops.releases["s1"])
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	if err := <-otherDone; err != nil {
		t.Fatal(err)
	}
}

func TestQueueSameTargetSerializesDispatch(t *testing.T) {
	ops := &concurrentQueueOps{
		started:  make(chan string, 8),
		releases: map[string]chan struct{}{"s1": make(chan struct{})},
		calls:    map[string]int{},
	}
	q, _ := newTestQueue(t, ops)
	firstDone := make(chan error, 1)
	go func() { _, err := q.Submit(context.Background(), queueTask("一")); firstDone <- err }()
	select {
	case <-ops.started: // 第一个 Dispatch 已进入并阻塞
	case <-time.After(10 * time.Second):
		t.Fatal("第一个 Submit 未进入 Dispatch")
	}
	secondDone := make(chan error, 1)
	go func() { _, err := q.Submit(context.Background(), queueTask("二")); secondDone <- err }()
	select {
	case sid := <-ops.started:
		t.Fatalf("同目标第二个 Dispatch 在第一个完成前开始（sid=%s）", sid)
	case <-time.After(200 * time.Millisecond):
	}
	close(ops.releases["s1"])
	if err := <-firstDone; err != nil {
		t.Fatal(err)
	}
	// Submit 返回时 Pump 已走完：等 secondDone 是确定性信号，不用挂钟阈值
	// （fsync/负载下 1s 会把正确实现误判为失败，P2.5-03 复审 B1）。
	select {
	case err := <-secondDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("第二个 Submit 10s 内未返回")
	}
	if n := ops.callCount("s1"); n != 2 {
		t.Fatalf("dispatch calls = %d, want 2", n)
	}
}

func TestQueueConcurrentSameClientMsgIDCreatesOneTask(t *testing.T) {
	ops := &concurrentQueueOps{started: make(chan string, 16), calls: map[string]int{}}
	q, st := newTestQueue(t, ops)
	const n = 8
	ids := make([]TaskID, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			task := queueTask("同 id 并发")
			task.ClientMsgID = "same-concurrent"
			created, err := q.Submit(context.Background(), task)
			if err != nil {
				errs[i] = err
				return
			}
			ids[i] = created.ID
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("submit %d: %v", i, err)
		}
	}
	for i := 1; i < n; i++ {
		if ids[i] != ids[0] {
			t.Fatalf("concurrent same client_msg_id created %s vs %s", ids[0], ids[i])
		}
	}
	all, err := st.ListTasks(context.Background(), TaskFilter{})
	if err != nil || len(all) != 1 {
		t.Fatalf("ledger tasks = %d, %v; want 1", len(all), err)
	}
	if got := ops.callCount("s1"); got != 1 {
		t.Fatalf("dispatch calls = %d, want 1", got)
	}
}

// P2.5-03 核心断言：Pump 在 Dispatch 期间只持目标锁，不持索引锁 q.mu。
// 旧实现全程 q.mu，其他会话的入队/计数会被一个慢投递拖住。
func TestQueuePumpDoesNotHoldIndexLockDuringDispatch(t *testing.T) {
	ops := &concurrentQueueOps{
		started:  make(chan string, 4),
		releases: map[string]chan struct{}{"s1": make(chan struct{})},
		calls:    map[string]int{},
	}
	q, _ := newTestQueue(t, ops)
	task := queueTask("慢派发")
	created, err := q.ledger.Create(context.Background(), task)
	if err != nil {
		t.Fatal(err)
	}
	q.Restore(created)
	pumpDone := make(chan error, 1)
	go func() { _, err := q.Pump(context.Background(), task.Target); pumpDone <- err }()
	select {
	case <-ops.started:
	case <-time.After(time.Second):
		t.Fatal("Pump 未进入 Dispatch")
	}
	locked := make(chan struct{})
	go func() {
		q.mu.Lock()
		q.mu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Dispatch 期间仍持有 q.mu：慢会话会阻塞其他目标的入队/计数")
	}
	close(ops.releases["s1"])
	if err := <-pumpDone; err != nil {
		t.Fatal(err)
	}
}

// P2.5-03 N1：Submit 在账本 I/O 期间只能持目标锁，不能持 q.mu。
// 用一个在 SaveTask 里阻塞的 store 做锚点：旧实现（全程 q.mu）下这里取 q.mu 会超时。
type blockingCreateStore struct {
	*MemoryStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *blockingCreateStore) SaveTask(ctx context.Context, t *Task) error {
	s.once.Do(func() {
		close(s.entered)
		<-s.release
	})
	return s.MemoryStore.SaveTask(ctx, t)
}

func TestQueueSubmitDoesNotHoldIndexLockDuringLedgerIO(t *testing.T) {
	store := &blockingCreateStore{
		MemoryStore: NewMemoryStore(),
		entered:     make(chan struct{}),
		release:     make(chan struct{}),
	}
	ops := &queueFakeOps{}
	q := NewQueue(NewLedger(store), ops)
	done := make(chan error, 1)
	go func() { _, err := q.Submit(context.Background(), queueTask("账本阻塞")); done <- err }()
	select {
	case <-store.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("Submit 未进入 SaveTask")
	}
	locked := make(chan struct{})
	go func() {
		q.mu.Lock()
		q.mu.Unlock()
		close(locked)
	}()
	select {
	case <-locked:
	case <-time.After(500 * time.Millisecond):
		close(store.release)
		<-done
		t.Fatal("Submit 在账本 I/O 期间持有 q.mu")
	}
	close(store.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

// P2.5-03 N2：keyedLocks 的引用计数归零后必须回收，避免一次性 key 无限增长。
func TestKeyedLocksReclaimAfterUnlock(t *testing.T) {
	var k keyedLocks
	u := k.lock("a")
	k.mu.Lock()
	n := len(k.m)
	k.mu.Unlock()
	if n != 1 {
		t.Fatalf("locks while held = %d, want 1", n)
	}
	u()
	k.mu.Lock()
	n = len(k.m)
	k.mu.Unlock()
	if n != 0 {
		t.Fatalf("locks after release = %d, want 0", n)
	}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				unlock := k.lock(string(rune('a' + (i+j)%4)))
				unlock()
			}
		}(i)
	}
	wg.Wait()
	k.mu.Lock()
	n = len(k.m)
	k.mu.Unlock()
	if n != 0 {
		t.Fatalf("locks after concurrent release = %d, want 0", n)
	}

	// 回收不能破坏互斥：若 unlock 时无条件 delete，等待者会拿到已摘掉的
	// 旧锁对象，两个 goroutine 可同时进入临界区（N-2 变异）。
	var inside atomic.Int32
	var violations atomic.Int32
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 100; j++ {
				unlock := k.lock("same")
				if inside.Add(1) != 1 {
					violations.Add(1)
				}
				time.Sleep(50 * time.Microsecond)
				inside.Add(-1)
				unlock()
			}
		}()
	}
	wg.Wait()
	if violations.Load() != 0 {
		t.Fatalf("mutual exclusion violations = %d", violations.Load())
	}
}

// P2.5-03 验收：多目标并发提交的基准（-bench 看吞吐；不参与普通单测）。
type benchQueueOps struct{}

func (benchQueueOps) ListWorkspaces(context.Context) ([]WorkspaceInfo, error) { return nil, nil }
func (benchQueueOps) CreateWorkspace(context.Context, CreateWorkspaceReq) (WorkspaceInfo, error) {
	return WorkspaceInfo{}, nil
}
func (benchQueueOps) ListSessions(context.Context, string) ([]SessionInfo, error) { return nil, nil }
func (benchQueueOps) SessionStatus(_ context.Context, ws, sid string) (SessionInfo, error) {
	return SessionInfo{ID: sid, Workspace: ws}, nil
}
func (benchQueueOps) CreateSession(context.Context, CreateSessionReq) (SessionInfo, error) {
	return SessionInfo{}, nil
}
func (benchQueueOps) Dispatch(_ context.Context, req DispatchReq) (DispatchResult, error) {
	return DispatchResult{SessionID: req.SessionID, Accepted: true}, nil
}
func (benchQueueOps) InterruptSession(context.Context, string) error { return nil }

func BenchmarkQueueMultiTargetSubmit(b *testing.B) {
	q := NewQueue(NewLedger(NewMemoryStore()), benchQueueOps{})
	var seq atomic.Int64
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := seq.Add(1)
			task := queueTask("bench")
			task.Target.Workspace = "/w"
			task.Target.SessionID = "s" + strconv.FormatInt(n%8, 10)
			if _, err := q.Submit(context.Background(), task); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// P2.5-03 N-1：队首在账本里已被旁路改成终态（恢复/取消窗口）时，Pump 必须
// 摘掉陈旧条目让后续任务继续，不能每次 Transition 失败都把目标永久堵死。
func TestQueuePumpDropsStaleTerminalHead(t *testing.T) {
	ops := &queueFakeOps{}
	q, _ := newTestQueue(t, ops)
	ctx := context.Background()
	stale, err := q.ledger.Create(ctx, queueTask("已取消的陈旧队首"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.ledger.Transition(ctx, stale.ID, TaskCanceled, TransitionOptions{At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	stale2, err := q.ledger.Create(ctx, queueTask("另一个陈旧队首"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := q.ledger.Transition(ctx, stale2.ID, TaskCanceled, TransitionOptions{At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	next, err := q.ledger.Create(ctx, queueTask("应继续派发"))
	if err != nil {
		t.Fatal(err)
	}
	key := queueKey("/w", "s1")
	unlock := q.targets.lock(key)
	q.enqueueLocked(key, stale.ID)
	q.enqueueLocked(key, stale2.ID)
	q.enqueueLocked(key, next.ID)
	unlock()
	// 同一次 Pump 应越过陈旧队首并派发后面的真实任务，而不是把新任务
	// 留成「已排队」等下一次触发（P2.5-03 N）。
	dispatched, err := q.Pump(ctx, queueTask("x").Target)
	if err != nil || dispatched == nil || dispatched.ID != next.ID {
		t.Fatalf("pump = %+v, %v; want %s", dispatched, err, next.ID)
	}
	if n := q.QueuedCount("/w", "s1"); n != 0 {
		t.Fatalf("queued after pump = %d, want 0", n)
	}
}
