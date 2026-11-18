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
	"strings"
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
	mu    sync.Mutex
	ring  tailRing // 最近 maxTailBytes 字节（尾部窗口，环形缓冲 ⇒ 写入零分配）
	total int64    // 累计字节数（真值，不受窗口裁剪影响）
	seq   int64    // 已发帧数
	// flushedTotal 上次发帧时的累计字节数（用来判断「自上次发帧以来有没有新输出」）。
	flushedTotal int64
	lastFlush    time.Time
	emit         func(seq int64, tail string, total int64, truncated bool)
	// sink 异步投递口（nil = 同步直发；直调工具/测试用）。
	sink *tailSink
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
	// 事件出口是**异步**的（第 5 轮爬山第 2 步）：emit 一路走到宿主（bridge 写 stdout
	// → 前端），可能阻塞；而它原本跑在**子进程的拷贝协程**里 ⇒ 宿主一卡，子进程的
	// stdout 管道就堵 ⇒ 命令被拖慢（实测：回调每块睡 100ms ⇒ 命令 173ms → 2.14s）。
	// 搬进独立协程后，拷贝协程只做「存最新一帧 + 敲铃」，恒为 O(1)、永不阻塞。
	sink := newTailSink(func(seq int64, tail string, total int64, truncated bool) {
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
	})
	t.sink = sink
	t.emit = sink.push
	return t
}

// close 收尾：排空最后一帧并等发射协程退出（命令结束时调用）。
//
// 预算（tailCloseBudget）：发射协程可能卡在宿主上（前端卡 ⇒ bridge 写 stdout 阻塞）
// ⇒ 不能无限等。超预算就放弃等待（协程会在宿主恢复后自行退出，不泄漏）。
func (t *outputTailer) close() {
	if t == nil || t.sink == nil {
		return
	}
	t.sink.close(tailCloseBudget)
}

// tailRing 尾巴窗口的环形缓冲（第 5 轮爬山第 3 步）。
//
// **为什么**：原来的窗口是 `append` + 前移切片（`w = w[len-4096:]`）。前移切片
// **不吃容量**（cap 跟着左边界一起缩）⇒ 每来一块（os/exec 的拷贝块 32KB）就得
// 重新分配一块 ~40KB 并拷贝一遍 ⇒ 实测 16 MB 输出 = **21 MB 垃圾**（1.3× 输出量），
// 1 GB 输出 = 1.3 GB churn（都进 GC，纯浪费）。
//
// 环形缓冲：固定 maxTailBytes 的数组 + 写指针 ⇒ **写入零分配**；只有发帧那一刻
// 才把窗口拷成字符串（≤ 4 次/秒）。
//
// 不变式（**逐字节与改造前一致**）：
//   - 窗口内容 = 输出的最后 min(total, maxTailBytes) 字节（最旧 → 最新）；
//   - 总量超过窗口上限时，左边界可能落在多字节字符中间 ⇒ 往前挪到 rune 边界
//     （否则前端显示一串替换字符 —— 日志里的中文错误信息会当场变乱码）。
type tailRing struct {
	buf   []byte // 固定容量 maxTailBytes（惰性分配：还没输出过就不占这 4KB）
	head  int    // 下一个写入位置
	n     int    // 当前有效字节数（≤ len(buf)）
	total int64  // 累计写入字节数（判断「窗口是否溢出过」⇒ 要不要做 rune 边界修剪）
}

// write 写一块输出（**零分配**；覆盖最旧的内容）。
func (r *tailRing) write(p []byte) {
	if len(p) == 0 {
		return
	}
	if r.buf == nil {
		r.buf = make([]byte, maxTailBytes)
	}
	capacity := len(r.buf)
	r.total += int64(len(p))
	if len(p) >= capacity {
		// 一块就能填满/超出窗口 ⇒ 只留最后 capacity 字节。
		copy(r.buf, p[len(p)-capacity:])
		r.head = 0
		r.n = capacity
		return
	}
	// 第一段：head..末尾；第二段（绕回）：0..
	first := copy(r.buf[r.head:], p)
	if first < len(p) {
		copy(r.buf, p[first:])
	}
	r.head = (r.head + len(p)) % capacity
	if r.n+len(p) <= capacity {
		r.n += len(p)
	} else {
		r.n = capacity
	}
}

// window 取当前窗口内容（最旧 → 最新）并拷成字符串。
//
// 一次分配（strings.Builder）：调用频率 ≤ 4 次/秒（节流），不是热路径。
func (r *tailRing) window() string {
	if r.n == 0 {
		return ""
	}
	start := (r.head - r.n + len(r.buf)) % len(r.buf)
	// 有效字节 = 从 start 开始的 n 个（可能绕回）—— **不能**直接把 buf[start:] 全写出去
	// （那会带上未写入的零字节：差分测试抓到过这个坑）。
	first := len(r.buf) - start
	if first > r.n {
		first = r.n
	}
	var sb strings.Builder
	sb.Grow(r.n)
	sb.Write(r.buf[start : start+first])
	if first < r.n {
		sb.Write(r.buf[:r.n-first])
	}
	// 窗口溢出过 ⇒ 左边界可能落在多字节字符中间 ⇒ 往前挪到最近的 rune 边界。
	// （与改造前逐字节一致：改造前是在写入时修剪窗口左边界。）
	if r.total > int64(len(r.buf)) {
		s := sb.String()
		for len(s) > 0 && !utf8.RuneStart(s[0]) {
			s = s[1:]
		}
		return s
	}
	return sb.String()
}

