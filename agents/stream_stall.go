package agents

import (
	"sync"
	"time"

	"github.com/seven7628/hai-harness/provider"
)

// streamStallWatch 流内停滞看门狗（2026-09-22）：把「流式响应中途静默挂死」从
// 「无限等待」变成「一次可重试的上游故障」。
//
// 为什么需要（用户反馈样本）：超时策略原本只有两段 —— HTTP ResponseHeaderTimeout
// （等响应头 60s）与首字预算（FirstChunkTimeout，默认 30s，只到首个事件为止）。首个事件
// 之后流内**完全不设界**（刻意如此：长 reasoning / 大文档生成可达数分钟，设总时限会误杀
// 健康生成）。但「不设总时限」不等于「允许永远没有动静」：上游连接僵死（TCP 不断开、
// 也不再发任何字节）时 provider 的 Recv/Next 会一直阻塞 —— 实测（GLM Provider）界面上
// 「超过 5 分钟没有任何输出」，且只能等对端 RST 或用户手动点停止。
//
// 语义边界：只界**相邻两个事件之间的静默**，不界总时长。每个 provider 事件都重置计时，
// 所以持续产出的长响应对本看门狗完全不可见；只有「真的一直没有动静」才会命中。
// 计时从**首个事件**开始（无首字的区间归首字预算管，两段不重叠、互不抢归因）。
type streamStallWatch struct {
	timeout time.Duration
	cancel  func(error)

	mu      sync.Mutex
	timer   *time.Timer
	last    time.Time
	stopped bool
}

// newStreamStallWatch 构造看门狗（timeout <= 0 = 不启用，返回 nil —— 所有方法对 nil
// 接收者安全，调用点无需分支）。
func newStreamStallWatch(timeout time.Duration, cancel func(error)) *streamStallWatch {
	if timeout <= 0 {
		return nil
	}
	return &streamStallWatch{timeout: timeout, cancel: cancel}
}

// touch 记录一次 provider 事件：首个事件启动计时，其后每个事件重置计时（有动静即
// 不判定停滞）。Stop/streamOnce 退出后到达的事件被 stopped 挡住，不再重挂计时器。
func (w *streamStallWatch) touch() {
	if w == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return
	}
	w.last = time.Now()
	if w.timer == nil {
		w.timer = time.AfterFunc(w.timeout, w.fire)
		return
	}
	w.timer.Reset(w.timeout)
}

// fire 计时到点：静默达预算 → 以 provider.StreamStallError 取消本次尝试的 ctx；
// 若在「到点」与「事件到达」之间发生了竞争（静默不足预算），按剩余时间重挂，不误杀。
func (w *streamStallWatch) fire() {
	w.mu.Lock()
	if w.stopped {
		w.mu.Unlock()
		return
	}
	silent := time.Since(w.last)
	if silent < w.timeout {
		w.timer.Reset(w.timeout - silent)
		w.mu.Unlock()
		return
	}
	w.stopped = true
	w.mu.Unlock()
	// 出锁调用：cancel 会触发 provider 侧的取消路径，不在锁内做任何外部调用。
	w.cancel(provider.NewStreamStallError(w.timeout, nil))
}

// stop 释放看门狗（streamOnce 退出时 defer 调用）：停表并阻止迟到的取消。
func (w *streamStallWatch) stop() {
	if w == nil {
		return
	}
	w.mu.Lock()
	w.stopped = true
	t := w.timer
	w.mu.Unlock()
	if t != nil {
		t.Stop()
	}
}
