package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

func evLLMStart(t *testing.T, runID, requestID, model string, ts time.Time) *events.LLMStart {
	t.Helper()
	return &events.LLMStart{RunId: runID, RequestId: requestID, Model: model, Timestamp: ts, EventType: events.LLMStartType}
}

func evLLMEnd(t *testing.T, runID, requestID, model string, content string, u *core.Usage, ts time.Time) *events.LLMEnd {
	t.Helper()
	return &events.LLMEnd{
		RunId: runID, RequestId: requestID, Content: content, Model: model,
		Usage: u, FinishReason: core.FinishReasonStop, Timestamp: ts, EventType: events.LLMEndType,
	}
}

func evContentChunk(t *testing.T, runID, requestID, content string, idx int64, ts time.Time) *events.ContentChunk {
	t.Helper()
	return &events.ContentChunk{RunId: runID, RequestId: requestID, Index: idx, Content: content, Timestamp: ts, EventType: events.ContentChunkType}
}

func evReasoningChunk(t *testing.T, runID, requestID, content string, idx int64, ts time.Time) *events.ReasoningChunk {
	t.Helper()
	return &events.ReasoningChunk{RunId: runID, RequestId: requestID, Index: idx, Content: content, Timestamp: ts, EventType: events.ReasoningChunkType}
}

func evUserInputs(t *testing.T, texts []string, contents [][]core.Content, ts time.Time) *events.UserInputsConsumed {
	t.Helper()
	return &events.UserInputsConsumed{SessionId: "s1", UserTexts: texts, UserContents: contents, Timestamp: ts, EventType: events.UserInputsConsumedType}
}

func evToolStart(t *testing.T, runID, id, name, args string, ts time.Time) *events.ToolStart {
	t.Helper()
	return &events.ToolStart{RunId: runID, Id: id, Name: name, Arguments: args, Timestamp: ts, EventType: events.ToolRunStartType}
}

func evToolEnd(t *testing.T, runID, id, name, result string, isErr bool, diff *core.FileDiff, ts time.Time) *events.ToolResponse {
	t.Helper()
	return &events.ToolResponse{
		RunId: runID, Id: id, Name: name, Result: result, IsError: isErr,
		Diff: diff, Timestamp: ts, EventType: events.ToolRunEndType,
	}
}

func evAgentStart(t *testing.T, runID, parent, name, taskID string, ts time.Time) *events.AgentStart {
	t.Helper()
	return &events.AgentStart{RunId: runID, ParentRunId: parent, Name: name, TaskId: taskID, Timestamp: ts, EventType: events.AgentStartType}
}

func evAgentEnd(t *testing.T, runID string, fr core.FinishReason, ts time.Time) *events.AgentEnd {
	t.Helper()
	return &events.AgentEnd{RunId: runID, FinishReason: fr, Timestamp: ts, EventType: events.AgentEndType}
}

func mustApply(t *testing.T, r *ViewReducer, ev events.Event) {
	t.Helper()
	if err := r.Apply(ev, EventMeta{Provider: "deepseek"}); err != nil {
		t.Fatalf("apply %s: %v", ev.Type(), err)
	}
}

func blockByKind(t *testing.T, cp *Checkpoint, kind string) []Block {
	t.Helper()
	var out []Block
	for _, b := range cp.Blocks {
		if b.Kind == kind {
			out = append(out, b)
		}
	}
	return out
}

func usageFor(cp *Checkpoint) Usage { return cp.Usage }

// TestReducerUserInputsConsumed 用户输入（含图片附件）按确认事件进入对话块。
func TestReducerUserInputsConsumed(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Minute)
	mustApply(t, r, evUserInputs(t, []string{"hello"}, [][]core.Content{{{Type: core.ContentTypeText, Content: "hello"}}}, ts))
	mustApply(t, r, evUserInputs(t, nil, [][]core.Content{{{Type: core.ContentTypeImage, Content: "data:image/png;base64,AAA", MimeType: "image/png"}}}, ts.Add(time.Second)))

	cp := r.Snapshot()
	users := blockByKind(t, cp, "user")
	if len(users) != 2 {
		t.Fatalf("want 2 user blocks, got %d", len(users))
	}
	if users[0].Text != "hello" || len(users[0].Contents) != 1 {
		t.Fatalf("user[0] mismatch: %+v", users[0])
	}
	if users[1].Text != "(附件)" || len(users[1].Contents) != 1 || users[1].Contents[0].Type != "image" || users[1].Contents[0].MimeType != "image/png" {
		t.Fatalf("user[1] image block mismatch: %+v", users[1])
	}
}

// TestReducerChunkAdjacentMerge 只合并相邻同 key chunk；非相邻（不同 request/kind）先 flush。
func TestReducerChunkAdjacentMerge(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evLLMStart(t, "r1", "q1", "m1", ts))
	mustApply(t, r, evReasoningChunk(t, "r1", "q1", "A", 0, ts.Add(time.Millisecond)))
	mustApply(t, r, evReasoningChunk(t, "r1", "q1", "B", 1, ts.Add(2*time.Millisecond)))
	mustApply(t, r, evContentChunk(t, "r1", "q1", "C", 0, ts.Add(10*time.Millisecond)))
	mustApply(t, r, evContentChunk(t, "r1", "q1", "D", 1, ts.Add(11*time.Millisecond)))
	mustApply(t, r, evLLMEnd(t, "r1", "q1", "m1", "", &core.Usage{Input: 100, Output: 50}, ts.Add(20*time.Millisecond)))

	cp := r.Snapshot()
	as := blockByKind(t, cp, "assistant")
	if len(as) != 1 {
		t.Fatalf("want 1 assistant block, got %d", len(as))
	}
	if as[0].Thinking != "AB" {
		t.Fatalf("thinking = %q, want AB", as[0].Thinking)
	}
	if as[0].Text != "CD" {
		t.Fatalf("text = %q, want CD", as[0].Text)
	}
	// TTFT = 首个任意 chunk（reasoning）距 llm_start
	if as[0].TtftMs != 1 {
		t.Fatalf("ttft = %d, want 1", as[0].TtftMs)
	}
	// thinkMs = 首个 reasoning → 首个 content
	if as[0].ThinkMs != 9 {
		t.Fatalf("thinkMs = %d, want 9", as[0].ThinkMs)
	}
	// durMs = llm_start → llm_end
	if as[0].DurMs != 20 {
		t.Fatalf("durMs = %d, want 20", as[0].DurMs)
	}
	if len(cp.Turns) != 1 || cp.Turns[0].ThinkMs != 9 || cp.Turns[0].TtftMs != 1 || cp.Turns[0].DurMs != 20 {
		t.Fatalf("turn mismatch: %+v", cp.Turns)
	}
}

// TestReducerChunkNonAdjacentFlush 非相邻 chunk（中间夹语义事件）必须分开，不得跨事件合并。
func TestReducerChunkNonAdjacentFlush(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evLLMStart(t, "r1", "q1", "m1", ts))
	mustApply(t, r, evContentChunk(t, "r1", "q1", "A", 0, ts.Add(time.Millisecond)))
	mustApply(t, r, evContentChunk(t, "r1", "q1", "B", 1, ts.Add(2*time.Millisecond)))
	// 中间夹一个语义事件：加压缩开始（瞬态），chunk 必须 flush 后再继续
	mustApply(t, r, &events.CompressStart{Before: 10, RunId: "r1", EventType: events.CompressStartType})
	mustApply(t, r, evContentChunk(t, "r1", "q1", "C", 2, ts.Add(3*time.Millisecond)))
	mustApply(t, r, evLLMEnd(t, "r1", "q1", "m1", "", nil, ts.Add(5*time.Millisecond)))

	cp := r.Snapshot()
	as := blockByKind(t, cp, "assistant")
	if len(as) != 1 || as[0].Text != "ABC" {
		t.Fatalf("text = %q, want ABC (flush 后继续合并)", func() string {
			if len(as) == 0 {
				return "<none>"
			}
			return as[0].Text
		}())
	}
}

// TestReducerLLMEndDedupAndUsage 重复 llm_end 幂等；usage/cost 只累计一次。
func TestReducerLLMEndDedupAndUsage(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	start := evLLMStart(t, "r1", "q1", "deepseek-v4-flash", ts)
	mustApply(t, r, start)
	end := evLLMEnd(t, "r1", "q1", "deepseek-v4-flash", "final", &core.Usage{Input: 1000, Output: 100, CacheRead: 900, CacheWrite: 60, Reasoning: 40}, ts.Add(time.Second))
	mustApply(t, r, end)
	// 同一事件重送（IPC 重放/重复投递）→ 幂等
	mustApply(t, r, end)
	mustApply(t, r, end)

	cp := r.Snapshot()
	if len(cp.Turns) != 1 {
		t.Fatalf("turns = %d, want 1", len(cp.Turns))
	}
	want := Usage{Input: 1000, Output: 100, CacheRead: 900, CacheWrite: 60, Reasoning: 40}
	if cp.Usage != want {
		t.Fatalf("usage = %+v, want %+v", cp.Usage, want)
	}
	if cp.LifetimeUsage != want {
		t.Fatalf("lifetime = %+v, want %+v", cp.LifetimeUsage, want)
	}
	// deepseek 直连价：input $0.15/M, cacheRead $0.003/M, output $0.6/M（无写入价 = 0）
	// Input=1000 为总输入（含 CacheRead=900 + CacheWrite=60）→ miss=40 按全价、900 按低价，
	// 60 写入按写价 0 计（多数厂商无写入价：不计费，也不从 miss 里漏掉）。
	// 比较用容差：常量表达式在编译期折叠（精确），实现按 float64 逐步累加 —— 末位可能差 1ulp。
	wantCost := ((1000-900-60)*0.15 + 900*0.003 + 100*0.6) / 1e6
	if abs(cp.CostUSD-wantCost) > 1e-15 {
		t.Fatalf("cost = %f, want %f", cp.CostUSD, wantCost)
	}
	as := blockByKind(t, cp, "assistant")
	if len(as) != 1 || as[0].Text != "final" {
		t.Fatalf("assistant block mismatch: %+v", as)
	}
}

