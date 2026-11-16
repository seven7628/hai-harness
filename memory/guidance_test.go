package memory

import (
	"strings"
	"testing"
)

// 常驻段必须让模型拿到**缺了就写不对**的最小集：目录、索引登记规则、注入是子集。
// 详细规则（type 语义、查重、引用核对、索引行样例）自 2026-11 起在 .state/GUIDE.md，
// 由 TestGuideDocCarriesTheDetails 单独钉住——那边断的是内容，这边断的是"还给不给得出指针"。
func TestGuidance_StatesPathAndShape(t *testing.T) {
	d := memDir(t)
	g := guidance(d, true, InjectionFull, 0)

	mustContain := []string{
		d.Path,        // 目录绝对路径：模型唯一的写入目标
		IndexFileName, // 索引文件名
		"行内要带文件名",     // 解析器硬规则：没有文件名就不算条目
		// 按需指南的入口用**相对路径**：绝对路径每会话多付约 90 字节，
		// 而"相对上一行的记忆目录"已无歧义。guidePath(d) 是可解析的落点，测试在那儿钉。
		".state/" + GuideFileName,
		"200",   // 行限额
		"25000", // 字节限额
	}
	for _, want := range mustContain {
		if !strings.Contains(g, want) {
			t.Fatalf("说明段缺少 %q：\n%s", want, g)
		}
	}
}

// 自我约束 1：说明段**只陈述存储形态，不规定行为**。
//
// 上一版自创 6 条行为规则（"不要每次都写"、"跳过可从代码推导的"…）内部互相矛盾，
// 实测采纳率 12%~62%。官方对应的判据是连续变量，要先做 A/B 对照实验才能决定加不加。
// 这条测试的作用是让"顺手加一句劝告"在 CI 上直接失败，而不是再走一轮才发现。
func TestGuidance_CarriesNoBehaviouralRules(t *testing.T) {
	g := guidance(memDir(t), true, InjectionFull, 0)

	// 逐条对应官方 §2.2 里"还没做 A/B、所以一律不加"的行为判据。
	forbidden := []string{
		"不要每次", "不必每次", "每次都",
		"可从代码推导", "能从代码", "重复的",
		"CLAUDE.md",
		"请记录", "记得写", "应该写", "务必",
		"不要创建", "Do not create",
	}
	for _, bad := range forbidden {
		if strings.Contains(g, bad) {
			t.Fatalf("说明段混入了行为判据 %q——它必须等 A/B 实验：\n%s", bad, g)
		}
	}
}

// 自我约束 2：每一句都要能指回官方原文。指引性的祈使句（"请…"/"务必…"）在官方
// auto memory 的注入里没有对应物，是我们上一版翻车的文体特征。
func TestGuidance_HasNoImperativeAdvice(t *testing.T) {
	g := guidance(memDir(t), true, InjectionFull, 0)
	for _, bad := range []string{"请", "务必", "应该", "建议"} {
		if strings.Contains(g, bad) {
			t.Fatalf("说明段出现指引性措辞 %q：\n%s", bad, g)
		}
	}
}

// 无索引与有索引只差"当前尚无记忆"一句——其余完全相同。
//
// 这样"文件不存在"与"文件存在但没条目"在模型看来是同一件事（都还没有记忆），
// 不会因为文件碰巧存在就被读成"你的索引空了"（那像是什么丢了）。
func TestGuidance_EmptyDiffersOnlyByAbsenceLine(t *testing.T) {
	d := memDir(t)
	with, without := guidance(d, true, InjectionFull, 0), guidance(d, false, InjectionFull, 0)

	if with == without {
		t.Fatal("hasIndex 不影响输出")
	}
	const absence = "当前尚无记忆"
	if !strings.Contains(without, absence) {
		t.Fatalf("无索引时应说明尚无记忆：\n%s", without)
	}
	if strings.Contains(with, absence) {
		t.Fatalf("有索引时不得说尚无记忆：\n%s", with)
	}
	// 去掉那一句之后两者必须逐字节相同（而不是"差不多"）。
	trimmed := strings.Replace(without, "\n\n"+absence+"（MEMORY.md 不存在或为空）。\n", "\n", 1)
	if trimmed != with {
		t.Fatalf("去掉“尚无记忆”一句后应与有索引版逐字节相同：\n%q\nwant\n%q", trimmed, with)
	}
}

