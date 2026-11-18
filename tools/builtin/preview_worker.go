package builtin

// 预览转换的**常驻 worker**（第 4 轮爬山第 8 步）。
//
// 问题：一次性执行模型下，每次预览都要起一个 python 进程并重新 import 依赖：
//
//	解释器启动 ~20ms + import openpyxl ~80ms CPU（实测 -X importtime：import openpyxl
//	累计 459ms wall，其中 writer 子模块 136ms —— 只读也要加载它，躲不掉）
//	/ mammoth ~40ms / python-pptx ~100ms
//
// 「连续预览多份文档」时这笔钱重复付（每份一次）。
//
// 现在：按**工作区**复用一个常驻进程（沙箱策略按工作区 ⇒ 进程也必须按工作区分，
// 否则一个进程的策略会罩住另一个工作区）。协议是**行协议**（一行一个 JSON）：
//
//	→ {"src": "<输入>", "dst": "<输出>"}
//	← {"ok": true, "log": "..."} 或 {"ok": false, "error": "..."}
//
// **兜底**：worker 起不来 / 协议出错 / 进程死了 ⇒ 调用方**自动退回一次性执行**
// （老路径原封不动）。预览绝不能因为「优化」而不能用。
//
// 内存代价：一个常驻 python 进程约 30–40 MB（openpyxl 已加载）。空闲 5 分钟即回收
// （见 reapIdle），避免「打开过很多工作区」的桌面端一直挂着几十个进程。

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/seven7628/hai-harness/runtime"
	"github.com/seven7628/hai-harness/sandbox"
)

// previewWorkerBootstrap 常驻转换 worker 的引导脚本。
//
// 它做三件事：① 以**模块**方式加载转换脚本（转换脚本因此不能在自己 import 时就跑 ——
// 所以脚本末尾改成 `if __name__ == "__main__": main()`）；② 循环读行协议；
// ③ 把转换脚本自己的 stdout/stderr 收进缓冲，一起回给宿主（否则脚本的成功标记行
// 会污染协议流，而错误文案又会丢给用户看不见的地方）。
const previewWorkerBootstrap = `# -*- coding: utf-8 -*-
"""预览转换的常驻 worker（协议：一行一个 JSON）。

用法: python preview_worker.py <转换脚本.py>

  → {"src": "<输入路径>", "dst": "<输出路径>"}
  ← {"ok": true, "log": "<脚本输出>"} 或 {"ok": false, "error": "<原因>"}

stdout 是**协议通道**：转换脚本自己的 print/traceback 都被收进 log 字段，
以免污染协议流（也免得宿主把标记行当协议解析）。
"""
import contextlib
import importlib.util
import io
import json
import sys
import traceback


def load(path):
    spec = importlib.util.spec_from_file_location("preview_convert", path)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def main():
    if len(sys.argv) < 2:
        sys.stderr.write("用法: preview_worker.py <转换脚本>\n")
        return 2
    mod = load(sys.argv[1])
    out = sys.stdout
    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            req = json.loads(line)
            src, dst = req["src"], req["dst"]
        except Exception as exc:
            out.write(json.dumps({"ok": False, "error": "请求解析失败: %s" % exc},
                                 ensure_ascii=False) + "\n")
            out.flush()
            continue
        buf = io.StringIO()
        try:
            with contextlib.redirect_stdout(buf), contextlib.redirect_stderr(buf):
                mod.main([src, dst])
            resp = {"ok": True, "log": buf.getvalue()[-8000:]}
        except SystemExit as exc:
            # 脚本用 sys.exit(2) 表示「用户可理解的错误」⇒ 原因在 buf 里（中文单行）
            code = exc.code if isinstance(exc.code, int) else 1
            reason = buf.getvalue().strip() or ("转换脚本退出码 %s" % code)
            resp = {"ok": False, "error": reason[-8000:]}
        except BaseException:
            resp = {"ok": False, "error": (buf.getvalue() + traceback.format_exc())[-8000:]}
        out.write(json.dumps(resp, ensure_ascii=False) + "\n")
        out.flush()
    return 0


if __name__ == "__main__":
    sys.exit(main())
`

