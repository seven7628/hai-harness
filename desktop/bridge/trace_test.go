package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// traceTestBase 测试用时间基准。
var traceTestBase = time.Date(2026, 9, 10, 12, 0, 0, 0, time.UTC)

// timeAt 基准 + n 毫秒。
func timeAt(n int) time.Time { return traceTestBase.Add(time.Duration(n) * time.Millisecond) }

// ev 构造一行事件 JSON（测试用；只填需要的字段）。
func ev(m map[string]any) traceEvent {
	b, _ := json.Marshal(m)
	e, ok := parseTraceEvent(string(b))
	if !ok {
		panic("test event not parsed: " + string(b))
	}
	return e
}

// ts 生成 RFC3339 时间戳（基准 2026-09-10T12:00:00Z + n 毫秒）。
func ts(n int) string {
	return timeAt(n).Format("2006-01-02T15:04:05.000000Z07:00")
}

// TestBuildTracesBasic：主 run + 1 轮 llm + 1 次工具 → 折叠成 3 个 span，
// 时间偏移与时长按事件差计算，摘要汇总正确。
func TestBuildTracesBasic(t *testing.T) {
	evs := []traceEvent{
		ev(map[string]any{"event_type": "agent_start", "run_id": "r1", "model": "m1",
			"content": "给链路 tab 加瀑布图", "timestamp": ts(0)}),
		ev(map[string]any{"event_type": "llm_start", "run_id": "r1", "request_id": "req1",
			"model": "m1", "timestamp": ts(10)}),
		ev(map[string]any{"event_type": "llm_end", "run_id": "r1", "request_id": "req1",
			"model": "m1", "timestamp": ts(510), "finish_reason": "tool_call",
			"usage": map[string]any{"Input": 100, "Output": 20, "CacheRead": 80, "Reasoning": 5}}),
		ev(map[string]any{"event_type": "tool_run_start", "run_id": "r1", "id": "call_1",
			"name": "read_file", "timestamp": ts(520)}),
		ev(map[string]any{"event_type": "tool_run_end", "run_id": "r1", "id": "call_1",
			"name": "read_file", "timestamp": ts(800), "is_error": false}),
		ev(map[string]any{"event_type": "agent_end", "run_id": "r1", "model": "m1",
			"timestamp": ts(1000), "duration_ms": 1000, "finish_reason": "stop"}),
	}
	ds := buildTraces(evs, "")
	if len(ds) != 1 {
		t.Fatalf("trace 条数 = %d, want 1", len(ds))
	}
	d := ds[0]
	if len(d.Spans) != 3 {
		t.Fatalf("span 条数 = %d, want 3（run+llm+tool）: %+v", len(d.Spans), d.Spans)
	}
	// 顺序应为时间序：run, llm, tool
	kinds := []string{d.Spans[0].Kind, d.Spans[1].Kind, d.Spans[2].Kind}
	if kinds[0] != traceKindRun || kinds[1] != traceKindLLM || kinds[2] != traceKindTool {
		t.Fatalf("span 顺序 = %v, want [run llm tool]", kinds)
	}
	run, llm, tool := d.Spans[0], d.Spans[1], d.Spans[2]
	if run.StartAt != 0 || run.DurMs != 1000 {
		t.Fatalf("run start/dur = %d/%d, want 0/1000", run.StartAt, run.DurMs)
	}
	if llm.StartAt != 10 || llm.DurMs != 500 {
		t.Fatalf("llm start/dur = %d/%d, want 10/500", llm.StartAt, llm.DurMs)
	}
	if tool.StartAt != 520 || tool.DurMs != 280 {
		t.Fatalf("tool start/dur = %d/%d, want 520/280", tool.StartAt, tool.DurMs)
	}
	// 摘要
	sum := d.Summary
	if sum.ID != "r1" || sum.Turns != 1 || sum.Tools != 1 || sum.Spans != 3 || sum.Subs != 0 {
		t.Fatalf("summary = %+v", sum)
	}
	if sum.InTok != 100 || sum.OutTok != 20 {
		t.Fatalf("tokens = %d/%d, want 100/20", sum.InTok, sum.OutTok)
	}
	if sum.WallMs != 1000 {
		t.Fatalf("wall = %d, want 1000", sum.WallMs)
	}
	if llm.Label == "" || run.Label != "给链路 tab 加瀑布图" {
		t.Fatalf("labels: run=%q llm=%q", run.Label, llm.Label)
	}
}

