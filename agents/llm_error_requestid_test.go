// llm_error_requestid_test.go —— 失败 attempt 的请求 id（2026-10）。
//
// 为什么事件必须带它：同一个 run 内 LLM 调用严格串行，但**重试会换新的 request id**。
// 没有它时下游只能靠「同 run 里唯一活着的那格」去猜（view_reducer 的 reqState 回收
// 原本就是这么写的），迟到事件按 request_id 命中错误的结算锚；有了它，回收与
// 「这次失败属于哪一轮」都变成精确操作。
package agents

import (
	"context"
	"errors"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

// TestLLMErrorCarriesAttemptRequestID 失败事件必须带上**这次 attempt** 的 request id
// （provider 的 LLMStart 给出）。下游据此精确归轮 —— view_reducer 的 reqState 回收、
// 前端按轮记账都不必再靠「同 run 里唯一活着的那格」去猜；重试会换新 id，串到别的
// attempt 上就会把迟到事件算到错误的结算锚。
func TestLLMErrorCarriesAttemptRequestID(t *testing.T) {
	usage := core.Usage{
		Input: 10, Output: 2, TotalTokens: 12,
		Cost: core.Cost{Input: 1, Output: 1, Total: 2, Priced: true, Source: core.CostSourceBuiltin},
	}
	failRound := func(handler func(e provider.StreamEvent) error) error {
		if err := handler(provider.LLMStartEvent{Model: "m1", RequestId: "req-a"}); err != nil {
			return err
		}
		return provider.WithPartialUsage(errors.New("stream broken"), usage)
	}
	okRound := func(handler func(e provider.StreamEvent) error) error {
		return handler(provider.LLMEndEvent{RequestId: "req-b", FinishReason: core.FinishReasonStop, Content: "ok", Model: "m1"})
	}
	p := &mockProvider{rounds: []func(handler func(e provider.StreamEvent) error) error{failRound, okRound}}
	loop := NewAgentLoop(WithProvider(p), WithModel("m1"), WithMaxRetries(3))

	handler, list := collectEvents()
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler)
	if ac.Err != nil {
		t.Fatalf("重试后应成功: %v", ac.Err)
	}
	errs := collectLLMErrors(list)
	if len(errs) != 1 {
		t.Fatalf("want 1 llm_error, got %d", len(errs))
	}
	if errs[0].RequestId != "req-a" {
		t.Fatalf("LLMError.RequestId = %q, want req-a（失败 attempt 自己的 id）", errs[0].RequestId)
	}
}

// TestLLMErrorWithoutStartHasNoRequestID 首事件之前就失败（连接失败 / 首字超时）：
// 没有 request id 可带 → 字段为空（omitempty），不编一个。
func TestLLMErrorWithoutStartHasNoRequestID(t *testing.T) {
	failRound := func(handler func(e provider.StreamEvent) error) error {
		return errors.New("connection refused")
	}
	p := &mockProvider{rounds: []func(handler func(e provider.StreamEvent) error) error{failRound}}
	loop := NewAgentLoop(WithProvider(p), WithModel("m1"), WithMaxRetries(1))

	handler, list := collectEvents()
	_ = loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})}, handler)
	errs := collectLLMErrors(list)
	if len(errs) != 1 {
		t.Fatalf("want 1 llm_error, got %d", len(errs))
	}
	if errs[0].RequestId != "" {
		t.Fatalf("首事件前失败不应伪造 request id: %q", errs[0].RequestId)
	}
}
