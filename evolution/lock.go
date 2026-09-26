package evolution

import "sync"

// chainLock 一个简单的可重入无关互斥句柄。
//
// 为什么用返回 unlock 函数的闭包而不是 sync.Mutex 直接暴露：本包的写路径
// （Verify / Promote / RecordOutcome / Rollback）都是"多步读-改-写"，需要把锁
// 覆盖整段操作。返回句柄让 Service 内部可以用 `defer unlock()` 把锁的生命周期
// 与函数绑定，看不出"忘了 unlock"的可能。
type chainLock struct{ mu sync.Mutex }

// lock 加锁并返回解锁函数。
func (c *chainLock) lock() func() {
	c.mu.Lock()
	return c.mu.Unlock
}