// TestBuildTracesSubAgentTree：子 agent 的 span 归入父 trace 的子树（一条 trace），
// 且摘要统计到子 run 数；父子关系由 run span 的 parent_id 承载。
func TestBuildTracesSubAgentTree(t *testing.T) {
	evs := []traceEvent{
		ev(map[string]any{"event_type": "agent_start", "run_id": "root", "content": "主任务", "timestamp": ts(0)}),
		ev(map[string]any{"event_type": "agent_start", "run_id": "sub", "parent_run_id": "root",
			"name": "span-model-review", "depth": 1, "timestamp": ts(100)}),
		ev(map[string]any{"event_type": "llm_start", "run_id": "sub", "request_id": "s1", "timestamp": ts(110)}),
		ev(map[string]any{"event_type": "llm_end", "run_id": "sub", "request_id": "s1", "timestamp": ts(610)}),
		ev(map[string]any{"event_type": "agent_end", "run_id": "sub", "timestamp": ts(700), "duration_ms": 600}),
		ev(map[string]any{"event_type": "agent_end", "run_id": "root", "timestamp": ts(900), "duration_ms": 900}),
	}
	ds := buildTraces(evs, "")
	if len(ds) != 1 {
		t.Fatalf("子 agent 应归入父 trace（1 条），got %d", len(ds))
	}
	d := ds[0]
	if d.Summary.Subs != 1 {
		t.Fatalf("Subs = %d, want 1", d.Summary.Subs)
	}
	var sub *traceSpan
	for i := range d.Spans {
		if d.Spans[i].Kind == traceKindRun && d.Spans[i].RunID == "sub" {
			sub = &d.Spans[i]
		}
	}
	if sub == nil {
		t.Fatal("子 run span 缺失")
	}
	if sub.ParentID != "root" || sub.Depth != 1 {
		t.Fatalf("子 run 父子关系 = parent %q depth %d, want root/1", sub.ParentID, sub.Depth)
	}
	if sub.StartAt != 100 || sub.DurMs != 600 {
		t.Fatalf("子 run start/dur = %d/%d, want 100/600", sub.StartAt, sub.DurMs)
	}
}

// TestBuildTracesMultipleRoots：多个顶层 run → 多条 trace，按开始时间倒序（最近在上）。
func TestBuildTracesMultipleRoots(t *testing.T) {
	evs := []traceEvent{
		ev(map[string]any{"event_type": "agent_start", "run_id": "old", "content": "第一条", "timestamp": ts(0)}),
		ev(map[string]any{"event_type": "agent_end", "run_id": "old", "timestamp": ts(100), "duration_ms": 100}),
		ev(map[string]any{"event_type": "agent_start", "run_id": "new", "content": "第二条", "timestamp": ts(5000)}),
		ev(map[string]any{"event_type": "agent_end", "run_id": "new", "timestamp": ts(5100), "duration_ms": 100}),
	}
	ds := buildTraces(evs, "")
	if len(ds) != 2 {
		t.Fatalf("trace 条数 = %d, want 2", len(ds))
	}
	if ds[0].Summary.ID != "new" {
		t.Fatalf("首条应为最近的 trace（new），got %q", ds[0].Summary.ID)
	}
	if ds[1].Summary.ID != "old" {
		t.Fatalf("次条应为 old，got %q", ds[1].Summary.ID)
	}
}

// TestBuildTracesUnclosedStartIsInterrupted：只有 start 没有 end（崩溃/中断残留）
// → status=interrupted，且不虚构时长。
func TestBuildTracesUnclosedStartIsInterrupted(t *testing.T) {
	evs := []traceEvent{
		ev(map[string]any{"event_type": "agent_start", "run_id": "r1", "content": "x", "timestamp": ts(0)}),
		ev(map[string]any{"event_type": "tool_run_start", "run_id": "r1", "id": "c1", "name": "bash", "timestamp": ts(10)}),
	}
	ds := buildTraces(evs, "")
	if len(ds) != 1 {
		t.Fatalf("trace 条数 = %d, want 1", len(ds))
	}
	d := ds[0]
	if d.Summary.Status != "interrupted" {
		t.Fatalf("trace 状态 = %q, want interrupted", d.Summary.Status)
	}
	for _, s := range d.Spans {
		if s.Status != "interrupted" {
			t.Fatalf("span %s 状态 = %q, want interrupted", s.Kind, s.Status)
		}
		if s.DurMs != 0 {
			t.Fatalf("未闭合 span 不应有虚构时长: %d", s.DurMs)
		}
	}
}

