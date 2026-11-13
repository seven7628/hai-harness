package tools

import (
	"context"
	"encoding/json"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type mockTool struct {
	name        string
	canParallel bool
	block       bool
	delay       time.Duration
	toolTimeout time.Duration // ToolTimeoutProvider 返回值（0=引擎默认，>0=覆盖，<0=豁免）

	calls       atomic.Int64
	inflight    atomic.Int64
	maxInflight atomic.Int64
}

func (m *mockTool) Name() string                                      { return m.name }
func (m *mockTool) Description() string                               { return "mock tool " + m.name }
func (m *mockTool) Parameters() any                                   { return map[string]any{"type": "object"} }
func (m *mockTool) CanParallel() bool                                 { return m.canParallel }
func (m *mockTool) ToolTimeout() time.Duration                        { return m.toolTimeout }
func (m *mockTool) ValidParams(context.Context, string, string) error { return nil }
func (m *mockTool) BeforeCall(context.Context, core.ToolCall) BeforeToolCallResponse {
	return BeforeToolCallResponse{Block: m.block, Reasoning: "user refused"}
}
func (m *mockTool) Call(ctx context.Context, name, arguments string) (string, error) {
	m.calls.Add(1)
	cur := m.inflight.Add(1)
	for {
		prev := m.maxInflight.Load()
		if cur <= prev || m.maxInflight.CompareAndSwap(prev, cur) {
			break
		}
	}
	defer m.inflight.Add(-1)

	if m.delay > 0 {
		select {
		case <-time.After(m.delay):
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	return "result:" + name + ":" + arguments, nil
}
func (m *mockTool) AfterCall(context.Context, core.ToolCall) {}

// collectHandler 收集事件（写加锁；读取在全部执行结束后进行）。
func collectHandler() (events.EventHandler, *[]events.Event) {
	var mu sync.Mutex
	var list []events.Event
	return func(_ context.Context, e events.Event) {
		mu.Lock()
		defer mu.Unlock()
		list = append(list, e)
	}, &list
}

func TestRegisterAndGetTool(t *testing.T) {
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &mockTool{name: "a"})

	tool, err := e.GetTool(context.Background(), "a")
	if err != nil || tool.Name() != "a" {
		t.Fatalf("want tool a, got %v, err %v", tool, err)
	}
	if _, err := e.GetTool(context.Background(), "missing"); err == nil {
		t.Fatal("want error for unregistered tool")
	}
}

func TestToolParams(t *testing.T) {
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &mockTool{name: "a"})
	e.RegisterTool(context.Background(), &mockTool{name: "b"})

	schemas, err := e.ToolParams(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(schemas) != 2 {
		t.Fatalf("want 2 schemas, got %d", len(schemas))
	}
	names := map[string]bool{}
	for _, s := range schemas {
		names[s.Name] = true
	}
	if !names["a"] || !names["b"] {
		t.Fatalf("want schemas for a and b, got %+v", names)
	}
}

func TestSequence(t *testing.T) {
	e := NewToolEngine()
	m := &mockTool{name: "echo"}
	e.RegisterTool(context.Background(), m)

	handler, list := collectHandler()
	results, err := e.Sequence(context.Background(), []core.ToolCall{
		{Id: "c1", Index: 0, Name: "echo", Arguments: `{"a":1}`, RequestId: "req-x"},
		{Id: "c2", Index: 1, Name: "echo", Arguments: `{"a":2}`, RequestId: "req-x"},
	}, handler)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	if results[0].Id != "c1" || results[0].Result != `result:echo:{"a":1}` {
		t.Fatalf("unexpected result[0]: %+v", results[0])
	}
	if results[1].Id != "c2" {
		t.Fatalf("unexpected result[1]: %+v", results[1])
	}
	if m.calls.Load() != 2 {
		t.Fatalf("want 2 calls, got %d", m.calls.Load())
	}

	// 事件顺序：ToolStart, ToolResponse, ToolStart, ToolResponse
	if len(*list) != 4 {
		t.Fatalf("want 4 events, got %d", len(*list))
	}
	if (*list)[0].Type() != events.ToolRunStartType || (*list)[1].Type() != events.ToolRunEndType {
		t.Fatalf("unexpected event order: %v, %v", (*list)[0].Type(), (*list)[1].Type())
	}
	// 工具事件携带 RequestId（自包含）与时间戳（耗时可算）
	if ts, ok := (*list)[0].(*events.ToolStart); !ok || ts.RequestId != "req-x" || ts.Timestamp.IsZero() {
		t.Fatalf("tool_start identifiers: %+v", (*list)[0])
	}
	// ToolStart 携带实际执行参数（UI 展示「传了什么」）
	if ts, ok := (*list)[0].(*events.ToolStart); !ok || ts.Arguments != `{"a":1}` {
		t.Fatalf("tool_start 应携带 Arguments: %+v", (*list)[0])
	}
	if tr, ok := (*list)[1].(*events.ToolResponse); !ok || tr.RequestId != "req-x" || tr.Timestamp.IsZero() {
		t.Fatalf("tool_response identifiers: %+v", (*list)[1])
	}
}

func TestParallel(t *testing.T) {
	e := NewToolEngine(WithMaxParallel(4))
	m := &mockTool{name: "echo", canParallel: true, delay: 50 * time.Millisecond}
	e.RegisterTool(context.Background(), m)

	handler, _ := collectHandler()
	start := time.Now()
	results, err := e.Parallel(context.Background(), []core.ToolCall{
		{Id: "c1", Name: "echo"},
		{Id: "c2", Name: "echo"},
		{Id: "c3", Name: "echo"},
	}, handler)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed := time.Since(start); elapsed > 90*time.Millisecond {
		t.Fatalf("not parallel: took %v", elapsed)
	}
	if len(results) != 3 || results[0].Id != "c1" || results[2].Id != "c3" {
		t.Fatalf("results not aligned to input: %+v", results)
	}
	if peak := m.maxInflight.Load(); peak < 2 {
		t.Fatalf("want concurrency >= 2, got %d", peak)
	}
}

// 工具事件携带所属 run（事件自包含）：WithToolContext 注入 → ToolStart/ToolResponse 带 run_id；
// 无 run 上下文（独立引擎调用）→ 空（omitempty 保持 JSON 干净）。
func TestToolEventsCarryRunID(t *testing.T) {
	e := NewToolEngine()
	m := &mockTool{name: "echo"}
	e.RegisterTool(context.Background(), m)

	handler, list := collectHandler()
	ctx := events.WithToolContext(context.Background(), "run-x", 0, handler, nil, nil)
	if _, err := e.Sequence(ctx, []core.ToolCall{{Id: "c1", Name: "echo"}}, handler); err != nil {
		t.Fatal(err)
	}
	if len(*list) != 2 {
		t.Fatalf("want 2 events, got %d", len(*list))
	}
	if ts, ok := (*list)[0].(*events.ToolStart); !ok || ts.RunId != "run-x" {
		t.Fatalf("tool_start run_id = %+v", (*list)[0])
	}
	if tr, ok := (*list)[1].(*events.ToolResponse); !ok || tr.RunId != "run-x" {
		t.Fatalf("tool_response run_id = %+v", (*list)[1])
	}

	handler2, list2 := collectHandler()
	if _, err := e.Sequence(context.Background(), []core.ToolCall{{Id: "c2", Name: "echo"}}, handler2); err != nil {
		t.Fatal(err)
	}
	if ts, ok := (*list2)[0].(*events.ToolStart); !ok || ts.RunId != "" {
		t.Fatalf("tool_start 无 run 上下文时 run_id 应为空: %+v", (*list2)[0])
	}
}

func TestBlockIntercept(t *testing.T) {
	e := NewToolEngine()
	m := &mockTool{name: "secret", block: true}
	e.RegisterTool(context.Background(), m)

	handler, _ := collectHandler()
	results, err := e.Sequence(context.Background(), []core.ToolCall{{Id: "c1", Name: "secret"}}, handler)
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].IsError {
		t.Fatalf("want IsError for blocked call, got %+v", results[0])
	}
	if m.calls.Load() != 0 {
		t.Fatalf("Call must not run when blocked, calls=%d", m.calls.Load())
	}
}

func TestUnregisteredTool(t *testing.T) {
	e := NewToolEngine()

	handler, _ := collectHandler()
	results, err := e.Sequence(context.Background(), []core.ToolCall{{Id: "c1", Name: "nope"}}, handler)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || !results[0].IsError {
		t.Fatalf("want single IsError result, got %+v", results)
	}
}

func TestTruncate(t *testing.T) {
	e := NewToolEngine(WithMaxResponseSize(10))
	// 头尾都保留（head=5/tail=5），中段省略量精确量化 —— 精确断言见
	// TestTruncateHeadTailExactOmitted。
	s := "HEAD" + strings.Repeat("m", 20) + "TAIL" // 28 字节
	got := e.truncate(s, "call-1")
	if !strings.HasPrefix(got, "HEADm") || !strings.HasSuffix(got, "mTAIL") {
		t.Fatalf("want head+tail preserved: %q", got)
	}
	if !strings.Contains(got, "[... 18 bytes omitted from the middle (64.29% of 28 bytes total); showing the first 5 and the last 5 bytes") {
		t.Fatalf("want exact omitted byte count: %q", got)
	}
	if s := e.truncate("123", "call-1"); s != "123" {
		t.Fatalf("short string must not truncate: %q", s)
	}
}

// TestToolTimeoutDefault 引擎兜底超时：未声明超时的工具被统一拦截（防挂死 agent）。
func TestToolTimeoutDefault(t *testing.T) {
	e := NewToolEngine(WithToolTimeout(100 * time.Millisecond))
	m := &mockTool{name: "slow", delay: 5 * time.Second} // toolTimeout 0 → 引擎默认
	e.RegisterTool(context.Background(), m)

	start := time.Now()
	results, err := e.Sequence(context.Background(), []core.ToolCall{{Id: "c1", Name: "slow"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].IsError || !strings.Contains(results[0].Result, "deadline exceeded") {
		t.Fatalf("want timeout error result, got %+v", results[0])
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("engine timeout not enforced: took %v", elapsed)
	}
}

// TestToolTimeoutOverride 工具声明自身超时（>0 覆盖引擎默认）。
func TestToolTimeoutOverride(t *testing.T) {
	e := NewToolEngine(WithToolTimeout(time.Second)) // 引擎 1s（若生效则测试慢）
	m := &mockTool{name: "declare", delay: 5 * time.Second, toolTimeout: 50 * time.Millisecond}
	e.RegisterTool(context.Background(), m)

	start := time.Now()
	results, err := e.Sequence(context.Background(), []core.ToolCall{{Id: "c1", Name: "declare"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].IsError {
		t.Fatalf("want timeout error result, got %+v", results[0])
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("tool timeout override not applied: took %v", elapsed)
	}
}

// TestToolPanicRecover 工具 panic 转错误：不炸 agent 主流程，错误进结果文本（模型可见）。
func TestToolPanicRecover(t *testing.T) {
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &mockTool{name: "boom"})
	// 注册一个会 panic 的变体
	e.tools["panic-tool"] = &panicTool{}

	results, err := e.Sequence(context.Background(), []core.ToolCall{{Id: "c1", Name: "panic-tool"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !results[0].IsError || !strings.Contains(results[0].Result, "panicked") {
		t.Fatalf("panic must become IsError result, got %+v", results[0])
	}
}

type panicTool struct{ mockTool }

func (p *panicTool) Call(context.Context, string, string) (string, error) {
	panic("boom! tool crashed")
}

// TestToolTimeoutExempt 工具显式豁免（<0）：长时子 agent 类工具不被引擎默认超时误杀。
func TestToolTimeoutExempt(t *testing.T) {
	e := NewToolEngine(WithToolTimeout(50 * time.Millisecond))
	m := &mockTool{name: "long", delay: 300 * time.Millisecond, toolTimeout: -1}
	e.RegisterTool(context.Background(), m)

	results, err := e.Sequence(context.Background(), []core.ToolCall{{Id: "c1", Name: "long"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].IsError {
		t.Fatalf("exempt tool must not be killed by engine timeout: %+v", results[0])
	}
}

// TestToolParamsDeterministic 缓存命中的前提：schema 列表按名字典序、多次调用
// 顺序逐字节一致（map 遍历顺序随机 —— 顺序抖动会让同一会话多轮请求的
// 工具 schema 前缀每次失配，provider 缓存永不命中）。
func TestToolParamsDeterministic(t *testing.T) {
	eng := NewToolEngine()
	for _, name := range []string{"zeta", "alpha", "echo", "mid"} {
		eng.RegisterTool(context.Background(), &mockTool{name: name})
	}

	var prev []string
	for i := 0; i < 5; i++ {
		schemas, err := eng.ToolParams(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		names := make([]string, 0, len(schemas))
		for _, s := range schemas {
			names = append(names, s.Name)
		}
		if i == 0 {
			prev = names
			continue
		}
		for j := range names {
			if names[j] != prev[j] {
				t.Fatalf("tool order must be deterministic across calls: %v vs %v", prev, names)
			}
		}
	}
	want := []string{"alpha", "echo", "mid", "zeta"}
	for i := range want {
		if prev[i] != want[i] {
			t.Fatalf("tools must be sorted lexicographically: got %v, want %v", prev, want)
		}
	}
}

// —— 长时运行异步化：promote 摘离（docs/ASYNC_TOOLS_AND_PROMOTE.md Phase 1）——

// gateTool 可释放阻塞工具：Call 阻塞直到 gate 关闭或 ctx 取消（模拟长时工具/同步子 agent）。
type gateTool struct {
	name        string
	canParallel bool
	gate        chan struct{}
	releaseOnce sync.Once
	calls       atomic.Int64
	usage       core.Usage // ToolUsageProvider 返回值（摘离完成时随结果回传，决策 #7）

	disableAuto bool          // DisableAutoPromote 返回值（自动摘离豁免）
	asyncAfter  time.Duration // AsyncPromoteAfter 返回值（>0 覆盖批级阈值；<0 禁用）
}

func (g *gateTool) Name() string                                      { return g.name }
func (g *gateTool) Description() string                               { return "gate tool " + g.name }
func (g *gateTool) Parameters() any                                   { return map[string]any{"type": "object"} }
func (g *gateTool) CanParallel() bool                                 { return g.canParallel }
func (g *gateTool) DisableAutoPromote() bool                          { return g.disableAuto }
func (g *gateTool) AsyncPromoteAfter() time.Duration                  { return g.asyncAfter }
func (g *gateTool) Usage() core.Usage                                 { return g.usage }
func (g *gateTool) ValidParams(context.Context, string, string) error { return nil }
func (g *gateTool) BeforeCall(context.Context, core.ToolCall) BeforeToolCallResponse {
	return BeforeToolCallResponse{}
}
func (g *gateTool) AfterCall(context.Context, core.ToolCall) {}
func (g *gateTool) Release()                                 { g.releaseOnce.Do(func() { close(g.gate) }) }
func (g *gateTool) Call(ctx context.Context, name, arguments string) (string, error) {
	g.calls.Add(1)
	select {
	case <-g.gate:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return "released:" + name, nil
}

// waitFor 轮询条件直到满足或超时（测试辅助）。
func waitFor(t *testing.T, cond func() bool, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// taskIDOf 按调用 Id 查批内工具任务（测试辅助；batch 与 toolTask 同包可见）。
func taskIDOf(t *testing.T, b *BatchController, callID string) string {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, tt := range b.tasks {
		if tt.call.Id == callID {
			return tt.taskID
		}
	}
	t.Fatalf("no task for call %q", callID)
	return ""
}

// placeholder 解析占位结果 {task_id,status,name}。
func placeholder(t *testing.T, r core.ToolResult) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal([]byte(r.Result), &m); err != nil {
		t.Fatalf("placeholder must be JSON, got %q: %v", r.Result, err)
	}
	return m
}

// TestRunBatchNormalAligned 普通工具批：结果与调用对齐、顺序一致（回归 Sequence/Parallel 语义）。
func TestRunBatchNormalAligned(t *testing.T) {
	e := NewToolEngine()
	m := &mockTool{name: "echo"} // 非并行：runBatch 保持串行语义
	e.RegisterTool(context.Background(), m)

	handler, list := collectHandler()
	calls := []core.ToolCall{
		{Id: "c1", Name: "echo", Arguments: `{"a":1}`, RequestId: "req-x"},
		{Id: "c2", Name: "echo", Arguments: `{"a":2}`, RequestId: "req-x"},
	}
	batch := NewBatchController(len(calls), nil)
	results := e.runBatch(context.Background(), calls, handler, nil, batch, 0)
	if len(results) != 2 {
		t.Fatalf("want 2 results, got %d", len(results))
	}
	if results[0].Id != "c1" || results[0].Result != `result:echo:{"a":1}` {
		t.Fatalf("unexpected results[0]: %+v", results[0])
	}
	if results[1].Id != "c2" {
		t.Fatalf("unexpected results[1]: %+v", results[1])
	}
	if m.calls.Load() != 2 {
		t.Fatalf("want 2 calls, got %d", m.calls.Load())
	}
	// 事件顺序：ToolStart, ToolResponse, ToolStart, ToolResponse
	if len(*list) != 4 {
		t.Fatalf("want 4 events, got %d", len(*list))
	}
	if (*list)[0].Type() != events.ToolRunStartType || (*list)[1].Type() != events.ToolRunEndType {
		t.Fatalf("unexpected event order: %v, %v", (*list)[0].Type(), (*list)[1].Type())
	}
}

// TestRunBatchPromote 运行中工具被摘离：返回占位 {status:running}、慢工具不被取消、
// release 后完成且 onDone 收到真实结果。重复 promote 幂等（once，无双重占位）。
func TestRunBatchPromote(t *testing.T) {
	e := NewToolEngine()
	g := &gateTool{name: "gate", gate: make(chan struct{}), usage: core.Usage{Input: 100, Output: 20}}
	e.RegisterTool(context.Background(), g)

	onDone := make(chan core.ToolResult, 1)
	batch := NewBatchController(1, func(taskID string, r core.ToolResult, interrupted bool) { onDone <- r })

	var results []core.ToolResult
	done := make(chan struct{})
	go func() {
		defer close(done)
		results = e.runBatch(context.Background(), []core.ToolCall{{Id: "c1", Name: "gate"}}, nil, nil, batch, 0)
	}()

	waitFor(t, func() bool { return g.calls.Load() >= 1 }, 2*time.Second)
	taskID := taskIDOf(t, batch, "c1")
	if !batch.Promote(taskID) {
		t.Fatal("Promote must accept running task")
	}
	if !batch.Promote(taskID) { // 幂等：第二次 no-op（once），不 panic、不重复占位
		t.Fatal("repeat promote must not fail")
	}

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runBatch must return after promote")
	}
	if len(results) != 1 || results[0].Id != "c1" {
		t.Fatalf("want 1 aligned result, got %+v", results)
	}
	ph := placeholder(t, results[0])
	if ph["task_id"] != taskID || ph["status"] != "running" || ph["name"] != "gate" {
		t.Fatalf("placeholder: %+v", ph)
	}

	// 摘离任务不被取消（g 仍在跑）：release 后完成 → onDone 收到真实结果 + usage（决策 #7）
	g.Release()
	select {
	case r := <-onDone:
		if r.Result != "released:gate" || r.IsError {
			t.Fatalf("onDone result: %+v", r)
		}
		if r.Usage.Input != 100 || r.Usage.Output != 20 {
			t.Fatalf("onDone must carry usage, got %+v", r.Usage)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onDone must receive real result after release")
	}
}

// timedGateTool 带工具级时限的可释放阻塞工具（验证 promote 解除时限）。
type timedGateTool struct {
	gateTool
	timeout time.Duration
}

func (g *timedGateTool) ToolTimeout() time.Duration { return g.timeout }

// TestRunBatchPromoteLiftsTimeout promote 摘离后解除时限：工具超过其声明的 300ms
// 时限仍不被杀；release 后完成并回传真实结果（后台任务 = 不限时）。
func TestRunBatchPromoteLiftsTimeout(t *testing.T) {
	e := NewToolEngine()
	g := &timedGateTool{gateTool: gateTool{name: "tgate", gate: make(chan struct{})}, timeout: 300 * time.Millisecond}
	e.RegisterTool(context.Background(), g)

	onDone := make(chan core.ToolResult, 1)
	batch := NewBatchController(1, func(_ string, r core.ToolResult, interrupted bool) { onDone <- r })

	var results []core.ToolResult
	done := make(chan struct{})
	go func() {
		defer close(done)
		results = e.runBatch(context.Background(), []core.ToolCall{{Id: "c1", Name: "tgate"}}, nil, nil, batch, 0)
	}()

	waitFor(t, func() bool { return g.calls.Load() >= 1 }, 2*time.Second)
	taskID := taskIDOf(t, batch, "c1")
	if !batch.Promote(taskID) {
		t.Fatal("Promote must accept running task")
	}

	// 越过其 300ms 时限：Lift 已解除时限 → 工具不被 deadline 杀掉（仍在阻塞等待 gate）
	time.Sleep(700 * time.Millisecond)

	// 释放 → onDone 收到真实结果（而非超时错误）
	g.Release()
	select {
	case r := <-onDone:
		if r.Result != "released:tgate" || r.IsError {
			t.Fatalf("promote 后应拿到真实结果（时限已解除），got %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onDone must receive real result after release (promote lifted timeout)")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runBatch must return after promote")
	}
	ph := placeholder(t, results[0])
	if ph["task_id"] != taskID || ph["status"] != "running" {
		t.Fatalf("placeholder: %+v", ph)
	}
}

// TestRunBatchTimeoutStillKillsWhenNotPromoted 对照组：未 promote 时工具声明时限
// 到期照常取消（防失控工具挂死 agent 的兜底不能被自动摘离误伤）。
func TestRunBatchTimeoutStillKillsWhenNotPromoted(t *testing.T) {
	e := NewToolEngine()
	g := &timedGateTool{gateTool: gateTool{name: "tgate", gate: make(chan struct{})}, timeout: 150 * time.Millisecond}
	e.RegisterTool(context.Background(), g)

	start := time.Now()
	results := e.runBatch(context.Background(), []core.ToolCall{{Id: "c1", Name: "tgate"}}, nil, nil, nil, 0)
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	if !results[0].IsError || !strings.Contains(results[0].Result, "deadline") {
		t.Fatalf("want deadline error, got %+v", results[0])
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("runBatch took too long: %v", time.Since(start))
	}
}

// TestRunBatchPromoteNilSink 无 TaskSink（onDone nil）：promote 仍释放（占位回写），
// 结果仅 ToolResponse 事件、不推回（异常 #10）。
func TestRunBatchPromoteNilSink(t *testing.T) {
	e := NewToolEngine()
	g := &gateTool{name: "gate", gate: make(chan struct{})}
	e.RegisterTool(context.Background(), g)

	responses := make(chan *events.ToolResponse, 4)
	handler := func(_ context.Context, ev events.Event) {
		if tr, ok := ev.(*events.ToolResponse); ok {
			responses <- tr
		}
	}
	batch := NewBatchController(1, nil)
	var results []core.ToolResult
	done := make(chan struct{})
	go func() {
		defer close(done)
		results = e.runBatch(context.Background(), []core.ToolCall{{Id: "c1", Name: "gate"}}, handler, nil, batch, 0)
	}()
	waitFor(t, func() bool { return g.calls.Load() >= 1 }, 2*time.Second)
	if !batch.Promote(taskIDOf(t, batch, "c1")) {
		t.Fatal("promote failed")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runBatch must return after promote")
	}
	if ph := placeholder(t, results[0]); ph["status"] != "running" {
		t.Fatalf("placeholder: %v", ph)
	}
	// release 后：ToolResponse 事件（真实结果）发出，但无 onDone（无 sink）
	g.Release()
	select {
	case tr := <-responses:
		if tr.Result != "released:gate" {
			t.Fatalf("ToolResponse result: %+v", tr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ToolResponse must carry real result (no sink path)")
	}
}

// TestRunBatchAbortCancelsUnpromoted run abort：未摘离任务取消（取消结果文本），
// 已摘离任务存活（Detach 后不随 run 取消）。
func TestRunBatchAbortCancelsUnpromoted(t *testing.T) {
	e := NewToolEngine(WithMaxParallel(2))
	g := &gateTool{name: "gate", gate: make(chan struct{}), canParallel: true}
	e.RegisterTool(context.Background(), g)

	onDone := make(chan core.ToolResult, 1)
	batch := NewBatchController(2, func(taskID string, r core.ToolResult, interrupted bool) { onDone <- r })

	ctx, cancel := context.WithCancel(context.Background())
	var results []core.ToolResult
	done := make(chan struct{})
	go func() {
		defer close(done)
		results = e.runBatch(ctx,
			[]core.ToolCall{{Id: "c1", Name: "gate"}, {Id: "c2", Name: "gate"}},
			nil, nil, batch, 0)
	}()

	waitFor(t, func() bool { return g.calls.Load() >= 2 }, 2*time.Second)
	if !batch.Promote(taskIDOf(t, batch, "c1")) {
		t.Fatal("promote c1 failed")
	}
	cancel() // run abort

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runBatch must return after abort")
	}
	// c1 = 占位（promoted）；c2 = 取消结果（未摘离被 Cancel）
	if ph := placeholder(t, results[0]); ph["status"] != "running" {
		t.Fatalf("c1 must be placeholder, got %+v", results[0])
	}
	if !results[1].IsError {
		t.Fatalf("c2 must be canceled (IsError), got %+v", results[1])
	}

	// 摘离任务存活过 abort：release 后完成 → onDone
	g.Release()
	select {
	case r := <-onDone:
		if r.Result != "released:gate" {
			t.Fatalf("onDone result: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("promoted task must survive run abort")
	}
}

// TestRunBatchAutoPromote 自动摘离阈值：超时任务自动摘离（占位 + task_promoted 事件）；
// DisableAutoPromote 工具未超时照常（正常收集真实结果）。
func TestRunBatchAutoPromote(t *testing.T) {
	e := NewToolEngine()
	g := &gateTool{name: "gate", gate: make(chan struct{}), canParallel: true}
	d := &gateTool{name: "gate-disabled", gate: make(chan struct{}), canParallel: true, disableAuto: true}
	e.RegisterTool(context.Background(), g)
	e.RegisterTool(context.Background(), d)

	promoted := make(chan string, 2)
	handler := func(_ context.Context, ev events.Event) {
		if tp, ok := ev.(*events.TaskPromoted); ok {
			promoted <- tp.TaskId
		}
	}
	onDone := make(chan core.ToolResult, 1)
	batch := NewBatchController(2, func(taskID string, r core.ToolResult, interrupted bool) { onDone <- r })

	var results []core.ToolResult
	done := make(chan struct{})
	go func() {
		defer close(done)
		results = e.runBatch(context.Background(),
			[]core.ToolCall{{Id: "c1", Name: "gate"}, {Id: "c2", Name: "gate-disabled"}},
			handler, nil, batch, 50*time.Millisecond)
	}()

	waitFor(t, func() bool { return g.calls.Load() >= 1 && d.calls.Load() >= 1 }, 2*time.Second)

	// c1 超时 → 自动摘离（占位 + task_promoted 事件）
	select {
	case id := <-promoted:
		if id != taskIDOf(t, batch, "c1") {
			t.Fatalf("auto-promote wrong task: %s", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("auto-promote must fire for c1")
	}

	// c2（禁用自动摘离）未超时照常：release 后正常收集真实结果
	d.Release()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runBatch must return after disabled tool completes")
	}
	if ph := placeholder(t, results[0]); ph["status"] != "running" {
		t.Fatalf("c1 must be auto-promoted placeholder, got %+v", results[0])
	}
	if results[1].Result != "released:gate-disabled" || results[1].IsError {
		t.Fatalf("c2 must collect real result, got %+v", results[1])
	}

	// c1 后台完成 → onDone 真实结果
	g.Release()
	select {
	case r := <-onDone:
		if r.Result != "released:gate" {
			t.Fatalf("c1 onDone result: %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("c1 must complete in background and push via onDone")
	}
}

// TestToolStartCarriesTaskId ToolStart 事件携带 task_id（前端据此关联后台任务）。
func TestToolStartCarriesTaskId(t *testing.T) {
	e := NewToolEngine()
	m := &mockTool{name: "echo"}
	e.RegisterTool(context.Background(), m)

	handler, list := collectHandler()
	batch := NewBatchController(1, nil)
	e.runBatch(context.Background(), []core.ToolCall{{Id: "c1", Name: "echo"}}, handler, nil, batch, 0)
	var ts *events.ToolStart
	for _, ev := range *list {
		if s, ok := ev.(*events.ToolStart); ok {
			ts = s
			break
		}
	}
	if ts == nil {
		t.Fatal("no ToolStart event")
	}
	if ts.TaskId == "" || !strings.HasPrefix(ts.TaskId, "tooltask-") {
		t.Fatalf("ToolStart must carry task_id, got %q", ts.TaskId)
	}
}

// TestEngineTaskIDRandomUnique tooltask id 为 8 位随机（tooltask-<8>）：
// 符合字母表形状、两个 Engine 实例（loop 重建/多会话）间不重复——
// 旧递增序列随 Engine 重建归零，导致多代任务复用 tooltask-1/2、前端按 id
// 匹配历史块串卡（2026-08-30 实证）。
func TestEngineTaskIDRandomUnique(t *testing.T) {
	re := regexp.MustCompile(`^tooltask-[abcdefghjkmnpqrstuvwxyz23456789]{8}$`)
	seen := map[string]bool{}
	for _, e := range []*Engine{NewToolEngine(), NewToolEngine()} { // 两代 Engine（模拟重建）
		for i := 0; i < 100; i++ {
			id := e.newTaskID()
			if !re.MatchString(id) {
				t.Fatalf("id %q 不符合随机形状", id)
			}
			if seen[id] {
				t.Fatalf("跨 Engine 重复 id: %s（递增序列才会有的碰撞）", id)
			}
			seen[id] = true
		}
	}
}

// TestRunBatchPromoteMultiple 同批多个长工具同时摘离：各自独立占位 + 独立完成回调，
// 互不阻塞（文档 §9：多任务同批 promote，各自独立释放，每 task 独立 goroutine + once）。
func TestRunBatchPromoteMultiple(t *testing.T) {
	e := NewToolEngine(WithMaxParallel(4))
	g1 := &gateTool{name: "gate-a", gate: make(chan struct{}), canParallel: true}
	g2 := &gateTool{name: "gate-b", gate: make(chan struct{}), canParallel: true}
	e.RegisterTool(context.Background(), g1)
	e.RegisterTool(context.Background(), g2)

	var doneMu sync.Mutex
	var doneIDs []string
	onDone := func(taskID string, r core.ToolResult, interrupted bool) {
		doneMu.Lock()
		doneIDs = append(doneIDs, taskID)
		doneMu.Unlock()
	}
	batch := NewBatchController(2, onDone)
	var results []core.ToolResult
	done := make(chan struct{})
	go func() {
		defer close(done)
		results = e.runBatch(context.Background(),
			[]core.ToolCall{{Id: "c1", Name: "gate-a"}, {Id: "c2", Name: "gate-b"}},
			nil, nil, batch, 0)
	}()

	waitFor(t, func() bool { return g1.calls.Load() >= 1 && g2.calls.Load() >= 1 }, 2*time.Second)
	idA := taskIDOf(t, batch, "c1")
	idB := taskIDOf(t, batch, "c2")
	if !batch.Promote(idA) {
		t.Fatal("promote gate-a failed")
	}
	if !batch.Promote(idB) {
		t.Fatal("promote gate-b failed")
	}

	// 两个都占位，主 loop 立即返回（不阻塞）
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("runBatch must return after both promoted")
	}
	if ph := placeholder(t, results[0]); ph["task_id"] != idA || ph["status"] != "running" {
		t.Fatalf("results[0]: %+v", results[0])
	}
	if ph := placeholder(t, results[1]); ph["task_id"] != idB || ph["status"] != "running" {
		t.Fatalf("results[1]: %+v", results[1])
	}

	// 各自独立完成：release 一个只触发它自己的 onDone，另一个不受影响
	g1.Release()
	waitFor(t, func() bool {
		doneMu.Lock()
		defer doneMu.Unlock()
		return len(doneIDs) == 1
	}, 2*time.Second)
	g2.Release()
	waitFor(t, func() bool {
		doneMu.Lock()
		defer doneMu.Unlock()
		return len(doneIDs) == 2
	}, 2*time.Second)
}

// TestParseToolErrorCompactDefaults 精简 TOOL_ERROR（code/next_action/detail/fix）
// 缺省字段取现网统一默认值；重复失败调用拦截依赖 RetryWithSameArguments=false 缺省。
func TestParseToolErrorCompactDefaults(t *testing.T) {
	r := "TOOL_ERROR\ncode: old_string_not_found\nnext_action: read_file\ndetail: old_string was not found in a.ts\nfix: re-read and retry"
	e := parseToolError(r)
	if e == nil {
		t.Fatal("TOOL_ERROR block must parse")
	}
	if e.Code != "old_string_not_found" || e.NextAction != "read_file" || e.Recovery != "re-read and retry" {
		t.Fatalf("parsed fields = %+v", e)
	}
	if e.Changed != "false" || !e.Retryable || e.RetryWithSameArguments {
		t.Fatalf("defaults = %+v (want changed=false retryable=true retrySame=false)", e)
	}
	// 非 TOOL_ERROR 文本不解析
	if parseToolError("plain error") != nil {
		t.Fatal("plain error must not parse as ToolError")
	}
	// 旧格式键仍兼容
	old := parseToolError("TOOL_ERROR\ntool: edit_file\ncode: x\nchanged: false\nretryable: true\nretry_with_same_arguments: false\nnext_action: read_file\nrecovery: legacy")
	if old == nil || old.Code != "x" || old.Recovery != "legacy" || old.RetryWithSameArguments {
		t.Fatalf("legacy format = %+v", old)
	}
	// 显式 retry_with_same_arguments: true 不被缺省覆盖
	same := parseToolError("TOOL_ERROR\ncode: y\nretry_with_same_arguments: true")
	if same == nil || !same.RetryWithSameArguments {
		t.Fatalf("explicit retrySame = %+v", same)
	}
}
