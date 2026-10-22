package agents

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/tools"
)

// 上游重试提示（2026-10）：Retry-After / x-should-retry 由 provider 挂在错误上，
// 重试层必须听服务器的 —— 等够服务器要求的时间，或在明确不该重试/等待过久时快速失败。
func TestRetryHintHonored(t *testing.T) {
	t.Run("Retry-After 覆盖本地退避", func(t *testing.T) {
		err := provider.WithRetryHint(errors.New("429"), provider.RetryHint{
			After: 60 * time.Millisecond, HasAfter: true,
		})
		p := failingRounds(1, err)
		loop := NewAgentLoop(
			WithProvider(p), WithToolEngine(tools.NewToolEngine()), WithModel("mock-model"),
			WithRetryBackoff(func(int, error) time.Duration { return time.Millisecond }),
		)
		handler, list := collectEvents()
		ac := loop.RunStream(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler, nil)
		if ac.Err != nil {
			t.Fatalf("应重试后成功: %v", ac.Err)
		}
		errs := llmErrors(list)
		if len(errs) != 1 {
			t.Fatalf("LLMError 事件 = %d, want 1", len(errs))
		}
		if errs[0].RetryDelayMs < 60 {
			t.Fatalf("RetryDelayMs = %d, want >= 60（Retry-After 未生效）", errs[0].RetryDelayMs)
		}
	})

	t.Run("Retry-After 超长 → 不重试", func(t *testing.T) {
		err := provider.WithRetryHint(errors.New("429"), provider.RetryHint{
			After: 10 * time.Minute, HasAfter: true,
		})
		p := failingRounds(1, err)
		loop := NewAgentLoop(WithProvider(p), WithToolEngine(tools.NewToolEngine()), WithModel("mock-model"))
		handler, list := collectEvents()
		ac := loop.RunStream(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler, nil)
		if ac.Err == nil {
			t.Fatal("等待超过 MaxRetryAfter 应快速失败")
		}
		errs := llmErrors(list)
		if len(errs) != 1 || errs[0].WillRetry {
			t.Fatalf("应只尝试一次且不重试: %+v", errs)
		}
	})

	t.Run("x-should-retry=false → 不重试", func(t *testing.T) {
		no := false
		err := provider.WithRetryHint(errors.New("500"), provider.RetryHint{ShouldRetry: &no})
		p := failingRounds(1, err)
		loop := NewAgentLoop(WithProvider(p), WithToolEngine(tools.NewToolEngine()), WithModel("mock-model"))
		handler, list := collectEvents()
		ac := loop.RunStream(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler, nil)
		if ac.Err == nil {
			t.Fatal("x-should-retry=false 应不重试")
		}
		errs := llmErrors(list)
		if len(errs) != 1 || errs[0].WillRetry {
			t.Fatalf("应只尝试一次且不重试: %+v", errs)
		}
	})

	t.Run("无提示 → 行为不变", func(t *testing.T) {
		p := failingRounds(1, errors.New("transient"))
		loop := NewAgentLoop(
			WithProvider(p), WithToolEngine(tools.NewToolEngine()), WithModel("mock-model"),
			WithRetryBackoff(func(int, error) time.Duration { return time.Millisecond }),
		)
		handler, list := collectEvents()
		ac := loop.RunStream(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler, nil)
		if ac.Err != nil {
			t.Fatalf("应重试后成功: %v", ac.Err)
		}
		if len(llmErrors(list)) != 1 {
			t.Fatalf("应有一次失败事件")
		}
	})
}
