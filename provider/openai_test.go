package provider

import (
	"context"
	"encoding/json"
	"errors"
	"github.com/seven7628/hai-harness/core"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sashabaranov/go-openai"
)

func ptr[T any](v T) *T { return &v }

// sseServer 按给定 chunk 顺序返回 SSE 流，并捕获请求体。
func sseServer(t *testing.T, chunks []map[string]any) (*httptest.Server, map[string]any) {
	t.Helper()
	gotBody := map[string]any{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read body: %v", err)
		}
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Errorf("unmarshal body: %v", err)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		for _, c := range chunks {
			b, _ := json.Marshal(c)
			if _, err := w.Write([]byte("data: " + string(b) + "\n\n")); err != nil {
				return
			}
		}
	}))
	return srv, gotBody
}

func TestOpenAIStreamReasoningAndToolCall(t *testing.T) {
	srv, gotBody := sseServer(t, []map[string]any{
		{
			"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
				"role": "assistant", "reasoning_content": "正在思考",
			}}},
		},
		{
			"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "你好"}}},
		},
		{
			"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index": 0, "id": "call_1", "type": "function",
					"function": map[string]any{"name": "echo", "arguments": `{"a":`},
				}},
			}}},
		},
		{
			"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{
				"tool_calls": []any{map[string]any{
					"index": 0, "id": "call_1", "type": "function",
					"function": map[string]any{"name": "echo", "arguments": `1}`},
				}},
			}}},
		},
		{
			"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}},
			"usage": map[string]any{
				"prompt_tokens": 10, "completion_tokens": 8, "total_tokens": 18,
				"prompt_tokens_details":     map[string]any{"cached_tokens": 3},
				"completion_tokens_details": map[string]any{"reasoning_tokens": 5},
			},
		},
	})
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL, "test-key")

	var events []StreamEvent
	err := p.Stream(context.Background(), &StreamRequest{
		Model: "gpt-5",
		Config: &RequestConfig{
			Effort:    ReasoningEffortLevelHigh,
			MaxTokens: 1000,
		},
		Messages: []core.Message{
			core.NewSystemMessage("sys"),
			core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
		},
		Tools: []core.ToolSchema{{Name: "echo", Description: "d", Parameters: map[string]any{
			"type":                 "object",
			"properties":           map[string]any{"a": map[string]any{"type": "string"}},
			"required":             []string{"a"},
			"additionalProperties": false,
		}}},
	}, func(e StreamEvent) error {
		events = append(events, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	// ---- 请求体断言：reasoning 与工具翻译真实发出 ----
	if gotBody["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort not sent: %v", gotBody["reasoning_effort"])
	}
	so, ok := gotBody["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Fatalf("stream_options.include_usage not sent: %v", gotBody["stream_options"])
	}
	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) != 2 {
		t.Fatalf("messages not translated: %v", gotBody["messages"])
	}
	if m := msgs[0].(map[string]any); m["role"] != "system" || m["content"] != "sys" {
		t.Fatalf("system message wrong: %v（deepseek 非标准端点恒 system，对齐 pi isNonStandard）", m)
	}
	tools, ok := gotBody["tools"].([]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools not translated: %v", gotBody["tools"])
	}
	if fn := tools[0].(map[string]any)["function"].(map[string]any); fn["name"] != "echo" {
		t.Fatalf("tool function wrong: %v", fn)
	}
	params, ok := tools[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	if !ok || params["type"] != "object" || params["additionalProperties"] != false {
		t.Fatalf("tool parameters object constraints were not transmitted: %v", params)
	}
	required, ok := params["required"].([]any)
	if !ok || len(required) != 1 || required[0] != "a" {
		t.Fatalf("tool required was not transmitted: %v", params["required"])
	}
	// reasoning 模型不传 temperature（库强制校验：推理模型温度固定为 1）
	if _, ok := gotBody["temperature"]; ok {
		t.Fatalf("temperature must be omitted for reasoning model: %v", gotBody["temperature"])
	}

	// ---- 事件序列 ----
	wantTypes := []string{
		"LLMStartEvent",
		"LLMReasoningDeltaEvent",
		"LLMContentDeltaEvent",
		"LLMToolCallStartEvent",
		"LLMToolCallDeltaEvent",
		"LLMToolCallDeltaEvent",
		"LLMEndEvent",
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("want %d events, got %d", len(wantTypes), len(events))
	}
	for i, e := range events {
		got := string(typeOf(e))
		if got != wantTypes[i] {
			t.Fatalf("event[%d]: want %s, got %s", i, wantTypes[i], got)
		}
	}

	// ---- 请求 ID 链路：chunk.ID 填充到 delta 事件与聚合结果 ----
	if end := events[len(events)-1].(LLMEndEvent); end.RequestId != "chatcmpl-1" {
		t.Fatalf("llm_end request id: %q", end.RequestId)
	}
	if r, ok := events[1].(LLMReasoningDeltaEvent); !ok || r.RequestId != "chatcmpl-1" {
		t.Fatalf("reasoning delta request id: %+v", events[1])
	}
	if c, ok := events[2].(LLMContentDeltaEvent); !ok || c.RequestId != "chatcmpl-1" {
		t.Fatalf("content delta request id: %+v", events[2])
	}

	// ---- 聚合断言 ----
	end := events[len(events)-1].(LLMEndEvent)
	if end.Content != "你好" {
		t.Fatalf("content: %q", end.Content)
	}
	if end.Reasoning != "正在思考" {
		t.Fatalf("reasoning: %q", end.Reasoning)
	}
	if len(end.ToolCalls) != 1 {
		t.Fatalf("want 1 tool call, got %+v", end.ToolCalls)
	}
	tc := end.ToolCalls[0]
	if tc.Id != "call_1" || tc.Name != "echo" || tc.Arguments != `{"a":1}` {
		t.Fatalf("tool call not aggregated: %+v", tc)
	}
	if tc.RequestId != "chatcmpl-1" {
		t.Fatalf("tool call request id not filled: %+v", tc)
	}
	if end.FinishReason != core.FinishReasonToolCall {
		t.Fatalf("finish reason: %v", end.FinishReason)
	}
	if end.Usage.Input != 10 || end.Usage.Output != 8 || end.Usage.TotalTokens != 18 {
		t.Fatalf("usage: %+v", end.Usage)
	}
	if end.Usage.CacheRead != 3 {
		t.Fatalf("cache read: %+v", end.Usage)
	}
	if end.Usage.Reasoning != 5 {
		t.Fatalf("reasoning tokens: %+v", end.Usage)
	}
}

// typeOf 取事件类型名，便于断言。
func typeOf(e StreamEvent) string {
	switch e.(type) {
	case LLMStartEvent:
		return "LLMStartEvent"
	case LLMContentDeltaEvent:
		return "LLMContentDeltaEvent"
	case LLMReasoningDeltaEvent:
		return "LLMReasoningDeltaEvent"
	case LLMToolCallStartEvent:
		return "LLMToolCallStartEvent"
	case LLMToolCallDeltaEvent:
		return "LLMToolCallDeltaEvent"
	case LLMEndEvent:
		return "LLMEndEvent"
	}
	return "unknown"
}

// TestOpenAIStreamAbortLatency 客户端中断（ctx 取消）是否立即中断流式响应。
// 复现「模型持续流式输出，用户点停止」：SSE 服务器发出首块后挂起不结束，
// 中断应让 Stream 在毫秒级返回，而非等到服务器写完全部内容（数十秒/永不）。
func TestOpenAIStreamAbortLatency(t *testing.T) {
	started := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// 首块发出（流已建立、客户端可感知），随后挂起模拟慢/长响应
		b, _ := json.Marshal(map[string]any{
			"id": "chatcmpl-abort", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "持续输出"}}},
		})
		_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		close(started)
		// 挂起：若 ctx 取消不能及时中断流，Stream 将阻塞在此直到服务器结束。
		// 用客户端断开信号提前退出，避免拖慢测试。
		<-r.Context().Done()
	}))
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL, "test-key")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	got := make(chan StreamEvent, 64)
	go func() {
		done <- p.Stream(ctx, &StreamRequest{
			Model:  "gpt-5",
			Config: &RequestConfig{MaxTokens: 1000},
		}, func(e StreamEvent) error { got <- e; return nil })
	}()

	// 等流建立（收到首个 content chunk）
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never started")
	}

	cancel() // 客户端中断（Abort 即走 ctx 取消）

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("abort should surface an error, got nil")
		}
		if !errors.Is(err, context.Canceled) && !strings.Contains(err.Error(), "canceled") && !strings.Contains(err.Error(), "context") {
			t.Logf("abort error: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("abort did not interrupt the stream promptly (still blocked after 5s)")
	}
}