// TestReducerRetryCostAccounted 失败尝试记账（2026-09-23 决策）：llm_error 携带的
// 部分用量折算成成本 → 会话展示成本累计 + 错误块「重试已花费」跨多次重试累加；
// 成功轮（llm_end）只含自己那次，不双计。
func TestReducerRetryCostAccounted(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evLLMStart(t, "r1", "q1", "deepseek-v4-flash", ts))

	attempt1 := core.Usage{Input: 100, Output: 10, TotalTokens: 110,
		Cost: core.Cost{Input: 14, Output: 3, Total: 17, Priced: true, Source: core.CostSourceBuiltin}}
	attempt2 := core.Usage{Input: 120, Output: 12, TotalTokens: 132,
		Cost: core.Cost{Input: 17, Output: 3, Total: 20, Priced: true, Source: core.CostSourceBuiltin}}
	mustApply(t, r, &events.LLMError{RunId: "r1", Model: "deepseek-v4-flash", Message: "boom", Attempt: 1,
		WillRetry: true, ErrorKind: "generic", Usage: &attempt1, EventType: events.LLMErrorType})
	mustApply(t, r, &events.LLMError{RunId: "r1", Model: "deepseek-v4-flash", Message: "boom", Attempt: 2,
		WillRetry: false, ErrorKind: "generic", Usage: &attempt2, EventType: events.LLMErrorType})

	cp := r.Snapshot()
	// 会话展示成本 = 两次失败尝试之和（37 µUSD = 3.7e-5 USD）
	if want := 37e-6; cp.CostUSD < want-1e-12 || cp.CostUSD > want+1e-12 {
		t.Fatalf("会话成本 = %v, want %v（失败尝试也要进账）", cp.CostUSD, want)
	}
	errs := blockByKind(t, cp, "error")
	if len(errs) != 1 {
		t.Fatalf("error blocks = %d, want 1", len(errs))
	}
	if want := 37e-6; errs[0].RetryCostUSD < want-1e-12 || errs[0].RetryCostUSD > want+1e-12 {
		t.Fatalf("错误块重试成本 = %v, want %v（跨多次重试累加）", errs[0].RetryCostUSD, want)
	}
	if !strings.Contains(errs[0].Text, "重试已耗尽") {
		t.Fatalf("error text = %q（最终失败文案）", errs[0].Text)
	}
}

// TestReducerErrorPlacedByTime 错误块按**事件时刻**落位（与记录/回执同一判据）：
// 迟到的失败必须出现在它发生的那一刻，不得堆在已经开始的下一次运行之后。
// 运行级失败（session_run_error）也要带事件时刻 —— 缺了它 checkpoint 里这一行既无法
// 排位，刷新/重放后也读不出失败发生在什么时候。
func TestReducerErrorPlacedByTime(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	// 更晚的一块：工具块的事件时刻记在 started_at（blockTime 认它）
	mustApply(t, r, &events.ToolStart{RunId: "r2", Id: "call-late", Name: "bash", Arguments: "{}", Timestamp: ts.Add(10 * time.Second), EventType: events.ToolRunStartType})
	// 迟到的运行级失败：事件时刻更早 → 必须插到它前面
	mustApply(t, r, &events.SessionRunError{SessionId: "s1", Error: "boom", ErrorKind: "permanent", Timestamp: ts.Add(time.Second), EventType: events.SessionRunErrorType})

	cp := r.Snapshot()
	errIdx, toolIdx := -1, -1
	for i, b := range cp.Blocks {
		switch b.Kind {
		case "error":
			errIdx = i
		case "tool":
			toolIdx = i
		}
	}
	if errIdx < 0 || toolIdx < 0 {
		t.Fatalf("缺块：err@%d tool@%d", errIdx, toolIdx)
	}
	if errIdx > toolIdx {
		t.Fatalf("错误块必须按事件时刻插到更晚的块之前（err@%d, tool@%d）", errIdx, toolIdx)
	}
	if got, want := cp.Blocks[errIdx].Ts, ts.Add(time.Second).UnixMilli(); got != want {
		t.Fatalf("错误块时刻 = %d, want %d（不得写成处理时刻）", got, want)
	}
}

// TestReducerLLMErrorAndSessionRunError 错误块单条更新 + lastError + 子 agent 错误项。
func TestReducerLLMErrorAndSessionRunError(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evLLMStart(t, "r1", "q1", "m1", ts))
	mustApply(t, r, &events.LLMError{RunId: "r1", Message: "boom", Attempt: 1, WillRetry: true, ErrorKind: "generic", EventType: events.LLMErrorType})
	mustApply(t, r, &events.LLMError{RunId: "r1", Message: "boom", Attempt: 1, WillRetry: true, ErrorKind: "generic", EventType: events.LLMErrorType}) // 重送幂等
	mustApply(t, r, &events.SessionRunError{SessionId: "s1", Error: "fatal", ErrorKind: "permanent", Timestamp: ts.Add(time.Second), EventType: events.SessionRunErrorType})

	cp := r.Snapshot()
	errs := blockByKind(t, cp, "error")
	if len(errs) != 1 {
		t.Fatalf("error blocks = %d, want 1（原位更新）", len(errs))
	}
	if !strings.Contains(errs[0].Text, "运行失败") {
		t.Fatalf("error text = %q", errs[0].Text)
	}
	if cp.LastError == nil || cp.LastError.Message != "fatal" {
		t.Fatalf("lastError mismatch: %+v", cp.LastError)
	}
}

// TestReducerToolLifecycle 工具 start/end：结果/错误/diff/时长/run node；重送幂等。
func TestReducerToolLifecycle(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evToolStart(t, "r1", "call-1", "write_file", `{"path":"a.txt"}`, ts))
	mustApply(t, r, evToolStart(t, "r1", "call-1", "write_file", `{"path":"a.txt"}`, ts)) // 重送幂等
	diff := &core.FileDiff{Path: "a.txt", Added: 2, Removed: 1, Unified: "---\n+++", Truncated: false}
	mustApply(t, r, evToolEnd(t, "r1", "call-1", "write_file", "ok", false, diff, ts.Add(time.Second)))

	cp := r.Snapshot()
	tools := blockByKind(t, cp, "tool")
	if len(tools) != 1 {
		t.Fatalf("tool blocks = %d, want 1", len(tools))
	}
	tb := tools[0]
	if tb.Status != "done" || tb.Result != "ok" || tb.DurMs != 1000 || tb.Diff == nil || tb.Diff.Path != "a.txt" || tb.Diff.Added != 2 {
		t.Fatalf("tool block mismatch: %+v", tb)
	}
	if len(cp.Diffs) != 1 || cp.Diffs[0].Path != "a.txt" {
		t.Fatalf("diffs mismatch: %+v", cp.Diffs)
	}
	// run node
	found := false
	for _, n := range cp.RunNodes {
		if n.ID == "tool-call-1" && n.Kind == "tool" && n.Status == "done" && n.ParentID == "r1" {
			found = true
		}
	}
	if !found {
		t.Fatalf("tool run node missing: %+v", cp.RunNodes)
	}
}

// TestReducerAgentLifecycle 主/子 run：节点、卡片、中断与占位收敛。
func TestReducerAgentLifecycle(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evAgentStart(t, "root-1", "", "主会话", "", ts))
	mustApply(t, r, evAgentStart(t, "sub-1", "root-1", "调查", "task-9", ts.Add(time.Second)))
	mustApply(t, r, evReasoningChunk(t, "sub-1", "s1q", "think…", 0, ts.Add(1500*time.Millisecond)))
	mustApply(t, r, evAgentEnd(t, "sub-1", core.FinishReasonStop, ts.Add(5*time.Second)))
	mustApply(t, r, evAgentEnd(t, "root-1", core.FinishReasonStop, ts.Add(6*time.Second)))

	cp := r.Snapshot()
	if len(cp.RunNodes) != 2 {
		t.Fatalf("run nodes = %d, want 2", len(cp.RunNodes))
	}
	if cp.RunNodes[0].ID != "root-1" || cp.RunNodes[0].ParentID != "" || cp.RunNodes[0].Status != "done" {
		t.Fatalf("root node mismatch: %+v", cp.RunNodes[0])
	}
	if cp.RunNodes[1].ID != "sub-1" || cp.RunNodes[1].ParentID != "root-1" || cp.RunNodes[1].Status != "done" {
		t.Fatalf("sub node mismatch: %+v", cp.RunNodes[1])
	}
	agents := blockByKind(t, cp, "agent")
	if len(agents) != 1 {
		t.Fatalf("agent cards = %d, want 1", len(agents))
	}
	ag := agents[0]
	if ag.RunID != "sub-1" || ag.Status != "completed" || ag.TaskID != "task-9" || ag.Label != "调查" {
		t.Fatalf("agent card mismatch: %+v", ag)
	}
	if len(ag.Items) != 1 || ag.Items[0].Kind != "thinking" || ag.Items[0].Text != "think…" {
		t.Fatalf("agent items mismatch: %+v", ag.Items)
	}
	if cp.LastRun == nil || cp.LastRun.RunID != "root-1" || cp.LastRun.Status != "done" {
		t.Fatalf("lastRun mismatch: %+v", cp.LastRun)
	}
}

