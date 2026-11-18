package builtin

// bash_output_tail.go：命令**运行中**的输出尾部（tail -f）——只给人看，不给模型看。
//
// 为什么需要（用户 2026-10 定调）：一条跑几分钟的命令，界面上此前只有「…」；用户既
// 不知道它卡在哪，也不知道它有没有在干活。但**增量绝不能进模型上下文**：模型要的是
// 最终结果（引擎已做截断 + 落盘），中间过程塞进上下文只是噪音 + 成本。
//
// 所以这里只做一件事：把子进程的输出**节流**成一条尾巴，经 events.ToolOutput 发给宿主，
// 由前端替换自己手里的窗口。整条链路有三条硬约束：
//
//  1. **不改变阻塞语义**：bash 工具照旧等命令结束才返回，模型侧一无所知；
//  2. **不拖慢命令**：回调在 os/exec 的拷贝协程里同步执行，必须瞬时返回 ——
//     攒够时间片（minInterval）才发一帧，每帧上限 maxTailBytes；
//  3. **不落盘**：宿主按事件类型过滤（bridge 的 isStreamingChunkType），
//     与 content_chunk / reasoning_chunk 同一口径 —— 可重建的展示层增量。
//
// 一条尾巴的语义是**替换**而不是追加：客户端拿到 Tail 就整段替换自己的窗口，于是
// 重复投递、乱序、丢帧都不会把界面弄脏（最坏情况是少看几行）。

import (
	"context"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/seven7628/hai-harness/events"
)

const (
	// maxTailBytes 单帧尾巴窗口上限。4 KiB ≈ 四五十行日志 —— 人看得清「在干什么」，
	// 又不至于让事件流变成第二条输出通道（那正是我们要避免的）。
	maxTailBytes = 4 << 10
	// tailMinInterval 最小发射间隔。250ms：再密对人没意义（肉眼看不清），却会把
	// 事件流和前端渲染压满；再疏会让人觉得「卡住了」。
	tailMinInterval = 250 * time.Millisecond
)

// outputTailer 一个调用的尾巴发射器。
//
// 并发：stdout / stderr 两个拷贝协程会同时调 write → 全程持锁。
// emit 在锁内调用（顺序 = 帧号顺序），实现必须是快速、非阻塞的（发事件 + 序列化）。
type outputTailer struct {
	mu        sync.Mutex
	window    []byte // 最近 maxTailBytes 字节（尾部窗口）
	total     int64  // 累计字节数（真值，不受窗口裁剪影响）
	seq       int64  // 已发帧数
	lastFlush time.Time
	// flushedTotal 上次发帧时的累计字节数（用来判断「自上次发帧以来有没有新输出」）。
	flushedTotal int64
	emit         func(seq int64, tail string, total int64, truncated bool)
}

// newOutputTailer 从工具 ctx 里取事件出口，装一个尾巴发射器；没有出口时返回 nil。
//
// 三种情况下本来就没有出口，这里必须静默降级（不报错、不影响命令）：
//   - 直接调用工具（测试、SDK 外部调用）：没有 ToolContext；
//   - 嵌套调用（codemode 脚本里的 tools.bash）：出口是 nestedApprovalHandler，
//     它只放行审批事件 —— 尾巴（以及任何其它事件）都被有意过滤掉；
//   - 宿主未接线 handler。
func newOutputTailer(ctx context.Context) *outputTailer {
	tc := events.ToolContextFrom(ctx)
	if tc == nil || tc.Handler == nil {
		return nil
	}
	h := tc.Handler
	runID := tc.RunId
	callID := tc.CallId
	if callID == "" {
		return nil // 没有调用 id 就无法归属到某一行工具卡，发了也没人认领
	}
	// lastFlush 起手就设成「现在」：**第一个时间片内不发射**。否则每条命令都会先冒一帧
	// 尾巴（0.1s 的 `ls` 也一样），前端就是「出现又消失」的抖动 —— 这正是执行中条加
	// 400ms 延迟要避免的观感。节流从 t0 起算，250ms 内跑完的命令一帧都不发。
	t := &outputTailer{lastFlush: time.Now()}
	t.emit = func(seq int64, tail string, total int64, truncated bool) {
		h(ctx, &events.ToolOutput{
			RunId:      runID,
			Id:         callID,
			Seq:        seq,
			Tail:       tail,
			TotalBytes: total,
			Truncated:  truncated,
			Timestamp:  time.Now(),
			EventType:  events.ToolOutputType,
		})
	}
	return t
}

