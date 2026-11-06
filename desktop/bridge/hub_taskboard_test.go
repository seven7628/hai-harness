package main

// P2-06：任务板（列表/详情/取消/重试）与 Needs you（审批/提问落库、决策、过期）。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

func TestHubTaskBoardLifecycle(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	createWorkspaceAgent(t, m, buf, 1, "BoardAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	_, _, hubData := hubCmd(t, m, buf, 2, "hub_agent_create", map[string]any{
		"profile": map[string]any{"name": "BoardHub", "provider": "deepseek", "model": "deepseek-v4-flash", "role": "派发"},
	})
	hubAgent := agentFromData(t, hubData)
	hubBS, err := m.hubDMSession(string(hubAgent.ID))
	if err != nil {
		t.Fatal(err)
	}
	target, err := m.create(wsPath, "s-board", "")
	if err != nil {
		t.Fatal(err)
	}
	target.running.Store(true)
	tool, err := hubBS.hubEngine.GetTool(context.Background(), "session_send")
	if err != nil {
		t.Fatal(err)
	}
	send := func(text string) string {
		t.Helper()
		out, err := tool.Call(context.Background(), "session_send",
			`{"workspace":`+jsonString(wsPath)+`,"session_id":"s-board","text":`+jsonString(text)+`}`)
		if err != nil {
			t.Fatal(err)
		}
		var got struct {
			TaskID string `json:"task_id"`
		}
		if json.Unmarshal([]byte(out), &got) != nil || got.TaskID == "" {
			t.Fatalf("send = %s", out)
		}
		return got.TaskID
	}
	first := send("任务一")
	second := send("任务二")

	ok, errMsg, data := hubCmd(t, m, buf, 3, "hub_task_list", map[string]any{"status": "queued"})
	if !ok {
		t.Fatalf("task_list: %s", errMsg)
	}
	var list struct {
		Tasks []*agenthub.Task `json:"tasks"`
	}
	raw, _ := json.Marshal(data)
	_ = json.Unmarshal(raw, &list)
	if len(list.Tasks) != 2 {
		t.Fatalf("task_list count = %d, want 2 (%s)", len(list.Tasks), raw)
	}

	ok, errMsg, data = hubCmd(t, m, buf, 4, "hub_task_get", map[string]any{"task_id": first})
	if !ok {
		t.Fatalf("task_get: %s", errMsg)
	}
	var detail struct {
		Task   *agenthub.Task       `json:"task"`
		Events []agenthub.TaskEvent `json:"events"`
	}
	raw, _ = json.Marshal(data)
	_ = json.Unmarshal(raw, &detail)
	if detail.Task == nil || detail.Task.ID != agenthub.TaskID(first) || len(detail.Events) == 0 {
		t.Fatalf("detail = %s", raw)
	}

	ok, errMsg, data = hubCmd(t, m, buf, 5, "hub_task_cancel", map[string]any{"task_id": first})
	if !ok {
		t.Fatalf("cancel: %s", errMsg)
	}
	var canceled struct {
		Task *agenthub.Task `json:"task"`
	}
	raw, _ = json.Marshal(data)
	_ = json.Unmarshal(raw, &canceled)
	if canceled.Task == nil || canceled.Task.Status != agenthub.TaskCanceled {
		t.Fatalf("canceled = %s", raw)
	}
	q, err := m.hubQueueFor()
	if err != nil {
		t.Fatal(err)
	}
	if got := q.QueuedCount(wsPath, "s-board"); got != 1 {
		t.Fatalf("queued after cancel = %d, want 1", got)
	}

	ok, errMsg, data = hubCmd(t, m, buf, 6, "hub_task_retry", map[string]any{"task_id": first, "instruction": "再做一次"})
	if !ok {
		t.Fatalf("retry: %s", errMsg)
	}
	var retried struct {
		Task *agenthub.Task `json:"task"`
	}
	raw, _ = json.Marshal(data)
	_ = json.Unmarshal(raw, &retried)
	if retried.Task == nil || retried.Task.ID == agenthub.TaskID(first) {
		t.Fatalf("retried = %s", raw)
	}
	if retried.Task.ParentTaskID != agenthub.TaskID(first) || retried.Task.RootTaskID != agenthub.TaskID(first) {
		t.Fatalf("chain = parent %s root %s", retried.Task.ParentTaskID, retried.Task.RootTaskID)
	}
	if retried.Task.Attempt != 2 || retried.Task.Instruction != "再做一次" {
		t.Fatalf("retry task = %+v", retried.Task)
	}
	if got := q.QueuedCount(wsPath, "s-board"); got != 2 {
		t.Fatalf("queued after retry = %d, want 2", got)
	}
	// hub_clear：清空 DM 上下文（保留 system）。
	ok, errMsg, data = hubCmd(t, m, buf, 7, "hub_clear", map[string]any{"target": map[string]any{"kind": "dm", "agent_id": string(hubAgent.ID)}})
	if !ok {
		t.Fatalf("hub_clear: %s", errMsg)
	}
	var cleared struct {
		SessionID string `json:"session_id"`
	}
	raw, _ = json.Marshal(data)
	_ = json.Unmarshal(raw, &cleared)
	if cleared.SessionID != hubBS.id {
		t.Fatalf("cleared = %s", raw)
	}
	_ = second
}

