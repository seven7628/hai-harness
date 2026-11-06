package agents

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/seven7628/hai-harness/agents/prompt"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/todo"
	"github.com/seven7628/hai-harness/tools"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockProvider 按预设脚本逐轮发射 LLM 事件。
type mockProvider struct {
	rounds []func(handler func(e provider.StreamEvent) error) error
	calls  int
}

func (m *mockProvider) Stream(_ context.Context, _ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	// 轮次用尽后复用最后一轮：可模拟「模型持续要求调用工具」的死循环场景
	round := m.rounds[m.calls]
	if m.calls < len(m.rounds)-1 {
		m.calls++
	}
	return round(handler)
}

type echoTool struct{}

func (echoTool) Name() string                                      { return "echo" }
func (echoTool) Description() string                               { return "echo back arguments" }
func (echoTool) Parameters() any                                   { return map[string]any{"type": "object"} }
func (echoTool) CanParallel() bool                                 { return true }
func (echoTool) ValidParams(context.Context, string, string) error { return nil }
func (echoTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}
func (echoTool) Call(_ context.Context, name, arguments string) (string, error) {
	return "echo:" + name + ":" + arguments, nil
}
func (echoTool) AfterCall(context.Context, core.ToolCall) {}

func collectEvents() (events.EventHandler, *[]events.Event) {
	var mu sync.Mutex
	var list []events.Event
	return func(_ context.Context, e events.Event) {
		mu.Lock()
		defer mu.Unlock()
		list = append(list, e)
	}, &list
}

// captureProvider 记录每次 Stream 的 RequestConfig（断言宿主配置透传到 provider 用）。
type captureProvider struct {
	mu  sync.Mutex
	cfg *provider.RequestConfig
}

func (c *captureProvider) Stream(_ context.Context, req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	c.mu.Lock()
	c.cfg = req.Config
	c.mu.Unlock()
	return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "done"})
}

type captureRoundsProvider struct {
	mu       sync.Mutex
	requests []*provider.StreamRequest
	rounds   []func(func(provider.StreamEvent) error) error
	calls    int
}

func (p *captureRoundsProvider) Stream(_ context.Context, req *provider.StreamRequest, handler func(provider.StreamEvent) error) error {
	p.mu.Lock()
	copyReq := *req
	copyReq.Messages = append([]core.Message(nil), req.Messages...)
	copyReq.Tools = append([]core.ToolSchema(nil), req.Tools...)
	p.requests = append(p.requests, &copyReq)
	idx := p.calls
	p.calls++
	p.mu.Unlock()
	if idx >= len(p.rounds) {
		return errors.New("captureRoundsProvider: scripted rounds exhausted")
	}
	return p.rounds[idx](handler)
}

type reminderTestTool struct {
	name string
	err  error
}

func (t reminderTestTool) Name() string                                      { return t.name }
func (t reminderTestTool) Description() string                               { return t.name }
func (t reminderTestTool) Parameters() any                                   { return map[string]any{"type": "object"} }
func (t reminderTestTool) CanParallel() bool                                 { return false }
func (t reminderTestTool) ValidParams(context.Context, string, string) error { return nil }
func (t reminderTestTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}
func (t reminderTestTool) Call(context.Context, string, string) (string, error) {
	if t.err != nil {
		return "", t.err
	}
	return "ok", nil
}
func (t reminderTestTool) AfterCall(context.Context, core.ToolCall) {}

func reminderToolRound(name, id string) func(func(provider.StreamEvent) error) error {
	return func(handler func(provider.StreamEvent) error) error {
		return handler(provider.LLMEndEvent{
			FinishReason: core.FinishReasonToolCall,
			ToolCalls:    []core.ToolCall{{Id: id, Name: name, Arguments: `{}`}},
		})
	}
}

func reminderStopRound(text string) func(func(provider.StreamEvent) error) error {
	return func(handler func(provider.StreamEvent) error) error {
		return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: text})
	}
}

func requestHasGoalReminder(req *provider.StreamRequest) bool {
	for _, m := range req.Messages {
		for _, c := range m.Content {
			if m.Role == core.User && strings.Contains(c.Content, "<system-remind>") {
				return true
			}
		}
	}
	return false
}

// WithProviderName 注入的 provider 名必须透传到每次 StreamRequest.Provider
// （价表 FillUsageCost/模型元数据 Lookup 精确按 provider+model 取 —— 此前缺失导致
// 注册表只能按模型名全局兜底，第三方 provider 成本常查不到）。
func TestProviderNameFlowsToStreamRequest(t *testing.T) {
	p := &captureRoundsProvider{rounds: []func(func(provider.StreamEvent) error) error{reminderStopRound("done")}}
	eng := tools.NewToolEngine()
	loop := NewAgentLoop(
		WithProvider(p),
		WithProviderName("command-code"),
		WithModel("deepseek/deepseek-v4-flash"),
		WithToolEngine(eng),
	)
	handler, _ := collectEvents()
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler)
	if ac.Err != nil {
		t.Fatalf("run failed: %v", ac.Err)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.requests) != 1 {
		t.Fatalf("want 1 request, got %d", len(p.requests))
	}
	if got := p.requests[0].Provider; got != "command-code" {
		t.Fatalf("StreamRequest.Provider = %q, want command-code", got)
	}
}

// 压缩器（LLMCompressor）同样把 WithCompressProviderName 注入 StreamRequest.Provider。
func TestCompressorProviderNameFlowsToStreamRequest(t *testing.T) {
	cp := &compressProvider{content: "# 会话摘要\n\n## 任务目标\n- 目标", model: "m1",
		usage: core.Usage{Input: 100, Output: 20, TotalTokens: 120}}
	c := NewLLMCompressor(cp, "m1", WithCompressProviderName("command-code"))
	input := []core.Message{
		core.NewSystemMessage("你是助手"),
		core.NewUserMessage(core.Content{Type: "text", Content: "你好"}),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "你好"}}, "", nil),
		core.NewUserMessage(core.Content{Type: "text", Content: "帮我查资料"}),
	}
	if _, err := c.Compact(context.Background(), input); err != nil {
		t.Fatal(err)
	}
	if cp.req == nil || cp.req.Provider != "command-code" {
		t.Fatalf("compress StreamRequest.Provider = %+v, want command-code", cp.req)
	}
}

// TestGoalAlignmentReminderResetsAfterSuccessfulWrite 验证提醒按连续决策轮次触发：
// 达到阈值后只在当前无修改区间注入一次；成功 write_file 将计数清零并重新开始计算。
func TestGoalAlignmentReminderResetsAfterSuccessfulWrite(t *testing.T) {
	p := &captureRoundsProvider{rounds: []func(func(provider.StreamEvent) error) error{
		reminderToolRound("echo", "e1"),
		reminderToolRound("echo", "e2"),
		reminderToolRound("write_file", "w1"),
		reminderToolRound("echo", "e3"),
		reminderToolRound("echo", "e4"),
		reminderStopRound("done"),
	}}
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), reminderTestTool{name: "echo"})
	eng.RegisterTool(context.Background(), reminderTestTool{name: "write_file"})
	var eventsList []events.Event
	handler := func(_ context.Context, e events.Event) { eventsList = append(eventsList, e) }
	loop := NewAgentLoop(
		WithProvider(p),
		WithToolEngine(eng),
		WithGoalAlignmentReminderRounds(2),
		WithMaxIterations(10),
	)
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "task"})}, handler)
	if ac.Err != nil {
		t.Fatalf("run failed: %v", ac.Err)
	}

	p.mu.Lock()
	requests := append([]*provider.StreamRequest(nil), p.requests...)
	p.mu.Unlock()
	if len(requests) != 6 {
		t.Fatalf("want 6 provider requests, got %d", len(requests))
	}
	for i, req := range requests {
		want := i == 2 || i == 5
		if got := requestHasGoalReminder(req); got != want {
			t.Fatalf("request %d reminder: want %v, got %v", i+1, want, got)
		}
	}
	var reminders []events.GoalAlignmentReminder
	for _, e := range eventsList {
		if r, ok := e.(*events.GoalAlignmentReminder); ok {
			reminders = append(reminders, *r)
		}
	}
	if len(reminders) != 2 || reminders[0].Round != 2 || reminders[1].Round != 2 {
		t.Fatalf("want reminders at round 2 after each reset, got %+v", reminders)
	}
	for _, m := range ac.Messages {
		if strings.Contains(messageText(m), prompt.GoalAlignmentReminder) {
			t.Fatal("transient reminder must not enter agent context")
		}
	}
}

