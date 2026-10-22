package provider

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
)

func TestParseRetryHint(t *testing.T) {
	cases := []struct {
		name   string
		header map[string]string
		want   RetryHint
		ok     bool
	}{
		{"retry-after-ms 优先", map[string]string{"retry-after-ms": "1500", "retry-after": "99"}, RetryHint{After: 1500 * time.Millisecond, HasAfter: true}, true},
		{"retry-after 秒", map[string]string{"retry-after": "3"}, RetryHint{After: 3 * time.Second, HasAfter: true}, true},
		{"非正值忽略", map[string]string{"retry-after": "0"}, RetryHint{}, false},
		{"解析失败忽略", map[string]string{"retry-after": "soon"}, RetryHint{}, false},
		{"x-should-retry false", map[string]string{"x-should-retry": "false"}, func() RetryHint {
			b := false
			return RetryHint{ShouldRetry: &b}
		}(), true},
		{"x-should-retry true", map[string]string{"x-should-retry": "TRUE"}, func() RetryHint {
			b := true
			return RetryHint{ShouldRetry: &b}
		}(), true},
		{"无关头", map[string]string{"x-request-id": "abc"}, RetryHint{}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h := http.Header{}
			for k, v := range c.header {
				h.Set(k, v)
			}
			got, ok := ParseRetryHint(h)
			if ok != c.ok {
				t.Fatalf("ok = %v, want %v (hint=%+v)", ok, c.ok, got)
			}
			if !c.ok {
				return
			}
			if got.HasAfter != c.want.HasAfter || got.After != c.want.After {
				t.Fatalf("After = %v/%v, want %v/%v", got.After, got.HasAfter, c.want.After, c.want.HasAfter)
			}
			if (got.ShouldRetry == nil) != (c.want.ShouldRetry == nil) {
				t.Fatalf("ShouldRetry 存在性不符: %+v vs %+v", got, c.want)
			}
			if got.ShouldRetry != nil && *got.ShouldRetry != *c.want.ShouldRetry {
				t.Fatalf("ShouldRetry = %v, want %v", *got.ShouldRetry, *c.want.ShouldRetry)
			}
		})
	}
}

// 端到端：429 + Retry-After 必须随错误带回（两个发送通道都要覆盖 —— SDK 通道的
// APIError 不带响应头，只能靠传输层捕获）。
func TestRetryHintFromResponseHeaders(t *testing.T) {
	server := func(status int, headers map[string]string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			for k, v := range headers {
				w.Header().Set(k, v)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit"}}`))
		}))
	}
	req := func() *StreamRequest {
		return &StreamRequest{
			Model:    "gpt-5",
			Messages: []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})},
		}
	}
	run := func(t *testing.T, p *OpenAIProvider) error {
		t.Helper()
		return p.Stream(context.Background(), req(), func(StreamEvent) error { return nil })
	}

	t.Run("SDK 通道", func(t *testing.T) {
		srv := server(429, map[string]string{"retry-after": "2"})
		defer srv.Close()
		p := NewOpenAIProvider(srv.URL, "k") // 全能力 → SDK 通道
		err := run(t, p)
		if err == nil {
			t.Fatal("429 应报错")
		}
		var rl *RateLimitError
		if !errors.As(err, &rl) {
			t.Fatalf("应是 RateLimitError，got %T: %v", err, err)
		}
		hint, ok := RetryHintOf(err)
		if !ok || !hint.HasAfter || hint.After != 2*time.Second {
			t.Fatalf("重试提示缺失: ok=%v hint=%+v", ok, hint)
		}
	})

	t.Run("minimal 通道", func(t *testing.T) {
		srv := server(429, map[string]string{"retry-after-ms": "2500", "x-should-retry": "false"})
		defer srv.Close()
		p := NewOpenAIProviderWithCapabilities(srv.URL, "k", MinimalOpenAICompatCapabilities())
		err := run(t, p)
		if err == nil {
			t.Fatal("429 应报错")
		}
		hint, ok := RetryHintOf(err)
		if !ok {
			t.Fatal("重试提示缺失")
		}
		if hint.After != 2500*time.Millisecond {
			t.Fatalf("After = %v, want 2.5s", hint.After)
		}
		if hint.ShouldRetry == nil || *hint.ShouldRetry {
			t.Fatalf("x-should-retry=false 未解析: %+v", hint)
		}
	})

	t.Run("无提示头 → 无 hint（行为不变）", func(t *testing.T) {
		srv := server(500, nil)
		defer srv.Close()
		p := NewOpenAIProvider(srv.URL, "k")
		err := run(t, p)
		if err == nil {
			t.Fatal("500 应报错")
		}
		if _, ok := RetryHintOf(err); ok {
			t.Fatal("没有提示头时不应产生 hint")
		}
	})
}
