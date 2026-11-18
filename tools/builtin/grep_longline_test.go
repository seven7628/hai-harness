package builtin

// grep 的**超长行**行为（2026-11-17，第 2 步顺带修掉的一个真 bug）。
//
// 背景：一行超过 maxGrepLineBytes（4 MB）时，扫描器累积前 4 MB、丢弃其余。
// 改造前（2026-11-17 之前）**整行丢弃、不返回** ⇒ 调用方拿不到这一行 ⇒
// **行号计数不递增** ⇒ 它之后每一行的行号都往前挪 1 ⇒
// grep 报出来的行号是错的（模型按行号去 read_file 会读错位置）。
//
// 现在：超长行**仍被返回**（内容 = 截断后的前缀），行号计数正确。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGrepLongLineKeepsLineNumbers 超长行之后的行号不能错位。
func TestGrepLongLineKeepsLineNumbers(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString("head MATCH\n")            // 行 1
	b.WriteString(strings.Repeat("z", 5<<20)) // 行 2：5 MB 单行
	b.WriteString("\n")
	b.WriteString("tail MATCH\n") // 行 3
	p := filepath.Join(dir, "long.txt")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	g := newGrepTool(NewFileTools(dir))
	out, err := g.Call(context.Background(), "", `{"pattern":"MATCH"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "long.txt:1:") {
		t.Errorf("应命中第 1 行:\\n%s", out[:min(len(out), 300)])
	}
	if !strings.Contains(out, "long.txt:3:") {
		t.Errorf("超长行之后的命中行号应仍是 3（不能错位成 2）:\\n%s", out[:min(len(out), 300)])
	}
}

// TestGrepLongLineStillMatchesInside 超长行的**前 4 MB 内**若有命中，也应命中
// （改造前整行丢弃 ⇒ 命中被吞掉）。
func TestGrepLongLineStillMatchesInside(t *testing.T) {
	dir := t.TempDir()
	var b strings.Builder
	b.WriteString(strings.Repeat("q", 1<<20))
	b.WriteString("NEEDLE") // 在超长行的前 4 MB 内
	b.WriteString(strings.Repeat("w", 5<<20))
	b.WriteString("\n")
	p := filepath.Join(dir, "long2.txt")
	if err := os.WriteFile(p, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}

	g := newGrepTool(NewFileTools(dir))
	out, err := g.Call(context.Background(), "", `{"pattern":"NEEDLE"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "long2.txt:1:") {
		t.Errorf("超长行前段里的命中不该被吞掉:\\n%s", out[:min(len(out), 300)])
	}
}

// TestReadFileLongLineMarker 超长行在 read_file 里被截断时，**明说**截断了。
func TestReadFileLongLineMarker(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "big-line.txt")
	if err := os.WriteFile(p, []byte(strings.Repeat("y", 300*1024)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	tool := newReadFileTool(NewFileTools(dir))
	out, err := tool.Call(context.Background(), "read_file", `{"path":"big-line.txt"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "truncated at 65536 bytes") {
		t.Errorf("超长行应明说被截断:\\n%s", out[:min(len(out), 300)])
	}
	// 结果本身有界（引擎闸门 20 KB ⇒ 工具侧也不该给几 MB）
	if len(out) > 200000 {
		t.Errorf("结果应远小于被截断的行：len=%d", len(out))
	}
}

var _ = fmt.Sprintf
