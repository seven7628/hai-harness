package main

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/subagent"
	"github.com/seven7628/hai-harness/tools"
)

// 真实桥接装配下的后台子 agent 结果回传复现：驱动 agent_spawn 走引擎（与生产同链路），
// 断言任务结束后 wire 上出现 task_result_delivered（含正常结束与异常结束两种）。
func TestSubagentResultDeliveredAtBridge(t *testing.T) {
	for _, tc := range []struct {
		name     string
		provErr  error
		content  string
		wantStat string
	}{
		{"normal", nil, "研究结论", "completed"},
		{"error", errReproBoom{}, "", "failed"},
		{"interrupt", nil, "", "interrupted"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ws := t.TempDir()
			m, buf := testManager(t)
			sid := newSessionID(t, m, buf, ws)
			bs := m.get(ws, sid)
			if bs == nil {
				t.Fatal("session not found")
			}

			// 子 agent loop：可阻塞（interrupt 用例），可返回错误（error 用例）。
			release := make(chan struct{})
			childP := &reproSubProvider{err: tc.provErr, content: tc.content, gate: release}
			childLoop := agents.NewAgentLoop(agents.WithProvider(childP), agents.WithMaxRetries(1))

			// 主 loop：轮1 派 agent_spawn，轮2 收尾。
			mainP := &bridgeMockProvider{}
			mainP.rounds = []func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
				func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
					return handler(provider.LLMEndEvent{
						FinishReason: core.FinishReasonToolCall,
						ToolCalls:    []core.ToolCall{{Id: "c1", Index: 0, Name: "agent_spawn", Arguments: `{"name":"research","task":"研究 X"}`}},
					})
				},
				func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
					return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "已派出"})
				},
			}

			// 复用会话内已有的引擎（生产形态）：把 agent_spawn 换成指向本测试子 loop 的工具，
			// 但**沿用会话同一 Registry** —— 结果回传链路（SetOnTaskDone）正是挂在它上面。
			reg := bs.s.Registry()
			eng := tools.NewToolEngine()
			// 与生产同装配：注入文件型状态存储（subagentStateStore）——这是本次改动的核心新链路。
			eng.RegisterTool(context.Background(), subagent.NewBackgroundTool(subagent.Spec{
				Name: "agent_spawn", Description: "d", Loop: childLoop, Background: true,
				Store: subagentStateStore(ws, sid),
			}, func() context.Context { return bs.s.Context() }, reg))
			bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(mainP), agents.WithToolEngine(eng), agents.WithMaxIterations(3)))

			buf.Reset()
			m.dispatch(cmd(0, "ask", map[string]any{"workspace": ws, "session_id": sid, "text": "研究 X"}))

			// 等任务登记，取 task_id。
			taskID := ""
			waitBridgeString(t, buf, func(s string) bool {
				ev := findFirst(s, func(mm map[string]any) bool { return mm["event_type"] == "task_started" })
				if ev != nil {
					taskID = str(ev, "task_id")
				}
				return taskID != ""
			}, 3*time.Second)

			if tc.name == "interrupt" {
				m.dispatch(cmd(1, "interrupt_task", map[string]any{"workspace": ws, "session_id": sid, "task_id": taskID}))
			}
			close(release)

			// 关键断言：任务结束后 wire 上出现 task_result_delivered（结果通知主会话）。
			buf.Reset()
			waitBridgeString(t, buf, func(s string) bool {
				return findFirst(s, func(mm map[string]any) bool {
					return mm["event_type"] == "task_result_delivered" && str(mm, "task_id") == taskID
				}) != nil
			}, 5*time.Second)

			ev := findFirst(buf.String(), func(mm map[string]any) bool { return mm["event_type"] == "task_result_delivered" })
			if got := str(ev, "status"); got != tc.wantStat {
				t.Fatalf("status = %q, want %q (ev=%+v)", got, tc.wantStat, ev)
			}
		})
	}
}