// TestReducerAgentAbort 主 run abort：清错误块、工具块收尾、插 divider（横线隔离）。
func TestReducerAgentAbort(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evAgentStart(t, "root-1", "", "主会话", "", ts))
	mustApply(t, r, evToolStart(t, "root-1", "call-1", "bash", "sleep", ts.Add(time.Second)))
	mustApply(t, r, evAgentEnd(t, "root-1", core.FinishReasonAbort, ts.Add(2*time.Second)))

	cp := r.Snapshot()
	tools := blockByKind(t, cp, "tool")
	if len(tools) != 1 || tools[0].Status != "done" {
		t.Fatalf("abort 时工具块应收尾为 done: %+v", tools)
	}
	as := blockByKind(t, cp, "assistant")
	for _, b := range as {
		if b.Text == "（已中断）" {
			t.Fatalf("abort 不再追加「（已中断）」文本块: %+v", b)
		}
	}
	dv := blockByKind(t, cp, "divider")
	if len(dv) != 1 || dv[0].DividerReason != "abort" {
		t.Fatalf("abort 应插 divider(reason=abort): %+v", dv)
	}
	if cp.LastRun == nil || cp.LastRun.Status != "interrupted" {
		t.Fatalf("lastRun should be interrupted: %+v", cp.LastRun)
	}
}

// TestReducerTaskLifecycle task 占位/终态/结果回填；结果找不到任务时保留独立结果块。
func TestReducerTaskLifecycle(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, &events.TaskStarted{TaskId: "task-1", Name: "背景调查", Async: true, Timestamp: ts, EventType: events.TaskStartedType})
	mustApply(t, r, &events.TaskStarted{TaskId: "task-1", Name: "背景调查", Async: true, Timestamp: ts, EventType: events.TaskStartedType}) // 重送幂等
	mustApply(t, r, &events.TaskEnd{TaskId: "task-1", Status: "completed", Result: "done", Timestamp: ts.Add(time.Second), EventType: events.TaskEndType})
	mustApply(t, r, &events.TaskResultDelivered{SessionId: "s1", TaskId: "task-1", Status: "completed", Result: "result body", Timestamp: ts.Add(2 * time.Second), EventType: events.TaskResultDeliveredType})
	mustApply(t, r, &events.TaskResultDelivered{SessionId: "s1", TaskId: "task-1", Status: "completed", Result: "result body", Timestamp: ts.Add(2 * time.Second), EventType: events.TaskResultDeliveredType}) // 重送幂等

	cp := r.Snapshot()
	tasks := blockByKind(t, cp, "async_task")
	if len(tasks) != 1 {
		t.Fatalf("async_task blocks = %d, want 1", len(tasks))
	}
	if tasks[0].TaskID != "task-1" || tasks[0].Status != "completed" || tasks[0].TaskResult != "result body" || !tasks[0].DeliveredToMain {
		t.Fatalf("async_task mismatch: %+v", tasks[0])
	}
	if len(blockByKind(t, cp, "task_result")) != 0 {
		t.Fatal("有任务卡片时不应产生独立结果块")
	}
	// 但**交付通知行**必须落进 checkpoint：旧实现只由前端在内存里补，刷新后
	// 「Task … completed」整行消失（用户 2026-10：实时与恢复后不一致）。
	if got := len(blockByKind(t, cp, "task_delivery")); got != 1 {
		t.Fatalf("task_delivery blocks = %d, want 1（有卡片时也要留一条交付通知）", got)
	}

	// 异常顺序：结果先于任何任务事件 → 独立结果块保留（不丢）
	r2 := NewViewReducer()
	mustApply(t, r2, &events.TaskResultDelivered{SessionId: "s1", TaskId: "ghost", Status: "failed", Error: "err", Timestamp: ts, EventType: events.TaskResultDeliveredType})
	cp2 := r2.Snapshot()
	if len(blockByKind(t, cp2, "task_result")) != 1 || len(blockByKind(t, cp2, "task_delivery")) != 1 {
		t.Fatalf("ghost 任务应保留独立结果块: %+v", cp2.Blocks)
	}
}

// TestReducerTaskUsageCost 任务终态携用量（TaskEnd.Usage / TaskResultDelivered.Usage）→
// 卡片记 tokens + 成本（µUSD → USD，cost_usd 随 checkpoint 落盘可恢复）；
// 会话级成本**不**因此变化（卡片级口径，addUsage 不搬 CostUSD，杜绝双计）。
func TestReducerTaskUsageCost(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	u := core.Usage{Input: 1000, Output: 200, CacheRead: 800, TotalTokens: 1200, Cost: core.Cost{Input: 140, Output: 56, Total: 196}}
	mustApply(t, r, &events.TaskStarted{TaskId: "task-u", Name: "审查", Async: true, Timestamp: ts, EventType: events.TaskStartedType})
	mustApply(t, r, &events.TaskEnd{TaskId: "task-u", Status: "completed", Result: "ok", Usage: &u, Timestamp: ts.Add(time.Second), EventType: events.TaskEndType})

	cp := r.Snapshot()
	tasks := blockByKind(t, cp, "async_task")
	if len(tasks) != 1 {
		t.Fatalf("async_task blocks = %d, want 1", len(tasks))
	}
	if tasks[0].Usage.Input != 1000 || tasks[0].Usage.Output != 200 || tasks[0].Usage.CacheRead != 800 {
		t.Fatalf("任务卡应记 tokens: %+v", tasks[0].Usage)
	}
	if abs(tasks[0].Usage.CostUSD-0.000196) > 1e-12 {
		t.Fatalf("任务卡成本（USD）= %v, want 0.000196", tasks[0].Usage.CostUSD)
	}
	if cp.CostUSD != 0 {
		t.Fatalf("卡片成本不得计入会话累计（双计）: %v", cp.CostUSD)
	}

	// 结果回传亦携用量（宿主可能只发 push 路径）：同源覆盖，不叠加
	mustApply(t, r, &events.TaskResultDelivered{SessionId: "s1", TaskId: "task-u", Status: "completed", Result: "ok", Usage: &u, Timestamp: ts.Add(2 * time.Second), EventType: events.TaskResultDeliveredType})
	cp = r.Snapshot()
	tasks = blockByKind(t, cp, "async_task")
	if tasks[0].Usage.Input != 1000 || abs(tasks[0].Usage.CostUSD-0.000196) > 1e-12 {
		t.Fatalf("重复投递不得叠加用量: %+v", tasks[0].Usage)
	}

	// promoted 工具任务：TaskEnd 同样收敛工具行并挂用量
	r2 := NewViewReducer()
	mustApply(t, r2, &events.ToolStart{RunId: "root-1", Id: "call-1", Name: "bash", Arguments: "sleep 120", TaskId: "tooltask-1", Timestamp: ts, EventType: events.ToolRunStartType})
	mustApply(t, r2, &events.TaskPromoted{TaskId: "tooltask-1", Kind: "tool", Name: "bash", Timestamp: ts.Add(time.Second), EventType: events.TaskPromotedType})
	mustApply(t, r2, &events.TaskEnd{TaskId: "tooltask-1", Status: "completed", Result: "done", Usage: &u, Timestamp: ts.Add(2 * time.Second), EventType: events.TaskEndType})
	tools := blockByKind(t, r2.Snapshot(), "tool")
	if len(tools) != 1 || tools[0].TaskStatus != "completed" || tools[0].Usage.Input != 1000 {
		t.Fatalf("promoted 工具行应记任务用量: %+v", tools)
	}
}

