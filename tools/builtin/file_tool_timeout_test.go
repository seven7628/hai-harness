package builtin

// 文件工具（read_file / write_file / edit_file）的**可中断性 + 读上限**测试
// （2026-11-17，第 3 步）。
//
// 改造前这三个工具：① 不看 ctx ⇒ 引擎的 5 分钟工具超时对它们**完全无效**
// （超时靠取消 ctx 实现，工具不看就收不到）；② 整文件读**无上限**
// ⇒ 一个 100 MB 文件 = 峰值 4–6× 文件大小的内存。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/tools"
)

// TestFileToolsDeclareTimeout 三个文件工具都声明工具级时限。
func TestFileToolsDeclareTimeout(t *testing.T) {
	ft := NewFileTools(t.TempDir())
	for _, tool := range []tools.Tool{newReadFileTool(ft), newWriteFileTool(ft), newEditFileTool(ft)} {
		tp, ok := tool.(tools.ToolTimeoutProvider)
		if !ok {
			t.Errorf("%s 未实现 ToolTimeoutProvider", tool.Name())
			continue
		}
		d := tp.ToolTimeout()
		if d <= 0 || d > 5*time.Minute {
			t.Errorf("%s 的工具级时限不合理: %v", tool.Name(), d)
		}
	}
}

// TestFileToolsRespectCancelledContext 已取消的 ctx ⇒ 立刻报错，不读文件。
func TestFileToolsRespectCancelledContext(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ft := NewFileTools(dir)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tc := range []struct {
		name string
		tool tools.Tool
		args string
	}{
		{"read_file", newReadFileTool(ft), fmt.Sprintf(`{"path":%q}`, p)},
		{"write_file", newWriteFileTool(ft), fmt.Sprintf(`{"path":%q,"content":"x"}`, filepath.Join(dir, "b.txt"))},
		{"edit_file", newEditFileTool(ft), fmt.Sprintf(`{"path":%q,"old_string":"hello","new_string":"bye"}`, p)},
	} {
		start := time.Now()
		_, err := tc.tool.Call(ctx, tc.name, tc.args)
		if err == nil {
			t.Errorf("%s: 已取消的 ctx 应当报错", tc.name)
			continue
		}
		if d := time.Since(start); d > 2*time.Second {
			t.Errorf("%s: 取消后返回太慢: %v", tc.name, d)
		}
	}
}

// TestEditFileTooLargeIsRejected 超过上限的文件 ⇒ 明确报错，而不是把几百 MB 读进内存。
func TestEditFileTooLargeIsRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.txt")
	// 写一个超过上限的文件（稀疏写：只写头尾，内容无所谓）
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("head needle tail\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxFileToolReadBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()

	tool := newEditFileTool(NewFileTools(dir))
	_, err = tool.Call(context.Background(), "edit_file", fmt.Sprintf(`{"path":%q,"old_string":"needle","new_string":"x"}`, p))
	if err == nil {
		t.Fatalf("超大文件应当报错（而不是整读进内存）")
	}
	if !strings.Contains(err.Error(), "file_too_large") {
		t.Errorf("错误码不对: %v", err)
	}
	// 错误文案要指路（模型的下一步动作）
	if !strings.Contains(err.Error(), "bash") {
		t.Errorf("错误应指路 bash/python: %v", err)
	}
}

// TestWriteFileTooLargeIsRejected write_file 覆盖一个超大既有文件 ⇒ 同样拒绝。
func TestWriteFileTooLargeIsRejected(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big.txt")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.WriteString("old\n"); err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(maxFileToolReadBytes + 1); err != nil {
		t.Fatal(err)
	}
	f.Close()

	tool := newWriteFileTool(NewFileTools(dir))
	_, err = tool.Call(context.Background(), "write_file", fmt.Sprintf(`{"path":%q,"content":"new"}`, p))
	if err == nil {
		t.Fatalf("超大文件应当报错")
	}
	if !strings.Contains(err.Error(), "file_too_large") {
		t.Errorf("错误码不对: %v", err)
	}
}

// TestReadFileSmallStillWorks 正常文件不受影响（有界化不能把正常路径搞坏）。
func TestReadFileSmallStillWorks(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "a.txt")
	if err := os.WriteFile(p, []byte("l1\nl2\nl3\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := newReadFileTool(NewFileTools(dir))
	out, err := tool.Call(context.Background(), "read_file", fmt.Sprintf(`{"path":%q}`, p))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"1: l1", "2: l2", "3: l3"} {
		if !strings.Contains(out, want) {
			t.Errorf("缺少 %q:\n%s", want, out)
		}
	}
}