func TestHubNeedFromEventsAndResolve(t *testing.T) {
	m, buf := testManager(t)
	hubAgent := createHubAgent(t, m, buf, 1, "NeedHub")
	bs, err := m.hubDMSession(string(hubAgent.ID))
	if err != nil {
		t.Fatal(err)
	}
	// 审批事件 → need（幂等：重复事件不重建）。
	approval := &events.ToolApprovalRequested{
		RunId: "r1", Id: "call-1", Name: "bash", Arguments: `{"command":"ls"}`,
		Timestamp: time.Now(), EventType: events.ToolApprovalType,
	}
	bs.emit(approval)
	bs.emit(approval)
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	needs, err := st.ListNeeds(context.Background(), agenthub.NeedFilter{Status: "pending"})
	if err != nil || len(needs) != 1 {
		t.Fatalf("needs = %+v, %v", needs, err)
	}
	if needs[0].Kind != "approval" || needs[0].Anchor != "call-1" || needs[0].SessionID != bs.id {
		t.Fatalf("need = %+v", needs[0])
	}
	// 提问事件 → 第二条 need；根 AgentEnd 把它们标 expired。
	q := &events.AskUserQuestion{RunId: "r1", Id: "batch-1", Timestamp: time.Now(), EventType: events.AskUserQuestionType}
	bs.emit(q)
	needs, _ = st.ListNeeds(context.Background(), agenthub.NeedFilter{Status: "pending"})
	if len(needs) != 2 {
		t.Fatalf("pending = %d, want 2", len(needs))
	}
	bs.emit(&events.AgentEnd{RunId: "r1", Timestamp: time.Now(), EventType: events.AgentEndType})
	// 过期是异步的（不能阻塞 emit 交还 run goroutine）：轮询到 pending 清空。
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		needs, _ = st.ListNeeds(context.Background(), agenthub.NeedFilter{Status: "pending"})
		if len(needs) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(needs) != 0 {
		t.Fatalf("after AgentEnd pending = %d, want 0", len(needs))
	}
	expired, _ := st.ListNeeds(context.Background(), agenthub.NeedFilter{Status: "expired"})
	if len(expired) != 2 {
		t.Fatalf("expired = %d, want 2", len(expired))
	}

	// hub_need_list 默认 pending：为空；failure need 的 resolve 幂等。
	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_need_list", map[string]any{})
	if !ok {
		t.Fatalf("need_list: %s", errMsg)
	}
	var listed struct {
		Needs []*agenthub.Need `json:"needs"`
	}
	raw, _ := json.Marshal(data)
	_ = json.Unmarshal(raw, &listed)
	if len(listed.Needs) != 0 {
		t.Fatalf("pending list = %s", raw)
	}
	fail := &agenthub.Need{ID: "need_failure_1", Kind: "failure", Status: "pending", Title: "任务失败", CreatedAt: time.Now().UTC()}
	if err := st.SaveNeed(context.Background(), fail); err != nil {
		t.Fatal(err)
	}
	ok, errMsg, data = hubCmd(t, m, buf, 3, "hub_need_resolve", map[string]any{"need_id": string(fail.ID), "resolution": "acknowledged"})
	if !ok {
		t.Fatalf("resolve: %s", errMsg)
	}
	got, err := st.LoadNeed(context.Background(), fail.ID)
	if err != nil || got.Status != "resolved" || got.ResolvedAt == nil {
		t.Fatalf("resolved need = %+v, %v", got, err)
	}
	ok, errMsg, data = hubCmd(t, m, buf, 4, "hub_need_resolve", map[string]any{"need_id": string(fail.ID), "resolution": "again"})
	if !ok {
		t.Fatalf("repeat resolve: %s", errMsg)
	}
	var again struct {
		Already bool `json:"already"`
	}
	raw, _ = json.Marshal(data)
	_ = json.Unmarshal(raw, &again)
	if !again.Already {
		t.Fatalf("repeat resolve = %s", raw)
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// P2-06 B1：Hub 智能体 DM 会话的审批 need 必须能 resolve（sessionByID 要扫 hubRuntimes）。
func TestHubNeedResolveOnHubDMSession(t *testing.T) {
	m, buf := testManager(t)
	hubAgent := createHubAgent(t, m, buf, 1, "NeedHubDM")
	bs, err := m.hubDMSession(string(hubAgent.ID))
	if err != nil {
		t.Fatal(err)
	}
	call := core.ToolCall{Id: "call-hub-1", Name: "bash", Arguments: `{"command":"ls"}`}
	if _, err := bs.s.RequestApproval(context.Background(), call); err != nil {
		t.Fatalf("register approval: %v", err)
	}
	bs.emit(&events.ToolApprovalRequested{
		RunId: "r1", Id: "call-hub-1", Name: "bash", Arguments: `{"command":"ls"}`,
		Timestamp: time.Now(), EventType: events.ToolApprovalType,
	})
	st, _ := m.hubStore()
	needs, err := st.ListNeeds(context.Background(), agenthub.NeedFilter{Status: "pending"})
	if err != nil || len(needs) != 1 {
		t.Fatalf("needs = %+v, %v", needs, err)
	}
	ok, errMsg, _ := hubCmd(t, m, buf, 2, "hub_need_resolve", map[string]any{
		"need_id": string(needs[0].ID), "resolution": "approve",
	})
	if !ok {
		t.Fatalf("resolve hub DM approval: %s", errMsg)
	}
	got, err := st.LoadNeed(context.Background(), needs[0].ID)
	if err != nil || got.Status != "resolved" {
		t.Fatalf("need = %+v, %v", got, err)
	}
}

// P2-06 B2：会话存在但空闲时取消 dispatched/running，必须落 canceled，
// 不能回「已请求中断」然后永远停在运行中。
func TestHubTaskCancelIdleSessionIsTerminal(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	createWorkspaceAgent(t, m, buf, 1, "CancelIdleAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	if _, err := m.create(wsPath, "s-idle", ""); err != nil {
		t.Fatal(err)
	}
	st, _ := m.hubStore()
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: "ag_idle"},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: wsPath, SessionID: "s-idle"},
		Title:  "空闲任务", Instruction: "x", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{}); err != nil {
		t.Fatal(err)
	}
	ok, errMsg, _ := hubCmd(t, m, buf, 2, "hub_task_cancel", map[string]any{"task_id": string(task.ID)})
	if !ok {
		t.Fatalf("cancel: %s", errMsg)
	}
	got, err := st.LoadTask(context.Background(), task.ID)
	if err != nil || got.Status != agenthub.TaskCanceled {
		t.Fatalf("status = %v, err=%v; want canceled", got.Status, err)
	}
}

