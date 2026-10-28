package prompt

import (
	"strings"
	"testing"
)

// hasCJK 检测字符串是否含 CJK 统一表意文字(BMP 范围)。
// 提示词一律英文(规范第 1 条),含 CJK 视为规范违例。
func hasCJK(s string) bool {
	for _, r := range s {
		if r >= 0x4E00 && r <= 0x9FFF {
			return true
		}
	}
	return false
}

// allPrompts 全部提示词常量(含注入层标题),用于规范断言。
// 2026-09-18(C3 死代码清理):V1 常量 DesktopCode / DesktopExplore 已删除(生产零引用),
// 本表同步移除对应两项——它只做「非空 + 全英文」两条规范断言,不是常量存在性契约;
// 常量本身的删除依据与核实过程见 docs/PROMPT_CATALOG_CN.md §12。
var allPrompts = map[string]string{
	"DefaultSystemPrompt":      DefaultSystemPrompt,
	"SummaryPrompt":            SummaryPrompt,
	"SummaryPromptV2":          SummaryPromptV2,
	"PlanModeHint":             PlanModeHint,
	"CompactContinueReminder":  CompactContinueReminder,
	"CompactHandoffReminder":   CompactHandoffReminder,
	"CompactHandoffReminderV2": CompactHandoffReminderV2,
	"PendingTasksReminder":     PendingTasksReminder,
	"TaskAnchorReminder":       TaskAnchorReminder,
	"CompactTodoReminder":      CompactTodoReminder,
	"SkillManifestHeader":      SkillManifestHeader,
	"LoadedSkillHeader":        LoadedSkillHeader,
	"DesktopCodePromptV2":      DesktopCodePromptV2,
	"DesktopExploreV2":         DesktopExploreV2,
	"DesktopWork":              DesktopWork,
	"FlappyMain":               FlappyMain,
	"FlappyReview":             FlappyReview,
}

// TestPromptsNonEmpty 全部提示词非空。
func TestPromptsNonEmpty(t *testing.T) {
	for name, p := range allPrompts {
		if strings.TrimSpace(p) == "" {
			t.Errorf("%s: empty prompt", name)
		}
	}
}

// TestPromptsEnglish 全部提示词无中文(规范第 1 条:提示词本体一律英文)。
func TestPromptsEnglish(t *testing.T) {
	for name, p := range allPrompts {
		if hasCJK(p) {
			t.Errorf("%s: contains CJK characters, system prompts must be English", name)
		}
	}
}

// TestDefaultSystemPromptSections 行为契约层保持行为规则分节(字节稳定基准)。
func TestDefaultSystemPromptSections(t *testing.T) {
	for _, s := range []string{"## Working method", "## Approval and safety", "## Background tasks", "## Runtime reminders"} {
		if !strings.Contains(DefaultSystemPrompt, s) {
			t.Errorf("DefaultSystemPrompt missing section %q", s)
		}
	}
}

// TestDefaultSystemPromptBackgroundTasks 后台任务行为契约（S2-A）：spawn 后不重复
// 其范围、running 结果不重复等待、任务结果作为新证据而非用户指令。
func TestDefaultSystemPromptBackgroundTasks(t *testing.T) {
	if !strings.Contains(DefaultSystemPrompt, "agent_spawn") {
		t.Error("DefaultSystemPrompt missing agent_spawn mention")
	}
	for _, s := range []string{"do not duplicate", "do not re-spawn", "never call the same tool again to wait", "new evidence"} {
		if !strings.Contains(DefaultSystemPrompt, s) {
			t.Errorf("DefaultSystemPrompt Background tasks missing clause %q", s)
		}
	}
}

// TestDefaultSystemPromptHonesty 行为契约层含诚实工作与语言跟随句。
func TestDefaultSystemPromptHonesty(t *testing.T) {
	if !strings.Contains(DefaultSystemPrompt, "Work honestly") ||
		!strings.Contains(DefaultSystemPrompt, "never inventing results") {
		t.Error("DefaultSystemPrompt missing honesty clause")
	}
	if !strings.Contains(DefaultSystemPrompt, "follow the user's language") {
		t.Error("DefaultSystemPrompt missing language-following clause")
	}
}

