package agents

// provider_semaphore.go：provider 并发信号量（P6-03）。
//
// 为什么在 loop 层而不是在 bridge 调度器里：一个群 run 一轮会发起**多次** provider
// 调用（工具轮之后继续），调度器只能限制「同时有几个 run」，限不住「同时有几个
// 上游请求」。11 人群 @all 时并发放行 11 个 run 是对的（每个智能体都在干活），但
// 同时打到上游的请求数必须可控 —— 否则 429 会成片出现，退避又把整群拖慢。
//
// 语义：
//   - 0 或负数 = 不限（默认，保持旧行为）；
//   - Acquire 阻塞直到拿到槽位或 ctx 结束；等待**不计入首字预算**（看门狗在拿到
//     槽位之后才启动，否则「排队等槽位」会被误判成「上游无响应」）；
//   - 槽位是进程级的（同一个 limiter 实例被同一 provider 的所有 loop 共享）。

import "context"

// ProviderSemaphore 限制同时进行的 provider 调用数。零值/空指针 = 不限。
type ProviderSemaphore struct {
	ch chan struct{}
	n  int
}

// NewProviderSemaphore 构造容量为 n 的信号量；n <= 0 返回 nil（不限）。
func NewProviderSemaphore(n int) *ProviderSemaphore {
	if n <= 0 {
		return nil
	}
	return &ProviderSemaphore{ch: make(chan struct{}, n), n: n}
}

// Capacity 槽位总数（0 = 不限）。
func (s *ProviderSemaphore) Capacity() int {
	if s == nil {
		return 0
	}
	return s.n
}

// Acquire 占一个槽位；ctx 结束时返回 ctx.Err()。
func (s *ProviderSemaphore) Acquire(ctx context.Context) error {
	if s == nil {
		return nil
	}
	select {
	case s.ch <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Release 归还槽位（必须与成功的 Acquire 成对）。
func (s *ProviderSemaphore) Release() {
	if s == nil {
		return
	}
	select {
	case <-s.ch:
	default:
		// 多还了：宁可忽略也不要 panic（关停路径可能重复调用）。
	}
}

// InFlight 当前占用槽位数（诊断/测试用）。
func (s *ProviderSemaphore) InFlight() int {
	if s == nil {
		return 0
	}
	return len(s.ch)
}

// WithProviderConcurrency 设置 provider 并发上限（<=0 = 不限，默认）。
func WithProviderConcurrency(n int) Option {
	return func(c *Config) { c.ProviderSem = NewProviderSemaphore(n) }
}

// WithProviderSemaphore 直接注入信号量（bridge 装配时共享同一实例用）。
func WithProviderSemaphore(sem *ProviderSemaphore) Option {
	return func(c *Config) { c.ProviderSem = sem }
}