// P2-06 B3：resolve 与异步 expire 并发时，不能出现「resolve 返回 ok，但
// 最终落盘被 expired 覆盖」。按 need 串行化后：先到者赢，后到者 already/已过期。
func TestHubNeedResolveExpireNoLostUpdate(t *testing.T) {
	m, buf := testManager(t)
	hubAgent := createHubAgent(t, m, buf, 1, "NeedRace")
	bs, err := m.hubDMSession(string(hubAgent.ID))
	if err != nil {
		t.Fatal(err)
	}
	st, _ := m.hubStore()
	for i := 0; i < 12; i++ {
		id := fmt.Sprintf("call-race-%d", i)
		call := core.ToolCall{Id: id, Name: "bash", Arguments: "{}"}
		if _, err := bs.s.RequestApproval(context.Background(), call); err != nil {
			t.Fatalf("round %d register: %v", i, err)
		}
		bs.emit(&events.ToolApprovalRequested{
			RunId: "r1", Id: id, Name: "bash", Arguments: "{}",
			Timestamp: time.Now(), EventType: events.ToolApprovalType,
		})
		needs, err := st.ListNeeds(context.Background(), agenthub.NeedFilter{Status: "pending"})
		if err != nil {
			t.Fatal(err)
		}
		var need *agenthub.Need
		for _, n := range needs {
			if n.Anchor == id {
				need = n
			}
		}
		if need == nil {
			t.Fatalf("round %d need not found", i)
		}
		done := make(chan struct{})
		go func() { m.expireSessionNeeds(bs); close(done) }()
		ok, errMsg, data := hubCmd(t, m, buf, 100+i, "hub_need_resolve", map[string]any{
			"need_id": string(need.ID), "resolution": "approve",
		})
		<-done
		if !ok {
			t.Fatalf("round %d resolve: %s", i, errMsg)
		}
		var resp struct {
			Already bool `json:"already"`
		}
		raw, _ := json.Marshal(data)
		_ = json.Unmarshal(raw, &resp)
		got, err := st.LoadNeed(context.Background(), need.ID)
		if err != nil {
			t.Fatal(err)
		}
		if !resp.Already && got.Status != "resolved" {
			t.Fatalf("round %d: resolve applied but final status=%s (lost update)", i, got.Status)
		}
		if resp.Already && got.Status != "expired" && got.Status != "resolved" {
			t.Fatalf("round %d: already but status=%s", i, got.Status)
		}
		// 若 expire 赢了，审批 waiter 仍挂着；清掉以免阻塞下一轮。
		_ = bs.s.Approve(id, false)
	}
}