// TestReducerCallUsageCost 调用级用量（非任务）也要落到展示层：
//   - ToolResponse.Usage（子 agent 类工具经 ToolUsageProvider 回传）→ 工具行 / 卡片工具项；
//     普通工具（全零）不留空白用量位；
//   - AgentEnd.Usage（整次运行跨轮累计的权威值，含压缩轮与成本）→ 子 agent 卡片
//     （同步子 agent 没有 TaskEnd，这是它唯一的结算来源）；
//   - CompressEnd.Usage → 压缩块（此前只进会话累计、不落块 → 恢复后 tokens 行静默消失）。
func TestReducerCallUsageCost(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	u := core.Usage{Input: 1000, Output: 200, CacheRead: 800, TotalTokens: 1200, Cost: core.Cost{Input: 140, Output: 56, Total: 196}}

	// 主 run 里的一次子 agent 类工具调用（同步形态）
	mustApply(t, r, evAgentStart(t, "root-1", "", "主会话", "", ts))
	mustApply(t, r, evToolStart(t, "root-1", "call-1", "subagent_x", "{}", ts.Add(time.Second)))
	mustApply(t, r, &events.ToolResponse{RunId: "root-1", Id: "call-1", Name: "subagent_x", Result: "done", Usage: &u, Timestamp: ts.Add(2 * time.Second), EventType: events.ToolRunEndType})
	// 普通工具：引擎恒填 Usage 指针（零值）——不得因此写出空用量
	mustApply(t, r, evToolStart(t, "root-1", "call-2", "read_file", "{}", ts.Add(3*time.Second)))
	mustApply(t, r, &events.ToolResponse{RunId: "root-1", Id: "call-2", Name: "read_file", Result: "ok", Usage: &core.Usage{}, Timestamp: ts.Add(4 * time.Second), EventType: events.ToolRunEndType})

	cp := r.Snapshot()
	tools := blockByKind(t, cp, "tool")
	if len(tools) != 2 {
		t.Fatalf("tool blocks = %d, want 2", len(tools))
	}
	var withUsage, plain Block
	for _, b := range tools {
		if b.ToolID == "call-1" {
			withUsage = b
		} else {
			plain = b
		}
	}
	if withUsage.Usage.Input != 1000 || withUsage.Usage.Output != 200 {
		t.Fatalf("工具行应记本次调用 tokens: %+v", withUsage.Usage)
	}
	if abs(withUsage.Usage.CostUSD-0.000196) > 1e-12 {
		t.Fatalf("工具行成本（USD）= %v, want 0.000196", withUsage.Usage.CostUSD)
	}
	if plain.Usage.Input != 0 || plain.Usage.CostUSD != 0 {
		t.Fatalf("普通工具不得写出用量: %+v", plain.Usage)
	}

	// 同步子 agent：AgentStart(父) → AgentEnd.Usage 是卡片权威用量（无 TaskEnd）
	r2 := NewViewReducer()
	mustApply(t, r2, evAgentStart(t, "root-1", "", "主会话", "", ts))
	mustApply(t, r2, evAgentStart(t, "sub-1", "root-1", "调查", "task-7", ts.Add(time.Second)))
	agents := blockByKind(t, r2.Snapshot(), "agent")
	if len(agents) != 1 || agents[0].Usage.Input != 0 {
		t.Fatalf("子 agent 卡片初始用量应为 0: %+v", agents)
	}
	mustApply(t, r2, &events.AgentEnd{RunId: "sub-1", ParentRunId: "root-1", TaskId: "task-7", FinishReason: core.FinishReasonStop, Usage: &u, Timestamp: ts.Add(5 * time.Second), EventType: events.AgentEndType})
	agents = blockByKind(t, r2.Snapshot(), "agent")
	if agents[0].Usage.Input != 1000 || agents[0].Usage.Output != 200 || agents[0].Usage.CacheRead != 800 {
		t.Fatalf("AgentEnd.Usage 应作卡片权威用量: %+v", agents[0].Usage)
	}
	if abs(agents[0].Usage.CostUSD-0.000196) > 1e-12 {
		t.Fatalf("卡片成本（USD）= %v, want 0.000196", agents[0].Usage.CostUSD)
	}

	// 主 run 压缩：用量（含成本）落块（恢复后压缩卡仍显示 tokens/成本）
	r3 := NewViewReducer()
	mustApply(t, r3, evAgentStart(t, "root-1", "", "主会话", "", ts))
	mustApply(t, r3, &events.CompressEnd{Before: 100, After: 20, Reason: "automatic", CtxTokens: 5000, Model: "m1", Summary: "s", Usage: &u, Timestamp: ts.Add(time.Second), EventType: events.CompressEndType})
	comps := blockByKind(t, r3.Snapshot(), "compression")
	if len(comps) != 1 {
		t.Fatalf("compression blocks = %d, want 1", len(comps))
	}
	if comps[0].Usage.Input != 1000 || abs(comps[0].Usage.CostUSD-0.000196) > 1e-12 {
		t.Fatalf("压缩块应落用量+成本: %+v", comps[0].Usage)
	}
}

// TestReducerCompression 主/子压缩收敛：主追加 compression 块；子卡片项收敛。
func TestReducerCompression(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evAgentStart(t, "root-1", "", "主会话", "", ts))
	mustApply(t, r, evAgentStart(t, "sub-1", "root-1", "调查", "task-9", ts.Add(time.Second)))
	mustApply(t, r, &events.CompressStart{Before: 100, RunId: "", EventType: events.CompressStartType})
	mustApply(t, r, &events.CompressEnd{Before: 100, After: 20, Reason: "automatic", CtxTokens: 5000, Model: "m1", Summary: "s", Analysis: "1. TRANSITION: t1 [x]", Usage: &core.Usage{Input: 5000, Output: 200, Cost: core.Cost{Total: 150}}, EventType: events.CompressEndType})
	mustApply(t, r, &events.CompressStart{Before: 50, RunId: "sub-1", EventType: events.CompressStartType})
	mustApply(t, r, &events.CompressEnd{Before: 50, After: 10, Reason: "automatic", RunId: "sub-1", Analysis: "sub analysis", EventType: events.CompressEndType})

	cp := r.Snapshot()
	comps := blockByKind(t, cp, "compression")
	if len(comps) != 1 {
		t.Fatalf("main compression blocks = %d, want 1", len(comps))
	}
	if comps[0].Before != 100 || comps[0].After != 20 || comps[0].CtxTokens != 5000 {
		t.Fatalf("compression block mismatch: %+v", comps[0])
	}
	if comps[0].Analysis != "1. TRANSITION: t1 [x]" {
		t.Fatalf("compression block analysis missing: %+v", comps[0])
	}
	agents := blockByKind(t, cp, "agent")
	if len(agents) != 1 {
		t.Fatalf("agent cards = %d, want 1", len(agents))
	}
	foundComp := false
	for _, it := range agents[0].Items {
		if it.Kind == "compression" && !it.Active && it.After == 10 {
			foundComp = true
			if it.Analysis != "sub analysis" {
				t.Fatalf("sub compression item analysis missing: %+v", it)
			}
		}
	}
	if !foundComp {
		t.Fatalf("sub compression item missing: %+v", agents[0].Items)
	}
	// JSON round-trip：checkpoint 序列化必须保留 analysis（重启恢复链路）
	raw, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(raw, []byte("1. TRANSITION: t1 [x]")) || !bytes.Contains(raw, []byte("sub analysis")) {
		t.Fatalf("checkpoint JSON lost analysis: %s", string(raw[:min(len(raw), 500)]))
	}
	// 压缩调用成本计入会话展示累计（llm_end 不覆盖压缩轮）：µUSD 150 → $0.00015
	if cp.CostUSD != 150.0/1e6 {
		t.Fatalf("compress cost not aggregated: %f, want 0.00015", cp.CostUSD)
	}
}

// TestReducerCtxAnchorSource 上下文锚点口径：llm_end（provider 真实 usage）→ real；
// compress_end（压后字符/4 估算）→ estimated —— 两者写入同一个字段，展示层必须能区分；
// Snapshot → Restore 往返保留口径标记。
func TestReducerCtxAnchorSource(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evAgentStart(t, "root-1", "", "主会话", "", ts))
	mustApply(t, r, &events.LLMEnd{
		RunId: "root-1", RequestId: "q1", Model: "m1",
		Usage: &core.Usage{Input: 3100, Output: 10}, Timestamp: ts, EventType: events.LLMEndType,
	})
	if tok, est := r.CtxAnchor(); tok != 3100 || est {
		t.Fatalf("llm_end 后锚点 = (%d, %v), want (3100, false)", tok, est)
	}
	mustApply(t, r, &events.CompressEnd{
		Before: 100, After: 20, Reason: "automatic", CtxTokens: 800, Model: "m1",
		Timestamp: ts, EventType: events.CompressEndType,
	})
	if tok, est := r.CtxAnchor(); tok != 800 || !est {
		t.Fatalf("compress_end 后锚点 = (%d, %v), want (800, true)", tok, est)
	}
	cp := r.Snapshot()
	if cp.CtxTokens != 800 || !cp.CtxTokensEstimated {
		t.Fatalf("checkpoint 未保留锚点口径: ctx=%d estimated=%v", cp.CtxTokens, cp.CtxTokensEstimated)
	}
	r2 := NewViewReducer()
	if err := r2.Restore(cp); err != nil {
		t.Fatal(err)
	}
	if tok, est := r2.CtxAnchor(); tok != 800 || !est {
		t.Fatalf("restore 后锚点 = (%d, %v), want (800, true)", tok, est)
	}
	// 压缩后的下一轮真实 usage 把口径置回真实
	mustApply(t, r, &events.LLMEnd{
		RunId: "root-1", RequestId: "q2", Model: "m1",
		Usage: &core.Usage{Input: 900, Output: 5}, Timestamp: ts, EventType: events.LLMEndType,
	})
	if tok, est := r.CtxAnchor(); tok != 900 || est {
		t.Fatalf("压缩后新一轮锚点 = (%d, %v), want (900, false)", tok, est)
	}
}

// TestReducerClearGeneration clear 代际 +1；blocks/run/turn/展示 usage 重置；
// todos 与 lifetime 对账值保留。
func TestReducerClearGeneration(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evUserInputs(t, []string{"hello"}, nil, ts))
	mustApply(t, r, evLLMStart(t, "r1", "q1", "deepseek-v4-flash", ts.Add(time.Second)))
	mustApply(t, r, evLLMEnd(t, "r1", "q1", "deepseek-v4-flash", "hi", &core.Usage{Input: 100, Output: 10}, ts.Add(2*time.Second)))
	mustApply(t, r, &events.CommandResult{Name: "clear", Result: "context cleared", Timestamp: ts.Add(3 * time.Second), EventType: events.CommandResultType})

	cp := r.Snapshot()
	if cp.SchemaVersion != 1 {
		t.Fatalf("schema_version = %d, want 1", cp.SchemaVersion)
	}
	if cp.LastRun != nil || cp.LastError != nil {
		t.Fatalf("clear 后 last_run/last_error 应清空: %+v", cp)
	}
	if len(cp.Turns) != 0 || len(cp.RunNodes) != 0 {
		t.Fatalf("clear 后 turns/runNodes 应清空")
	}
	if cp.Usage != (Usage{}) || cp.CostUSD != 0 {
		t.Fatalf("clear 后展示 usage/cost 应归零: %+v", cp.Usage)
	}
	if cp.LifetimeUsage.Input != 100 || cp.LifetimeCostUSD <= 0 {
		t.Fatalf("clear 后 lifetime 应保留: %+v", cp.LifetimeUsage)
	}
	dv := blockByKind(t, cp, "divider")
	if len(dv) != 1 || dv[0].DividerReason != "clear" {
		t.Fatalf("clear 后应只有 divider(clear) 横线: %+v", dv)
	}
	if as := blockByKind(t, cp, "assistant"); len(as) != 0 {
		t.Fatalf("clear 后不应再有 assistant 文本块: %+v", as)
	}
	if r.ClearGeneration() != 1 {
		t.Fatalf("clearGeneration = %d, want 1", r.ClearGeneration())
	}
}