// previewWorkerIdleTimeout 空闲多久回收常驻进程（省内存）。
const previewWorkerIdleTimeout = 5 * time.Minute

// previewWorkerReapInterval 回收巡检间隔。
const previewWorkerReapInterval = time.Minute

// previewWorkerGlobalMax 全局常驻进程上限（xlsx/docx/pptx 三个池**合计**）。
//
// 为什么要有（第 5 轮爬山第 4 步，实测）：三个 previewer 都是 manager 级单例、
// 共用「按工作区」的池，而**工作区数没有上限** ⇒ 常驻进程数 = 3 × 工作区数，
// 每个约 33MB（实测：常驻 python 进程 RSS 24–34MB）⇒ 10 个工作区就是 **1GB RSS**。
// 这是内置工具层**唯一真正吃 swap 的大头**（Go 侧的堆是干净的：实测 12 轮大输出
// 后活堆 0.3MB → 0.3MB）。
//
// 4 个 = 约 130MB 上限：桌面端同时活跃的工作区是个位数，预览只在用户点开产出时
// 建 worker ⇒ 4 个够用；超了按 LRU 淘汰最久没用的那个（腾出位置给它）。
var previewWorkerGlobalMax = 4

// previewWorkerRegistry 全局登记所有常驻转换进程：预算 + LRU 淘汰共用。
//
// 为什么是**全局**而不是每个池各限各的：三个池互相独立 ⇒ 各限各的话总进程数
// 仍是 3 倍 ⇒ 限不住。必须有一个跨池的账本。
type previewWorkerRegistry struct {
	mu      sync.Mutex
	workers map[*previewWorker]struct{}
}

// previewWorkers 全局账本（进程级单例）。
//
// 注意：上限**不在这里存一份**（那样改策略 var 不会生效 —— 上限是策略，不是账本
// 自己的状态）：admit() 直接读 previewWorkerGlobalMax。
var previewWorkers = &previewWorkerRegistry{
	workers: map[*previewWorker]struct{}{},
}

// admit 为新 worker 腾位置：没超预算 ⇒ true；超了 ⇒ 淘汰**全局最久没用且空闲**的
// 那一个（LRU）。返回 false = 挤不出位置（所有 worker 都在忙）⇒ 调用方应退回
// 一次性执行（慢一点，但不占常驻内存）。
func (r *previewWorkerRegistry) admit() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.workers) < previewWorkerGlobalMax {
		return true
	}
	var victim *previewWorker
	for w := range r.workers {
		if w.inUse() {
			continue // 正在转换 ⇒ 不能杀（杀了这次转换就失败）
		}
		if victim == nil || w.lastUse().Before(victim.lastUse()) {
			victim = w
		}
	}
	if victim == nil {
		return false // 全在忙 ⇒ 挤不进来
	}
	if !victim.markDying() {
		return false // 淘汰窗口内它开始跑了 ⇒ 放弃（宁可不换也不要打断正在跑的转换）
	}
	// 关进程 + 从账本里摘掉。属主池下次用到它会发现 broken ⇒ 自动重建。
	victim.close()
	delete(r.workers, victim)
	return true
}

// add 登记一个新 worker。
func (r *previewWorkerRegistry) add(w *previewWorker) {
	r.mu.Lock()
	r.workers[w] = struct{}{}
	r.mu.Unlock()
}

// remove 从账本里摘掉一个 worker（它被关掉了）。
func (r *previewWorkerRegistry) remove(w *previewWorker) {
	r.mu.Lock()
	delete(r.workers, w)
	r.mu.Unlock()
}

// count 当前登记的常驻进程数（诊断用）。
func (r *previewWorkerRegistry) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.workers)
}

// previewWorkerPool 按工作区复用常驻转换进程。
type previewWorkerPool struct {
	py     PythonRuntime
	sbxFor SandboxFor
	script string // 转换脚本（编译期常量）

	// starts 常驻进程启动次数（诊断 + 回归网：连续转换应当只起一次）。
	starts atomic.Int64

	mu         sync.Mutex
	workers    map[string]*previewWorker // key = 工作区
	scriptPath string                    // 转换脚本落盘路径（写一次）
	bootPath   string                    // 引导脚本落盘路径（写一次）
	scriptDir  string                    // 两个脚本所在目录
	reapOnce   sync.Once
	closed     bool          // 池已关闭（Close 之后不再起进程）
	closeCh    chan struct{} // 停回收巡检用
}

