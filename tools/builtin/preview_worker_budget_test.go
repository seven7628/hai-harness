package builtin

// 常驻转换 worker 的**预算与回收**（第 5 轮爬山第 4 步）。
//
// 背景（实测）：三个 previewer（xlsx/docx/pptx）都是 manager 级单例、共用「按工作区」
// 的池，而**工作区数没有上限** ⇒ 常驻进程数 = 3 × 工作区数，每个约 33MB RSS
// ⇒ 10 个工作区就是 1GB RSS。这是内置工具层**唯一真正吃 swap 的大头**
// （Go 侧的堆是干净的：12 轮大输出后活堆 0.3MB → 0.3MB）。
//
// 本文件钉住四条：
//  ① 全局上限真的生效（超了淘汰最久没用的那个）；
//  ② **正在转换的** worker 不许被淘汰（杀了这次转换就失败）；
//  ③ 空闲回收器也不许动正在转换的 worker（改造前：跑了 6 分钟的转换被当成
//     「空闲 5 分钟」杀掉）；
//  ④ 取消路径要 **Close** 进程（改造前只 Kill ⇒ 管道 fd 不关、子进程成僵尸）。

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeProc 假常驻进程（记录 Close/Kill 次数）。
type fakeProc struct {
	mu     sync.Mutex
	stdout io.ReadCloser
	closed int
	killed int
}

func (f *fakeProc) Stdin() io.WriteCloser { return nopWriteCloser{io.Discard} }
func (f *fakeProc) Stdout() io.ReadCloser { return f.stdout }
func (f *fakeProc) Wait() error           { return nil }
func (f *fakeProc) Kill() error {
	f.mu.Lock()
	f.killed++
	f.mu.Unlock()
	return nil
}
func (f *fakeProc) Close() error {
	f.mu.Lock()
	f.closed++
	f.mu.Unlock()
	return nil
}

func (f *fakeProc) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// nopWriteCloser 丢弃写入的 WriteCloser。
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// fakeWorker 造一个不依赖真实解释器的 worker（stdout 是一段固定内容）。
func fakeWorker(t *testing.T, lastUse time.Time) *previewWorker {
	t.Helper()
	w := newPreviewWorker(&fakeProc{stdout: io.NopCloser(strings.NewReader(""))})
	w.last.Store(lastUse.UnixNano())
	return w
}

// swapRegistry 临时替换全局账本（测试后还原）。
func swapRegistry(t *testing.T, max int) *previewWorkerRegistry {
	t.Helper()
	oldMax, oldReg := previewWorkerGlobalMax, previewWorkers
	previewWorkerGlobalMax = max
	previewWorkers = &previewWorkerRegistry{workers: map[*previewWorker]struct{}{}}
	t.Cleanup(func() {
		previewWorkerGlobalMax = oldMax
		previewWorkers = oldReg
	})
	return previewWorkers
}

// TestPreviewWorkerGlobalCapEvictsLRU：超上限时淘汰**最久没用**的那个。
func TestPreviewWorkerGlobalCapEvictsLRU(t *testing.T) {
	reg := swapRegistry(t, 2)
	old := time.Now().Add(-2 * time.Minute)
	recent := time.Now().Add(-1 * time.Minute)
	wOld := fakeWorker(t, old)
	wNew := fakeWorker(t, recent)
	reg.add(wOld)
	reg.add(wNew)

	if !reg.admit() {
		t.Fatal("有空位可腾（最久没用的那个是空闲的）⇒ admit 应成功")
	}
	if got := reg.count(); got != 1 {
		t.Fatalf("淘汰后应只剩 1 个，实际 %d", got)
	}
	if !wOld.broken.Load() {
		t.Fatal("被淘汰的应是最久没用的那个（wOld）")
	}
	if wNew.broken.Load() {
		t.Fatal("较新的那个不该被淘汰")
	}
}

// TestPreviewWorkerGlobalCapSkipsBusy：正在转换的 worker **不许**被淘汰。
func TestPreviewWorkerGlobalCapSkipsBusy(t *testing.T) {
	reg := swapRegistry(t, 1)
	w := fakeWorker(t, time.Now().Add(-10*time.Minute))
	if !w.beginRun() {
		t.Fatal("beginRun 应成功")
	}
	reg.add(w)

	if reg.admit() {
		t.Fatal("唯一的 worker 正在转换 ⇒ 不该有位置可腾")
	}
	if w.broken.Load() {
		t.Fatal("正在转换的 worker 被淘汰了（杀了它这次转换就失败）")
	}
}