// TestReducerUnknownEventDiagnostics 未知事件保留为受约束 diagnostic，不改变稳定视图。
func TestReducerUnknownEventDiagnostics(t *testing.T) {
	// 未来新增事件类型在旧 version 中不可解析：dispatch 层会以 opaque wrapper 进入。
	// 这里用最小 events.Event 实现模拟（Type 命中 default 分支）。
	opaque := &opaqueEventT{evType: events.EventType("something_new")}
	r := NewViewReducer()
	_ = r.ApplyPlain(opaque)
	_ = r.ApplyPlain(opaque)

	cp := r.Snapshot()
	if len(cp.Blocks) != 0 {
		t.Fatalf("未知事件不得改变稳定视图: %+v", cp.Blocks)
	}
	if len(cp.Diagnostics) != 1 || cp.Diagnostics[0].EventType != "something_new" || cp.Diagnostics[0].Count != 2 {
		t.Fatalf("diagnostics mismatch: %+v", cp.Diagnostics)
	}
}

type opaqueEventT struct{ evType events.EventType }

func (o *opaqueEventT) Type() events.EventType { return o.evType }

// TestReducerSnapshotImmutableAndRestore Snapshot 结果不受后续 Apply 影响；Restore 完整还原。
func TestReducerSnapshotImmutableAndRestore(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evLLMStart(t, "r1", "q1", "m1", ts))
	mustApply(t, r, evContentChunk(t, "r1", "q1", "A", 0, ts.Add(time.Millisecond)))
	mustApply(t, r, evLLMEnd(t, "r1", "q1", "m1", "", &core.Usage{Input: 50, Output: 5}, ts.Add(time.Second)))

	cp := r.Snapshot()
	// 快照之后继续应用事件，已拍快照不得被修改
	seq := r.LastSeq()
	mustApply(t, r, evUserInputs(t, []string{"later"}, nil, ts.Add(2*time.Second)))
	if len(cp.Blocks) != 1 || len(cp.Turns) != 1 {
		t.Fatalf("已拍快照被后续事件修改")
	}
	if r.LastSeq() != seq+1 {
		t.Fatalf("seq = %d, want %d", r.LastSeq(), seq+1)
	}

	// Restore 还原：内存去重集合清空，后续事件可正常应用
	r2 := NewViewReducer()
	if err := r2.Restore(cp); err != nil {
		t.Fatal(err)
	}
	cp2 := r2.Snapshot()
	if len(cp2.Blocks) != 1 || cp2.Blocks[0].Text != "A" || cp2.Usage.Input != 50 {
		t.Fatalf("restore 后视图不一致: %+v", cp2)
	}
	if r2.LastSeq() != 0 {
		t.Fatalf("restore 应重置 seq 基线（由 dispatcher 设定）")
	}
}

// TestReducerNormalizeForRestore 恢复归一化：running → 终态（子 agent/任务 abandoned）。
func TestReducerNormalizeForRestore(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evAgentStart(t, "root-1", "", "主会话", "", ts))
	mustApply(t, r, evAgentStart(t, "sub-1", "root-1", "调查", "task-9", ts.Add(time.Second)))
	mustApply(t, r, &events.TaskStarted{TaskId: "task-10", Name: "占位", Async: true, Timestamp: ts.Add(2 * time.Second), EventType: events.TaskStartedType})
	mustApply(t, r, evToolStart(t, "root-1", "call-1", "bash", "sleep", ts.Add(3*time.Second)))

	cp := r.Snapshot()
	// 快照时 run 仍 running（crash 前最后一个 commit 边界场景）
	r2 := NewViewReducer()
	if err := r2.Restore(cp); err != nil {
		t.Fatal(err)
	}
	r2.NormalizeForRestore()
	ncp := r2.Snapshot()

	for _, n := range ncp.RunNodes {
		if n.Status == "running" {
			t.Fatalf("恢复归一化后不应有 running run node: %+v", n)
		}
	}
	for _, b := range ncp.Blocks {
		switch b.Kind {
		case blockKindAgent, blockKindAsyncTask:
			if b.Status == "running" || b.Status == "interrupting" {
				t.Fatalf("恢复归一化后 %s 块应为 abandoned: %+v", b.Kind, b)
			}
		case blockKindTool:
			if b.Status == "running" {
				t.Fatalf("恢复归一化后 tool 块应为 done: %+v", b)
			}
			// 僵尸后台任务卡（status=done + promoted + task_status=running）：
			// 后台任务随进程关闭而亡，恢复时必须收敛为 abandoned——此前漏收敛该形态，
			// 僵尸「执行中」卡经 checkpoint 代代固化（2026-08-30 实证 8 条）。
			if b.Promoted && b.TaskID != "" && (b.TaskStatus == "running" || b.TaskStatus == "interrupting") {
				t.Fatalf("恢复归一化后 promoted tool 块 task_status 应为 abandoned: %+v", b)
			}
		}
	}
}

// TestReducerNormalizeForRestoreZombiePromotedTool 僵尸后台任务卡收敛（2026-08-30 实证场景）：
// 已结算 tool 块（status=done）+ promoted + task_status=running 的组合——后台任务早随
// 进程关闭而亡，但 taskStatus 残留 running（如跨代 id 复用被新 promote 事件误翻转）。
// 恢复归一化必须把它收敛为 abandoned，否则经 checkpoint 代代固化成「一直执行中」的僵尸卡。
func TestReducerNormalizeForRestoreZombiePromotedTool(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evAgentStart(t, "root-1", "", "主会话", "", ts))
	// 真实链路：ToolStart 自带 task_id（引擎批内生成），promote 事件按它匹配工具行
	mustApply(t, r, &events.ToolStart{RunId: "root-1", Id: "call-1", Name: "bash", Arguments: "sleep 120", TaskId: "tooltask-1", Timestamp: ts.Add(time.Second), EventType: events.ToolRunStartType})
	// promote 翻转（promoted=true + taskStatus=running）。此后进程死亡：后台任务的
	// tool_run_end / task_result_delivered 都不会再到达 → checkpoint 里固化为
	// status=done（start 块的默认结算形态）+ task_status=running 的僵尸卡。
	mustApply(t, r, &events.TaskPromoted{TaskId: "tooltask-1", Kind: "tool", Name: "bash", Timestamp: ts.Add(2 * time.Second), EventType: events.TaskPromotedType})

	cp := r.Snapshot()
	r2 := NewViewReducer()
	if err := r2.Restore(cp); err != nil {
		t.Fatal(err)
	}
	r2.NormalizeForRestore()
	ncp := r2.Snapshot()

	found := false
	for _, b := range ncp.Blocks {
		if b.Kind == blockKindTool && b.TaskID == "tooltask-1" {
			found = true
			if b.TaskStatus != statusAbandoned {
				t.Fatalf("僵尸后台任务卡应为 abandoned, got task_status=%q: %+v", b.TaskStatus, b)
			}
		}
	}
	if !found {
		t.Fatal("未找到 promoted tool 块")
	}
}

// TestReducerMarshalMatchesStorageEnvelope Marshal 产物满足 storage 层 envelope 校验
// （schema_version=1、blocks/run_nodes/turns 为非 null 数组）。
func TestReducerMarshalMatchesStorageEnvelope(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evUserInputs(t, []string{"hi"}, nil, ts))
	data, err := r.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		t.Fatal(err)
	}
	var v int
	if err := json.Unmarshal(obj["schema_version"], &v); err != nil || v != 1 {
		t.Fatalf("schema_version = %d, want 1", v)
	}
	for _, field := range []string{"blocks", "run_nodes", "turns"} {
		raw, ok := obj[field]
		if !ok {
			t.Fatalf("缺少字段 %s", field)
		}
		var arr []any
		if err := json.Unmarshal(raw, &arr); err != nil {
			t.Fatalf("%s 不是数组: %s", field, raw)
		}
	}
	// 空 reducer 也必须产生合法 envelope
	empty, err := NewViewReducer().Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(empty), `"blocks":[]`) {
		t.Fatalf("空 checkpoint 的 blocks 应为 []: %s", empty)
	}
}

// TestReducerConcurrentApplyAndSnapshot 并发 Apply/Snapshot/Marshal -race 验证：
// per-session dispatcher 串行是调用方契约；reducer 内部互斥锁是防御层，
// 保证误用（并发直接调用）时不产生数据竞争/半写快照。
func TestReducerConcurrentApplyAndSnapshot(t *testing.T) {
	r := NewViewReducer()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			runID := fmt.Sprintf("r-%d", n)
			for j := 0; j < 100; j++ {
				req := fmt.Sprintf("q-%d-%d", n, j)
				_ = r.ApplyPlain(&events.LLMStart{RunId: runID, RequestId: req, Model: "m", Timestamp: time.Now(), EventType: events.LLMStartType})
				_ = r.ApplyPlain(&events.ContentChunk{RunId: runID, RequestId: req, Index: 0, Content: "x", Timestamp: time.Now(), EventType: events.ContentChunkType})
				_ = r.ApplyPlain(&events.LLMEnd{RunId: runID, RequestId: req, Model: "m", Content: "x", Usage: &core.Usage{Input: 1}, FinishReason: core.FinishReasonStop, Timestamp: time.Now(), EventType: events.LLMEndType})
				cp := r.Snapshot() // 并发快照：不得与 Apply 交错产生半写
				if cp == nil || cp.SchemaVersion != checkpointSchemaVersion {
					t.Errorf("snapshot 不完整: %+v", cp)
				}
				_, _ = r.Marshal()
			}
		}(i)
	}
	wg.Wait()
	cp := r.Snapshot()
	if len(cp.Turns) != 800 {
		t.Fatalf("turns = %d, want 800（每个 llm_end 独立结算，并发不影响计数）", len(cp.Turns))
	}
	if cp.Usage.Input != 800 {
		t.Fatalf("usage = %+v, want input=800", cp.Usage)
	}
}

