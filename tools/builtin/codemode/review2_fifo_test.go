//go:build !windows

package codemode

// review2_fifo_test.go（F2）：image() 不得读非普通文件。
//
// 为什么单列：这条要用 unix 的 Mkfifo 造一个真 FIFO。缺陷形态是「st.Size() 对 FIFO 恒 0
// ⇒ 绕过 8 MiB 闸门」，且 os.ReadFile 会**一直阻塞**在没有写者的 FIFO 上 —— 沙箱被杀之后
// 宿主这条 goroutine 仍卡在 read 里（fd + goroutine 泄漏，ExecuteScript 早已返回）。
// 故这里除了「被拒」，还要断言「立刻返回」（不许卡住）。

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestImageRefusesNonRegularFile(t *testing.T) {
	dir := t.TempDir()
	fifo := filepath.Join(dir, "pipe.png")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("本平台不支持 Mkfifo: %v", err)
	}
	b, _ := newOpBridge(t)

	done := make(chan string, 1)
	go func() {
		r := b.hostOp(context.Background(), opImage,
			json.RawMessage(`{"path":`+jsonString(fifo)+`}`))
		done <- string(r.Value)
	}()
	select {
	case got := <-done:
		if !strings.Contains(got, "regular file") {
			t.Fatalf("FIFO 必须以「不是普通文件」被拒，实际: %s", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("image(FIFO) 卡住了宿主（没有写者时 os.ReadFile 会一直阻塞）—— 这就是 F2")
	}
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}
