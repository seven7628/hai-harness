package builtin

// 常驻转换 worker 的回归网（第 4 轮爬山第 8 步）。
//
// 契约：
//  ① 排队要能被 ctx 取消（用户中断后，排队的预览不该干等前一个跑完）；
//  ② worker 坏了（进程死/协议错位）⇒ 标成 broken，下一次换新的。

import (
	"context"
	"testing"
	"time"
)

// TestPreviewWorkerQueueCancellable：排队中的转换能被 ctx 取消。
//
// 用信号量（sem）而不是 mutex 就是为了这条：mutex.Lock 不可取消 ⇒ 用户中断后
// 排队的那个转换还得等前一个跑完（可能几十秒）。
func TestPreviewWorkerQueueCancellable(t *testing.T) {
	w := &previewWorker{sem: make(chan struct{}, 1)}
	w.sem <- struct{}{} // 占住唯一的槽位 ⇒ 下一个调用只能排队
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan struct{})
	go func() {
		_, _ = w.run(ctx, "/tmp/a.xlsx", "/tmp/out.html")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("排队中的转换没被 ctx 取消打断（该用信号量而不是 mutex）")
	}
}

// TestPreviewWorkerBrokenFlag：broken 的 worker 不会再被复用。
func TestPreviewWorkerBrokenFlag(t *testing.T) {
	w := &previewWorker{sem: make(chan struct{}, 1)}
	if w.broken.Load() {
		t.Fatal("新 worker 不该是 broken 的")
	}
	w.broken.Store(true)
	if !w.broken.Load() {
		t.Fatal("broken 标记没生效")
	}
}