// write 接在 sandbox.ExecSpec.OnOutput 上（p 仅在调用期间有效，这里只做拷贝）。
func (t *outputTailer) write(p []byte) {
	if t == nil || t.emit == nil || len(p) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total += int64(len(p))
	t.window = append(t.window, p...)
	if len(t.window) > maxTailBytes {
		t.window = t.window[len(t.window)-maxTailBytes:]
		// 窗口左边界可能落在多字节字符中间：往后挪到最近的 rune 边界，
		// 否则前端会显示一串替换字符（日志里的中文错误信息会当场变乱码）。
		for len(t.window) > 0 && !utf8.RuneStart(t.window[0]) {
			t.window = t.window[1:]
		}
	}
	// 节流：不到时间片不发射（但窗口与总量照常累积，下一帧自然带上最新的尾巴）。
	if time.Since(t.lastFlush) < tailMinInterval {
		return
	}
	t.flushLocked()
}

// flushLocked 发一帧（调用方持锁）。
func (t *outputTailer) flushLocked() {
	t.seq++
	t.lastFlush = time.Now()
	t.flushedTotal = t.total
	t.emit(t.seq, string(t.window), t.total, t.total > int64(len(t.window)))
}

// flushFinal 收尾补帧：命令结束时把**最后一段输出**补发出去。
//
// 为什么需要（第 5 轮爬山第 1 步，实测）：节流窗口 250ms ⇒ 命令**最后 250ms 内**
// 的输出永远发不出去（实测 `echo FIRST; sleep 0.1; echo LAST` ⇒ 一帧都没有）。
// 而**错误信息通常在结尾**（"error: cannot find package X"）⇒ 用户在界面上看到的是
// 「命令还在跑、最后一条日志是好的」，实际上一条命令早就失败了。
//
// 为什么短命令不补：**防抖**是有意的（0.1s 的命令不该「出现又消失」地闪一帧，
// 见 newOutputTailer 的注释）。所以判据是「**已经发过帧**」：
//   - 一帧都没发过 ⇒ 短命令 ⇒ 不补（保持不闪）；
//   - 发过帧 ⇒ 长命令 ⇒ 补结尾（窗口里的内容比上一帧更新才有意义）。
//
// 为什么还要看 flushedTotal：命令在最后一次发帧之后**没有新输出**时，补一帧是
// 白白发一个内容完全相同的事件（前端会替换成同样的窗口 = 无意义的抖动）。
func (t *outputTailer) flushFinal() {
	if t == nil || t.emit == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.seq == 0 {
		return // 一帧都没发过 ⇒ 短命令 ⇒ 不补（防抖优先）
	}
	if t.total == t.flushedTotal {
		return // 自上次发帧以来没有新输出 ⇒ 补了也是重复内容
	}
	t.flushLocked()
}

// spec0OnOutput 把 tailer 转成 ExecSpec.OnOutput；nil tailer → nil（执行层走历史路径）。
//
// 为什么单独一个函数：ExecSpec.OnOutput == nil 与「非 nil 但什么都不做」在热路径上
// 不等价 —— 前者不包 writerFunc、不产生每次 Write 的额外调用，保持「没接尾巴 = 零开销」。
func spec0OnOutput(t *outputTailer) func([]byte) {
	if t == nil {
		return nil
	}
	return t.write
}