// 说明段是**无条件注入**的（只要记忆开关开着且能解析出目录），所以它的体积
// 直接乘进每一个会话的固定开销。这条测试把上限写死，防止"顺手多加一句"。
//
// 现状 ~600 字节。参照：官方限额本身是 200 行 / 25000 字节，说明段不到其 3%。
// 真正的防线是那三条自我约束（形态、不发明行为、每句可溯源），这条只是兜底。
func TestGuidance_StaysSmall(t *testing.T) {
	g := guidance(memDir(t), false, InjectionFull, 0)
	const limit = 1200
	if len(g) > limit {
		t.Fatalf("说明段 %d 字节，超过上限 %d——它在每个会话里无条件注入：\n%s", len(g), limit, g)
	}
}

// 说明段不是索引内容：它不得混入任何索引里的条目名，
// 否则"模型写的清单"与"宿主加的话"就再也分不开了（评估时要靠这个边界归因）。
func TestGuidance_DoesNotEchoIndexEntries(t *testing.T) {
	d := memDir(t)
	writeIndex(t, d, "- user_role.md\n- feedback_testing.md\n")

	inj, err := (fullCfg()).BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	// 剥掉宿主加的部分后，剩下的必须正好是索引正文。
	_, body := boundIndex(d, []byte("- user_role.md\n- feedback_testing.md\n"))
	want := indexHeading + "\n\n" + body + "\n\n" + guidance(d, true, InjectionFull, 0)
	if inj.Text != want {
		t.Fatalf("说明段与索引正文混在了一起：\n%q\nwant\n%q", inj.Text, want)
	}
}

// 文档与解析器必须对齐：指南里给出的那行样例，必须真的能被宿主认成一条记忆。
//
// 这条测试防的是一类静默失效：索引行由模型手写，注入/恢复只认"行内有定位符"的行。
// 指南若给出一个解析器不认的样例（或解析器日后收紧而文档没跟上），模型就会照着
// 写出永远不进注入的索引行——而这条路径没有任何报错，只有"怎么注入里没有它"。
// 2026-11 起样例从常驻说明段搬到了 .state/GUIDE.md，所以测试也跟过去。
func TestGuide_IndexExampleIsParseable(t *testing.T) {
	doc := guideDoc("/mem")
	var example string
	for _, line := range strings.Split(doc, "\n") {
		if strings.Contains(line, "[Title](file.md)") {
			example = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "- "))
			break
		}
	}
	if example == "" {
		t.Fatalf("指南里找不到索引行样例：\n%s", doc)
	}

	target, summary, ok := splitLocator(example)
	if !ok {
		t.Fatalf("指南给的索引行样例解析不出定位符：%q", example)
	}
	if target != "file.md" {
		t.Fatalf("样例定位符 = %q，期望 file.md", target)
	}
	if summary == "" {
		t.Fatalf("样例没有摘要部分：%q", example)
	}
	names := parseIndexEntries(example)
	if len(names) != 1 || names[0] != "file.md" {
		t.Fatalf("parseIndexEntries(%q) = %v，期望 [file.md]", example, names)
	}
}

// 反向：行内没有文件名时，说明段承诺"不算作一条记忆"——解析器必须真的这样行为。
func TestGuidance_LineWithoutFilenameIsNotAMemory(t *testing.T) {
	for _, line := range []string{
		"- 标题 — 一句话摘要，没有文件名",
		"- 只是分组说明文字",
	} {
		if _, _, ok := splitLocator(strings.TrimPrefix(line, "- ")); ok {
			t.Fatalf("%q 不该被认成一条记忆", line)
		}
	}
}
