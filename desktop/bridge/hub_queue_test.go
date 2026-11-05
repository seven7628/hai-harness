package main

// P2-03：SessionStatus 探针 + Hub 队列在 bridge 侧的接线。

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/events"
)

func TestHubWorkspaceSessionStatus(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	p := createWorkspaceAgent(t, m, buf, 1, "StatusProbe", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	bs, err := m.create(wsPath, "s-status", "")
	if err != nil {
		t.Fatal(err)
	}
	ops := newHubWorkspaceOps(m)
	info, err := ops.SessionStatus(context.Background(), wsPath, "s-status")
	if err != nil || info.Running || info.Pending != 0 {
		t.Fatalf("idle status = %+v, %v", info, err)
	}
	bs.running.Store(true)
	info, err = ops.SessionStatus(context.Background(), wsPath, "s-status")
	if err != nil || !info.Running {
		t.Fatalf("running status = %+v, %v", info, err)
	}
	bs.running.Store(false)
	if _, err := ops.SessionStatus(context.Background(), wsPath, "nope"); err == nil {
		t.Fatal("unknown session must be ErrNotFound")
	}
	// ListSessions 也带 pending 字段（前端任务板/闸门共用）。
	rows, err := ops.ListSessions(context.Background(), wsPath)
	if err != nil || len(rows) != 1 {
		t.Fatalf("list = %+v, %v", rows, err)
	}
	_ = agenthub.AgentID(p.ID)
}

// session_send 走队列：目标 running → queued；空闲后 pump 派发一次。
func TestHubSessionSendQueuesWhileTargetRunning(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	p := createWorkspaceAgent(t, m, buf, 1, "QueueAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	_, _, hubData := hubCmd(t, m, buf, 2, "hub_agent_create", map[string]any{
		"profile": map[string]any{"name": "QueueHub", "provider": "deepseek", "model": "deepseek-v4-flash", "role": "派发"},
	})
	hubAgent := agentFromData(t, hubData)
	hubBS, err := m.hubDMSession(string(hubAgent.ID))
	if err != nil {
		t.Fatal(err)
	}
	target, err := m.create(wsPath, "s-queue", "")
	if err != nil {
		t.Fatal(err)
	}
	target.running.Store(true) // 模拟目标正在跑

	// 直接用 Hub 会话引擎的 session_send（buildHubEngine 里已接 Queue）。
	tool, err := hubBS.hubEngine.GetTool(context.Background(), "session_send")
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Call(context.Background(), "session_send", `{"workspace":"`+wsPath+`","session_id":"s-queue","text":"排队任务"}`)
	if err != nil {
		t.Fatalf("session_send: %v", err)
	}
	var sent struct {
		TaskID string `json:"task_id"`
		Status string `json:"status"`
		Queued bool   `json:"queued"`
	}
	if json.Unmarshal([]byte(out), &sent) != nil || !sent.Queued || sent.Status != string(agenthub.TaskQueued) || sent.TaskID == "" {
		t.Fatalf("send while running = %s", out)
	}
	q, err := m.hubQueueFor()
	if err != nil {
		t.Fatal(err)
	}
	if got := q.QueuedCount(wsPath, "s-queue"); got != 1 {
		t.Fatalf("queued count = %d, want 1", got)
	}
	// 目标空闲 + AgentEnd 泵：派发队首（任务离开队列）。
	target.running.Store(false)
	m.pumpHubQueue(wsPath, "s-queue")
	if got := q.QueuedCount(wsPath, "s-queue"); got != 0 {
		t.Fatalf("after pump queued = %d, want 0", got)
	}
	_ = p
}

