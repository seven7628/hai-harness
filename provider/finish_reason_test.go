package provider

import (
	"context"
	"errors"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// finish_reason 三态归一（2026-10）：网关「发了工具调用但 finish_reason 缺失或写成 stop」
// 时，若原样透传，agents 层（只在 ==toolCall 时执行工具，agent_loop.go:1156）会**静默丢弃
// 工具调用**、会话看着像正常结束。三条路径都要覆盖。
func TestFinishReasonInference(t *testing.T) {
	toolDelta := map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "gpt-5",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
			"tool_calls": []any{map[string]any{
				"index": 0, "id": "call_1", "type": "function",
				"function": map[string]any{"name": "echo", "arguments": `{"a":"x"}`},
			}},
		}}},
	}
	textDelta := map[string]any{
		"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "gpt-5",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "done"}}},
	}
	finish := func(v string) map[string]any {
		return map[string]any{
			"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": v}},
		}
	}

	cases := []struct {
		name      string
		chunks    []map[string]any
		want      core.FinishReason
		wantRaw   string
		wantTools int
	}{
		{
			name:      "缺失 + 有工具调用 → toolCall",
			chunks:    []map[string]any{toolDelta},
			want:      core.FinishReasonToolCall,
			wantTools: 1,
		},
		{
			name:      "stop + 有工具调用 → toolCall（网关常见形态）",
			chunks:    []map[string]any{toolDelta, finish("stop")},
			want:      core.FinishReasonToolCall,
			wantRaw:   "stop",
			wantTools: 1,
		},
		{
			name:    "缺失 + 无工具调用 → stop",
			chunks:  []map[string]any{textDelta},
			want:    core.FinishReasonStop,
			wantRaw: "",
		},
		{
			name:    "未知取值 → stop（原始值保留）",
			chunks:  []map[string]any{textDelta, finish("weird_reason")},
			want:    core.FinishReasonStop,
			wantRaw: "weird_reason",
		},
		{
			name:      "未知取值 + 有工具调用 → toolCall（原始值保留）",
			chunks:    []map[string]any{toolDelta, finish("weird_reason")},
			want:      core.FinishReasonToolCall,
			wantRaw:   "weird_reason",
			wantTools: 1,
		},
		{
			name:    "content_filter 不被工具调用改写",
			chunks:  []map[string]any{toolDelta, finish("content_filter")},
			want:    core.FinishReasonContentFilter,
			wantRaw: "content_filter",
			// 内容过滤命中的工具调用不执行：ToolCalls 照常上报由上层决定，这里只断言
			// 结束原因不被改写成 toolCall（否则会被当成正常工具轮执行）。
			wantTools: 1,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv, _ := sseServer(t, c.chunks)
			defer srv.Close()
			p := NewOpenAIProvider(srv.URL, "k")
			var end *LLMEndEvent
			err := p.Stream(context.Background(), &StreamRequest{
				Model:    "gpt-5",
				Messages: []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})},
			}, func(e StreamEvent) error {
				if ev, ok := e.(LLMEndEvent); ok {
					end = &ev
				}
				return nil
			})
			if err != nil {
				t.Fatalf("Stream: %v", err)
			}
			if end == nil {
				t.Fatal("没有 LLMEndEvent")
			}
			if end.FinishReason != c.want {
				t.Fatalf("FinishReason = %q, want %q", end.FinishReason, c.want)
			}
			if end.RawFinishReason != c.wantRaw {
				t.Fatalf("RawFinishReason = %q, want %q", end.RawFinishReason, c.wantRaw)
			}
			if len(end.ToolCalls) != c.wantTools {
				t.Fatalf("ToolCalls = %d, want %d", len(end.ToolCalls), c.wantTools)
			}
		})
	}
}

// 严格模式（openai_compat.requires_finish_reason=true）：声明支持的端点没发 finish_reason
// = 流被截断 → 可重试的上游故障（pi "Stream ended without finish_reason" 同款语义）。
func TestFinishReasonStrictMode(t *testing.T) {
	strict := true
	chunks := []map[string]any{{
		"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "gpt-5",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hi"}}},
	}}

	srv, _ := sseServer(t, chunks)
	defer srv.Close()
	p := NewOpenAIProviderWithCapabilities(srv.URL, "k",
		OpenAICompatCapabilities{BasePath: "/v1", RequiresFinishReason: &strict})
	err := p.Stream(context.Background(), &StreamRequest{
		Model:    "gpt-5",
		Messages: []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})},
	}, func(StreamEvent) error { return nil })
	if err == nil {
		t.Fatal("严格模式下缺 finish_reason 应该报错")
	}
	if IsPermanent(err) {
		t.Fatalf("截断必须是可重试的（非永久）错误: %v", err)
	}
	var up *UpstreamUnavailableError
	if !errors.As(err, &up) {
		t.Fatalf("应是 UpstreamUnavailableError，got %T: %v", err, err)
	}

	// 默认（宽松）路径同一条流不报错 —— 兼容网关本来就常不发该字段。
	srv2, _ := sseServer(t, chunks)
	defer srv2.Close()
	p2 := NewOpenAIProvider(srv2.URL, "k")
	if err := p2.Stream(context.Background(), &StreamRequest{
		Model:    "gpt-5",
		Messages: []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})},
	}, func(StreamEvent) error { return nil }); err != nil {
		t.Fatalf("宽松模式不应报错: %v", err)
	}
}
