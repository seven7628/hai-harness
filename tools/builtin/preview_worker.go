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
}

// newPreviewWorkerPool 构造常驻 worker 池（script/marker 与一次性路径同一份）。
func newPreviewWorkerPool(py PythonRuntime, sbxFor SandboxFor, script string) *previewWorkerPool {
	return &previewWorkerPool{
		py:      py,
		sbxFor:  sbxFor,
		script:  script,
		workers: map[string]*previewWorker{},
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
	if w, ok := p.workers[workspace]; ok {
		if !w.broken.Load() {
			w.touch()
			p.startReaperLocked()
			return w, nil
		}
		// 坏掉的（崩了/协议错位）：关掉重起
		w.close()
		delete(p.workers, workspace)
	}
	w, err := p.start(ctx, workspace)
	if err != nil {
		return nil, err
	}
	p.workers[workspace] = w
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
	return &previewWorker{proc: proc, reader: bufio.NewReader(proc.Stdout()), sem: make(chan struct{}, 1)}, nil
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
func (p *previewWorkerPool) startReaperLocked() {
	p.reapOnce.Do(func() {
		go func() {
			t := time.NewTicker(previewWorkerReapInterval)
			defer t.Stop()
			for range t.C {
				p.reapIdle(time.Now())
			}
		}()
	})
}

// reapIdle 回收空闲超时的常驻进程。
func (p *previewWorkerPool) reapIdle(now time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for key, w := range p.workers {
		if now.Sub(w.lastUse()) > previewWorkerIdleTimeout {
			w.close()
			delete(p.workers, key)
		}
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
		w.broken.Store(true)
		_ = w.proc.Kill()
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
func (w *previewWorker) close() {
	w.broken.Store(true)
	_ = w.proc.Close()
}

// previewWorkerQueueCancellableDoc 放在文件末尾提醒：队列用信号量（可取消）而不是
// mutex（不可取消）—— 用户中断后，排队的预览不该干等前一个跑完。
