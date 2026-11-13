package agents

import (
	"context"
	"errors"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/tools"
	"testing"
	"time"
)

// failingRounds 构造 mockProvider：前 n 轮返回 failErr，之后成功一轮。
func failingRounds(n int, failErr error) *mockProvider {
	rounds := make([]func(handler func(e provider.StreamEvent) error) error, 0, n+1)
	for i := 0; i < n; i++ {
		rounds = append(rounds, func(func(e provider.StreamEvent) error) error { return failErr })
	}
	rounds = append(rounds, func(handler func(e provider.StreamEvent) error) error {
		if err := handler(provider.LLMStartEvent{}); err != nil {
			return err
		}
		return handler(provider.LLMEndEvent{
			FinishReason: core.FinishReasonStop,
			Content:      "ok",
			Usage:        core.Usage{Input: 5, Output: 3},
		})
	})
	return &mockProvider{rounds: rounds}
}

func llmErrors(list *[]events.Event) []*events.LLMError {
	var errs []*events.LLMError
	for _, e := range *list {
		if ev, ok := e.(*events.LLMError); ok {
			errs = append(errs, ev)
		}
	}
	return errs
}

// TestRetryBackoffApplies 退避生效：两次失败各等待注入的 20ms → 第三次成功；
// LLMError 事件带 Attempt/WillRetry/RetryDelayMs。
func TestRetryBackoffApplies(t *testing.T) {
	p := failingRounds(2, errors.New("transient"))
	loop := NewAgentLoop(
		WithProvider(p),
		WithToolEngine(tools.NewToolEngine()),
		WithModel("mock-model"),
		WithRetryBackoff(func(int, error) time.Duration { return 20 * time.Millisecond }),
	)
	handler, list := collectEvents()
	start := time.Now()
	ac := loop.RunStream(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler, nil)
	if ac.Err != nil {
		t.Fatalf("run should succeed after backoff retries: %v", ac.Err)
	}
	if elapsed := time.Since(start); elapsed < 40*time.Millisecond {
		t.Fatalf("two backoff waits should take >= 40ms, took %v", elapsed)
	}
	errs := llmErrors(list)
	if len(errs) != 2 || errs[0].Attempt != 1 || errs[1].Attempt != 2 || !errs[0].WillRetry || !errs[1].WillRetry {
		t.Fatalf("want 2 retryable LLMError, got %+v", errs)
	}
	if errs[0].RetryDelayMs != 20 {
		t.Fatalf("retry delay should be reported in event, got %+v", errs[0])
	}
}

// TestRetryBackoff429NotPermanent 限流错误（RateLimitError）走退避重试：
// 两次 429 → 第三次成功；每次失败一个 LLMError（WillRetry=true,true —— 第 3 次成功无错误事件）。
func TestRetryBackoff429NotPermanent(t *testing.T) {
	p := failingRounds(2, &provider.RateLimitError{StatusCode: 429, Err: errors.New("rl")})
	loop := NewAgentLoop(
		WithProvider(p),
		WithToolEngine(tools.NewToolEngine()),
		WithModel("mock-model"),
		WithRetryBackoff(func(int, error) time.Duration { return time.Millisecond }),
	)
	handler, list := collectEvents()
	ac := loop.RunStream(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler, nil)
	if ac.Err != nil {
		t.Fatalf("rate limit should be retried: %v", ac.Err)
	}
	errs := llmErrors(list)
	if len(errs) != 2 {
		t.Fatalf("want 2 failure events (3 attempts, last succeeds), got %+v", errs)
	}
	for i, ev := range errs {
		if ev.Attempt != i+1 || !ev.WillRetry || ev.RetryDelayMs <= 0 {
			t.Fatalf("attempt %d: want will_retry=true with delay, got %+v", i+1, ev)
		}
	}
}