// TestGoalAlignmentReminderFailedWriteDoesNotReset 验证失败的 write/edit 不算文件进展：
// 提醒不会因失败工具调用而重新计数或被重复注入。
func TestGoalAlignmentReminderFailedWriteDoesNotReset(t *testing.T) {
	p := &captureRoundsProvider{rounds: []func(func(provider.StreamEvent) error) error{
		reminderToolRound("echo", "e1"),
		reminderToolRound("echo", "e2"),
		reminderToolRound("write_file", "w1"),
		reminderStopRound("done"),
	}}
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), reminderTestTool{name: "echo"})
	eng.RegisterTool(context.Background(), reminderTestTool{name: "write_file", err: errors.New("write failed")})
	var eventsList []events.Event
	loop := NewAgentLoop(
		WithProvider(p),
		WithToolEngine(eng),
		WithGoalAlignmentReminderRounds(2),
		WithMaxIterations(10),
	)
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "task"})}, func(_ context.Context, e events.Event) { eventsList = append(eventsList, e) })
	if ac.Err != nil {
		t.Fatalf("run failed: %v", ac.Err)
	}
	p.mu.Lock()
	requests := append([]*provider.StreamRequest(nil), p.requests...)
	p.mu.Unlock()
	if len(requests) != 4 || !requestHasGoalReminder(requests[2]) || requestHasGoalReminder(requests[3]) {
		t.Fatalf("failed write must not reset/reinject reminder: requests=%d r3=%v r4=%v", len(requests), requestHasGoalReminder(requests[2]), requestHasGoalReminder(requests[3]))
	}
}

// → RequestConfig.RetainReasoning 指针 true（覆盖按模型名自动判断）；不设置 → nil（走 auto）。
func TestRetainReasoningOption(t *testing.T) {
	run := func(opts ...Option) *provider.RequestConfig {
		p := &captureProvider{}
		all := append([]Option{WithProvider(p), WithModel("deepseek-v4-flash"), WithToolEngine(tools.NewToolEngine())}, opts...)
		loop := NewAgentLoop(all...)
		handler, _ := collectEvents()
		loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler)
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.cfg
	}
	if cfg := run(); cfg.RetainReasoning != nil {
		t.Fatalf("unset option must leave RetainReasoning nil (auto), got %v", *cfg.RetainReasoning)
	}
	if cfg := run(WithRetainReasoning(true)); cfg.RetainReasoning == nil || !*cfg.RetainReasoning {
		t.Fatalf("WithRetainReasoning(true) must flow into RequestConfig, got %v", cfg.RetainReasoning)
	}
	if cfg := run(WithRetainReasoning(false)); cfg.RetainReasoning == nil || *cfg.RetainReasoning {
		t.Fatalf("WithRetainReasoning(false) must flow into RequestConfig, got %v", cfg.RetainReasoning)
	}
}

// TestRunMultiRound 验证多轮循环：第一轮工具调用（并行执行、回写），第二轮正常结束，用量累计。
func TestRunMultiRound(t *testing.T) {
	p := &mockProvider{
		rounds: []func(handler func(e provider.StreamEvent) error) error{
			func(handler func(e provider.StreamEvent) error) error {
				if err := handler(provider.LLMStartEvent{}); err != nil {
					return err
				}
				return handler(provider.LLMEndEvent{
					RequestId:    "req-1",
					Model:        "mock-model",
					FinishReason: core.FinishReasonToolCall,
					Reasoning:    "思考中",
					ToolCalls: []core.ToolCall{
						{Id: "c1", Index: 0, Name: "echo", Arguments: `{"a":1}`},
						{Id: "c2", Index: 1, Name: "echo", Arguments: `{"a":2}`},
					},
					Usage: core.Usage{Input: 10, Output: 5},
				})
			},
			func(handler func(e provider.StreamEvent) error) error {
				if err := handler(provider.LLMStartEvent{}); err != nil {
					return err
				}
				return handler(provider.LLMEndEvent{
					FinishReason: core.FinishReasonStop,
					Content:      "done",
					Usage:        core.Usage{Input: 15, Output: 3},
				})
			},
		},
	}

	engine := tools.NewToolEngine(tools.WithMaxParallel(4))
	engine.RegisterTool(context.Background(), echoTool{})

	loop := NewAgentLoop(
		WithProvider(p),
		WithToolEngine(engine),
		WithModel("mock-model"),
		WithMaxIterations(5),
	)

	handler, list := collectEvents()
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler)

	// 运行状态（工具调用数经 AgentEnd.ToolCalls 暴露，见下方结算断言）
	if ac.Iteration != 2 {
		t.Fatalf("want 2 iterations, got %d", ac.Iteration)
	}
	if ac.Usage.Input != 25 || ac.Usage.Output != 8 {
		t.Fatalf("usage not accumulated: %+v", ac.Usage)
	}

	// 上下文回写：system（默认 base 注入）+ user + assistant + 2×tool = 5
	if len(ac.Messages) != 5 {
		t.Fatalf("want 5 messages, got %d: %+v", len(ac.Messages), ac.Messages)
	}
	if ac.Messages[2].Role != core.Assistant {
		t.Fatalf("want assistant message at [2], got %+v", ac.Messages[2])
	}
	// 推理内容与工具调用必须随 assistant 消息回写（DeepSeek 多轮工具调用要求）
	if ac.Messages[2].Reasoning != "思考中" {
		t.Fatalf("reasoning not written back: %q", ac.Messages[2].Reasoning)
	}
	// 同模型 thinking 回传判断：assistant 消息必须记录来源模型（agent loop 写入点）
	if ac.Messages[2].Model != "mock-model" {
		t.Fatalf("assistant message Model not written: %q, want mock-model", ac.Messages[2].Model)
	}
	if len(ac.Messages[2].ToolCalls) != 2 || ac.Messages[2].ToolCalls[0].Id != "c1" {
		t.Fatalf("tool calls not written back: %+v", ac.Messages[2].ToolCalls)
	}
	if ac.Messages[3].Role != core.Tool || ac.Messages[3].ToolCallId != "c1" {
		t.Fatalf("want tool result for c1 at [3], got %+v", ac.Messages[3])
	}
	if ac.Messages[3].Content[0].Content != `echo:echo:{"a":1}` {
		t.Fatalf("unexpected tool content: %q", ac.Messages[3].Content[0].Content)
	}

	// 事件覆盖（并行工具事件的先后顺序不定，按类型计数断言）
	counts := map[events.EventType]int{}
	for _, e := range *list {
		counts[e.Type()]++
	}
	want := map[events.EventType]int{
		events.AgentStartType:   1,
		events.AgentEndType:     1,
		events.LLMStartType:     2,
		events.LLMEndType:       2,
		events.ToolRunStartType: 2,
		events.ToolRunEndType:   2,
	}
	for k, v := range want {
		if counts[k] != v {
			t.Fatalf("event %s: want %d, got %d (all: %v)", k, v, counts[k], counts)
		}
	}
	if len(*list) == 0 || (*list)[0].Type() != events.AgentStartType {
		t.Fatalf("first event must be agent_start, got %v", (*list)[0].Type())
	}

	// AgentStart 携带首条输入摘要 + 运行级标识
	start, ok := (*list)[0].(*events.AgentStart)
	if !ok || start.Content != "hi" || start.RunId == "" || start.Model != "mock-model" {
		t.Fatalf("agent_start: %+v", (*list)[0])
	}

	// LLMEnd 透传单次调用级标识
	for _, e := range *list {
		if end, ok := e.(*events.LLMEnd); ok && end.Content == "你好" {
			if end.Model != "mock-model" || end.RequestId != "req-1" {
				t.Fatalf("llm_end identifiers not passed through: %+v", end)
			}
			if end.Timestamp.IsZero() {
				t.Fatal("llm_end timestamp missing")
			}
		}
	}

	// AgentEnd 聚合结算：最终回复 + 跨轮工具调用 + 累计用量
	last := (*list)[len(*list)-1]
	end, ok := last.(*events.AgentEnd)
	if !ok {
		t.Fatalf("last event must be agent_end, got %v", last.Type())
	}
	if end.Content != "done" || end.Reasoning != "" {
		t.Fatalf("agent_end content/reasoning: %+v", end)
	}
	if len(end.ToolCalls) != 2 || end.ToolCalls[0].Id != "c1" {
		t.Fatalf("agent_end tool calls not aggregated: %+v", end.ToolCalls)
	}
	if end.Usage == nil || end.Usage.Input != 25 || end.Usage.Output != 8 {
		t.Fatalf("agent_end usage not aggregated: %+v", end.Usage)
	}
	if end.FinishReason != core.FinishReasonStop || end.Error != "" {
		t.Fatalf("agent_end finish reason: %+v", end)
	}
	// RunId 与 AgentStart 一致（事件流关联锚点）
	if end.RunId != start.RunId || end.Model != "mock-model" || end.Timestamp.IsZero() {
		t.Fatalf("agent_end run identifiers: %+v", end)
	}
}

