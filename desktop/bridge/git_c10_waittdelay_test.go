package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// C10（2026-09-18）：`gitRun` 执行 `commit`/`checkout` 会**触发 git hook**，而 hook 是
// **任意用户脚本**（husky / pre-commit / lint-staged），可以 fork 出**跨进程组存活**的后代
// 并继承 stdout ⇒ 管道写端被持有 ⇒ `CombinedOutput()` 的拷贝协程读不尽 ⇒ `Wait()` 永不返回。
// 此时 `exec.CommandContext` 的 8s 超时**救不了**（它只杀直接子进程）。
//
// 本文件用**真 git + 真 hook**验证修复（与 C8/`runtime` 同处方；不 mock git）。

// initRepoWithPipeHoldingHook 建一个临时 repo，并装一个「后台持管道」的 post-commit hook：
// hook 立刻退出，但 `sleep 8 &` 继承 stdout 写端并把管道持有 8s。
func initRepoWithPipeHoldingHook(t *testing.T) string {
	t.Helper()
	ws := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		c := exec.Command("git", append([]string{"-C", ws}, args...)...)
		c.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
		)
		if out, err := c.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	git("init", "-q")
	// 锁死会影响本用例的本地配置（不依赖用户全局配置）：
	// hooksPath 确保我们的 hook 一定被执行；gpgsign 关掉免得 commit 因签名不可用而失败。
	git("config", "core.hooksPath", ".git/hooks")
	git("config", "commit.gpgsign", "false")
	if err := os.WriteFile(filepath.Join(ws, "a.txt"), []byte("x\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	git("add", "a.txt")
	git("commit", "-q", "-m", "init") // 首次提交：hook 尚未安装，不涉及持管道者

	hook := "#!/bin/sh\nsleep 8 & echo hook-ran\n"
	if err := os.WriteFile(filepath.Join(ws, ".git", "hooks", "post-commit"), []byte(hook), 0o755); err != nil {
		t.Fatal(err)
	}
	return ws
}

func TestGitRunBoundedWhenHookLeavesPipeHolder(t *testing.T) {
	ws := initRepoWithPipeHoldingHook(t)
	if err := os.WriteFile(filepath.Join(ws, "b.txt"), []byte("y\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	start := time.Now()
	_, err := gitCommitWorktree(context.Background(), ws, "c10 bounded", true)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("commit 已成功（hook 起了后台进程不算失败，不得判失败）: %v", err)
	}
	// 阈值 6s 落在 gitWaitDelay(3s) 与 hook 后代寿命(8s) 之间 ⇒ 能区分「有界」与「等后代结束」
	if elapsed > 6*time.Second {
		t.Fatalf("未在有界时间内返回：%v（>6s ⇒ 退化成等 hook 后代自然结束，即旧行为）", elapsed)
	}
	t.Logf("有界返回：elapsed=%v（gitWaitDelay=%v；hook 后代持管道 8s）", elapsed, gitWaitDelay)
}

// TestGitRunWaitDelayIsLoadBearing 对照实验：同一构造**不设 WaitDelay** ⇒ 观测窗内不返回。
// 没有这条，上面那条无法证明「是有界性修复起的作用，而不是恰好很快返回」。
func TestGitRunWaitDelayIsLoadBearing(t *testing.T) {
	ws := initRepoWithPipeHoldingHook(t)
	if err := os.WriteFile(filepath.Join(ws, "c.txt"), []byte("z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 必须先暂存：否则 `git commit` 因「无内容可提交」而失败 ⇒ **post-commit hook 不触发**
	// ⇒ 不会留下持管道后代（我第一版就漏了这步，导致对照用例 0.08s 假通过）。
	{
		add := exec.Command("git", "-C", ws, "add", "c.txt")
		if out, err := add.CombinedOutput(); err != nil {
			t.Fatalf("git add: %v\n%s", err, out)
		}
	}

	// 手工复刻 gitRun 的**修复前**写法：同命令、同缓冲、同 8s ctx，但**不设 WaitDelay**。
	// ⚠️ 第一版我漏了 Stdout/Stderr 赋值 ⇒ Go 把子进程 stdout 接到 /dev/null ⇒ **根本没有管道**
	// ⇒ 无从挂起，对照 0.25s 假通过。**「stdout 走 io.Writer」正是 C8 机制的前提**，删掉前提就什么也没验证。
	var cbuf bytes.Buffer
	c, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	cmd := exec.CommandContext(c, "git", "-C", ws, "commit", "-q", "-m", "c10 contrast")
	cmd.Dir = ws
	cmd.Stdout = &cbuf
	cmd.Stderr = &cbuf
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@example.com",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@example.com",
	)
	outCh := make(chan error, 1)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() { outCh <- cmd.Wait() }()

	select {
	case err := <-outCh:
		t.Fatalf("不设 WaitDelay 时竟在 4s 内返回（err=%v）⇒ 本用例无法区分，前提不成立：\n"+
			"多半是 commit 本身没成功（hook 不触发）⇒ 未留下持管道后代。git 输出：%s",
			err, cmd.ProcessState.String())
	case <-time.After(4 * time.Second):
		t.Logf("对照成立：不设 WaitDelay 时 4s 观测窗内 cmd.Wait 未返回（hook 后代持管道 8s）")
	}
	// 收尸：hook 后代 ≤8s 自灭；这里显式杀 git 进程组避免残留（Setpgid 未设 ⇒ 杀 pid 即可）
	_ = cmd.Process.Kill()
}