// TestPreviewWorkerReaperSkipsBusy：空闲回收器也不许动正在转换的 worker。
//
// 改造前的缺陷：回收器只看「多久没用」⇒ 一个跑了 6 分钟的转换会在第 5 分钟
// 被当成「空闲 5 分钟」杀掉 ⇒ 转换凭空失败。
func TestPreviewWorkerReaperSkipsBusy(t *testing.T) {
	p := newPreviewWorkerPool(nil, nil, "x")
	w := fakeWorker(t, time.Now().Add(-10*time.Minute))
	if !w.beginRun() {
		t.Fatal("beginRun 应成功")
	}
	p.workers["ws"] = w

	p.reapIdle(time.Now().Add(time.Hour)) // 假装过了很久

	if _, ok := p.workers["ws"]; !ok {
		t.Fatal("正在转换的 worker 被空闲回收器杀了")
	}
	if w.broken.Load() {
		t.Fatal("正在转换的 worker 被关掉了")
	}
}

// TestPreviewWorkerReaperCollectsIdle：空闲的照收（别把回收功能改没了）。
func TestPreviewWorkerReaperCollectsIdle(t *testing.T) {
	p := newPreviewWorkerPool(nil, nil, "x")
	w := fakeWorker(t, time.Now().Add(-10*time.Minute))
	p.workers["ws"] = w

	p.reapIdle(time.Now().Add(time.Hour))

	if _, ok := p.workers["ws"]; ok {
		t.Fatal("空闲超时的 worker 没被回收")
	}
	if !w.broken.Load() {
		t.Fatal("被回收的 worker 应标记 broken")
	}
}

// TestPreviewWorkerCancelClosesProcess：取消路径要 **Close** 进程。
//
// 改造前只 Kill()：管道 fd 不关、子进程不被 wait ⇒ **僵尸 + fd 泄漏**，
// 最多滞留到空闲回收器接手（5 分钟）。
func TestPreviewWorkerCancelClosesProcess(t *testing.T) {
	pr, pw := io.Pipe()
	_ = pw // 故意不写 ⇒ worker 永远等不到响应
	fp := &fakeProc{stdout: pr}
	w := newPreviewWorker(fp)

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := w.run(ctx, "/tmp/a.xlsx", "/tmp/out.html")
		errCh <- err
	}()
	time.Sleep(50 * time.Millisecond) // 让它写完请求、卡在读响应上
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("取消应返回错误")
		}
	case <-time.After(3 * time.Second):
		t.Fatal("取消后没返回")
	}
	if fp.closeCount() == 0 {
		t.Fatal("取消路径没有 Close 进程（fd 不关 ⇒ 泄漏僵尸，最多滞留 5 分钟）")
	}
}

// TestPreviewWorkerNewIsNotIdle：新建的 worker 不是「空闲 5 分钟」。
//
// 改造前 last=0（= 1970）⇒ 空闲回收器在第一次转换**之前**就会把它杀掉。
func TestPreviewWorkerNewIsNotIdle(t *testing.T) {
	w := newPreviewWorker(&fakeProc{stdout: io.NopCloser(strings.NewReader(""))})
	if w.lastUse().Before(time.Now().Add(-time.Minute)) {
		t.Fatalf("新建的 worker 应算「刚用过」，lastUse=%v", w.lastUse())
	}
	p := newPreviewWorkerPool(nil, nil, "x")
	p.workers["ws"] = w
	p.reapIdle(time.Now()) // 立刻巡检一次
	if _, ok := p.workers["ws"]; !ok {
		t.Fatal("新建的 worker 被当成空闲回收了（last=0 的老 bug）")
	}
}

// TestPreviewPoolCloseStopsWorkersAndReaper：关池 ⇒ 进程关掉、巡检协程退出。
func TestPreviewPoolCloseStopsWorkersAndReaper(t *testing.T) {
	reg := swapRegistry(t, 4)
	p := newPreviewWorkerPool(nil, nil, "x")
	w := fakeWorker(t, time.Now())
	p.workers["ws"] = w
	reg.add(w)

	p.Close()

	if w.broken.Load() != true {
		t.Fatal("关池应把 worker 关掉")
	}
	if reg.count() != 0 {
		t.Fatalf("关池后账本应清空，实际 %d", reg.count())
	}
	if _, err := p.worker(context.Background(), "ws"); err == nil {
		t.Fatal("关掉的池不该再能起 worker")
	}
}