// TestCoordinatorConcurrentRequest 并发 Request（sync 模式，多个 emit goroutine）
// 由 commitOnce 串行化：全部成功提交、revision 单调、无 -race。
func TestCoordinatorConcurrentRequest(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("HOME", home)
	appDataDirOverride = dataDir
	t.Cleanup(func() { appDataDirOverride = "" })

	store, err := newRecordStore()
	if err != nil {
		t.Fatal(err)
	}
	reducer := NewViewReducer()
	stopCh := make(chan struct{})
	coord := newCheckpointCoordinator("ws-c", "s1", store, reducer, nil, stopCh, true)

	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				req := fmt.Sprintf("q-%d-%d", n, j)
				_ = reducer.ApplyPlain(&events.LLMEnd{RunId: "r", RequestId: req, Model: "m", Usage: &core.Usage{Input: 1}, FinishReason: core.FinishReasonStop, Timestamp: time.Now(), EventType: events.LLMEndType})
				coord.Request()
			}
		}(i)
	}
	wg.Wait()
	commits, skipped := coord.Stats()
	if commits == 0 {
		t.Fatalf("并发提交 0 次: skipped=%d", skipped)
	}
	rec, err := coord.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rec.Revision != commits {
		t.Fatalf("revision = %d, want %d（单调且与提交次数一致）", rec.Revision, commits)
	}
	close(stopCh)
}

// TestReducerDanglingLLMStartNormalized llm_start 无 llm_end（中断/崩溃残留）：
// Snapshot 时完全空壳 assistant 块（无正文/思考/未结算）→ 从投影移除（不展示任何东西，
// 不标「已中断」）；有思考内容的块保留；内存不残留 reqState/thinkStart。
func TestReducerDanglingLLMStartNormalized(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evLLMStart(t, "r1", "q1", "m1", ts))
	mustApply(t, r, evReasoningChunk(t, "r1", "q1", "思考中", 0, ts.Add(time.Millisecond)))
	// 无 llm_end：Snapshot 归一化
	cp := r.Snapshot()
	as := blockByKind(t, cp, "assistant")
	if len(as) != 1 {
		t.Fatalf("assistant 块 = %d, want 1", len(as))
	}
	// 有思考内容 → 不算空壳（保留思考）；正文为空可接受（思考模型中断）
	if as[0].Thinking != "思考中" {
		t.Fatalf("思考内容应保留: %+v", as[0])
	}
	if as[0].Text != "" {
		t.Fatalf("中断轮无正文, text = %q", as[0].Text)
	}

	// 完全空壳（无 chunk）：从投影移除（不展示任何东西）
	r2 := NewViewReducer()
	mustApply(t, r2, evLLMStart(t, "r2", "q2", "m1", ts))
	cp2 := r2.Snapshot()
	as2 := blockByKind(t, cp2, "assistant")
	if len(as2) != 0 {
		t.Fatalf("空壳 assistant 应从投影移除: %+v", as2)
	}
	// 内存不残留：后续 llm_end 不应再产生第二块（reqState 仍指向原块）
	mustApply(t, r2, evLLMEnd(t, "r2", "q2", "m1", "补全", nil, ts.Add(time.Second)))
	cp3 := r2.Snapshot()
	as3 := blockByKind(t, cp3, "assistant")
	if len(as3) != 1 {
		t.Fatalf("llm_end 迟到应结算到原块而非新建: %+v", as3)
	}
	if as3[0].Text != "补全" {
		t.Fatalf("迟到 llm_end 应补全正文: %+v", as3[0])
	}
}

// TestReducerDanglingToolStartNormalized tool_start 无 tool_end：Snapshot 收敛 done
// （有 task 归属 → abandoned）；迟到 tool_end 仍更新同一块（不新建）。
func TestReducerDanglingToolStartNormalized(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evAgentStart(t, "root-1", "", "主会话", "", ts))
	mustApply(t, r, evToolStart(t, "root-1", "call-1", "bash", "sleep", ts.Add(time.Second)))
	// 无 tool_end：快照归一化（内存仍 running）
	cp := r.Snapshot()
	tools := blockByKind(t, cp, "tool")
	if len(tools) != 1 || tools[0].Status != "done" {
		t.Fatalf("悬空 tool 应归一化 done: %+v", tools)
	}
	// 迟到 tool_end 更新同一块
	mustApply(t, r, evToolEnd(t, "root-1", "call-1", "bash", "finished", false, nil, ts.Add(2*time.Second)))
	cp2 := r.Snapshot()
	tools2 := blockByKind(t, cp2, "tool")
	if len(tools2) != 1 || tools2[0].Result != "finished" || tools2[0].Status != "done" || tools2[0].DurMs != 1000 {
		t.Fatalf("迟到 tool_end 应更新原块: %+v", tools2)
	}

	// promoted 后台任务悬空：TaskID 归属 → abandoned
	r3 := NewViewReducer()
	mustApply(t, r3, evAgentStart(t, "root-1", "", "主会话", "", ts))
	mustApply(t, r3, &events.ToolStart{RunId: "root-1", Id: "call-2", Name: "long_tool", TaskId: "task-9", Timestamp: ts, EventType: events.ToolRunStartType})
	cp3 := r3.Snapshot()
	tools3 := blockByKind(t, cp3, "tool")
	if len(tools3) != 1 || tools3[0].Status != "done" || tools3[0].TaskStatus != "abandoned" {
		t.Fatalf("promoted 悬空 tool 应为 done+abandoned: %+v", tools3)
	}
}

// TestReducerDanglingAgentStartNormalized agent_start 无 agent_end：
// 实时 Snapshot 保留 running（中间提交不误标）；重启 NormalizeForRestore 收敛
// abandoned + ended_at + run 节点 done。
func TestReducerDanglingAgentStartNormalized(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evAgentStart(t, "root-1", "", "主会话", "", ts))
	mustApply(t, r, evAgentStart(t, "sub-1", "root-1", "调查", "task-9", ts.Add(time.Second)))
	mustApply(t, r, evToolStart(t, "root-1", "call-1", "bash", "sleep", ts.Add(2*time.Second)))
	// 中间边界提交（tool_end 触发）：agent 仍在 running —— 不得误标 abandoned
	mustApply(t, r, evToolEnd(t, "root-1", "call-1", "bash", "done", false, nil, ts.Add(3*time.Second)))
	cp := r.Snapshot()
	agents := blockByKind(t, cp, "agent")
	if len(agents) != 1 || agents[0].Status != statusRunning {
		t.Fatalf("实时提交不得把运行中子 agent 标成 abandoned: %+v", agents)
	}
	for _, n := range cp.RunNodes {
		if n.ID == "root-1" && n.Status != statusRunning {
			t.Fatalf("实时提交不得把运行中主 run 标成 done: %+v", n)
		}
	}

	// 重启恢复：NormalizeForRestore 收敛
	r2 := NewViewReducer()
	if err := r2.Restore(cp); err != nil {
		t.Fatal(err)
	}
	r2.NormalizeForRestore()
	ncp := r2.Snapshot()
	agents2 := blockByKind(t, ncp, "agent")
	if len(agents2) != 1 || agents2[0].Status != statusAbandoned || agents2[0].EndedAt == 0 {
		t.Fatalf("重启后 agent 应为 abandoned + ended_at: %+v", agents2)
	}
	for _, n := range ncp.RunNodes {
		if n.ID == "root-1" && n.Status != statusDone {
			t.Fatalf("重启后主 run 应为 done: %+v", n)
		}
	}
}

// TestReducerToolOnlyTurnNotMarkedInterrupted 纯工具调用轮（llm_end 有 tool_calls
// 无正文）：已结算的空 assistant 块不得被归一化成「（已中断）」。
func TestReducerToolOnlyTurnNotMarkedInterrupted(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evLLMStart(t, "r1", "q1", "m1", ts))
	// llm_end 带 tool_calls、无正文/思考（纯工具轮）
	mustApply(t, r, &events.LLMEnd{
		RunId: "r1", RequestId: "q1", Model: "m1",
		ToolCalls:    []core.ToolCall{{Id: "call-1", Name: "bash", Arguments: "{}"}},
		FinishReason: core.FinishReasonToolCall, Usage: &core.Usage{Input: 10},
		Timestamp: ts.Add(time.Second), EventType: events.LLMEndType,
	})
	cp := r.Snapshot()
	as := blockByKind(t, cp, "assistant")
	if len(as) != 1 {
		t.Fatalf("assistant 块 = %d, want 1", len(as))
	}
	if as[0].Text != "" {
		t.Fatalf("纯工具轮空正文不得被标成已中断: %+v", as[0])
	}
}

// TestReducerDanglingCompressStartNormalized compress_start 无 compress_end：
// Snapshot 时主压缩瞬态已重置（mainCompressing 不持久化）；子 agent 压缩项收敛 aborted。
func TestReducerDanglingCompressStartNormalized(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evAgentStart(t, "root-1", "", "主会话", "", ts))
	mustApply(t, r, evAgentStart(t, "sub-1", "root-1", "调查", "task-9", ts.Add(time.Second)))
	mustApply(t, r, &events.CompressStart{Before: 100, RunId: "", EventType: events.CompressStartType})
	mustApply(t, r, &events.CompressStart{Before: 50, RunId: "sub-1", EventType: events.CompressStartType})

	cp := r.Snapshot()
	// 主：无 compression 块（start 未结算不产生块）
	if len(blockByKind(t, cp, "compression")) != 0 {
		t.Fatalf("主压缩 start 无 end 不应产生块: %+v", cp.Blocks)
	}
	// 子：卡片内 active 压缩项 → 归一化 aborted（normalizeSnapshotBlocks）
	agents := blockByKind(t, cp, "agent")
	if len(agents) != 1 {
		t.Fatalf("agent 卡 = %d, want 1", len(agents))
	}
	if len(agents[0].Items) != 1 || agents[0].Items[0].Kind != "compression" || agents[0].Items[0].Active || !agents[0].Items[0].Aborted {
		t.Fatalf("子压缩项应收敛 aborted: %+v", agents[0].Items)
	}
}