// ctxAwareBody 模拟「服务器发出首块后挂起」：Read 首块后阻塞，直到请求 ctx 取消（Abort 即
// 走 ctx 取消）时以 ctx 错误解除阻塞 —— 等价真实「模型持续吐字 / 服务器慢」场景。
type ctxAwareBody struct {
	ctx   context.Context
	first []byte
	sent  bool
}

func (b *ctxAwareBody) Read(p []byte) (int, error) {
	if !b.sent {
		b.sent = true
		return copy(p, b.first), nil
	}
	<-b.ctx.Done() // 挂起：只有 ctx 取消才返回（模拟阻塞的网络读）
	return 0, b.ctx.Err()
}

// hangRoundTripper 注入的 RoundTripper：返回「首块即挂起」的 SSE 响应体。
// 不绑定真实端口（本沙箱禁止 listen），用注入 transport 驱动真实 go-openai 的
// bufio 流式 reader，严格验证 ctx 取消能否立即打断阻塞的 Recv()。
type hangRoundTripper struct {
	started chan struct{}
}

func (rt *hangRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	_, _ = io.ReadAll(req.Body) // 与真实实现一致：先消费请求体
	first := []byte("data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"持续输出\"}}]}\n\n")
	close(rt.started)
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       io.NopCloser(&ctxAwareBody{ctx: req.Context(), first: first}),
	}, nil
}

// TestOpenAIStreamAbortLatencyInjected 严格验证真实 go-openai provider 的流在 ctx 取消
// （Abort 即走 ctx 取消）时是否立即解除阻塞的 Recv()。用注入 RoundTripper（规避沙箱禁止
// listen），body 首块后挂起直到 ctx 取消 —— 与真实「服务器持续输出/挂起」场景等价。
func TestOpenAIStreamAbortLatencyInjected(t *testing.T) {
	rt := &hangRoundTripper{started: make(chan struct{})}
	cfg := openai.DefaultConfig("test-key")
	cfg.HTTPClient = &http.Client{Transport: rt}
	cfg.BaseURL = "http://injected.invalid/v1"
	p := &OpenAIProvider{client: openai.NewClientWithConfig(cfg)}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	var mu sync.Mutex
	afterAbort := 0
	aborted := atomic.Bool{}
	go func() {
		done <- p.Stream(ctx, &StreamRequest{Model: "gpt-5", Config: &RequestConfig{MaxTokens: 100}}, func(e StreamEvent) error {
			if aborted.Load() { // 度量 abort 之后才投递的事件（应为 0）
				mu.Lock()
				afterAbort++
				mu.Unlock()
			}
			return nil
		})
	}()

	select {
	case <-rt.started:
	case <-time.After(5 * time.Second):
		t.Fatal("stream never started")
	}
	time.Sleep(30 * time.Millisecond) // 确认首块已处理、已进入挂起
	aborted.Store(true)
	cancelAt := time.Now()
	cancel()

	select {
	case err := <-done:
		if err == nil {
			t.Fatalf("abort should surface an error, got nil")
		}
		latency := time.Since(cancelAt)
		if latency > 1*time.Second {
			t.Fatalf("real provider stream abort was NOT prompt: %v", latency)
		}
		t.Logf("real go-openai provider stream aborted in %v (err=%v)", latency, err)
	case <-time.After(5 * time.Second):
		t.Fatal("abort did not interrupt the real provider stream promptly (Recv still blocked)")
	}

	mu.Lock()
	defer mu.Unlock()
	if afterAbort > 0 {
		t.Fatalf("post-abort events delivered: %d (abort 后不应再有输出)", afterAbort)
	}
}

