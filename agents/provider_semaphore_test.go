package agents

// P6-03：provider 并发信号量 —— 同时打到上游的请求数必须被闸住，
// 但排队等槽位的时间不能算进首字预算。

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestProviderSemaphoreCapsConcurrency(t *testing.T) {
	sem := NewProviderSemaphore(3)
	if sem == nil || sem.Capacity() != 3 {
		t.Fatalf("semaphore = %+v, want capacity 3", sem)
	}
	var inFlight, peak int64
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := sem.Acquire(context.Background()); err != nil {
				t.Errorf("acquire: %v", err)
				return
			}
			defer sem.Release()
			cur := atomic.AddInt64(&inFlight, 1)
			mu.Lock()
			if cur > peak {
				peak = cur
			}
			mu.Unlock()
			time.Sleep(2 * time.Millisecond)
			atomic.AddInt64(&inFlight, -1)
		}()
	}
	wg.Wait()
	mu.Lock()
	got := peak
	mu.Unlock()
	if got > 3 {
		t.Fatalf("并发峰值 = %d, 超过上限 3", got)
	}
	if got < 2 {
		t.Fatalf("并发峰值 = %d，闸门太严（信号量没并行放行）", got)
	}
	if n := sem.InFlight(); n != 0 {
		t.Fatalf("结束后占用 = %d, want 0", n)
	}
}

func TestProviderSemaphoreNilIsUnlimited(t *testing.T) {
	var sem *ProviderSemaphore
	if err := sem.Acquire(context.Background()); err != nil {
		t.Fatalf("nil 信号量不该阻塞: %v", err)
	}
	sem.Release() // 多还也不 panic
	if NewProviderSemaphore(0) != nil || NewProviderSemaphore(-1) != nil {
		t.Fatal("<=0 必须返回 nil（不限）")
	}
}

func TestProviderSemaphoreAcquireHonorsContext(t *testing.T) {
	sem := NewProviderSemaphore(1)
	if err := sem.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := sem.Acquire(ctx); err == nil {
		t.Fatal("ctx 已取消时 Acquire 必须返回错误（不能永久阻塞）")
	}
	sem.Release()
}

// TestProviderConcurrencyOption 装配口径：<=0 = 不限；>64 钳到 64。
func TestProviderConcurrencyOption(t *testing.T) {
	cfg := Config{}
	WithProviderConcurrency(0)(&cfg)
	if cfg.ProviderSem != nil {
		t.Fatal("0 = 不限（nil）")
	}
	WithProviderConcurrency(4)(&cfg)
	if cfg.ProviderSem == nil || cfg.ProviderSem.Capacity() != 4 {
		t.Fatalf("sem = %+v, want capacity 4", cfg.ProviderSem)
	}
}
