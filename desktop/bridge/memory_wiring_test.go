package main

// Project Memory 接线的端到端验证（Step 3 注入 + Step 4 modified 钩子）。
//
// 这些测试存在的理由：接线本身没有别的护栏。memory 包的 61 项单测全绿、agents 与
// tools 的既有测试也全绿，但它们各自都**不知道对方存在**——一个接线漏字、闸门写反、
// 或路径比较写错，都会呈现为"测试全过、记忆功能完全不工作"。
//
// 所以这里测的是**跨包行为**：真实 write_file 写到真实记忆目录里，frontmatter 的
// modified 必须被补上；关掉开关，同一条路径必须一个字节都不动。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/memory"
	"github.com/seven7628/hai-harness/tools/builtin"
)

// newTestMemHost 造一个指向临时记忆根的 memoryHost（不依赖真实 ~/.go-code）。
func newTestMemHost(t *testing.T, root string, enabled bool) *memoryHost {
	t.Helper()
	cfg := memory.Config{Directory: root}
	if !enabled {
		cfg.Enabled = memory.Bool(false)
	}
	return newMemoryHost(cfg)
}

// memTopicDir 造一个"看起来像 git 仓库"的记忆目录：直接给 memoryHost 注入已解析的
// 目录，绕开 git 身份解析（那部分由 memory/identity_test.go 负责）。
func memTopicDir(t *testing.T) (*memoryHost, *memory.Dir, string) {
	t.Helper()
	root := t.TempDir()
	h := newTestMemHost(t, root, true)
	dir := memory.Dir{Path: filepath.Join(root, "repos", "testid")}
	if err := os.MkdirAll(dir.Path, 0o700); err != nil {
		t.Fatalf("建记忆目录: %v", err)
	}
	h.dir = &dir
	return h, &dir, root
}

// TestPostWriteStampsTopicFrontmatter：模型用标准 write_file 写记忆 topic，
// modified 必须被补上。这是 Step 4 的**唯一**存在理由。
func TestPostWriteStampsTopicFrontmatter(t *testing.T) {
	h, dir, _ := memTopicDir(t)
	topic := filepath.Join(dir.Path, "relay_tunnel_id.md")
	body := "---\ntype: project\n---\n\nThe tunnel identifier is `q7-tunnel-corp`.\n"
	if err := os.WriteFile(topic, []byte(body), 0o600); err != nil {
		t.Fatalf("预置 topic: %v", err)
	}

	ws := t.TempDir()
	ft := builtin.NewFileTools(ws).
		WithPostWrite(func(p string) error { return h.postWrite(context.Background(), ws, p) })
	// 记忆目录在工作区之外 —— 必须用**绝对路径**。这正是官方形态：模型被告知索引在
	// 工作区外某处，用标准 write_file 写绝对路径过去。若这里改用相对路径而写到工作区
	// 内的同名文件，测试会假通过而生产仍然坏。
	if err := callWriteFile(ft, topic, body); err != nil {
		t.Fatalf("write_file 应成功: %v", err)
	}

	got, err := os.ReadFile(topic)
	if err != nil {
		t.Fatalf("读回: %v", err)
	}
	if !strings.Contains(string(got), "modified:") {
		t.Fatalf("frontmatter 未被补 modified：\n%s", got)
	}
	// 原有字段必须原样保留（行级原地编辑，不是 yaml 往返）
	if !strings.Contains(string(got), "type: project") {
		t.Fatalf("原有 type 字段丢失：\n%s", got)
	}
	if !strings.Contains(string(got), "q7-tunnel-corp") {
		t.Fatalf("正文被改写：\n%s", got)
	}
}