// newPreviewWorkerPool 构造常驻 worker 池（script/marker 与一次性路径同一份）。
func newPreviewWorkerPool(py PythonRuntime, sbxFor SandboxFor, script string) *previewWorkerPool {
	return &previewWorkerPool{
		py:      py,
		sbxFor:  sbxFor,
		script:  script,
		workers: map[string]*previewWorker{},
		closeCh: make(chan struct{}),
	}
}

// convert 走常驻 worker 做一次转换；返回脚本输出（成功标记在里面）。
//
// 出错时调用方应**退回一次性执行**（本函数不自己兜底 —— 兜底需要调用方的沙箱与
// 临时目录上下文）。
func (p *previewWorkerPool) convert(ctx context.Context, workspace, src, dst string) (string, error) {
	w, err := p.worker(ctx, workspace)
	if err != nil {
		return "", err
	}
	return w.run(ctx, src, dst)
}

// worker 取（必要时起）某工作区的常驻进程。
func (p *previewWorkerPool) worker(ctx context.Context, workspace string) (*previewWorker, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return nil, fmt.Errorf("预览转换池已关闭")
	}
	if w, ok := p.workers[workspace]; ok {
		if !w.broken.Load() {
			w.touch()
			p.startReaperLocked()
			return w, nil
		}
		// 坏掉的（崩了/协议错位）：关掉重起
		w.close()
		previewWorkers.remove(w)
		delete(p.workers, workspace)
	}
	// 全局预算：超了就先淘汰最久没用的空闲 worker（第 5 轮爬山第 4 步）。
	// 挤不出位置（都在忙）⇒ 报错 ⇒ 调用方退回一次性执行。
	if !previewWorkers.admit() {
		return nil, fmt.Errorf("常驻转换进程已达全局上限（%d 个，都在忙）", previewWorkerGlobalMax)
	}
	w, err := p.start(ctx, workspace)
	if err != nil {
		return nil, err
	}
	p.workers[workspace] = w
	previewWorkers.add(w)
	p.startReaperLocked()
	return w, nil
}

// start 起一个常驻进程（受管解释器 + 引导脚本 + 转换脚本）。
func (p *previewWorkerPool) start(ctx context.Context, workspace string) (*previewWorker, error) {
	py, err := p.py.Ensure(ctx)
	if err != nil {
		return nil, fmt.Errorf("受管运行时不可用: %w", err)
	}
	var sbx sandbox.Sandbox = sandbox.NoSandbox{}
	if p.sbxFor != nil {
		if s := p.sbxFor(workspace); s != nil {
			sbx = s
		}
	}
	// **先**确认后端支持常驻进程，再落盘脚本：否则「不支持」这条路上会白留一个
	// 脚本目录（失败路径不留残留是硬约定）。
	starter, ok := sbx.(sandbox.Starter)
	if !ok {
		return nil, fmt.Errorf("沙箱后端 %T 不支持常驻进程", sbx)
	}
	bootPath, scriptPath, dir, err := p.ensureScripts(py)
	if err != nil {
		return nil, err
	}
	cmdline := shQuote(py.VenvPython) + " " + shQuote(bootPath) + " " + shQuote(scriptPath)
	proc, err := starter.Start(ctx, sandbox.ExecSpec{Command: cmdline, Cwd: dir})
	if err != nil {
		return nil, fmt.Errorf("起常驻转换进程失败: %w", err)
	}
	p.starts.Add(1)
	return newPreviewWorker(proc), nil
}

// newPreviewWorker 造一个常驻 worker。
//
// **构造即打点**：改造前 last=0（= 1970）⇒ 空闲回收器在第一次转换**之前**就会
// 把它当成「空闲 5 分钟」杀掉（新建后第一次转换可能被拒）。
func newPreviewWorker(proc sandbox.Process) *previewWorker {
	w := &previewWorker{proc: proc, reader: bufio.NewReader(proc.Stdout()), sem: make(chan struct{}, 1)}
	w.touch()
	return w
}

