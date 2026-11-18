package builtin

// read_file 的**有界化**测试（2026-11-17，第 2 步）。
//
// 三个契约：
//  1. **扫描到窗口就停**：读 200 行不该把整个文件读完（改造前为了报一句
//     「N lines total」读全文 ⇒ 读 200 行 = O(N) 磁盘读）。
//  2. **窗口硬钳**：模型显式给 start_line/end_line 也只给 maxLines 行
//     （改造前**没有任何上限** ⇒ 一次调用把整个文件收进内存，99.9% 被引擎扔掉）。
//  3. **超长行不失败**：> 4 MB 的单行（minified bundle / base64 blob）——
//     改造前 bufio.Scanner 报 ErrTooLong ⇒ **整个 read_file 失败**
//     （连前 200 行都看不到）；现在只截断那一行。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// rfCall 直接调 read_file（不经引擎，输出即模型看到的东西）。
func rfCall(t *testing.T, tool *readFileTool, args string) string {
	t.Helper()
	out, err := tool.Call(context.Background(), "read_file", args)
	if err != nil {
		t.Fatalf("read_file failed: %v", err)
	}
	return out
}

// TestReadFileStopsAtWindow 窗口之后的**超长行**不该被碰到。
//
// 这是「扫描到 end 就停」的**确定性**证据：改造前会一路扫到文件尾，
// 撞上 > 4 MB 的单行 ⇒ ErrTooLong ⇒ 整个 read_file 失败。
func TestReadFileStopsAtWindow(t *testing.T) {
	dir := t.TempDir()
	// 前 250 行正常（第 201 行之后），之后是一行 5 MB 的超长行（改造前必失败）。
	var b strings.Builder
	for i := 1; i <= 250; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	b.WriteString(strings.Repeat("z", 5*1024*1024))
	b.WriteString("\n")
	p := filepath.Join(dir, "late-long.txt")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := newReadFileTool(NewFileTools(dir))
	out := rfCall(t, tool, `{"path":"late-long.txt"}`)
	if !strings.Contains(out, "1: line 1") {
		t.Errorf("应读到窗口内的内容:\n%s", out[:min(len(out), 300)])
	}
	if !strings.Contains(out, "200: line 200") {
		t.Errorf("应读到窗口的最后一行:\n%s", out[:min(len(out), 300)])
	}
	if strings.Contains(out, "zzzzz") {
		t.Errorf("不该读到窗口之外的超长行")
	}
}

// TestReadFileLongLineTruncatedNotFailed 窗口**之内**的超长行：截断后继续，不是失败。
func TestReadFileLongLineTruncatedNotFailed(t *testing.T) {
	dir := t.TempDir()
	// 第 1 行超长（5 MB），后面还有正常行。
	var b strings.Builder
	b.WriteString(strings.Repeat("y", 5*1024*1024))
	b.WriteString("\nafter\n")
	p := filepath.Join(dir, "long.txt")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := newReadFileTool(NewFileTools(dir))
	out := rfCall(t, tool, `{"path":"long.txt"}`)
	// 改造前：整个 read_file 失败（ErrTooLong）⇒ 这里必然是失败。
	if !strings.Contains(out, "2: after") {
		t.Errorf("超长行之后的内容应照常读到（改造前整个调用失败）:\n%s", out[:min(len(out), 300)])
	}
	if !strings.Contains(out, "1: yyyy") {
		t.Errorf("超长行应被截断后保留前缀:\n%s", out[:min(len(out), 120)])
	}
}

// TestReadFileWindowIsClamped 显式给 end_line 也只给 maxLines 行，并报出续读位置。
func TestReadFileWindowIsClamped(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 1000; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	p := filepath.Join(dir, "big.txt")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := newReadFileTool(NewFileTools(dir))
	// 模型要 1..1000000 行 ⇒ 只给 200 行 + 续读指引
	out := rfCall(t, tool, `{"path":"big.txt","start_line":1,"end_line":1000000}`)
	lines := strings.Count(out, "\n:")
	if lines > 210 {
		t.Errorf("窗口应被钳到 200 行，实际约 %d 行", lines)
	}
	if !strings.Contains(out, "at most 200 lines per call; use start_line=201 to continue") {
		t.Errorf("应报出续读位置:\n%s", out[:min(len(out), 300)])
	}
	if !strings.Contains(out, "200: line 200") {
		t.Errorf("应读到第 200 行:\n%s", out[:min(len(out), 300)])
	}
	if strings.Contains(out, "201: line 201") {
		t.Errorf("不该越过窗口:\n%s", out[:min(len(out), 300)])
	}
}

// TestReadFilePagingCoversWholeFile 分页读（start_line 续读）能覆盖整个文件。
func TestReadFilePagingCoversWholeFile(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	for i := 1; i <= 450; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	p := filepath.Join(dir, "mid.txt")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	tool := newReadFileTool(NewFileTools(dir))
	seen := map[int]bool{}
	for start := 1; start <= 450; start += 200 {
		out := rfCall(t, tool, fmt.Sprintf(`{"path":"mid.txt","start_line":%d}`, start))
		for _, line := range strings.Split(out, "\n") {
			if !strings.Contains(line, ": line ") {
				continue
			}
			n := 0
			if _, err := fmt.Sscanf(line, "%d: line %d", &n, &n); err != nil {
				continue
			}
			if n > 0 {
				seen[n] = true
			}
		}
	}
	if len(seen) != 450 {
		t.Errorf("分页读应覆盖 450 行，实际 %d 行", len(seen))
	}
}

// TestReadFileSingleOpenReusesFD 三次文件系统往返 → 一次（结构性改动，行为等价）。
// 这里钉住**行为**：同一次调用仍能正确处理「头是魔数但后面是文本」等边界。
func TestReadFileHeadProbeStillWorks(t *testing.T) {
	dir := t.TempDir()
	// 极短文件（头不足 512 B）
	if err := os.WriteFile(filepath.Join(dir, "tiny.txt"), []byte("ab"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := newReadFileTool(NewFileTools(dir))
	out := rfCall(t, tool, `{"path":"tiny.txt"}`)
	if !strings.Contains(out, "1: ab") {
		t.Errorf("短文件应正常读出:\n%s", out)
	}
}