// P2-06 复审 B-2：根 AgentEnd 已到、结算未完成时，取消不能把已完成任务抢成
// canceled（否则迟到结算只能静默丢结果）。用真实 emit 链路 + 占住 hubFinishMu
// 放大结算窗口；读取 running=false 后取消，期望「已请求中断」且任务最终 completed。
func TestHubTaskCancelDuringSettlementDoesNotStealResult(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	createWorkspaceAgent(t, m, buf, 1, "SettleAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	bs, err := m.create(wsPath, "s-settle", "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: "ag_settle"},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: wsPath, SessionID: "s-settle"},
		Title:  "结算窗口", Instruction: "x", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{}); err != nil {
		t.Fatal(err)
	}
	bs.hubTaskIDs = []agenthub.TaskID{task.ID}
	bs.running.Store(true)

	// 占住结算锁：emit 会先清 running（暴露结算窗口），再在 finishHubTask 处等待。
	m.hubFinishMu.Lock()
	emitDone := make(chan struct{})
	go func() {
		bs.emit(&events.AgentEnd{
			EventType: events.AgentEndType, RunId: "r-settle", Content: "完成",
		})
		close(emitDone)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for bs.running.Load() {
		if time.Now().After(deadline) {
			m.hubFinishMu.Unlock()
			t.Fatal("emit 未在 3s 内清掉 running")
		}
		time.Sleep(2 * time.Millisecond)
	}

	ok, errMsg, data := hubCmd(t, m, buf, 2, "hub_task_cancel", map[string]any{"task_id": string(task.ID)})
	if !ok {
		m.hubFinishMu.Unlock()
		t.Fatalf("cancel: %s", errMsg)
	}
	note := fmt.Sprint(data["note"])
	if !strings.Contains(note, "正在结算") {
		m.hubFinishMu.Unlock()
		t.Fatalf("cancel during settlement note = %q; want 结果保留提示", note)
	}
	mid, _ := st.LoadTask(context.Background(), task.ID)
	if mid.Status == agenthub.TaskCanceled {
		m.hubFinishMu.Unlock()
		t.Fatal("结算窗口内已完成的任务被 cancel 抢成 canceled")
	}

	m.hubFinishMu.Unlock()
	select {
	case <-emitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("emit 结算未在 5s 内完成")
	}
	got, err := st.LoadTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agenthub.TaskCompleted {
		t.Fatalf("status = %s, want completed (结算不能被迟到 cancel 改写)", got.Status)
	}
	for _, ev := range readLines(buf) {
		if ev["event_type"] == "hub_alert" && ev["kind"] == "settle_failed" {
			t.Fatalf("settlement produced settle_failed: %v", ev)
		}
	}
}

// P2-06 复审 B-3（REG-4）：cancel 之后迟到的 AgentEnd 不得再结算、不得发
// settle_failed 假告警（人已取消，迟到结算是预期内的）。
func TestHubTaskLateAgentEndAfterCancelIsQuiet(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	createWorkspaceAgent(t, m, buf, 1, "LateAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	bs, err := m.create(wsPath, "s-late", "")
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: "ag_late"},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: wsPath, SessionID: "s-late"},
		Title:  "已取消", Instruction: "x", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskCanceled, agenthub.TransitionOptions{Error: "用户取消"}); err != nil {
		t.Fatal(err)
	}
	bs.hubTaskIDs = []agenthub.TaskID{task.ID}
	m.finishHubTask(bs, &events.AgentEnd{
		EventType: events.AgentEndType, RunId: "r-late",
		FinishReason: core.FinishReasonAbort, ErrorKind: "canceled",
	})
	got, err := st.LoadTask(context.Background(), task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agenthub.TaskCanceled {
		t.Fatalf("status = %s, want canceled", got.Status)
	}
	for _, ev := range readLines(buf) {
		if ev["event_type"] == "hub_alert" && ev["kind"] == "settle_failed" {
			t.Fatalf("late AgentEnd produced settle_failed: %v", ev)
		}
	}
}

