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

// P2.5-02 复审 B1：同一 client key 有多条 attempt，holder 改键后不能整条丢失。
func TestTaskIndexClientKeySurvivesHolderMove(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	led := NewLedger(st)
	a, err := led.Create(ctx, indexTestTask("ag_move", "first", "c-move"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := led.Create(ctx, indexTestTask("ag_move", "second", "c-move"))
	if err != nil {
		t.Fatal(err)
	}
	moved := cloneTask(b)
	moved.ClientMsgID = ""
	if err := st.SaveTask(ctx, moved); err != nil {
		t.Fatal(err)
	}
	got, ok := st.FindByClientMsgID(TaskTarget{Kind: TargetWorkspace, Workspace: "/tmp/idx", SessionID: "s1"}, "c-move")
	if !ok || got == nil || got.ID != a.ID {
		t.Fatalf("after holder move = %v/%v, want %s", got, ok, a.ID)
	}
}

// P2.5-02 复审 N5：成本按 UpdatedAt 的 UTC 日归日，不是按“现在”。
func TestTaskIndexDailyCostUsesUpdatedAt(t *testing.T) {
	st := NewMemoryStore()
	now := time.Now().UTC()
	old := &Task{
		ID: "task_oldcost", Status: TaskCompleted,
		Origin:    TaskOrigin{Kind: OriginDM, AgentID: "ag_day"},
		UpdatedAt: now.Add(-24 * time.Hour),
		Result:    &TaskResult{Usage: Usage{CostPriced: true, CostUSD: 5}},
	}
	today := &Task{
		ID: "task_todaycost", Status: TaskCompleted,
		Origin:    TaskOrigin{Kind: OriginDM, AgentID: "ag_day"},
		UpdatedAt: now,
		Result:    &TaskResult{Usage: Usage{CostPriced: true, CostUSD: 7}},
	}
	if err := st.SaveTask(context.Background(), old); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveTask(context.Background(), today); err != nil {
		t.Fatal(err)
	}
	if agent, _, _ := st.DailyCost("ag_day", "", now); agent != 7 {
		t.Fatalf("today cost = %v, want 7 (old day must not count)", agent)
	}
}

// P2.5-02 复审 N5：delivered 是终态，不能继续算在途；成本仍保留。
func TestTaskIndexDeliveredIsTerminal(t *testing.T) {
	st := NewMemoryStore()
	ctx := context.Background()
	task := &Task{
		ID: "task_delivered", Status: TaskDelivered,
		Origin:    TaskOrigin{Kind: OriginDM, AgentID: "ag_del"},
		UpdatedAt: time.Now().UTC(),
		Result:    &TaskResult{Usage: Usage{CostPriced: true, CostUSD: 3}},
	}
	if err := st.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if n := st.ActiveCount("ag_del"); n != 0 {
		t.Fatalf("delivered active = %d, want 0", n)
	}
	if cost, _, _ := st.DailyCost("ag_del", "", time.Now().UTC()); cost != 3 {
		t.Fatalf("delivered cost = %v, want 3", cost)
	}
}

// P2.5-02 复审 N5：dedupe 的 workspace/session 按原样比较（与旧 sameTarget 一致）。
func TestTaskIndexDedupeDoesNotNormalizeTarget(t *testing.T) {
	st := NewMemoryStore()
	now := time.Now().UTC()
	base := indexTestTask("ag_dedupe", "same", "c-a")
	base.ID = "task_dedupe_a"
	base.Target.Workspace = "/w"
	base.CreatedAt = now
	base.UpdatedAt = now
	if err := st.SaveTask(context.Background(), base); err != nil {
		t.Fatal(err)
	}
	probe := indexTestTask("ag_dedupe", "same", "c-b")
	probe.Target.Workspace = "/w/"
	if _, ok := st.FindDuplicate(probe, now.Add(-time.Minute)); ok {
		t.Fatal("/w and /w/ must not be treated as the same dedupe target")
	}
}

// P2.5-02 复审 N4：只实现 Store（不实现 TaskIndex）的替身必须回退全盘扫描。
type fallbackStore struct{ Store }

func TestLedgerAndGuardFallbackWithoutTaskIndex(t *testing.T) {
	ctx := context.Background()
	base := NewMemoryStore()
	fs := &fallbackStore{Store: base}
	led := NewLedger(fs)
	created, err := led.Create(ctx, indexTestTask("ag_fallback", "same", "c-fallback"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := led.FindByClientMsgID(ctx, TaskTarget{Kind: TargetWorkspace, Workspace: "/tmp/idx", SessionID: "s1"}, "c-fallback")
	if err != nil || got == nil || got.ID != created.ID {
		t.Fatalf("fallback FindByClientMsgID = %v, %v; want %s", got, err, created.ID)
	}
	probe := indexTestTask("ag_fallback", "same", "c-other")
	probe.CreatedAt = time.Now().UTC()
	guard := &Guard{Now: func() time.Time { return time.Now().UTC() }, Limits: GuardLimits{MaxConcurrent: 10}}
	if err := guard.Check(ctx, fs, probe); !errors.Is(err, ErrDuplicateTask) {
		t.Fatalf("fallback Guard err = %v, want ErrDuplicateTask", err)
	}
}

// P2.5-02 复审 N3：零值 Store 不能声称自己有空索引（否则 Guard 会静默 fail-open）。
func TestZeroValueStoreTaskIndexNotReady(t *testing.T) {
	var fs FileStore
	var ms MemoryStore
	if fs.TaskIndexReady() {
		t.Fatal("zero-value FileStore must not claim TaskIndex")
	}
	if ms.TaskIndexReady() {
		t.Fatal("zero-value MemoryStore must not claim TaskIndex")
	}
}

// P2.5-02 N-a：实现 TaskIndex 但 Ready=false 的包装 store 必须回退 ListTasks，
// 不能把空索引当成「没有重复」。M5/M6 变异（删掉 && TaskIndexReady()）会挂。
type notReadyIndexStore struct{ *MemoryStore }

func (s *notReadyIndexStore) TaskIndexReady() bool { return false }

func TestGuardAndLedgerFallBackWhenTaskIndexNotReady(t *testing.T) {
	ctx := context.Background()
	mem := NewMemoryStore()
	led := NewLedger(mem)
	now := time.Now().UTC()
	created, err := led.Create(ctx, indexTestTask("ag_fallback_ready", "same", "c-ready"))
	if err != nil {
		t.Fatal(err)
	}
	st := &notReadyIndexStore{MemoryStore: mem}
	got, err := NewLedger(st).FindByClientMsgID(ctx, TaskTarget{Kind: TargetWorkspace, Workspace: "/tmp/idx", SessionID: "s1"}, "c-ready")
	if err != nil || got == nil || got.ID != created.ID {
		t.Fatalf("ledger fallback = %v, %v; want %s", got, err, created.ID)
	}
	probe := indexTestTask("ag_fallback_ready", "same", "c-other")
	probe.CreatedAt, probe.UpdatedAt = now, now
	guard := &Guard{Now: func() time.Time { return now }, Limits: GuardLimits{MaxConcurrent: 10}}
	if err := guard.Check(ctx, st, probe); !errors.Is(err, ErrDuplicateTask) {
		t.Fatalf("guard fallback err = %v, want ErrDuplicateTask", err)
	}
}

// P2.5-02 N-b：holder 改 target（不只是清空 client id）也要只摘旧 key 的单条。
func TestTaskIndexHolderTargetMoveKeepsSiblings(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	led := NewLedger(st)
	a, err := led.Create(ctx, indexTestTask("ag_move_t", "first", "c-move-t"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := led.Create(ctx, indexTestTask("ag_move_t", "second", "c-move-t"))
	if err != nil {
		t.Fatal(err)
	}
	moved := cloneTask(b)
	moved.Target.Workspace = "/tmp/idx2"
	moved.Target.SessionID = "s2"
	if err := st.SaveTask(ctx, moved); err != nil {
		t.Fatal(err)
	}
	oldTarget := TaskTarget{Kind: TargetWorkspace, Workspace: "/tmp/idx", SessionID: "s1"}
	got, ok := st.FindByClientMsgID(oldTarget, "c-move-t")
	if !ok || got == nil || got.ID != a.ID {
		t.Fatalf("old key after target move = %v/%v, want %s", got, ok, a.ID)
	}
	newTarget := TaskTarget{Kind: TargetWorkspace, Workspace: "/tmp/idx2", SessionID: "s2"}
	got, ok = st.FindByClientMsgID(newTarget, "c-move-t")
	if !ok || got == nil || got.ID != b.ID {
		t.Fatalf("new key after target move = %v/%v, want %s", got, ok, b.ID)
	}
}

// P2.5-02 N-c：delivered 在 removeLocked 里也算终态（先 running 再 delivered）。
func TestTaskIndexDeliveredRemovalDecrementsActive(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	task := &Task{
		ID: "task_del_remove", Status: TaskRunning,
		Origin:    TaskOrigin{Kind: OriginDM, AgentID: "ag_del_rm"},
		UpdatedAt: time.Now().UTC(),
	}
	if err := st.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if n := st.ActiveCount("ag_del_rm"); n != 1 {
		t.Fatalf("running active = %d, want 1", n)
	}
	task.Status = TaskDelivered
	if err := st.SaveTask(ctx, task); err != nil {
		t.Fatal(err)
	}
	if n := st.ActiveCount("ag_del_rm"); n != 0 {
		t.Fatalf("delivered active after re-save = %d, want 0", n)
	}
}

// P2.5-02 N-d：同一 client key 重复 Save 不能在列表里堆积重复 ID。
func TestTaskIndexRepeatedSaveDoesNotDuplicateClientEntry(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	task := indexTestTask("ag_dup_save", "x", "c-dup-save")
	task.ID = "task_dup_save"
	for i := 0; i < 5; i++ {
		task.Status = TaskQueued
		if err := st.SaveTask(ctx, task); err != nil {
			t.Fatal(err)
		}
	}
	st.taskIdx.mu.RLock()
	n := 0
	for _, ids := range st.taskIdx.byClient {
		n += len(ids)
	}
	st.taskIdx.mu.RUnlock()
	if n != 1 {
		t.Fatalf("byClient entries = %d, want 1", n)
	}
}