// TestMemoryPathIsReachableByFileTools：记忆目录必须对标准文件工具**可达**。
//
// 这条是接线的前提，缺了它上面那条测试也会假通过（测试用的临时目录不在
// ~/.go-code 下，绕过了生产里那道保护）。官方形态没有记忆专用工具，模型只有
// read_file / write_file / edit_file —— 记忆目录被 deny 罩住的话，模型既读不到
// 注入索引指向的 topic，也写不进新记忆，auto memory 整个不成立。
func TestMemoryPathIsReachableByFileTools(t *testing.T) {
	home := t.TempDir()
	topic := filepath.Join(home, ".go-code", "memory", "repos", "id", "t.md")
	if err := os.MkdirAll(filepath.Dir(topic), 0o700); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	if err := os.WriteFile(topic, []byte("---\ntype: project\n---\n\nx\n"), 0o600); err != nil {
		t.Fatalf("写: %v", err)
	}
	t.Setenv("HOME", home)
	ft := builtin.NewFileTools(t.TempDir())
	for _, tl := range ft.Tools() {
		if tl.Name() != "read_file" {
			continue
		}
		_, err := tl.Call(context.Background(), "read_file", `{"path":`+jsonQuote(topic)+`}`)
		if err != nil {
			t.Fatalf("read_file 读不到记忆目录（deny 名单未放行 memory 子树）: %v", err)
		}
		return
	}
	t.Fatal("未注册 read_file")
}

// TestPostWriteDisabledIsInert：开关关闭时，同一条写入路径必须**一个字节都不动**。
//
// 这是"关闭 = 完全不生效"的全部语义。只接了读侧（不注入）而漏了写侧，用户关了
// 记忆却在背后继续生成文件 —— 比不实现更糟。
func TestPostWriteDisabledIsInert(t *testing.T) {
	root := t.TempDir()
	h := newTestMemHost(t, root, false)
	dir := memory.Dir{Path: filepath.Join(root, "repos", "testid")}
	if err := os.MkdirAll(dir.Path, 0o700); err != nil {
		t.Fatalf("建目录: %v", err)
	}
	topic := filepath.Join(dir.Path, "x.md")
	body := "---\ntype: project\n---\n\nbody\n"
	if err := os.WriteFile(topic, []byte(body), 0o600); err != nil {
		t.Fatalf("预置: %v", err)
	}
	before, _ := os.Stat(topic)
	beforeBody, _ := os.ReadFile(topic)

	// 开关关闭时，路径判定也必须直接短路（不建 .lock、不解析身份）
	if err := h.postWrite(context.Background(), t.TempDir(), topic); err != nil {
		t.Fatalf("关闭时不该报错: %v", err)
	}

	after, _ := os.Stat(topic)
	afterBody, _ := os.ReadFile(topic)
	if string(beforeBody) != string(afterBody) {
		t.Fatalf("关闭开关时正文被改了")
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("关闭开关时文件被重写（mtime 变化）")
	}
	entries, _ := os.ReadDir(dir.Path)
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".lock") {
			t.Fatalf("关闭开关时仍建了锁侧车文件：%s", e.Name())
		}
	}
}

// TestPostWriteIgnoresNonMemoryPaths：工作区内的普通写入不触发任何记忆逻辑。
func TestPostWriteIgnoresNonMemoryPaths(t *testing.T) {
	h, dir, _ := memTopicDir(t)
	ws := t.TempDir()
	// ① 工作区内的 .md
	if err := h.postWrite(context.Background(), ws, filepath.Join(ws, "README.md")); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	// ② 记忆目录内的 MEMORY.md（索引，不是 topic）
	if err := h.postWrite(context.Background(), ws, dir.Path+string(filepath.Separator)+"MEMORY.md"); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	// ③ 相对路径直接拒
	if err := h.postWrite(context.Background(), ws, "relative/x.md"); err != nil {
		t.Fatalf("不该报错: %v", err)
	}
	// 记忆目录里除 MEMORY.md 外不应多出任何 .md
	entries, _ := os.ReadDir(dir.Path)
	if len(entries) != 0 {
		t.Fatalf("非记忆写入产生了文件：%v", entries)
	}
}

// TestPostWriteEditFileAlsoStamps：edit_file 也走同一个钩子。
//
// 官方说的是"文件被写入时补 modified"，edit_file 同样是写入。若只接 write_file，
// 模型用 edit_file 改记忆时时间戳会静默停更，而索引和 topic 看上去完全正常。
func TestPostWriteEditFileAlsoStamps(t *testing.T) {
	h, dir, _ := memTopicDir(t)
	topic := filepath.Join(dir.Path, "y.md")
	if err := os.WriteFile(topic, []byte("---\ntype: project\n---\n\nold body\n"), 0o600); err != nil {
		t.Fatalf("预置: %v", err)
	}
	ws := t.TempDir()
	ft := builtin.NewFileTools(ws).
		WithPostWrite(func(p string) error { return h.postWrite(context.Background(), ws, p) })
	if err := callEditFile(ft, topic, "old body", "new body"); err != nil {
		t.Fatalf("edit_file 应成功: %v", err)
	}
	got, _ := os.ReadFile(topic)
	if !strings.Contains(string(got), "modified:") {
		t.Fatalf("edit_file 未触发补戳：\n%s", got)
	}
	if !strings.Contains(string(got), "new body") {
		t.Fatalf("edit 未生效：\n%s", got)
	}
}

