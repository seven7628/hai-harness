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
