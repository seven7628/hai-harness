package memory

import (
	"strconv"
	"strings"
)

// 注入段落末尾的「记忆目录说明」（Step 3 的一部分，2026-09-30 新增）。
//
// # 这段文本为什么存在
//
// 此前 Step 3 **只注入索引正文**，不注入任何说明，理由记为「官方没有记忆协议」。
// 冷启动因此无解，而且是无解成一个闭环：
//
//	模型不知道记忆目录在哪 → 不写 → 没有 MEMORY.md → 不注入 → 仍不知道路径
//
// Step 4 的 `modified` 钩子只在文件**落入记忆目录**时才触发，所以它不是"概率性
// 不触发"，是**恒不触发**——Step 3/4 接线那两轮（93b9a87）测的是这条链的形状，
// 没有人验证过它的触发条件在真实会话里能否成立。答案是不能。
//
// # 为什么现在敢写：反推，不是引文
//
// 官方文档确实**没有**给出 Claude Code auto memory（A）的注入样例。但可以反推：
//
//	模型若不知道记忆目录的绝对路径 → 官方「Claude reads MEMORY.md」不可能发生
//	模型若不被告知要写 frontmatter  → 官方「type 记在 frontmatter」不可能发生
//
// 前提是「官方 auto memory 是能正常工作的产品」。故 A 必然告知了路径与格式，
// **存在性确定、形态未知**。推导过程见 Wiki《HAI-Harness-Memory官方原文摘录》§4.1。
//
// 注意这与 API memory tool（B）无关：B 的协议原文不能证明 A 的行为，两者是两个
// 产品面（Wiki §3）。这里用的是反推，不是拿 B 当证据。
//
// # 三条自我约束（写死在这里，以免后来者"顺手优化"回上一版那 6 条协议）
//
//  1. **只陈述存储形态，不规定行为。** 上一版自创了 6 条行为规则（"不要每次都写"
//     "跳过可从代码推导的"…），内部互相矛盾，实测采纳率 12%~62%。官方对应的判据
//     （§2.2）是**连续变量**："加了会不会更好"必须先做 A/B 对照实验才成立，
//     在那之前它们不属于这里。见 Wiki §6.1。
//  2. **每一句都要能指回官方原文。** 指不回去的不写。
//  3. **不需要劝模型去写。** 2026-09-29 的真实 provider 实验：只告知目录位置，
//     模型 4/5 主动写；不告知的对照臂 0/5。**路径本身就是全部邀请**，
//     额外的一句"请记录你的发现"反而是自创协议的开始。
//
// # 为什么放在索引**之后**而不是之前
//
// 与 truncationNotice 同一个理由：索引是这一段的正文，模型按行读它时最先看到的
// 应该是清单而不是说明。说明放末尾，只在模型读完清单、准备据此行动时生效。
// 冷启动（无索引）时说明就是全部内容，位置无差别。