// TestNoMemoryLeavesPromptByteIdentical：没有记忆时，系统提示词逐字节不变。
//

// TestInjectionEmptyWithoutIndex：有身份、无索引时不得注入。
func TestInjectionEmptyWithoutIndex(t *testing.T) {
	h, _, _ := memTopicDir(t) // 目录存在但没有 MEMORY.md
	if got := h.injection(context.Background(), t.TempDir()); got != "" {
		t.Fatalf("无索引时不应注入，得到：%q", got)
	}
}

// TestInjectionReturnsIndexWhenPresent：有索引时必须注入其内容。
func TestInjectionReturnsIndexWhenPresent(t *testing.T) {
	h, dir, _ := memTopicDir(t)
	idx := filepath.Join(dir.Path, "MEMORY.md")
	if err := os.WriteFile(idx, []byte("- relay_tunnel_id.md\n- release_gate.md\n"), 0o600); err != nil {
		t.Fatalf("写索引: %v", err)
	}
	got := h.injection(context.Background(), t.TempDir())
	for _, want := range []string{"relay_tunnel_id.md", "release_gate.md"} {
		if !strings.Contains(got, want) {
			t.Fatalf("注入缺少 %q：\n%s", want, got)
		}
	}
}

// TestInjectionDisabledReturnsEmpty：开关关闭时注入为空。
func TestInjectionDisabledReturnsEmpty(t *testing.T) {
	h := newTestMemHost(t, t.TempDir(), false)
	if got := h.injection(context.Background(), t.TempDir()); got != "" {
		t.Fatalf("关闭开关时不该注入，得到 %q", got)
	}
}

// ---------- 小工具 ----------

func callWriteFile(ft *builtin.FileTools, abs, content string) error {
	for _, tl := range ft.Tools() {
		if tl.Name() != "write_file" {
			continue
		}
		args := `{"path":` + jsonQuote(abs) + `,"content":` + jsonQuote(content) + `}`
		ctx := context.Background()
		if err := tl.ValidParams(ctx, tl.Name(), args); err != nil {
			return err
		}
		_, err := tl.Call(ctx, tl.Name(), args)
		return err
	}
	return os.ErrNotExist
}

func callEditFile(ft *builtin.FileTools, abs, oldS, newS string) error {
	for _, tl := range ft.Tools() {
		if tl.Name() != "edit_file" {
			continue
		}
		args := `{"path":` + jsonQuote(abs) +
			`,"old_string":` + jsonQuote(oldS) + `,"new_string":` + jsonQuote(newS) + `}`
		ctx := context.Background()
		if err := tl.ValidParams(ctx, tl.Name(), args); err != nil {
			return err
		}
		_, err := tl.Call(ctx, tl.Name(), args)
		return err
	}
	return os.ErrNotExist
}

func jsonQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// TestEmptyStateAssertionSkipsOnlyForWiredPackage：基线断言的跳过是**有条件的**。
//
// Step 3/4 接线后 desktop/bridge 成为 importer，基线在产品树上不成立是已知且有意的，
// 装置应当跳过。但护栏要防的是"不知道自己在测什么"，所以**出现任何其他 importer
// 仍然必须硬失败**。这条测试把两个分支都钉住 —— 否则将来某次意外接线会被这个
// 跳过静默吞掉，而那正是"基线数字悄悄变成测实现"的路径。
func TestEmptyStateAssertionSkipsOnlyForWiredPackage(t *testing.T) {
	// 前提：接线已完成，desktop/bridge 就是那个 importer
	offenders := baselineMemoryImporters(t)
	if len(offenders) == 0 {
		t.Skip("当前树未接线，本测试的前提不成立")
	}
	// 只允许 desktop/bridge 一个 importer
	if len(offenders) != 1 || offenders[0] != "github.com/seven7628/hai-harness/desktop/bridge" {
		t.Fatalf("接线白名单被打破，importer = %v", offenders)
	}
}