// TestSubagentResultDeliveredAfterResume 原地续跑后**再次**结束，结果是否仍能回传？
// 生产形态：带 handler 的 ToolContext（事件正常上报）+ **不**调 Wait（Wait 会按设计
// 抑制 OnTaskDone —— 结果由 Wait 消费）。
func TestSubagentResultDeliveredAfterResume(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	sid := newSessionID(t, m, buf, ws)
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session not found")
	}

	reg := bs.s.Registry()
	store := subagentStateStore(ws, sid)
	bgCtx := func() context.Context { return bs.s.Context() }

	var runs int
	childLoop := agents.NewAgentLoop(agents.WithProvider(&reproSeqProvider{seq: &runs}), agents.WithMaxRetries(1))
	jp := func(id string) (string, error) { return journalPathFor(wsSessionsDir(ws), sid, id) }
	eng := tools.NewToolEngine()
	for _, tl := range subagent.NewAgentTools(childLoop, bgCtx, reg, jp, nil, nil, subagent.WithAgentStateStore(store)) {
		eng.RegisterTool(context.Background(), tl)
	}
	spawn, _ := eng.GetTool(context.Background(), "agent_spawn")
	resume, _ := eng.GetTool(context.Background(), "agent_resume")

	// 与 agents 运行期一致的工具上下文：带 handler（事件转发到 wire）。
	tctx := events.WithToolContext(context.Background(), "run-1", 0,
		func(_ context.Context, e events.Event) { bs.emit(e) }, nil, nil)

	buf.Reset()
	out, err := spawn.Call(tctx, "", `{"name":"n","task":"第一轮任务"}`)
	if err != nil {
		t.Fatalf("spawn: %v", err)
	}
	taskID := reproTaskID(t, out)

	// 等首轮结束（轮询注册表，不用 Wait）。
	waitTaskSettled(t, reg, taskID)
	waitBridgeString(t, buf, func(s string) bool {
		return countTaskDelivered(s, taskID) >= 1
	}, 5*time.Second)

	// 续跑（同一 task_id），再等它结束。
	if _, err := resume.Call(tctx, "", `{"task_id":"`+taskID+`","message":"接着做"}`); err != nil {
		t.Fatalf("resume: %v", err)
	}
	waitTaskSettled(t, reg, taskID)
	waitBridgeString(t, buf, func(s string) bool {
		return countTaskDelivered(s, taskID) >= 2
	}, 5*time.Second)
}

// waitTaskSettled 轮询直到任务离开 running/interrupting。
func waitTaskSettled(t *testing.T, reg *subagent.Registry, id string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := reg.Snapshot(id); err == nil && info.Status != subagent.TaskRunning && info.Status != subagent.TaskInterrupting {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("task did not settle")
}

// countTaskDelivered 统计某 task 的 task_result_delivered 条数。
func countTaskDelivered(s, taskID string) int {
	n := 0
	for _, l := range strings.Split(s, "\n") {
		var mm map[string]any
		if json.Unmarshal([]byte(l), &mm) == nil &&
			mm["event_type"] == "task_result_delivered" && str(mm, "task_id") == taskID {
			n++
		}
	}
	return n
}

// reproTaskID 从 agent_spawn 返回 JSON 取 task_id。
func reproTaskID(t *testing.T, out string) string {
	t.Helper()
	var v struct {
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal([]byte(out), &v); err != nil || v.TaskID == "" {
		t.Fatalf("spawn 返回无 task_id: %q err=%v", out, err)
	}
	return v.TaskID
}

type reproSeqProvider struct {
	seq *int
}

func (p *reproSeqProvider) Stream(_ context.Context, _ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	*p.seq++
	return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "结论"})
}

type errReproBoom struct{}

func (errReproBoom) Error() string { return "boom" }

type reproSubProvider struct {
	err     error
	content string
	gate    chan struct{}
}

func (p *reproSubProvider) Stream(_ context.Context, _ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	if p.gate != nil {
		<-p.gate
	}
	if p.err != nil {
		return p.err
	}
	if p.content == "" {
		// 中断用例：返回后由 abort 归一为 interrupted
		return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "done"})
	}
	return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: p.content})
}

