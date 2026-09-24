// Package sandbox 命令执行沙箱抽象：bash 工具的执行后端。
//
// 设计：Sandbox 是 bash 工具的可注入执行后端，工具本身不知道自己在沙箱里。
// nil（未注入）= NoSandbox —— 进程隔离直通执行（默认形态）：
// 独立进程组 + 最小环境白名单 + 超时杀整组，命令无法影响 agent 主进程；
// 安全实现（bwrap/firejail/Docker）在 NoSandbox 之上叠加
// 网络/目录/资源策略（全部收敛在 Run 内部）。
package sandbox

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ExecSpec 一次命令执行规格。
type ExecSpec struct {
	Command string        // 原始命令行（由实现决定如何执行，如 sh -c）
	Cwd     string        // 工作目录
	Timeout time.Duration // 建议时限（<= 0 用实现默认）
	// ExitCode/TimedOut/Canceled 可选出参：非 nil 时由实现回填命令结局（bash 工具用于把
	// 「失败/超时/取消」从纯文本升级为结构化信号）。nil = 不采集（既有调用方行为不变）。
	// Canceled 与 TimedOut 互斥：前者 = 父 ctx 被取消（用户中断/会话关闭），
	// 后者 = 时限到点。合并成一个标志会让用户中断被印成 [TIMEOUT …]（见 runExec）。
	ExitCode *int
	TimedOut *bool
	Canceled *bool
}

// Sandbox 沙箱执行后端。
type Sandbox interface {
	// Run 执行命令并返回合并输出（stdout+stderr）。
	// 语义约定（与 bash 工具一致）：非零退出/超时不视为错误返回 ——
	// 输出与原因并入返回文本，模型可见后自行修正。
	Run(ctx context.Context, spec ExecSpec) (string, error)
}

// NoSandbox 进程隔离直通执行（默认形态）。
//
// 隔离措施（防命令影响 agent 主进程）：
//   - 独立进程组（Setpgid）：超时/取消时 kill 整个进程组 —— 命令 fork 的
//     孙进程一并清理，不留残留；命令信号也无法穿透到主进程
//   - 最小环境白名单（PATH/HOME/代理）：agent 进程的密钥/token 等
//     环境变量不泄漏给命令
//   - 命令在独立子进程中运行（exec 天然隔离）：主进程崩溃不受命令影响
type NoSandbox struct{}

// Run 执行 sh -c 命令（进程隔离语义：非零退出并入文本返回）。
func (NoSandbox) Run(ctx context.Context, spec ExecSpec) (string, error) {
	cmd := exec.Command("sh", "-c", spec.Command)
	cmd.Dir = spec.Cwd
	cmd.Env = isolatedEnv()
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 独立进程组
	return runExec(ctx, cmd, spec)
}

// backendSelector 运行期按选择器切换执行后端：会话状态（如权限模式）动态变化时
// 无需重建工具/loop，每次 Run 重新选择。picker 必须并发安全（多会话共享同名 loop
// 的后端时可能被并行调用）。
type backendSelector struct {
	pick func() Sandbox
}

// Run 每次调用经 pick 选择后端执行；pick 返回 nil → NoSandbox 直通兜底。
func (b backendSelector) Run(ctx context.Context, spec ExecSpec) (string, error) {
	sb := b.pick()
	if sb == nil {
		sb = NoSandbox{}
	}
	return sb.Run(ctx, spec)
}

// WithBackendSelector 按运行期选择器包装 Sandbox（如按会话权限模式切换
// 沙箱/直通）。pick 每次 Run 调用且需并发安全。
func WithBackendSelector(pick func() Sandbox) Sandbox {
	return backendSelector{pick: pick}
}

