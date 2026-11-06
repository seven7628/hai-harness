package main

// P2-08：崩溃恢复（queued 重入队 / 无 run → interrupted / 未回推重推 / 不自动重试）。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
)

func TestHubRecoveryQueuedRequeued(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	createWorkspaceAgent(t, m, buf, 1, "RecoverAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	if _, err := m.create(wsPath, "s-rec", ""); err != nil {
		t.Fatal(err)
	}
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	task, err := agenthub.NewLedger(st).Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: "ag_recover"},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: wsPath, SessionID: "s-rec"},
		Title:  "重启排队", Instruction: "重启排队", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	m.recoverHubTasks()
	q, err := m.hubQueueFor()
	if err != nil {
		t.Fatal(err)
	}
	if got := q.QueuedCount(wsPath, "s-rec"); got != 1 {
		t.Fatalf("queued = %d, want 1", got)
	}
	// 重复恢复不重复入队。
	m.recoverHubTasks()
	if got := q.QueuedCount(wsPath, "s-rec"); got != 1 {
		t.Fatalf("after second recovery queued = %d, want 1", got)
	}
	_ = task
}

func TestHubRecoveryDispatchedWithoutRunInterrupted(t *testing.T) {
	m, buf := testManager(t)
	hubAgent := createHubAgent(t, m, buf, 1, "RecoverHub")
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: hubAgent.ID},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: t.TempDir(), SessionID: "s-gone"},
		Title:  "无 run 任务", Instruction: "无 run 任务", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	m.recoverHubTasks()
	got, err := ledger.Replay(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agenthub.TaskInterrupted && got.Status != agenthub.TaskDelivered {
		t.Fatalf("status = %s, want interrupted（或回推后的 delivered）", got.Status)
	}
	needs, err := st.ListNeeds(context.Background(), agenthub.NeedFilter{Status: "pending"})
	if err != nil || len(needs) != 1 || needs[0].Kind != "failure" {
		t.Fatalf("needs = %+v, %v", needs, err)
	}
	// 不自动重试：任务没有变成新的 queued 任务。
	tasks, _ := st.ListTasks(context.Background(), agenthub.TaskFilter{})
	if len(tasks) != 1 {
		t.Fatalf("tasks = %d, want 1（不得自动重试）", len(tasks))
	}
}

func TestHubRecoveryCompletedRedeliveredOnce(t *testing.T) {
	m, buf := testManager(t)
	hubAgent := createHubAgent(t, m, buf, 1, "RecoverPush")
	if _, err := m.hubDMSession(string(hubAgent.ID)); err != nil {
		t.Fatal(err)
	}
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: hubAgent.ID},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: t.TempDir(), SessionID: "s-push"},
		Title:  "已完成", Instruction: "已完成", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Finish(context.Background(), agenthub.FinishInput{
		TaskID: task.ID, RunID: "r-done", Status: agenthub.TaskCompleted,
		Result: &agenthub.TaskResult{Summary: "结果摘要", Usage: agenthub.Usage{Input: 10, Output: 5, CostPriced: true, CostUSD: 0.25}},
	}); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	m.recoverHubTasks()
	got, err := ledger.Replay(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agenthub.TaskDelivered {
		t.Fatalf("status = %s, want delivered", got.Status)
	}
	// 第二次恢复不得再推一次：task_result_delivered 事件只出现一次。
	m.recoverHubTasks()
	line := buf.String()
	if n := strings.Count(line, `"task_result_delivered"`); n != 1 {
		t.Fatalf("task_result_delivered count = %d, want 1: %s", n, line)
	}
	got2, _ := ledger.Replay(context.Background(), task.ID)
	if got2.Status != agenthub.TaskDelivered {
		t.Fatalf("second recovery status = %s", got2.Status)
	}
	_ = buf
}

// P2-05 复审 B-2：恢复与正常结算共用 hubFinishMu，并发恢复也只能推一次。
func TestHubRecoveryConcurrentSinglePush(t *testing.T) {
	m, buf := testManager(t)
	hubAgent := createHubAgent(t, m, buf, 1, "RecoverConcurrent")
	if _, err := m.hubDMSession(string(hubAgent.ID)); err != nil {
		t.Fatal(err)
	}
	st, _ := m.hubStore()
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: hubAgent.ID},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: t.TempDir(), SessionID: "s-conc"},
		Title:  "并发恢复", Instruction: "x", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Finish(context.Background(), agenthub.FinishInput{
		TaskID: task.ID, RunID: "run-conc", Status: agenthub.TaskCompleted,
		Result: &agenthub.TaskResult{Summary: "done"},
	}); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); m.recoverHubTasks() }()
	}
	wg.Wait()
	if n := strings.Count(buf.String(), `"task_result_delivered"`); n != 1 {
		t.Fatalf("concurrent recovery pushed %d times, want 1", n)
	}
}

