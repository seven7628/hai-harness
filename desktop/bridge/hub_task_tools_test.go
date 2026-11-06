package main

// P2-09 阶段验收 B1/B2：模型侧任务工具 + 人工介入标记。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

func TestHubTaskToolsStatusAndCancel(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "TaskTools")
	hubBS, err := m.hubDMSession(string(p.ID))
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: p.ID, AgentName: p.Name},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: "/w", SessionID: "s1"},
		Title:  "模型工具", Instruction: "x", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}

	status, err := hubBS.hubEngine.GetTool(context.Background(), "task_status")
	if err != nil {
		t.Fatalf("task_status not registered: %v", err)
	}
	out, err := status.Call(context.Background(), "", `{"task_id":"`+string(task.ID)+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	var statusOut struct {
		Task   *agenthub.Task       `json:"task"`
		Events []agenthub.TaskEvent `json:"events"`
	}
	if err := json.Unmarshal([]byte(out), &statusOut); err != nil {
		t.Fatalf("task_status output = %s: %v", out, err)
	}
	if statusOut.Task == nil || statusOut.Task.ID != task.ID || statusOut.Task.Status != agenthub.TaskQueued {
		t.Fatalf("task_status = %s", out)
	}

	cancel, err := hubBS.hubEngine.GetTool(context.Background(), "task_cancel")
	if err != nil {
		t.Fatalf("task_cancel not registered: %v", err)
	}
	out, err = cancel.Call(context.Background(), "", `{"task_id":"`+string(task.ID)+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := ledger.Replay(context.Background(), task.ID)
	if err != nil || got.Status != agenthub.TaskCanceled {
		t.Fatalf("task_cancel status = %+v, %v (out=%s)", got, err, out)
	}
	if !strings.Contains(out, "已取消") {
		t.Fatalf("task_cancel note missing: %s", out)
	}

	// 别人的任务不可见：不能拿随机 id 读到同机其他智能体的任务。
	foreign, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: "ag_other"},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: "/w", SessionID: "s1"},
		Title:  "别人的", Instruction: "x", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := status.Call(context.Background(), "", `{"task_id":"`+string(foreign.ID)+`"}`); err == nil {
		t.Fatal("task_status must hide other agents' tasks")
	}
	if _, err := cancel.Call(context.Background(), "", `{"task_id":"`+string(foreign.ID)+`"}`); err == nil {
		t.Fatal("task_cancel must hide other agents' tasks")
	}
	if got, err := ledger.Replay(context.Background(), foreign.ID); err != nil || got.Status != agenthub.TaskQueued {
		t.Fatalf("foreign task was modified: %+v, %v", got, err)
	}
}

func TestHubHumanInterventionMarksTask(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	createWorkspaceAgent(t, m, buf, 1, "HumanAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	hub := createHubAgent(t, m, buf, 2, "HumanHub")
	hubBS, err := m.hubDMSession(string(hub.ID))
	if err != nil {
		t.Fatal(err)
	}
	target, err := m.create(wsPath, "s-human", "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: hub.ID, AgentName: hub.Name},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: wsPath, SessionID: "s-human"},
		Title:  "人工介入", Instruction: "x", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{}); err != nil {
		t.Fatal(err)
	}
	target.hubTaskIDs = []agenthub.TaskID{task.ID}
	target.running.Store(true)
	// 运行中插话：Injected=true，且不含 hub_task header。
	target.emit(&events.UserInputsConsumed{
		EventType: events.UserInputsConsumedType,
		UserTexts: []string{"顺便把标题改成蓝色"},
		UserContents: [][]core.Content{{
			{Type: core.ContentTypeText, Content: "顺便把标题改成蓝色"},
		}},
		Injected: true,
	})
	target.emit(&events.AgentEnd{
		EventType: events.AgentEndType, RunId: "r-human", Content: "完成",
	})
	got, err := ledger.Replay(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.HumanIntervened {
		t.Fatalf("human_intervened = false, want true (status=%s)", got.Status)
	}
	var pushed string
	for _, msg := range hubBS.s.History() {
		for _, c := range msg.Content {
			if c.Type == core.ContentTypeTaskResult && strings.Contains(c.Content, string(task.ID)) {
				pushed = c.Content
			}
		}
	}
	if !strings.Contains(pushed, "人类在该任务运行中介入了") {
		t.Fatalf("task_result missing intervention notice: %q", pushed)
	}
	// 新 batch 开口必须清零：上一轮的介入标记不能外溢到下一个任务。
	target.hubHumanIntervened.Store(true)
	target.emit(&events.UserInputsConsumed{
		EventType: events.UserInputsConsumedType,
		UserTexts: []string{"下一个任务"},
		Injected:  false,
	})
	if target.hubHumanIntervened.Load() {
		t.Fatal("新 batch 开口未清零 hubHumanIntervened")
	}
}
