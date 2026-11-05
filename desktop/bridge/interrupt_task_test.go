package main

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/subagent"
	"github.com/seven7628/hai-harness/tools"
)

// —— 客户端「停止单个异步任务」：interrupt_task 命令 → Session.InterruptTask 精确路由 ——

// TestInterruptTaskCommandErrors 错误路径：会话不存在 / 未知任务 id（tooltask-* 与 task-*）。
func TestInterruptTaskCommandErrors(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)

	// 会话不存在
	m.dispatch(cmd(0, "interrupt_task", map[string]any{"workspace": ws, "session_id": "nope", "task_id": "tooltask-1"}))
	got := findFirst(buf.String(), func(mm map[string]any) bool { return true })
	if got["ok"] != false || !strings.Contains(str(got, "error"), "session not found") {
		t.Fatalf("session-not-found: %+v", got)
	}
	buf.Reset()

	// 未知 tooltask id（会话存在但无运行批控制器）
	sid := newSessionID(t, m, buf, ws)
	m.dispatch(cmd(1, "interrupt_task", map[string]any{"workspace": ws, "session_id": sid, "task_id": "tooltask-999"}))
	got = findFirst(buf.String(), func(mm map[string]any) bool { return true })
	if got["ok"] != false || !strings.Contains(str(got, "error"), "task not found") {
		t.Fatalf("unknown tooltask: %+v", got)
	}
	buf.Reset()

	// 未知子 agent id（task-* 路由到注册表）
	m.dispatch(cmd(2, "interrupt_task", map[string]any{"workspace": ws, "session_id": sid, "task_id": "task-999"}))
	got = findFirst(buf.String(), func(mm map[string]any) bool { return true })
	if got["ok"] != false || !strings.Contains(str(got, "error"), "task not found") {
		t.Fatalf("unknown task: %+v", got)
	}
}

// TestInterruptTaskCommandSuccess 端到端：promoted 长工具（runTools 已返回）→
// interrupt_task（按 taskId 精确停止）→ 工具 ctx 被取消 → task_result_delivered
// 终态 interrupted；主会话不被中断（第一次 agent_end 正常 stop，无 abort）。
func TestInterruptTaskCommandSuccess(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	sid := newSessionID(t, m, buf, ws)
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session not found")
	}

	block := &bridgeBlockTool{gate: make(chan struct{})}
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), block)
	p := &bridgeMockProvider{}
	p.rounds = []func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error { // 轮1：调 block
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonToolCall,
				ToolCalls:    []core.ToolCall{{Id: "c1", Index: 0, Name: "block", Arguments: `{}`}},
			})
		},
		func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error { // 轮2：看到占位，stop
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "moved to background"})
		},
	}
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(p), agents.WithToolEngine(eng), agents.WithMaxIterations(3)))

	// 触发一轮：block 工具运行中被 promote
	buf.Reset()
	m.dispatch(cmd(0, "ask", map[string]any{"workspace": ws, "session_id": sid, "text": "run"}))

	taskID := ""
	waitBridgeString(t, buf, func(s string) bool {
		ev := findFirst(s, func(mm map[string]any) bool {
			return mm["event_type"] == "tool_run_start" && str(mm, "task_id") != ""
		})
		if ev != nil {
			taskID = str(ev, "task_id")
		}
		return ev != nil
	}, 3*time.Second)

	// promote（工具转后台），主 run 拿到占位正常结束（第一次 agent_end）
	buf.Reset()
	m.dispatch(cmd(1, "promote_task", map[string]any{"workspace": ws, "session_id": sid, "task_id": taskID}))
	if resp := findFirst(buf.String(), func(mm map[string]any) bool { return true }); resp["ok"] != true {
		t.Fatalf("promote_task: %+v", resp)
	}
	waitBridgeString(t, buf, func(s string) bool { return countAgentEnd(s) >= 1 }, 3*time.Second)
	if abortAgentEnd(buf.String()) != nil {
		t.Fatal("main session must not be aborted by task interrupt")
	}

	// 精确停止：interrupt_task → ok:true；随后 task_result_delivered 终态 interrupted
	m.dispatch(cmd(2, "interrupt_task", map[string]any{"workspace": ws, "session_id": sid, "task_id": taskID}))
	if resp := findFirst(buf.String(), func(mm map[string]any) bool {
		return mm["event_type"] == "command_response" && mm["id"] == float64(2)
	}); resp == nil || resp["ok"] != true {
		t.Fatalf("interrupt_task: %+v", resp)
	}
	waitBridgeString(t, buf, func(s string) bool {
		ev := findFirst(s, func(mm map[string]any) bool {
			return mm["event_type"] == "task_result_delivered" && str(mm, "task_id") == taskID
		})
		return ev != nil && str(ev, "status") == "interrupted"
	}, 3*time.Second)

	// 重复停止（已终态）→ 明确错误
	m.dispatch(cmd(3, "interrupt_task", map[string]any{"workspace": ws, "session_id": sid, "task_id": taskID}))
	if resp := findFirst(buf.String(), func(mm map[string]any) bool {
		return mm["event_type"] == "command_response" && mm["id"] == float64(3)
	}); resp == nil || resp["ok"] != false {
		t.Fatalf("repeat interrupt_task must fail: %+v", resp)
	}
	waitStoreSettled(t, ws, sid) // 排空 task_result 异步落盘，避免 TempDir 清理竞态
}