// P2-07 B1：blocked_budget 也必须走「回推 → MarkNotified → 恢复重推」，
// 否则预算阻断的结果首次推送失败就永久丢失。
func TestHubRecoveryBlockedBudgetRedelivered(t *testing.T) {
	m, buf := testManager(t)
	hubAgent := createHubAgent(t, m, buf, 1, "RecoverBudget")
	if _, err := m.hubDMSession(string(hubAgent.ID)); err != nil {
		t.Fatal(err)
	}
	st, _ := m.hubStore()
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: hubAgent.ID},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: t.TempDir(), SessionID: "s-budget"},
		Title:  "预算阻断", Instruction: "x", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Finish(context.Background(), agenthub.FinishInput{
		TaskID: task.ID, RunID: "run-budget", Status: agenthub.TaskBlockedBudget,
		Result: &agenthub.TaskResult{Summary: "太贵", Error: "单任务成本超限"},
	}); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	m.recoverHubTasks()
	got, err := ledger.Replay(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agenthub.TaskBlockedBudget {
		t.Fatalf("status = %s, want blocked_budget", got.Status)
	}
	if len(got.NotifiedRuns) != 1 || got.NotifiedRuns[0] != "run-budget" {
		t.Fatalf("notified_runs = %v, want [run-budget]", got.NotifiedRuns)
	}
	if !strings.Contains(buf.String(), `"task_result_delivered"`) {
		t.Fatalf("blocked_budget 结果未回推: %s", buf.String())
	}
	if strings.Contains(buf.String(), `"notify_failed"`) {
		t.Fatalf("blocked_budget 已回推却报 notify_failed: %s", buf.String())
	}
	// 二次恢复：已 notified 的任务不得再进入 push（M5 类回归）。仓库此前没有
	// 这条断言，绕过 alreadyNotified 只会多发一条 hub_task_update。
	buf.Reset()
	m.recoverHubTasks()
	if strings.Contains(buf.String(), `"task_result_delivered"`) || strings.Contains(buf.String(), `"hub_task_update"`) {
		t.Fatalf("二次恢复重复处理了已通知任务: %s", buf.String())
	}
}

// P2-08 复审 B-1：ListTasks 按 UpdatedAt 降序，Restore 不能照单全收。
// A/B/C 三条排队任务恢复后必须按创建顺序派发，而不是 C/B/A。
func TestHubRecoveryQueuedKeepsCreatedOrder(t *testing.T) {
	m, buf := testManager(t)
	_ = buf
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenthub.NewLedger(st)
	base := time.Now().UTC().Add(-time.Hour)
	for i, title := range []string{"A", "B", "C"} {
		at := base.Add(time.Duration(i) * time.Second)
		if _, err := ledger.Create(context.Background(), &agenthub.Task{
			Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: "ag_order"},
			Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: "/w", SessionID: "s1"},
			Title:  title, Instruction: title, Callback: agenthub.CallbackAgentEnd,
			CreatedAt: at, UpdatedAt: at,
		}); err != nil {
			t.Fatal(err)
		}
	}
	ops := &recoveryOrderOps{}
	m.hubQueue = agenthub.NewQueue(ledger, ops)
	if err := m.recoverHubTasks(); err != nil {
		t.Fatal(err)
	}
	target := agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: "/w", SessionID: "s1"}
	for i := 0; i < 3; i++ {
		if _, err := m.hubQueue.Pump(context.Background(), target); err != nil {
			t.Fatal(err)
		}
	}
	if got := strings.Join(ops.dispatched, ","); got != "A,B,C" {
		t.Fatalf("恢复后派发顺序 = %s, want A,B,C", got)
	}
}

type recoveryOrderOps struct {
	dispatched []string
}

func (o *recoveryOrderOps) ListWorkspaces(context.Context) ([]agenthub.WorkspaceInfo, error) {
	return nil, nil
}
func (o *recoveryOrderOps) CreateWorkspace(context.Context, agenthub.CreateWorkspaceReq) (agenthub.WorkspaceInfo, error) {
	return agenthub.WorkspaceInfo{}, nil
}
func (o *recoveryOrderOps) ListSessions(context.Context, string) ([]agenthub.SessionInfo, error) {
	return nil, nil
}
func (o *recoveryOrderOps) SessionStatus(context.Context, string, string) (agenthub.SessionInfo, error) {
	return agenthub.SessionInfo{ID: "s1", Workspace: "/w"}, nil
}
func (o *recoveryOrderOps) CreateSession(context.Context, agenthub.CreateSessionReq) (agenthub.SessionInfo, error) {
	return agenthub.SessionInfo{}, nil
}
func (o *recoveryOrderOps) Dispatch(_ context.Context, req agenthub.DispatchReq) (agenthub.DispatchResult, error) {
	o.dispatched = append(o.dispatched, req.Text)
	return agenthub.DispatchResult{SessionID: req.SessionID, Accepted: true}, nil
}
func (o *recoveryOrderOps) InterruptSession(context.Context, string) error { return nil }

// P2-08 复审 B-2：恢复失败不能先闩。半写 task.json → 首次 hub_open 告警且不置
// hubRecovered；修好后同一进程再次 hub_open 必须真正重试。
func TestHubRecoveryFailureDoesNotLatch(t *testing.T) {
	m, buf := testManager(t)
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	task, err := agenthub.NewLedger(st).Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: "ag_latch"},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: "/w", SessionID: "s1"},
		Title:  "半写", Instruction: "x", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(appDataDir(), "hub", "tasks", string(task.ID), "task.json")
	good, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.ensureHubRecovery()
	if m.hubRecovered {
		t.Fatal("恢复失败后 hubRecovered 不应置位")
	}
	if !strings.Contains(buf.String(), `"recovery_failed"`) {
		t.Fatalf("恢复失败没有告警: %s", buf.String())
	}
	for _, want := range []string{`"scope":"hub"`, `"code":"recovery_failed"`, `"severity":"error"`} {
		if !strings.Contains(buf.String(), want) {
			t.Fatalf("audit envelope 缺 %s: %s", want, buf.String())
		}
	}
	if err := os.WriteFile(path, good, 0o600); err != nil {
		t.Fatal(err)
	}
	m.ensureHubRecovery()
	if !m.hubRecovered {
		t.Fatal("存储修好后第二次 hub_open 必须重试并成功")
	}
	q, err := m.hubQueueFor()
	if err != nil {
		t.Fatal(err)
	}
	if n := q.QueuedCount("/w", "s1"); n != 1 {
		t.Fatalf("第二次恢复 queued = %d, want 1", n)
	}
}