func TestOpenAIStreamStop(t *testing.T) {
	srv, gotBody := sseServer(t, []map[string]any{
		{
			"id": "chatcmpl-2", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "ok"}}},
		},
		{
			"id": "chatcmpl-2", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		},
	})
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL, "test-key")
	var events []StreamEvent
	err := p.Stream(context.Background(), &StreamRequest{Model: "gpt-4o", Config: &RequestConfig{Temperature: ptr(0.9)}}, func(e StreamEvent) error {
		events = append(events, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 {
		t.Fatalf("want 3 events, got %d", len(events))
	}
	end := events[len(events)-1].(LLMEndEvent)
	if end.Content != "ok" || end.FinishReason != core.FinishReasonStop {
		t.Fatalf("unexpected end: %+v", end)
	}
	// 普通模型允许 temperature，请求体应传递
	if gotBody["temperature"].(float64) != 0.9 {
		t.Fatalf("temperature wrong: %v", gotBody["temperature"])
	}
}

// TestOpenAIStreamHistoryRoundTrip 验证历史消息完整回传：
// assistant 的 reasoning_content / tool_calls 必须带回去（DeepSeek 多轮工具调用要求，缺失会 400）。
func TestOpenAIStreamHistoryRoundTrip(t *testing.T) {
	srv, gotBody := sseServer(t, []map[string]any{
		{
			"id": "chatcmpl-3", "object": "chat.completion.chunk", "model": "deepseek-chat",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "final"}}},
		},
		{
			"id": "chatcmpl-3", "object": "chat.completion.chunk", "model": "deepseek-chat",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		},
	})
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL, "test-key")
	err := p.Stream(context.Background(), &StreamRequest{
		Model: "deepseek-chat",
		Messages: []core.Message{
			core.NewUserMessage(core.Content{Type: "text", Content: "hi"}),
			core.NewAssistantMessage(nil, "上一步的思考", []core.ToolCall{{Id: "c1", Name: "echo", Arguments: `{"a":1}`}}),
			core.NewToolMessage("c1", "result"),
		},
	}, func(e StreamEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}

	msgs, ok := gotBody["messages"].([]any)
	if !ok || len(msgs) != 3 {
		t.Fatalf("want 3 messages, got %v", gotBody["messages"])
	}

	assistant := msgs[1].(map[string]any)
	if assistant["role"] != "assistant" {
		t.Fatalf("role: %v", assistant["role"])
	}
	if assistant["reasoning_content"] != "上一步的思考" {
		t.Fatalf("reasoning_content not round-tripped: %v", assistant["reasoning_content"])
	}
	tcs, ok := assistant["tool_calls"].([]any)
	if !ok || len(tcs) != 1 {
		t.Fatalf("tool_calls not round-tripped: %v", assistant["tool_calls"])
	}
	tc0 := tcs[0].(map[string]any)
	if tc0["id"] != "c1" {
		t.Fatalf("tool call id: %v", tc0["id"])
	}
	if fn := tc0["function"].(map[string]any); fn["name"] != "echo" || fn["arguments"] != `{"a":1}` {
		t.Fatalf("tool call function: %v", fn)
	}

	toolMsg := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "c1" {
		t.Fatalf("tool message not round-tripped: %v", toolMsg)
	}
}

func TestOpenAICompatMinimalRequest(t *testing.T) {
	var gotPath string
	var gotHeader http.Header
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotHeader = r.Header.Clone()
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(body, &gotBody); err != nil {
			t.Fatal(err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"compat-1\",\"object\":\"chat.completion.chunk\",\"model\":\"gpt-5.4\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := NewOpenAIProviderWithCapabilities(srv.URL, "compat-key", MinimalOpenAICompatCapabilities())
	var got []StreamEvent
	err := p.Stream(context.Background(), &StreamRequest{
		Model:    "gpt-5.4",
		Config:   &RequestConfig{Effort: ReasoningEffortLevelHigh, MaxTokens: 128000},
		Messages: []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})},
		Tools:    []core.ToolSchema{{Name: "must_send", Parameters: map[string]any{"type": "object", "required": []string{"path"}, "additionalProperties": false}}},
	}, func(e StreamEvent) error {
		got = append(got, e)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/v1/chat/completions" {
		t.Fatalf("path = %q, want /v1/chat/completions", gotPath)
	}
	if gotHeader.Get("Authorization") != "Bearer compat-key" {
		t.Fatalf("authorization = %q", gotHeader.Get("Authorization"))
	}
	// minimal 只省略**格式类**字段（max_completion_tokens/stream_options）；
	// reasoning_effort 是语义参数（档位由模型能力消化）→ 照发，否则「用户选了档位
	// 但请求体里什么都没有」（2026-09 决策，见 supportsReasoningEffort 注释）。
	for _, forbidden := range []string{"max_completion_tokens", "stream_options"} {
		if _, ok := gotBody[forbidden]; ok {
			t.Fatalf("minimal request unexpectedly sent %s: %#v", forbidden, gotBody)
		}
	}
	// gpt-5.4 声明档位 [low,medium,high,xhigh] → high 原样（未降档）
	if gotBody["reasoning_effort"] != "high" {
		t.Fatalf("minimal request must still carry effort, got %v: %#v", gotBody["reasoning_effort"], gotBody)
	}
	tools, ok := gotBody["tools"].([]any)
	if !ok || len(tools) != 1 || tools[0].(map[string]any)["function"].(map[string]any)["name"] != "must_send" {
		t.Fatalf("minimal request must retain tools: %#v", gotBody["tools"])
	}
	minimalParams := tools[0].(map[string]any)["function"].(map[string]any)["parameters"].(map[string]any)
	if minimalParams["additionalProperties"] != false {
		t.Fatalf("minimal request lost additionalProperties: %#v", minimalParams)
	}
	if required, ok := minimalParams["required"].([]any); !ok || len(required) != 1 || required[0] != "path" {
		t.Fatalf("minimal request lost required: %#v", minimalParams["required"])
	}
	if gotBody["model"] != "gpt-5.4" || gotBody["stream"] != true {
		t.Fatalf("minimal request missing model/stream: %#v", gotBody)
	}
	if len(got) != 3 || got[len(got)-1].(LLMEndEvent).Content != "ok" {
		t.Fatalf("unexpected events: %#v", got)
	}
}

