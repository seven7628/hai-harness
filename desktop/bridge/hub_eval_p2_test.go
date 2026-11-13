package main

// P2-09：评测 4–6（方案 §10.3）。
//
//  4. 一句话建工作区派任务：workspace_create → session_create → session_send，
//     目标任务真实跑完，账本 completed→delivered，发起者收到 task_result；
//  5. 人工介入不续跑：人取消排队任务后不自动执行、不自动重试（重试是显式动作）；
//  6. 重启可解释：无 run 的任务标 interrupted + Needs you，未回推的 completed 重推一次。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agenthub"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

func TestEval4OneShotWorkspaceTask(t *testing.T) {
	m, buf := testManager(t)
	parent := t.TempDir()
	ok, errMsg, data := hubCmd(t, m, buf, 1, "hub_agent_create", map[string]any{
		"profile": map[string]any{
			"name": "Eval4Hub", "provider": "deepseek", "model": "deepseek-v4-flash", "role": "派发",
			"policy": map[string]any{"workspace_roots": []string{parent}},
		},
	})
	if !ok {
		t.Fatalf("create hub agent: %s", errMsg)
	}
	hubAgent := agentFromData(t, data)
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	hubBS, err := m.hubDMSession(string(hubAgent.ID))
	if err != nil {
		t.Fatal(err)
	}
	call := func(name string, args any) string {
		t.Helper()
		tool, err := hubBS.hubEngine.GetTool(context.Background(), name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		raw, _ := json.Marshal(args)
		out, err := tool.Call(context.Background(), name, string(raw))
		if err != nil {
			t.Fatalf("%s(%s): %v", name, raw, err)
		}
		return out
	}
	out := call("workspace_create", map[string]any{"parent": parent, "name": "eval4-app", "title": "Eval4"})
	var ws struct {
		Workspace struct {
			Path string `json:"path"`
		} `json:"workspace"`
	}
	if json.Unmarshal([]byte(out), &ws) != nil || ws.Workspace.Path == "" {
		t.Fatalf("workspace_create = %s", out)
	}
	out = call("session_create", map[string]any{"workspace": ws.Workspace.Path, "title": "实现登录"})
	var sess struct {
		Session struct {
			ID string `json:"id"`
		} `json:"session"`
	}
	if json.Unmarshal([]byte(out), &sess) != nil || sess.Session.ID == "" {
		t.Fatalf("session_create = %s", out)
	}
	// 目标任务用确定性 provider 真跑一轮。
	target := m.get(ws.Workspace.Path, sess.Session.ID)
	if target == nil {
		t.Fatal("target session missing")
	}
	target.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(&fixedEvalProvider{reply: "登录页已完成"})))
	out = call("session_send", map[string]any{"workspace": ws.Workspace.Path, "session_id": sess.Session.ID, "text": "实现登录页"})
	var sent struct {
		TaskID string `json:"task_id"`
		Queued bool   `json:"queued"`
	}
	if json.Unmarshal([]byte(out), &sent) != nil || sent.TaskID == "" {
		t.Fatalf("session_send = %s", out)
	}
	// 模型侧工具面：派完必须能用 task_status 查（§6.3.1）。
	out = call("task_status", map[string]any{"task_id": sent.TaskID})
	if !strings.Contains(out, sent.TaskID) || !strings.Contains(out, `"status"`) {
		t.Fatalf("task_status = %s", out)
	}
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenthub.NewLedger(st)
	var got *agenthub.Task
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if t, err := ledger.Replay(context.Background(), agenthub.TaskID(sent.TaskID)); err == nil {
			got = t
			if t.Status == agenthub.TaskDelivered {
				break
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("task not found")
	}
	// 结果回推后必须是 delivered（completed 停在未回推 = 回推链路坏了）。
	if got.Status != agenthub.TaskDelivered {
		t.Fatalf("task status = %s, want delivered (err=%s)", got.Status, got.Error)
	}
	if got.Result == nil || !strings.Contains(got.Result.Summary, "登录页已完成") {
		t.Fatalf("result = %+v", got.Result)
	}
	if got.Status == agenthub.TaskDelivered && len(got.NotifiedRuns) == 0 {
		t.Fatal("delivered without notified run")
	}
	// 发起者 DM 收到 task_result（事件行或 pending）。
	if !strings.Contains(buf.String(), `"task_result_delivered"`) && !strings.Contains(buf.String(), `"task_result"`) {
		t.Fatalf("no task_result push: %s", buf.String())
	}
}

