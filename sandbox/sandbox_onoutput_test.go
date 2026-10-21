package sandbox

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestExecSpecOnOutputTeesOutput OnOutput 必须拿到**子进程真实输出**的增量，
// 而 Run 的返回值仍是完整输出（尾巴是附加视图，不是替代通道）。
func TestExecSpecOnOutputTeesOutput(t *testing.T) {
	var mu sync.Mutex
	var got strings.Builder
	s := NoSandbox{}
	out, err := s.Run(context.Background(), ExecSpec{
		Command:  "echo hello-onoutput",
		Timeout:  10 * time.Second,
		OnOutput: func(p []byte) { mu.Lock(); got.Write(p); mu.Unlock() },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	streamed := got.String()
	mu.Unlock()
	if !strings.Contains(streamed, "hello-onoutput") {
		t.Fatalf("OnOutput 没拿到输出：%q", streamed)
	}
	if !strings.Contains(out, "hello-onoutput") {
		t.Fatalf("返回值必须仍是完整输出：%q", out)
	}
	// 拼接后的流与返回值一致（同一批字节，只做了拷贝）
	if strings.TrimSpace(streamed) != strings.TrimSpace(out) {
		t.Fatalf("流式内容应与返回值一致：streamed=%q out=%q", streamed, out)
	}
}

// TestExecSpecOnOutputConcurrentStreamsOk stdout 与 stderr 由两个拷贝协程并发写入，
// 回调必须能承受并发（实现方自带锁）——这里用 -race 跑得出竞争就说明接线错了。
func TestExecSpecOnOutputConcurrentStreamsOk(t *testing.T) {
	var mu sync.Mutex
	var n int
	s := NoSandbox{}
	_, err := s.Run(context.Background(), ExecSpec{
		Command:  "for i in $(seq 1 50); do echo out$i; echo err$i 1>&2; done",
		Timeout:  10 * time.Second,
		OnOutput: func(p []byte) { mu.Lock(); n += len(p); mu.Unlock() },
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if n == 0 {
		t.Fatal("并发流没有回调到任何字节")
	}
}
