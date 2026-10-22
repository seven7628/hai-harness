package provider

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// 重试提示（2026-10，对齐 pi utils/provider-retry.ts:23-67）：
// 上游在**响应头**里明确告诉你「什么时候再来」（`retry-after` / `retry-after-ms`）和
// 「这次要不要重试」（`x-should-retry`）。我们此前只有自己的指数退避 —— 429/503 时
// 按自己的节奏撞上去，既可能太早（继续被限）也可能太晚（白等）。
//
// 为什么放在**传输层**：两个发送通道（SDK 通道 / minimal 自建通道）唯一共同的交汇处
// 是 http.RoundTripper；SDK 的 APIError 不带响应头（go-openai 的 APIError 只有状态码与
// 正文），拿不到就得改 SDK 或走字符串解析。这里用请求 ctx 里的槽位把 header 带回来，
// provider.Stream 在错误返回前挂到 error 上，agents 的重试层用 `RetryHintOf` 取。
//
// 只做「建议」不做「决策」：是否重试仍由 agents 层定（超长等待直接快速失败，见
// agents.MaxRetryAfter）。无提示 = 行为完全不变。

// RetryHint 上游给出的重试提示。零值 = 没有提示。
type RetryHint struct {
	// After Retry-After / Retry-After-Ms 解析出的等待时长。按 RFC 也支持 HTTP-date。
	After    time.Duration
	HasAfter bool
	// ShouldRetry x-should-retry（true/false）；nil = 未给。
	ShouldRetry *bool
}

// IsZero 无任何提示。
func (h RetryHint) IsZero() bool { return !h.HasAfter && h.ShouldRetry == nil }

// ParseRetryHint 从响应头解析重试提示；没有任何相关头时 ok=false。
//
// `retry-after-ms` 优先（毫秒精度，Anthropic 等用），其次 `retry-after`（秒数或 HTTP-date）。
// 解析失败/非正值一律忽略（交给本地退避），不当成「立即重试」。
func ParseRetryHint(h http.Header) (RetryHint, bool) {
	if h == nil {
		return RetryHint{}, false
	}
	var hint RetryHint
	if v := strings.TrimSpace(h.Get("retry-after-ms")); v != "" {
		if ms, err := strconv.ParseFloat(v, 64); err == nil && ms > 0 {
			hint.After = time.Duration(ms * float64(time.Millisecond))
			hint.HasAfter = true
		}
	}
	if !hint.HasAfter {
		if v := strings.TrimSpace(h.Get("retry-after")); v != "" {
			if secs, err := strconv.ParseFloat(v, 64); err == nil {
				if secs > 0 {
					hint.After = time.Duration(secs * float64(time.Second))
					hint.HasAfter = true
				}
			} else if t, err := http.ParseTime(v); err == nil {
				if d := time.Until(t); d > 0 {
					hint.After = d
					hint.HasAfter = true
				}
			}
		}
	}
	if v := strings.TrimSpace(h.Get("x-should-retry")); v != "" {
		switch strings.ToLower(v) {
		case "true", "1", "yes":
			b := true
			hint.ShouldRetry = &b
		case "false", "0", "no":
			b := false
			hint.ShouldRetry = &b
		}
	}
	return hint, !hint.IsZero()
}

// retryHintSlot 请求级槽位：传输层写、Stream 读。单请求单 goroutine 使用，
// 仍加锁是因为 RoundTripper 可能被同一 ctx 的并发请求复用（防御性）。
type retryHintSlot struct {
	mu   sync.Mutex
	hint RetryHint
	ok   bool
}

func (s *retryHintSlot) set(h RetryHint) {
	s.mu.Lock()
	s.hint, s.ok = h, true
	s.mu.Unlock()
}

// take 读出并清空（一次尝试的提示不带到下一次）。
func (s *retryHintSlot) take() (RetryHint, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hint, s.ok
	s.hint, s.ok = RetryHint{}, false
	return h, ok
}

type retryHintCtxKey struct{}

// withRetryHintSlot 把槽位放进 ctx（provider 内部使用；子包可复用同一约定）。
func withRetryHintSlot(ctx context.Context) (context.Context, *retryHintSlot) {
	slot := &retryHintSlot{}
	return context.WithValue(ctx, retryHintCtxKey{}, slot), slot
}

func retryHintSlotFromContext(ctx context.Context) *retryHintSlot {
	slot, _ := ctx.Value(retryHintCtxKey{}).(*retryHintSlot)
	return slot
}

// retryHintTransport 从响应头捕获重试提示写进 ctx 槽位；其余行为完全透传。
type retryHintTransport struct{ base http.RoundTripper }

// Unwrap 暴露被包装的 transport（BaseTransport 据此穿透）。
func (t *retryHintTransport) Unwrap() http.RoundTripper { return t.base }

func (t *retryHintTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	base := t.base
	if base == nil {
		base = http.DefaultTransport
	}
	resp, err := base.RoundTrip(req)
	if err != nil || resp == nil {
		return resp, err
	}
	if slot := retryHintSlotFromContext(req.Context()); slot != nil {
		if hint, ok := ParseRetryHint(resp.Header); ok {
			slot.set(hint)
		}
	}
	return resp, nil
}

// WrapRetryHintClient 就地把客户端包上重试提示捕获（保留 Transport / Timeout / Jar）。
// nil 进 nil 出；已包过则原样返回（幂等）。
func WrapRetryHintClient(c *http.Client) *http.Client {
	if c == nil {
		return nil
	}
	if _, done := c.Transport.(*retryHintTransport); done {
		return c
	}
	base := c.Transport
	if base == nil {
		base = http.DefaultTransport
	}
	out := *c
	out.Transport = &retryHintTransport{base: base}
	return &out
}

// retryHintError 把提示挂在错误链上（Unwrap 保留原错误的分类/Is/As 语义）。
type retryHintError struct {
	err  error
	hint RetryHint
}

func (e *retryHintError) Error() string { return e.err.Error() }
func (e *retryHintError) Unwrap() error { return e.err }

// WithRetryHint 把提示附到错误上（hint 为空 / err 为 nil 时原样返回）。
func WithRetryHint(err error, hint RetryHint) error {
	if err == nil || hint.IsZero() {
		return err
	}
	return &retryHintError{err: err, hint: hint}
}

// RetryHintOf 从错误链里取重试提示（没有则 ok=false）。
func RetryHintOf(err error) (RetryHint, bool) {
	var he *retryHintError
	if errors.As(err, &he) {
		return he.hint, true
	}
	return RetryHint{}, false
}

// attachRetryHint 把本次尝试槽位里的提示挂到错误上（无提示原样返回）。
func attachRetryHint(slot *retryHintSlot, err error) error {
	if slot == nil || err == nil {
		return err
	}
	if hint, ok := slot.take(); ok {
		return WithRetryHint(err, hint)
	}
	return err
}