// TestSummaryPromptDoubleLayer 压缩提示词含 <analysis>/<summary> 双层输出。
func TestSummaryPromptDoubleLayer(t *testing.T) {
	for _, s := range []string{"<analysis>", "</analysis>", "<summary>", "</summary>"} {
		if !strings.Contains(SummaryPrompt, s) {
			t.Errorf("SummaryPrompt missing %q (double-layer output)", s)
		}
	}
}

// TestSummaryPromptSecurityClause 压缩提示词含安全敏感指令逐字保留条款。
func TestSummaryPromptSecurityClause(t *testing.T) {
	if !strings.Contains(SummaryPrompt, "Security-relevant") ||
		!strings.Contains(SummaryPrompt, "verbatim") {
		t.Error("SummaryPrompt missing security-preservation clause")
	}
}

// TestSummaryPromptSections 压缩提示词保留 10 section 标题(旧测试依赖)。
func TestSummaryPromptSections(t *testing.T) {
	for _, s := range []string{
		"Main Requests and Intent", "Key Decisions", "File Changes Summary",
		"Fixed Issues", "User Inputs", "Open Tasks", "Current Task",
		"Background Tasks", "Suggested Next Steps", "File Context",
	} {
		if !strings.Contains(SummaryPrompt, "## "+s) {
			t.Errorf("SummaryPrompt missing section %q", "## "+s)
		}
	}
}

// TestSummaryPromptVerbatimGoal 压缩提示词要求任务目标逐字引用(防任务目标被浓缩)。
func TestSummaryPromptVerbatimGoal(t *testing.T) {
	if !strings.Contains(SummaryPrompt, "quoted verbatim") {
		t.Error("SummaryPrompt missing verbatim-goal clause")
	}
}

// TestSummaryPromptCarryOver 压缩提示词要求滚动合并继承未完成事项(信息单调不递减)。
func TestSummaryPromptCarryOver(t *testing.T) {
	if !strings.Contains(SummaryPrompt, "Carry over unfinished items") {
		t.Error("SummaryPrompt missing carry-over clause")
	}
}

// TestSummaryPromptPriority 压缩提示词声明预算优先级(Critical sections 优先)。
func TestSummaryPromptPriority(t *testing.T) {
	if !strings.Contains(SummaryPrompt, "allocate the budget there") {
		t.Error("SummaryPrompt missing budget-priority clause")
	}
}

// TestSummaryPromptAuthority 压缩提示词声明 [FileLedger]/[TaskStates] 权威优先。
func TestSummaryPromptAuthority(t *testing.T) {
	if !strings.Contains(SummaryPrompt, "[FileLedger]") || !strings.Contains(SummaryPrompt, "authoritative") {
		t.Error("SummaryPrompt missing authoritative-ledger clause")
	}
}

// TestSummaryPromptV2Clauses V2 压缩提示词关键条款（2026-08-24 压缩优化方案，
// 标准：docs/PROMPT_CATALOG_CN.md §10.1.1）。
func TestSummaryPromptV2Clauses(t *testing.T) {
	for _, s := range []string{
		"## Continuation State",                               // S1 首节
		"await user input",                                    // S1 防漂移守卫
		"map, not a reading list",                             // S2 地图语义
		"At most 10 entries",                                  // S2 条数上限
		"Verbatim copy of the engine-provided [TodoSnapshot]", // S5 Task List 权威
		"Engine-generated compaction",                         // S6 自描述行
		"under ~10 lines",                                     // S6 analysis 限额
		"MUST appear in the input",                            // L2 路径来源约束
		"[truncated ...]",                                     // C6 截断感知
		"[TaskStates] block) you MUST include",                // B3 触发条件显式化
		"[FileLedger] (files touched",                         // 输入方括号块说明
		"The example below shows FORMAT only",                 // S8 示例块硬约束
		"Anti-patterns observed in practice",                  // S8 反例
		"<example>\n<analysis>",                               // S8 示例含 analysis 层（防照抄漏层）
		"~4000 tokens",                                        // S4 预算
		"the verbatim Task List copy does not count",          // S4 Task List 豁免
	} {
		if !strings.Contains(SummaryPromptV2, s) {
			t.Errorf("SummaryPromptV2 missing clause %q", s)
		}
	}
}

