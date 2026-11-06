package main

// P2-04：hub_task 相关性 + AgentEnd 结算。

import (
	"context"
	"strings"
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
	// 真实路径是 Usage.Add 跨轮累加（P2-04 复审 B1）：不能手搓一个 Priced=true 的结构。
	var usage core.Usage
	usage = usage.Add(core.Usage{Input: 60, Cost: core.Cost{Total: 1800, Priced: true, Source: core.CostSourceBuiltin}})
	usage = usage.Add(core.Usage{Input: 40, Output: 20, Cost: core.Cost{Total: 1200, Priced: true, Source: core.CostSourceBuiltin}})
	if !usage.Cost.Priced {
		t.Fatalf("aggregated usage lost Priced: %+v", usage.Cost)
	}
	m.finishHubTask(bs, &events.AgentEnd{
		RunId: "run-1", Content: "对比表已发你", FinishReason: core.FinishReasonStop,
		Usage: &usage,
		Artifacts: []core.Artifact{
			{Name: "out.md", Path: "out.md", Kind: "created", Added: 3, Size: 120, Lines: 4},
		},
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
	if len(got.Result.Artifacts) != 1 || got.Result.Artifacts[0].Name != "out.md" || got.Result.Artifacts[0].Kind != "created" {
		t.Fatalf("artifacts = %+v", got.Result.Artifacts)
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

// P2-05：终态回推发起智能体的 DM 会话（task_result 消息 + TaskResultDelivered 事件），
// 成功后账本进入 delivered（幂等标记）。
func TestHubTaskFinishPushesResultToOrigin(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "OriginAgent")
	if _, err := m.hubDMSession(string(p.ID)); err != nil {
		t.Fatal(err)
	}
	st, _ := m.hubStore()
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: p.ID, AgentName: "OriginAgent"},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: "/w", SessionID: "s1"},
		Title:  "对比表任务", Instruction: "做一份对比表", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{}); err != nil {
		t.Fatal(err)
	}
	buf.Take()
	bs := &bridgeSession{ctx: context.Background(), hubTaskIDs: []agenthub.TaskID{task.ID}}
	m.finishHubTask(bs, &events.AgentEnd{
		RunId: "run-push", Content: "表格已生成", FinishReason: core.FinishReasonStop,
		Usage: &core.Usage{Input: 10, Output: 5},
		Artifacts: []core.Artifact{
			{Name: "table.md", Path: "out/table.md", Kind: "created", Added: 12, Size: 345, Lines: 12},
		},
	})
	got, err := st.LoadTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agenthub.TaskDelivered {
		t.Fatalf("status = %q, want delivered", got.Status)
	}
	if got.Result == nil || len(got.Result.Artifacts) != 1 || got.Result.Artifacts[0].Path != "out/table.md" {
		t.Fatalf("artifacts = %+v", got.Result)
	}
	found := false
	for _, msg := range readLines(buf) {
		if msg["event_type"] == "task_result_delivered" && msg["task_id"] == string(task.ID) {
			found = true
		}
	}
	if !found {
		t.Fatalf("missing task_result_delivered for %s", task.ID)
	}
}

// 同一 run 重复结算（事件重放）不重复回推。
func TestHubTaskFinishIdempotentPush(t *testing.T) {
	m, buf := testManager(t)
	p := createHubAgent(t, m, buf, 1, "IdemAgent")
	if _, err := m.hubDMSession(string(p.ID)); err != nil {
		t.Fatal(err)
	}
	st, _ := m.hubStore()
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: p.ID},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: "/w", SessionID: "s1"},
		Title:  "幂等", Instruction: "x", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{}); err != nil {
		t.Fatal(err)
	}
	buf.Take()
	ae := &events.AgentEnd{RunId: "run-idem", Content: "done", FinishReason: core.FinishReasonStop}
	for i := 0; i < 2; i++ {
		bs := &bridgeSession{ctx: context.Background(), hubTaskIDs: []agenthub.TaskID{task.ID}}
		m.finishHubTask(bs, ae)
	}
	n := 0
	var seen []map[string]any
	for _, msg := range readLines(buf) {
		seen = append(seen, msg)
		if msg["event_type"] == "task_result_delivered" && msg["task_id"] == string(task.ID) {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("task_result_delivered count = %d, want 1", n)
	}
	// 重放不能撞状态机产生假告警；delivered 必须有快照（复审 B1/B3）。
	deliveredUpdates := 0
	for _, msg := range seen {
		if msg["event_type"] == "hub_alert" && msg["kind"] == "settle_failed" {
			t.Fatalf("replay produced settle_failed: %v", msg)
		}
		if msg["event_type"] == "hub_task_update" && msg["status"] == string(agenthub.TaskDelivered) {
			if msg["task_id"] != string(task.ID) {
				t.Fatalf("hub_task_update missing flattened task_id: %v", msg)
			}
			deliveredUpdates++
		}
	}
	if deliveredUpdates != 1 {
		t.Fatalf("delivered hub_task_update = %d, want 1 (seen=%v)", deliveredUpdates, seen)
	}
}