// TestZhipuProviderChatCompletionsPath 智谱 /api/paas/v4 端点：baseURL 含版本根 /v4（非 /v1），
// 完整能力（Minimal=false）走 SDK chat/completions → 请求落在 /api/paas/v4/chat/completions，
// 而不是默认 /v1 拼出的 /api/paas/v4/v1 错误路径。
func TestZhipuProviderChatCompletionsPath(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"z-1\",\"object\":\"chat.completion.chunk\",\"model\":\"glm-5.2\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	// 等价 NewZhipuProvider 的组装：baseURL 版本根 /v4 + ZhipuOpenAICompatCapabilities
	p := NewOpenAIProviderWithCapabilities(srv.URL+"/api/paas/v4", "z-key", ZhipuOpenAICompatCapabilities())
	if err := p.Stream(context.Background(), &StreamRequest{
		Model:    "glm-5.2",
		Config:   &RequestConfig{Effort: ReasoningEffortLevelHigh, MaxTokens: 8192},
		Messages: []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})},
	}, func(StreamEvent) error { return nil }); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/api/paas/v4/chat/completions" {
		t.Fatalf("zhipu chat completions path = %q, want /api/paas/v4/chat/completions", gotPath)
	}
}

func TestOpenAICompatMinimalHTTPErrorClassification(t *testing.T) {
	for _, tc := range []struct {
		status        int
		wantPermanent bool
		wantRateLimit bool
	}{
		{http.StatusBadRequest, true, false},
		{http.StatusTooManyRequests, false, true},
		{http.StatusServiceUnavailable, false, false},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(`{"code":"UPSTREAM","message":"gateway says no"}`))
			}))
			defer srv.Close()
			err := NewOpenAIProviderWithCapabilities(srv.URL, "key", MinimalOpenAICompatCapabilities()).Stream(context.Background(), &StreamRequest{Model: "gpt-5.4"}, func(StreamEvent) error { return nil })
			if err == nil || IsPermanent(err) != tc.wantPermanent {
				t.Fatalf("err=%v permanent=%v", err, IsPermanent(err))
			}
			var rl *RateLimitError
			if errors.As(err, &rl) != tc.wantRateLimit {
				t.Fatalf("err=%v rate_limit=%v", err, errors.As(err, &rl))
			}
			if !strings.Contains(err.Error(), "gateway says no") {
				t.Fatalf("error lost gateway message: %v", err)
			}
		})
	}
}

func TestOpenAIAPIErrorPermanent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request","type":"invalid_request_error"}}`))
	}))
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL, "test-key")
	err := p.Stream(context.Background(), &StreamRequest{Model: "gpt-4o"}, func(e StreamEvent) error {
		return nil
	})
	if err == nil {
		t.Fatal("want error from 400 response")
	}
	if !IsPermanent(err) {
		t.Fatalf("4xx error should be permanent: %v", err)
	}
}

// TestOpenAIAPIError429Retryable 429 是限流错误：包装为 RateLimitError 且不标记永久（可退避重试）。
func TestOpenAIAPIError429Retryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded","type":"rate_limit_error"}}`))
	}))
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL, "test-key")
	err := p.Stream(context.Background(), &StreamRequest{Model: "gpt-4o"}, func(e StreamEvent) error {
		return nil
	})
	if err == nil {
		t.Fatal("want error from 429 response")
	}
	var rl *RateLimitError
	if !errors.As(err, &rl) {
		t.Fatalf("429 should be RateLimitError: %v", err)
	}
	if rl.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status code: %d", rl.StatusCode)
	}
	if IsPermanent(err) {
		t.Fatal("429 must be retryable, not permanent")
	}
}

// TestOpenAIAPIError408Retryable 瞬时类 4xx（408 请求超时）不标记永久（普通退避档位重试）。
func TestOpenAIAPIError408Retryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestTimeout)
		_, _ = w.Write([]byte(`{"error":{"message":"request timeout","type":"server_error"}}`))
	}))
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL, "test-key")
	err := p.Stream(context.Background(), &StreamRequest{Model: "gpt-4o"}, func(e StreamEvent) error {
		return nil
	})
	if err == nil {
		t.Fatal("want error from 408 response")
	}
	if IsPermanent(err) {
		t.Fatalf("408 must be retryable, not permanent: %v", err)
	}
}

// TestOpenAIAPIError503Retryable 5xx 服务器错误不经永久标记（可重试，普通退避档位）。
func TestOpenAIAPIError503Retryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"service unavailable","type":"server_error"}}`))
	}))
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL, "test-key")
	err := p.Stream(context.Background(), &StreamRequest{Model: "gpt-4o"}, func(e StreamEvent) error {
		return nil
	})
	if err == nil {
		t.Fatal("want error from 503 response")
	}
	if IsPermanent(err) {
		t.Fatalf("5xx must be retryable, not permanent: %v", err)
	}
}

// TestClassifyError 错误分类传导标签：rate_limit / permanent / canceled / generic。
func TestClassifyError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want string
	}{
		{"rate limit", &RateLimitError{StatusCode: 429, Err: errors.New("rl")}, ErrorKindRateLimit},
		{"permanent", MarkPermanent(errors.New("bad request")), ErrorKindPermanent},
		{"canceled", context.Canceled, ErrorKindCanceled},
		{"generic", errors.New("malformed response body"), ErrorKindGeneric},
		{"upstream unavailable text", errors.New("503 Service Unavailable: Endpoint is unavailable"), ErrorKindUpstreamUnavailable},
		{"nil", nil, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ClassifyError(c.err); got != c.want {
				t.Fatalf("ClassifyError: got %q, want %q", got, c.want)
			}
		})
	}
}

// TestOpenAIAPIError429MidStream 流中段 429 错误帧：不经 classifyAPIError，保持可重试（不永久）。
func TestOpenAIAPIError429MidStream(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"error":{"message":"rate limited","type":"rate_limit_error"}}` + "\n\n"))
	}))
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL, "test-key")
	err := p.Stream(context.Background(), &StreamRequest{Model: "gpt-4o"}, func(e StreamEvent) error {
		return nil
	})
	if err == nil {
		t.Fatal("want error from mid-stream 429 frame")
	}
	if IsPermanent(err) {
		t.Fatalf("mid-stream 429 must stay retryable: %v", err)
	}
}

