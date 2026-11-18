package builtin

// grep 的**并行扫描**管线（G9）。
//
// 改造前：整条扫描链单线程 —— 遍历（readdir）→ 打开文件 → 读内容 → 正则匹配
// 全部在一个 goroutine 里串行。改造后的 profile 显示 **88% 的时间在等 I/O**
// （syscall.read），也就是「读文件内容」这一段是瓶颈，而它恰好是最容易并行的一段。
//
// 为什么不并行「遍历」而是并行「读文件」：
//
//  1. ripgrep 并行的是**遍历 + 搜索**（crossbeam work-stealing），代价是**输出顺序不确定**。
//     我们的 grep 有 offset 分页 —— 「全局第 k 个命中」必须有一个**稳定且确定**的全局顺序，
//     否则 offset=200 翻出来的第二页和第一次翻页对不上。
//     ⇒ 遍历保持**串行且顺序确定**（和改造前逐字节一致），并行的是「读文件内容」。
//  2. 实测瓶颈也不在遍历上：忽略列表（第 1 步）把 116,431 个文件剪到 1,804 个，
//     遍历本身只枚举几千个目录项；而内容读取是 29.5 MB / 1,804 次 open+read。
//
// 三段式：
//
//	遍历（串行、顺序确定） → 并行「第一遍：数命中」 → 收集（按遍历顺序） → 并行「第二遍：取内容」
//
// 为什么分两遍：
//
//   - 第一遍只**数命中 + 记命中行号**，不保留任何行内容 ⇒ 内存与文件大小无关；
//   - 第二遍只对**真正要输出的那几个文件**再读一次，取「命中行 ± context」的内容。
//     改造前也是两遍扫描（bufio.Scanner 时代的同一设计），这里只是把两遍拆到了两个阶段，
//     让**第一遍（读全部文件、最耗时）并行**。

import (
	"context"
	"os"
	"regexp"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"

	"io/fs"
	"path/filepath"
)

const (
	// grepMaxWorkers 并行扫描的 worker 数上限。
	// 超过 CPU 核数对 I/O 密集的读没有额外收益（ripgrep 也把线程数压在 12 以内）。
	grepMaxWorkers = 8
	// grepWorkersEnv 覆盖 worker 数（调优 / 基准用）。
	grepWorkersEnv = "HAI_GREP_WORKERS"
)

// grepWorkerCount 并行扫描的 worker 数。
func grepWorkerCount() int {
	if v := os.Getenv(grepWorkersEnv); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 1 {
			return n
		}
	}
	n := runtime.GOMAXPROCS(0)
	if n > grepMaxWorkers {
		n = grepMaxWorkers
	}
	if n < 1 {
		n = 1
	}
	return n
}

// grepJob 一个待扫描的文件（遍历阶段产出）。
type grepJob struct {
	seq uint64 // 遍历序号（用于把乱序返回的结果排回遍历顺序）
	rel string // 相对工作区根的路径（输出用）
	abs string // 绝对路径
}

// grepCountResult 单文件**第一遍**扫描的结果：只数命中、记命中行号，不留行内容。
type grepCountResult struct {
	seq    uint64
	rel    string
	abs    string
	lines  []int // 命中的行号（升序；最多 budget 个）
	count  int   // 命中总数（== len(lines) 时可能还有更多，见 capped）
	capped bool  // 命中数达到 budget → 已停止读该文件（后面还有命中）
	total  int   // 已读到的行数（capped 时是文件的一部分）
	err    error
}

// grepContentReq 第二遍的内容采集请求。
type grepContentReq struct {
	rel   string
	abs   string
	lines []int // 要输出的命中行号（已按窗口切好）
	total int   // 文件总行数（用于把 context 区间夹在文件内）
}

// grepCollector 收集窗口：把各文件的命中并进「本页」，并决定何时停止。
//
// 分页语义（对齐 Claude Code 的 head_limit + offset、OpenCode/Roo 的条数上限）：
//
//   - 全局第 k 个命中（1 基）落在 [offset+1, offset+max] ⇒ 收进本页；
//   - k ≤ offset ⇒ 跳过（但**继续扫**，后面可能还有）；
//   - k > offset+max ⇒ 本页到此为止（后面还有，表头会报出来）。
//
// 为什么需要分页：改造前有 200 条硬上限且**没有翻页口** —— 模型看到
// 「more than 200 matches」后只能改 pattern 再搜一遍，或者放弃。
type grepCollector struct {
	offset    int
	max       int
	total     int  // 已并入的命中总数（含被 offset 跳过的）
	taken     int  // 本页收下的命中数
	files     int  // 有命中的文件数（含命中全被 offset 跳过的）
	truncated bool // 命中数超过本页窗口 ⇒ 后面还有
	results   []grepFileResult
}

// end 本页窗口的上界（全局命中序号，闭区间）。
func (c *grepCollector) end() int { return c.offset + c.max }

