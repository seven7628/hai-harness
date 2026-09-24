package prompt

// 按需注入的机制说明:替代 base 里被移除的机制教育,只在事件边界低频注入,
// 不常驻 base 前缀(对齐 Claude Code 的 system-reminder 思路——机制告诉模型
// "现在发生了什么",而不是常驻教育"系统怎么运作")。

// GoalAlignmentReminder is appended as a transient user message when a run has
// spent many decision rounds without a successful file change. The user role is
// intentional: it works with providers that accept only one system message.
// 中文说明：这是运行时信号，不是新用户需求；目标一致时忽略，偏离时重新聚焦，不能为提醒而强制改文件。
const GoalAlignmentReminder = `<system-remind>
I should check whether my current work still serves the user's requested outcome. If it does, I should ignore this reminder and continue. If it does not, I should refocus on the user's request and choose the next justified action. I must not change files merely to satisfy this reminder.
</system-remind>`

// 不重启重做。语义对应 base 移除的"对话可能压缩,harness 自动处理"教育,
// 改为只在真正压缩发生时出现。
// 用法:agent_loop.maybeCompact 压缩成功后追加为 system 消息(低频,压缩本已断层,
// 缓存代价可忽略)。
//
// 中文释义:"你的对话历史已被压缩成上方的摘要。直接从摘要与最新输入继续;
// 不要重启或重做已完成的工作。"
// 注:maybeCompact 现默认注入 CompactHandoffReminder(统一交接提醒,含锚点/文件账本/
// 任务状态);本常量保留兼容(独立使用或回退场景)。
const CompactContinueReminder = "Your conversation history was compacted into a summary above. Continue naturally from the summary and the most recent input; do not restart or redo work already completed."

// CompactHandoffReminder 压缩后统一交接提醒(S1-B/S1-E 提醒层补丁,替代单句
// CompactContinueReminder 的注入定位,常量保留兼容)。告诉续跑模型:
//   - 从摘要继续、不重启重做;
//   - 原始任务目标已由引擎逐字保留在 TaskAnchorReminder 消息中(如存在),视为权威;
//   - 文件内容已不在上下文,依赖细节前用 read_file 重读;
//   - %s = 引擎侧上下文清单段:文件账本 [FileLedger] + 后台任务状态 [TaskStates]
//     (任一不存在即为空,提醒退化为前三句)。
//
// 中文释义:"压缩后交接:对话历史已压缩为上方摘要,从摘要与最近输入继续,不重启
// 不重做;原始任务目标(如存在)已由引擎逐字保留在上方 TaskAnchorReminder 中,
// 视为权威;此前读取/修改的文件内容已不在上下文,需要细节时先 read_file 重读。
// %s 为引擎记录的文件账本与后台任务状态。"
const CompactHandoffReminder = `<system-reminder>Handoff after compaction:
- The conversation history was compacted into a summary above. Continue naturally from the summary and the most recent inputs; do not restart or redo work already completed.
- The original task goal, if present, was preserved verbatim by the engine in the TaskAnchorReminder message above — treat it as authoritative.
- Tool results and file contents read earlier are no longer in context — re-read files with read_file before relying on details.%s
</system-reminder>`

// TaskAnchorReminder 压缩后任务锚点注记(S1-A):引擎在压缩时逐字保留首条用户任务
// 指令(不经过摘要转述),注入摘要之后、近窗/最新输入之前——任务目标不随滚动合并
// 退化。%s = 锚点原文(引擎截断后,上限 taskAnchorMaxChars)。
// 用法:llm_compressor.Compact 输出组装(splitMessages 提取,锚点已在 latestUser
// 保留块内时跳过)。
//
// 中文释义:"本会话的原始任务目标——压缩时由引擎逐字保留、未经摘要转述——引用于下。
// 原始请求之后用户的追加指令见上方最近输入。"
const TaskAnchorReminder = `<system-reminder>This session's original task goal — preserved verbatim by the engine at compaction time, not rephrased by the summarizer — is quoted below. Additional user instructions after the original request are in the most recent inputs above:
%s
</system-reminder>`

// CompactTodoReminder 压缩后待办快照注入（事件边界一次性,不常驻 base）:
// 仅在 Config.TodoResetOnCompact=true（WithTodoResetOnCompact,默认 false）时使用——
// 默认语义下压缩不清空会话 todo(todo 是会话级独立状态,压缩只替换 Messages,见
// agent_loop.maybeCompact),无需重建。老产品依赖"压缩即清空"时显式开启。
// %s 替换为清单快照文本(todo.Store.List 输出)。
// 用法:agent_loop.maybeCompact 压缩成功后,先取快照 → Store.Clear → 注入本常量。
//
// 中文释义:"会话待办已在压缩时清空。这是压缩前的待办清单快照(如需新待办,
// 请调用 todo_add/todo_update 重建):\n<快照>"
const CompactTodoReminder = `<system-reminder>Session todos were cleared on compaction. Snapshot of the todo list before compaction (recreate with todo_add / todo_update if your plan needs them):
%s
</system-reminder>`