// 取消/超时路径的时间预算（C8，2026-09-18）。为什么这三个常量缺一不可 —— 事故机制：
// cmd.Stdout/cmd.Stderr 是 io.Writer（&out）⇒ os/exec 会建 os.Pipe + 拷贝协程，Wait()
// 只有在**管道写端全部关闭**后才返回（栈顶 os/exec.(*Cmd).awaitGoroutines ← Wait）。
// 而 kill(-pgid) 会漏掉与 kill 并发的 fork 子进程（逃逸机制见
// seatbelt_cancel_darwin_test.go 中 killGroupByTag 的说明）：逃逸者握着写端 ⇒
// Wait() 永不返回 ⇒ 旧代码的 `<-done` 无限阻塞 ⇒ runExec 不返回。实测挂起 602s，
// 且挂住的正是超时/取消路径 —— 上层超时救不了（上层也在等 runExec 返回）。
//
//	waitDelayAfterKill = 3s
//	  子进程退出后管道仍开着时，只再等这么久，之后由 Go 强制关掉管道让 Wait() 返回
//	  （Go 1.20+ 的 Cmd.WaitDelay）。这是「管道被逃逸后代占着」的**根治**手段：把旧
//	  行为「无限期等管道 EOF」换成「有界等待 + 截断」。注意其计时器起算点是
//	  「Wait 观察到子进程已退出」或「绑定的 ctx 结束」；本包的 cmd 由 exec.Command
//	  创建（未绑 ctx）⇒ 子进程自己**不退出**时 WaitDelay 不生效 —— 所以下面两层预算
//	  是必需的兜底，不是冗余。
//	killWaitBudget = 4s
//	  首次 kill(-pgid) 之后等命令结束的预算（> WaitDelay：正常有逃逸者时 WaitDelay
//	  先到、命令在预算内结束、不触发升级；无逃逸者时命令立刻结束，取消路径不额外变慢）。
//	killEscalationWait = 2s
//	  升级（再杀组 + 杀 pid）后**最后**一个预算，用完即带部分输出返回。
//
// 不变量（本文件唯一的硬保证）：runExec 必定在
// timeout + killWaitBudget + killEscalationWait 内返回；正常取消/超时的实际返回时间
// ≈ timeout + WaitDelay（有逃逸者持管道）或 ≈ timeout（无逃逸者）。
const (
	waitDelayAfterKill = 3 * time.Second
	killWaitBudget     = 4 * time.Second
	killEscalationWait = 2 * time.Second
	// killFailedMark 极端分支（升级后命令仍未结束）在返回文本里的专属标记：调用方/审计
	// 据此区分「已杀干净」与「可能还有逃逸后代在跑，返回的是部分输出」。
	killFailedMark = "[KILL FAILED — descendant may still be running]"
)

