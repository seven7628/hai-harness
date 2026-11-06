package main

// P2-07：护栏在生产链路的可见性（hub_alert）与结算预算。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

func TestHubGuardBlocksAndAlerts(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	createWorkspaceAgent(t, m, buf, 1, "GuardAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_agent_create", map[string]any{
		"profile": map[string]any{
			"name": "GuardHub", "provider": "deepseek", "model": "deepseek-v4-flash", "role": "预算",
			"policy": map[string]any{"daily_budget_usd": 0.01},
		},
	})
	if !ok {
		t.Fatalf("create: %s", errMsg)
	}
	hubAgent := agentFromData(t, data)
	// 已经花了 $5（今天）→ 日预算 0.01 必须拒绝新派发。
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenthub.NewLedger(st)
	created, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: hubAgent.ID},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: wsPath, SessionID: "s-old"},
		Title:  "已花", Instruction: "已花", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), created.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Finish(context.Background(), agenthub.FinishInput{
		TaskID: created.ID, RunID: "r-old", Status: agenthub.TaskCompleted,
		Result: &agenthub.TaskResult{Usage: agenthub.Usage{CostPriced: true, CostUSD: 5}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := m.create(wsPath, "s-guard", ""); err != nil {
		t.Fatal(err)
	}
	hubBS, err := m.hubDMSession(string(hubAgent.ID))
	if err != nil {
		t.Fatal(err)
	}
	tool, err := hubBS.hubEngine.GetTool(context.Background(), "session_send")
	if err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if _, err := tool.Call(context.Background(), "session_send",
		`{"workspace":`+jsonString(wsPath)+`,"session_id":"s-guard","text":"新任务"}`); err == nil {
		t.Fatal("session_send 应被日预算拒绝")
	}
	if !strings.Contains(buf.String(), `"hub_alert"`) || !strings.Contains(buf.String(), `"blocked_budget"`) {
		t.Fatalf("missing hub_alert blocked_budget: %s", buf.String())
	}
}

func TestHubFinishBlocksOverTaskBudget(t *testing.T) {
	m, buf := testManager(t)
	ok, errMsg, data := hubCmd(t, m, buf, 1, "hub_agent_create", map[string]any{
		"profile": map[string]any{
			"name": "BudgetHub", "provider": "deepseek", "model": "deepseek-v4-flash", "role": "预算",
			"policy": map[string]any{"max_task_cost_usd": 0.5},
		},
	})
	if !ok {
		t.Fatalf("create: %s", errMsg)
	}
	hubAgent := agentFromData(t, data)
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: hubAgent.ID},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: t.TempDir(), SessionID: "s1"},
		Title:  "贵任务", Instruction: "贵任务", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	hubBS, err := m.hubDMSession(string(hubAgent.ID))
	if err != nil {
		t.Fatal(err)
	}
	hubBS.hubTaskIDs = []agenthub.TaskID{task.ID}
	buf.Reset()
	usage := &core.Usage{Cost: core.Cost{Priced: true, Total: 1_500_000}}
	m.finishHubTask(hubBS, &events.AgentEnd{
		RunId: "r1", Content: "做完了", Timestamp: time.Now(),
		EventType: events.AgentEndType, Usage: usage,
	})
	got, err := ledger.Replay(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agenthub.TaskBlockedBudget {
		t.Fatalf("status = %s, want blocked_budget", got.Status)
	}
	if got.Result == nil || !strings.Contains(got.Result.Error, "超过上限") {
		t.Fatalf("result = %+v", got.Result)
	}
	if !strings.Contains(buf.String(), `"blocked_budget"`) {
		t.Fatalf("missing hub_alert: %s", buf.String())
	}
	// blocked_budget 仍必须回推结果（预算阻断 ≠ 结果不可送达）：保留状态，
	// 但 NotifiedRuns 要占位，发起者收到 task_result_delivered。
	if len(got.NotifiedRuns) != 1 || got.NotifiedRuns[0] != "r1" {
		t.Fatalf("blocked_budget notified_runs = %v, want [r1]", got.NotifiedRuns)
	}
	if !strings.Contains(buf.String(), `"task_result_delivered"`) {
		t.Fatalf("blocked_budget result not pushed: %s", buf.String())
	}
	if strings.Contains(buf.String(), `"notify_failed"`) {
		t.Fatalf("blocked_budget pushed but notify_failed: %s", buf.String())
	}
	var taskResult string
	for _, msg := range hubBS.s.History() {
		for _, c := range msg.Content {
			if c.Type == core.ContentTypeTaskResult && strings.Contains(c.Content, string(task.ID)) {
				taskResult = c.Content
			}
		}
	}
	if taskResult == "" {
		t.Fatalf("blocked_budget task_result not in origin history: %s", buf.String())
	}
	for _, want := range []string{"预算阻断", "不要自动重试", "超过上限"} {
		if !strings.Contains(taskResult, want) {
			t.Fatalf("blocked_budget task_result missing %q: %s", want, taskResult)
		}
	}
	if strings.Contains(taskResult, "据此继续当前工作") {
		t.Fatalf("blocked_budget must not tell model to continue: %s", taskResult)
	}
	_ = json.Marshal
}