// TestInterruptKeepsPromotedToolTask 主「停止」不得波及 promoted 后台工具任务（回归锚点）：
// settings.json 里即使显式写着 runtime.stop_background_on_interrupt=true，该键也已**失效**
// （无条件规则：主停止只 abort 当前主 run）；任务卡上的精确停止是 interrupt_task 唯一入口。
// 设置仍在文件里却必须无效 —— 复现真机形态：Electron 把「默认值」当持久值写进了设置。
func TestInterruptKeepsPromotedToolTask(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	sid := newSessionID(t, m, buf, ws)
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session not found")
	}

	block := &bridgeBlockTool{gate: make(chan struct{})}
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), block)
	p := &bridgeMockProvider{}
	p.rounds = []func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error { // 轮1：调 block
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonToolCall,
				ToolCalls:    []core.ToolCall{{Id: "c1", Index: 0, Name: "block", Arguments: `{}`}},
			})
		},
		func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error { // 轮2：看到占位，stop
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "moved to background"})
		},
	}
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(p), agents.WithToolEngine(eng), agents.WithMaxIterations(3)))
	// 收尾：放行阻塞工具（promoted 任务跑完并交付）→ 等它从在途集合消失 → 停会话 →
	// 等落盘稳定，避免迟到写入与 t.TempDir RemoveAll 竞态（directory not empty）。
	// 摘离任务没有 Task 实例、没有 Wait 句柄，只能轮询任务榜（交付完成即移出控制器 map）。
	defer func() {
		block.once.Do(func() { close(block.gate) })
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			inFlight := false
			for _, ti := range bs.s.Registry().List() {
				if strings.HasPrefix(ti.ID, "tooltask-") {
					inFlight = true
				}
			}
			if !inFlight {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		_ = bs.s.Shutdown(context.Background())
		bs.cancel()
		waitStoreSettled(t, ws, sid)
	}()

	// 触发一轮：block 工具运行中被 promote
	buf.Reset()
	m.dispatch(cmd(0, "ask", map[string]any{"workspace": ws, "session_id": sid, "text": "run"}))
	taskID := ""
	waitBridgeString(t, buf, func(s string) bool {
		ev := findFirst(s, func(mm map[string]any) bool {
			return mm["event_type"] == "tool_run_start" && str(mm, "task_id") != ""
		})
		if ev != nil {
			taskID = str(ev, "task_id")
		}
		return ev != nil
	}, 3*time.Second)

	// promote_task 响应按**命令 id** 匹配，不假设它是缓冲里的第一行：run goroutine 同时在
	// 写事件行，混合缓冲下「第一行」可能是事件（实测偶发）→ 固定取首位会误判 promote 失败。
	buf.Reset()
	m.dispatch(cmd(1, "promote_task", map[string]any{"workspace": ws, "session_id": sid, "task_id": taskID}))
	var promoteResp map[string]any
	waitBridgeString(t, buf, func(s string) bool {
		promoteResp = findFirst(s, func(mm map[string]any) bool {
			return mm["event_type"] == "command_response" && mm["id"] == float64(1)
		})
		return promoteResp != nil
	}, 3*time.Second)
	if promoteResp["ok"] != true {
		t.Fatalf("promote_task: %+v", promoteResp)
	}
	waitBridgeString(t, buf, func(s string) bool { return countAgentEnd(s) >= 1 }, 3*time.Second)

	// 主「停止」：promoted 工具任务必须仍在任务榜上 running（级联已删除，无开关可翻转）
	buf.Reset()
	m.dispatch(cmd(2, "interrupt", map[string]any{"workspace": ws, "session_id": sid}))
	if resp := findFirst(buf.String(), func(mm map[string]any) bool {
		return mm["event_type"] == "command_response" && mm["id"] == float64(2)
	}); resp == nil || resp["ok"] != true {
		t.Fatalf("interrupt: %+v", resp)
	}
	if got := taskStatus(bs, taskID); got != subagent.TaskRunning {
		t.Fatalf("main stop must not touch promoted tool tasks, got %s", got)
	}
	if ev := findFirst(buf.String(), func(mm map[string]any) bool {
		return mm["event_type"] == "task_result_delivered" && str(mm, "task_id") == taskID
	}); ev != nil {
		t.Fatalf("main stop must not deliver a task result: %+v", ev)
	}

	// 精确停止仍走 interrupt_task（任务卡入口）：终态 interrupted，主会话此前已正常结束
	buf.Reset()
	m.dispatch(cmd(3, "interrupt_task", map[string]any{"workspace": ws, "session_id": sid, "task_id": taskID}))
	if resp := findFirst(buf.String(), func(mm map[string]any) bool {
		return mm["event_type"] == "command_response" && mm["id"] == float64(3)
	}); resp == nil || resp["ok"] != true {
		t.Fatalf("interrupt_task: %+v", resp)
	}
	waitBridgeString(t, buf, func(s string) bool {
		ev := findFirst(s, func(mm map[string]any) bool {
			return mm["event_type"] == "task_result_delivered" && str(mm, "task_id") == taskID
		})
		return ev != nil && str(ev, "status") == "interrupted"
	}, 3*time.Second)
	waitStoreSettled(t, ws, sid)
}

// taskStatus 查注册表中任务状态（不存在 = 已从列表移除，视为终态）。
func taskStatus(bs *bridgeSession, taskID string) subagent.TaskStatus {
	for _, ti := range bs.s.Registry().List() {
		if ti.ID == taskID {
			return ti.Status
		}
	}
	return subagent.TaskInterrupted
}
