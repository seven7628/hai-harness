package main

// P2-06：任务板（列表/详情/取消/重试）与 Needs you（审批/提问落库、决策、过期）。

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
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