// TestCoordinatorThrottleMerge 节流合并：连续边界请求在 debounce 窗口内合并为
// 一次提交（revision 只 +1 一次）；窗口后新请求触发下一次提交。
// 用异步模式（syncMode=false）+ 关闭 debounce 计时器控制窗口。
func TestCoordinatorThrottleMerge(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("HOME", home)
	appDataDirOverride = dataDir
	t.Cleanup(func() { appDataDirOverride = "" })

	store, err := newRecordStore()
	if err != nil {
		t.Fatal(err)
	}
	reducer := NewViewReducer()
	stopCh := make(chan struct{})
	coord := newCheckpointCoordinator("ws-t", "s1", store, reducer, nil, stopCh, false)

	// 连续 5 个边界请求（同一窗口内）：应合并为一次提交
	for i := 0; i < 5; i++ {
		req := fmt.Sprintf("q-%d", i)
		_ = reducer.ApplyPlain(&events.LLMEnd{RunId: "r", RequestId: req, Model: "m", Usage: &core.Usage{Input: 1}, FinishReason: core.FinishReasonStop, Timestamp: time.Now(), EventType: events.LLMEndType})
		coord.Request()
	}
	// 等待 debounce 窗口结束（800ms）+ worker 提交完成
	time.Sleep(commitDebounce + 300*time.Millisecond)
	rec, err := coord.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rec.Revision != 1 {
		t.Fatalf("窗口内 5 个请求应合并为 1 次提交, revision = %d, want 1", rec.Revision)
	}
	if rec.ReducerSeq < 5 {
		t.Fatalf("reducer_seq = %d, want >= 5（快照包含窗口内全部事件）", rec.ReducerSeq)
	}
	commits, _ := coord.Stats()
	if commits != 1 {
		t.Fatalf("commits = %d, want 1（合并）", commits)
	}

	// 新窗口请求 → 第二次提交（轮询等待：第一次 commit 可能还在持锁，窗口从
	// worker 处理入队后才开始，需覆盖 提交耗时 + debounce 窗口）
	_ = reducer.ApplyPlain(&events.LLMEnd{RunId: "r", RequestId: "q-last", Model: "m", Usage: &core.Usage{Input: 1}, FinishReason: core.FinishReasonStop, Timestamp: time.Now(), EventType: events.LLMEndType})
	coord.Request()
	deadline := time.Now().Add(commitDebounce + 2*time.Second)
	for time.Now().Before(deadline) {
		if rec2, err := coord.Load(context.Background()); err == nil && rec2.Revision == 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	rec2, err := coord.Load(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if rec2.Revision != 2 {
		t.Fatalf("新窗口应提交 revision 2, got %d", rec2.Revision)
	}
	close(stopCh)
	time.Sleep(50 * time.Millisecond)
}

// TestCoordinatorStopDrainsRemaining stop 时立即提交队列剩余（不丢最后状态）：
// 请求入队后未等窗口结束就关闭，coord.done 前 record 应已包含该事件。
func TestCoordinatorStopDrainsRemaining(t *testing.T) {
	home := t.TempDir()
	dataDir := t.TempDir()
	t.Setenv("HOME", home)
	appDataDirOverride = dataDir
	t.Cleanup(func() { appDataDirOverride = "" })

	store, err := newRecordStore()
	if err != nil {
		t.Fatal(err)
	}
	reducer := NewViewReducer()
	stopCh := make(chan struct{})
	coord := newCheckpointCoordinator("ws-s", "s1", store, reducer, nil, stopCh, false)

	_ = reducer.ApplyPlain(&events.LLMEnd{RunId: "r", RequestId: "q1", Model: "m", Usage: &core.Usage{Input: 1}, FinishReason: core.FinishReasonStop, Timestamp: time.Now(), EventType: events.LLMEndType})
	coord.Request()
	// 立即关闭（不等 800ms 窗口）→ stop 分支应 drain 提交
	close(stopCh)
	select {
	case <-coord.done:
	case <-time.After(3 * time.Second):
		t.Fatal("coord 未在 stop 后退出（drain 阻塞）")
	}
	rec, err := coord.Load(context.Background())
	if err != nil {
		t.Fatalf("stop 后应有提交: %v", err)
	}
	if rec.Revision != 1 {
		t.Fatalf("stop drain 应提交 revision 1, got %d", rec.Revision)
	}
}

// TestReducerAssistantBlockFillsFromLLMEnd 无 chunk 事件时，assistant 块正文由 llm_end 补全。
func TestReducerAssistantBlockFillsFromLLMEnd(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evLLMStart(t, "r1", "q1", "m1", ts))
	mustApply(t, r, evLLMEnd(t, "r1", "q1", "m1", "完整回复", &core.Usage{Input: 10}, ts.Add(time.Second)))

	cp := r.Snapshot()
	as := blockByKind(t, cp, "assistant")
	if len(as) != 1 || as[0].Text != "完整回复" {
		t.Fatalf("assistant 块应由 llm_end 补全: %+v", as)
	}
}

// TestReducerSubAgentStreaming 子 agent chunk 归并到卡片 items（连续同类型合并）。
func TestReducerSubAgentStreaming(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evAgentStart(t, "root-1", "", "主会话", "", ts))
	mustApply(t, r, evAgentStart(t, "sub-1", "root-1", "调查", "task-9", ts.Add(time.Second)))
	mustApply(t, r, evLLMStart(t, "sub-1", "sq1", "m1", ts.Add(2*time.Second)))
	mustApply(t, r, evContentChunk(t, "sub-1", "sq1", "A", 0, ts.Add(2500*time.Millisecond)))
	mustApply(t, r, evContentChunk(t, "sub-1", "sq1", "B", 1, ts.Add(2600*time.Millisecond)))
	mustApply(t, r, evLLMEnd(t, "sub-1", "sq1", "m1", "", &core.Usage{Input: 30, Output: 3}, ts.Add(3*time.Second)))

	cp := r.Snapshot()
	agents := blockByKind(t, cp, "agent")
	if len(agents) != 1 {
		t.Fatalf("agent cards = %d, want 1", len(agents))
	}
	// 主叙述区不应有 sub 的 assistant 块（只有卡片）
	if len(blockByKind(t, cp, "assistant")) != 0 {
		t.Fatalf("子 agent 流式内容不应出现在主叙述区")
	}
	var texts []string
	for _, it := range agents[0].Items {
		if it.Kind == "text" {
			texts = append(texts, it.Text)
		}
	}
	if len(texts) != 1 || texts[0] != "AB" {
		t.Fatalf("子 agent 文本应为合并的 AB: %+v", texts)
	}
	if agents[0].Usage.Input != 30 || agents[0].Usage.Output != 3 {
		t.Fatalf("子 agent 卡片 usage 未累计: %+v", agents[0].Usage)
	}
}

// TestReducerMainRunAgentDurMs AgentEnd 结算主 run 最终 assistant 块的 AgentDurMs
// （AgentStart→AgentEnd 总时长，供前端「Agent 总耗时」展示）。
func TestReducerMainRunAgentDurMs(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evAgentStart(t, "root-1", "", "主会话", "", ts))
	mustApply(t, r, evLLMStart(t, "root-1", "q1", "m1", ts.Add(time.Second)))
	mustApply(t, r, evContentChunk(t, "root-1", "q1", "hello", 0, ts.Add(1500*time.Millisecond)))
	mustApply(t, r, evLLMEnd(t, "root-1", "q1", "m1", "hello", &core.Usage{Input: 100, Output: 10}, ts.Add(2*time.Second)))
	// AgentEnd 携带整轮运行时长（AgentStart→AgentEnd）
	mustApply(t, r, &events.AgentEnd{
		RunId: "root-1", FinishReason: core.FinishReasonStop,
		Timestamp: ts.Add(5 * time.Second), DurationMS: 5000,
		EventType: events.AgentEndType,
	})

	cp := r.Snapshot()
	as := blockByKind(t, cp, "assistant")
	if len(as) != 1 {
		t.Fatalf("assistant blocks = %d, want 1", len(as))
	}
	if as[0].AgentDurMs != 5000 {
		t.Fatalf("AgentDurMs = %d, want 5000（Agent 总耗时应结算到最终 assistant 块）", as[0].AgentDurMs)
	}
	if as[0].DurMs != 1000 {
		t.Fatalf("DurMs = %d, want 1000（LLM 总耗时应保持 LLMEnd 结算值）", as[0].DurMs)
	}
}

