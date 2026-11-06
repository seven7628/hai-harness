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

// P2.5-04：初始/重试护栏必须同时带群预算与全局预算，不能只在 retry 路径带群。
func TestHubGuardForTaskIncludesGroupAndGlobalBudget(t *testing.T) {
	m, buf := testManager(t)
	agent := createHubAgent(t, m, buf, 1, "BudgetOwner")
	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_group_create", map[string]any{
		"name": "预算法群",
		"members": []map[string]any{
			{"agent_id": string(agent.ID), "role": "owner"},
		},
		"policy": map[string]any{"daily_budget_usd": 50},
	})
	if !ok {
		t.Fatalf("group create: %s", errMsg)
	}
	g := groupFromData(t, data)
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	old, err := agenthub.NewLedger(st).Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginGroup, GroupID: g.ID, AgentID: agent.ID},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: t.TempDir(), SessionID: "s1"},
		Title:  "群任务", Instruction: "群任务", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	guard := m.hubGuardForTask(old)
	if guard.Limits.GroupDailyBudgetUSD != 50 {
		t.Fatalf("group budget = %v, want 50", guard.Limits.GroupDailyBudgetUSD)
	}
	if guard.Limits.GlobalDailyBudgetUSD != agenthub.DefaultGlobalDailyBudgetUSD {
		t.Fatalf("global budget = %v, want %v", guard.Limits.GlobalDailyBudgetUSD, agenthub.DefaultGlobalDailyBudgetUSD)
	}
	if guard.Limits.DailyBudgetUSD != agenthub.DefaultAgentDailyBudgetUSD {
		t.Fatalf("agent budget = %v, want %v", guard.Limits.DailyBudgetUSD, agenthub.DefaultAgentDailyBudgetUSD)
	}
}

// P2.5-04：无价用量按智能体只告警一次，避免每个任务刷屏。
func TestHubUnpricedUsageWarnsOncePerAgent(t *testing.T) {
	m, buf := testManager(t)
	agent := createHubAgent(t, m, buf, 1, "UnpricedOwner")
	buf.Reset()
	m.warnUnpricedUsageOnce("task_a", agent.ID)
	m.warnUnpricedUsageOnce("task_b", agent.ID)
	if got := strings.Count(buf.String(), `"usage_unpriced"`); got != 1 {
		t.Fatalf("usage_unpriced count = %d, want 1; out=%s", got, buf.String())
	}
	if !usageHasTokens(agenthub.Usage{Input: 1}) || usageHasTokens(agenthub.Usage{}) {
		t.Fatal("usageHasTokens 口径错误")
	}
}

// P2.5-04：日预算/单任务上限的运行中硬停判定（已结算成本 + 本任务已花）。
func TestHubBudgetCheckHardStop(t *testing.T) {
	m, buf := testManager(t)
	ok, errMsg, data := hubCmd(t, m, buf, 1, "hub_agent_create", map[string]any{
		"profile": map[string]any{
			"name": "HardStop", "provider": "deepseek", "model": "deepseek-v4-flash", "role": "预算",
			"policy": map[string]any{"daily_budget_usd": 20, "max_task_cost_usd": 1},
		},
	})
	if !ok {
		t.Fatalf("create: %s", errMsg)
	}
	agent := agentFromData(t, data)
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	task, err := agenthub.NewLedger(st).Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginHuman, AgentID: agent.ID},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: t.TempDir(), SessionID: "s1"},
		Title:  "预算任务", Instruction: "预算任务", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if stop, msg := m.hubBudgetCheck(task.ID, 1.01); !stop || !strings.Contains(msg, "单任务") {
		t.Fatalf("single-task stop = %v %q, want true 单任务", stop, msg)
	}
	if stop, _ := m.hubBudgetCheck(task.ID, 0.5); stop {
		t.Fatal("under budget should not stop")
	}
	if err := st.SaveTask(context.Background(), func() *agenthub.Task {
		tk := *task
		tk.Status = agenthub.TaskRunning
		tk.UpdatedAt = time.Now().UTC()
		tk.Result = &agenthub.TaskResult{Usage: agenthub.Usage{CostPriced: true, CostUSD: 20}}
		return &tk
	}()); err != nil {
		t.Fatal(err)
	}
	if stop, msg := m.hubBudgetCheck(task.ID, 0.01); !stop || !strings.Contains(msg, "日预算") {
		t.Fatalf("daily stop = %v %q, want true 日预算", stop, msg)
	}
}

// P2.5-04：AgentEnd 收到 aborted 时，硬停标记必须把终态改回 blocked_budget。
func TestHubFinishBudgetStopOverridesAborted(t *testing.T) {
	m, buf := testManager(t)
	agent := createHubAgent(t, m, buf, 1, "StopOwner")
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginHuman, AgentID: agent.ID},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: t.TempDir(), SessionID: "s1"},
		Title:  "会被停", Instruction: "会被停", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	bs := &bridgeSession{hubTaskIDs: []agenthub.TaskID{task.ID}}
	bs.hubBudgetStop.Store(true)
	bs.hubBudgetReason.Store("智能体日预算 $20.00 已用满，运行已中止")
	m.finishHubTask(bs, &events.AgentEnd{
		RunId: "run-stop", FinishReason: core.FinishReasonAbort, Content: "部分产出",
	})
	got, err := ledger.Replay(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agenthub.TaskBlockedBudget || !strings.Contains(got.Error, "日预算") {
		t.Fatalf("settled = %s / %q, want blocked_budget with 日预算", got.Status, got.Error)
	}
}
