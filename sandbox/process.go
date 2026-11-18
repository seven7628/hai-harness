package sandbox

import (
	"context"
	"errors"
	"io"
	"os/exec"
	"sync"
	"syscall"
)

// errNoArgv 空 argv（内部错误：调用方拼错）。
var errNoArgv = errors.New("sandbox: 空 argv")

// Process 长期存活的沙箱进程（**协议由调用方定**，沙箱只负责「进程在策略里活着 +
// 管道能通」）。
//
// 为什么需要（第 4 轮爬山）：一次性 Run 的模型下，每次执行都要重新起一个进程、
// 重新 import 依赖。对「连续做很多次同类转换」的场景（办公文档预览：docx/xlsx/pptx
// 各一个常驻转换器）这是重复支出 ——
//
//	解释器启动 ~20ms + import openpyxl ~80ms CPU（实测 -X importtime：import openpyxl
//	累计 459ms wall，其中 writer 子模块 136ms —— 只读也要加载它，躲不掉）
//
// 建议协议：**行协议**（一行一个 JSON），最省事也最好调试。
//
// 生命周期由调用方负责：Close 收尾（关管道 + 杀进程组 + 等退出）。
type Process interface {
	// Stdin 子进程标准输入（写）。写完不关 ⇒ 子进程读到 EOF 才退出。
	Stdin() io.WriteCloser
	// Stdout 子进程标准输出（读）。
	//
	// 注意（os/exec 的规矩）：调用方**读完**之后才能 Wait —— Wait 会关掉管道。
	// 子进程的 stderr **不**在这里（默认继承父进程的 stderr ⇒ 诊断信息照常进日志，
	// 且不会污染协议流）。
	Stdout() io.ReadCloser
	// Wait 等进程退出并返回其错误（nil = 正常退出）。只生效一次。
	Wait() error
	// Kill 杀整个进程组（超时/取消/协议出错时用；幂等）。
	Kill() error
	// Close 收尾：关 stdin + 杀进程组 + 等退出（幂等）。
	Close() error
}

// Starter 可选能力：启动长期存活的进程。
//
// 为什么是**可选接口**而不是加进 Sandbox：Sandbox 只有 Run（一次性）是所有后端
// 都有的语义；长期进程目前只有 NoSandbox / Seatbelt 两种后端实现 ⇒ 用类型断言
// 探测，没有就退回一次性 Run（调用方永远有退路）。
type Starter interface {
	Start(ctx context.Context, spec ExecSpec) (Process, error)
}

// procProcess 长期进程的通用实现（NoSandbox / Seatbelt 共用：都是「argv + cwd + env」）。
type procProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser

	waitOnce sync.Once
	waitErr  error
	closeOne sync.Once
}

// startProcProcess 起一个长期存活的进程（argv 形式）。
//
// 进程组：Setpgid（与 Run 一致）⇒ 超时/取消时能整组杀干净，不留孙进程。
// stderr：继承父进程（不接管道）⇒ 子进程的诊断信息照常进日志，且不污染协议流。
func startProcProcess(ctx context.Context, argv []string, cwd string, env []string) (Process, error) {
	if len(argv) == 0 {
		return nil, errNoArgv
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	if env != nil {
		cmd.Env = env
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		return nil, err
	}
	// ctx 只管「起进程」这一刻：进程起来后由调用方自己管生命周期
	//（否则 ctx 一取消就把常驻进程杀了 —— 而常驻进程恰恰要活过单次调用）。
	_ = ctx
	return &procProcess{cmd: cmd, stdin: stdin, stdout: stdout}, nil
}

func (p *procProcess) Stdin() io.WriteCloser { return p.stdin }
func (p *procProcess) Stdout() io.ReadCloser { return p.stdout }

// Wait 等进程退出（只生效一次）。
func (p *procProcess) Wait() error {
	p.waitOnce.Do(func() { p.waitErr = p.cmd.Wait() })
	return p.waitErr
}

// Kill 杀整个进程组（幂等）。
func (p *procProcess) Kill() error {
	if p.cmd.Process == nil {
		return nil
	}
	// 负 pid = 杀进程组（与 runExec 的超时路径同口径：孙进程一并清理）。
	return syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
}

// Close 收尾：关 stdin（让子进程读到 EOF 正常退出）+ 杀进程组 + 等退出（幂等）。
func (p *procProcess) Close() error {
	var err error
	p.closeOne.Do(func() {
		_ = p.stdin.Close()
		_ = p.Kill()
		_ = p.Wait()
	})
	return err
}
