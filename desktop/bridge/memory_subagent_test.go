package main

// 子 agent 的记忆边界（官方 memory#how-it-works，2026-09-30 补）。
//
// 官方原文三句，逐句对应一条测试：
//
//	"The main conversation's auto memory **isn't loaded into subagents**; the
//	 exception is a **fork**, which inherits the parent conversation and system
//	 prompt. **A subagent's own auto memory, enabled with the subagent `memory`
//	 field, is a separate directory.**"
//
// 这补的是 Wiki §5.2 记的那处误判（「subagent 记忆继承官方未规定，本轮明确不做」）——
// 官方规定了。

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/memory"
	"github.com/seven7628/hai-harness/subagent"
)

// TestSubagentDoesNotInheritMainMemory：主会话的 auto memory **不进子 agent**。
//
// 这条我们一直是对的（buildPersonaLoop 从不带 WithMemoryInjection），但一直是
// 「碰巧没接」而不是「被断言保证」。官方把它写成了明确要求，所以钉住。
func TestSubagentDoesNotInheritMainMemory(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	h := newMemoryHost(memory.Config{Directory: filepath.Join(home, "mem")})

	// 主会话有记忆：索引里有一条，且注入了目录说明。
	dir, err := h.resolve(context.Background(), ws)
	if err != nil || dir == nil {
		t.Fatalf("解析主目录失败: %v", err)
	}
	if err := os.MkdirAll(dir.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir.Path, memory.IndexFileName),
		[]byte("- main_only_topic.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mainInj := h.injection(context.Background(), ws)
	if !strings.Contains(mainInj, "main_only_topic.md") {
		t.Fatalf("前提失败：主会话应注入索引，实际：\n%s", mainInj)
	}

	// 未声明 memory 的子 agent：不注入任何东西（连目录说明都没有——它压根不该知道
	// 有记忆这回事，否则会往主目录里写孤儿）。
	if got := h.subagentInjection(context.Background(), ws, ""); got != "" {
		t.Fatalf("未声明 memory 的子 agent 不该注入，得到：%q", got)
	}
}

// TestSubagentMemoryIsSeparateDirectory：声明 memory: true → 拿到**自己那份**目录，
// 且与主目录不相交。
func TestSubagentMemoryIsSeparateDirectory(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	h := newMemoryHost(memory.Config{Directory: filepath.Join(home, "mem")})

	main, err := h.resolve(context.Background(), ws)
	if err != nil || main == nil {
		t.Fatalf("解析主目录失败: %v", err)
	}
	sub, ok := h.cfg.SubagentDir(*main, "docs-writer")
	if !ok {
		t.Fatal("SubagentDir 应当成功")
	}

	// 官方原词就是 separate directory：不相交是硬要求，不相交才让 postWrite
	// 能靠路径判定归属（钩子拿不到调用者身份）。
	if sub.Path == main.Path {
		t.Fatalf("子 agent 目录与主目录相同：%q", sub.Path)
	}
	if strings.HasPrefix(sub.Path, main.Path+string(filepath.Separator)) {
		t.Fatalf("子 agent 目录落在主目录**之内**：%q ⊂ %q", sub.Path, main.Path)
	}
	if !strings.Contains(sub.Path, string(filepath.Separator)+"agents"+string(filepath.Separator)) {
		t.Fatalf("子 agent 目录应在 agents/ 命名空间下：%q", sub.Path)
	}

	// 注入的是**子 agent 那份**：给子目录写一条索引，主目录不受影响。
	if err := os.MkdirAll(sub.Path, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub.Path, memory.IndexFileName),
		[]byte("- sub_only_topic.md\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	inj := h.subagentInjection(context.Background(), ws, "docs-writer")
	if !strings.Contains(inj, "sub_only_topic.md") {
		t.Fatalf("应注入子 agent 自己的索引：\n%s", inj)
	}
	if strings.Contains(inj, "main_only_topic.md") {
		t.Fatalf("子 agent 不该看到主会话的记忆：\n%s", inj)
	}
	if !strings.Contains(inj, sub.Path) {
		t.Fatalf("注入段必须指向子 agent 自己的目录 %q：\n%s", sub.Path, inj)
	}
}

// TestSubagentWriteStampsItsOwnDir：子 agent 写入 → 补戳落在**它自己**那份记忆上，
// 主目录的同名文件不受影响。
//
// 这是整套设计的关键一环：主 agent 与子 agent 共用同一个 ToolEngine，钩子只拿到
// absPath。它能正确分流，靠的就是两个目录不相交。
func TestSubagentWriteStampsItsOwnDir(t *testing.T) {
	home := t.TempDir()
	ws := t.TempDir()
	h := newMemoryHost(memory.Config{Directory: filepath.Join(home, "mem")})

	main, err := h.resolve(context.Background(), ws)
	if err != nil || main == nil {
		t.Fatalf("解析主目录失败: %v", err)
	}
	sub, ok := h.cfg.SubagentDir(*main, "docs-writer")
	if !ok {
		t.Fatal("SubagentDir 应当成功")
	}
	for _, d := range []memory.Dir{*main, sub} {
		if err := os.MkdirAll(d.Path, 0o700); err != nil {
			t.Fatal(err)
		}
	}

	// 主目录与子目录各放一个同名 topic：若归属判错，补戳会落到另一个文件上。
	name := "same-name.md"
	body := "---\ntype: project\n---\n\nbody\n"
	mainTopic := filepath.Join(main.Path, name)
	subTopic := filepath.Join(sub.Path, name)
	for _, p := range []string{mainTopic, subTopic} {
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := h.postWrite(context.Background(), ws, subTopic); err != nil {
		t.Fatalf("子目录写入不该报错: %v", err)
	}

	subGot, _ := os.ReadFile(subTopic)
	if !strings.Contains(string(subGot), "modified:") {
		t.Fatalf("子 agent 的 topic 应被补 modified：\n%s", subGot)
	}
	mainGot, _ := os.ReadFile(mainTopic)
	if strings.Contains(string(mainGot), "modified:") {
		t.Fatalf("主目录的同名 topic 不该被这次写入影响：\n%s", mainGot)
	}
}

// TestSubagentMemoryOffByDefault：未声明 memory 的定义默认不启用。
//
// 默认开着会串味：子 agent 没被注入主索引（官方要求），却完全能往主目录写，
// 产出没人索引的孤儿记忆。所以默认关，且这条要有测试。
func TestSubagentMemoryOffByDefault(t *testing.T) {
	def := loadSubagentDef(t, "plain",
		"---\nname: plain\ndescription: 不声明 memory 的子 agent\n---\n\n正文\n")
	if def.Memory {
		t.Fatal("未声明 memory 的定义不应启用子 agent 记忆")
	}
}

// TestSubagentMemoryFieldParsing：`memory: true` 真的被解析出来（含字符串写法）。
func TestSubagentMemoryFieldParsing(t *testing.T) {
	cases := map[string]struct {
		meta string
		want bool
	}{
		"布尔 true":  {"memory: true", true},
		"字符串 yes":  {"memory: \"yes\"", true},
		"布尔 false": {"memory: false", false},
		"未声明":      {"tools: [\"Read\"]", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			def := loadSubagentDef(t, "a",
				"---\nname: a\ndescription: d\n"+tc.meta+"\n---\n\n正文\n")
			if def.Memory != tc.want {
				t.Fatalf("Memory = %v, want %v", def.Memory, tc.want)
			}
		})
	}
}

// loadSubagentDef 写一个定义文件并经**注册表**加载（走的是生产路径：
// NewDefinitionRegistry → Load，而不是直接调解析器）。
func loadSubagentDef(t *testing.T, name, body string) *subagent.AgentDefinition {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, name+".md"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	reg, err := subagent.NewDefinitionRegistry(dir)
	if err != nil {
		t.Fatalf("建注册表失败: %v", err)
	}
	def, err := reg.Load(name)
	if err != nil {
		t.Fatalf("加载定义 %q 失败: %v", name, err)
	}
	return def
}
