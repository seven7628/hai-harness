package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
)

// rewriteTransport 把请求改写到本地 httptest（URL 保持 opencode.ai —— 生产路径据此
// 判定为 OpenCode Go 网关），用于在不访问真实网关的前提下验证协议头真的发出。
// inner 是保存下来的真实 transport（不能回调 http.DefaultTransport —— 它已被本类型替换）。
type rewriteTransport struct {
	target string
	inner  http.RoundTripper
}

func (r *rewriteTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.URL.Scheme = "http"
	clone.URL.Host = strings.TrimPrefix(r.target, "http://")
	clone.Host = clone.URL.Host
	return r.inner.RoundTrip(clone)
}

// TestOpenCodeGatewayEndToEndSessionHeader 端到端回归（真实装配路径）：用户报告
// OpenCode Go 返回 400 "Request is missing x-opencode-session and cannot be routed
// efficiently"。官网要求「Send a stable session ID in x-opencode-session for each
// conversation」，缺失即拒。
//
// 覆盖 providerConfig → newProvider（llmHTTPClient 首包超时 + compat + registry）
// → Stream 的完整链路，断言：
//   - x-opencode-session = 会话稳定 id（bridge 下发的 SessionID）
//   - UA 为自有标识（官网要求，而非 Go-http-client / SDK 通用库名）
//
// 通过替换 http.DefaultTransport 把 opencode.ai 流量引到本地 server ——
// newProvider 内部构造 client（llmHTTPClient）不走外部注入点，只有全局 transport
// 能拦到，这也正是生产链路。
func TestOpenCodeGatewayEndToEndSessionHeader(t *testing.T) {
	var mu sync.Mutex
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
	}))
	defer srv.Close()

	orig := http.DefaultTransport
	http.DefaultTransport = &rewriteTransport{target: srv.URL, inner: orig}
	defer func() { http.DefaultTransport = orig }()

	pc := &providerConfig{
		active:    "opencode",
		baseURL:   map[string]string{"opencode": "https://opencode.ai/zen/go/v1"},
		apiKey:    map[string]string{"opencode": "sk-test"},
		models:    map[string]map[string]int64{"opencode": {"glm-5.3": 1000000}},
		maxTokens: map[string]map[string]int64{"opencode": {"glm-5.3": 131072}},
		compat:    map[string]provider.OpenAICompatCapabilities{"opencode": provider.FullOpenAICompatCapabilities()},
		registry:  provider.NewRegistry(),
		protocol:  map[string]string{"opencode": protocolChatCompletions},
	}

	p := pc.newProvider("opencode")
	if err := p.Stream(context.Background(), &provider.StreamRequest{
		Model:     "glm-5.3",
		Provider:  "opencode",
		SessionID: "s-test-0001",
		Messages:  []core.Message{core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "hi"})},
	}, func(provider.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("Stream err = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if v := got.Get(provider.OpenCodeSessionHeader); v != "s-test-0001" {
		t.Fatalf("%s = %q, want 会话稳定 id（缺此头网关 400：\"Request is missing %s\"）",
			provider.OpenCodeSessionHeader, v, provider.OpenCodeSessionHeader)
	}
	if v := got.Get("User-Agent"); v != provider.OpenCodeUserAgent {
		t.Fatalf("User-Agent = %q, want %q（官网要求自报身份，而非通用库名）",
			v, provider.OpenCodeUserAgent)
	}
}

// TestNonGatewayEndToEndNoSessionHeader 非网关 provider（DeepSeek 直连）不受影响：
// 不注入网关协议头（改动范围仅限 opencode.ai 端点）。
func TestNonGatewayEndToEndNoSessionHeader(t *testing.T) {
	var mu sync.Mutex
	var got http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = r.Header.Clone()
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"x\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n"))
	}))
	defer srv.Close()

	// deepseek 端点直连 httptest（无需改写 host）
	pc := &providerConfig{
		active:    "deepseek",
		baseURL:   map[string]string{"deepseek": srv.URL},
		apiKey:    map[string]string{"deepseek": "sk-test"},
		models:    map[string]map[string]int64{"deepseek": {"deepseek-v4-flash": 1000000}},
		maxTokens: map[string]map[string]int64{"deepseek": {"deepseek-v4-flash": 384000}},
		compat:    map[string]provider.OpenAICompatCapabilities{"deepseek": provider.FullOpenAICompatCapabilities()},
		registry:  provider.NewRegistry(),
		protocol:  map[string]string{"deepseek": protocolChatCompletions},
	}

	p := pc.newProvider("deepseek")
	if err := p.Stream(context.Background(), &provider.StreamRequest{
		Model:     "deepseek-v4-flash",
		Provider:  "deepseek",
		SessionID: "s-1",
		Messages:  []core.Message{core.NewUserMessage(core.Content{Type: core.ContentTypeText, Content: "hi"})},
	}, func(provider.StreamEvent) error { return nil }); err != nil {
		t.Fatalf("Stream err = %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if v := got.Get(provider.OpenCodeSessionHeader); v != "" {
		t.Fatalf("非网关端点不应带 %s: %q", provider.OpenCodeSessionHeader, v)
	}
}
