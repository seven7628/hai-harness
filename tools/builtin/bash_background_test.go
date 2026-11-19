package builtin

// 「进程的生命周期」回归网（2026-11-18「资源有界化」第 1 步）。
//
// 钉住三件事：
//  1. **命令正常结束 ⇒ 它留下的后台进程被收掉**（改造前：留下孤儿，PPID=1、
//     继续烧 CPU —— 本机 100 个孤儿 headless Chrome 的成因）。
//  2. **模型能显式表达「我要一个后台进程」**（run_in_background）⇒ 宿主的
//     「一刀切收组」才不会误杀用户要的服务（误杀率必须为 0）。
//  3. **工具描述里写明了进程收尾契约**（提示词层的契约 ⇒ 模型知道规则）。
//
// 第 3 条是**不变式测试**：以后谁改工具描述把契约删了，这条就红。

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

// bashBGAlive 探活（signal 0）：true = 进程还在。
func bashBGAlive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// bashBGWaitGone 等进程消失（≤timeout）：true = 已消失。
func bashBGWaitGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !bashBGAlive(pid) {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// bashBGReadPID 读子进程自报的 pid。
func bashBGReadPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
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

// TestBashNormalExitReapsLeftoverProcess：命令正常结束 ⇒ 残留进程被收掉。
func TestBashNormalExitReapsLeftoverProcess(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "child.pid")
	tool := NewBashTool(dir, 5*time.Second, nil, nil)

	// 后台进程不持有 stdout（否则执行层要等 WaitDelay 才返回；那是另一条路径）。
	cmd := "sleep 30 >/dev/null 2>&1 & echo $! > " + pidFile + "; exit 0"
	out, err := tool.Call(context.Background(), "c1", `{"command":`+strconv.Quote(cmd)+`}`)
	if err != nil {
		t.Fatalf("执行失败: %v (out=%q)", err, out)
	}
	pid := bashBGReadPID(t, pidFile)
	if !bashBGWaitGone(pid, 5*time.Second) {
		t.Fatalf("命令正常结束后残留进程 %d 仍活着 ⇒ 成了孤儿", pid)
	}
}

// TestBashRunInBackgroundStartsPersistentProcess：run_in_background ⇒ 起常驻进程，
// 立即返回句柄（pid + 日志路径），进程在调用返回后**仍活着**（豁免生效）。
func TestBashRunInBackgroundStartsPersistentProcess(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "bg.pid")
	tool := NewBashTool(dir, 5*time.Second, nil, nil)

	out, err := tool.Call(context.Background(), "c1",
		`{"command":`+strconv.Quote("echo $$ > "+pidFile+"; echo hello-from-bg; sleep 30")+`,"run_in_background":true}`)
	if err != nil {
		t.Fatalf("后台启动失败: %v (out=%q)", err, out)
	}
	if !strings.Contains(out, "started in background") {
		t.Fatalf("结果没声明「后台已启动」: %q", out)
	}
	// 日志路径：结果里报出来（模型据此 tail / read_file）。
	logPath := bashBGLogPath(t, out)
	defer os.RemoveAll(filepath.Dir(logPath)) // 收尾：本次测试的落盘目录不留
	// 日志文件有内容（后台进程的 stdout 落盘了）。
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(logPath); err == nil && strings.Contains(string(b), "hello-from-bg") {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if b, _ := os.ReadFile(logPath); !strings.Contains(string(b), "hello-from-bg") {
		t.Fatalf("后台日志里没有进程输出: %q", string(b))
	}
	// 进程还活着（这次调用**没有**收掉它 ⇒ 豁免生效）。
	pid := bashBGReadPID(t, pidFile)
	if !bashBGAlive(pid) {
		t.Fatalf("后台进程 %d 在调用返回后就没了 ⇒ 豁免没生效", pid)
	}
	// 后续命令的收组也不能波及它。
	if _, err := tool.Call(context.Background(), "c2",
		`{"command":`+strconv.Quote("sleep 30 >/dev/null 2>&1 & exit 0")+`}`); err != nil {
		t.Fatalf("后续命令失败: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if !bashBGAlive(pid) {
		t.Fatalf("后台进程 %d 被后续命令的收组误杀了", pid)
	}
	// 收尾：杀掉它（别把测试进程留在后台）。
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// bashBGLogPath 从结果文本里取出日志路径（"[started in background] pid=N log=P"）。
func bashBGLogPath(t *testing.T, out string) string {
	t.Helper()
	i := strings.Index(out, "log=")
	if i < 0 {
		t.Fatalf("结果里没有日志路径: %q", out)
	}
	rest := out[i+len("log="):]
	if j := strings.IndexAny(rest, " \n"); j >= 0 {
		rest = rest[:j]
	}
	if rest == "" {
		t.Fatalf("结果里的日志路径为空: %q", out)
	}
	return rest
}

// TestBashDescriptionStatesProcessContract：工具描述里必须写明进程收尾契约。
//
// 这是**提示词层**的契约（模型据此知道「`&` 留下的进程会被收」）⇒ 删掉它，
// 模型就会继续用 `&` 起后台进程 ⇒ 我们就又要一刀切地杀/不杀。
func TestBashDescriptionStatesProcessContract(t *testing.T) {
	tool := NewBashTool("", 5*time.Second, nil, nil)
	d := tool.Description_
	for _, want := range []string{
		"run_in_background", // 模型能显式表达「后台」
		"cleaned up",         // 残留进程会被收
	} {
		if !strings.Contains(d, want) {
			t.Fatalf("工具描述里缺少 %q（进程收尾契约）:\n%s", want, d)
		}
	}
}