// ensureScripts 把引导脚本与转换脚本落盘（**整个池只写一次**）。
func (p *previewWorkerPool) ensureScripts(py runtime.Python) (bootPath, scriptPath, dir string, err error) {
	// 脚本目录在受管临时目录里 ⇒ 会被**按年龄**的清扫器回收（sweepStaleTempDirs）。
	// 已运行的 worker 不受影响（模块已加载进内存），但「进程死了要重启」时脚本得还在
	// ⇒ 用前确认一次，不在就重写。
	if p.bootPath != "" {
		if fileExists(p.bootPath) && fileExists(p.scriptPath) {
			return p.bootPath, p.scriptPath, p.scriptDir, nil
		}
		p.bootPath, p.scriptPath, p.scriptDir = "", "", ""
	}
	bootPath, cleanup, err := writeTempFile(py, "preview-worker-", "worker.py", []byte(previewWorkerBootstrap))
	if err != nil {
		return "", "", "", err
	}
	dir = filepath.Dir(bootPath)
	scriptPath = filepath.Join(dir, "convert.py")
	if err := os.WriteFile(scriptPath, []byte(p.script), 0o600); err != nil {
		cleanup()
		return "", "", "", err
	}
	p.bootPath = bootPath
	p.scriptPath = scriptPath
	p.scriptDir = dir
	return bootPath, scriptPath, dir, nil
}

// fileExists 文件在否（用于「脚本被按年龄清扫掉 ⇒ 重写」）。
func fileExists(path string) bool {
	fi, err := os.Stat(path)
	return err == nil && !fi.IsDir()
}

// startReaperLocked 起空闲回收巡检（每个池只起一次；调用方须持 p.mu）。
//
// 巡检协程会随池关闭而退出（closeCh）—— 改造前是 `for range t.C` **永不退出**，
// 池被丢弃（槽位淘汰）时这个协程连同 ticker 一起泄漏。
func (p *previewWorkerPool) startReaperLocked() {
	p.reapOnce.Do(func() {
		go func() {
			t := time.NewTicker(previewWorkerReapInterval)
			defer t.Stop()
			for {
				select {
				case <-t.C:
					p.reapIdle(time.Now())
				case <-p.closeCh:
					return
				}
			}
		}()
	})
}

// Close 关池：停回收巡检 + 关掉所有常驻进程（幂等）。
//
// 谁会调：预览槽位被淘汰时（workspace_slots.go 的槽位上限 32）。manager 级单例
// 不用调（它们跟 bridge 同寿）。不关的话，这些 worker 要等 5 分钟空闲回收才消失
// —— 槽位反复换工作区时，就是一批进程在内存里排队等死。
func (p *previewWorkerPool) Close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	workers := p.workers
	p.workers = map[string]*previewWorker{}
	p.mu.Unlock()

	for _, w := range workers {
		w.close()
		previewWorkers.remove(w)
	}
	close(p.closeCh)
}

// reapIdle 回收空闲超时的常驻进程。
//
// **正在转换的不回收**（改造前的缺陷：一个跑了 6 分钟的转换会被当成「空闲 5 分钟」
// 杀掉 ⇒ 转换凭空失败）。
func (p *previewWorkerPool) reapIdle(now time.Time) {
	p.mu.Lock()
	var victims []*previewWorker
	for key, w := range p.workers {
		if w.inUse() {
			continue // 正在转换 ⇒ 不动
		}
		if now.Sub(w.lastUse()) > previewWorkerIdleTimeout {
			victims = append(victims, w)
			delete(p.workers, key)
		}
	}
	p.mu.Unlock()
	// 关进程放在锁外（kill+wait 不该占着池锁）。
	for _, w := range victims {
		w.close()
		previewWorkers.remove(w)
	}
}

