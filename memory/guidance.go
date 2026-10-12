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
func guidance(d Dir, hasIndex bool) string {
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
	b.WriteString("- 记忆文件以 YAML frontmatter 开头，frontmatter 的 `type` 取 ")
	b.WriteString(strings.Join(memoryTypes(), " / "))
	b.WriteString(" 之一。\n")
	b.WriteString("- 启动时只加载 `")
	b.WriteString(IndexFileName)
	b.WriteString("` 的前 ")
	b.WriteString(strconv.Itoa(MaxIndexLines))
	b.WriteString(" 行 / ")
	b.WriteString(strconv.Itoa(MaxIndexBytes))
	b.WriteString(" 字节（取先到者）；记忆正文不自动加载，需要时用标准文件工具自行读取。")

	return b.String()
}

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
