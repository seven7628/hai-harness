package builtin

// bash 输出的**有界化 + 落盘**测试（2026-11-17）。
//
// 三个契约：
//  1. **内存有界**：命令输出 8 MB，工具侧的分配量必须与输出大小无关
//     （改造前：32 MB 输出 = 96 MB 分配 —— 扩容 ~2× + String() 拷贝 1×）。
//  2. **完整输出落盘**：模型拿到路径就能读到**全文**（不是被窗口截过的文本）。
//  3. **头尾都在**：结果里同时有输出的开头和结尾 —— 错误信息通常在结尾。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// bigOutputCmd 造一条指定大小的输出（首行与末行带标记，便于验证头尾都在）。
func bigOutputCmd(size int) string {
	return fmt.Sprintf(
		"printf 'HEAD-MARKER\\n'; head -c %d /dev/zero | tr '\\0' x; printf '\\nTAIL-MARKER\\n'",
		size)
}

// TestBashOutputMemoryBounded 输出 8 MB：工具侧分配量必须远小于输出本身。
func TestBashOutputMemoryBounded(t *testing.T) {
	const size = 8 << 20 // 8 MB
	tool := NewBashTool(t.TempDir(), 0, nil, nil)
	args := fmt.Sprintf(`{"command":%q}`, bigOutputCmd(size))

	// 预热（首次调用有一次性开销：spill 目录创建等）
	if _, err := tool.Call(context.Background(), "bash", args); err != nil {
		t.Fatal(err)
	}
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	out, err := tool.Call(context.Background(), "bash", args)
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&m1)
	alloc := float64(m1.TotalAlloc - m0.TotalAlloc)

	// 窗口 16 KB + 头尾窗口 + 若干次字符串拷贝 ⇒ 分配应是 KB 级，不是 MB 级。
	if alloc > 1<<20 {
		t.Errorf("输出 %d 字节，工具侧却分配了 %.1f MB —— 内存没有随窗口封顶：%s",
			size, alloc/(1<<20), out[:min(len(out), 200)])
	}
	t.Logf("输出 %d 字节 → 工具侧分配 %.1f KB（结果长度 %d）", size, alloc/1024, len(out))
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// TestBashOutputSpillsFullOutput 大输出 ⇒ 结果报出落盘路径，文件里是**完整**输出。
func TestBashOutputSpillsFullOutput(t *testing.T) {
	const size = 200 << 10 // 200 KB
	tool := NewBashTool(t.TempDir(), 0, nil, nil)
	out, err := tool.Call(context.Background(), "bash", fmt.Sprintf(`{"command":%q}`, bigOutputCmd(size)))
	if err != nil {
		t.Fatal(err)
	}

	// 结果首部必须有落盘提示
	if !strings.Contains(out, "full output saved to") {
		t.Fatalf("结果应报出落盘路径:\n%s", out[:min(len(out), 300)])
	}
	// 头尾都在（错误信息通常在结尾）
	if !strings.Contains(out, "HEAD-MARKER") {
		t.Errorf("结果应含输出开头:\n%s", out[:min(len(out), 300)])
	}
	if !strings.Contains(out, "TAIL-MARKER") {
		t.Errorf("结果应含输出结尾:\n%s", out[max(0, len(out)-300):])
	}
	// 结果有界（压在引擎阈值 20 KB 内 ⇒ 引擎不二次截断/二次落盘）
	if len(out) > 20000 {
		t.Errorf("结果应压在引擎阈值内：len=%d", len(out))
	}

	// 抽路径 → 文件里是**完整**输出
	path := extractSpillPath(t, out)
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("落盘文件读不出: %v", err)
	}
	if len(full) < size {
		t.Errorf("落盘文件不是完整输出：%d 字节（应 ≥ %d）", len(full), size)
	}
	if !strings.Contains(string(full), "HEAD-MARKER") || !strings.Contains(string(full), "TAIL-MARKER") {
		t.Errorf("落盘文件缺头/尾标记")
	}
	// 权限：文件 0600、目录仅属主
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("落盘文件权限 = %04o，want 0600", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dirInfo.Mode().Perm(); perm&0o077 != 0 {
		t.Errorf("落盘目录权限 = %04o，want 仅属主可访问", perm)
	}
}

// TestBashSmallOutputNoSpill 小输出：不落盘、不加提示，结果就是全文。
func TestBashSmallOutputNoSpill(t *testing.T) {
	tool := NewBashTool(t.TempDir(), 0, nil, nil)
	out, err := tool.Call(context.Background(), "bash", `{"command":"echo hello-small"}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "full output saved to") {
		t.Errorf("小输出不该落盘:\n%s", out)
	}
	if !strings.Contains(out, "hello-small") {
		t.Errorf("小输出应原样返回:\n%s", out)
	}
}

// TestBashSpillFileReadableByModel 落盘路径模型能自己读（read_file 走绝对路径）。
func TestBashSpillFileReadableByModel(t *testing.T) {
	const size = 100 << 10
	ws := t.TempDir()
	tool := NewBashTool(ws, 0, nil, nil)
	out, err := tool.Call(context.Background(), "bash", fmt.Sprintf(`{"command":%q}`, bigOutputCmd(size)))
	if err != nil {
		t.Fatal(err)
	}
	path := extractSpillPath(t, out)

	// read_file 支持绝对路径（resolveFor 对绝对路径不做工作区逃逸检查）
	ft := NewFileTools(ws)
	rf := newReadFileTool(ft)
	text, err := rf.Call(context.Background(), "read_file", fmt.Sprintf(`{"path":%q}`, path))
	if err != nil {
		t.Fatalf("模型读不出落盘文件: %v", err)
	}
	if !strings.Contains(text, "HEAD-MARKER") {
		t.Errorf("读到的内容不对:\n%s", text[:min(len(text), 200)])
	}
}

// extractSpillPath 从结果里抽出落盘路径。
func extractSpillPath(t *testing.T, out string) string {
	t.Helper()
	const marker = "full output saved to "
	i := strings.Index(out, marker)
	if i < 0 {
		t.Fatalf("结果里没有落盘路径:\n%s", out[:min(len(out), 300)])
	}
	rest := out[i+len(marker):]
	j := strings.IndexAny(rest, "\n ")
	if j < 0 {
		t.Fatalf("落盘路径解析失败: %q", rest)
	}
	return rest[:j]
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

var _ = time.Second
