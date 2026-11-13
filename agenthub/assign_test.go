package agenthub

// P3b-03：assign_task 的账本路径（kind=agent 目标 + 同对在途上限）。

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// assignOps 在 queueFakeOps（工作区能力）之上补 AgentDispatchOps。
type assignOps struct {
	*queueFakeOps

	mu         sync.Mutex
	agentSent  []AgentDispatchReq
	agentBusy  bool
	agentErr   error
	agentDup   bool
	agentCalls int
}

func newAssignOps() *assignOps {
	return &assignOps{queueFakeOps: &queueFakeOps{}}
}

func (o *assignOps) AgentDispatch(_ context.Context, req AgentDispatchReq) (AgentDispatchResult, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.agentCalls++
	if o.agentErr != nil {
		return AgentDispatchResult{}, o.agentErr
	}
	if o.agentBusy {
		return AgentDispatchResult{SessionID: req.Target.SessionID, Accepted: false}, nil
	}
	o.agentSent = append(o.agentSent, req)
	msg := ""
	if o.agentDup {
		msg = DispatchDuplicateMessage
	}
	return AgentDispatchResult{SessionID: req.Target.SessionID, Accepted: true, Message: msg}, nil
}

func (o *assignOps) sent() []AgentDispatchReq {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]AgentDispatchReq(nil), o.agentSent...)
}

func newAssignQueue(t *testing.T) (*Queue, *MemoryStore, *assignOps) {
	t.Helper()
	st := NewMemoryStore()
	ops := newAssignOps()
	return NewQueue(NewLedger(st), ops), st, ops
}

func assignTask(from, to AgentID, text string) *Task {
	return &Task{
		Origin:      TaskOrigin{Kind: OriginGroup, GroupID: "grp_assign", AgentID: from},
		Target:      TaskTarget{Kind: TargetAgent, AgentID: to, GroupID: "grp_assign", SessionID: "hub." + string(to) + ".g.grp_assign"},
		Title:       "测试指派",
		Instruction: text,
		Callback:    CallbackAgentEnd,
	}
}