// addFile 把一个文件的计数结果并入窗口。
//
// 返回需要采集内容的请求（nil = 这个文件不用出内容：要么没命中，
// 要么命中全被 offset 跳过）。
//
// 局部下标 i（0 基）对应全局命中序号 total+i+1；落在窗口 [offset+1, offset+max]
// ⇔ i ∈ [offset-total, offset+max-total-1]。
func (c *grepCollector) addFile(p grepCountResult) *grepContentReq {
	if p.count == 0 {
		return nil
	}
	c.files++ // 有命中的文件数（含命中全被 offset 跳过的）
	lo := c.offset - c.total
	if lo < 0 {
		lo = 0
	}
	hi := c.offset + c.max - c.total - 1
	if hi > p.count-1 {
		hi = p.count - 1
	}
	if lo > hi {
		// 这个文件的命中全在窗口之前（或全在窗口之后）⇒ 不出内容。
		c.total += p.count
		c.checkTruncated()
		return nil
	}
	c.taken += hi - lo + 1
	need := p.lines[lo : hi+1]
	c.total += p.count
	c.checkTruncated()
	return &grepContentReq{rel: p.rel, abs: p.abs, lines: need, total: p.total}
}

// checkTruncated 命中总数越过窗口上界 ⇒ 后面还有（表头要报出来）。
func (c *grepCollector) checkTruncated() {
	if c.total > c.end() {
		c.truncated = true
	}
}

// grepSearch 遍历工作区、并行扫描文件、并按遍历顺序收集命中。
//
// 返回的错误只可能是「上下文取消/超时」；单文件的失败（二进制、读不出）被跳过。
func (t *grepTool) search(ctx context.Context, re *regexp.Regexp, root string, a grepArgs, ign *ignoreMatcher, c *grepCollector) error {
	workers := grepWorkerCount()
	// 每个文件最多保留 budget 个命中：窗口内命中的局部下标最大是 offset+max-1，
	// 而**整个文件**的命中可能全部落在 offset 之前（offset 被一个"命中很多"的文件吃掉），
	// 所以预算必须按 offset 放大：budget = offset+max+1。
	// 多出来的 1 个用于判断「后面还有」（capped）。
	budget := c.offset + c.max + 1

	jobs := make(chan grepJob, workers)
	outs := make(chan grepCountResult, workers)

	// 并行 worker：只做第一遍（数命中、记行号），不碰行内容。
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			var sc *grepScanner // 懒分配：没抢到任务的 worker 不分配读缓冲
			for job := range jobs {
				if sc == nil {
					sc = newGrepScanner()
				}
				res := grepCountResult{seq: job.seq, rel: job.rel, abs: job.abs}
				if cerr := ctx.Err(); cerr != nil {
					res.err = cerr
				} else {
					res.lines, res.count, res.total, res.capped, res.err =
						grepCountFile(ctx, re, sc, job.abs, job.rel, budget)
				}
				outs <- res
			}
		}()
	}
	go func() {
		wg.Wait()
		close(outs)
	}()

	// 遍历（串行、顺序确定）：产出任务。
	var stop atomic.Bool
	go func() {
		defer close(jobs)
		var seq uint64
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if cerr := ctx.Err(); cerr != nil {
				return cerr // 被取消/超时：立刻停止遍历（不再读任何文件）
			}
			if stop.Load() {
				return fs.SkipAll // 本页窗口已满：不再派新任务
			}
			if err != nil {
				return nil // 单个文件不可读（权限等）：跳过，不中断搜索
			}
			if d.IsDir() {
				if path != root && ign.ignoredDir(path) {
					return fs.SkipDir // 整棵子树剪掉：不遍历、不打开
				}
				return nil
			}
			if path != root && ign.ignoredFile(path) {
				return nil
			}
			if !d.Type().IsRegular() {
				return nil
			}
			rel, err := filepath.Rel(t.root, path)
			if err != nil {
				rel = path
			}
			rel = filepath.ToSlash(rel)
			// include 过滤：glob 匹配文件名（*.go）或完整相对路径（**/test/*.go）任一命中即可
			if a.Include != "" {
				baseMatch, _ := matchGlob(a.Include, filepath.Base(rel))
				pathMatch, _ := matchGlob(a.Include, rel)
				if !baseMatch && !pathMatch {
					return nil
				}
			}
			jobs <- grepJob{seq: seq, rel: rel, abs: path}
			seq++
			return nil
		})
	}()

	// 收集：worker 是乱序返回的 ⇒ 按遍历序号排回来（pending 的大小被 jobs 缓冲限住）。
	pending := make(map[uint64]grepCountResult, workers*2)
	var next uint64
	var fatal error
	var csc *grepScanner // 采集器自己的读缓冲（懒分配）
	for res := range outs {
		if fatal != nil {
			continue // 已经出错：只把剩下的结果排空，避免 worker 卡住
		}
		pending[res.seq] = res
		for {
			p, ok := pending[next]
			if !ok {
				break
			}
			delete(pending, next)
			next++
			if p.err != nil {
				if ctx.Err() != nil {
					fatal = searchCancelledError(ctx, "grep")
					stop.Store(true)
				}
				continue
			}
			req := c.addFile(p)
			if c.truncated {
				stop.Store(true) // 本页满了：让遍历停下（已在途的任务读掉就读掉）
			}
			if req == nil {
				continue
			}
			if csc == nil {
				csc = newGrepScanner()
			}
			fr, err := grepCollectFile(ctx, csc, req.abs, req.rel, req.lines, a.Context, req.total)
			if err != nil {
				if ctx.Err() != nil {
					fatal = searchCancelledError(ctx, "grep")
					stop.Store(true)
				}
				continue
			}
			c.results = append(c.results, fr)
		}
	}
	if fatal == nil && ctx.Err() != nil {
		// 遍历/worker 都没来得及报取消（例如恰好在最后一个任务完成时被取消）
		fatal = searchCancelledError(ctx, "grep")
	}
	return fatal
}
