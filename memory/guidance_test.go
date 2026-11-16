package memory

import (
	"strings"
	"testing"
)

// `## Memory Guide` 必须让模型拿到写记忆所需的最小完整集：目录、索引行规则、
// type 语义、查重、时效、注入是子集。少任何一条，模型就会写出一份宿主读不懂
// 或者白白稀释预算的记忆。
func TestGuidance_StatesPathAndShape(t *testing.T) {
	d := memDir(t)
	g := guidance(d, InjectionFull, 0)

	mustContain := []string{
		d.Path,                    // 目录绝对路径：模型唯一的写入目标
		IndexFileName,             // 索引文件名
		"not an entry",            // 解析器硬规则：没有文件名就不算条目
		"200", "25000",            // 限额
		"`user`", "`feedback`", "`project`", "`reference`", // 四种 type 的语义
		"Check for an existing entry first",                // 查重
		"snapshot of what was true",                        // 时效性
	}
	for _, want := range mustContain {
		if !strings.Contains(g, want) {
			t.Fatalf("Memory Guide 缺少 %q：\n%s", want, g)
		}
	}
}

// 自我约束 1：说明段**只陈述存储形态，不规定行为**（上一版 6 条行为协议实测
// 采纳率 12%~62%，要先做 A/B 才配写进来）。
func TestGuidance_CarriesNoBehaviouralRules(t *testing.T) {
	g := guidance(memDir(t), InjectionFull, 0)
	forbidden := []string{
		"不要每次", "不必每次", "每次都",
		"可从代码推导", "能从代码",
		"CLAUDE.md",
		"请记录", "记得写", "应该写", "务必",
		"不要创建", "Do not create", "you should", "make sure to",
	}
	for _, bad := range forbidden {
		if strings.Contains(g, bad) {
			t.Fatalf("说明段混入了行为判据 %q：\n%s", bad, g)
		}
	}
}

// 自我约束 2：指引性祈使句不出现。
func TestGuidance_HasNoImperativeAdvice(t *testing.T) {
	g := guidance(memDir(t), InjectionFull, 0)
	for _, bad := range []string{"请", "务必", "应该", "建议", "Please ", "You must"} {
		if strings.Contains(g, bad) {
			t.Fatalf("说明段出现指引性措辞 %q：\n%s", bad, g)
		}
	}
}

// 每会话都付的固定开销，写死上限防止"顺手多加一句"。
func TestGuidance_StaysSmall(t *testing.T) {
	g := guidance(memDir(t), InjectionFull, 0)
	const limit = 1600 // 装下四种 type 的语义 + 查重 + 时效之后的上限
	if len(g) > limit {
		t.Fatalf("Memory Guide %d 字节，超过上限 %d：\n%s", len(g), limit, g)
	}
}

// 文档里的索引行样例必须真的能被宿主解析器认成一条记忆。
// 样例解析不出来 = 教模型写出永远不进注入的行，而且这条路径没有任何报错。
func TestGuidance_IndexExampleIsParseable(t *testing.T) {
	g := guidance(memDir(t), InjectionFull, 0)
	var example string
	for _, line := range strings.Split(g, "\n") {
		if i := strings.Index(line, "`- [name](file.md)"); i >= 0 {
			example = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line[i:]), "`"))
			example = strings.TrimSuffix(example, "`")
			break
		}
	}
	if example == "" {
		t.Fatalf("说明段里找不到索引行样例：\n%s", g)
	}
	title, target, summary, ok := splitLocatorFull(example)
	if !ok {
		t.Fatalf("样例解析不出定位符：%q", example)
	}
	if title != "name" || target != "file.md" {
		t.Fatalf("样例解析成 title=%q target=%q，期望 name / file.md", title, target)
	}
	if summary == "" {
		t.Fatalf("样例缺摘要：%q", example)
	}
}

// 反向：行内没有文件名时不算条目——说明段这么承诺，解析器就必须这样行为。
func TestGuidance_LineWithoutFilenameIsNotAMemory(t *testing.T) {
	for _, line := range []string{"标题 — 一句话摘要，没有文件名", "只是分组说明文字"} {
		if _, _, ok := splitLocator(line); ok {
			t.Fatalf("%q 不该被认成一条记忆", line)
		}
	}
}

// 注入必须是**两段**：Guide 在前、Memory 在后，且 Memory 段承载索引正文。
func TestInjectionHasTwoSections(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "- [甲](a.md) — 甲的摘要\n")

	inj, err := (Config{}).BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	gi := strings.Index(inj.Text, memoryGuideHeading)
	mi := strings.Index(inj.Text, memoryHeading)
	if gi < 0 || mi < 0 {
		t.Fatalf("两段标题缺失：\n%s", inj.Text)
	}
	if gi > mi {
		t.Fatalf("Memory Guide 应在 Memory 之前：\n%s", inj.Text)
	}
	mem := inj.Text[mi:]
	if !strings.Contains(mem, "甲") || !strings.Contains(mem, "a.md") {
		t.Fatalf("Memory 段应含条目名与路径：\n%s", mem)
	}
}

// 没有索引时 `## Memory` 段明说还没有记忆——不能空着，那会让模型以为内容丢了。
func TestInjectionEmptyMemorySaysSo(t *testing.T) {
	d := memDir(t)
	inj, err := (Config{}).BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(inj.Text, noMemoryYet) {
		t.Fatalf("无记忆时应在 Memory 段写明：\n%s", inj.Text)
	}
	if strings.Index(inj.Text, memoryGuideHeading) > strings.Index(inj.Text, noMemoryYet) {
		t.Fatal("Memory Guide 应在前")
	}
}
