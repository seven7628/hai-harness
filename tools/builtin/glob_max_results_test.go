package builtin

// glob 的 max_results 硬上限（第 5 轮爬山第 6 步）。
//
// 问题：max_results 是**模型可控**的，而 schema 只声明了 minimum 没声明 maximum
// ⇒ 模型传 max_results=10000000 时，工具会把整个工作区的文件名收进内存
// （一次调用就是一次内存放大）。grep 用的是固定 200（不可调）⇒ 这里给一个够用
// 但有界的上限。

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/tools"
)

// TestGlobMaxResultsIsClamped：模型传再大的 max_results，也只能拿到有界的一页。
func TestGlobMaxResultsIsClamped(t *testing.T) {
	root := t.TempDir()
	// 造比硬上限更多的文件（上限 + 100）。
	const want = maxGlobMaxResults + 100
	for i := 0; i < want; i++ {
		name := filepath.Join(root, fmt.Sprintf("f%05d.txt", i))
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ft := NewFileTools(root)
	var globTool tools.Tool
	for _, tl := range ft.Tools() {
		if tl.Name() == "glob" {
			globTool = tl
		}
	}
	if globTool == nil {
		t.Fatal("没找到 glob 工具")
	}
	out, err := globTool.Call(t.Context(), "glob", `{"pattern":"*.txt","max_results":10000000}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	lines := strings.Split(strings.TrimRight(out, "\n"), "\n")
	// 第一行是表头，后面每行一个文件。
	got := len(lines) - 1
	if got > maxGlobMaxResults {
		t.Fatalf("返回了 %d 条，超过硬上限 %d（max_results 没被钳住）", got, maxGlobMaxResults)
	}
	if got != maxGlobMaxResults {
		t.Fatalf("应恰好返回上限 %d 条，实际 %d", maxGlobMaxResults, got)
	}
	if !strings.Contains(lines[0], fmt.Sprintf("showing first %d", maxGlobMaxResults)) {
		t.Fatalf("表头应说明被截断到 %d 条，实际 %q", maxGlobMaxResults, lines[0])
	}
}

// TestGlobMaxResultsNormalUnchanged：正常取值不受影响（上限远大于默认值）。
func TestGlobMaxResultsNormalUnchanged(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < 5; i++ {
		if err := os.WriteFile(filepath.Join(root, fmt.Sprintf("g%d.txt", i)), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	ft := NewFileTools(root)
	for _, tl := range ft.Tools() {
		if tl.Name() != "glob" {
			continue
		}
		out, err := tl.Call(t.Context(), "glob", `{"pattern":"*.txt"}`)
		if err != nil {
			t.Fatalf("Call: %v", err)
		}
		if n := len(strings.Split(strings.TrimRight(out, "\n"), "\n")) - 1; n != 5 {
			t.Fatalf("默认应返回 5 条，实际 %d（输出 %q）", n, out)
		}
	}
}
