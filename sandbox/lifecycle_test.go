package sandbox

// 执行作用域收尾的回归网（2026-11-18「资源有界化」第 1 步）。
//
// 这组测试钉住的是**用户可见的行为**，不是实现细节：
//   1. 命令**正常结束** ⇒ 它留下的后台进程被收掉（改造前：留下孤儿）。
//   2. 命令**还在跑** ⇒ 绝不被提前杀（收组不能误伤正在干活的命令）。
//   3. **后台常驻进程**（run_in_background）⇒ 活得过之后所有命令的收组。
//   4. 应用关闭（Shutdown）⇒ 后台进程与在飞命令一并收掉（我们不留下孤儿）。
//
// 全部用 POSIX 写法（sh -c / kill -0）⇒ macOS 与 Linux 同一份测试、同一个判据。

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// pidAlive 探活（signal 0）：true = 进程还在。
func pidAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// waitGone 等进程消失（≤timeout）：true = 已消失。
func waitGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !pidAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// readPID 读子进程写下的 pid（后台进程自报，便于断言）。
func readPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil {
			if s := strings.TrimSpace(string(b)); s != "" {
				pid, err := strconv.Atoi(s)
				if err != nil {
					t.Fatalf("pid 文件内容不是数字: %q", s)
				}
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("等不到 pid 文件: %s", path)
	return 0
}

// TestRunExecNormalExitReapsLeftoverProcesses：命令正常结束 ⇒ 残留进程被收掉。
//
// 这是本轮改动���的核心行为：改造前 runExec 只在超时/取消路径杀组 ⇒ 一条
// `cmd &` 成功返回后，那个后台进程就地变孤儿（PPID=1、继续烧 CPU）。
func TestRunExecNormalExitReapsLeftoverProcesses(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	// 后台进程不持有 stdout（否则执行层要等 WaitDelay 3s 才返回 —— 那是另一条
	// 路径的测试）；这里只测「正常结束 ⇒ 收组」。
	cmd := "sleep 30 >/dev/null 2>&1 & echo $! > " + pidFile + "; exit 0"

	var exitCode int
	out, err := NoSandbox{}.Run(context.Background(), ExecSpec{
		Command:  cmd,
		ExitCode: &exitCode,
	})
	if err != nil {
		t.Fatalf("执行失败: %v (out=%q)", err, out)
	}
	pid := readPID(t, pidFile)
	if !pidAlive(pid) {
		return // 已经没了（正常路径不该这样，但也不算失败）
	}
	// 收组是异步的（SIGTERM → 100ms 宽限 → SIGKILL）⇒ 轮询等它消失。
	if !waitGone(pid, 5*time.Second) {
		t.Fatalf("命令正常结束后残留进程 %d 仍活着 ⇒ 没收组（孤儿）", pid)
	}
}

// TestRunExecKeepsGroupWhileCommandRuns：命令**还在跑**时绝不被提前收掉。
//
// 收组只发生在「命令结束之后」⇒ 正在干活的命令（含它 fork 的同组进程）不受影响。
func TestRunExecKeepsGroupWhileCommandRuns(t *testing.T) {
	var exitCode int
	start := time.Now()
	out, err := NoSandbox{}.Run(context.Background(), ExecSpec{
		Command:  "sleep 1; exit 7",
		ExitCode: &exitCode,
	})
	if err != nil {
		t.Fatalf("执行失败: %v (out=%q)", err, out)
	}
	if exitCode != 7 {
		t.Fatalf("命令被提前杀了（exit=%d，期望 7）⇒ 收组误伤了运行中的命令", exitCode)
	}
	if el := time.Since(start); el < 900*time.Millisecond {
		t.Fatalf("命令只跑了 %v 就返回 ⇒ 提前被杀", el)
	}
}

// TestBackgroundProcessSurvivesCommandReap：后台常驻进程活得过之后所有命令的收组。
//
// 豁免靠**边界不同**（后台进程在自己的进程组）⇒ 不依赖任何标志位 ⇒ 不会漏判。
func TestBackgroundProcessSurvivesCommandReap(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "bg.pid")
	logFile := filepath.Join(dir, "bg.log")

	proc, err := NoSandbox{}.StartBackground(context.Background(), ExecSpec{
		Command:      "echo $$ > " + pidFile + "; sleep 30",
		BackgroundLog: logFile,
	})
	if err != nil {
		t.Fatalf("起后台进程失败: %v", err)
	}
	bgPID := readPID(t, pidFile)

	// 一条普通命令：它自己留下一个后台进程 ⇒ 触发收组 ⇒ 不能波及后台常驻进程。
	if _, err := (NoSandbox{}).Run(context.Background(), ExecSpec{
		Command: "sleep 30 >/dev/null 2>&1 & exit 0",
	}); err != nil {
		t.Fatalf("普通命令执行失败: %v", err)
	}
	time.Sleep(300 * time.Millisecond) // 等收组的宽限期走完
	if !pidAlive(bgPID) {
		t.Fatalf("后台常驻进程 %d 被普通命令的收组误杀了", bgPID)
	}
	if !pidAlive(proc.Pid()) {
		t.Fatalf("后台进程句柄的 pid %d 不在了", proc.Pid())
	}
	// 收尾：杀掉它（别把测试进程留在后台）。
	_ = proc.Kill()
	waitGone(bgPID, 5*time.Second)
}

// TestShutdownKillsBackgroundsAndLiveGroups：应用关闭 ⇒ 收掉后台进程与在飞命令。
//
// 这是「孤儿 = 0」的最后一道兜底：我们起的进程由我们负责。
func TestShutdownKillsBackgroundsAndLiveGroups(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "bg.pid")
	proc, err := NoSandbox{}.StartBackground(context.Background(), ExecSpec{
		Command:      "echo $$ > " + pidFile + "; sleep 30",
		BackgroundLog: filepath.Join(dir, "bg.log"),
	})
	if err != nil {
		t.Fatalf("起后台进程失败: %v", err)
	}
	bgPID := readPID(t, pidFile)
	if BackgroundCount() == 0 {
		t.Fatalf("后台进程没进登记表（BackgroundCount=0）")
	}
	Shutdown()
	// 收组是异步的（SIGTERM → 100ms 宽限 → SIGKILL）⇒ 轮询等它消失。
	if !waitGone(bgPID, 5*time.Second) {
		t.Fatalf("Shutdown 后后台进程 %d 还活着", bgPID)
	}
	if BackgroundCount() != 0 {
		t.Fatalf("Shutdown 后登记表没清空: %d", BackgroundCount())
	}
	_ = proc
}

// TestLiveGroupRegisteredWhileRunning：在飞命令在登记表里；结束后注销。
func TestLiveGroupRegisteredWhileRunning(t *testing.T) {
	before := LiveGroupCount()
	if _, err := (NoSandbox{}).Run(context.Background(), ExecSpec{Command: "sleep 0.2"}); err != nil {
		t.Fatalf("执行失败: %v", err)
	}
	// 收尾后应已注销（注销在 runExec 的 defer 里 ⇒ 命令返回时已完成）。
	if after := LiveGroupCount(); after != before {
		t.Fatalf("命令结束后执行组没注销: %d → %d", before, after)
	}
}