// write 接在 sandbox.ExecSpec.OnOutput 上（p 仅在调用期间有效，这里只做拷贝）。
func (t *outputTailer) write(p []byte) {
	if t == nil || t.emit == nil || len(p) == 0 {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.total += int64(len(p))
	t.ring.write(p)
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
	win := t.ring.window()
	t.emit(t.seq, win, t.total, t.total > int64(len(win)))
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

// ---------------------------------------------------------------------------
// tailSink：尾巴帧的异步投递口（第 5 轮爬山第 2 步）
// ---------------------------------------------------------------------------

// tailCloseBudget 收尾时等发射协程退出的预算。
//
// 为什么要有预算：发射协程可能卡在宿主上（前端卡 ⇒ bridge 写 stdout 阻塞）⇒
// 无界等待会把「前端卡」变成「工具调用卡」。超预算就放弃等（协程在宿主恢复后
// 自行退出 ⇒ 不泄漏）。
const tailCloseBudget = 200 * time.Millisecond

// tailSink 尾巴帧的异步投递口。
//
// **为什么要它**：尾巴帧的 emit 会一路走到宿主（bridge 写 stdout → 前端）⇒ 可能阻塞。
// 而 emit 原本跑在**子进程的拷贝协程**里（sandbox 的 OnOutput 回调）⇒ 宿主一卡，
// 子进程的 stdout 管道就堵 ⇒ **命令本身被拖慢**（实测：回调每块睡 100ms ⇒ 命令
// 173ms → 2.14s，12×；而这条回调的终点就是 bridge 的一次 stdout 写）。
//
// 搬到这里之后：拷贝协程只做「把最新一帧放进槽位 + 敲一下铃」，**恒为 O(1)、
// 永不阻塞**；真正发事件的是独立协程，宿主卡多久都只影响它自己。
//
// **为什么只留最新一帧**（而不是排队）：一帧携带的是**整个窗口**（不是增量）⇒
// 新帧的内容**包含**旧帧的内容 ⇒ 覆盖旧帧不丢信息（最坏是少看几行，与尾巴
// 「整段替换」的语义一致）。排队反而会让 UI 落后于真实输出。
type tailSink struct {
	mu      sync.Mutex
	pending *tailFrame // 最新一帧（旧的被覆盖）
	closed  bool

	sig  chan struct{} // 有新帧 ⇒ 敲一下铃
	quit chan struct{} // 收尾 ⇒ 停
	done chan struct{} // 发射协程退出

	emit func(seq int64, tail string, total int64, truncated bool)
}

// tailFrame 一帧尾巴（整窗替换语义）。
type tailFrame struct {
	seq       int64
	tail      string
	total     int64
	truncated bool
}

// newTailSink 造投递口并起发射协程。
func newTailSink(emit func(seq int64, tail string, total int64, truncated bool)) *tailSink {
	s := &tailSink{
		sig:  make(chan struct{}, 1),
		quit: make(chan struct{}),
		done: make(chan struct{}),
		emit: emit,
	}
	go s.loop()
	return s
}

// loop 发射协程：等铃 ⇒ 取最新一帧 ⇒ 发出去。
func (s *tailSink) loop() {
	defer close(s.done)
	for {
		select {
		case <-s.sig:
			s.drainAndEmit()
		case <-s.quit:
			// 收尾：**最后一帧不能丢**（命令的最后一段输出就在里面）。
			s.drainAndEmit()
			return
		}
	}
}

// drainAndEmit 取走最新一帧并发出去（没有待发帧就什么都不做）。
func (s *tailSink) drainAndEmit() {
	s.mu.Lock()
	f := s.pending
	s.pending = nil
	s.mu.Unlock()
	if f != nil {
		s.emit(f.seq, f.tail, f.total, f.truncated)
	}
}

// push 存下最新一帧并敲铃（**恒不阻塞**）。这是拷贝协程唯一会碰的路径。
func (s *tailSink) push(seq int64, tail string, total int64, truncated bool) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return // 已收尾 ⇒ 不再收帧
	}
	s.pending = &tailFrame{seq: seq, tail: tail, total: total, truncated: truncated}
	s.mu.Unlock()
	select {
	case s.sig <- struct{}{}:
	default:
		// 铃已经响着（发射协程还没醒）⇒ 不用再敲；它醒来时会取走**最新**那一帧。
	}
}

// close 收尾：停发射协程，等它把最后一帧发出去（有预算）。
func (s *tailSink) close(budget time.Duration) {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return
	}
	s.closed = true
	s.mu.Unlock()
	close(s.quit)
	select {
	case <-s.done:
	case <-time.After(budget):
		// 发射协程卡在宿主上 ⇒ 放弃等待（它会在宿主恢复后自行退出）。
	}
}