// P2-04 复审 B2：用户中断（FinishReasonAbort，ErrorKind 空）必须是 interrupted。
func TestHubTaskFinishAbortIsInterrupted(t *testing.T) {
	m, _ := testManager(t)
	task := newDispatchedHubTask(t, m)
	bs := &bridgeSession{ctx: context.Background(), hubTaskIDs: []agenthub.TaskID{task.ID}}
	m.finishHubTask(bs, &events.AgentEnd{RunId: "r-abort", FinishReason: core.FinishReasonAbort})
	st, _ := m.hubStore()
	got, err := st.LoadTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agenthub.TaskInterrupted {
		t.Fatalf("status = %s, want interrupted", got.Status)
	}
}

// P2-04 复审 B3：插话路径消费到的 hub_task 必须 failed + 告警，不能静默停在 dispatched。
func TestHubInjectedHubTaskFails(t *testing.T) {
	m, buf := testManager(t)
	ws := t.TempDir()
	bs, err := m.create(ws, "s-injected", "")
	if err != nil {
		t.Fatal(err)
	}
	task := newDispatchedHubTask(t, m)
	task.Target.Workspace = ws
	task.Target.SessionID = bs.id
	header, err := agenthub.EncodeTaskHeader(task)
	if err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	bs.emit(&events.UserInputsConsumed{
		Injected:     true,
		UserContents: [][]core.Content{{{Type: core.ContentTypeHubTask, Content: header}}},
		EventType:    events.UserInputsConsumedType, Timestamp: time.Now(),
	})
	st, _ := m.hubStore()
	got, err := st.LoadTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agenthub.TaskFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
	if !strings.Contains(buf.String(), "injected_conflict") {
		t.Fatalf("missing alert: %s", buf.String())
	}
	// 坏 header：告警但不结算。
	buf.Reset()
	bs.emit(&events.UserInputsConsumed{
		Injected:     false,
		UserContents: [][]core.Content{{{Type: core.ContentTypeHubTask, Content: "{bad"}}},
		EventType:    events.UserInputsConsumedType, Timestamp: time.Now(),
	})
	if !strings.Contains(buf.String(), "bad_header") {
		t.Fatalf("missing bad_header alert: %s", buf.String())
	}
}

// P2-04 复审 B1：普通工具结果恒为空 Usage，累加后不能抹掉本 run 的有价性；
// taskUsageOf 必须给预算判定一个 CostPriced=true。
func TestTaskUsageOfPreservesPricedAcrossEmptyToolRounds(t *testing.T) {
	var u core.Usage
	u = u.Add(core.Usage{Input: 10, Cost: core.Cost{Total: 100, Priced: true, Source: core.CostSourceBuiltin}})
	u = u.Add(core.Usage{}) // 普通工具（bash/read/...）的 Usage 恒零值
	u = u.Add(core.Usage{Input: 5, Cost: core.Cost{Total: 50, Priced: true, Source: core.CostSourceBuiltin}})
	got := taskUsageOf(&u)
	if !got.CostPriced || got.CostUSD != 0.00015 || got.Input != 15 {
		t.Fatalf("taskUsageOf = %+v, want priced $0.00015 input=15", got)
	}
}

// P2-04 复审 NB-4：轮数护栏截断没有最终答复，必须 failed，不能 completed。
func TestHubTaskFinishMaxIterationsIsFailed(t *testing.T) {
	m, _ := testManager(t)
	task := newDispatchedHubTask(t, m)
	bs := &bridgeSession{ctx: context.Background(), hubTaskIDs: []agenthub.TaskID{task.ID}}
	m.finishHubTask(bs, &events.AgentEnd{RunId: "r-maxiter", FinishReason: core.FinishReasonMaxIterations})
	st, _ := m.hubStore()
	got, err := st.LoadTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agenthub.TaskFailed {
		t.Fatalf("status = %s, want failed", got.Status)
	}
}