// TestTodoDefaultIndependentStore 未注入 Todo 的运行默认独立待办：
// todo 工具可用（不再报 no session todo store），store 随运行独立。
func TestTodoDefaultIndependentStore(t *testing.T) {
	engine := tools.NewToolEngine()
	for _, tool := range todo.Tools() {
		engine.RegisterTool(context.Background(), tool)
	}
	p := &mockProvider{rounds: []func(handler func(e provider.StreamEvent) error) error{
		func(handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonToolCall,
				ToolCalls:    []core.ToolCall{{Id: "t1", Index: 0, Name: "todo_add", Arguments: `{"title":"独立任务"}`}},
				Usage:        core.Usage{Input: 10, Output: 5},
			})
		},
		func(handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
		},
	}}
	loop := NewAgentLoop(WithProvider(p), WithToolEngine(engine), WithModel("mock-model"))
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, nil)
	if ac.Err != nil {
		t.Fatalf("todo_add should work with default independent store: %v", ac.Err)
	}
	for _, m := range ac.Messages {
		if m.Role == core.Tool && m.ToolCallId == "t1" {
			if strings.Contains(m.Content[0].Content, "no session todo store") {
				t.Fatalf("todo_add must not fail without injected store: %q", m.Content[0].Content)
			}
		}
	}
}

// TestAgentEndError 验证失败运行时 AgentEnd 携带错误信息。
func TestAgentEndError(t *testing.T) {
	failRound := func(handler func(e provider.StreamEvent) error) error {
		return errors.New("boom")
	}
	p := &mockProvider{rounds: []func(handler func(e provider.StreamEvent) error) error{failRound}}
	loop := NewAgentLoop(WithProvider(p), WithMaxRetries(1))

	handler, list := collectEvents()
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler)

	if ac.Err == nil {
		t.Fatal("want run failure")
	}
	end := (*list)[len(*list)-1].(*events.AgentEnd)
	if end.FinishReason != core.FinishReasonError || end.Error == "" {
		t.Fatalf("agent_end must carry error: %+v", end)
	}
}

// TestCompressor 验证压缩策略可插拔注入：触发压缩、发事件、上下文被替换。
func TestCompressor(t *testing.T) {
	p := &mockProvider{
		rounds: []func(handler func(e provider.StreamEvent) error) error{
			func(handler func(e provider.StreamEvent) error) error {
				return handler(provider.LLMEndEvent{
					FinishReason: core.FinishReasonStop,
					Content:      "ok",
				})
			},
		},
	}

	comp := &countingCompressor{}
	loop := NewAgentLoop(
		WithProvider(p),
		WithCompressor(comp),
		WithContextWindow(125), // 预算 = 125 × 0.8 = 100
	)

	handler, list := collectEvents()
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler)

	if comp.compactCalls != 1 {
		t.Fatalf("want 1 compact call, got %d", comp.compactCalls)
	}
	// 压缩后上下文 = 压缩结果 + 压缩继续提醒（maybeCompact 注入 compact reminder）
	if len(ac.Messages) != 2 {
		t.Fatalf("context should be compacted to summary + compact reminder, got %d", len(ac.Messages))
	}

	counts := map[events.EventType]int{}
	for _, e := range *list {
		counts[e.Type()]++
	}
	if counts[events.CompressStartType] != 1 || counts[events.CompressEndType] != 1 {
		t.Fatalf("want compress_start/end events, got %v", counts)
	}
}

// collectLLMErrors 收集事件流中的 LLMError 事件。
func collectLLMErrors(list *[]events.Event) []events.LLMError {
	var errs []events.LLMError
	for _, e := range *list {
		if l, ok := e.(*events.LLMError); ok {
			errs = append(errs, *l)
		}
	}
	return errs
}

// TestLLMRetrySuccess 验证自动重试：前 2 次失败后第 3 次成功，事件流携带每次失败。
func TestLLMRetrySuccess(t *testing.T) {
	failCount := 0
	failRound := func(handler func(e provider.StreamEvent) error) error {
		failCount++
		return errors.New("boom")
	}
	successRound := func(handler func(e provider.StreamEvent) error) error {
		return handler(provider.LLMEndEvent{
			FinishReason: core.FinishReasonStop,
			Content:      "ok",
		})
	}

	p := &mockProvider{rounds: []func(handler func(e provider.StreamEvent) error) error{failRound, failRound, successRound}}
	loop := NewAgentLoop(WithProvider(p), WithMaxRetries(10))

	handler, list := collectEvents()
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler)

	if ac.Err != nil {
		t.Fatalf("run should succeed after retries: %v", ac.Err)
	}
	if failCount != 2 {
		t.Fatalf("want 2 failed attempts, got %d", failCount)
	}
	errs := collectLLMErrors(list)
	if len(errs) != 2 {
		t.Fatalf("want 2 llm_error events, got %d", len(errs))
	}
	if errs[0].Attempt != 1 || errs[1].Attempt != 2 {
		t.Fatalf("attempt numbers wrong: %+v", errs)
	}
	for _, e := range errs {
		if !e.WillRetry {
			t.Fatalf("attempts before final must mark will_retry: %+v", e)
		}
	}
}

// TestLLMRetryExhausted 验证重试耗尽：达到 MaxRetries 后停止，最终错误进入 AgentContext。
func TestLLMRetryExhausted(t *testing.T) {
	failCount := 0
	failRound := func(handler func(e provider.StreamEvent) error) error {
		failCount++
		return errors.New("boom")
	}

	p := &mockProvider{rounds: []func(handler func(e provider.StreamEvent) error) error{failRound}}
	loop := NewAgentLoop(WithProvider(p), WithMaxRetries(3))

	handler, list := collectEvents()
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler)

	if ac.Err == nil {
		t.Fatal("run should fail after retries exhausted")
	}
	if failCount != 3 {
		t.Fatalf("want 3 attempts, got %d", failCount)
	}
	errs := collectLLMErrors(list)
	if len(errs) != 3 {
		t.Fatalf("want 3 llm_error events, got %d", len(errs))
	}
	if errs[2].Attempt != 3 || errs[2].WillRetry {
		t.Fatalf("final attempt must not retry: %+v", errs[2])
	}
	if errs[0].WillRetry != true || errs[1].WillRetry != true {
		t.Fatalf("early attempts must retry: %+v", errs)
	}
	// LLMError 携带 RunId（事件自包含）
	if errs[0].RunId == "" || errs[0].RunId != ac.RunId {
		t.Fatalf("llm_error run id: %+v", errs[0])
	}
}

// TestLLMRetryCanceled 验证用户取消不重试（context.Canceled）。
func TestLLMRetryCanceled(t *testing.T) {
	cancelRound := func(handler func(e provider.StreamEvent) error) error {
		return context.Canceled
	}

	p := &mockProvider{rounds: []func(handler func(e provider.StreamEvent) error) error{cancelRound}}
	loop := NewAgentLoop(WithProvider(p), WithMaxRetries(10))

	handler, list := collectEvents()
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler)

	if !errors.Is(ac.Err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", ac.Err)
	}
	if errs := collectLLMErrors(list); len(errs) != 1 {
		t.Fatalf("canceled must not retry, want 1 llm_error, got %d", len(errs))
	}
}

// TestLLMRetryPermanent 验证永久错误（参数类 4xx）不重试。
func TestLLMRetryPermanent(t *testing.T) {
	permRound := func(handler func(e provider.StreamEvent) error) error {
		return provider.MarkPermanent(errors.New("bad request"))
	}

	p := &mockProvider{rounds: []func(handler func(e provider.StreamEvent) error) error{permRound}}
	loop := NewAgentLoop(WithProvider(p), WithMaxRetries(10))

	handler, list := collectEvents()
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler)

	if ac.Err == nil || !provider.IsPermanent(ac.Err) {
		t.Fatalf("want permanent error, got %v", ac.Err)
	}
	if errs := collectLLMErrors(list); len(errs) != 1 {
		t.Fatalf("permanent must not retry, want 1 llm_error, got %d", len(errs))
	}
}

// TestRunStreamInterjection 验证插话检查点：LLM End 自然结束时 poll 注入的
// 输入成批插入上下文（且本轮 assistant 回复先回写），继续下一轮。
func TestRunStreamInterjection(t *testing.T) {
	p := &mockProvider{
		rounds: []func(handler func(e provider.StreamEvent) error) error{
			func(handler func(e provider.StreamEvent) error) error {
				return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "first"})
			},
			func(handler func(e provider.StreamEvent) error) error {
				return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "second"})
			},
		},
	}
	loop := NewAgentLoop(WithProvider(p), WithMaxIterations(5))

	pollCount := 0
	poll := func(context.Context) []core.Message {
		pollCount++
		// 每轮 LLM Start 前也会 poll（收集暂存输入）；此处验证 stop 边界插话：
		// 第 2 次 poll = 第 1 轮的 stop 边界，注入插话 → 继续第 2 轮。
		if pollCount == 2 {
			return []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "interrupt!"})}
		}
		return nil
	}

	ac := loop.RunStream(
		context.Background(),
		[]core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})},
		nil, &RunOptions{Poll: poll},
	)

	if ac.Iteration != 2 {
		t.Fatalf("want 2 iterations, got %d", ac.Iteration)
	}
	if pollCount != 4 {
		// 每轮 2 次 poll：LLM Start 前 + stop 边界；2 轮 = 4 次
		t.Fatalf("want 4 poll calls, got %d", pollCount)
	}
	// [system(默认 base), user, assistant("first"), user(插话), assistant("second")]
	// —— 回复先回写再接插话；system 在链首
	if len(ac.Messages) != 5 || ac.Messages[0].Role != core.System {
		t.Fatalf("want 5 messages with leading system, got %d: %+v", len(ac.Messages), ac.Messages)
	}
	want := []struct {
		role    core.MessageRole
		content string
	}{
		{core.User, "hi"},
		{core.Assistant, "first"},
		{core.User, "interrupt!"},
		{core.Assistant, "second"},
	}
	for i, w := range want {
		m := ac.Messages[i+1]
		if m.Role != w.role || m.Content[0].Content != w.content {
			t.Fatalf("message[%d]: want %v/%q, got %v/%q", i+1, w.role, w.content, m.Role, m.Content[0].Content)
		}
	}
}