// TestAgentEndErrorKind 错误传导到 Agent 层：重试耗尽 → AgentEnd.ErrorKind 分类标签
// （rate_limit 可稍后重试 / permanent 配置问题）。
func TestAgentEndErrorKind(t *testing.T) {
	// 429 重试耗尽（MaxRetries=2：一次失败即耗尽）→ AgentEnd{error, rate_limit}
	p := failingRounds(1, &provider.RateLimitError{StatusCode: 429, Err: errors.New("rl")})
	loop := NewAgentLoop(
		WithProvider(p),
		WithToolEngine(tools.NewToolEngine()),
		WithModel("mock-model"),
		WithMaxRetries(1), // 不重试：错误直达结算
	)
	handler, list := collectEvents()
	ac := loop.RunStream(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler, nil)
	if ac.Err == nil {
		t.Fatal("want run error")
	}
	var end *events.AgentEnd
	for _, e := range *list {
		if ev, ok := e.(*events.AgentEnd); ok {
			end = ev
		}
	}
	if end == nil || end.FinishReason != core.FinishReasonError || end.ErrorKind != provider.ErrorKindRateLimit {
		t.Fatalf("want AgentEnd error/rate_limit, got %+v", end)
	}

	// 永久错误 → permanent
	p2 := failingRounds(1, provider.MarkPermanent(errors.New("bad request")))
	loop2 := NewAgentLoop(
		WithProvider(p2),
		WithToolEngine(tools.NewToolEngine()),
		WithModel("mock-model"),
	)
	handler2, list2 := collectEvents()
	ac2 := loop2.RunStream(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler2, nil)
	if ac2.Err == nil {
		t.Fatal("want run error")
	}
	for _, e := range *list2 {
		if ev, ok := e.(*events.AgentEnd); ok {
			if ev.FinishReason != core.FinishReasonError || ev.ErrorKind != provider.ErrorKindPermanent {
				t.Fatalf("want AgentEnd error/permanent, got %+v", ev)
			}
		}
	}
}

// TestRetryBackoffDefaultGrowth 默认退避档位：普通 200ms 基线上限 5s；限流 1s 基线上限 60s；
// 等额抖动 → 非零、有界、单调增长。
func TestRetryBackoffDefaultGrowth(t *testing.T) {
	// 普通档位：attempt 1 ∈ [100ms, 200ms)
	d1 := DefaultBackoff(1, errors.New("boom"))
	if d1 < 100*time.Millisecond || d1 >= 200*time.Millisecond {
		t.Fatalf("generic attempt-1 backoff should be [100ms,200ms), got %v", d1)
	}
	// 单调增长且封顶 5s
	d2 := DefaultBackoff(2, errors.New("boom"))
	if d2 <= d1 || d2 > 5*time.Second {
		t.Fatalf("backoff should grow monotonically and cap at 5s, got %v -> %v", d1, d2)
	}
	if d6 := DefaultBackoff(10, errors.New("boom")); d6 > 5*time.Second {
		t.Fatalf("generic backoff must cap at 5s, got %v", d6)
	}
	// 限流档位：attempt 1 ∈ [500ms, 1s)，封顶 60s
	rl := &provider.RateLimitError{StatusCode: 429, Err: errors.New("rl")}
	d3 := DefaultBackoff(1, rl)
	if d3 < 500*time.Millisecond || d3 >= time.Second {
		t.Fatalf("rate-limit attempt-1 backoff should be [500ms,1s), got %v", d3)
	}
	d4 := DefaultBackoff(10, rl)
	if d4 <= d3 || d4 > 60*time.Second {
		t.Fatalf("rate-limit backoff should grow and cap at 60s, got %v -> %v", d3, d4)
	}
}

// TestRetryBackoffCtxCancelDuringWait 退避等待中取消：ctx 感知，立即以 Canceled 结束（不等退避耗尽）。
func TestRetryBackoffCtxCancelDuringWait(t *testing.T) {
	p := failingRounds(1, errors.New("transient"))
	loop := NewAgentLoop(
		WithProvider(p),
		WithToolEngine(tools.NewToolEngine()),
		WithModel("mock-model"),
		WithRetryBackoff(func(int, error) time.Duration { return 2 * time.Second }),
	)
	ctx, cancel := context.WithCancel(context.Background())
	handler, _ := collectEvents()
	start := time.Now()
	go func() {
		time.Sleep(50 * time.Millisecond) // 等待首轮失败进入退避等待
		cancel()
	}()
	ac := loop.RunStream(ctx, []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler, nil)
	if !errors.Is(ac.Err, context.Canceled) {
		t.Fatalf("cancel during backoff should end run with Canceled, got %v", ac.Err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("cancel should interrupt backoff wait, took %v", elapsed)
	}
}