// TestSummaryPromptV2Sections V2 保留全部 V1 节标题并新增两节（滚动合并与护栏依赖英文标题）。
func TestSummaryPromptV2Sections(t *testing.T) {
	for _, s := range []string{
		"Continuation State", "Main Requests and Intent", "Task List", "Key Decisions",
		"File Changes Summary", "Fixed Issues", "User Inputs", "Open Tasks",
		"Current Task", "Background Tasks", "Suggested Next Steps", "File Context",
	} {
		if !strings.Contains(SummaryPromptV2, "## "+s) {
			t.Errorf("SummaryPromptV2 missing section %q", "## "+s)
		}
	}
}

// TestCompactHandoffReminderV2Clauses V2 交接提醒关键条款（目录 §10.3.1）。
func TestCompactHandoffReminderV2Clauses(t *testing.T) {
	for _, s := range []string{
		"Continuation State",                           // 单步继续
		"do not bulk re-read",                          // 显式禁批量重读
		"todo_update; do not rebuild it with todo_add", // todo 权威
		"map, not a reading list",                      // 地图语义
	} {
		if !strings.Contains(CompactHandoffReminderV2, s) {
			t.Errorf("CompactHandoffReminderV2 missing clause %q", s)
		}
	}
}

// TestPendingTasksReminderClauses stop 检查点在途任务提醒关键条款（目录 §10.8）。
func TestPendingTasksReminderClauses(t *testing.T) {
	for _, s := range []string{
		"Background tasks are still running",
		"Do not report their outcomes",
		"do not declare the overall task complete",
	} {
		if !strings.Contains(PendingTasksReminder, s) {
			t.Errorf("PendingTasksReminder missing clause %q", s)
		}
	}
}

// TestIncompleteTodosReminderClauses 已随门控删除（2026-10）：收尾前完成度检查（旧
// agent_loop 门控 4）连同 prompt.IncompleteTodosReminder 一并移除——实测 145 次触发里
// 123 次的下一轮只调了 todo_update，用最贵的机制做记账。回滚见 docs/PROMPT_CATALOG_CN.md §10.8 的移除说明。

// TestDesktopCodePromptV2Clauses 桌面 Code 唯一简洁系统提示词关键条款（2026-08-25
// 裁决：单一 DesktopCodePromptV2，主会话与 Spawn 子 Agent 共用）。
func TestDesktopCodePromptV2Clauses(t *testing.T) {
	for _, s := range []string{
		"## Harness", // 公共行为契约节（工具/审批/提醒）
		"A rejected call means the user declined", // 审批拒绝边界
		"<system-remind>",                         // 运行时提醒语义
		"## Background Tasks",                     // 后台任务契约节
		"do not re-spawn",                         // 不重复派发
		"never call the same tool again to wait",  // 不空转等待
		"do not declare the user's request complete while remaining work depends on in-flight tasks", // 完成判定
		"install into the current workspace's .agents/skills",                                        // 对话安装 skills 默认工作区 .agents/skills
		"global ~/.agents/skills", // 全局安装需显式要求
	} {
		if !strings.Contains(DesktopCodePromptV2, s) {
			t.Errorf("DesktopCodePromptV2 missing clause %q", s)
		}
	}
}

// TestTodoImmediateUpdateClause todo 即时更新纪律（2026-08-25 增补）：完成一项
// 立即 todo_update 置为完成，不攒到最后批量更新——DefaultSystemPrompt（SDK 层）
// 与 DesktopCodePromptV2（桌面 Code/Spawn）两常量必须同步生效。
func TestTodoImmediateUpdateClause(t *testing.T) {
	for name, p := range map[string]string{
		"DefaultSystemPrompt": DefaultSystemPrompt,
		"DesktopCodePromptV2": DesktopCodePromptV2,
	} {
		for _, s := range []string{
			"todo_update immediately when it is completed",
			"never defer status updates to an end-of-task batch",
		} {
			if !strings.Contains(p, s) {
				t.Errorf("%s missing todo-immediate-update clause %q", name, s)
			}
		}
	}
}

