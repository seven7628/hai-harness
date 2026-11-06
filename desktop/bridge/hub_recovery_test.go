package main

// P2-08：崩溃恢复（queued 重入队 / 无 run → interrupted / 未回推重推 / 不自动重试）。

import (
	"context"
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
