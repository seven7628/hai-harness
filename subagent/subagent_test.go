package subagent

import (
	"context"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/tools"
	"strings"
	"sync"
	"testing"
)

// mockProvider 按预设脚本发射 LLM 事件。
type mockProvider struct {
	rounds []func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error
	calls  int
}

func (m *mockProvider) Stream(_ context.Context, req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	round := m.rounds[m.calls]
	if m.calls < len(m.rounds)-1 {
		m.calls++
	}
	return round(req, handler)
}

func stopRound(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
}

// TestSubAgentBasic 验证完整链路：根 agent 调用 subagent 工具 →
// 子运行（独立上下文）→ 最终回复作为工具结果、用量计入父结算、
// 事件带 ParentRunId/Depth（可重建调用树）。
func TestSubAgentBasic(t *testing.T) {
	// 子 agent：一轮 stop，返回研究结果 + 用量
	childP := &mockProvider{rounds: []func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonStop,
				Content:      "研究结果：X 方案可行",
				Usage:        core.Usage{Input: 50, Output: 30, TotalTokens: 80, Cost: core.Cost{Total: 20}},
			})
		},
	}}
	childLoop := agents.NewAgentLoop(agents.WithProvider(childP), agents.WithModel("child-model"))

	// 根 agent：第一轮调 subagent 工具，第二轮结束
	engine := tools.NewToolEngine()
	engine.RegisterTool(context.Background(), NewTool(Spec{
		Name:        "subagent_research",
		Description: "派子 agent 做研究",
		Loop:        childLoop,
		MaxDepth:    1,
	}))
	rootP := &mockProvider{rounds: []func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonToolCall,
				ToolCalls: []core.ToolCall{
					{Id: "c1", Index: 0, Name: "subagent_research", Arguments: `{"name":"research","task":"研究 X"}`},
				},
				Usage: core.Usage{Input: 10, Output: 5},
			})
		},
		func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonStop,
				Content:      "完成",
				Usage:        core.Usage{Input: 10, Output: 5},
			})
		},
	}}
	rootLoop := agents.NewAgentLoop(
		agents.WithProvider(rootP),
		agents.WithToolEngine(engine),
		agents.WithSystemPrompt("你是协调者"),
	)

	handler, list := collectSubEvents()
	ac := rootLoop.Run(context.Background(), []core.Message{
		core.NewUserMessage(core.Content{Type: "text", Content: "研究一下 X"}),
	}, handler)
	if ac.Err != nil {
		t.Fatal(ac.Err)
	}

	// 子最终回复作为工具结果回传父上下文
	found := false
	for _, m := range ac.Messages {
		if m.Role == core.Tool && strings.Contains(m.Content[0].Content, "研究结果") {
			found = true
		}
	}
	if !found {
		t.Fatalf("subagent result should be in parent context: %+v", ac.Messages)
	}

	// 成本传导：父结算 = 根两轮 + 子一轮
	if ac.Usage.Input != 10+50+10 || ac.Usage.Output != 5+30+5 || ac.Usage.Cost.Total != 20 {
		t.Fatalf("sub usage should be accounted in parent: %+v", ac.Usage)
	}

	// 事件树：两个 AgentStart（根 Depth=0，子 ParentRunId=根 + Depth=1）
	var rootStart *events.AgentStart
	var childStart *events.AgentStart
	var childEnd *events.AgentEnd
	for _, e := range *list {
		if s, ok := e.(*events.AgentStart); ok {
			if s.ParentRunId == "" {
				rootStart = s
			} else {
				childStart = s
			}
		}
		if end, ok := e.(*events.AgentEnd); ok && end.ParentRunId != "" {
			childEnd = end
		}
	}
	if rootStart == nil || rootStart.Depth != 0 {
		t.Fatalf("root agent_start should have depth 0: %+v", rootStart)
	}
	if childStart == nil || childStart.ParentRunId != rootStart.RunId || childStart.Depth != 1 {
		t.Fatalf("child agent_start should link to root: %+v", childStart)
	}
	if childEnd == nil || childEnd.Usage == nil || childEnd.Usage.Input != 50 {
		t.Fatalf("child agent_end should carry sub usage: %+v", childEnd)
	}

	// ToolResponse 事件带子用量
	var tr *events.ToolResponse
	for _, e := range *list {
		if r, ok := e.(*events.ToolResponse); ok && r.Name == "subagent_research" {
			tr = r
		}
	}
	if tr == nil || tr.Usage == nil || tr.Usage.Input != 50 {
		t.Fatalf("tool_response should carry sub usage: %+v", tr)
	}
}