// TestBuildTracesErrorRun：finish_reason=error → run/trace 状态 error；
// Errors 只数「失败处数」（不把 run 自身的 error 重复计入子失败）。
func TestBuildTracesErrorRun(t *testing.T) {
	evs := []traceEvent{
		ev(map[string]any{"event_type": "agent_start", "run_id": "r1", "content": "x", "timestamp": ts(0)}),
		ev(map[string]any{"event_type": "tool_run_start", "run_id": "r1", "id": "c1", "name": "bash", "timestamp": ts(10)}),
		ev(map[string]any{"event_type": "tool_run_end", "run_id": "r1", "id": "c1", "name": "bash",
			"timestamp": ts(90), "is_error": true}),
		ev(map[string]any{"event_type": "agent_end", "run_id": "r1", "timestamp": ts(100),
			"duration_ms": 100, "finish_reason": "error", "error": "boom"}),
	}
	ds := buildTraces(evs, "")
	d := ds[0]
	if d.Summary.Status != "error" {
		t.Fatalf("trace 状态 = %q, want error", d.Summary.Status)
	}
	if d.Summary.Errors != 1 {
		t.Fatalf("Errors = %d, want 1（一次失败不应被 run+tool 重复计数）", d.Summary.Errors)
	}
	for _, s := range d.Spans {
		if s.Kind == traceKindTool && s.Status != "error" {
			t.Fatalf("工具 span 状态 = %q, want error", s.Status)
		}
	}
}

// TestBuildTracesRunErrorWithoutChildError：run 失败但无失败子 span（如 LLM 层错误
// 导致本轮终止）→ Errors 仍为 1，不让「失败」显示成 0。
func TestBuildTracesRunErrorWithoutChildError(t *testing.T) {
	evs := []traceEvent{
		ev(map[string]any{"event_type": "agent_start", "run_id": "r1", "content": "x", "timestamp": ts(0)}),
		ev(map[string]any{"event_type": "llm_start", "run_id": "r1", "request_id": "q1", "timestamp": ts(10)}),
		ev(map[string]any{"event_type": "llm_end", "run_id": "r1", "request_id": "q1", "timestamp": ts(50)}),
		ev(map[string]any{"event_type": "agent_end", "run_id": "r1", "timestamp": ts(60),
			"duration_ms": 60, "finish_reason": "error", "error": "provider down"}),
	}
	ds := buildTraces(evs, "")
	if ds[0].Summary.Status != "error" || ds[0].Summary.Errors != 1 {
		t.Fatalf("run 失败应记 1 处错误: status=%q errors=%d",
			ds[0].Summary.Status, ds[0].Summary.Errors)
	}
}

// TestBuildTracesCompressAndWait：compress_start/end 配对成 span（P0 新补的 timestamp），
// ask_user_question 成 wait span；压缩摘要携带 before/after。
func TestBuildTracesCompressAndWait(t *testing.T) {
	evs := []traceEvent{
		ev(map[string]any{"event_type": "agent_start", "run_id": "r1", "content": "x", "timestamp": ts(0)}),
		ev(map[string]any{"event_type": "compress_start", "run_id": "r1",
			"before": 42, "reason": "automatic", "timestamp": ts(100)}),
		ev(map[string]any{"event_type": "compress_end", "run_id": "r1", "before": 42, "after": 12,
			"ctx_tokens": 3400, "model": "m1", "timestamp": ts(1200)}),
		ev(map[string]any{"event_type": "ask_user_question", "run_id": "r1", "id": "q-1",
			"questions": []any{map[string]any{"question": "继续？", "options": []any{"是", "否"}}},
			"timestamp": ts(1500)}),
		ev(map[string]any{"event_type": "agent_end", "run_id": "r1", "timestamp": ts(2000), "duration_ms": 2000}),
	}
	ds := buildTraces(evs, "")
	d := ds[0]
	var comp, wait *traceSpan
	for i := range d.Spans {
		switch d.Spans[i].Kind {
		case traceKindCompress:
			comp = &d.Spans[i]
		case traceKindWait:
			wait = &d.Spans[i]
		}
	}
	if comp == nil || wait == nil {
		t.Fatalf("compress/wait span 缺失: %+v", d.Spans)
	}
	if comp.StartAt != 100 || comp.DurMs != 1100 || comp.Before != 42 || comp.After != 12 {
		t.Fatalf("compress span = %+v", comp)
	}
	if comp.CtxTokens != 3400 || comp.Model != "m1" {
		t.Fatalf("compress 指标 = %+v", comp)
	}
	if wait.WaitType != "ask_user" || wait.Questions != 1 {
		t.Fatalf("wait span = %+v", wait)
	}
	// wait 是**单事件** span（ask_user_question 没有 end），不得标成 interrupted ——
	// 只有 run/llm/tool/compress 这些「成对事件」才可能未闭合。
	// 该语义与前端 trace.ts 必须一致（交叉比对 desktop/bridge/trace_crossimpl_test.go 固定）。
	if wait.Status != "done" {
		t.Fatalf("wait span 状态应为 done（单事件无 end 属正常），got %q", wait.Status)
	}
}

