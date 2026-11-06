package agenthub

// P2-03：任务账本 + 空闲闸门 + 队列。

import (
	"context"
	"errors"
	"testing"
	"time"
)

type queueFakeOps struct {
	running     bool
	pending     int
	dispatched  []DispatchReq
	dispatchErr error
	statusErr   error
	duplicate   bool
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

func (f *queueFakeOps) Dispatch(_ context.Context, req DispatchReq) (DispatchResult, error) {
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

// P2-06 复审 B-3（REG-3）：cancel 的 busy 判定必须在 q.mu 内求值。
// 测试直接占住 q.mu 模拟 Pump 持锁派发：旧实现会立刻调用 busy 返回；修复后
// 必须等锁释放，才能与 Transition(queued→dispatched)→Dispatch 串行。
func TestQueueCancelDispatchedSerializedWithDispatch(t *testing.T) {
	ops := &queueFakeOps{}
	q, _ := newTestQueue(t, ops)
	created, err := q.Submit(context.Background(), queueTask("串行取消"))
	if err != nil || created.Status != TaskDispatched {
		t.Fatalf("submit = %+v, %v", created, err)
	}
	q.mu.Lock()
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
		q.mu.Unlock()
		<-done
		t.Fatal("busy 在 q.mu 外求值：取消可与 Pump 的 Transition→Dispatch 交错")
	case <-time.After(200 * time.Millisecond):
	}
	q.mu.Unlock()
	<-done
	if cancelErr != nil || !did || got == nil || got.Status != TaskCanceled {
		t.Fatalf("cancel = %+v did=%v err=%v; want canceled/true", got, did, cancelErr)
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
