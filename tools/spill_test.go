package tools

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// 截断即落盘（WithSpillDir）的行为契约。
//
// 起因（实测本机历史事件）：680 条工具结果被 20 KB 截断，累计丢掉 **1.17 GB** 中段
// （最惨单条 122 MB，来自 `grep -rn --include=*` 扫到巨型文件）。模型能看到的只有
// 「[省略 N bytes]」和一句「Narrow the output」—— 除了重跑没有别的出路。
// 落盘把「永久丢失」变成「路径在手、按需分段读」。

// TestSpillOnTruncateWritesFullText 落盘的是**未截断**全文（不是截断后的文本），
// 并且路径被写进模型的可见文案里（否则模型不知道去哪读）。
func TestSpillOnTruncateWritesFullText(t *testing.T) {
	// 目录**不存在**（引擎自己建）：权限断言才有意义（t.TempDir 本身是 0755）
	dir := filepath.Join(t.TempDir(), "spill")
	e := NewToolEngine(WithMaxResponseSize(10), WithSpillDir(dir))
	s := "HEAD" + strings.Repeat("m", 20) + "TAIL" // 28 字节，中段 18 字节被省
	got := e.truncate(s, "call-abc/123")           // 含 '/' 的调用 id 也要能落成文件名

	if !strings.Contains(got, "18 bytes omitted from the middle") {
		t.Fatalf("want byte accounting preserved: %q", got)
	}
	if strings.Contains(got, "Narrow the output") {
		t.Fatalf("落盘后不该再让模型去 re-run: %q", got)
	}
	path := spillPathOf(t, got)
	if !strings.HasPrefix(path, dir) {
		t.Fatalf("spill 路径应在配置目录内：%q（dir=%q）", path, dir)
	}
	if !strings.Contains(filepath.Base(path), "call-abc_123") {
		t.Fatalf("文件名应带（安全化的）调用 id：%q", filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("spill 文件应可读：%v", err)
	}
	if string(data) != s {
		t.Fatalf("落盘的必须是**未截断**全文：got %d bytes, want %d", len(data), len(s))
	}
	if fi, err := os.Stat(path); err == nil && fi.Mode().Perm() != 0o600 {
		t.Fatalf("工具输出可能含密钥，文件权限应 0600，实际 %o", fi.Mode().Perm())
	}
	if fi, err := os.Stat(dir); err == nil && fi.Mode().Perm() != 0o700 {
		t.Fatalf("目录权限应 0700，实际 %o", fi.Mode().Perm())
	}
}

// TestSpillOffKeepsLegacyText 未配置 spillDir = 历史行为**逐字节**不变。
// 这条是回归护栏：默认装配没接线时，任何文案漂移都会击穿 provider 前缀缓存。
func TestSpillOffKeepsLegacyText(t *testing.T) {
	e := NewToolEngine(WithMaxResponseSize(10))
	s := "HEAD" + strings.Repeat("m", 20) + "TAIL"
	got := e.truncate(s, "call-1")
	want := "HEADm\n\n[... 18 bytes omitted from the middle (64.29% of 28 bytes total); showing the first 5 and the last 5 bytes — both ends are present, the middle is not. Narrow the output to see it all (e.g. `head`/`-l`/`wc`, a smaller path or pattern) ...]\n\nmTAIL"
	if got != want {
		t.Fatalf("未配置 spillDir 时文案必须逐字节不变：\n got=%q\nwant=%q", got, want)
	}
}

// TestSpillFailureIsSilent 落盘失败不能改变截断路径（磁盘满/目录不可写）：
// 返回历史文案，不 panic，不产生半截文件。
func TestSpillFailureIsSilent(t *testing.T) {
	dir := t.TempDir()
	blocked := filepath.Join(dir, "not-a-dir")
	if err := os.WriteFile(blocked, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}
	e := NewToolEngine(WithMaxResponseSize(10), WithSpillDir(blocked))
	s := "HEAD" + strings.Repeat("m", 20) + "TAIL"
	got := e.truncate(s, "call-1")
	if strings.Contains(got, "written to") {
		t.Fatalf("落盘失败却声称写了文件：%q", got)
	}
	if !strings.Contains(got, "Narrow the output") {
		t.Fatalf("落盘失败应退回历史文案：%q", got)
	}
}

// TestSpillShortOutputNotWritten 未触发截断就不落盘（否则每个小结果都留一个文件）。
func TestSpillShortOutputNotWritten(t *testing.T) {
	dir := t.TempDir()
	e := NewToolEngine(WithMaxResponseSize(1000), WithSpillDir(dir))
	if got := e.truncate("short", "call-1"); got != "short" {
		t.Fatalf("短结果不该改动：%q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("未截断时不该产生 spill 文件，实际 %d 个", len(entries))
	}
}

// TestSafeFileToken 调用 id 来自 provider，可能是任意字符串（含路径分隔符、空白、
// 超长）；文件名片段必须只留安全字符且有界。
func TestSafeFileToken(t *testing.T) {
	cases := map[string]string{
		"":                 "call",
		"call_0_abc-DEF.1": "call_0_abc-DEF.1",
		"../../etc/passwd": ".._.._etc_passwd",
		"a b\tc":           "a_b_c",
	}
	for in, want := range cases {
		if got := safeFileToken(in); got != want {
			t.Errorf("safeFileToken(%q) = %q, want %q", in, got, want)
		}
	}
	long := strings.Repeat("x", 200)
	if got := safeFileToken(long); len(got) > 64 {
		t.Errorf("文件名片段应 ≤64，实际 %d", len(got))
	}
}

// spillPathOf 从文案里取回落盘路径（`... written to <path> — read ...`）。
func spillPathOf(t *testing.T, text string) string {
	t.Helper()
	const marker = "written to "
	i := strings.Index(text, marker)
	if i < 0 {
		t.Fatalf("文案里没有落盘路径：%q", text)
	}
	rest := text[i+len(marker):]
	if j := strings.Index(rest, " — "); j > 0 {
		rest = rest[:j]
	}
	p := strings.TrimSpace(rest)
	if p == "" {
		t.Fatalf("落盘路径为空：%q", text)
	}
	return p
}

// bigOutputTool 返回超大文本的工具（模拟 `grep -rn --include=*` 扫到巨型文件）。
type bigOutputTool struct{ mockTool }

func (b *bigOutputTool) Call(context.Context, string, string) (string, error) {
	return "HEAD" + strings.Repeat("x", 50_000) + "TAIL", nil
}

// TestSpillThroughExecute 走**真实的执行管线**（不是直接调 truncate）：
// 断言调用 id 被正确带进落盘文件名 —— 这条线断掉时，截断照常发生、只是路径不见了，
// 是那种「不报错、只是少了东西」的静默退化，所以必须由集成级用例钉住。
func TestSpillThroughExecute(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spill")
	e := NewToolEngine(WithMaxResponseSize(200), WithSpillDir(dir))
	e.RegisterTool(context.Background(), &bigOutputTool{mockTool{name: "big"}})

	results, err := e.Sequence(context.Background(), []core.ToolCall{{
		Id: "call-spill-42", Name: "big", Arguments: "{}",
	}}, nil)
	if err != nil {
		t.Fatalf("Sequence: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("want 1 result, got %d", len(results))
	}
	out := results[0].Result
	path := spillPathOf(t, out)
	if !strings.Contains(filepath.Base(path), "call-spill-42") {
		t.Fatalf("文件名应带调用 id：%q", filepath.Base(path))
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("spill 文件应可读：%v", err)
	}
	if len(data) != 50_008 {
		t.Fatalf("全文应完整落盘：%d bytes, want 50008", len(data))
	}
	if strings.Contains(out, "Narrow the output") {
		t.Fatalf("落盘后不该再让模型 re-run：%q", out)
	}
}
