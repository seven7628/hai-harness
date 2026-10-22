package provider

import (
	"context"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// 端到端：只发 `usage.prompt_cache_hit_tokens` 的网关（不发 prompt_tokens_details）时，
// 归一层补出的明细必须真正被 SDK 读进 core.Usage.CacheRead —— 否则缓存命中被记成 miss。
func TestUsageCacheHitAliasEndToEnd(t *testing.T) {
	srv, _ := sseServer(t, []map[string]any{
		{
			"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hi"}}},
		},
		{
			"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
			"usage": map[string]any{
				"prompt_tokens": 4000, "completion_tokens": 8, "total_tokens": 4008,
				"prompt_cache_hit_tokens": 3840, // 原生名（DeepSeek/Kimi 形态）
			},
		},
	})
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL, "k")
	var end *LLMEndEvent
	if err := p.Stream(context.Background(), &StreamRequest{
		Model:    "gpt-5",
		Messages: []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})},
	}, func(e StreamEvent) error {
		if ev, ok := e.(LLMEndEvent); ok {
			end = &ev
		}
		return nil
	}); err != nil {
		t.Fatalf("Stream: %v", err)
	}
	if end == nil {
		t.Fatal("没有 LLMEndEvent")
	}
	if end.Usage.CacheRead != 3840 {
		t.Fatalf("CacheRead = %d, want 3840（prompt_cache_hit_tokens 未归一）", end.Usage.CacheRead)
	}
	if end.Usage.Input != 4000 {
		t.Fatalf("Input = %d, want 4000", end.Usage.Input)
	}
}