// TestOpenAIStreamMultimodal 验证内容块数组翻译：文本+图片混排 → content 数组 part。
func TestOpenAIStreamMultimodal(t *testing.T) {
	srv, gotBody := sseServer(t, []map[string]any{
		{
			"id": "chatcmpl-4", "object": "chat.completion.chunk", "model": "gpt-4o",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "看懂了"}}},
		},
		{
			"id": "chatcmpl-4", "object": "chat.completion.chunk", "model": "gpt-4o",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}},
		},
	})
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL, "test-key")
	err := p.Stream(context.Background(), &StreamRequest{
		Model: "gpt-4o",
		Messages: []core.Message{
			core.NewUserMessage(
				core.Content{Type: "text", Content: "这张图里有什么？"},
				core.Content{Type: "image", Content: "data:image/png;base64,xxx", MimeType: "image/png"},
			),
		},
	}, func(e StreamEvent) error { return nil })
	if err != nil {
		t.Fatal(err)
	}

	msgs := gotBody["messages"].([]any)
	user := msgs[0].(map[string]any)
	parts, ok := user["content"].([]any)
	if !ok || len(parts) != 2 {
		t.Fatalf("want content array of 2 parts, got %v", user["content"])
	}

	p0 := parts[0].(map[string]any)
	if p0["type"] != "text" || p0["text"] != "这张图里有什么？" {
		t.Fatalf("part[0]: %v", p0)
	}
	p1 := parts[1].(map[string]any)
	if p1["type"] != "image_url" {
		t.Fatalf("part[1] type: %v", p1)
	}
	if img := p1["image_url"].(map[string]any); img["url"] != "data:image/png;base64,xxx" {
		t.Fatalf("part[1] url: %v", img)
	}
}

