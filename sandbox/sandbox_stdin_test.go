package sandbox

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// TestExecSpecStdinFeedsProcess Stdin 必须真的喂进子进程。
// 这是 ExecSpec.Stdin 的唯一目的：让脚本类解释器能用 `python -` / `node -`
// 经 stdin 收源码，而不必把源码拼进命令行（两层转义任一组合都能把脚本改坏）。
func TestExecSpecStdinFeedsProcess(t *testing.T) {
	s := NoSandbox{}
	out, err := s.Run(context.Background(), ExecSpec{
		Command: "cat",
		Stdin:   strings.NewReader("hello-from-stdin"),
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "hello-from-stdin") {
		t.Fatalf("out = %q, want it to contain the stdin payload", out)
	}
}

// TestExecSpecStdinNilIsOldBehavior 零值不变式：Stdin==nil 时行为与本字段
// 加入前完全一致，且不会因为「cmd.Stdin 为 nil 导致 exec 等待输入」而挂死。
func TestExecSpecStdinNilIsOldBehavior(t *testing.T) {
	s := NoSandbox{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		out, err := s.Run(context.Background(), ExecSpec{
			Command: "echo no-stdin",
			Timeout: 10 * time.Second,
		})
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		if !strings.Contains(out, "no-stdin") {
			t.Errorf("out = %q, want 'no-stdin'", out)
		}
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Run with nil Stdin hung — nil must not make exec wait on stdin")
	}
}

// TestExecSpecStdinMultiLine  多行 payload 完整送达（换行不被截断）。
func TestExecSpecStdinMultiLine(t *testing.T) {
	payload := "line1\nline2\nline3\n"
	s := NoSandbox{}
	out, err := s.Run(context.Background(), ExecSpec{
		Command: "cat",
		Stdin:   strings.NewReader(payload),
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	for _, line := range []string{"line1", "line2", "line3"} {
		if !strings.Contains(out, line) {
			t.Fatalf("out = %q, missing %q", out, line)
		}
	}
}

// TestExecSpecStdinLargePayload 大 payload（1 MiB）验证管道正确交接。
func TestExecSpecStdinLargePayload(t *testing.T) {
	payload := bytes.Repeat([]byte("abcdefgh"), 128*1024) // 1 MiB
	s := NoSandbox{}
	out, err := s.Run(context.Background(), ExecSpec{
		Command: "wc -c",
		Stdin:   bytes.NewReader(payload),
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "1048576") {
		t.Fatalf("out = %q, want 1048576 bytes", out)
	}
}

// TestExecSpecStdinRespectsTimeout 有 stdin 且子进程不消费时，超时仍须按
// 三层 kill 预算返回（不因「等待输入」绕过超时）。
func TestExecSpecStdinRespectsTimeout(t *testing.T) {
	pr, pw := exec.Command("sh", "-c").StdinPipe() // 占位，仅为类型完整
	_ = pr
	_ = pw

	s := NoSandbox{}
	start := time.Now()
	out, _ := s.Run(context.Background(), ExecSpec{
		Command: "cat",                  // 读 stdin；下面给一个不关闭的 reader
		Stdin:   &neverClosingReader{},  // 永不 EOF
		Timeout: 500 * time.Millisecond, //
	})
	elapsed := time.Since(start)

	// 不变量：runExec 必定在 timeout + killWaitBudget + killEscalationWait 内返回。
	// （该常量见 sandbox.go:111-113）
	limit := 500*time.Millisecond + killWaitBudget + killEscalationWait + 3*time.Second
	if elapsed > limit {
		t.Fatalf("elapsed = %v, want <= %v (三层 kill 预算不变量)", elapsed, limit)
	}
	_ = out
}

// neverClosingReader 永不返回 EOF 的 reader（模拟「子进程等输入」）。
type neverClosingReader struct{}

func (*neverClosingReader) Read(p []byte) (int, error) {
	// 阻塞一小会儿再返回 0,nil —— 模拟无数据但未 EOF。
	time.Sleep(50 * time.Millisecond)
	return 0, nil
}
