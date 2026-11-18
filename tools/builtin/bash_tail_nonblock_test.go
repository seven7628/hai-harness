package builtin

// 尾巴帧的**异步投递**（第 5 轮爬山第 2 步）。
//
// 问题（实测）：emit 原本跑在**子进程的拷贝协程**里（sandbox 的 OnOutput 回调）。
// emit 的终点是宿主（bridge 写 stdout → 前端）⇒ 宿主一卡（前端冻结 ⇒ stdout 写
// 阻塞），拷贝协程就卡 ⇒ 子进程的 stdout 管道（64KB）填满 ⇒ **子进程自己卡住**
// ⇒ 命令被拖慢。实测：宿主每次卡 2 秒时，一条本该 3 秒跑完的命令跑了 **34.3 秒**。
//
// 现在：emit 挪进独立协程（tailSink），拷贝协程只做「存最新一帧 + 敲铃」，
// **恒为 O(1)、永不阻塞** ⇒ 命令与宿主解耦（同样条件下 4.0 秒 = 命令的自然速度）。
//
// 两条必须钉住的：
//  1. 生产端（拷贝协程）**永不阻塞**；
//  2. 宿主卡住期间产生的多帧 ⇒ 最终发出去的是**最新**那一帧（整窗替换语义 ⇒
//     丢旧帧不丢信息）。

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/events"
)

// TestTailSinkNeverBlocksProducer：生产端不得被宿主拖住。
func TestTailSinkNeverBlocksProducer(t *testing.T) {
	release := make(chan struct{})
	emitted := make(chan int64, 16)
	s := newTailSink(func(seq int64, tail string, total int64, truncated bool) {
		<-release // 宿主卡住（模拟 bridge 写 stdout 阻塞）
		emitted <- seq
	})

	// 生产端：连推 5 帧，必须**立刻**返回。
	pushed := make(chan struct{})
	go func() {
		for i := 1; i <= 5; i++ {
			s.push(int64(i), "tail", int64(i), false)
		}
		close(pushed)
	}()
	select {
	case <-pushed:
	case <-time.After(2 * time.Second):
		close(release)
		t.Fatal("push 被宿主拖住了（生产端必须恒为 O(1)、永不阻塞）")
	}

	close(release)
	s.close(2 * time.Second)

	var got []int64
	for {
		select {
		case seq := <-emitted:
			got = append(got, seq)
			continue
		default:
		}
		break
	}
	if len(got) == 0 {
		t.Fatal("一帧都没发出去")
	}
	last := got[len(got)-1]
	if last != 5 {
		t.Fatalf("最后发出的应是**最新**那一帧（seq=5），实际 %v", got)
	}
}

// TestBashTailHostFrozenDoesNotStallCommand：宿主冻结时，命令**不能**被拖住。
//
// 判据是确定性的（不靠墙钟比大小）：宿主处理函数阻塞在一条 channel 上，测试
// **先不放**它 ⇒ 若命令被拖住，命令根本不会结束（子进程 stdout 管道填满 ⇒
// 子进程卡住）⇒ 用超时判「命令是否在宿主放行前就结束」。
func TestBashTailHostFrozenDoesNotStallCommand(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	var frames int64
	handler := func(context.Context, events.Event) {
		atomic.AddInt64(&frames, 1)
		<-release // 宿主冻结
	}
	ctx := events.WithCallId(
		events.WithToolContextForRun(context.Background(), "run-frozen", "", 0, handler, nil, nil), "c")

	bt := NewBashTool(t.TempDir(), 60*time.Second, nil, nil)
	// 命令持续吐输出（约 2 秒、600KB）⇒ 一定跨过节流窗口 ⇒ 一定发过帧。
	const cmd = "for i in $(seq 1 20); do head -c 32768 /dev/zero | tr '\\0' 'x'; sleep 0.1; done"

	done := make(chan error, 1)
	go func() {
		_, err := bt.Call(ctx, "bash", mustJSON(t, map[string]string{"command": cmd}))
		done <- err
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		if atomic.LoadInt64(&frames) == 0 {
			t.Fatal("本用例的前提是「发过帧」（否则测不到宿主冻结的影响）")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("命令被宿主冻结拖住了（宿主一卡 ⇒ 拷贝协程卡 ⇒ 子进程 stdout 管道填满 ⇒ 命令卡住）")
	}
}