// P2-06 复审 B-2：真正在跑的会话不能被「空闲兜底」直接取消。
func TestHubTaskCancelRunningSessionIsNotCanceled(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	createWorkspaceAgent(t, m, buf, 1, "CancelRunningAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	bs, err := m.create(wsPath, "s-run", "")
	if err != nil {
		t.Fatal(err)
	}
	bs.running.Store(true)
	st, _ := m.hubStore()
	ledger := agenthub.NewLedger(st)
	task, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: "ag_run"},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: wsPath, SessionID: "s-run"},
		Title:  "运行中", Instruction: "x", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), task.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{}); err != nil {
		t.Fatal(err)
	}
	ok, errMsg, _ := hubCmd(t, m, buf, 2, "hub_task_cancel", map[string]any{"task_id": string(task.ID)})
	if !ok {
		t.Fatalf("cancel: %s", errMsg)
	}
	got, _ := st.LoadTask(context.Background(), task.ID)
	if got.Status == agenthub.TaskCanceled {
		t.Fatal("running session was wrongly canceled by idle fallback")
	}
	if got.Status != agenthub.TaskDispatched {
		t.Fatalf("status = %s, want dispatched (interrupt requested)", got.Status)
	}
	bs.running.Store(false)
	bs.hubDispatchPending.Store(false)
	ok, errMsg, _ = hubCmd(t, m, buf, 3, "hub_task_cancel", map[string]any{"task_id": string(task.ID)})
	if !ok {
		t.Fatalf("cancel idle: %s", errMsg)
	}
	got, _ = st.LoadTask(context.Background(), task.ID)
	if got.Status != agenthub.TaskCanceled {
		t.Fatalf("status = %s, want canceled after idle", got.Status)
	}
}

// P3a-07 B4：审批/提问 need 超过 30 分钟未处理，hub_need_list 惰性置 expired；
// 交接/失败不自动过期。
func TestHubNeedLazyExpiry(t *testing.T) {
	m, buf := testManager(t)
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	old := &agenthub.Need{
		ID: "need_old_approval", Kind: "approval", Status: "pending",
		GroupID: "grp_ttl", CreatedAt: now.Add(-31 * time.Minute), Title: "旧审批",
	}
	fresh := &agenthub.Need{
		ID: "need_fresh_approval", Kind: "approval", Status: "pending",
		GroupID: "grp_ttl", CreatedAt: now.Add(-29 * time.Minute), Title: "新审批",
	}
	handoff := &agenthub.Need{
		ID: "need_old_handoff", Kind: "handoff", Status: "pending",
		GroupID: "grp_ttl", CreatedAt: now.Add(-2 * time.Hour), Title: "旧交接",
	}
	for _, n := range []*agenthub.Need{old, fresh, handoff} {
		if err := st.SaveNeed(context.Background(), n); err != nil {
			t.Fatal(err)
		}
	}
	ok, errMsg, data := hubCmd(t, m, buf, 1, "hub_need_list", map[string]any{"group_id": "grp_ttl"})
	if !ok {
		t.Fatalf("need list: %s", errMsg)
	}
	var out struct {
		Needs []*agenthub.Need `json:"needs"`
	}
	raw, _ := json.Marshal(data)
	_ = json.Unmarshal(raw, &out)
	if len(out.Needs) != 2 {
		t.Fatalf("pending = %s, want fresh+handoff", raw)
	}
	got, _ := st.LoadNeed(context.Background(), old.ID)
	if got.Status != "expired" || got.ResolvedAt == nil {
		t.Fatalf("old approval = %+v", got)
	}
	if got, _ := st.LoadNeed(context.Background(), fresh.ID); got.Status != "pending" {
		t.Fatalf("fresh approval = %+v", got)
	}
	if got, _ := st.LoadNeed(context.Background(), handoff.ID); got.Status != "pending" {
		t.Fatalf("handoff = %+v", got)
	}
	// 决策入口也做 TTL：列表没打开时也不能批准已过期审批。
	ok, _, data = hubCmd(t, m, buf, 2, "hub_need_resolve", map[string]any{
		"need_id": string(old.ID), "resolution": "approve",
	})
	if !ok {
		t.Fatalf("resolve expired: %s", errMsg)
	}
	var already struct {
		Already bool `json:"already"`
	}
	raw, _ = json.Marshal(data)
	_ = json.Unmarshal(raw, &already)
	if !already.Already {
		t.Fatalf("expired need was approved: %s", raw)
	}
}