// TestRunStreamEmptyAssistantNotRecorded 验证空回复轮不回写上下文：模型 stop 且无可见文本
// （纯思考轮 / 全空轮）时，不 append 空 assistant 消息到 Messages —— 否则下一轮回传时上游
// 400 "content or tool_calls must be set"（opencode 网关 2026-08-17 实证）。空轮后若有
// 插话输入仍正常继续下一轮。
func TestRunStreamEmptyAssistantNotRecorded(t *testing.T) {
	p := &mockProvider{
		rounds: []func(handler func(e provider.StreamEvent) error) error{
			func(handler func(e provider.StreamEvent) error) error { // 轮1：空回复（stop + 无内容）
				return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: ""})
			},
			func(handler func(e provider.StreamEvent) error) error { // 轮2：正常回复
				return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "done"})
			},
		},
	}
	loop := NewAgentLoop(WithProvider(p), WithMaxIterations(5))

	pollCount := 0
	poll := func(context.Context) []core.Message {
		pollCount++
		if pollCount == 2 { // 第 1 轮 stop 边界注入插话 → 继续第 2 轮
			return []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "interrupt!"})}
		}
		return nil
	}

	ac := loop.RunStream(
		context.Background(),
		[]core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})},
		nil, &RunOptions{Poll: poll},
	)

	if ac.Iteration != 2 {
		t.Fatalf("want 2 iterations, got %d", ac.Iteration)
	}
	// [system, user("hi"), user(插话), assistant("done")] —— 空 assistant 轮不留痕迹
	if len(ac.Messages) != 4 {
		t.Fatalf("want 4 messages, got %d: %+v", len(ac.Messages), ac.Messages)
	}
	want := []struct {
		role    core.MessageRole
		content string
	}{
		{core.User, "hi"},
		{core.User, "interrupt!"},
		{core.Assistant, "done"},
	}
	for i, w := range want {
		m := ac.Messages[i+1]
		if m.Role != w.role || m.Content[0].Content != w.content {
			t.Fatalf("message[%d]: want %v/%q, got %v/%q", i+1, w.role, w.content, m.Role, m.Content[0].Content)
		}
	}
}

// TestRunStreamCollectAtToolRoundStart 验证方案点1的「每轮 LLM Start 收集暂存用户输入」：
// 用户运行中输入即使发生在工具调用轮之后，也会在下一轮（工具轮）LLM Start 前被 poll 收集、
// 拼到 Messages 末尾（而非等到 stop 边界 / run 结束）。
func TestRunStreamCollectAtToolRoundStart(t *testing.T) {
	p := &mockProvider{
		rounds: []func(handler func(e provider.StreamEvent) error) error{
			func(handler func(e provider.StreamEvent) error) error { // 轮1：工具调用
				if err := handler(provider.LLMStartEvent{}); err != nil {
					return err
				}
				return handler(provider.LLMEndEvent{
					FinishReason: core.FinishReasonToolCall,
					ToolCalls:    []core.ToolCall{{Id: "c1", Index: 0, Name: "echo", Arguments: `{"a":1}`}},
					Usage:        core.Usage{Input: 10, Output: 2},
				})
			},
			func(handler func(e provider.StreamEvent) error) error { // 轮2：stop
				if err := handler(provider.LLMStartEvent{}); err != nil {
					return err
				}
				return handler(provider.LLMEndEvent{
					FinishReason: core.FinishReasonStop,
					Content:      "done",
					Usage:        core.Usage{Input: 15, Output: 3},
				})
			},
		},
	}
	engine := tools.NewToolEngine()
	engine.RegisterTool(context.Background(), echoTool{})

	loop := NewAgentLoop(
		WithProvider(p),
		WithToolEngine(engine),
		WithModel("mock-model"),
		WithMaxIterations(5),
	)

	pollCount := 0
	poll := func(context.Context) []core.Message {
		pollCount++
		// 第 2 次 poll = 第 2 轮 LLM Start 前（上一轮是工具轮）：收集暂存用户输入
		if pollCount == 2 {
			return []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "运行中追问"})}
		}
		return nil
	}

	ac := loop.RunStream(
		context.Background(),
		[]core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})},
		nil, &RunOptions{Poll: poll},
	)

	if ac.Iteration != 2 {
		t.Fatalf("want 2 iterations, got %d", ac.Iteration)
	}
	if pollCount != 3 {
		// 轮1 start + 轮2 start + 轮2 stop 边界
		t.Fatalf("want 3 poll calls, got %d", pollCount)
	}
	// 期望：system, user(hi), assistant(工具调用), tool(c1), user(运行中追问), assistant(done)
	// —— 工具轮后，下一轮 LLM Start 前注入的 user 输入应紧跟工具回执、先于最终答复。
	foundUser := false
	for _, m := range ac.Messages {
		if m.Role == core.User && len(m.Content) > 0 && m.Content[0].Content == "运行中追问" {
			foundUser = true
		}
		if m.Role == core.Assistant && len(m.Content) > 0 && m.Content[0].Content == "done" && !foundUser {
			t.Fatalf("最终答复先于插话输入出现：%+v", ac.Messages)
		}
	}
	if !foundUser {
		t.Fatalf("运行中追问未被收集进上下文：%+v", ac.Messages)
	}
}

// TestChunkRequestId 验证 chunk 事件透传 RequestId（多轮运行时可区分轮次）。
func TestChunkRequestId(t *testing.T) {
	p := &mockProvider{
		rounds: []func(handler func(e provider.StreamEvent) error) error{
			func(handler func(e provider.StreamEvent) error) error {
				if err := handler(provider.LLMStartEvent{RequestId: "req-9", Model: "mock-model", Timestamp: time.Now()}); err != nil {
					return err
				}
				if err := handler(provider.LLMReasoningDeltaEvent{RequestId: "req-9", Delta: "想"}); err != nil {
					return err
				}
				if err := handler(provider.LLMContentDeltaEvent{RequestId: "req-9", Delta: "答"}); err != nil {
					return err
				}
				return handler(provider.LLMEndEvent{RequestId: "req-9", FinishReason: core.FinishReasonStop, Content: "答"})
			},
		},
	}
	loop := NewAgentLoop(WithProvider(p))
	handler, list := collectEvents()
	loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler)

	// LLMStart 透传调用级标识
	start := (*list)[1].(*events.LLMStart)
	if start.RequestId != "req-9" || start.Timestamp.IsZero() {
		t.Fatalf("llm_start identifiers: %+v", start)
	}
	// chunk 携带 RequestId
	for _, e := range *list {
		switch c := e.(type) {
		case *events.ContentChunk:
			if c.RequestId != "req-9" {
				t.Fatalf("content chunk request id: %+v", c)
			}
		case *events.ReasoningChunk:
			if c.RequestId != "req-9" {
				t.Fatalf("reasoning chunk request id: %+v", c)
			}
		}
	}
}

// TestRunStreamNoInterjection 验证无插话输入时 RunStream 等价 Run。
func TestRunStreamNoInterjection(t *testing.T) {
	p := &mockProvider{
		rounds: []func(handler func(e provider.StreamEvent) error) error{
			func(handler func(e provider.StreamEvent) error) error {
				return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
			},
		},
	}
	loop := NewAgentLoop(WithProvider(p))

	ac := loop.RunStream(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})},
		nil, &RunOptions{Poll: func(context.Context) []core.Message { return nil }})

	if ac.Iteration != 1 {
		t.Fatalf("want 1 iteration, got %d", ac.Iteration)
	}
	// 流式模式（poll 非 nil）下 assistant 回复始终回写，保持上下文连贯；
	// 链首为默认 base 注入的 system
	if len(ac.Messages) != 3 || ac.Messages[0].Role != core.System || ac.Messages[2].Role != core.Assistant {
		t.Fatalf("want [system, user, assistant], got %d: %+v", len(ac.Messages), ac.Messages)
	}
}