// TestSubAgentMaxDepth 验证深度限制：超过 Spec.MaxDepth 的嵌套调用被拒绝。
func TestSubAgentMaxDepth(t *testing.T) {
	childLoop := agents.NewAgentLoop(agents.WithProvider(&mockProvider{rounds: []func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{stopRound}}))
	tool := NewTool(Spec{Name: "sub", Description: "d", Loop: childLoop, MaxDepth: 1})

	// 根直接调用（无父上下文）：子深度 1 ≤ 1，允许
	if _, err := tool.Call(context.Background(), "", `{"task":"x"}`); err != nil {
		t.Fatalf("root-level call should be allowed: %v", err)
	}

	// 父深度 2（嵌套在子 agent 内）：子深度 3 > 1，拒绝
	ctx := events.WithToolContext(context.Background(), "parent", 2, nil, nil, nil)
	if _, err := tool.Call(ctx, "", `{"task":"x"}`); err == nil || !strings.Contains(err.Error(), "max depth") {
		t.Fatalf("want max depth error, got %v", err)
	}
}

// TestSubAgentParams 验证参数校验：name（必填、限长）+ task 必填。
func TestSubAgentParams(t *testing.T) {
	tool := NewTool(Spec{Name: "sub", Description: "d", Loop: nil})
	ctx := context.Background()
	if err := tool.ValidParams(ctx, "", `{"name":"review","task":"x"}`); err != nil {
		t.Fatalf("valid params rejected: %v", err)
	}
	for _, bad := range []string{`{}`, `{"task":""}`, `{"task":"x"}`, `{"name":"","task":"x"}`, `{"name":"` + strings.Repeat("a", 41) + `","task":"x"}`, `not-json`} {
		if err := tool.ValidParams(ctx, "", bad); err == nil {
			t.Fatalf("want error for %q", bad)
		}
	}
	if tool.CanParallel() {
		t.Fatal("subagent must be sequential")
	}
}

// TestSubAgentFailure 验证子运行失败：错误返回（引擎标 IsError），父不崩溃。
func TestSubAgentFailure(t *testing.T) {
	failP := &mockProvider{rounds: []func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonError, Content: ""})
		},
	}}
	childLoop := agents.NewAgentLoop(agents.WithProvider(failP), agents.WithMaxRetries(1))
	engine := tools.NewToolEngine()
	engine.RegisterTool(context.Background(), NewTool(Spec{Name: "sub", Description: "d", Loop: childLoop}))

	rootP := &mockProvider{rounds: []func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonToolCall,
				ToolCalls:    []core.ToolCall{{Id: "c1", Index: 0, Name: "sub", Arguments: `{"task":"x"}`}},
			})
		},
		func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
		},
	}}
	rootLoop := agents.NewAgentLoop(agents.WithProvider(rootP), agents.WithToolEngine(engine))
	ac := rootLoop.Run(context.Background(), []core.Message{
		core.NewUserMessage(core.Content{Type: "text", Content: "go"}),
	}, nil)
	if ac.Err != nil {
		t.Fatal(ac.Err)
	}
}

// TestToolContextRoundTrip 验证工具上下文的注入与取出。
func TestToolContextRoundTrip(t *testing.T) {
	if tc := events.ToolContextFrom(context.Background()); tc != nil {
		t.Fatal("uninjected context should return nil")
	}
	ctx := events.WithToolContext(context.Background(), "root-run", 0, nil, nil, nil)
	tc := events.ToolContextFrom(ctx)
	if tc == nil || tc.ParentRunId != "root-run" || tc.Depth != 0 {
		t.Fatalf("tool context mismatch: %+v", tc)
	}
}

func collectSubEvents() (events.EventHandler, *[]events.Event) {
	var mu sync.Mutex
	var list []events.Event
	return func(_ context.Context, e events.Event) {
		mu.Lock()
		defer mu.Unlock()
		list = append(list, e)
	}, &list
}