// TestReducerAgentDurMsWithNextRunStarted run A 结束（AgentEnd）时 run B 已开始：
// AgentDurMs 必须结算到 run A 的块，而不是全局最后一条（run B 的）块。
// 实测缺陷：多轮对话中后续 run 已追加新块，lastAssistantBlockLocked 指向新 run 块
// → 旧 run 块 agent_dur_ms 恒为 None。
func TestReducerAgentDurMsWithNextRunStarted(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	// run A：完整一轮（llm_start → chunk → llm_end → agent_end with DurationMS）
	mustApply(t, r, evAgentStart(t, "run-a", "", "主会话", "", ts))
	mustApply(t, r, evLLMStart(t, "run-a", "qa1", "m1", ts.Add(time.Second)))
	mustApply(t, r, evContentChunk(t, "run-a", "qa1", "A的回复", 0, ts.Add(1500*time.Millisecond)))
	mustApply(t, r, evLLMEnd(t, "run-a", "qa1", "m1", "A的回复", &core.Usage{Input: 100, Output: 10}, ts.Add(2*time.Second)))
	mustApply(t, r, &events.AgentEnd{
		RunId: "run-a", FinishReason: core.FinishReasonStop,
		Timestamp: ts.Add(5 * time.Second), DurationMS: 5000,
		EventType: events.AgentEndType,
	})
	// run B：已开始（新 llm_start + chunk，尚未结束）
	mustApply(t, r, evAgentStart(t, "run-b", "", "主会话", "", ts.Add(6*time.Second)))
	mustApply(t, r, evLLMStart(t, "run-b", "qb1", "m1", ts.Add(7*time.Second)))
	mustApply(t, r, evContentChunk(t, "run-b", "qb1", "B的回复", 0, ts.Add(7500*time.Millisecond)))

	cp := r.Snapshot()
	var aDur, bDur int64
	for _, b := range cp.Blocks {
		if b.Kind != blockKindAssistant {
			continue
		}
		switch b.RunID {
		case "run-a":
			aDur = b.AgentDurMs
		case "run-b":
			bDur = b.AgentDurMs
		}
	}
	if aDur != 5000 {
		t.Fatalf("run-a AgentDurMs = %d, want 5000（必须结算到 run-a 的块）", aDur)
	}
	if bDur != 0 {
		t.Fatalf("run-b AgentDurMs = %d, want 0（未结束的 run 不应有值）", bDur)
	}
}

// TestReducerAgentEndNoLLMEndFallbackDurMs 末轮没有 llm_end（中断 / run 级失败 /
// 达轮数上限截断）时，该轮正文块仍拿不到任何耗时指标 —— 前端底部指标行的门
// （ttft/dur 缺一即整行不渲染）因此整行消失，刷新后依旧缺。
// AgentEnd 必须用 llm_start→AgentEnd 的已流逝时间兜底 DurMs；TtftMs 不补
// （首字超时那类一个 chunk 都没来过，如实留空，不编造）。
func TestReducerAgentEndNoLLMEndFallbackDurMs(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evAgentStart(t, "root-a", "", "主会话", "", ts))
	// 第 1 轮：正常结算
	mustApply(t, r, evLLMStart(t, "root-a", "q1", "m1", ts.Add(time.Second)))
	mustApply(t, r, evContentChunk(t, "root-a", "q1", "第一轮", 0, ts.Add(1500*time.Millisecond)))
	mustApply(t, r, evLLMEnd(t, "root-a", "q1", "m1", "第一轮", &core.Usage{Input: 100, Output: 10}, ts.Add(2*time.Second)))
	// 第 2 轮：用户中断 —— llm_start + 半截正文，**没有 llm_end**
	mustApply(t, r, evLLMStart(t, "root-a", "q2", "m1", ts.Add(3*time.Second)))
	mustApply(t, r, evContentChunk(t, "root-a", "q2", "说到一半", 0, ts.Add(4*time.Second)))
	mustApply(t, r, &events.AgentEnd{
		RunId: "root-a", FinishReason: core.FinishReasonAbort,
		Timestamp: ts.Add(8 * time.Second), DurationMS: 8000,
		EventType: events.AgentEndType,
	})

	cp := r.Snapshot()
	var partial Block
	for _, b := range cp.Blocks {
		if b.Kind == blockKindAssistant && b.Text == "说到一半" {
			partial = b
		}
	}
	if partial.ID == 0 {
		t.Fatalf("未找到末轮正文块：%+v", cp.Blocks)
	}
	if partial.DurMs != 5000 {
		t.Fatalf("末轮 DurMs = %d, want 5000（llm_start→AgentEnd 的已流逝时间兜底）", partial.DurMs)
	}
	if partial.TtftMs != 1000 {
		t.Fatalf("末轮 TtftMs = %d, want 1000（首 chunk 已到达，TTFT 应由 chunk 分支结算）", partial.TtftMs)
	}
	if partial.AgentDurMs != 8000 {
		t.Fatalf("末轮 AgentDurMs = %d, want 8000", partial.AgentDurMs)
	}
}

// TestReducerAgentEndFallbackSkipsSettledTurn 已结算的轮次不被兜底覆盖：
// llm_end 已给出 DurMs 的块，AgentEnd 兜底不得改写（第 1 轮仍应是 1000ms）。
func TestReducerAgentEndFallbackSkipsSettledTurn(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, evAgentStart(t, "root-b", "", "主会话", "", ts))
	mustApply(t, r, evLLMStart(t, "root-b", "q1", "m1", ts.Add(time.Second)))
	mustApply(t, r, evContentChunk(t, "root-b", "q1", "正文", 0, ts.Add(1500*time.Millisecond)))
	mustApply(t, r, evLLMEnd(t, "root-b", "q1", "m1", "正文", &core.Usage{Input: 100, Output: 10}, ts.Add(2*time.Second)))
	mustApply(t, r, &events.AgentEnd{
		RunId: "root-b", FinishReason: core.FinishReasonStop,
		Timestamp: ts.Add(9 * time.Second), DurationMS: 9000,
		EventType: events.AgentEndType,
	})

	cp := r.Snapshot()
	as := blockByKind(t, cp, "assistant")
	if len(as) != 1 {
		t.Fatalf("assistant blocks = %d, want 1", len(as))
	}
	if as[0].DurMs != 1000 {
		t.Fatalf("DurMs = %d, want 1000（已结算轮次不得被 AgentEnd 兜底改写）", as[0].DurMs)
	}
}

// TestReducerTaskDeliveryOrderedByTime 交付通知按**事件时刻**落位，不是一律追加到末尾。
// 用户 2026-10 的形态：任务结果推回会话后主 run 自动续跑（新 AgentStart 的块已经在前面），
// 任务完成的通知必须在它之前 —— 前端实时路径已按此落位，checkpoint 必须同序。
func TestReducerTaskDeliveryOrderedByTime(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Hour)
	mustApply(t, r, &events.TaskStarted{TaskId: "task-ord", Name: "bg", Async: true, Timestamp: ts, EventType: events.TaskStartedType})
	// 新一次运行的块：时刻晚于任务完成事件
	mustApply(t, r, &events.ToolStart{Id: "call-late", Name: "bash", Arguments: "{}", RunId: "run-2", Timestamp: ts.Add(10 * time.Second), EventType: events.ToolRunStartType})
	mustApply(t, r, &events.TaskResultDelivered{SessionId: "s1", TaskId: "task-ord", Status: "completed", Result: "done", Timestamp: ts.Add(5 * time.Second), EventType: events.TaskResultDeliveredType})

	cp := r.Snapshot()
	deliveryIdx, toolIdx := -1, -1
	for i, b := range cp.Blocks {
		if b.Kind == "task_delivery" && b.TaskID == "task-ord" {
			deliveryIdx = i
		}
		if b.Kind == "tool" && b.ToolID == "call-late" {
			toolIdx = i
		}
	}
	if deliveryIdx < 0 || toolIdx < 0 {
		t.Fatalf("块缺失：delivery=%d tool=%d", deliveryIdx, toolIdx)
	}
	if deliveryIdx > toolIdx {
		t.Fatalf("交付通知必须排在更晚的块之前（delivery@%d, tool@%d）", deliveryIdx, toolIdx)
	}
}

// P2-04：hub_task header 是宿主协议，不进用户气泡（正文是紧随的 text 块）。
func TestReducerUserInputsSkipsHubTaskHeader(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Minute)
	header := `{"task_id":"task_x","origin":{"kind":"dm","agent_id":"ag_1"}}`
	mustApply(t, r, evUserInputs(t, nil, [][]core.Content{{
		{Type: core.ContentTypeHubTask, Content: header},
		{Type: core.ContentTypeText, Content: "做一份对比表"},
	}}, ts))
	users := blockByKind(t, r.Snapshot(), "user")
	if len(users) != 1 {
		t.Fatalf("want 1 user block, got %d", len(users))
	}
	if users[0].Text != "做一份对比表" {
		t.Fatalf("user text = %q (header leaked?)", users[0].Text)
	}
	for _, c := range users[0].Contents {
		if strings.Contains(c.Content, "task_x") || c.Type == core.ContentTypeHubTask {
			t.Fatalf("header content leaked into block: %+v", users[0].Contents)
		}
	}
}

// P3b-01a：hub_group_wake hidden 块（header JSON + 群快照）不能进用户气泡。
func TestReducerUserInputsSkipsHubGroupWake(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Minute)
	hidden := `{"group_id":"grp_x","agent_id":"ag_x","round_id":"rnd_x","trigger_seq":7}` + "\n\n群：测试群\n#7 人: @你 看一下"
	mustApply(t, r, evUserInputs(t, nil, [][]core.Content{{
		{Type: core.ContentTypeHubGroupWake, Content: hidden},
		{Type: core.ContentTypeText, Content: "@你 看一下"},
	}}, ts))
	users := blockByKind(t, r.Snapshot(), "user")
	if len(users) != 1 {
		t.Fatalf("want 1 user block, got %d", len(users))
	}
	if users[0].Text != "@你 看一下" {
		t.Fatalf("user text = %q (snapshot leaked?)", users[0].Text)
	}
	for _, c := range users[0].Contents {
		if strings.Contains(c.Content, "grp_x") || strings.Contains(c.Content, "群：测试群") {
			t.Fatalf("group wake hidden content leaked: %+v", users[0].Contents)
		}
	}
}
