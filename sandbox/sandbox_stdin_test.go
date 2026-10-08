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

// ---- Seatbelt 后端：同一个接线点必须同样生效（darwin 专有，其余平台 skip）----

// seatbeltOrSkip 构造一个可用的 Seatbelt 后端；不可用（非 darwin / 无
// sandbox-exec / 策略探测失败）时跳过 —— 本文件在非 macOS 也要能编译通过。
func seatbeltOrSkip(t *testing.T) *Seatbelt {
	t.Helper()
	if !Available() {
		t.Skip("sandbox-exec 不可用（非 darwin 或未安装），跳过真实沙箱用例")
	}
	s, err := NewSeatbelt(t.TempDir())
	if err != nil {
		t.Skipf("Seatbelt 构造失败（本环境不支持真实沙箱）: %v", err)
	}
	return s
}

// TestExecSpecStdinFeedsSeatbelt Stdin 必须经 runExec 统一接线，**两个后端都要生效**。
//
// 回归背景：接线点曾只写在 NoSandbox.Run 里，Seatbelt 侧静默丢弃 Stdin ——
// `cat` 读到 EOF 立即返回空输出，`node --input-type=module -` 会当成空脚本跑完并
// 返回 exit 0：调用方看到的是「成功但没有任何输出」。而 codemode 的脚本正是经 stdin
// 喂进去的，且执行器被设计为注入「与 bash 同一个 sandbox 实例」
// （desktop/bridge/main.go 在 sandbox mode = seatbelt 时就是这条路径）。
func TestExecSpecStdinFeedsSeatbelt(t *testing.T) {
	s := seatbeltOrSkip(t)
	out, err := s.Run(context.Background(), ExecSpec{
		Command: "cat",
		Stdin:   strings.NewReader("stdin-payload-marker"),
		Timeout: 10 * time.Second,
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out, "stdin-payload-marker") {
		t.Fatalf("out = %q, want it to contain the stdin payload（Seatbelt 未转发 ExecSpec.Stdin）", out)
	}
}

// TestExecSpecStdinNilIsOldBehaviorSeatbelt 零值不变式在 Seatbelt 侧同样成立：
// Stdin==nil 不接 stdin，且不因「等输入」挂死。
func TestExecSpecStdinNilIsOldBehaviorSeatbelt(t *testing.T) {
	s := seatbeltOrSkip(t)
	done := make(chan struct{})
	go func() {
		defer close(done)
		out, err := s.Run(context.Background(), ExecSpec{Command: "echo nil-stdin-ok", Timeout: 10 * time.Second})
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		if !strings.Contains(out, "nil-stdin-ok") {
			t.Errorf("out = %q, want 'nil-stdin-ok'", out)
		}
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Seatbelt Run with nil Stdin hung")
	}
}