func TestQueueAssignAgentTargetQueuedThenDispatched(t *testing.T) {
	q, st, ops := newAssignQueue(t)
	ctx := context.Background()
	created, err := q.SubmitAgent(ctx, assignTask("ag_a", "ag_b", "写一份对比"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if created.Status != TaskQueued {
		t.Fatalf("SubmitAgent 不应自己派发：status = %q", created.Status)
	}
	if created.Target.Kind != TargetAgent || created.Target.AgentID != "ag_b" ||
		created.Target.GroupID != "grp_assign" || created.Target.SessionID != "hub.ag_b.g.grp_assign" {
		t.Fatalf("target = %+v", created.Target)
	}
	if created.Callback != CallbackAgentEnd || created.RootTaskID != created.ID || len(created.Chain) != 1 {
		t.Fatalf("账本字段不全: %+v", created)
	}
	if q.QueuedCountFor(created.Target) != 1 {
		t.Fatalf("queued count = %d, want 1", q.QueuedCountFor(created.Target))
	}
	if len(ops.sent()) != 0 {
		t.Fatal("SubmitAgent 不应投递")
	}

	bound, err := NewLedger(st).BindOriginMessageSeq(ctx, created.ID, 7)
	if err != nil {
		t.Fatal(err)
	}
	if bound.Origin.MessageSeq != 7 {
		t.Fatalf("message seq = %d", bound.Origin.MessageSeq)
	}
	// Replay 也必须能看到 seq（事件流而不是只有快照文件）。
	replayed, err := NewLedger(st).Replay(ctx, created.ID)
	if err != nil || replayed.Origin.MessageSeq != 7 {
		t.Fatalf("replay seq = %+v, %v", replayed, err)
	}

	dispatched, err := q.Pump(ctx, created.Target)
	if err != nil {
		t.Fatal(err)
	}
	if dispatched == nil || dispatched.ID != created.ID || dispatched.Status != TaskDispatched {
		t.Fatalf("pump = %+v", dispatched)
	}
	sent := ops.sent()
	if len(sent) != 1 {
		t.Fatalf("sent = %d, want 1", len(sent))
	}
	if sent[0].Text != "写一份对比" || sent[0].OriginAgent != "ag_a" || sent[0].ClientMsgID != string(created.ID) {
		t.Fatalf("dispatch req = %+v", sent[0])
	}
	h, err := ParseTaskHeader(sent[0].TaskHeader)
	if err != nil || h.TaskID != created.ID {
		t.Fatalf("header = %+v, %v", h, err)
	}
	if q.QueuedCountFor(created.Target) != 0 {
		t.Fatal("dispatched task must leave the pending queue")
	}
}

func TestQueueAssignBusyKeepsQueued(t *testing.T) {
	q, st, ops := newAssignQueue(t)
	ctx := context.Background()
	ops.agentBusy = true
	created, err := q.SubmitAgent(ctx, assignTask("ag_a", "ag_b", "忙时排队"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := q.Pump(ctx, created.Target)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || got.Status != TaskQueued {
		t.Fatalf("busy pump = %+v", got)
	}
	if len(ops.sent()) != 0 {
		t.Fatal("busy target must not receive a snapshot")
	}
	replayed, err := NewLedger(st).Replay(ctx, created.ID)
	if err != nil || replayed.Status != TaskQueued {
		t.Fatalf("ledger = %+v, %v", replayed, err)
	}
	if q.QueuedCountFor(created.Target) != 1 {
		t.Fatal("busy task must stay in the pending queue")
	}
}

func TestQueueAssignPairLimit(t *testing.T) {
	q, st, ops := newAssignQueue(t)
	ctx := context.Background()
	ops.agentBusy = true // 都不派发，稳定留在非终态
	first, err := q.SubmitAgent(ctx, assignTask("ag_a", "ag_b", "第一件"), nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := q.SubmitAgent(ctx, assignTask("ag_a", "ag_b", "第二件"), nil)
	if err != nil {
		t.Fatalf("第二个在途任务应允许: %v", err)
	}
	_, err = q.SubmitAgent(ctx, assignTask("ag_a", "ag_b", "第三件"), nil)
	if !errors.Is(err, ErrTooConcurrent) {
		t.Fatalf("第三个 = %v, want ErrTooConcurrent", err)
	}
	all, err := st.ListTasks(ctx, TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 {
		t.Fatalf("tasks = %d, want 2", len(all))
	}
	if q.QueuedCountFor(first.Target) != 2 {
		t.Fatalf("queued = %d", q.QueuedCountFor(first.Target))
	}

	// 换一对 A→C / C→B 不受影响。
	if _, err := q.SubmitAgent(ctx, assignTask("ag_a", "ag_c", "给 C"), nil); err != nil {
		t.Fatalf("A→C: %v", err)
	}
	if _, err := q.SubmitAgent(ctx, assignTask("ag_c", "ag_b", "C 给 B"), nil); err != nil {
		t.Fatalf("C→B: %v", err)
	}

	// 第一个任务终态后空出名额。
	if _, err := NewLedger(st).Transition(ctx, first.ID, TaskCanceled, TransitionOptions{At: time.Now().UTC(), Error: "测试取消"}); err != nil {
		t.Fatal(err)
	}
	if _, err := q.SubmitAgent(ctx, assignTask("ag_a", "ag_b", "补一个"), nil); err != nil {
		t.Fatalf("终态后应允许: %v", err)
	}
	if _, err := q.SubmitAgent(ctx, assignTask("ag_a", "ag_b", "再补一个"), nil); !errors.Is(err, ErrTooConcurrent) {
		t.Fatalf("still full = %v, want ErrTooConcurrent", err)
	}
	_ = second
}

func TestQueueAssignPairLimitConcurrent(t *testing.T) {
	q, _, ops := newAssignQueue(t)
	ops.agentBusy = true
	const n = 8
	var wg sync.WaitGroup
	results := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, results[i] = q.SubmitAgent(context.Background(), assignTask("ag_a", "ag_b", fmt.Sprintf("并发 %d", i)), nil)
		}(i)
	}
	wg.Wait()
	ok, too := 0, 0
	for _, err := range results {
		switch {
		case err == nil:
			ok++
		case errors.Is(err, ErrTooConcurrent):
			too++
		default:
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if ok != MaxAgentPairInFlight || too != n-MaxAgentPairInFlight {
		t.Fatalf("ok=%d too=%d, want %d/%d", ok, too, MaxAgentPairInFlight, n-MaxAgentPairInFlight)
	}
}

func TestQueueAssignValidation(t *testing.T) {
	q, st, _ := newAssignQueue(t)
	ctx := context.Background()
	bad := []*Task{
		assignTask("ag_a", "ag_a", "派给自己"),
		func() *Task { x := assignTask("ag_a", "ag_b", "跨群"); x.Target.GroupID = "grp_other"; return x }(),
		func() *Task { x := assignTask("ag_a", "ag_b", "没有 session"); x.Target.SessionID = " "; return x }(),
		func() *Task { x := assignTask("ag_a", "", "没有目标"); return x }(),
		func() *Task { x := assignTask("ag_a", "ag_b", "DM 来的"); x.Origin.Kind = OriginDM; return x }(),
		assignTask("ag_a", "ag_b", "   "),
	}
	for i, task := range bad {
		if _, err := q.SubmitAgent(ctx, task, nil); !errors.Is(err, ErrInvalid) {
			t.Errorf("case %d = %v, want ErrInvalid", i, err)
		}
	}
	all, err := st.ListTasks(ctx, TaskFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 0 {
		t.Fatalf("validation failures left %d tasks", len(all))
	}
}

// 同 client_msg_id 的快速重试命中旧任务；旧任务终态后可重投成新 attempt，
// 且不把已终态的旧 attempt 算进在途上限。
func TestQueueAssignRetryAfterTerminal(t *testing.T) {
	q, st, _ := newAssignQueue(t)
	ctx := context.Background()
	base := assignTask("ag_a", "ag_b", "重试我")
	base.ClientMsgID = "cid-1"
	first, err := q.SubmitAgent(ctx, base, nil)
	if err != nil {
		t.Fatal(err)
	}
	again := assignTask("ag_a", "ag_b", "重试我")
	again.ClientMsgID = "cid-1"
	dup, err := q.SubmitAgent(ctx, again, nil)
	if err != nil {
		t.Fatal(err)
	}
	if dup.ID != first.ID {
		t.Fatalf("同 client_msg_id 建了两条任务: %s vs %s", dup.ID, first.ID)
	}
	if _, err := NewLedger(st).Transition(ctx, first.ID, TaskCanceled, TransitionOptions{At: time.Now().UTC(), Error: "测试取消"}); err != nil {
		t.Fatal(err)
	}
	retry := assignTask("ag_a", "ag_b", "重试我")
	retry.ClientMsgID = "cid-1"
	next, err := q.SubmitAgent(ctx, retry, nil)
	if err != nil {
		t.Fatal(err)
	}
	if next.ID == first.ID || next.ParentTaskID != first.ID || next.Attempt != 2 {
		t.Fatalf("retry = %+v (first %s)", next, first.ID)
	}
	if len(next.Chain) != 2 || next.Chain[0] != first.ID || next.Chain[1] != next.ID {
		t.Fatalf("retry chain = %v", next.Chain)
	}
}

func TestTaskValidateAgentTarget(t *testing.T) {
	base := func() *Task {
		return &Task{
			ID: "task_agent", RootTaskID: "task_agent", Chain: []TaskID{"task_agent"},
			Origin: TaskOrigin{Kind: OriginGroup, GroupID: "grp_assign", AgentID: "ag_a"},
			Target: TaskTarget{Kind: TargetAgent, AgentID: "ag_b", GroupID: "grp_assign"},
			Title:  "t",
			Status: TaskQueued,
		}
	}
	if err := base().Validate(); err != nil {
		t.Fatalf("valid agent target: %v", err)
	}
	noAgent := base()
	noAgent.Target.AgentID = ""
	if err := noAgent.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing agent_id = %v", err)
	}
	noGroup := base()
	noGroup.Target.GroupID = ""
	if err := noGroup.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("missing group_id = %v", err)
	}
}

// P3b-03 R3：CancelDispatchedIfIdle 必须与 pumpAgent 锁**同一个** key（queueKeyFor）。
// 旧实现用 queueKey(workspace, sessionID)：agent 目标的 workspace 为空，key 与泵的
// (group, agent) 不同 —— 取消不与「Transition(dispatched) → AgentDispatch」串行，
// 会在 Ask 之后的窗口里把任务当空闲取消（变异：改回 workspace+sid key → 本测试红）。
func TestQueueCancelDispatchedAgentSerializedWithPump(t *testing.T) {
	q, _, _ := newAssignQueue(t)
	ctx := context.Background()
	created, err := q.SubmitAgent(ctx, assignTask("ag_a", "ag_b", "派发窗口取消"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if pumped, err := q.Pump(ctx, created.Target); err != nil || pumped == nil || pumped.Status != TaskDispatched {
		t.Fatalf("pump = %+v, %v; want dispatched", pumped, err)
	}
	// 占住 pumpAgent 用的那把键，模拟「泵正在 Transition→AgentDispatch 之间」。
	unlockTarget := q.targets.lock(queueKeyFor(created.Target))
	busyCalled := make(chan struct{})
	done := make(chan struct{})
	var (
		got       *Task
		did       bool
		cancelErr error
	)
	go func() {
		got, did, cancelErr = q.CancelDispatchedIfIdle(ctx, created.ID, "竞态取消", func() bool {
			close(busyCalled)
			return false
		})
		close(done)
	}()
	select {
	case <-busyCalled:
		unlockTarget()
		<-done
		t.Fatal("busy 在 agent 目标锁外求值：取消可与 pumpAgent 的 Ask→Transition 交错")
	case <-time.After(200 * time.Millisecond):
	}
	unlockTarget()
	<-done
	if cancelErr != nil || !did || got == nil || got.Status != TaskCanceled {
		t.Fatalf("cancel = %+v did=%v err=%v; want canceled/true", got, did, cancelErr)
	}
}

// P3b-03 N1：FileStore/MemoryStore 都必须实现 PairActiveIndex；同对在途上限走内存
// 索引（回退 ListTasks 会让每次 assign 全表扫一遍任务目录）。
func TestQueueAssignPairLimitUsesIndex(t *testing.T) {
	st := &countingListStore{MemoryStore: NewMemoryStore()}
	var idx PairActiveIndex = st
	if !idx.TaskIndexReady() {
		t.Fatal("MemoryStore 未实现 PairActiveIndex（Queue 会回退 ListTasks 全表扫描）")
	}
	ops := newAssignOps()
	ops.agentBusy = true // 都不派发，稳定留在非终态
	q := NewQueue(NewLedger(st), ops)
	ctx := context.Background()
	for _, text := range []string{"第一件", "第二件"} {
		if _, err := q.SubmitAgent(ctx, assignTask("ag_a", "ag_b", text), nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := q.SubmitAgent(ctx, assignTask("ag_a", "ag_c", "另一对"), nil); err != nil {
		t.Fatal(err)
	}
	if n := idx.PairActiveCount("grp_assign", "ag_a", "ag_b"); n != 2 {
		t.Fatalf("A→B pair count = %d, want 2", n)
	}
	if n := idx.PairActiveCount("grp_assign", "ag_a", "ag_c"); n != 1 {
		t.Fatalf("A→C pair count = %d, want 1", n)
	}
	// from:to 口径：同目标、不同发起者不算同一对。
	if n := idx.PairActiveCount("grp_assign", "ag_c", "ag_b"); n != 0 {
		t.Fatalf("C→B pair count = %d, want 0", n)
	}
	if _, err := q.SubmitAgent(ctx, assignTask("ag_a", "ag_b", "第三件"), nil); !errors.Is(err, ErrTooConcurrent) {
		t.Fatalf("第三件 = %v, want ErrTooConcurrent", err)
	}
	if st.listCalls != 0 {
		t.Fatalf("同对上限走了 ListTasks 全表扫描 %d 次（应走 TaskIndex）", st.listCalls)
	}
}

// FileStore 同口径：增量维护 + 重启重建索引后计数一致（否则上限会在重启后失效）。
func TestFileStorePairActiveCount(t *testing.T) {
	root := t.TempDir()
	st, err := OpenFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	var idx PairActiveIndex = st
	if !idx.TaskIndexReady() {
		t.Fatal("FileStore 未实现 PairActiveIndex")
	}
	ops := newAssignOps()
	ops.agentBusy = true
	q := NewQueue(NewLedger(st), ops)
	ctx := context.Background()
	for _, text := range []string{"一", "二"} {
		if _, err := q.SubmitAgent(ctx, assignTask("ag_a", "ag_b", text), nil); err != nil {
			t.Fatal(err)
		}
	}
	if n := st.PairActiveCount("grp_assign", "ag_a", "ag_b"); n != 2 {
		t.Fatalf("pair count = %d, want 2", n)
	}
	reopened, err := OpenFileStore(root)
	if err != nil {
		t.Fatal(err)
	}
	if n := reopened.PairActiveCount("grp_assign", "ag_a", "ag_b"); n != 2 {
		t.Fatalf("重建索引后 pair count = %d, want 2", n)
	}
}

// P3b-03 N2：永久性派发失败（目标群/成员已不存在）必须落 canceled 并出队，
// 不能留一条永远不会成功的队首把该目标的后续任务全堵死。
func TestQueueAssignPermanentDispatchErrorCancelsHead(t *testing.T) {
	q, st, ops := newAssignQueue(t)
	ctx := context.Background()
	ops.agentErr = fmt.Errorf("%w: 群组 grp_assign 不存在", ErrNotFound)
	first, err := q.SubmitAgent(ctx, assignTask("ag_a", "ag_b", "目标已删"), nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := q.Pump(ctx, first.Target)
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("pump err = %v, want ErrNotFound（失败要如实回给调用方）", err)
	}
	if got == nil || got.Status != TaskCanceled {
		t.Fatalf("pump = %+v, want canceled", got)
	}
	if q.QueuedCountFor(first.Target) != 0 {
		t.Fatalf("永久失败的队首没有出队：queued = %d", q.QueuedCountFor(first.Target))
	}
	// 后续任务不被堵死：错误消失后同目标照常派发。
	ops.agentErr = nil
	second, err := q.SubmitAgent(ctx, assignTask("ag_a", "ag_b", "补一件"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if pumped, err := q.Pump(ctx, second.Target); err != nil || pumped == nil || pumped.Status != TaskDispatched {
		t.Fatalf("后续派发 = %+v, %v; want dispatched", pumped, err)
	}

	// ErrInvalid（已不是成员 / 发起者被移出群）同样是永久失败。
	ops.agentErr = fmt.Errorf("%w: 不是群成员", ErrInvalid)
	third, err := q.SubmitAgent(ctx, assignTask("ag_a", "ag_b", "目标不合法"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err = q.Pump(ctx, third.Target); !errors.Is(err, ErrInvalid) || got == nil || got.Status != TaskCanceled {
		t.Fatalf("ErrInvalid pump = %+v, %v; want canceled", got, err)
	}

	// 瞬时错误仍然留队（不能把临时故障当永久失败取消）。
	ops.agentErr = errors.New("磁盘忙")
	fourth, err := q.SubmitAgent(ctx, assignTask("ag_a", "ag_b", "暂时失败"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got, err = q.Pump(ctx, fourth.Target); err == nil || got != nil {
		t.Fatalf("瞬时失败 = %+v, %v; want (nil, err)", got, err)
	}
	if replayed, err := NewLedger(st).Replay(ctx, fourth.ID); err != nil || replayed.Status != TaskQueued {
		t.Fatalf("瞬时失败后 = %+v, %v; want queued", replayed, err)
	}
	if q.QueuedCountFor(fourth.Target) != 1 {
		t.Fatalf("瞬时失败必须留队：queued = %d", q.QueuedCountFor(fourth.Target))
	}
}
