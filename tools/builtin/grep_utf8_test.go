package builtin

// G8：截断必须落在 **UTF-8 字符边界**上。
//
// 问题：改造前 `content[:maxGrepLineLength]`（按字节切 200 字节）可能把一个
// 多字节字符切成两半 ⇒ 残缺的字节序列进了输出。encoding/json 遇到非法 UTF-8
// 会把它替换成 U+FFFD（�）⇒ 结果里凭空多出一个乱码字符，而且「这个字符本来
// 是什么」永远丢了。
//
// 同一个类问题还有一处：超长行（> 4 MB）被截断时，切口也可能落在字符中间。

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// TestGrepTruncateKeepsUTF8Boundary 截断后的行内容必须是合法 UTF-8，
// 且不含替换符（U+FFFD）。
func TestGrepTruncateKeepsUTF8Boundary(t *testing.T) {
	// 一行 300 个中文字符（900 字节）⇒ 200 字节的切口正好落在字符中间。
	line := strings.Repeat("中", 300)
	dir := t.TempDir()
	p := filepath.Join(dir, "u.txt")
	if err := os.WriteFile(p, []byte("MATCH "+line+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := newGrepTool(NewFileTools(dir))
	out, err := g.Call(context.Background(), "", `{"pattern":"MATCH"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(out) {
		t.Errorf("输出不是合法 UTF-8:\n%q", out)
	}
	if strings.ContainsRune(out, utf8.RuneError) {
		t.Errorf("输出里有替换符 U+FFFD（说明切口切在字符中间）:\n%q", out)
	}
	// 截断后的内容必须是原行的一个**前缀**（按字符，不多不少）
	// 输出形如 "[grep: 1 match(es) in 1 file(s)]\nu.txt:1:MATCH 中中中...\n"
	body := out[strings.IndexByte(out, '\n')+1:]
	body = strings.SplitN(body, ":", 3)[2]
	body = strings.TrimSuffix(strings.TrimSuffix(body, "\n"), "...")
	body = strings.TrimPrefix(body, "MATCH ")
	if !strings.HasPrefix(line, body) {
		t.Errorf("截断结果不是原行的前缀:\ngot=%q", body)
	}
	if len(body) == 0 {
		t.Errorf("截断后不该是空的")
	}
}

// TestTruncateGrepLineUnits 直接测截断函数：切口必须落在字符边界。
func TestTruncateGrepLineUnits(t *testing.T) {
	cases := []string{
		strings.Repeat("中", 300),          // 切口在 3 字节字符中间
		strings.Repeat("é", 300),          // 2 字节字符
		strings.Repeat("𝄞", 300),          // 4 字节字符
		strings.Repeat("a", 300),          // 纯 ASCII
		strings.Repeat("中", 66) + "abcde", // 切口正好在字符边界
	}
	for _, s := range cases {
		got := truncateGrepLine(s)
		if !utf8.ValidString(got) {
			t.Errorf("截断结果不是合法 UTF-8: %q", got)
		}
		if strings.ContainsRune(got, utf8.RuneError) {
			t.Errorf("截断结果含替换符: %q", got)
		}
		if !strings.HasSuffix(got, "...") {
			t.Errorf("截断结果应带省略标记: %q", got)
		}
		// 截断后必须仍是原串的**字符前缀**
		if !strings.HasPrefix(s, strings.TrimSuffix(got, "...")) {
			t.Errorf("截断结果不是原串的前缀: %q", got)
		}
	}
	// 不超长 ⇒ 原样返回
	if got := truncateGrepLine("short"); got != "short" {
		t.Errorf("短行不该被截断: %q", got)
	}
}

// TestTrimIncompleteRune 去掉尾部残缺的 UTF-8 序列，完整的不动。
func TestTrimIncompleteRune(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"abc", "abc"},
		{"中文", "中文"},
		{"中", "中"},
		{"abc\xE4", "abc"},                     // 「中」的头字节
		{"abc\xE4\xB8", "abc"},                 // 「中」的两个字节
		{"abc\xE4\xB8\xAD", "abc\xE4\xB8\xAD"}, // 完整
		{"abc\xF0\x9D", "abc"},                 // 4 字节字符的前 3 字节
		{"abc\xFF", "abc\xFF"},                 // 非法字节：原样保留（不是"不完整"）
	}
	for _, c := range cases {
		got := string(trimIncompleteRune([]byte(c.in)))
		if got != c.want {
			t.Errorf("trimIncompleteRune(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestGrepLongLineCutKeepsUTF8Boundary 超长行（> 4 MB）被截断时，
// 切口也不能落在字符中间。
func TestGrepLongLineCutKeepsUTF8Boundary(t *testing.T) {
	dir := t.TempDir()
	// 一个 > 4 MB 的中文字符行（3 字节/字）⇒ 上限处必然切在字符中间
	huge := strings.Repeat("中", 1400000) // 4.2 MB
	p := filepath.Join(dir, "huge.txt")
	if err := os.WriteFile(p, []byte("MATCH\n"+huge+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	g := newGrepTool(NewFileTools(dir))
	out, err := g.Call(context.Background(), "", `{"pattern":"MATCH"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !utf8.ValidString(out) {
		t.Errorf("输出不是合法 UTF-8（超长行切口切在字符中间）")
	}
	if strings.ContainsRune(out, utf8.RuneError) {
		t.Errorf("输出里有替换符 U+FFFD")
	}
	// 命中行本身（"MATCH"）必须还在
	if !strings.Contains(out, "MATCH") {
		t.Errorf("命中行丢了:\n%s", out[:min2(len(out), 200)])
	}
}

func min2(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var _ = fmt.Sprintf