// previewWorker 一个常驻转换进程（一次只跑一个转换：协议是串行的）。
type previewWorker struct {
	proc   sandbox.Process
	reader *bufio.Reader

	// sem 容量 1 的信号量（**不是 mutex**）：排队要能被 ctx 打断 ——
	// mutex.Lock 不可取消 ⇒ 用户中断后，那个排队的转换还要干等前一个跑完
	// （前一个可能还在跑几十秒）。信号量的 select 天然可取消。
	sem    chan struct{}
	broken atomic.Bool
	last   atomic.Int64 // unix nano

	// mu 保护 busy/dying 两个标志的切换（不是保护数据，是保护**状态迁移**）。
	//
	// busy：正在跑转换。**正在跑的 worker 不能被回收**（杀了它，转换就失败）——
	// 改造前回收器只看「多久没用」，于是一个跑了 6 分钟的转换会被当成
	// 「空闲 5 分钟」杀掉（第 5 轮爬山第 4 步修的既有缺陷）。
	// dying：正在被淘汰（全局预算挤掉）。run 看到它就直接放弃 ⇒ 调用方退回
	// 一次性执行，而不是把一个刚被杀的进程用下去。
	mu    sync.Mutex
	busy  bool
	dying bool
}

// inUse 正在跑转换吗（回收器据此跳过它）。
func (w *previewWorker) inUse() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.busy
}

// beginRun 声明「开始跑一次转换」；返回 false = 这个 worker 正在被淘汰，别用了。
func (w *previewWorker) beginRun() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.dying {
		return false
	}
	w.busy = true
	return true
}

// endRun 声明「这次转换结束」。
func (w *previewWorker) endRun() {
	w.mu.Lock()
	w.busy = false
	w.mu.Unlock()
}

// markDying 标记「正在被淘汰」并返回「现在是否可以关它」（忙 ⇒ 不行）。
func (w *previewWorker) markDying() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.busy {
		return false
	}
	w.dying = true
	return true
}

func (w *previewWorker) touch() { w.last.Store(time.Now().UnixNano()) }

func (w *previewWorker) lastUse() time.Time { return time.Unix(0, w.last.Load()) }

// run 发一次转换请求并等结果。
func (w *previewWorker) run(ctx context.Context, src, dst string) (string, error) {
	// 排队可被 ctx 打断（见 sem 的注释）。
	select {
	case w.sem <- struct{}{}:
		defer func() { <-w.sem }()
	case <-ctx.Done():
		return "", ctx.Err()
	}
	if w.broken.Load() {
		return "", fmt.Errorf("常驻转换进程已失效")
	}
	// 占用标记：回收器与全局淘汰都据此跳过「正在转换」的 worker。
	if !w.beginRun() {
		return "", fmt.Errorf("常驻转换进程正在被淘汰")
	}
	defer w.endRun()
	w.touch()
	req, err := json.Marshal(map[string]string{"src": src, "dst": dst})
	if err != nil {
		return "", err
	}
	if _, err := w.proc.Stdin().Write(append(req, '\n')); err != nil {
		w.broken.Store(true)
		return "", fmt.Errorf("写常驻进程失败: %w", err)
	}
	// 读一行响应。ctx 取消 ⇒ 杀进程（这次转换的结果已不可信，且留着会挡住后续请求）。
	type readResult struct {
		line string
		err  error
	}
	ch := make(chan readResult, 1)
	go func() {
		line, err := w.reader.ReadString('\n')
		ch <- readResult{line: line, err: err}
	}()
	select {
	case <-ctx.Done():
		// 取消：关掉整个进程（杀进程组 + **关管道 + wait 回收**）。
		// 改造前只 Kill() ⇒ 管道 fd 不关、子进程不被 wait ⇒ **僵尸 + fd 泄漏**，
		// 最多滞留到空闲回收器接手（5 分钟）。
		w.broken.Store(true)
		w.close()
		return "", ctx.Err()
	case r := <-ch:
		if r.err != nil {
			w.broken.Store(true)
			return "", fmt.Errorf("读常驻进程失败: %w", r.err)
		}
		var resp struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
			Log   string `json:"log"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(r.line)), &resp); err != nil {
			w.broken.Store(true)
			return "", fmt.Errorf("常驻进程响应无法解析: %w", err)
		}
		if !resp.OK {
			return "", fmt.Errorf("%s", resp.Error)
		}
		return resp.Log, nil
	}
}

// close 关掉常驻进程（幂等）。
//
// nil-safe：没有进程时（测试里的假 worker、构造失败的残留）直接标记即可。
func (w *previewWorker) close() {
	w.broken.Store(true)
	if w.proc == nil {
		return
	}
	_ = w.proc.Close()
}