// TestBuildTracesNoTimestamp：全无时间戳的极旧事件不得崩溃，也不得虚构时间
// （StartAt/DurMs 保持 0）—— 兼容性要求（设计文档 §8）。
func TestBuildTracesNoTimestamp(t *testing.T) {
	evs := []traceEvent{
		ev(map[string]any{"event_type": "agent_start", "run_id": "r1", "content": "x"}),
		ev(map[string]any{"event_type": "tool_run_start", "run_id": "r1", "id": "c1", "name": "bash"}),
		ev(map[string]any{"event_type": "tool_run_end", "run_id": "r1", "id": "c1", "name": "bash"}),
	}
	ds := buildTraces(evs, "")
	if len(ds) != 1 {
		t.Fatalf("trace 条数 = %d, want 1", len(ds))
	}
	for _, s := range ds[0].Spans {
		if s.StartAt != 0 || s.DurMs != 0 {
			t.Fatalf("无时间戳时不应虚构时间: %+v", s)
		}
	}
}

// TestParseTraceEventFiltersChunks：流式增量/未知类型不进投影（白名单过滤）。
func TestParseTraceEventFiltersChunks(t *testing.T) {
	skip := []string{
		`{"event_type":"content_chunk","run_id":"r1","content":"x"}`,
		`{"event_type":"reasoning_chunk","run_id":"r1","content":"x"}`,
		`{"event_type":"session_closed","session_id":"s1"}`,
		`not json at all`,
		`{"no_event_type":true}`,
	}
	for _, line := range skip {
		if _, ok := parseTraceEvent(line); ok {
			t.Fatalf("不应被投影: %s", line)
		}
	}
	keep := `{"event_type":"tool_run_start","run_id":"r1","id":"c1","name":"bash"}`
	if _, ok := parseTraceEvent(keep); !ok {
		t.Fatalf("应被投影: %s", keep)
	}
}

// TestTraceSpanOmitsPayload：投影后的 span 不含正文字段（result/content/arguments）
// —— 正文永远单份，trace 只存元数据（设计文档 §7.2）。
func TestTraceSpanOmitsPayload(t *testing.T) {
	evs := []traceEvent{
		ev(map[string]any{"event_type": "agent_start", "run_id": "r1",
			"content": "very long goal " + string(make([]byte, 0)), "timestamp": ts(0)}),
		ev(map[string]any{"event_type": "tool_run_start", "run_id": "r1", "id": "c1",
			"name": "bash", "arguments": `{"command":"rm -rf /"}`, "timestamp": ts(10)}),
		ev(map[string]any{"event_type": "tool_run_end", "run_id": "r1", "id": "c1", "name": "bash",
			"result": "SECRET_PAYLOAD_MUST_NOT_APPEAR", "timestamp": ts(20)}),
	}
	ds := buildTraces(evs, "")
	b, err := json.Marshal(ds)
	if err != nil {
		t.Fatal(err)
	}
	for _, bad := range []string{"SECRET_PAYLOAD_MUST_NOT_APPEAR", "rm -rf"} {
		if strings.Contains(string(b), bad) {
			t.Fatalf("投影结果不应包含正文（%q）: %s", bad, b)
		}
	}
}

// TestBuildTracesLiveRootIsRunning：正在运行的 root run（无 agent_end）应判 running，
// 而不是 interrupted —— 否则「运行中」的链路被显示成「未完成」，与实际状态矛盾。
// 对照：TestBuildTracesUnclosedStartIsInterrupted（liveRoot 为空 = 中断残留）。
func TestBuildTracesLiveRootIsRunning(t *testing.T) {
	evs := []traceEvent{
		ev(map[string]any{"event_type": "agent_start", "run_id": "r1", "content": "正在跑", "timestamp": ts(0)}),
		ev(map[string]any{"event_type": "tool_run_start", "run_id": "r1", "id": "c1", "name": "bash", "timestamp": ts(10)}),
	}
	// liveRoot = r1（会话正在跑）
	ds := buildTraces(evs, "r1")
	if len(ds) != 1 {
		t.Fatalf("trace 条数 = %d, want 1", len(ds))
	}
	d := ds[0]
	if d.Summary.Status != "running" {
		t.Fatalf("活跃 trace 状态 = %q, want running", d.Summary.Status)
	}
	for _, s := range d.Spans {
		if s.Status != "running" {
			t.Fatalf("活跃 span(%s) 状态 = %q, want running", s.Kind, s.Status)
		}
	}
	// 对照：同一份事件流、liveRoot 为空（会话已不在跑）→ interrupted
	ds2 := buildTraces(evs, "")
	if ds2[0].Summary.Status != "interrupted" {
		t.Fatalf("非活跃 trace 状态 = %q, want interrupted", ds2[0].Summary.Status)
	}
}