// guidance 拼出记忆目录说明段。
//
// hasIndex 为 false 时额外加一句「当前尚无记忆」——这是本次加载的既成事实，
// 不是引导。不加的话模型会把"说明里列了格式、清单却是空的"读成记忆丢失，
// 而实际上只是还没写过。
func guidance(d Dir, hasIndex bool, tier string, omitted int) string {
	var b strings.Builder

	b.WriteString("记忆目录：")
	b.WriteString(d.Path)
	b.WriteString("\n")

	if !hasIndex {
		b.WriteString("\n当前尚无记忆（MEMORY.md 不存在或为空）。\n")
	}

	// 三条形态事实，逐条对应官方原文：
	//   ① "MEMORY.md # Index, one line per memory"（§2.3）
	//   ② "Claude records the kind as a `type` field in the memory file's frontmatter"
	//      + 四种 kind（§2.2）——这句是 `modified` 能触发的前提，官方对该字段的
	//      写法有明文，我们只是把它连同取值域一起告知模型。
	//   ③ "Claude doesn't load topic files at startup. Claude reads them on demand
	//      using its standard file tools."（§2.4）
	//
	// ②③④ 里的 200 行 / 25KB 也一并说明：官方
	// "This limit applies only to MEMORY.md"，模型需要知道限额只管索引，
	// 否则会误以为记忆正文也该写得很短。
	b.WriteString("\n- 一条记忆 = 该目录下的一个 .md 文件；文件名在索引 `")
	b.WriteString(IndexFileName)
	b.WriteString("` 中一行一条登记。\n")
	// 索引行的格式也是**形态**，不是行为要求。为什么要写出来：索引行由模型手写，
	// 而宿主侧认行的规则是"行内要有定位符"（文件名写在 `](…)`、反引号或裸词里）。
	// 没有定位符的行会被静默跳过——那条记忆永远不出现在启动注入里，而审计只在
	// 写入那一刻提醒一次。实测两个记忆库已经写出两种都能解析的写法（链接 / 反引号），
	// 但"写出一行解析不了的东西"这条失败路径此前一个字都没说明。
	// 示例由 indexLineExample 提供，guidance_test 会拿它去跑真实解析器。
	b.WriteString("- 索引行形如 " + indexLineExample +
		"：行内要有文件名（写在 `](…)` 或反引号里），没有文件名的行不算作一条记忆。\n")
	b.WriteString("- 有 frontmatter 时，`type` 取 ")
	b.WriteString(strings.Join(memoryTypes(), " / "))
	b.WriteString(" 之一。\n")
	// ④ 这一句必须**按档位如实陈述**，不能照抄官方那句"加载前 200 行"：
	// 官方描述的是全量注入，而 hot 档注入的是 Top-K 子集。说错了会让模型把
	// "注入里只有 K 条"读成"索引里只有 K 条"，进而重写索引或重记记忆——
	// 与 truncationNotice 要防的是同一个循环，只是入口不同。
	//
	// 只有**真的省略了条目**时才说"K 条"：索引本来就只有 3 条时照说"挑出的 K 条"
	// 会让模型以为还藏着若干条，然后去找一个不存在的东西。
	if tier == InjectionHot && omitted > 0 {
		b.WriteString("- 启动时只加载 `")
		b.WriteString(IndexFileName)
		b.WriteString("` 中按使用频次挑出的 ")
		b.WriteString(strconv.Itoa(HotListSize))
		b.WriteString(" 条（不是全部）；完整索引与记忆正文需要时用标准文件工具自行读取。")
	} else {
		b.WriteString("- 启动时只加载 `")
		b.WriteString(IndexFileName)
		b.WriteString("` 的前 ")
		b.WriteString(strconv.Itoa(MaxIndexLines))
		b.WriteString(" 行 / ")
		b.WriteString(strconv.Itoa(MaxIndexBytes))
		b.WriteString(" 字节（取先到者）；记忆正文不自动加载，需要时用标准文件工具自行读取。")
	}

	return b.String()
}

// indexLineExample 是说明段里给出的索引行样例。
//
// 单独提出来是为了让测试拿它去跑**真实解析器**（splitLocator / parseIndexEntries）：
// 文档里的示例一旦解析不出条目，这段说明就成了"教模型写出注入不认的行"的说明书。
const indexLineExample = "`- [标题](文件名.md) — 一句话摘要`"

// memoryTypes 官方的四种记忆 kind（§2.2，原文逐字）：
//
//	user       用户的角色、专长与工作偏好
//	feedback   用户给出的纠正，以及用户确认过的做法
//	project    无法从代码或 git 历史推导出的进行中工作、截止时间与决策
//	reference  项目之外的信息位置（issue 看板、仪表盘等）
//
// 官方只规定了这四个取值，没有规定"必须选一个"——所以这里陈述取值域而不规定
// 选择行为（自我约束 1）。
func memoryTypes() []string {
	return []string{
		"`user`", "`feedback`", "`project`", "`reference`",
	}
}
