package tools

import (
	"context"
	"sync"
	"sync/atomic"
	"time"
)

// UnlimitedTimeoutKey 上下文标记键：任务被摘离为后台（promote）后，以此 key 置 true，
// 让工具层的自设时限（如 bash 的模型 timeout 参数）感知并放开 —— 后台任务期望不限时。
var UnlimitedTimeoutKey = &struct{ name string }{name: "tools.unlimited-timeout"}

// IsUnlimited 判断 ctx 是否标记为「后台任务不限时」（promote 摘离后为 true）。
func IsUnlimited(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(UnlimitedTimeoutKey).(bool)
	return v
}

// liftableTimeout 可摘离时限上下文：给单次工具执行一个可选时限，promote（转后台）可 Lift 解除。
//   - 未 Lift：到点或父取消 → Done 关闭（Err = deadline/父原因），工具照常受时限；
//   - Lift 后：时限解除——Deadline() 无值、时限计时器停止、Done 不再因时限关闭、
//     Value 标记 Unlimited；任务存活至自然完成或显式 Cancel（Interrupt）/ 父取消。
//     注意：父取消**仍级联**（watcher 在 Lift 后不退出）——精确中断（BatchController.
//     Interrupt → rctx.Cancel）必须能穿透 Lift 后的执行 ctx，否则 Bash 等长工具停不下来。
//
// 实现要点：调用方（runBatch）把本 ctx 作为任务的执行 ctx；Lift 是**动态**的，工具内部
// 依赖 ctx.Done()/ctx.Deadline() 的窗口（如 sandbox.runExec 不再自叠 WithTimeout）会
// 自然跟随——这是「promote 后不再限时」能穿透到最内层的保证。
type liftableTimeout struct {
	parent   context.Context
	deadline time.Time
	lifted   atomic.Bool

	timerOnce sync.Once
	timer     *time.Timer

	stopOnce sync.Once
	stopCh   chan struct{} // 任务结束回收：watcher 退出（Lift 不关此通道——取消仍须级联）

	doneOnce sync.Once
	done     chan struct{}
	errMu    sync.Mutex
	err      error
}

func newLiftableTimeout(parent context.Context, timeout time.Duration) *liftableTimeout {
	lb := &liftableTimeout{
		parent:   parent,
		deadline: time.Now().Add(timeout),
		stopCh:   make(chan struct{}),
		done:     make(chan struct{}),
	}
	lb.timer = time.AfterFunc(timeout, func() {
		if lb.lifted.Load() {
			return // 已摘离（promote）：不再限时
		}
		lb.closeDone(context.DeadlineExceeded)
	})
	go lb.watchParent()
	return lb
}

// watchParent 父取消级联：lift 后仍生效（精确中断穿透）——watcher 只随任务回收（stop）
// 或 done 关闭退出。父为 Reparentable：Detach 后父取消不再自动级联，但显式 Cancel
// （BatchController.Interrupt）仍会到达这里。
func (lb *liftableTimeout) watchParent() {
	select {
	case <-lb.parent.Done():
		lb.closeDone(context.Cause(lb.parent)) // 未 Lift：时限窗口父取消；已 Lift：中断取消
	case <-lb.done:
	case <-lb.stopCh:
	}
}

// lift 解除时限（promote 调用）：停表 + 标记 Unlimited；幂等。
// 不关 stopCh——父取消（精确中断）必须持续级联到本任务执行 ctx。
func (lb *liftableTimeout) lift() {
	if lb.lifted.Swap(true) {
		return
	}
	lb.timerOnce.Do(func() {
		if lb.timer != nil {
			lb.timer.Stop()
		}
	})
}

// stop 任务正常结束后回收（停表 + 退 watcher）；幂等。
func (lb *liftableTimeout) stop() {
	lb.timerOnce.Do(func() {
		if lb.timer != nil {
			lb.timer.Stop()
		}
	})
	lb.stopOnce.Do(func() {
		close(lb.stopCh)
	})
}

// closeDone 幂等关闭 done（仅一次生效）。
// 注：不做 lifted 兜底——lift 只解除「时限到期关闭」，取消（父/显式）必须始终可关
// （被精确中断的后台任务经此把取消原因传给工具）。
func (lb *liftableTimeout) closeDone(err error) {
	lb.doneOnce.Do(func() {
		lb.errMu.Lock()
		lb.err = err
		lb.errMu.Unlock()
		close(lb.done)
	})
}

// —— context.Context ——

func (lb *liftableTimeout) Deadline() (time.Time, bool) {
	if lb.lifted.Load() {
		return time.Time{}, false
	}
	if pd, ok := lb.parent.Deadline(); ok && pd.Before(lb.deadline) {
		return pd, true
	}
	return lb.deadline, true
}

func (lb *liftableTimeout) Done() <-chan struct{} { return lb.done }

func (lb *liftableTimeout) Err() error {
	lb.errMu.Lock()
	defer lb.errMu.Unlock()
	return lb.err
}

func (lb *liftableTimeout) Value(key any) any {
	if key == UnlimitedTimeoutKey && lb.lifted.Load() {
		return true
	}
	return lb.parent.Value(key)
}