// TestSystemPromptInjection 验证系统提示词注入：首条非 system 时前置注入，不重复注入。
// 拼接链 = DefaultSystemPrompt（行为契约层）→ WithSystemPrompt（产品层）→ Agent.md → 技能。
func TestSystemPromptInjection(t *testing.T) {
	p := &mockProvider{
		rounds: []func(handler func(e provider.StreamEvent) error) error{
			func(handler func(e provider.StreamEvent) error) error {
				return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
			},
		},
	}
	loop := NewAgentLoop(WithProvider(p), WithSystemPrompt("你是测试助手"))
	composed := DefaultSystemPrompt + "\n\n" + "你是测试助手"

	ac := loop.Run(context.Background(),
		[]core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, nil)

	if len(ac.Messages) != 2 || ac.Messages[0].Role != core.System {
		t.Fatalf("system prompt not injected: %+v", ac.Messages)
	}
	if ac.Messages[0].Content[0].Content != composed {
		t.Fatalf("system content wrong: %+v", ac.Messages[0])
	}

	// 已含 system 首条但内容不同 → 原位替换（恢复会话时配置/Agent.md 变更自动生效）
	withSystem := []core.Message{
		core.NewSystemMessage("已有系统提示"),
		core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
	}
	ac2 := loop.Run(context.Background(), withSystem, nil)
	if len(ac2.Messages) != 2 || ac2.Messages[0].Content[0].Content != composed {
		t.Fatalf("different system prompt should be replaced in place: %+v", ac2.Messages)
	}
	// 替换行进 RunHistory（一次；下次内容已匹配不再追加）
	var hasSystem bool
	for _, m := range ac2.RunHistory {
		if m.Role == core.System {
			hasSystem = true
		}
	}
	if !hasSystem {
		t.Fatalf("replaced system should land in RunHistory: %+v", ac2.RunHistory)
	}

	// 已含相同 system → 不注入、不替换（字节稳定 = provider 前缀缓存命中，零历史噪音）
	same := []core.Message{
		core.NewSystemMessage(composed),
		core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
	}
	ac3 := loop.Run(context.Background(), same, nil)
	if len(ac3.Messages) != 2 {
		t.Fatalf("matching system must not be touched: %+v", ac3.Messages)
	}
	for _, m := range ac3.RunHistory {
		if m.Role == core.System {
			t.Fatalf("matching system must not append history: %+v", ac3.RunHistory)
		}
	}
}

// TestMissingLLMEnd 验证 provider 未发 LLMEnd 事件时按失败处理（防假成功）。
func TestMissingLLMEnd(t *testing.T) {
	// 返回 nil 但一个事件都不发
	p := &mockProvider{
		rounds: []func(handler func(e provider.StreamEvent) error) error{
			func(handler func(e provider.StreamEvent) error) error {
				return nil
			},
		},
	}
	loop := NewAgentLoop(WithProvider(p), WithMaxRetries(2))

	handler, list := collectEvents()
	ac := loop.Run(context.Background(),
		[]core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler)

	if ac.Err == nil {
		t.Fatal("missing LLMEnd must be treated as failure")
	}
	if errs := collectLLMErrors(list); len(errs) != 2 {
		t.Fatalf("want 2 llm_error events (retried once), got %d", len(errs))
	}
}

// countingCompressor 简单压缩策略：预算>0 且消息数>=1 即压缩为第一条。
type countingCompressor struct {
	compactCalls int
}

func (c *countingCompressor) ShouldCompact(_ context.Context, stats CompressStats) bool {
	return stats.Budget > 0
}

func (c *countingCompressor) Compact(_ context.Context, messages []core.Message) (CompressResult, error) {
	c.compactCalls++
	return CompressResult{Messages: messages[:1]}, nil
}

// TestMaxIterations 验证最大轮数护栏。
func TestMaxIterations(t *testing.T) {
	p := &mockProvider{
		rounds: []func(handler func(e provider.StreamEvent) error) error{
			func(handler func(e provider.StreamEvent) error) error {
				return handler(provider.LLMEndEvent{
					FinishReason: core.FinishReasonToolCall,
					ToolCalls:    []core.ToolCall{{Id: "c1", Name: "echo", Arguments: "{}"}},
				})
			},
		},
	}

	engine := tools.NewToolEngine()
	engine.RegisterTool(context.Background(), echoTool{})

	loop := NewAgentLoop(
		WithProvider(p),
		WithToolEngine(engine),
		WithMaxIterations(3),
	)

	handler, _ := collectEvents()
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler)

	if ac.Iteration != 3 {
		t.Fatalf("want loop stopped at iteration 3, got %d", ac.Iteration)
	}
}

// TestMaxIterationsTruncation 验证护栏截断必须以 FinishReasonMaxIterations 暴露，
// 而非伪装成自然 stop——这正是「工具调用中途无后续输出、需手动输入继续」的根因：
// 任务未完成时被 MaxIterations 护栏强制终止，UI 却误判为正常完成。
func TestMaxIterationsTruncation(t *testing.T) {
	// 模型每轮都要求调用工具（模拟仍在工作的多步任务），轮次无限复用
	p := &mockProvider{
		rounds: []func(handler func(e provider.StreamEvent) error) error{
			func(handler func(e provider.StreamEvent) error) error {
				return handler(provider.LLMEndEvent{
					FinishReason: core.FinishReasonToolCall,
					ToolCalls:    []core.ToolCall{{Id: "c1", Name: "echo", Arguments: "{}"}},
				})
			},
		},
	}
	engine := tools.NewToolEngine()
	engine.RegisterTool(context.Background(), echoTool{})

	loop := NewAgentLoop(WithProvider(p), WithToolEngine(engine), WithMaxIterations(3))
	handler, evs := collectEvents()
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler)

	if ac.Iteration != 3 {
		t.Fatalf("want iteration 3, got %d", ac.Iteration)
	}
	// 护栏截断必须报 max_iterations（区别于模型自然 stop），否则 UI 误判「完成」。
	var end *events.AgentEnd
	for _, e := range *evs {
		if ae, ok := e.(*events.AgentEnd); ok {
			end = ae
		}
	}
	if end == nil {
		t.Fatal("expected AgentEnd event")
	}
	if end.FinishReason != core.FinishReasonMaxIterations {
		t.Fatalf("want finish_reason=max_iterations on truncation, got %q", end.FinishReason)
	}
	if ac.Err != nil {
		t.Fatalf("truncation must not be an error: %v", ac.Err)
	}
}

// TestNaturalStopKeepsStop 验证模型给出最终答复（无工具调用）自然结束时，
// AgentEnd 仍为 finish_reason=stop（不被新逻辑误标为 max_iterations）。
func TestNaturalStopKeepsStop(t *testing.T) {
	p := &mockProvider{
		rounds: []func(handler func(e provider.StreamEvent) error) error{
			func(handler func(e provider.StreamEvent) error) error {
				return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "all done"})
			},
		},
	}
	engine := tools.NewToolEngine()
	engine.RegisterTool(context.Background(), echoTool{})

	loop := NewAgentLoop(WithProvider(p), WithToolEngine(engine), WithMaxIterations(3))
	handler, evs := collectEvents()
	loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler)

	var end *events.AgentEnd
	for _, e := range *evs {
		if ae, ok := e.(*events.AgentEnd); ok {
			end = ae
		}
	}
	if end == nil {
		t.Fatal("expected AgentEnd event")
	}
	if end.FinishReason != core.FinishReasonStop {
		t.Fatalf("natural stop must report finish_reason=stop, got %q", end.FinishReason)
	}
}

// TestUnregisteredTool 验证未注册工具的协议完整性：必须回写 tool 消息（标 IsError），
// 否则 assistant+tool_calls 无配对（DeepSeek 等推理模型回传必 400）。
func TestUnregisteredTool(t *testing.T) {
	eng := tools.NewToolEngine() // 空引擎：无任何注册工具
	p := &mockProvider{
		rounds: []func(handler func(e provider.StreamEvent) error) error{
			func(handler func(e provider.StreamEvent) error) error {
				return handler(provider.LLMEndEvent{
					FinishReason: core.FinishReasonToolCall,
					ToolCalls:    []core.ToolCall{{Id: "c1", Index: 0, Name: "ghost_tool", Arguments: `{}`}},
				})
			},
			func(handler func(e provider.StreamEvent) error) error {
				return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "done"})
			},
		},
	}
	loop := NewAgentLoop(WithProvider(p), WithToolEngine(eng))
	ac := loop.Run(context.Background(), []core.Message{
		core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
	}, nil)
	if ac.Err != nil {
		t.Fatal(ac.Err)
	}

	var toolMsg *core.Message
	for i := range ac.Messages {
		if ac.Messages[i].Role == core.Tool {
			toolMsg = &ac.Messages[i]
		}
	}
	if toolMsg == nil {
		t.Fatalf("unregistered tool must produce a tool message: %+v", ac.Messages)
	}
	if !strings.Contains(toolMsg.Content[0].Content, "not registered") {
		t.Fatalf("tool message should carry error result: %+v", toolMsg)
	}
}

