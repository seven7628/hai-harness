package agents

import (
	"errors"
	"math/rand"
	"time"

	"github.com/seven7628/hai-harness/provider"
)

// Backoff 计算第 attempt 次尝试失败后的等待时长（attempt 1-based，即刚失败的这一次）；
// 返回 0 = 立即重试。lastErr 供按错误类型区分档位（如限流错误 429 用更宽的退避）。
type Backoff func(attempt int, lastErr error) time.Duration

// WithRetryBackoff 自定义重试退避策略（覆盖默认的指数退避 + 抖动）。
func WithRetryBackoff(f Backoff) Option {
	return func(c *Config) { c.Backoff = f }
}

// FixedBackoff 固定退避：每次重试前固定等待 d（2026-09 决策：重试「快速重试」
// 而非指数退避——指数退避 10 次最坏等 ~243s，用户长时间无反馈体验差）。
// 配合「重试 10 次 × 首包 60s」：最坏总等待 = 10×60s + 10×1s ≈ 610s。
func FixedBackoff(d time.Duration) Backoff {
	return func(attempt int, lastErr error) time.Duration {
		return d
	}
}

// DefaultBackoff 默认退避：指数退避 + 等额抖动（AWS 风格）——
//
//	temp  = min(cap, base × 2^(attempt-1))
//	sleep = temp/2 + rand(0, temp/2)   // 非零、有界、单调增长
//
// 档位：普通可重试错误（5xx/超时/网络/无 LLMEnd）base 200ms、cap 5s；
// 限流错误（provider.RateLimitError）base 1s、cap 60s。
// MaxRetries=10 时最坏等待 ≈ 243s（1+2+4+8+16+32+60×3），可按需调 WithMaxRetries/WithRetryBackoff。
func DefaultBackoff(attempt int, err error) time.Duration {
	var base, capd time.Duration
	var rl *provider.RateLimitError
	if errors.As(err, &rl) {
		base, capd = time.Second, 60*time.Second
	} else {
		base, capd = 200*time.Millisecond, 5*time.Second
	}
	if attempt < 1 {
		attempt = 1
	}
	// 指数档位（乘前检查截断，防溢出）
	temp := base
	for i := 1; i < attempt; i++ {
		if temp >= capd/2 {
			temp = capd
			break
		}
		temp *= 2
	}
	// 等额抖动：[temp/2, temp)；temp ≥ base > 0，故 half > 0 恒成立
	half := temp / 2
	return half + time.Duration(rand.Int63n(int64(half)))
}