// TestDesktopCodePromptV2Concise 简洁性守卫（2026-08-25 裁决；2026-09 增补）：
// 工程流程节（Understand the task / Work loop / Recovery / Verification）已从
// 提示词移除；Change discipline / Reporting 为用户特意移除（2026-09，最小变更与
// 汇报纪律由模型默认能力承担）。防止后续编辑悄悄膨胀回完整版形态。
func TestDesktopCodePromptV2Concise(t *testing.T) {
	for _, s := range []string{
		"## Understand the task",
		"## Work loop",
		"## Recovery",
		"## Verification",
		"## Harness contract",  // 旧完整版 V2 的节名，已被 ## Harness 取代
		"## Change discipline", // 用户特意移除（最小变更由模型默认能力承担）
		"## Reporting",         // 用户特意移除（汇报纪律由模型默认能力承担）
	} {
		if strings.Contains(DesktopCodePromptV2, s) {
			t.Errorf("DesktopCodePromptV2 must stay concise but contains %q", s)
		}
	}
}

// TestDesktopExploreV2Clauses Explore Persona V2 bash 负面清单（目录 §7 建议目标文案）。
func TestDesktopExploreV2Clauses(t *testing.T) {
	for _, s := range []string{
		"read-only inspection",                         // bash 保留 + 用途
		"must not change anything",                     // 负面清单总则
		"commit/push/pull/checkout/reset/clean/revert", // VCS 状态变更类
		"npm/pip install",                              // 安装下载类
		"state in your reply what you would need",      // 出口条款
	} {
		if !strings.Contains(DesktopExploreV2, s) {
			t.Errorf("DesktopExploreV2 missing clause %q", s)
		}
	}
}

// TestOutputFormatStyleContract 展示样式契约：**分工**而非全文（2026-10 修订）。
//
// 历史：2026-09-13 曾把沙箱机制细节（不继承样式 / 底色为白 / 必须自带 <style> /
// 否则是浏览器默认样式）整段写进系统提示词，因为当时实测模型产出裸 <table>。
//
// 现在这些细节**已归位到 visual-ui 技能**（references/ 下有沙箱约束表、主题变量、
// 12 个带 <style> 的模板骨架），且技能 description 每轮都由 buildSkillManifest 注入。
// 把它们再抄进系统提示词 = 同一件事每轮付两遍钱（实测：提示词 557 字符 + 技能描述
// 433 字符都在讲"何时用 html 块"），且两处必然漂移——事实已经发生过一次：
// 「底色为白」是深色画布（markdown.tsx 的 canvasCss 注入 --hai-bg，深色主题近黑）
// 之前的遗留说法，留着只会让模型在深色主题下放浅色硬编码块。
//
// 故提示词只保留两条**技能承载不了**的信息：
//  1. 默认 GFM（技能只讲"要图就用我"，讲不了"能不用就别用"——这是全局优先级）；
//  2. 沙箱不继承应用样式（这一条即使技能被用户禁用也必须生效，是兜底红线）。
func TestOutputFormatStyleContract(t *testing.T) {
	for name, p := range map[string]string{
		"DesktopCodePromptV2": DesktopCodePromptV2,
		"DesktopWork":         DesktopWork,
	} {
		low := strings.ToLower(p)
		for _, s := range []string{
			"prefer gfm",  // 默认 GFM（应用已配色、随主题适配）
			"```html```",  // html 块仍是可用出口
			"inherits no", // 兜底红线：沙箱不继承应用样式（技能被禁用时仍要生效）
		} {
			if !strings.Contains(low, s) {
				t.Errorf("%s missing output-style clause %q", name, s)
			}
		}
		// 机制细节不得回流：它们归 visual-ui 技能，抄回提示词就是重复计费 + 漂移源。
		for _, s := range []string{
			"white background",      // 已是主题感知画布（深色主题近黑），这句是错的
			"its own <style>",       // 归技能：requirements 表 + 模板骨架已写明
			"bare browser defaults", // 归技能：Where the HTML actually runs 一节
		} {
			if strings.Contains(low, s) {
				t.Errorf("%s reintroduced sandbox mechanism %q — it belongs to the "+
					"visual-ui skill (duplicated context + guaranteed drift)", name, s)
			}
		}
	}
	// 反例守卫：不得退回「html 块作为表格首选」的旧表述（表格走 GFM 即可，自带配色）。
	for name, p := range map[string]string{
		"DesktopCodePromptV2": DesktopCodePromptV2,
		"DesktopWork":         DesktopWork,
	} {
		if strings.Contains(p, "use ```html``` blocks for tables") {
			t.Errorf("%s regressed to html-blocks-for-tables guidance", name)
		}
	}
}