// TestOnTurnCheckpoint 验证每轮结束检查点回调：工具轮与 stop 轮都触发，
// 回调时 ac.Messages 已含本轮产出。
func TestOnTurnCheckpoint(t *testing.T) {
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), echoTool{})
	p := &mockProvider{
		rounds: []func(handler func(e provider.StreamEvent) error) error{
			func(handler func(e provider.StreamEvent) error) error {
				return handler(provider.LLMEndEvent{
					FinishReason: core.FinishReasonToolCall,
					ToolCalls:    []core.ToolCall{{Id: "c1", Index: 0, Name: "echo", Arguments: `{"a":1}`}},
				})
			},
			func(handler func(e provider.StreamEvent) error) error {
				return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "done"})
			},
		},
	}

	var turns []int
	loop := NewAgentLoop(WithProvider(p), WithToolEngine(eng))
	_ = loop.RunStream(context.Background(), []core.Message{
		core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
	}, nil, &RunOptions{
		OnTurn: func(ac *AgentContext) {
			turns = append(turns, len(ac.Messages))
		},
	})

	// 工具轮后 [system, user, assistant, tool] = 4；stop 轮（非流式）回复只进
	// RunHistory，Messages 保持 4 —— 检查点回调仍各触发一次
	if len(turns) != 2 || turns[0] != 4 || turns[1] != 4 {
		t.Fatalf("want 2 turn callbacks at message counts [4 4], got %v", turns)
	}
}

// TestMaxIterationsUnlimited 验证 WithMaxIterations(0) = 不限制：
// 模型持续调用工具（单轮脚本复用）时运行不被默认护栏截断；Abort 可终止。
func TestMaxIterationsUnlimited(t *testing.T) {
	p := &mockProvider{rounds: []func(handler func(e provider.StreamEvent) error) error{
		func(handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonToolCall,
				ToolCalls:    []core.ToolCall{{Id: "c1", Index: 0, Name: "echo", Arguments: `{}`}},
			})
		},
	}}
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), echoTool{})

	loop := NewAgentLoop(WithProvider(p), WithToolEngine(eng), WithMaxIterations(0))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan *AgentContext, 1)
	done := make(chan struct{})
	var ac *AgentContext
	go func() {
		ac = loop.RunStream(ctx, []core.Message{
			core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
		}, nil, &RunOptions{
			OnRunning: func(r *AgentContext) { started <- r },
		})
		close(done)
	}()

	ac = <-started
	time.Sleep(100 * time.Millisecond) // 远超过默认护栏 10 轮的运行时长
	ac.Abort()                         // mock 无视 ctx 取消，用 abort 标志终止
	<-done

	if ac.Iteration <= 10 {
		t.Fatalf("unlimited iterations should exceed default guard, got %d", ac.Iteration)
	}
	if !ac.IsAborted() {
		t.Fatalf("run should end by abort")
	}
}

// blockingProvider 模拟「流式响应持续输出且只在 ctx 取消时解除阻塞」的 provider：
// 验证客户端中断（Abort）必须能立即打断 LLM 流式调用，而非等它自己结束。
type blockingProvider struct {
	started chan struct{}
}

func (b *blockingProvider) Stream(ctx context.Context, _ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	// 先发 LLMStart，让运行真正进入「流式输出中」状态
	if err := handler(provider.LLMStartEvent{Model: "block"}); err != nil {
		return err
	}
	close(b.started)
	// 持续输出直到被取消（模拟模型一直流式吐字）
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
			if err := handler(provider.LLMContentDeltaEvent{Delta: "."}); err != nil {
				return err
			}
		}
	}
}

// TestAbortInterruptsStreamingImmediately 客户端中断 Abort 是否立即中断 LLM 流式调用。
func TestAbortInterruptsStreamingImmediately(t *testing.T) {
	p := &blockingProvider{started: make(chan struct{})}
	loop := NewAgentLoop(WithProvider(p))

	done := make(chan struct{})
	var ac *AgentContext
	go func() {
		ac = loop.RunStream(context.Background(), []core.Message{
			core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
		}, nil, &RunOptions{
			OnRunning: func(r *AgentContext) { ac = r },
		})
		close(done)
	}()

	// 等流式已开始（LLMStart 后持续输出中）
	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never started")
	}
	time.Sleep(50 * time.Millisecond) // 确认已在持续输出

	abortAt := time.Now()
	ac.Abort()

	select {
	case <-done:
		latency := time.Since(abortAt)
		if latency > 1*time.Second {
			t.Fatalf("abort did not interrupt LLM streaming promptly: %v", latency)
		}
		t.Logf("LLM stream aborted in %v", latency)
	case <-time.After(3 * time.Second):
		t.Fatal("abort did not interrupt the blocking LLM stream (still running after 3s)")
	}
}

// bufferedProvider 复现 go-openai 的 bufio 缓冲行为：ctx 取消后仍继续向 handler 投递
// reasoning/content 块（缓冲已进内存、Recv 仍逐行吐出），直到 handler 返回错误为止。
// 用于验证中断守卫：abort 后首个 post-abort chunk 必须被 handler 以错误拦截，
// 不再翻译成客户端事件（即「已中断后仍持续流式输出」的根因修复）。
type bufferedProvider struct {
	started chan struct{}
}

func (b *bufferedProvider) Stream(ctx context.Context, req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	if err := handler(provider.LLMStartEvent{Model: "buf"}); err != nil {
		return err
	}
	close(b.started)
	// 交替吐 reasoning / content；即使 ctx 取消也不自查，全靠 handler 返回错误来停。
	for i := 0; ; i++ {
		if i%2 == 0 {
			if err := handler(provider.LLMReasoningDeltaEvent{Delta: "r"}); err != nil {
				return err
			}
		} else {
			if err := handler(provider.LLMContentDeltaEvent{Delta: "c"}); err != nil {
				return err
			}
		}
		time.Sleep(3 * time.Millisecond)
	}
}

// TestAbortStopsBufferedStreamEmission 中断后 provider 缓冲的后续 chunk 不得再翻译为事件
// （已中断后客户端仍持续流式输出的根因）。abort 之后不得再有任何 ContentChunk 发出。
func TestAbortStopsBufferedStreamEmission(t *testing.T) {
	p := &bufferedProvider{started: make(chan struct{})}
	loop := NewAgentLoop(WithProvider(p))

	var mu sync.Mutex
	postAbortContent := 0 // abort 之后才发出的 content 块（根因度量）
	endReason := core.FinishReasonStop
	aborted := atomic.Bool{}
	handler := func(_ context.Context, e events.Event) {
		mu.Lock()
		defer mu.Unlock()
		switch ev := e.(type) {
		case *events.ContentChunk:
			if aborted.Load() {
				postAbortContent++
			}
		case *events.AgentEnd:
			endReason = ev.FinishReason
		}
	}

	done := make(chan struct{})
	var ac *AgentContext
	go func() {
		ac = loop.RunStream(context.Background(), []core.Message{
			core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
		}, handler, &RunOptions{
			OnRunning: func(r *AgentContext) { ac = r },
		})
		close(done)
	}()

	select {
	case <-p.started:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never started")
	}
	time.Sleep(20 * time.Millisecond) // 让 reasoning/content 开始流出
	aborted.Store(true)               // 标记：此后的 content 均为「中断后发出」
	ac.Abort()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("abort did not stop the buffered provider stream")
	}

	mu.Lock()
	defer mu.Unlock()
	if endReason != core.FinishReasonAbort {
		t.Fatalf("want finish_reason=abort, got %q", endReason)
	}
	// 关键断言：abort 之后缓冲的 content 不得再翻译成事件（修复前会持续流出）
	if postAbortContent > 0 {
		t.Fatalf("content chunks leaked AFTER abort: %d (中断后不应再输出内容)", postAbortContent)
	}
	t.Logf("post-abort content=%d (must be 0)", postAbortContent)
}

// firstUserInput 取最近一条 user（多任务场景 AgentStart.Content 应为本次输入
// 而非历史首条——trace 审计 2026-08-10 修复）。
func TestFirstUserInputLastMessage(t *testing.T) {
	msgs := []core.Message{
		core.NewSystemMessage("s"),
		core.NewUserMessage(core.Content{Type: "text", Content: "任务1"}),
		core.NewAssistantMessage([]core.Content{{Type: "text", Content: "a"}}, "", nil),
		core.NewToolMessage("c1", "r"),
		core.NewUserMessage(core.Content{Type: "text", Content: "任务2"}),
	}
	if got := firstUserInput(msgs); got != "任务2" {
		t.Errorf("应取最近一条 user（本次输入），got %q", got)
	}
	// 空输入 / 无 user 回退
	if got := firstUserInput(nil); got != "" {
		t.Errorf("空输入应返回空，got %q", got)
	}
}

// —— 长时运行异步化：工具 promote（docs/ASYNC_TOOLS_AND_PROMOTE.md Phase 2）——

// blockTool 可释放阻塞工具：Call 阻塞直到 release 或 ctx 取消（模拟长时工具）。
type blockTool struct {
	gate  chan struct{}
	once  sync.Once
	calls atomic.Int64
}

