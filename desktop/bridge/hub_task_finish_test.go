package main

// P2-04：hub_task 相关性 + AgentEnd 结算。

import (
	"context"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

func newDispatchedHubTask(t *testing.T, m *manager) *agenthub.Task {
	t.Helper()
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: "ag_1"},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: "/w", SessionID: "s1"},
		Title:  "结算", Instruction: "做一份对比表", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	task, err = ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{At: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestHubTaskFinishCompleted(t *testing.T) {
	m, buf := testManager(t)
	task := newDispatchedHubTask(t, m)
	bs := &bridgeSession{ctx: context.Background(), hubTaskIDs: []agenthub.TaskID{task.ID}}
	m.finishHubTask(bs, &events.AgentEnd{
		RunId: "run-1", Content: "对比表已发你", FinishReason: core.FinishReasonStop,
		Usage: &core.Usage{Input: 100, Output: 20, Cost: core.Cost{Total: 3000, Priced: true}},
	})
	st, _ := m.hubStore()
	got, err := st.LoadTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agenthub.TaskCompleted {
		t.Fatalf("status = %q, want completed", got.Status)
	}
	if got.Result == nil || got.Result.Summary != "对比表已发你" || got.Result.Usage.Input != 100 ||
		got.Result.Usage.Output != 20 || !got.Result.Usage.CostPriced || got.Result.Usage.CostUSD != 0.003 {
		t.Fatalf("result = %+v", got.Result)
	}
	if len(bs.hubTaskIDs) != 0 {
		t.Fatal("hubTaskIDs must be cleared after settle")
	}
	_ = buf
}

func TestHubTaskFinishFailedAndInterrupted(t *testing.T) {
	m, _ := testManager(t)
	st, _ := m.hubStore()
	ledger := agenthub.NewLedger(st)
	mk := func() *agenthub.Task {
		task, err := ledger.Create(context.Background(), &agenthub.Task{
			Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: "ag_1"},
			Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: "/w", SessionID: "s1"},
			Title:  "t", Instruction: "x", Callback: agenthub.CallbackAgentEnd,
		})
		if err != nil {
			t.Fatal(err)
		}
		task, err = ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{})
		if err != nil {
			t.Fatal(err)
		}
		return task
	}
	failed := mk()
	bs := &bridgeSession{ctx: context.Background(), hubTaskIDs: []agenthub.TaskID{failed.ID}}
	m.finishHubTask(bs, &events.AgentEnd{RunId: "r1", FinishReason: core.FinishReasonError, Error: "上游 500", ErrorKind: "generic"})
	got, _ := st.LoadTask(context.Background(), failed.ID)
	if got.Status != agenthub.TaskFailed || got.Error == "" {
		t.Fatalf("failed task = %+v", got)
	}
	interrupted := mk()
	bs = &bridgeSession{ctx: context.Background(), hubTaskIDs: []agenthub.TaskID{interrupted.ID}}
	m.finishHubTask(bs, &events.AgentEnd{RunId: "r2", Error: "用户中断", ErrorKind: "aborted"})
	got, _ = st.LoadTask(context.Background(), interrupted.ID)
	if got.Status != agenthub.TaskInterrupted {
		t.Fatalf("interrupted task = %+v", got)
	}
}

func TestHubTaskFinishBatchConflict(t *testing.T) {
	m, buf := testManager(t)
	st, _ := m.hubStore()
	ledger := agenthub.NewLedger(st)
	ids := make([]agenthub.TaskID, 0, 2)
	for i := 0; i < 2; i++ {
		task, err := ledger.Create(context.Background(), &agenthub.Task{
			Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: "ag_1"},
			Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: "/w", SessionID: "s1"},
			Title:  "t", Instruction: "x", Callback: agenthub.CallbackAgentEnd,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{}); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, task.ID)
	}
	bs := &bridgeSession{ctx: context.Background(), hubTaskIDs: ids}
	m.finishHubTask(bs, &events.AgentEnd{RunId: "r1", Content: "ok", FinishReason: core.FinishReasonStop})
	first, _ := st.LoadTask(context.Background(), ids[0])
	second, _ := st.LoadTask(context.Background(), ids[1])
	if first.Status != agenthub.TaskCompleted {
		t.Fatalf("first status = %q", first.Status)
	}
	if second.Status != agenthub.TaskFailed || second.Error == "" {
		t.Fatalf("second (batch conflict) = %+v", second)
	}
	found := false
	for _, msg := range readLines(buf) {
		if msg["event_type"] == "hub_alert" && msg["kind"] == "batch_conflict" && msg["task_id"] == string(ids[1]) {
			found = true
		}
	}
	if !found {
		t.Fatal("missing hub_alert batch_conflict")
	}
}

func TestHubDispatchMessageHeaderFirst(t *testing.T) {
	msg := hubDispatchMessage("做一份对比表", `{"task_id":"task_x"}`)
	if len(msg.Content) != 2 || msg.Content[0].Type != core.ContentTypeHubTask || msg.Content[1].Type != core.ContentTypeText {
		t.Fatalf("content = %+v", msg.Content)
	}
	if msg.Content[1].Content != "做一份对比表" {
		t.Fatalf("body = %q", msg.Content[1].Content)
	}
	plain := hubDispatchMessage("只派发", "")
	if len(plain.Content) != 1 || plain.Content[0].Type != core.ContentTypeText {
		t.Fatalf("plain content = %+v", plain.Content)
	}
}