func TestEval5ManualCancelDoesNotAutoResume(t *testing.T) {
	m, buf := testManager(t)
	wsPath := t.TempDir()
	createWorkspaceAgent(t, m, buf, 1, "Eval5Agent", []string{wsPath})
	m.wsReg = newWorkspaceRegistry(t.TempDir() + "/workspaces.json")
	if _, err := m.wsReg.add(wsPath); err != nil {
		t.Fatal(err)
	}
	hubAgent := createHubAgent(t, m, buf, 2, "Eval5Hub")
	hubBS, err := m.hubDMSession(string(hubAgent.ID))
	if err != nil {
		t.Fatal(err)
	}
	target, err := m.create(wsPath, "s-eval5", "")
	if err != nil {
		t.Fatal(err)
	}
	target.running.Store(true) // 目标忙 → 任务留在队列，人能在这段时间取消
	tool, err := hubBS.hubEngine.GetTool(context.Background(), "session_send")
	if err != nil {
		t.Fatal(err)
	}
	out, err := tool.Call(context.Background(), "session_send",
		`{"workspace":`+jsonString(wsPath)+`,"session_id":"s-eval5","text":"会被取消的任务"}`)
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	var sent struct {
		TaskID string `json:"task_id"`
		Queued bool   `json:"queued"`
	}
	if json.Unmarshal([]byte(out), &sent) != nil || !sent.Queued {
		t.Fatalf("send = %s", out)
	}
	ok, errMsg, _ := hubCmd(t, m, buf, 3, "hub_task_cancel", map[string]any{"task_id": sent.TaskID})
	if !ok {
		t.Fatalf("cancel: %s", errMsg)
	}
	// 空闲后也不会再执行：队列已空、账本 canceled、没有目标 agent_end。
	target.running.Store(false)
	m.pumpHubQueue(wsPath, "s-eval5")
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	got, err := agenthub.NewLedger(st).Replay(context.Background(), agenthub.TaskID(sent.TaskID))
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agenthub.TaskCanceled {
		t.Fatalf("status = %s, want canceled", got.Status)
	}
	if hasEventForSession(buf.String(), "agent_end", "s-eval5") {
		t.Fatal("cancelled task ran on target")
	}
	all, _ := st.ListTasks(context.Background(), agenthub.TaskFilter{})
	if len(all) != 1 {
		t.Fatalf("tasks = %d, want 1（取消不自动重试）", len(all))
	}
}

func TestEval6RestartIsExplainable(t *testing.T) {
	m, buf := testManager(t)
	hubAgent := createHubAgent(t, m, buf, 1, "Eval6Hub")
	st, err := m.hubStore()
	if err != nil {
		t.Fatal(err)
	}
	ledger := agenthub.NewLedger(st)
	// 有 run 但没有结束事件（模拟崩溃）→ interrupted + Needs you + 告警。
	orphan, err := ledger.Create(context.Background(), &agenthub.Task{
		Origin: agenthub.TaskOrigin{Kind: agenthub.OriginDM, AgentID: hubAgent.ID},
		Target: agenthub.TaskTarget{Kind: agenthub.TargetWorkspace, Workspace: t.TempDir(), SessionID: "s-orphan"},
		Title:  "崩溃任务", Instruction: "崩溃任务", Callback: agenthub.CallbackAgentEnd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.Transition(context.Background(), orphan.ID, agenthub.TaskDispatched, agenthub.TransitionOptions{At: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	buf.Reset()
	if err := m.recoverHubTasks(); err != nil {
		t.Fatal(err)
	}
	got, err := ledger.Replay(context.Background(), orphan.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != agenthub.TaskInterrupted && got.Status != agenthub.TaskDelivered {
		t.Fatalf("orphan status = %s", got.Status)
	}
	if !strings.Contains(buf.String(), `"recovered_interrupted"`) {
		t.Fatalf("missing alert: %s", buf.String())
	}
	needs, err := st.ListNeeds(context.Background(), agenthub.NeedFilter{Status: "pending"})
	if err != nil || len(needs) != 1 || !strings.Contains(needs[0].Body, "重启") {
		t.Fatalf("needs = %+v, %v", needs, err)
	}
}

// hasEventForSession 按行解析事件，避免把别的会话的 agent_end 当成本会话（Eval5 假阳性）。
func hasEventForSession(raw, eventType, sid string) bool {
	for _, line := range strings.Split(raw, "\n") {
		var ev map[string]any
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev["event_type"] == eventType && ev["session_id"] == sid {
			return true
		}
	}
	return false
}

// fixedEvalProvider 永远回固定正文（不按 system 文本猜压缩请求）。
type fixedEvalProvider struct{ reply string }

func (p *fixedEvalProvider) Stream(_ context.Context, req *provider.StreamRequest, h func(provider.StreamEvent) error) error {
	return h(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: p.reply, Model: req.Model})
}