// P2-03 B1：状态点 only 由根 run 翻转 —— 子 agent AgentEnd 不能让闸门误判空闲。
func TestHubSessionStatusSubagentEndKeepsBusy(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	createWorkspaceAgent(t, m, buf, 1, "StatusSubagent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	bs, err := m.create(wsPath, "s-sub", "")
	if err != nil {
		t.Fatal(err)
	}
	ops := newHubWorkspaceOps(m)
	bs.emit(&events.AgentStart{RunId: "root", Timestamp: time.Now(), EventType: events.AgentStartType})
	if info, _ := ops.SessionStatus(context.Background(), wsPath, "s-sub"); !info.Running {
		t.Fatal("根 run 启动后应为 busy")
	}
	// 子 agent 结束（ParentRunId 非空）：根 run 仍在跑，闸门必须仍 busy。
	bs.emit(&events.AgentEnd{RunId: "root", ParentRunId: "root", Depth: 1,
		Timestamp: time.Now(), EventType: events.AgentEndType})
	if !bs.running.Load() {
		t.Fatal("子 agent AgentEnd 把状态点清零了")
	}
	if info, _ := ops.SessionStatus(context.Background(), wsPath, "s-sub"); !info.Running {
		t.Fatal("子 agent AgentEnd 后闸门误判空闲")
	}
	// 根 AgentEnd 才清零。
	bs.emit(&events.AgentEnd{RunId: "root", Timestamp: time.Now(), EventType: events.AgentEndType})
	if info, _ := ops.SessionStatus(context.Background(), wsPath, "s-sub"); info.Running {
		t.Fatal("根 AgentEnd 后应为空闲")
	}
}

// P2-03 B1：已 Ask、根 AgentStart 未到的窗口（takeBatch→AgentStart）必须算 busy。
func TestHubDispatchPendingGatesDispatch(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	createWorkspaceAgent(t, m, buf, 1, "StatusPending", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	bs, err := m.create(wsPath, "s-pend", "")
	if err != nil {
		t.Fatal(err)
	}
	ops := newHubWorkspaceOps(m)
	bs.hubDispatchPending.Store(true)
	if info, _ := ops.SessionStatus(context.Background(), wsPath, "s-pend"); !info.Running {
		t.Fatal("hubDispatchPending 必须让闸门读成 busy")
	}
	bs.emit(&events.AgentStart{RunId: "root", Timestamp: time.Now(), EventType: events.AgentStartType})
	if bs.hubDispatchPending.Load() || !bs.running.Load() {
		t.Fatal("根 AgentStart 后应由 running 表达忙")
	}
	bs.emit(&events.AgentEnd{RunId: "root", Timestamp: time.Now(), EventType: events.AgentEndType})
	if info, _ := ops.SessionStatus(context.Background(), wsPath, "s-pend"); info.Running {
		t.Fatal("根 AgentEnd 后应为空闲")
	}
}

// P2-03 M5：emit 接线必须真的在根 AgentEnd 后泵队列（不是只有测试直接调 pump）。
func TestHubAgentEndPumpsQueuedTask(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	createWorkspaceAgent(t, m, buf, 1, "PumpWiring", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	bs, err := m.create(wsPath, "s-pump", "")
	if err != nil {
		t.Fatal(err)
	}
	q, err := m.hubQueueFor()
	if err != nil {
		t.Fatal(err)
	}
	ops := newHubWorkspaceOps(m)
	bs.running.Store(true) // 目标忙 → 任务入队
	if _, err := q.Submit(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: "ag_pump"},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: wsPath, SessionID: "s-pump"},
		Title:  "队列任务", Instruction: "做一件事", Callback: agenthub.CallbackAgentEnd,
	}); err != nil {
		t.Fatal(err)
	}
	if got := q.QueuedCount(wsPath, "s-pump"); got != 1 {
		t.Fatalf("queued = %d, want 1", got)
	}
	// 只结束子 agent：不应泵（根 run 还在跑，闸门也仍 busy）。
	bs.emit(&events.AgentEnd{RunId: "root", ParentRunId: "root", Depth: 1,
		Timestamp: time.Now(), EventType: events.AgentEndType})
	if got := q.QueuedCount(wsPath, "s-pump"); got != 1 {
		t.Fatalf("子 agent AgentEnd 泵了队列: queued = %d", got)
	}
	// 根 AgentEnd：emit 内部泵走队首（闸门空闲）。
	bs.emit(&events.AgentEnd{RunId: "root", Timestamp: time.Now(), EventType: events.AgentEndType})
	if got := q.QueuedCount(wsPath, "s-pump"); got != 0 {
		t.Fatalf("根 AgentEnd 后 queued = %d, want 0", got)
	}
	if ops == nil {
		t.Fatal("unreachable")
	}
}