// TestDesktopWorkMentionsOfficeExecutionChannel work persona（桌面办公助理）没有 shell：
// run_python（受管 Python；python-docx/openpyxl/python-pptx/pypdf/pypdfium2/mammoth 预装）
// 加四个随包技能（docx/xlsx/pptx/pdf）才是它的执行通道。2026-09 前 Tools 段只列文件工具
// 与「不要跑命令」约束，run_python 一个字都没提 → 模型对 docx/xlsx/pdf 只能回「本会话
// 不支持」（能力其实已在工具集里，见 docs/office/TECH_PLAN.md §3.6）。此测试钉住两件事：
// 提示词必须点名这些能力，且引导顺序是 load_skill（拿 gotcha/流程）→ run_python（执行）。
func TestDesktopWorkMentionsOfficeExecutionChannel(t *testing.T) {
	for _, s := range []string{
		"run_python",  // 唯一执行通道（work 无 bash）
		"load_skill",  // 技能渐进披露入口
		"python-docx", // 四个办公库（模型据此知道格式真的能读）
		"openpyxl",
		"python-pptx",
		"pypdf",
		"docx/xlsx/pptx/pdf", // 随包技能覆盖的格式集合
	} {
		if !strings.Contains(DesktopWork, s) {
			t.Errorf("DesktopWork missing %q — work 的办公能力必须写进提示词（否则模型宣布不支持）", s)
		}
	}
	// 引导顺序：同一行里先 load_skill 再 run_python（先拿流程，再执行）。
	ordered := false
	for _, line := range strings.Split(DesktopWork, "\n") {
		li, ri := strings.Index(line, "load_skill"), strings.Index(line, "run_python")
		ordered = ordered || (li >= 0 && ri > li)
	}
	if !ordered {
		t.Error("DesktopWork 缺「load_skill → run_python」的引导顺序（同一行内）")
	}
}

// TestDesktopCodePromptV2NoPollingInvitation 桌面 Code 主会话的唯一系统提示词是
// DesktopCodePromptV2（desktop/bridge/main.go: systemPrompt = prompt.DesktopCodePromptV2，
// 且 replaceBase 时 WithBaseSystemPrompt("") 停用 SDK Base）——该会话收不到
// DefaultSystemPrompt，故「反轮询 + wait_seconds」契约必须在本常量内自洽；
// 旧句 "Use TaskList to check status" 在 Code 会话里是唯一的轮询指令，
// 实测（2026-09-18 审计）轮询 221 次/最近一周，历史最高。
func TestDesktopCodePromptV2NoPollingInvitation(t *testing.T) {
	if !strings.Contains(DesktopCodePromptV2, "wait_seconds") {
		t.Error("DesktopCodePromptV2 must point at TaskOutput's wait_seconds (the blocking mechanism)")
	}
	if strings.Contains(DesktopCodePromptV2, "Use TaskList to check status") {
		t.Error("DesktopCodePromptV2 must not invite polling via \"Use TaskList to check status\"")
	}
}

// TestDefaultSystemPromptVerificationAndPollingClauses 契约层（Work/Explore 用；
// Code 走 DesktopCodePromptV2，见上）2026-09-18 增补的两条：
// 验证后才算完成、反轮询指向 wait_seconds 机制。
func TestDefaultSystemPromptVerificationAndPollingClauses(t *testing.T) {
	for _, s := range []string{
		"Verify before you finish",
		"run the relevant check (build / test / lint)",
		"Background results arrive on their own",
		"wait_seconds",
	} {
		if !strings.Contains(DefaultSystemPrompt, s) {
			t.Errorf("DefaultSystemPrompt missing clause %q", s)
		}
	}
	if strings.Contains(DefaultSystemPrompt, "Use TaskList to check status") {
		t.Error("DefaultSystemPrompt must not invite polling")
	}
}