// eventSnap 线程安全事件收集器（运行中可并发读取快照）。
type eventSnap struct {
	mu   sync.Mutex
	list []events.Event
}

func (s *eventSnap) handler() events.EventHandler {
	return func(_ context.Context, e events.Event) {
		s.mu.Lock()
		s.list = append(s.list, e)
		s.mu.Unlock()
	}
}

func (s *eventSnap) snapshot() []events.Event {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]events.Event{}, s.list...)
}

func waitAgents(t *testing.T, cond func() bool, timeout time.Duration) {
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

func (b *blockTool) Name() string                                      { return "block" }
func (b *blockTool) Description() string                               { return "blocks until released" }
func (b *blockTool) Parameters() any                                   { return map[string]any{"type": "object"} }
func (b *blockTool) CanParallel() bool                                 { return true }
func (b *blockTool) ValidParams(context.Context, string, string) error { return nil }
func (b *blockTool) BeforeCall(context.Context, core.ToolCall) tools.BeforeToolCallResponse {
	return tools.BeforeToolCallResponse{}
}
func (b *blockTool) AfterCall(context.Context, core.ToolCall) {}
func (b *blockTool) Release()                                 { b.once.Do(func() { close(b.gate) }) }
func (b *blockTool) Call(ctx context.Context, name, arguments string) (string, error) {
	b.calls.Add(1)
	select {
	case <-b.gate:
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return "block-done:" + name, nil
}

// TestPromoteToolTask 集成：运行中长工具被摘离 → runTools 返回占位结果（模型下轮
// 看到"后台运行中"续跑）→ 原工具后台完成 → TaskSink 收到真实结果。
func TestPromoteToolTask(t *testing.T) {
	block := &blockTool{gate: make(chan struct{})}
	p := &mockProvider{rounds: []func(handler func(e provider.StreamEvent) error) error{
		func(handler func(e provider.StreamEvent) error) error { // 轮1：发起 block 工具调用
			_ = handler(provider.LLMStartEvent{})
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonToolCall,
				ToolCalls:    []core.ToolCall{{Id: "c1", Index: 0, Name: "block", Arguments: `{}`}},
			})
		},
		func(handler func(e provider.StreamEvent) error) error { // 轮2：看到占位，正常结束
			_ = handler(provider.LLMStartEvent{})
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonStop,
				Content:      "moved long tool to background, continuing",
			})
		},
	}}

	engine := tools.NewToolEngine()
	engine.RegisterTool(context.Background(), block)

	var sinkMu sync.Mutex
	var sinkResults []core.ToolResult
	sink := func(taskID string, r core.ToolResult, interrupted bool) {
		sinkMu.Lock()
		sinkResults = append(sinkResults, r)
		sinkMu.Unlock()
	}

	snap := &eventSnap{}
	acCh := make(chan *AgentContext, 1)
	runDone := make(chan *AgentContext, 1)
	go func() {
		loop := NewAgentLoop(
			WithProvider(p),
			WithToolEngine(engine),
			WithModel("mock-model"),
			WithMaxIterations(3),
		)
		ac := loop.RunStream(context.Background(),
			[]core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})},
			snap.handler(), &RunOptions{
				OnRunning: func(a *AgentContext) { acCh <- a },
				TaskSink:  sink,
			})
		runDone <- ac
	}()

	var ac *AgentContext
	select {
	case ac = <-acCh:
	case <-time.After(2 * time.Second):
		t.Fatal("OnRunning not called")
	}
	waitAgents(t, func() bool { return block.calls.Load() >= 1 }, 2*time.Second)

	// 从 ToolStart 事件取 task_id（前端关联同源）
	taskID := ""
	waitAgents(t, func() bool {
		for _, ev := range snap.snapshot() {
			if ts, ok := ev.(*events.ToolStart); ok && ts.Id == "c1" && ts.TaskId != "" {
				taskID = ts.TaskId
				return true
			}
		}
		return false
	}, 2*time.Second)

	if !ac.PromoteToolTask(taskID) {
		t.Fatal("PromoteToolTask must succeed for running tool")
	}
	// 幂等：重复 promote 不 panic（once）
	if !ac.PromoteToolTask(taskID) {
		t.Fatal("repeat promote must not fail")
	}

	// 主 loop 拿到占位结果，模型下轮续跑并正常结束
	var runAC *AgentContext
	select {
	case runAC = <-runDone:
	case <-time.After(5 * time.Second):
		t.Fatal("run must finish after promote")
	}
	if runAC.Iteration < 2 {
		t.Fatalf("model must continue after promote, iterations=%d", runAC.Iteration)
	}
	// 上下文里 c1 的结果 = 占位 {status:running}
	var ph map[string]string
	found := false
	for _, m := range runAC.Messages {
		if m.Role != core.Tool || len(m.Content) == 0 {
			continue
		}
		if err := json.Unmarshal([]byte(m.Content[0].Content), &ph); err == nil && ph["status"] == "running" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("placeholder {status:running} must be written to history, got %+v", runAC.Messages)
	}

	// 摘离任务后台完成 → TaskSink 收到真实结果
	block.Release()
	waitAgents(t, func() bool {
		sinkMu.Lock()
		defer sinkMu.Unlock()
		return len(sinkResults) == 1
	}, 2*time.Second)
	sinkMu.Lock()
	if sinkResults[0].Result != "block-done:block" || sinkResults[0].IsError {
		t.Fatalf("TaskSink result: %+v", sinkResults[0])
	}
	sinkMu.Unlock()
}

// blockingCompressor 阻塞压缩器：Compact 一直等到 ctx 取消（模拟真实 LLM 压缩调用在途）。
type blockingCompressor struct {
	started chan struct{} // Compact 已进入（压缩调用在途）
}

func (c *blockingCompressor) ShouldCompact(_ context.Context, stats CompressStats) bool {
	return stats.Budget > 0
}

func (c *blockingCompressor) Compact(ctx context.Context, _ []core.Message) (CompressResult, error) {
	close(c.started)
	<-ctx.Done()
	return CompressResult{}, ctx.Err()
}

// TestAbortDuringCompression 验证压缩在途时中止（Session.Interrupt → ac.Abort）：
// 压缩调用被取消、CompressEnd 带错误发出（压缩中断 = 无副作用）、Run 以 abort 收尾而非失败。
// 回归锚点：压缩期间用户点「停止」必须能终止（此前前端把停止按钮锁死，只能等 5 分钟超时）。
func TestAbortDuringCompression(t *testing.T) {
	comp := &blockingCompressor{started: make(chan struct{})}
	p := &mockProvider{
		rounds: []func(handler func(e provider.StreamEvent) error) error{
			func(handler func(e provider.StreamEvent) error) error {
				t.Fatal("压缩中止后不应再发起 LLM 调用")
				return nil
			},
		},
	}
	loop := NewAgentLoop(WithProvider(p), WithCompressor(comp))
	handler, list := collectEvents()

	var acRef atomic.Pointer[AgentContext]
	done := make(chan *AgentContext, 1)
	go func() {
		// 命令消息（compact）经注入段剥出 → 本轮强制压缩；压缩阻塞在 ctx 取消。
		ac := loop.RunStream(context.Background(),
			[]core.Message{core.NewCommandMessage("compact", "")},
			handler,
			&RunOptions{OnRunning: func(ac *AgentContext) { acRef.Store(ac) }},
		)
		done <- ac
	}()

	select {
	case <-comp.started:
	case <-time.After(3 * time.Second):
		t.Fatal("压缩调用未开始")
	}
	// 在途中止：等价 Session.Interrupt → running.Abort()
	if ac := acRef.Load(); ac != nil {
		ac.Abort()
	} else {
		t.Fatal("run 尚未注册 ac（无法中止）")
	}

	select {
	case ac := <-done:
		if !ac.IsAborted() {
			t.Fatal("run 应标记为已中止")
		}
		if ac.Err != nil {
			t.Fatalf("中止不是失败，ac.Err 应为 nil, got %v", ac.Err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("中止后 run 未及时收尾（压缩调用未随 ctx 取消）")
	}

	// CompressEnd 必须已发出（前端据此解除 compressing 锁）；错误 = ctx 取消
	var endSeen bool
	for _, e := range *list {
		if ce, ok := e.(*events.CompressEnd); ok {
			endSeen = true
			if ce.Error == "" {
				t.Fatal("中止路径 CompressEnd 应携带错误（ctx 取消），否则前端无法区分完成/中止")
			}
		}
	}
	if !endSeen {
		t.Fatal("中止后应发出 CompressEnd（前端 compressing 状态依赖它解锁）")
	}
	var end *events.AgentEnd
	for _, e := range *list {
		if a, ok := e.(*events.AgentEnd); ok {
			end = a
		}
	}
	if end == nil || end.FinishReason != core.FinishReasonAbort {
		t.Fatalf("AgentEnd 应以 abort 收尾, got %+v", end)
	}
}

// TestLLMEndZeroUsageNotFabricated 问题九回归：上游未回 usage（零值）时，
// Agent 事件 LLMEnd.Usage 必须为 nil（序列化 null），不得伪造 {"Input":0}；
// 有真实用量时照常携带。
func TestLLMEndZeroUsageNotFabricated(t *testing.T) {
	newLoop := func(rounds []func(func(e provider.StreamEvent) error) error) (*AgentLoop, *[]events.Event, events.EventHandler) {
		p := &mockProvider{rounds: rounds}
		loop := NewAgentLoop(WithProvider(p), WithModel("m1"))
		h, list := collectEvents()
		return loop, list, h
	}
	findEnd := func(list *[]events.Event) *events.LLMEnd {
		for i := len(*list) - 1; i >= 0; i-- {
			if e, ok := (*list)[i].(*events.LLMEnd); ok {
				return e
			}
		}
		return nil
	}

	// 零值用量 → Usage == nil
	loop, list, h := newLoop([]func(func(e provider.StreamEvent) error) error{
		func(handler func(e provider.StreamEvent) error) error {
			_ = handler(provider.LLMStartEvent{Model: "m1"})
			_ = handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "hi"})
			return nil
		},
	})
	loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "q"})}, h)
	end := findEnd(list)
	if end == nil {
		t.Fatal("no LLMEnd event")
	}
	if end.Usage != nil {
		t.Fatalf("zero usage must not be fabricated: %+v", end.Usage)
	}

	// 真实用量 → 照常携带
	loop2, list2, h2 := newLoop([]func(func(e provider.StreamEvent) error) error{
		func(handler func(e provider.StreamEvent) error) error {
			_ = handler(provider.LLMStartEvent{Model: "m1"})
			_ = handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "hi", Usage: core.Usage{Input: 100, Output: 10}})
			return nil
		},
	})
	loop2.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "q"})}, h2)
	end2 := findEnd(list2)
	if end2 == nil || end2.Usage == nil || end2.Usage.Input != 100 {
		t.Fatalf("real usage lost: %+v", end2)
	}

	// 仅 reasoning 用量（思考模型 usage 只报 reasoning 的边缘形态）也保留
	loop3, list3, h3 := newLoop([]func(func(e provider.StreamEvent) error) error{
		func(handler func(e provider.StreamEvent) error) error {
			_ = handler(provider.LLMStartEvent{Model: "m1"})
			_ = handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Usage: core.Usage{Reasoning: 5}})
			return nil
		},
	})
	loop3.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "q"})}, h3)
	end3 := findEnd(list3)
	if end3 == nil || end3.Usage == nil || end3.Usage.Reasoning != 5 {
		t.Fatalf("reasoning-only usage lost: %+v", end3)
	}
}