// runExec 命令执行公共骨架（NoSandbox / Seatbelt 共用）：独立进程组 + 超时/取消
// 杀整组（含孙进程），非零退出/超时原因并入返回文本不视为错误（bash 工具语义）。
// 命令结局（退出码/超时/取消）经 spec.ExitCode/spec.TimedOut/spec.Canceled 可选回填
// （nil = 不采集）。
// 时限语义（别让调用方自叠快照定时器——promote 后台任务要求「动态解除时限」）：
//   - timeout < 0 → 不限时：仅随 ctx 取消（bash 命令被 promote 为后台后不设时限）；
//   - timeout == 0 且 ctx 已有 deadline → 跟随 ctx（引擎 liftableTimeout 管理，
//     中途 Lift 后 deadline 消失、Done 不再因时限关闭 → 命令自然跑完）；
//   - 否则以 timeout 兜底（<=0 → 默认 30s）。
func runExec(ctx context.Context, cmd *exec.Cmd, spec ExecSpec) (string, error) {
	timeout := spec.Timeout
	var cctx context.Context
	var cancel context.CancelFunc
	switch {
	case timeout < 0:
		cctx, cancel = context.WithCancel(ctx) // 不限时：随显式取消/会话 ctx
	case timeout == 0:
		if _, ok := ctx.Deadline(); ok {
			cctx = ctx // 调用方已设时限：跟随 ctx（lift 可动态解除）
		} else {
			timeout = 30 * time.Second
			cctx, cancel = context.WithTimeout(ctx, timeout)
		}
	default:
		cctx, cancel = context.WithTimeout(ctx, timeout)
	}
	if cancel != nil {
		defer cancel()
	}

	// 输出走 lockedBuffer 而不是裸 bytes.Buffer：极端分支会在 cmd.Wait 的拷贝协程仍
	// 在往管道读/写时取部分输出，无锁会构成数据竞争（见 lockedBuffer 注释）。
	var out lockedBuffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Start(); err != nil {
		return "", err
	}
	// 管道挂起的根治（见上面常量注释的 waitDelayAfterKill）：Start 成功后设置。
	cmd.WaitDelay = waitDelayAfterKill

	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	select {
	case err := <-done:
		// ErrWaitDelay：进程**已成功退出**，只是输出管道被进程组外的后代占着（Go 仅在
		// 进程成功退出时返回它，见 os/exec.Cmd.Wait 文档）。旧行为是无限期等管道 EOF
		// —— 那正是本事故机制的一半；现在改成「有界等待 + 明说输出可能被截断」：
		// 不得把成功命令印成失败（ExitCode 仍取真实的 0）。
		if errors.Is(err, exec.ErrWaitDelay) {
			if spec.ExitCode != nil {
				*spec.ExitCode = 0
			}
			return fmt.Sprintf("%s\n[exit: 0；%v —— 输出可能被进程组外的后代截断]", strings.TrimSpace(out.String()), err), nil
		}
		if err != nil {
			// 非零退出：输出与原因并入文本（模型可见后自行修正）。
			// 结局回填：*exec.ExitError 给出真实退出码；被信号杀死时 ExitCode() == -1，
			// 照实回填（调用方据此判定失败）。
			if spec.ExitCode != nil {
				code := -1
				var ee *exec.ExitError
				if errors.As(err, &ee) && ee.ProcessState != nil {
					code = ee.ProcessState.ExitCode()
				}
				*spec.ExitCode = code
			}
			return fmt.Sprintf("%s\n[exit: %v]", strings.TrimSpace(out.String()), err), nil
		}
		if spec.ExitCode != nil {
			*spec.ExitCode = 0
		}
		return out.String(), nil
	case <-cctx.Done():
		// 超时/取消：杀整个进程组（含孙进程），再等待收集输出。
		// 结局回填：TimedOut/Canceled + ExitCode=-1 —— 返回的是**部分输出**且命令未完成，
		// 调用方必须据此把它判为失败（否则模型会把截断的中间结果当完整结果）。
		// 超时与取消必须分开（cctx.Err()）：context.Canceled = 父 ctx 被取消
		//（用户中断 / 会话关闭 / 后台任务被中断）；DeadlineExceeded = 时限到点。
		// 两者对模型是不同的事实（前者不是命令的问题，也不该建议「调大 timeout」）。
		// 注意 timeout < 0（后台不限时）时 cctx 由 context.WithCancel 派生 → 取消即
		// Canceled；ctx 自身带 deadline 的情形仍归 DeadlineExceeded。
		//
		// 有界性（唯一不变量）：本分支必定在 killWaitBudget + killEscalationWait 内返回，
		// 即 runExec 最坏返回时间 = timeout + 4s + 2s（见文件头常量注释）。旧代码在这里
		// `_ = syscall.Kill(...)` 吞掉杀进程错误、紧接着 `<-done` **无任何上界** ——
		// 逃逸后代持着管道写端时 Wait 永不返回 ⇒ runExec 永久挂起（实测 602s）。
		firstErr := killProcGroup(cmd.Process.Pid)
		o := awaitExit(done, killWaitBudget, killEscalationWait, func() error {
			// 升级：再杀一次组（单次 kill(-pgid) 会漏掉与 kill 并发的 fork 子进程，也
			// 漏掉已换组的后代）+ 直接杀 pid。
			return escalateKill(cmd.Process.Pid, cmd.Process)
		})
		note := ""
		if o.Escalated {
			note += fmt.Sprintf("\n[kill: 杀组后 %v 命令仍未结束 → 已升级（再杀组 + 杀 pid）]", killWaitBudget)
		}
		if o.Failed {
			note += "\n" + killFailedMark + "：已带部分输出返回，不在此处等待"
		}
		if why := killErrNote(firstErr, o.EscErr); why != "" {
			note += " " + why
		}
		if spec.Canceled != nil {
			*spec.Canceled = cctx.Err() == context.Canceled
		}
		if spec.TimedOut != nil {
			*spec.TimedOut = cctx.Err() == context.DeadlineExceeded
		}
		if spec.ExitCode != nil {
			*spec.ExitCode = -1
		}
		return fmt.Sprintf("%s\n[exit: %v]%s", strings.TrimSpace(out.String()), cctx.Err(), note), nil
	}
}

// killProcGroup 杀命令的整个进程组。包级变量 = 测试注入点（kill_bound_test.go 的
// 「组杀失败 → 升级」用例）；生产路径恒为 killGroup。
var killProcGroup = killGroup

// killGroup 杀 pid 所在进程组：先 kill(-pid, SIGKILL)（整组，含命令 fork 的孙进程），
// 失败再回退 kill(pid, SIGKILL)（pgid 已不存在 / 该 pid 不是组长这类 ESRCH 情形的兜底）。
// 返回 nil = 至少一刀投递成功；非 nil = 目标可能还活着，错误里带两刀各自的真实原因
// （errors.Is(err, syscall.ESRCH) 可判「本来就不存在」）。旧代码是 `_ =` 吞错：杀失败
// 无人知晓，紧接着就是无限等 Wait —— 禁止再出现吞错。
func killGroup(pid int) error { return killGroupWith(syscall.Kill, pid) }

