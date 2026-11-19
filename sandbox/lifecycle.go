package sandbox

// 执行/应用两级作用域的资源登记与收尾（2026-11-18「资源有界化」第 1 步）。
//
// 统一判据：**谁创建 / 谁承诺它还在 / 谁负责收**。本文件是「谁收」那一半：
//
//	执行作用域（一次命令）⇒ **进程组**：命令**正常结束**也要收组。
//	    改造前只有超时/取消路径收（killProcGroup）⇒ 一条 `cmd &` 成功返回后，
//	    它 fork 出来的后台进程就地变孤儿（PPID=1、继续烧 CPU 到天荒地老）。
//	    这正是本机 100 个孤儿 headless Chrome 的成因。
//	应用作用域（bridge 进程）⇒ **在飞的执行组 + 后台常驻进程**：应用关闭时一次性收。
//
// 三条硬约束（写在这里防以后有人改坏）：
//  1. **只杀我们自己的组**：kill(-pgid) 的 pgid 必须是本包起的进程组的组长 pid
//     （因 Setpgid，pgid == 子进程 pid）⇒ 绝不误伤宿主、别的会话或用户手动起的进程。
//  2. **不拖慢主路径**：收组是**异步**的（正常退出路径只投递信号就返回）⇒
//     工具返回时间不受宽限期影响（验收指标：杀组耗时 p99 < 200ms）。
//  3. **跨平台同行为**：只用 POSIX 原语（进程组 + kill 信号）⇒ macOS / Linux
//     行为一致（macOS 没有 PDEATHSIG / cgroup / subreaper，见调研文档 §3.2）。

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// reapGrace 收组时给「守规矩的进程」的收尾时间（SIGTERM → SIGKILL 之间）。
//
// 取 100ms 的理由：够 Chrome/Node 这类进程释放文件与端口（SIGTERM 处理器通常
// 在几十毫秒内退出），又不至于让残留进程多活一秒（验收指标：残留寿命 < 5s）。
// 宽限发生在**后台协程**里 ⇒ 工具返回零等待。
const reapGrace = 100 * time.Millisecond

// reapProcGroup 命令**正常结束**后收掉它留下的进程组（执行作用域收尾）。
//
// 语义（对齐 systemd 的 KillMode=control-group / 容器 stop 的默认行为）：
// 「命令结束 ⇒ 它留下的进程树一并结束」。改造前这条只在超时/取消路径存在，
// 于是「命令成功返回 + 留下后台进程」= 孤儿。
//
// 为什么先探活再起协程：正常退出（无残留）时组已空 ⇒ kill(-pgid, 0) 立刻 ESRCH
// ⇒ 直接返回，**不起后台协程**（每条命令都白起一个 100ms 的协程是不可接受的
// 无谓开销：100 cmd/s ⇒ 每秒 100 个协程）。
//
// 为什么 SIGTERM 后再 SIGKILL：SIGTERM 给进程机会收尾（释放端口/落盘），
// SIGKILL 保证「不守规矩的」一定消失（Playwright PR #41009 的同款思路：
// 优雅关闭要有硬上限）。
func reapProcGroup(pid int) {
	if pid <= 0 {
		return
	}
	// 探活：组已空（没有残留进程）⇒ 无事可做。
	if err := syscall.Kill(-pid, 0); err != nil {
		return // ESRCH = 组里没人了（正常路径的绝大多数情况）
	}
	go func() {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		time.Sleep(reapGrace)
		_ = syscall.Kill(-pid, syscall.SIGKILL)
	}()
}

// liveGroups 在飞的执行组登记表（全局）。
//
// 为什么需要：命令**还在跑**时它的进程组是活的；若宿主此刻退出（应用关闭），
// 那些进程就成了孤儿（PPID=1，继续烧 CPU）⇒ 关闭时必须收。
//
// 登记：runExec 起进程后；注销：runExec 收尾后（含超时/取消路径）。
var liveGroups = struct {
	mu   sync.Mutex
	pids map[int]struct{}
}{pids: map[int]struct{}{}}