// 流式块序号（2026-08-10 trace 审计发现修复）：content/reasoning 各自从 0 自增，
// 与厂商 choices 序号解耦（SDK 恒单 choice 恒 0，无信息量）。
func TestOpenAIStreamChunkIndexSequential(t *testing.T) {
	srv, _ := sseServer(t, []map[string]any{
		{"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "r0"}}}},
		{"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "c0"}}}},
		{"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "r1"}}}},
		{"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "c1"}}}},
		{"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "c2"}}}},
		{"id": "chatcmpl-1", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}},
	})
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL, "test-key")
	var contentIdx, reasoningIdx []int64
	err := p.Stream(context.Background(), &StreamRequest{Model: "gpt-5"}, func(e StreamEvent) error {
		switch ev := e.(type) {
		case LLMContentDeltaEvent:
			contentIdx = append(contentIdx, ev.Index)
		case LLMReasoningDeltaEvent:
			reasoningIdx = append(reasoningIdx, ev.Index)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(contentIdx, []int64{0, 1, 2}) {
		t.Errorf("content 块序号应 0,1,2 自增，got %v", contentIdx)
	}
	if !reflect.DeepEqual(reasoningIdx, []int64{0, 1}) {
		t.Errorf("reasoning 块序号应 0,1 自增，got %v", reasoningIdx)
	}
}

// LLMStart 自包含（2026-08-10 trace 审计修复）：延迟到首个 chunk 后发出，
// 带厂商 requestId，Timestamp 保留真实发起时刻。
func TestOpenAIStreamLLMStartCarriesRequestId(t *testing.T) {
	srv, _ := sseServer(t, []map[string]any{
		{"id": "chatcmpl-9", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hi"}}}},
		{"id": "chatcmpl-9", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}},
	})
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL, "test-key")
	var start *LLMStartEvent
	err := p.Stream(context.Background(), &StreamRequest{Model: "gpt-5"}, func(e StreamEvent) error {
		if ev, ok := e.(LLMStartEvent); ok {
			start = &ev
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if start == nil {
		t.Fatal("应收到 LLMStart")
	}
	if start.RequestId != "chatcmpl-9" {
		t.Errorf("LLMStart.RequestId 应携带厂商 id，got %q", start.RequestId)
	}
}

// TestOpenAIStreamSamplingPassthrough 验证采样参数纯透传：显式设置 top_p/temperature 才带上，
// 未设置（nil）不发（厂商默认）。默认值由宿主（bridge）构造 loop 时决定，provider 不内置。
func TestOpenAIStreamSamplingPassthrough(t *testing.T) {
	newSrv := func() (*httptest.Server, map[string]any) {
		return sseServer(t, []map[string]any{
			{"id": "chatcmpl-tp", "object": "chat.completion.chunk", "model": "deepseek-v4-flash",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}},
		})
	}

	// 显式设置 → 透传
	srv, gotBody := newSrv()
	defer srv.Close()
	p := NewOpenAIProvider(srv.URL, "test-key")
	if err := p.Stream(context.Background(), &StreamRequest{Model: "deepseek-v4-flash", Config: &RequestConfig{Temperature: ptr(1.0), TopP: ptr(0.95)}}, func(e StreamEvent) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if got := gotBody["temperature"]; got != float64(1.0) {
		t.Errorf("temperature = %v, want 1.0", got)
	}
	if got := gotBody["top_p"]; got != float64(0.95) {
		t.Errorf("top_p = %v, want 0.95", got)
	}

	// 未设置 → 不发（厂商默认）
	srv2, gotBody2 := newSrv()
	defer srv2.Close()
	p2 := NewOpenAIProvider(srv2.URL, "test-key")
	if err := p2.Stream(context.Background(), &StreamRequest{Model: "deepseek-v4-flash", Config: &RequestConfig{}}, func(e StreamEvent) error {
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := gotBody2["top_p"]; ok {
		t.Errorf("top_p sent = %v, want absent when unset", gotBody2["top_p"])
	}
	if _, ok := gotBody2["temperature"]; ok {
		t.Errorf("temperature sent = %v, want absent when unset", gotBody2["temperature"])
	}
}

// TestThinkingFormatDeepSeek deepseek thinkingFormat：请求体应含 thinking:{type} + reasoning_effort。
func TestThinkingFormatDeepSeek(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	p := NewOpenAIProviderWithDeps(Dependencies{Registry: NewRegistry()}, srv.URL, "sk-test", CustomOpenAICompatCapabilities())
	req := &StreamRequest{
		Provider: "deepseek",
		Model:    "deepseek-v4-flash",
		Config:   &RequestConfig{Effort: ReasoningEffortLevelHigh},
		Messages: []core.Message{{Role: core.User, Content: []core.Content{{Type: "text", Content: "hi"}}}},
	}
	_ = p.Stream(context.Background(), req, func(StreamEvent) error { return nil })

	if got == nil {
		t.Fatal("无请求体")
	}
	th, ok := got["thinking"].(map[string]any)
	if !ok || th["type"] != "enabled" {
		t.Fatalf("thinking = %v, want {type:enabled}", got["thinking"])
	}
	if got["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v, want high", got["reasoning_effort"])
	}
}

// TestThinkingFormatQwen qwen thinkingFormat：enable_thinking 布尔。
func TestThinkingFormatQwen(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	// 手工构造带 qwen thinkingFormat 的注册表条目
	reg := NewRegistry()
	reg.Override(ModelInfo{Provider: "custom", ID: "qwen3", ContextWindow: 1000000, MaxTokens: 65536,
		Reasoning: true, ThinkingFormat: "qwen", ThinkingSupportedEfforts: []ReasoningEffortLevel{ReasoningEffortLevelLow, ReasoningEffortLevelHigh}})
	p2 := NewOpenAIProviderWithDeps(Dependencies{Registry: reg}, srv.URL, "sk-test", CustomOpenAICompatCapabilities())
	req := &StreamRequest{
		Provider: "custom",
		Model:    "qwen3",
		Config:   &RequestConfig{Effort: ReasoningEffortLevelLow},
		Messages: []core.Message{{Role: core.User, Content: []core.Content{{Type: "text", Content: "hi"}}}},
	}
	_ = p2.Stream(context.Background(), req, func(StreamEvent) error { return nil })

	if got == nil {
		t.Fatal("无请求体")
	}
	if got["enable_thinking"] != true {
		t.Fatalf("enable_thinking = %v, want true", got["enable_thinking"])
	}
	if got["reasoning_effort"] != "low" {
		t.Fatalf("reasoning_effort = %v, want low", got["reasoning_effort"])
	}
}

// TestDeveloperRoleOpenAI openai 官方端点推理模型 → system 转 developer（对齐 pi
// supportsDeveloperRole：!isNonStandard && !isOpenRouter）；deepseek 等非标准恒 system。
func TestDeveloperRoleOpenAI(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &got)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer srv.Close()

	reg := NewRegistry()
	// openai 官方推理模型（gpt-5）：developer role
	reg.Override(ModelInfo{Provider: "openai", ID: "gpt-5", ContextWindow: 400000, MaxTokens: 128000,
		Reasoning: true, Protocol: ProtocolResponses})
	p := NewOpenAIProviderWithDeps(Dependencies{Registry: reg}, srv.URL, "sk-test", CustomOpenAICompatCapabilities())
	req := &StreamRequest{
		Provider: "openai", Model: "gpt-5",
		Messages: []core.Message{core.NewSystemMessage("sys"), core.NewUserMessage(core.Content{Type: "text", Content: "hi"})},
	}
	_ = p.Stream(context.Background(), req, func(StreamEvent) error { return nil })
	msgs := got["messages"].([]any)
	if m := msgs[0].(map[string]any); m["role"] != "developer" {
		t.Fatalf("openai gpt-5 system 应转 developer, got %v", m["role"])
	}

	// deepseek：恒 system
	got = nil
	req2 := &StreamRequest{
		Provider: "deepseek", Model: "deepseek-v4-flash",
		Messages: []core.Message{core.NewSystemMessage("sys"), core.NewUserMessage(core.Content{Type: "text", Content: "hi"})},
	}
	_ = p.Stream(context.Background(), req2, func(StreamEvent) error { return nil })
	msgs2 := got["messages"].([]any)
	if m := msgs2[0].(map[string]any); m["role"] != "system" {
		t.Fatalf("deepseek system 应保持 system, got %v", m["role"])
	}
}

// TestZaiToolStream zai thinkingFormat + ZaiToolStream：带工具时请求体应含
// tool_stream:true（对齐 pi zaiToolStream）；未标 ZaiToolStream 时不发该字段。
func TestZaiToolStream(t *testing.T) {
	newSrv := func() (*httptest.Server, map[string]any) {
		return sseServer(t, []map[string]any{
			{"id": "chatcmpl-zai", "object": "chat.completion.chunk", "model": "glm-4.6",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}},
		})
	}

	// ZaiToolStream=true + 带工具 → tool_stream:true
	srv, gotBody := newSrv()
	defer srv.Close()
	reg := NewRegistry()
	reg.Override(ModelInfo{Provider: "zai", ID: "glm-4.6", ContextWindow: 200000, MaxTokens: 65536,
		Reasoning: true, ThinkingFormat: "zai", ZaiToolStream: ptr(true)})
	p := NewOpenAIProviderWithDeps(Dependencies{Registry: reg}, srv.URL, "sk-test", CustomOpenAICompatCapabilities())
	req := &StreamRequest{
		Provider: "zai",
		Model:    "glm-4.6",
		Config:   &RequestConfig{Effort: ReasoningEffortLevelHigh},
		Messages: []core.Message{{Role: core.User, Content: []core.Content{{Type: "text", Content: "hi"}}}},
		Tools: []core.ToolSchema{{Name: "echo", Description: "d", Parameters: map[string]any{
			"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}},
		}}},
	}
	_ = p.Stream(context.Background(), req, func(StreamEvent) error { return nil })
	if gotBody["tool_stream"] != true {
		t.Fatalf("tool_stream = %v, want true（ZaiToolStream + 带工具）", gotBody["tool_stream"])
	}
	if th, ok := gotBody["thinking"].(map[string]any); !ok || th["type"] != "enabled" {
		t.Fatalf("thinking = %v, want {type:enabled}", gotBody["thinking"])
	}

	// ZaiToolStream=true 但无工具 → 不发 tool_stream
	srv1b, gotBody1b := newSrv()
	defer srv1b.Close()
	req2 := &StreamRequest{
		Provider: "zai",
		Model:    "glm-4.6",
		Config:   &RequestConfig{Effort: ReasoningEffortLevelHigh},
		Messages: []core.Message{{Role: core.User, Content: []core.Content{{Type: "text", Content: "hi"}}}},
	}
	_ = p.Stream(context.Background(), req2, func(StreamEvent) error { return nil })
	if _, ok := gotBody1b["tool_stream"]; ok {
		t.Fatalf("tool_stream = %v, want absent（无工具）", gotBody1b["tool_stream"])
	}

	// ZaiToolStream 未标（nil）+ 带工具 → 不发 tool_stream
	srv2, gotBody3 := newSrv()
	defer srv2.Close()
	reg2 := NewRegistry()
	reg2.Override(ModelInfo{Provider: "zai", ID: "glm-4.6", ContextWindow: 200000, MaxTokens: 65536,
		Reasoning: true, ThinkingFormat: "zai"})
	p2 := NewOpenAIProviderWithDeps(Dependencies{Registry: reg2}, srv2.URL, "sk-test", CustomOpenAICompatCapabilities())
	req3 := &StreamRequest{
		Provider: "zai",
		Model:    "glm-4.6",
		Config:   &RequestConfig{Effort: ReasoningEffortLevelHigh},
		Messages: []core.Message{{Role: core.User, Content: []core.Content{{Type: "text", Content: "hi"}}}},
		Tools: []core.ToolSchema{{Name: "echo", Description: "d", Parameters: map[string]any{
			"type": "object", "properties": map[string]any{"a": map[string]any{"type": "string"}},
		}}},
	}
	_ = p2.Stream(context.Background(), req3, func(StreamEvent) error { return nil })
	if _, ok := gotBody3["tool_stream"]; ok {
		t.Fatalf("tool_stream = %v, want absent（ZaiToolStream 未标）", gotBody3["tool_stream"])
	}
}

// TestBasetenChatTemplateArgs baseten thinkingFormat + ChatTemplateArgs：
// 模板变量 thinking.enabled 按思考开关注入 chat_template_args（true/false）。
func TestBasetenChatTemplateArgs(t *testing.T) {
	newSrv := func() (*httptest.Server, map[string]any) {
		return sseServer(t, []map[string]any{
			{"id": "chatcmpl-bs", "object": "chat.completion.chunk", "model": "deepseek-v3.2",
				"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}},
		})
	}

	// 思考开（Effort=high）→ enable_thinking:true
	srv, gotBody := newSrv()
	defer srv.Close()
	reg := NewRegistry()
	reg.Override(ModelInfo{Provider: "baseten", ID: "deepseek-v3.2", ContextWindow: 128000, MaxTokens: 65536,
		Reasoning: true, ThinkingFormat: "baseten",
		ChatTemplateArgs: map[string]string{"enable_thinking": "thinking.enabled"}})
	p := NewOpenAIProviderWithDeps(Dependencies{Registry: reg}, srv.URL, "sk-test", CustomOpenAICompatCapabilities())
	req := &StreamRequest{
		Provider: "baseten",
		Model:    "deepseek-v3.2",
		Config:   &RequestConfig{Effort: ReasoningEffortLevelHigh},
		Messages: []core.Message{{Role: core.User, Content: []core.Content{{Type: "text", Content: "hi"}}}},
	}
	_ = p.Stream(context.Background(), req, func(StreamEvent) error { return nil })
	args, ok := gotBody["chat_template_args"].(map[string]any)
	if !ok {
		t.Fatalf("chat_template_args = %v, want map", gotBody["chat_template_args"])
	}
	if args["enable_thinking"] != true {
		t.Fatalf("enable_thinking = %v, want true（思考开）", args["enable_thinking"])
	}

	// 思考关（Effort=none）→ enable_thinking:false
	srv2, gotBody2 := newSrv()
	defer srv2.Close()
	p2 := NewOpenAIProviderWithDeps(Dependencies{Registry: reg}, srv2.URL, "sk-test", CustomOpenAICompatCapabilities())
	req2 := &StreamRequest{
		Provider: "baseten",
		Model:    "deepseek-v3.2",
		Config:   &RequestConfig{Effort: ReasoningEffortLevelNone},
		Messages: []core.Message{{Role: core.User, Content: []core.Content{{Type: "text", Content: "hi"}}}},
	}
	_ = p2.Stream(context.Background(), req2, func(StreamEvent) error { return nil })
	args2, ok := gotBody2["chat_template_args"].(map[string]any)
	if !ok {
		t.Fatalf("chat_template_args = %v, want map（思考关）", gotBody2["chat_template_args"])
	}
	if args2["enable_thinking"] != false {
		t.Fatalf("enable_thinking = %v, want false（思考关）", args2["enable_thinking"])
	}
}

// TestThinkingAsTextAndReasoningContent S2/S3 修复验证：
//   - requiresThinkingAsText：thinking 转纯文本拼进 Content（sanitize 后）
//   - requiresReasoningContent + thinking-as-text 同时开：reasoning_content 字段仍存在（空格占位）
//   - 带图片块时正文不丢失（S3）
func TestThinkingAsTextAndReasoningContent(t *testing.T) {
	m := core.Message{
		Role:      core.Assistant,
		Reasoning: "思考中",
		Content: []core.Content{
			{Type: core.ContentTypeText, Content: "正文"},
			{Type: core.ContentTypeImage, Content: "data:image/png;base64,AAA", MimeType: "image/png"},
		},
	}
	// 仅 thinking-as-text：thinking + 正文（含图片块，正文不丢）
	msgs, err := translateMessages([]core.Message{m}, true, false, false, false, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 1 {
		t.Fatalf("msgs = %d, want 1", len(msgs))
	}
	got := msgs[0].Content
	if !strings.Contains(got, "思考中") || !strings.Contains(got, "正文") {
		t.Fatalf("thinking-as-text 应含 thinking+正文: %q", got)
	}

	// thinking-as-text + requiresReasoningContent：reasoning_content 字段仍存在
	m2 := core.Message{Role: core.Assistant, Reasoning: "思考中", Content: []core.Content{{Type: core.ContentTypeText, Content: "正文"}}}
	msgs2, err := translateMessages([]core.Message{m2}, true, false, true, false, false, true)
	if err != nil {
		t.Fatal(err)
	}
	if msgs2[0].ReasoningContent != " " {
		t.Fatalf("requiresReasoningContent 占位缺失: %q", msgs2[0].ReasoningContent)
	}
	if !strings.Contains(msgs2[0].Content, "思考中") {
		t.Fatalf("thinking 未转文本: %q", msgs2[0].Content)
	}

	// requiresReasoningContent + 无 thinking：空格占位（回归验证）
	m3 := core.Message{Role: core.Assistant, Content: []core.Content{{Type: core.ContentTypeText, Content: "hi"}}}
	msgs3, err := translateMessages([]core.Message{m3}, true, false, true, false, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if msgs3[0].ReasoningContent != " " {
		t.Fatalf("无 thinking 时占位缺失: %q", msgs3[0].ReasoningContent)
	}
}

// TestOpenAIUsageDetailsPassthrough 明细透传（2026-09-23 补齐维度）：上游 usage 里的
// 音频 / 预测输出 token 此前被整个丢弃 → 现在落到 Usage.Details（只透传**真的报了**的维度）。
// 约定：纯文本轮 Details == nil（不给「0 个音频 token」的错觉，也不占展示位）。
func TestOpenAIUsageDetailsPassthrough(t *testing.T) {
	srv, _ := sseServer(t, []map[string]any{
		{
			"id": "chatcmpl-d", "object": "chat.completion.chunk", "model": "gpt-4o-audio-preview",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hi"}, "finish_reason": "stop"}},
			"usage": map[string]any{
				"prompt_tokens": 100, "completion_tokens": 40, "total_tokens": 140,
				"prompt_tokens_details": map[string]any{"cached_tokens": 20, "audio_tokens": 15},
				"completion_tokens_details": map[string]any{
					"reasoning_tokens": 5, "audio_tokens": 7,
					"accepted_prediction_tokens": 11, "rejected_prediction_tokens": 2,
				},
			},
		},
	})
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL, "test-key")
	var end LLMEndEvent
	err := p.Stream(context.Background(), &StreamRequest{
		Model:    "gpt-4o-audio-preview",
		Config:   &RequestConfig{},
		Messages: []core.Message{core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "hi"})},
	}, func(e StreamEvent) error {
		if ev, ok := e.(LLMEndEvent); ok {
			end = ev
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Stream err = %v", err)
	}
	d := end.Usage.Details
	if d == nil {
		t.Fatal("上游报了明细 → Details 不应为 nil")
	}
	if d.InputAudio != 15 || d.OutputAudio != 7 {
		t.Fatalf("音频明细 = in %d / out %d, want 15/7", d.InputAudio, d.OutputAudio)
	}
	if d.PredictionAccepted != 11 || d.PredictionRejected != 2 {
		t.Fatalf("预测明细 = accepted %d / rejected %d, want 11/2", d.PredictionAccepted, d.PredictionRejected)
	}
	if d.InputImage != 0 {
		t.Fatalf("图片 token 该 SDK 不上报 → 应恒 0（不伪造），got %d", d.InputImage)
	}
	// 缓存/思考维度不受影响
	if end.Usage.CacheRead != 20 || end.Usage.Reasoning != 5 {
		t.Fatalf("cache/reasoning = %d/%d, want 20/5", end.Usage.CacheRead, end.Usage.Reasoning)
	}
	// 主口径不变：TotalTokens = Input + Output（明细是附加维度，不改主计数）
	if end.Usage.Input != 100 || end.Usage.Output != 40 || end.Usage.TotalTokens != 140 {
		t.Fatalf("主计数被明细影响: in %d out %d total %d", end.Usage.Input, end.Usage.Output, end.Usage.TotalTokens)
	}
}

// TestOpenAIUsageDetailsNilForTextOnly 纯文本轮（上游只回 cached/reasoning）→ Details 为 nil。
func TestOpenAIUsageDetailsNilForTextOnly(t *testing.T) {
	srv, _ := sseServer(t, []map[string]any{
		{
			"id": "chatcmpl-t", "object": "chat.completion.chunk", "model": "gpt-5",
			"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "hi"}, "finish_reason": "stop"}},
			"usage": map[string]any{
				"prompt_tokens": 10, "completion_tokens": 8, "total_tokens": 18,
				"prompt_tokens_details":     map[string]any{"cached_tokens": 3},
				"completion_tokens_details": map[string]any{"reasoning_tokens": 5},
			},
		},
	})
	defer srv.Close()

	p := NewOpenAIProvider(srv.URL, "test-key")
	var end LLMEndEvent
	if err := p.Stream(context.Background(), &StreamRequest{
		Model:    "gpt-5",
		Config:   &RequestConfig{},
		Messages: []core.Message{core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "hi"})},
	}, func(e StreamEvent) error {
		if ev, ok := e.(LLMEndEvent); ok {
			end = ev
		}
		return nil
	}); err != nil {
		t.Fatalf("Stream err = %v", err)
	}
	if end.Usage.Details != nil {
		t.Fatalf("纯文本轮不应挂 Details: %+v", end.Usage.Details)
	}
}
