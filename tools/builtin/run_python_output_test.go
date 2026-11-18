package builtin

// run_python 的**输出有界化 + 结局上报**测试（2026-11-17，第 1 步）。
//
// 三个契约：
//  1. **内存有界**：脚本 print 8 MB，工具侧的分配量必须与输出大小无关
//     （改造前：沙箱无界缓冲 8 MB + String() 再拷一份 ⇒ 峰值 ≈ 20 MB，
//     而模型侧真正能看到的只有引擎截断后的 20 KB）。
//  2. **完整输出落盘**：模型拿到路径就能读到**全文**（不是被窗口截过的文本）。
//  3. **结局必须上报**：脚本异常退出 / 超时 / 被取消，三种都必须能被看出是失败
//     （改造前 ExecSpec 没有 ExitCode/TimedOut/Canceled 出参 ⇒ 三种都返回
//     nil error ⇒ 引擎判成功 ⇒ 失败账本不记 ⇒ 重复失败守卫对 run_python 失效）。

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/tools"
)

// rpBigOutScript 造一条指定大小的输出（首行与末行带标记，便于验证头尾都在）。
func rpBigOutScript(size int) string {
	return fmt.Sprintf(
		"print('HEAD-MARKER')\nimport sys\nsys.stdout.write('x' * %d)\nsys.stdout.write('\\nTAIL-MARKER\\n')",
		size)
}

// TestRunPythonOutputMemoryBounded 输出 8 MB：工具侧分配量必须远小于输出本身。
func TestRunPythonOutputMemoryBounded(t *testing.T) {
	const size = 8 << 20 // 8 MB
	ws := t.TempDir()
	tool := NewRunPythonTool(ws, &rpFakeRuntime{py: rpFakePython(t)}, nil)

	// 预热（首次调用有一次性开销：spill 目录创建等）
	if _, err := rpCall(t, tool, map[string]any{"script": rpBigOutScript(size)}); err != nil {
		t.Fatal(err)
	}
	var m0, m1 runtime.MemStats
	runtime.ReadMemStats(&m0)
	out, err := rpCall(t, tool, map[string]any{"script": rpBigOutScript(size)})
	if err != nil {
		t.Fatal(err)
	}
	runtime.ReadMemStats(&m1)
	alloc := float64(m1.TotalAlloc - m0.TotalAlloc)

	// 窗口 16 KB + 头尾窗口 + 若干次字符串拷贝 ⇒ 分配应是 KB 级，不是 MB 级。
	if alloc > 1<<20 {
		t.Errorf("输出 %d 字节，工具侧却分配了 %.1f MB —— 内存没有随窗口封顶：%s",
			size, alloc/(1<<20), out[:rpMinInt(len(out), 200)])
	}
	t.Logf("输出 %d 字节 → 工具侧分配 %.1f KB（结果长度 %d）", size, alloc/1024, len(out))
}

// TestRunPythonOutputSpillsFullOutput 大输出 ⇒ 结果报出落盘路径，文件里是**完整**输出。
func TestRunPythonOutputSpillsFullOutput(t *testing.T) {
	const size = 200 << 10 // 200 KB
	ws := t.TempDir()
	tool := NewRunPythonTool(ws, &rpFakeRuntime{py: rpFakePython(t)}, nil)
	out, err := rpCall(t, tool, map[string]any{"script": rpBigOutScript(size)})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(out, "full output saved to") {
		t.Fatalf("结果应报出落盘路径:\n%s", out[:rpMinInt(len(out), 300)])
	}
	// 头尾都在（Python 的 traceback 通常在尾）
	if !strings.Contains(out, "HEAD-MARKER") {
		t.Errorf("结果应含输出开头:\n%s", out[:rpMinInt(len(out), 300)])
	}
	if !strings.Contains(out, "TAIL-MARKER") {
		t.Errorf("结果应含输出结尾:\n%s", out[rpMaxInt(0, len(out)-300):])
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
	// 权限：文件 0600
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("落盘文件权限 = %04o，want 0600", perm)
	}
}