// CompactHandoffReminderV2 压缩后统一交接提醒 V2（2026-08-24 压缩优化方案，
// 标准：docs/PROMPT_CATALOG_CN.md §10.3.1；依据：方案文档 §6 H1）。
// 与 V1（CompactHandoffReminder）并存用于 A/B 对比与回滚；接线（maybeCompact 按
// SW2 选用）在第三批。
//
// 相对 V1 的核心变化：
//   - 单步继续：指向摘要 Continuation State 的单一下一步（弱模型唯一锚点）；
//   - 显式禁止批量重读（V1 的 "re-read before relying on details" 无范围限定，
//     是压缩后批量重读的直接触发点之一）；
//   - todo 快照权威：todo_update 推进而非 todo_add 重建（治压缩后清单漂移）；
//   - [FileLedger] 地图语义 + 即用即读 + 定位手段（targeted range / grep）。
//
// %s = 引擎侧清单段（[FileLedger] + [TaskStates] + [TodoSnapshot]，空段省略）。
const CompactHandoffReminderV2 = `<system-reminder>Handoff after compaction:
- Continue with the single next action from "Continuation State" in the summary above; do not restart, do not redo completed work, and do not bulk re-read.
- The original task goal, if present, was preserved verbatim by the engine in the TaskAnchorReminder message above — treat it as authoritative.
- The todo snapshot below, if present, is the authoritative session task list — advance it with todo_update; do not rebuild it with todo_add.
- Files read earlier are no longer in context. The [FileLedger] below is a map, not a reading list: re-read just-in-time, only the files the current next action needs (prefer targeted read_file ranges or grep).%s
</system-reminder>`

// PendingTasksReminder stop 检查点在途任务提醒（2026-08-24 增补，
// 标准：docs/PROMPT_CATALOG_CN.md §10.8；依据：方案文档 §7B B2）。
//
// 背景：后台任务契约常驻 Base（远处），弱模型在"宣布完成"的决策时刻看不见它——
// 表现为提前输出最终总结、无视 spawn 出去仍在运行的子任务。本提醒在 Run 结束
// 检查点（模型产出最终回复、即将 break）注入，把在途任务清单放到决策眼前。
//
// 接线（第二批 R1）：agent_loop stop 检查点，opts.TaskState() 非空且本 Run 未提醒过
// → 注入并多跑一轮；每 Run 至多一次（模型再次坚持结束则放行，防死循环）。
// %s = session.taskStateSnapshot() 渲染的在途任务清单。
const PendingTasksReminder = `<system-remind>Background tasks are still running (no final result yet):
%s
Their results will arrive as new messages. Do not report their outcomes, and do not declare the overall task complete if it depends on them — continue with other work or end the turn awaiting their results.
</system-remind>`

// PostToolContinueNudge 工具轮后的空回复轮续跑提醒（问题十三）：ephemeral 注入，
// 不落历史。空轮本身无法区分「卡住」与「已完成但静默」——文案双分支引导两种
// 情况都收敛到好结果：未完成 → 继续干活；已完成 → 补一条简短收尾汇报
// （对用户而言「工具跑完就没下文」无论哪种原因都是坏体验）。
// 中文释义："上一轮工具结果返回后你没有产出任何可见输出。任务未完成 → 基于工具
// 结果继续；任务已完成 → 给用户一段简短的完成汇报。不要静默停止。"
const PostToolContinueNudge = `<system-remind>
Your previous turn ended with no visible output right after tool results were returned.
- If the task is still in progress: continue now, acting on those tool results.
- If the task is complete: reply with a short completion summary for the user.
Do not stop silently.
</system-remind>`

// IncompleteTodosReminder 收尾前的完成度检查（stop 检查点注入，写进 ac.Messages 持久化，
// 同 PendingTasksReminder 范式——模型做完剩余工作的期间一直可见）。
// 触发：本轮即将以 stop 结束，而会话 todo 仍有未完成项（引擎已知的事实，不依赖模型自述）。
// 中文说明：这是运行时信号，不是新用户需求；若确有未完成项应继续做完，
// 若确实无法完成应明确说明剩余项与原因，不得把未完成的项标成已完成。
//
// %s = 引擎渲染的未完成项清单（agent_loop.renderUnfinishedTodos，每行 "- <id>: <title>"，
// 上限 10 项，超出以 "- (more unfinished items omitted)" 省略；无未完成项时不会注入）。
//
// 用法（A4/B4 收尾前验收门控）：agent_loop stop 检查点检测到未完成 todo → 注入本提醒并
// 多跑一轮；每 Run 至多一次（模型再次 stop 且 todo 仍未完成则按现状放行，防死循环）。
// 走机制而非提示词：Code 主会话的产品提示词已按用户要求移除工程流程节（含 Verification），
// 提示词层约束在实测中反复失效，故由引擎在收尾时刻拦截。
const IncompleteTodosReminder = `<system-remind>
You are about to finish, but the session todo list still has unfinished items:
%s
Check them before you stop: finish the remaining work now, or — if it is genuinely
blocked or no longer needed — say explicitly which items are unfinished and why.
Never mark an item done that is not actually done, and never claim the task is
complete while an item you added for it is still open.
</system-remind>`