// TestReducerAcceptsResumedTaskLifecycle 原地续跑的第二轮必须**不被去重丢弃**。
//
// 回归锁：reducer 曾按 taskId 去重 TaskStarted/TaskEnd/TaskResultDelivered —— 会话级
// 恢复（agent_resume 复用同一 task_id，docs §13）后第二轮事件全被吞，Go 侧视图停在
// 上一轮终态、前端拿不到续跑结果（用户现象：「任务结束了但没有结果通知」）。
func TestReducerAcceptsResumedTaskLifecycle(t *testing.T) {
	r := NewViewReducer()
	const tid = "task-abc123fg"
	t0 := time.Unix(1000, 0)

	// —— 第一轮：中断结束 ——
	_ = r.ApplyPlain(&events.TaskStarted{TaskId: tid, Name: "n", Status: string(subagent.TaskRunning), Timestamp: t0, EventType: events.TaskStartedType})
	_ = r.ApplyPlain(&events.TaskEnd{TaskId: tid, Status: string(subagent.TaskInterrupted), Error: "budget", Timestamp: t0.Add(time.Second), EventType: events.TaskEndType})
	_ = r.ApplyPlain(&events.TaskResultDelivered{TaskId: tid, Status: string(subagent.TaskInterrupted), Error: "budget", Timestamp: t0.Add(time.Second), EventType: events.TaskResultDeliveredType})

	cp := r.Snapshot()
	if got := statusOfTask(t, cp, tid); got != string(subagent.TaskInterrupted) {
		t.Fatalf("第一轮终态 = %q, want interrupted", got)
	}

	// —— 第二轮（原地续跑）：同一 task_id 重新跑并完成 ——
	_ = r.ApplyPlain(&events.TaskStarted{TaskId: tid, Name: "n", Status: string(subagent.TaskRunning), Timestamp: t0.Add(2 * time.Second), EventType: events.TaskStartedType})
	_ = r.ApplyPlain(&events.TaskEnd{TaskId: tid, Status: string(subagent.TaskCompleted), Timestamp: t0.Add(3 * time.Second), EventType: events.TaskEndType})
	_ = r.ApplyPlain(&events.TaskResultDelivered{TaskId: tid, Status: string(subagent.TaskCompleted), Result: "第二轮结论", Timestamp: t0.Add(3 * time.Second), EventType: events.TaskResultDeliveredType})

	cp = r.Snapshot()
	if got := statusOfTask(t, cp, tid); got != string(subagent.TaskCompleted) {
		t.Fatalf("续跑后终态 = %q, want completed —— 第二轮被按 taskId 去重丢弃了", got)
	}
	// 结果回填（续跑那轮的结论必须落到卡片上）
	var result string
	for _, b := range cp.Blocks {
		if b.TaskID == tid {
			result = b.TaskResult
		}
	}
	if result != "第二轮结论" {
		t.Fatalf("续跑结果未回填: %q", result)
	}
	// 同轮重复投递仍幂等（同一 Timestamp → 同 key）
	n := len(cp.Blocks)
	_ = r.ApplyPlain(&events.TaskResultDelivered{TaskId: tid, Status: string(subagent.TaskCompleted), Result: "第二轮结论", Timestamp: t0.Add(3 * time.Second), EventType: events.TaskResultDeliveredType})
	if got := len(r.Snapshot().Blocks); got != n {
		t.Fatalf("同轮重复投递应幂等：blocks %d → %d", n, got)
	}
}

// statusOfTask 取某 task 卡片的当前状态（agent / async_task）。
func statusOfTask(t *testing.T, cp *Checkpoint, taskID string) string {
	t.Helper()
	for _, b := range cp.Blocks {
		if b.TaskID == taskID && (b.Kind == blockKindAgent || b.Kind == blockKindAsyncTask) {
			return b.Status
		}
	}
	t.Fatalf("未找到 task %s 的卡片: %+v", taskID, cp.Blocks)
	return ""
}