// TestRunPythonSpillFileReadableByModel 落盘路径模型能自己读（read_file 走绝对路径）。
func TestRunPythonSpillFileReadableByModel(t *testing.T) {
	ws := t.TempDir()
	tool := NewRunPythonTool(ws, &rpFakeRuntime{py: rpFakePython(t)}, nil)
	out, err := rpCall(t, tool, map[string]any{"script": rpBigOutScript(100 << 10)})
	if err != nil {
		t.Fatal(err)
	}
	path := extractSpillPath(t, out)

	ft := NewFileTools(ws)
	rf := newReadFileTool(ft)
	text, err := rf.Call(context.Background(), "read_file", fmt.Sprintf(`{"path":%q}`, path))
	if err != nil {
		t.Fatalf("模型读不出落盘文件: %v", err)
	}
	if !strings.Contains(text, "HEAD-MARKER") {
		t.Errorf("读到的内容不对:\n%s", text[:rpMinInt(len(text), 200)])
	}
}

// TestRunPythonReportsExitStatus 脚本失败 ⇒ 首行明说「失败」+ 真实退出码，
// 且 ExecSink 上报同一结局（引擎据此判失败、进失败账本）。
func TestRunPythonReportsExitStatus(t *testing.T) {
	ws := t.TempDir()
	tool := NewRunPythonTool(ws, &rpFakeRuntime{py: rpFakePython(t)}, nil)

	sink := &tools.ExecSink{}
	ctx := tools.WithExecSink(context.Background(), sink)
	raw := fmt.Sprintf(`{"script":%q}`, "import sys\nsys.stderr.write('boom\\n')\nsys.exit(3)")
	out, err := tool.Call(ctx, "run_python", raw)
	if err != nil {
		t.Fatalf("Call 不该返回 error（结局走 ExecSink 旁路）: %v", err)
	}
	// 首行必须声明「这是失败」+ 退出码
	first := out
	if i := strings.IndexByte(out, '\n'); i >= 0 {
		first = out[:i]
	}
	if !strings.HasPrefix(first, "[EXIT 3") {
		t.Errorf("首行应声明失败与退出码，实际 = %q", first)
	}
	// ExecSink 必须上报真实结局（否则引擎判成功 ⇒ 失败账本不记）
	cmd, code, timedOut, canceled, ok := sink.Exit()
	if !ok {
		t.Fatalf("ExecSink 没收到结局上报")
	}
	if code != 3 {
		t.Errorf("ExitCode = %d，want 3（cmd=%q timedOut=%v canceled=%v）", code, cmd, timedOut, canceled)
	}
}

// TestRunPythonSuccessHasNoFailureHeader 成功脚本 ⇒ 不加失败声明（别制造噪音）。
func TestRunPythonSuccessHasNoFailureHeader(t *testing.T) {
	ws := t.TempDir()
	tool := NewRunPythonTool(ws, &rpFakeRuntime{py: rpFakePython(t)}, nil)
	out, err := rpCall(t, tool, map[string]any{"script": "print('ok')"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.HasPrefix(out, "[EXIT") || strings.HasPrefix(out, "[TIMEOUT") || strings.HasPrefix(out, "[CANCELLED") {
		t.Errorf("成功脚本不该有失败声明:\n%s", out)
	}
	if !strings.Contains(out, "ok") {
		t.Errorf("成功脚本的输出丢了:\n%s", out)
	}
}

// TestRunPythonSmallOutputNoSpill 小输出：不落盘、不加提示。
func TestRunPythonSmallOutputNoSpill(t *testing.T) {
	ws := t.TempDir()
	tool := NewRunPythonTool(ws, &rpFakeRuntime{py: rpFakePython(t)}, nil)
	out, err := rpCall(t, tool, map[string]any{"script": "print('hello-small')"})
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

func rpMinInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func rpMaxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
