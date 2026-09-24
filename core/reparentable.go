package core

import (
	"context"
	"sync"
	"time"
)

// Reparentable 可摘离上下文（长时工具 promote 的核心原语）：
//   - Value() 恒委托父（ToolContext/待办/审批等 value 必须在场，Detach 后仍可读）；
//   - 取消默认随父级联（未 Detach：父 Done → 本 Done，Err = 父的取消原因）；
//   - Detach() 后父取消不再级联（promote 后任务存活过 run 结束与 run abort）；
//   - 主动 Cancel() 仍可用（batch abort 对未摘离任务调）。
//
// 实现必须自行管理 Done/Err，不可直接用 context.WithCancel(父)（后者无法摘离）。
// 一个 watcher goroutine：父 Done（未 Detach 时级联取消）/ 主动取消 / Detach 三者任一
// 即退出 —— 不泄漏。Detach 语义：此后父取消不再级联，任务生命周期移交调用方
// （摘离任务由 Session 级 ctx 管理，Shutdown 时随会话级联取消）。
type Reparentable struct {
	parent context.Context

	mu       sync.Mutex
	done     chan struct{}
	err      error
	detach   chan struct{} // Detach() 关闭：watcher 退出（父取消不再级联）
	detached bool
	once     sync.Once // done 只关闭一次
}

// NewReparentable 创建可摘离上下文并启动 watcher（父取消 → 级联；Detach/取消 → 退出）。
func NewReparentable(parent context.Context) *Reparentable {
	r := &Reparentable{
		parent: parent,
		done:   make(chan struct{}),
		detach: make(chan struct{}),
	}
	go r.watchParent()
	return r
}

// watchParent 父取消级联 + watcher 生命周期：
//   - 父 Done：未 Detach → 级联取消（Err = 父取消原因）；已 Detach → 忽略并退出；
//   - r.done（主动 Cancel/已级联）：退出；
//   - r.detach（Detach）：退出（此后父取消不再级联）。
func (r *Reparentable) watchParent() {
	select {
	case <-r.parent.Done():
		r.mu.Lock()
		det := r.detached
		r.mu.Unlock()
		if !det {
			r.cancel(r.parent.Err())
		}
	case <-r.done:
	case <-r.detach:
	}
}

// Context 返回自身（实现 context.Context），作为任务执行 ctx。
func (r *Reparentable) Context() context.Context { return r }

func (r *Reparentable) Deadline() (time.Time, bool) { return r.parent.Deadline() }

func (r *Reparentable) Done() <-chan struct{} { return r.done }

func (r *Reparentable) Err() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.err
}

func (r *Reparentable) Value(key any) any { return r.parent.Value(key) }

// Cancel 主动取消（batch abort 对未摘离任务调；Detach 后仍生效）。
func (r *Reparentable) Cancel() { r.cancel(context.Canceled) }

// cancel 关闭 done（幂等：once 保证只关一次）。
func (r *Reparentable) cancel(err error) {
	r.once.Do(func() {
		r.mu.Lock()
		r.err = err
		r.mu.Unlock()
		close(r.done)
	})
}

// Detach 摘离：此后父取消不再级联；主动 Cancel 仍可用。幂等（重复调用 no-op）。
func (r *Reparentable) Detach() {
	r.mu.Lock()
	if r.detached {
		r.mu.Unlock()
		return
	}
	r.detached = true
	r.mu.Unlock()
	close(r.detach)
}