// P2-02 复审 B2：生产链路（Hub engine 的 session_send → Service → Queue）同
// client_msg_id 必须返回同一账本任务，不能建两条。
func TestHubSessionSendClientMsgIDIdempotent(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	createWorkspaceAgent(t, m, buf, 1, "DedupAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	_, _, hubData := hubCmd(t, m, buf, 2, "hub_agent_create", map[string]any{
		"profile": map[string]any{"name": "DedupHub", "provider": "deepseek", "model": "deepseek-v4-flash", "role": "派发"},
	})
	hubAgent := agentFromData(t, hubData)
	hubBS, err := m.hubDMSession(string(hubAgent.ID))
	if err != nil {
		t.Fatal(err)
	}
	target, err := m.create(wsPath, "s-dedup", "")
	if err != nil {
		t.Fatal(err)
	}
	target.running.Store(true) // 目标忙：两条都留在队列里
	tool, err := hubBS.hubEngine.GetTool(context.Background(), "session_send")
	if err != nil {
		t.Fatal(err)
	}
	call := func(id int, text string) (taskID string, queued bool) {
		t.Helper()
		out, err := tool.Call(context.Background(), "session_send", fmt.Sprintf(
			`{"workspace":%q,"session_id":"s-dedup","text":%q,"client_msg_id":"prod-key-1"}`, wsPath, text))
		if err != nil {
			t.Fatalf("session_send #%d: %v", id, err)
		}
		var got struct {
			TaskID string `json:"task_id"`
			Queued bool   `json:"queued"`
		}
		if err := json.Unmarshal([]byte(out), &got); err != nil {
			t.Fatalf("send out = %s", out)
		}
		return got.TaskID, got.Queued
	}
	first, queued := call(1, "第一次")
	if !queued || first == "" {
		t.Fatalf("first = %q queued=%v", first, queued)
	}
	second, _ := call(2, "重试")
	if second != first {
		t.Fatalf("same client_msg_id returned different tasks: %s vs %s", first, second)
	}
	q, err := m.hubQueueFor()
	if err != nil {
		t.Fatal(err)
	}
	if got := q.QueuedCount(wsPath, "s-dedup"); got != 1 {
		t.Fatalf("queued = %d, want 1", got)
	}
	if st, _ := m.hubStore(); st != nil {
		task, err := st.LoadTask(context.Background(), agenthub.TaskID(first))
		if err != nil {
			t.Fatal(err)
		}
		if task.ClientMsgID != "prod-key-1" {
			t.Fatalf("ledger client_msg_id = %q", task.ClientMsgID)
		}
	}
}

// P2-02 复审 N4：session_list 的 updated_at 必须真的有值（不是恒 nil/0001）。
func TestHubWorkspaceListSessionsUpdatedAt(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	createWorkspaceAgent(t, m, buf, 1, "UpdatedAtAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	bs, err := m.create(wsPath, "s-updated", "")
	if err != nil {
		t.Fatal(err)
	}
	bs.lastActive.Store(time.Now().UnixMilli())
	rows, err := newHubWorkspaceOps(m).ListSessions(context.Background(), wsPath)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows = %+v, %v", rows, err)
	}
	if rows[0].UpdatedAt == nil || rows[0].UpdatedAt.IsZero() {
		t.Fatalf("updated_at = %v, want non-zero", rows[0].UpdatedAt)
	}
}

// P2-02 复审 B1 残留：空标题也要落盘，create 与 list 一致（defaultSessionTitle）。
func TestHubWorkspaceCreateDefaultTitleMatchesList(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	createWorkspaceAgent(t, m, buf, 1, "TitleAgent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	ops := newHubWorkspaceOps(m)
	info, err := ops.CreateSession(context.Background(), agenthub.CreateSessionReq{Workspace: wsPath, Title: "   "})
	if err != nil {
		t.Fatal(err)
	}
	if info.Title != defaultSessionTitle {
		t.Fatalf("create title = %q, want %q", info.Title, defaultSessionTitle)
	}
	rows, err := ops.ListSessions(context.Background(), wsPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rows {
		if r.ID == info.ID && r.Title != info.Title {
			t.Fatalf("list title = %q, create = %q", r.Title, info.Title)
		}
	}
	// 超长标题截到 80 rune。
	long, err := ops.CreateSession(context.Background(), agenthub.CreateSessionReq{Workspace: wsPath, Title: strings.Repeat("长", 300)})
	if err != nil {
		t.Fatal(err)
	}
	if len([]rune(long.Title)) != 80 {
		t.Fatalf("long title len = %d, want 80", len([]rune(long.Title)))
	}
}
