package main

// P2-03：SessionStatus 探针 + Hub 队列在 bridge 侧的接线。

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/seven7628/hai-harness/agenthub"
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