// registerLiveGroup 登记一个在飞的执行组（pid = 组长 pid = pgid）。
func registerLiveGroup(pid int) {
	if pid <= 0 {
		return
	}
	liveGroups.mu.Lock()
	liveGroups.pids[pid] = struct{}{}
	liveGroups.mu.Unlock()
}

// unregisterLiveGroup 注销一个已收尾的执行组。
func unregisterLiveGroup(pid int) {
	liveGroups.mu.Lock()
	delete(liveGroups.pids, pid)
	liveGroups.mu.Unlock()
}

// killLiveGroups 收掉所有在飞的执行组（应用关闭时调用）。返回收掉的组数。
func killLiveGroups() int {
	liveGroups.mu.Lock()
	pids := make([]int, 0, len(liveGroups.pids))
	for pid := range liveGroups.pids {
		pids = append(pids, pid)
	}
	liveGroups.pids = map[int]struct{}{}
	liveGroups.mu.Unlock()
	for _, pid := range pids {
		// 组杀（含孙进程）+ 直接杀组长（组已空时的兜底）。
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	return len(pids)
}

// ---------- 后台常驻进程（run_in_background） ----------

// BackgroundProc 后台常驻进程的句柄。
type BackgroundProc interface {
	// Pid 进程 pid（模型可见：可用来查状态 / 停止它）。
	Pid() int
	// Kill 收掉它（连同它自己的进程组）。幂等。
	Kill() error
}

// BackgroundStarter 可选能力：起一个**后台常驻**进程并**立即返回**（不等它结束）。
//
// 语义（对齐 Claude Code 的 run_in_background）：
//   - 进程在**自己的进程组**里（Setpgid）⇒ 与调用方那次执行的组**无关**
//     ⇒ 调用方那次执行结束时的「收组」不会误杀它（这正是豁免的实现方式：
//     不靠标志位，靠**边界不同**）；
//   - stdout+stderr 合并写进 spec.BackgroundLog（日志文件）⇒ 模型随后可读；
//   - stdin 接 /dev/null ⇒ 不等输入，不会因模型没喂 stdin 而挂住；
//   - 进程退出后被 wait 回收 ⇒ **我们不制造僵尸**（验收指标：僵尸数 = 0）。
//
// 为什么是**可选接口**而不是加进 Sandbox：Sandbox 是所有后端都要实现的最小面；
// 后台能力只有 NoSandbox / Seatbelt 两种后端 ⇒ 类型断言探测，没有就明确报错
// （不静默降级成「同步跑」——那会让模型以为自己拿到后台句柄，其实是前台阻塞）。
type BackgroundStarter interface {
	StartBackground(ctx context.Context, spec ExecSpec) (BackgroundProc, error)
}

// errNoBackground 后端不支持后台运行。
var errNoBackground = errors.New("sandbox: 当前执行后端不支持后台运行")

// StartBackground 起一个后台常驻进程（NoSandbox 形态：进程隔离直通）。
func (NoSandbox) StartBackground(ctx context.Context, spec ExecSpec) (BackgroundProc, error) {
	return startBackgroundProcess(ctx, []string{"sh", "-c", spec.Command}, spec.Cwd, isolatedEnv(), spec.BackgroundLog)
}

// StartBackground 转发到当前后端（运行期按选择器切换后端时保持同一语义）。
//
// 后端不支持 ⇒ 明确报错（不静默降级）：模型拿到的必须是**真的**后台句柄。
func (b backendSelector) StartBackground(ctx context.Context, spec ExecSpec) (BackgroundProc, error) {
	sb := b.pick()
	if sb == nil {
		sb = NoSandbox{}
	}
	bs, ok := sb.(BackgroundStarter)
	if !ok {
		return nil, errNoBackground
	}
	return bs.StartBackground(ctx, spec)
}

// startBackgroundProcess 起后台进程的共用实现（NoSandbox / Seatbelt 共用：
// 都是「argv + cwd + env + 日志文件」）。
func startBackgroundProcess(ctx context.Context, argv []string, cwd string, env []string, logPath string) (BackgroundProc, error) {
	if len(argv) == 0 {
		return nil, errNoArgv
	}
	if logPath == "" {
		return nil, errors.New("sandbox: 后台进程必须有日志路径（BackgroundLog）")
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	if env != nil {
		cmd.Env = env
	}
	// stdin 不设 ⇒ os/exec 接 /dev/null（后台进程不等输入）。
	// stdout/stderr 直接指到日志文件（不是管道）⇒ 子进程直接写盘，无拷贝协程、
	// 无「管道被后代占住」的挂起面（runExec 的 3s WaitDelay 问题在后台路径不存在）。
	cmd.Stdout = f
	cmd.Stderr = f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // 自己的组 ⇒ 天然豁免本次执行的收组
	if err := cmd.Start(); err != nil {
		_ = f.Close()
		return nil, err
	}
	p := &bgProcess{cmd: cmd}
	bgRegistry.add(p)
	// 回收协程：进程退出后 wait（**不制造僵尸**）+ 关日志文件 + 注销。
	// ctx 只管「起进程」这一刻（与 startProcProcess 同口径）：后台进程要活过
	// 这次调用，甚至活过会话。
	_ = ctx
	go func() {
		_ = cmd.Wait()
		_ = f.Close()
		bgRegistry.remove(p)
	}()
	return p, nil
}

// bgProcess 后台进程句柄的实现。
type bgProcess struct {
	cmd  *exec.Cmd
	once sync.Once
}

func (p *bgProcess) Pid() int {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Kill 收掉后台进程（连同它自己的进程组）。幂等。
func (p *bgProcess) Kill() error {
	pid := p.Pid()
	if pid <= 0 {
		return nil
	}
	var err error
	p.once.Do(func() {
		_ = syscall.Kill(-pid, syscall.SIGTERM)
		// 宽限后强杀（后台进程可能忽略 SIGTERM）。
		go func() {
			time.Sleep(reapGrace)
			_ = syscall.Kill(-pid, syscall.SIGKILL)
		}()
		err = nil
	})
	return err
}

// bgRegistry 后台进程登记表（全局）：应用关闭时一次性收掉。
type bgRegistryT struct {
	mu    sync.Mutex
	procs map[*bgProcess]struct{}
}

var bgRegistry = bgRegistryT{procs: map[*bgProcess]struct{}{}}

func (r *bgRegistryT) add(p *bgProcess) {
	r.mu.Lock()
	r.procs[p] = struct{}{}
	r.mu.Unlock()
}

func (r *bgRegistryT) remove(p *bgProcess) {
	r.mu.Lock()
	delete(r.procs, p)
	r.mu.Unlock()
}

func (r *bgRegistryT) killAll() int {
	r.mu.Lock()
	ps := make([]*bgProcess, 0, len(r.procs))
	for p := range r.procs {
		ps = append(ps, p)
	}
	r.procs = map[*bgProcess]struct{}{}
	r.mu.Unlock()
	for _, p := range ps {
		_ = p.Kill()
	}
	return len(ps)
}

// Shutdown 收掉本包起的一切常驻资源（应用/bridge 关闭时调用）：
// 在飞的执行组 + 后台常驻进程。返回收掉的进程/组数（诊断用）。
//
// 为什么放在 sandbox 包而不是宿主：**我们起的进程由我们负责**（孤儿 = 0 的
// 最后一道兜底）。宿主（bridge）在关闭路径调用一次即可。
func Shutdown() int {
	n := killLiveGroups()
	n += bgRegistry.killAll()
	return n
}

// BackgroundCount 当前在跑的后台进程数（诊断/回归网用）。
func BackgroundCount() int {
	bgRegistry.mu.Lock()
	defer bgRegistry.mu.Unlock()
	return len(bgRegistry.procs)
}

// LiveGroupCount 当前在飞的执行组数（诊断/回归网用）。
func LiveGroupCount() int {
	liveGroups.mu.Lock()
	defer liveGroups.mu.Unlock()
	return len(liveGroups.pids)
}