// TestPostToolEmptyRoundNudgesOnce 问题十三回归：工具轮之后模型输出空回复轮（仅
// reasoning、无 content 无工具调用）时，注入一次性 ephemeral 续跑提醒并继续——
// 不再静默 AgentEnd；再空则按现状结束（防死循环）。非工具轮后的空轮维持原语义。
func TestPostToolEmptyRoundNudgesOnce(t *testing.T) {
	var mu sync.Mutex
	var round3SawNudge, round4SawNudge bool
	var p *captureRoundsProvider // 先声明后赋值：轮3 闭包需引用 p.requests
	p = &captureRoundsProvider{rounds: []func(func(provider.StreamEvent) error) error{
		func(handler func(e provider.StreamEvent) error) error { // 轮1：工具调用
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonToolCall,
				ToolCalls:    []core.ToolCall{{Id: "c1", Index: 0, Name: "echo", Arguments: `{}`}},
			})
		},
		func(handler func(e provider.StreamEvent) error) error { // 轮2：空回复轮（仅 reasoning）
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Reasoning: "想完了没说"})
		},
		func(handler func(e provider.StreamEvent) error) error { // 轮3：应看到 nudge → 正常回复
			p.mu.Lock()
			var req *provider.StreamRequest
			if n := len(p.requests); n > 0 {
				req = p.requests[n-1]
			}
			p.mu.Unlock()
			if req != nil {
				for _, mm := range req.Messages {
					if mm.Role == core.User && strings.Contains(mm.Content[0].Content, "Do not stop silently") {
						mu.Lock()
						round3SawNudge = true
						mu.Unlock()
					}
				}
			}
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "done"})
		},
	}}
	_ = round4SawNudge
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), echoTool{})
	loop := NewAgentLoop(WithProvider(p), WithToolEngine(eng), WithPostToolNudgeEnabled())
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "task"})}, func(context.Context, events.Event) {})
	if ac.Err != nil {
		t.Fatalf("run err: %v", ac.Err)
	}
	mu.Lock()
	defer mu.Unlock()
	if !round3SawNudge {
		t.Fatal("轮3 请求应包含空轮续跑提醒")
	}
	if ac.FinalResponse != "done" {
		t.Fatalf("FinalResponse = %q, want done", ac.FinalResponse)
	}
}

// TestPostToolEmptyRoundSecondStops 问题十三防死循环：nudge 之后的再次空轮按现状结束。
func TestPostToolEmptyRoundSecondStops(t *testing.T) {
	p := &captureRoundsProvider{rounds: []func(func(provider.StreamEvent) error) error{
		func(handler func(e provider.StreamEvent) error) error { // 轮1：工具调用
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonToolCall,
				ToolCalls:    []core.ToolCall{{Id: "c1", Index: 0, Name: "echo", Arguments: `{}`}},
			})
		},
		func(handler func(e provider.StreamEvent) error) error { // 轮2：空轮 → nudge
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Reasoning: "空"})
		},
		func(handler func(e provider.StreamEvent) error) error { // 轮3：nudge 后仍空 → 按现状结束（不 nudge、不死循环）
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Reasoning: "还是空"})
		},
	}}
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), echoTool{})
	loop := NewAgentLoop(WithProvider(p), WithToolEngine(eng), WithPostToolNudgeEnabled())
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "task"})}, func(context.Context, events.Event) {})
	if ac.Err != nil {
		t.Fatalf("run err: %v", ac.Err)
	}
	if ac.FinalResponse != "" {
		t.Fatalf("二次空轮应按现状结束, got %q", ac.FinalResponse)
	}
}

// TestPostToolNudgeDefaultOff 默认语义回归（用户决策 2026-08-24）：空轮无法区分
// 「卡住」与「已完成但静默」，默认不启用续跑提醒——工具轮后空轮直接结束。
// 需要兜底的模型由宿主显式 WithPostToolNudgeEnabled()。
func TestPostToolNudgeDefaultOff(t *testing.T) {
	p := &captureRoundsProvider{rounds: []func(func(provider.StreamEvent) error) error{
		func(handler func(e provider.StreamEvent) error) error { // 轮1：工具调用
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonToolCall,
				ToolCalls:    []core.ToolCall{{Id: "c1", Index: 0, Name: "echo", Arguments: `{}`}},
			})
		},
		func(handler func(e provider.StreamEvent) error) error { // 轮2：空轮 → 开关关闭 → 直接结束
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Reasoning: "空"})
		},
	}}
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), echoTool{})
	loop := NewAgentLoop(WithProvider(p), WithToolEngine(eng)) // 默认不启用
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "task"})}, func(context.Context, events.Event) {})
	if ac.Err != nil {
		t.Fatalf("run err: %v", ac.Err)
	}
	if len(p.requests) != 2 {
		t.Fatalf("默认不启用 nudge（仅 2 轮请求）, got %d", len(p.requests))
	}
}

// TestMessageCharsCountsToolImage 工具消息里的图片同样按固定视觉 token 估算
// （不能把 base64 当文本 —— 否则一张截图就会误触发压缩）。
func TestMessageCharsCountsToolImage(t *testing.T) {
	img := core.Content{Type: core.ContentTypeImage, Content: strings.Repeat("A", 5_000_000), MimeType: "image/png"}
	toolMsg := core.NewToolMessageWithImages("t1", "screenshot", []core.Content{img})
	got := messageChars(toolMsg)
	want := int64(len("screenshot")) + estimatedImageTokens*4
	if got != want {
		t.Fatalf("tool image chars = %d, want %d (base64 must not count)", got, want)
	}
}

// TestToolImageOnlyMessageNotTreatedAsEmpty 纯图片 tool 消息（无文本）不得被
// 「空结果」处理路径丢弃（空 assistant 过滤 / 压缩输入渲染的边界）。
func TestToolImageOnlyMessageNotTreatedAsEmpty(t *testing.T) {
	m := core.NewToolMessageWithImages("t1", "", []core.Content{
		{Type: core.ContentTypeImage, Content: "data:image/png;base64,SHOT"},
	})
	if len(m.Content) != 2 || m.Content[0].Type != core.ContentTypeText || m.Content[1].Type != core.ContentTypeImage {
		t.Fatalf("blocks = %+v", m.Content)
	}
	// 空 text + image 的 tool 消息必须能通过校验（role/toolCallId 不变量）
	if err := m.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
}