// killGroupWith killGroup 的可注入形态（kill 生产值 = syscall.Kill）：纯函数，便于
// 不依赖真实进程验证「组杀失败 → 回退杀 pid」的升级路径。
func killGroupWith(kill func(pid int, sig syscall.Signal) error, pid int) error {
	if pid <= 0 {
		return fmt.Errorf("sandbox: 拒绝杀非法 pid %d", pid)
	}
	gerr := kill(-pid, syscall.SIGKILL)
	if gerr == nil {
		return nil
	}
	if perr := kill(pid, syscall.SIGKILL); perr != nil {
		return fmt.Errorf("kill(-%d): %w; kill(%d): %w", pid, gerr, pid, perr)
	}
	return nil
}

// killOutcome awaitExit 的结果。
type killOutcome struct {
	Escalated bool  // 首次预算内没等到命令结束 → 走过升级分支
	Failed    bool  // 升级后仍未结束：调用方必须带部分输出返回（不得再等）
	EscErr    error // 升级分支杀进程的错误（nil = 无错或只有「已不存在」这类无害错）
}

// awaitExit 用**有界**时间等命令结束（done = cmd.Wait 的结果通道）：budget 内就绪 →
// 直接返回；否则调 escalate()（升级杀进程）后再等 grant；仍不就绪 → Failed=true，由
// 调用方带部分输出返回。
// 不变量：本函数必定在 budget + grant 内返回 —— 旧代码在对应位置 `<-done` 没有任何
// 上界，这正是 2026-09-18 那次 602s 挂起的直接落点。
func awaitExit(done <-chan error, budget, grant time.Duration, escalate func() error) killOutcome {
	select {
	case <-done:
		return killOutcome{}
	case <-time.After(budget):
	}
	escErr := escalate()
	select {
	case <-done:
		return killOutcome{Escalated: true, EscErr: escErr}
	case <-time.After(grant):
		return killOutcome{Escalated: true, Failed: true, EscErr: escErr}
	}
}

// escalateKill 升级杀法：再杀一次进程组 + 直接杀 pid（组杀会漏掉与 kill 并发的 fork
// 子进程，也会漏掉已 setsid 换组的后代）。「组/进程已不存在」不算错误（命令可能刚好在
// 这之间自己退出了）—— 但其余错误一律上报，不吞。
func escalateKill(pid int, proc *os.Process) error {
	var errs []error
	if err := killProcGroup(pid); err != nil && !errors.Is(err, syscall.ESRCH) {
		errs = append(errs, err)
	}
	if err := proc.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		errs = append(errs, err)
	}
	return errors.Join(errs...)
}

// killErrNote 把杀进程失败的原因格式化成一行可审计文本；「已不存在/已回收」不算失败
// （返回空串）—— 那只是命令恰好自己退出了，不是杀失败。
func killErrNote(errs ...error) string {
	var msgs []string
	for _, err := range errs {
		if err == nil || errors.Is(err, syscall.ESRCH) || errors.Is(err, os.ErrProcessDone) {
			continue
		}
		msgs = append(msgs, err.Error())
	}
	if len(msgs) == 0 {
		return ""
	}
	return "(" + strings.Join(msgs, "; ") + ")"
}

// lockedBuffer 并发安全的输出缓冲。为什么需要锁：极端分支（升级后命令仍未结束）会在
// cmd.Wait 的拷贝协程**仍在读写输出管道**的同时取部分输出 —— 裸 bytes.Buffer 与那个
// 协程构成数据竞争（-race 会报，而"返回部分输出"是本包承诺的契约）。
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

// Write 实现 io.Writer（os/exec 的拷贝协程调用）。
func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String 取当前已收集的输出（可能是部分输出）。
func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// isolatedEnv 最小环境白名单：PATH/HOME/LANG/TERM/TMPDIR + 代理变量。
// agent 进程的全部其他环境变量（可能含密钥/token）不传给命令。
func isolatedEnv() []string {
	env := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + os.Getenv("HOME"),
		"LANG=C.UTF-8",
		"TERM=dumb",
		// 显式 TMPDIR=/tmp（沙箱内外统一）：无 TMPDIR 时 mktemp/go build/python tempfile
		// 等无法生产临时文件（2026-08-17 用户反馈"无法访问 /tmp"）。也不继承 agent 的
		// TMPDIR —— macOS 默认 /var/folders/... 在 Seatbelt 沙箱内未放行写 → EPERM。
		// /tmp 在 NoSandbox 全局可写、Seatbelt 已放行 file-write*（见 seatbelt_darwin.go）。
		"TMPDIR=/tmp",
	}
	for _, k := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
		if v := os.Getenv(k); v != "" {
			env = append(env, k+"="+v)
		}
	}
	return env
}
