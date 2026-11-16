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
func guidance(d Dir, tier string, omitted int) string {
	// 这一段是 `## Memory Guide`：教模型**怎么写**记忆。
	//
	// 英文：模型的系统提示与工具描述都是英文语境，混排会让"读一遍就能照做"的那部分变慢。
	// 中文只留源码注释与给人看的界面。
	//
	// 结构照着 Claude 的 Memory 规范写（path → 文件形态 → 四种 type → 索引 → 查重/反例
	// → 时效 → 限额），差异只出现在宿主行为不同的地方，且都写明了：
	//   - 记忆文件用 `type` + `title`，**不是** `name` + `description`：摘要由索引行承载，
	//     宿主启动时不逐条读文件（读 64 个文件换一份摘要，不值）；
	//   - 错误记忆是**归档**（可恢复）而不是删除：宿主不提供删除动作；
	//   - 没有 `[[双链]]`：宿主不解析它，指别处请写 `file_path`。
	var b strings.Builder

	b.WriteString("You have a persistent file-based memory at `")
	b.WriteString(d.Path)
	b.WriteString("/`. Each memory is one `.md` file holding one fact, with frontmatter:\n\n")

	b.WriteString("```markdown\n---\ntype: user | feedback | project | reference\n")
	b.WriteString("title: <short-kebab-case-slug>\n---\n\n<the fact>\n```\n\n")

	b.WriteString("For `feedback` and `project` entries, follow the fact with **Why:** and ")
	b.WriteString("**How to apply:** lines — the reason is what makes the memory reusable later. ")
	b.WriteString("To point at another memory, use its `file_path`.\n\n")

	b.WriteString("- `user` — who the user is: role, expertise, working preferences.\n")
	b.WriteString("- `feedback` — guidance the user has given on how to work: both corrections and ")
	b.WriteString("confirmed approaches; include the why.\n")
	b.WriteString("- `project` — ongoing work, goals or constraints not derivable from the code or git ")
	b.WriteString("history; convert relative dates to absolute.\n")
	b.WriteString("- `reference` — pointers to external resources: docs, dashboards, tickets.\n\n")

	b.WriteString("After writing the file, add one line to `")
	b.WriteString(IndexFileName)
	b.WriteString("`: `- [Title](file.md) — one-line description`. ")
	b.WriteString("**A line without a filename is not an entry** — it never reaches this prompt and ")
	b.WriteString("the archive tool cannot see it. `")
	b.WriteString(IndexFileName)
	b.WriteString("` is loaded every session: one line per memory, no frontmatter, never put memory ")
	b.WriteString("content there.\n\n")

	b.WriteString("Each entry under `## Memory` below is a `<memory>` block with `name`, `description` ")
	b.WriteString("and `file_path`. The description is not truncated; when it is not enough, read the ")
	b.WriteString("body at `file_path`.\n\n")

	b.WriteString("Check for an existing entry first: update that file instead of creating a second ")
	b.WriteString("one. Duplicates dilute the startup budget and are reported by the inventory.\n\n")

	b.WriteString("Don't save what the repository already records (code structure, past fixes, git ")
	b.WriteString("history) or what only matters to this conversation; if asked to remember one of ")
	b.WriteString("those, ask what was non-obvious about it and save that instead.\n\n")

	b.WriteString("A memory is a snapshot of what was true when it was written. Verify that a file, ")
	b.WriteString("function or flag it names still exists before relying on it — the inventory reports ")
	b.WriteString("dead code anchors, but only after the fact.\n\n")

	b.WriteString("A memory that turns out to be wrong is **archived, not deleted**: the host moves the ")
	b.WriteString("file out of the directory and keeps a snapshot, so it can be restored. ")
	b.WriteString("Deleting memory files yourself bypasses that.\n\n")

	if tier == InjectionHot && omitted > 0 {
		b.WriteString("Startup loads only the ")
		b.WriteString(strconv.Itoa(HotListSize))
		b.WriteString(" entries with the highest read count (not all of them) and stops at a byte ")
		b.WriteString("budget; read the full index and any memory body with the standard file tools.")
	} else {
		b.WriteString("Startup loads only the first ")
		b.WriteString(strconv.Itoa(MaxIndexLines))
		b.WriteString(" lines / ")
		b.WriteString(strconv.Itoa(MaxIndexBytes))
		b.WriteString(" bytes of `")
		b.WriteString(IndexFileName)
		b.WriteString("`, whichever comes first; memory bodies are not loaded automatically — read them ")
		b.WriteString("with the standard file tools.")
	}

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
