package memory

import (
	"strings"
	"testing"
)

// 说明段必须让模型拿到写记忆所需的**全部**输入：目录在哪、文件长什么样。
// 缺任何一条，Step 4 的 modified 钩子就仍然不会触发（guidance.go 顶部的闭环）。
func TestGuidance_StatesPathAndShape(t *testing.T) {
	d := memDir(t)
	g := guidance(d, true)

	mustContain := []string{
		d.Path,        // 目录绝对路径：模型唯一的写入目标
		IndexFileName, // 索引文件名
		"frontmatter", // 记忆文件的形态：官方 modified 规则挂在 frontmatter 上
		"200",         // 行限额
		"25000",       // 字节限额
		"user",        // 四种 kind 的取值域（官方 §2.2 逐字）
		"feedback",    //
		"project",     //
		"reference",   //
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
	g := guidance(memDir(t), true)

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
	g := guidance(memDir(t), true)
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
	with, without := guidance(d, true), guidance(d, false)

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
	g := guidance(memDir(t), false)
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

	inj, err := (Config{}).BuildInjection(d)
	if err != nil {
		t.Fatal(err)
	}
	// 剥掉宿主加的部分后，剩下的必须正好是索引正文。
	_, body := boundIndex(d, []byte("- user_role.md\n- feedback_testing.md\n"))
	want := indexHeading + "\n\n" + body + "\n\n" + guidance(d, true)
	if inj.Text != want {
		t.Fatalf("说明段与索引正文混在了一起：\n%q\nwant\n%q", inj.Text, want)
	}
}
