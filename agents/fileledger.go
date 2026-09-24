package agents

import (
	"encoding/json"
	"fmt"
	"github.com/seven7628/hai-harness/agents/prompt"
	"github.com/seven7628/hai-harness/core"
	"strings"
)

// 引擎侧文件账本（S1-B）：per-Run 记录工具调用触达的文件路径与最后操作。
//
// 定位：File Context 的权威数据源——不依赖摘要 LLM 从工具调用参数里"提取"
// （bash 里写文件、路径缺失、摘要截断丢节都是失真源）。压缩时渲染 [FileLedger]
// 进压缩输入（摘要 LLM 照抄优先，见 SummaryPrompt 权威段条款）+ 交接提醒
// （续跑模型拿到引擎级准确清单 + "内容已不在上下文需重读"指示）。
//
// 生命周期：per-Run（AgentContext.fileLedger，RunStream 创建）。与 slim 不同，
// 压缩成功后**不** reset——账本是元数据（不携带消息下标），不随 Messages 替换失效，
// 跨 Run 的文件由摘要 LLM 对全量历史照旧提取（提示词硬化后质量提升）。

// fileLedgerEntry 一条文件账本记录。
type fileLedgerEntry struct {
	op     string   // read / write / scan
	ranges []string // read_file 的行号区间（"L90-130"）；F2 符号级索引数据源
}

// fileLedger 文件账本（并发不要求：仅 Run 循环内读写；值对象，AgentContext 持有）。
type fileLedger struct {
	entries map[string]fileLedgerEntry
	order   []string // 按最后出现排序（更新时移到末尾 → 渲染序 = 旧 → 新）
}

func newFileLedger() *fileLedger {
	return &fileLedger{entries: make(map[string]fileLedgerEntry)}
}

// record 记账同路径的最近一次操作（read→write 只留 write）；ranges 追加行号区间
// （F2：read_file 的 start_line/end_line，供摘要器写符号级 File Context）。
func (l *fileLedger) record(path, op string, ranges ...string) {
	if l == nil || path == "" {
		return
	}
	if _, ok := l.entries[path]; ok {
		// 移出旧位置（末尾追加 = 最后出现排序）
		for i, p := range l.order {
			if p == path {
				l.order = append(l.order[:i], l.order[i+1:]...)
				break
			}
		}
	}
	entry := l.entries[path]
	entry.op = op
	if len(ranges) > 0 {
		// 去重合并新区间
		seen := map[string]bool{}
		for _, r := range append(entry.ranges, ranges...) {
			if r != "" && !seen[r] {
				seen[r] = true
				entry.ranges = append(entry.ranges, r)
			}
		}
		if len(entry.ranges) > 4 {
			entry.ranges = entry.ranges[len(entry.ranges)-4:] // 只保留最近 4 段
		}
	}
	l.entries[path] = entry
	l.order = append(l.order, path)
}

// render 渲染 [FileLedger] 段（空账本 = 空串；压缩输入注入与交接提醒共用）。
// C2.2：超过 fileLedgerMaxEntries 条时丢弃最老条目并加 omitted 头行——账本是
// "查询地图"不是全量清单，长会话的几百条路径只会稀释摘要预算。
// F2：read 条目带行号区间（"path (L90-130)"），scan/write 不带——摘要器据此
// 写符号级 File Context（模型直接 read_file 指定区段，不从 L1 全量扫）。
func (l *fileLedger) render() string {
	if l == nil || len(l.entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[FileLedger] (files touched this run — engine-tracked from tool call arguments; last op per path; read ranges from read_file args)\n")
	order := l.order
	if len(order) > fileLedgerMaxEntries {
		fmt.Fprintf(&b, "- ... and %d earlier entries omitted\n", len(order)-fileLedgerMaxEntries)
		order = order[len(order)-fileLedgerMaxEntries:]
	}
	for _, p := range order {
		e := l.entries[p]
		fmt.Fprintf(&b, "- %s: %s", e.op, p)
		if len(e.ranges) > 0 {
			fmt.Fprintf(&b, " (%s)", strings.Join(e.ranges, ", "))
		}
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// fileLedgerMaxEntries [FileLedger] 渲染条数上限（C2.2）：超出丢弃最老条目。
const fileLedgerMaxEntries = 40

// recordFileOps 文件账本记账（S1-B）：与 recordRead 同位置（工具结果回写循环内、
// append 前调用）。read_file → read（内容进过上下文）；grep/glob → scan（C2.1，
// 只扫过没读过——续跑模型"即用即读"时据此区分）；write_file/edit_file → write；
// bash 等其余工具不动（避免误报）。与 slim 无关（ledger 是元数据，不是下标索引）。
// F2：read_file 的 start_line/end_line 提取为行号区间（符号级索引数据源）。
func recordFileOps(ac *AgentContext, tc core.ToolCall) {
	if ac.fileLedger == nil {
		return
	}
	switch tc.Name {
	case "read_file":
		ac.fileLedger.record(readPathFromArgs(tc.Arguments), "read", readRangeFromArgs(tc.Arguments)...)
	case "grep", "glob":
		ac.fileLedger.record(readPathFromArgs(tc.Arguments), "scan")
	case "write_file", "edit_file":
		ac.fileLedger.record(readPathFromArgs(tc.Arguments), "write")
	}
}

// readRangeFromArgs 提取 read_file 参数中的行号区间（F2）。
// 有 start_line/end_line → "Lstart-end"（end 缺省 = "Lstart"）；两者都无 → 空。
func readRangeFromArgs(arguments string) []string {
	var args struct {
		StartLine int `json:"start_line"`
		EndLine   int `json:"end_line"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return nil
	}
	if args.StartLine <= 0 {
		return nil
	}
	if args.EndLine > args.StartLine {
		return []string{fmt.Sprintf("L%d-%d", args.StartLine, args.EndLine)}
	}
	return []string{fmt.Sprintf("L%d", args.StartLine)}
}

// renderCompactHandoff 渲染压缩后统一交接提醒（S1-B/S1-E 提醒层补丁）：
// CompactHandoffReminderV2 模板 + 引擎侧清单段（[FileLedger] + [TaskStates] +
// [TodoSnapshot] + [RecentFailures]，任一为空则省略）。V2 文案为 2026-08-25 现行标准
// （单步继续 / 地图非阅读清单 / todo 权威），V1 常量仅作回滚对照保留。
func (a *AgentLoop) renderCompactHandoff(ac *AgentContext, opts *RunOptions) string {
	var seg strings.Builder
	if ac.fileLedger != nil {
		if ledger := ac.fileLedger.render(); ledger != "" {
			seg.WriteString("\n")
			seg.WriteString(ledger)
		}
	}
	if opts != nil && opts.TaskState != nil {
		if st := opts.TaskState(); st != "" {
			seg.WriteString("\n")
			seg.WriteString(st)
		}
	}
	// 压缩元数据三块双侧注入·输出侧（L3/§10.6）：[TodoSnapshot] 在 [TaskStates] 之后
	// ——压缩后模型上下文由此获得权威 todo 视图（用 todo_update 推进，不重建）。
	// TodoResetOnCompact=true 兼容路径由 CompactTodoReminder 携带快照，此处跳过防双份。
	if !a.cfg.TodoResetOnCompact && ac.Todo != nil {
		if items := ac.Todo.Items(); len(items) > 0 {
			seg.WriteString("\n")
			seg.WriteString("[TodoSnapshot] (authoritative session task list)\n" + ac.Todo.List())
		}
	}
	// 最近失败段（C6/§2.2 剩余项，2026-09-18）：前三段注入里**没有失败信息** ——
	// 压缩后模型不知道「刚才哪条命令失败了、code 是什么」，于是可能重跑同一条失败命令
	// 或误判已完成。空账本 = 空串 ⇒ 整段省略（不注入空块）。与 [TodoSnapshot] 无重叠：
	// 本段只渲染工具失败，todo 状态仍由 todo 两段负责（不重复注入）。
	if seg2 := ac.failures.render(); seg2 != "" {
		seg.WriteString("\n")
		seg.WriteString(seg2)
	}
	return fmt.Sprintf(prompt.CompactHandoffReminderV2, seg.String())
}

// ---------------------------------------------------------------------------
// 失败账本（C5 去重守卫 + C6 压缩交接 [RecentFailures] 的**共享前置件**，2026-09-18）
// ---------------------------------------------------------------------------
//
// 为什么需要它：C5 的旧记录条件（r.IsError && r.ToolError != nil）按构造看不见
// bash 失败与散文错误（兜底分类器**不**写 ToolResult.ToolError —— 既有决策，未动），
// 而 C6 需要的「最近失败」在交接路径上根本没有数据源。两者缺的是同一件东西：
// 一份「本 Run 的工具失败」账本。
//
// 记账范围：**所有** IsError 结果（不要求 ToolError 信封）——工具名 + 首行摘要 +
// code（可空）+ 工具轮序号；信封可用时额外留 next_action/fix 供 C5 拦截文案用。
//
// 生命周期：per-Run（AgentContext.failures，RunStream 创建）。压缩后**不** reset
// （与 fileLedger 同理：携带的事实不随 Messages 替换失效，正是要在压缩后渲染出来）；
// 仅由 C5 的清理规则清空——见 clear 与 recordToolOutcomes。
//
// 并发：仅 Run 循环内读写（runTools 单 goroutine 顺序调用），无锁需求。

// failureLedgerMaxEntries [RecentFailures] 渲染条数上限（C6：最近 N 条，N ≤ 5）。
// 失败清单是「别重犯错」的提示，不是日志——超过 5 条只会稀释提示词预算。
const failureLedgerMaxEntries = 5

// failureDetailMaxRunes 单条失败摘要的字符上限（信封逐字照抄无用，首行足够定位问题）。
const failureDetailMaxRunes = 200

// toolFailure 一条工具失败记录（C5 去重判定 + C6 交接渲染共用同一份数据）。
type toolFailure struct {
	tool   string // 工具名（渲染与拦截文案用）
	detail string // 失败结果首行摘要（信封优先 detail: 行）
	code   string // 结构化 code：信封 code 优先，无信封时取 Exec 退出码/超时（可空）
	round  int    // 失败所在**工具轮**序号（1 起，见 AgentContext.toolRound）

	// envelope 工具显式声明的错误信封（可空）：C5 拦截文案取 next_action/fix；
	// RetryWithSameArguments=true 时该调用**不参与拦截**（既有语义：允许原样重试）。
	envelope *core.ToolError
}

// retryUnchanged 该失败是否属于「原样重试是允许的」信封（RetryWithSameArguments）。
func (f toolFailure) retryUnchanged() bool {
	return f.envelope != nil && f.envelope.RetryWithSameArguments
}

// failureLedger per-Run 工具失败账本。
// entries 按**调用键**（sha256(工具名 \0 参数)，见 toolCallKey）索引：同一失败调用
// 重复出现只留最新一条（轮次随之刷新），渲染清单因此不会同一行刷屏。
type failureLedger struct {
	entries map[string]toolFailure
	order   []string // 按最后出现排序（渲染序 = 旧 → 新）
}

func newFailureLedger() *failureLedger {
	return &failureLedger{entries: make(map[string]toolFailure)}
}

// record 记账一次失败调用：写/刷新渲染清单 + 去重索引（同一 entries）。
func (l *failureLedger) record(key string, f toolFailure) {
	if l == nil || key == "" {
		return
	}
	if _, ok := l.entries[key]; ok {
		// 移出旧位置（末尾追加 = 最后出现排序）
		for i, k := range l.order {
			if k == key {
				l.order = append(l.order[:i], l.order[i+1:]...)
				break
			}
		}
	}
	l.entries[key] = f
	l.order = append(l.order, key)
}

// clear 清空账本（C5 清理规则：任何状态变更类调用**成功** ⇒ 局面变了 ⇒ 重跑合理）。
// 这是「误杀」的解药：go build / go test 失败 → 改代码 → 原样重跑必须放行，
// 而实现方式不是去猜命令语义，而是「工作区/计划被改过」这一引擎级事实。
func (l *failureLedger) clear() {
	if l == nil {
		return
	}
	l.entries = make(map[string]toolFailure)
	l.order = nil
}

// blockedIn 判定「该调用在**紧邻的上一轮工具轮**里也失败过」（C5 拦截条件，刻意收窄）。
//
// round = 当前工具轮序号；命中要求记录轮次恰为 round-1 —— 本 Run 更早的失败**不**
// 构成拦截理由。这不是装饰：`go build` / `go test` 的成败依赖外部状态，「本 Run 内
// 失败过就永久拦截」会把模型改完代码后的**合法重跑**全部误杀（batch1 书面决策）。
// 命中「宁可不拦」的另两类：Envelope.RetryWithSameArguments=true（既有语义）、
// 失败发生在同一轮更早位置（同轮内不拦——那由模型的自我重复兜着，不由守卫）。
func (l *failureLedger) blockedIn(key string, round int) (toolFailure, bool) {
	if l == nil {
		return toolFailure{}, false
	}
	f, ok := l.entries[key]
	if !ok || f.round != round-1 || f.retryUnchanged() {
		return toolFailure{}, false
	}
	return f, true
}

// render 渲染 [RecentFailures] 段（无失败 = 空串；调用方据此**省略整段**不注入空块）。
// 段头写成可执行的话：压缩后「重跑刚失败过的同一条命令」是审计里最贵的浪费形态之一。
func (l *failureLedger) render() string {
	if l == nil || len(l.entries) == 0 {
		return ""
	}
	order := l.order
	if len(order) > failureLedgerMaxEntries {
		order = order[len(order)-failureLedgerMaxEntries:]
	}
	var b strings.Builder
	b.WriteString("[RecentFailures] (recent failing tool calls this run — engine-tracked; do not repeat one unchanged. Change state or arguments first)\n")
	for _, k := range order {
		f := l.entries[k]
		fmt.Fprintf(&b, "- %s", f.tool)
		if f.code != "" {
			fmt.Fprintf(&b, " [%s]", f.code)
		}
		if f.detail != "" {
			fmt.Fprintf(&b, ": %s", f.detail)
		}
		b.WriteByte('\n')
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// recordToolOutcomes 工具批结果落账（C5/C6 唯一写入口，runTools 批末调用）：
// 先记失败，再按**状态变更**清理。
//
// 顺序刻意如此：同一轮里「失败 + 成功写文件」时清理赢 —— 局面已变，该失败不再具备
// 任何拦截理由；也正因如此，交接段不会渲染已被后续动作取代的失败。
// 只记 engine 执行过的结果（calls ⊇ results 的反查）：模式/hook 拒绝与守卫自身的
// 拦截结果**不**记账 —— 否则被拦的调用会刷新自己的轮次，变成「永久拦截」。
func recordToolOutcomes(ac *AgentContext, calls []core.ToolCall, results []core.ToolResult) {
	if ac == nil || ac.failures == nil {
		return
	}
	byID := make(map[string]core.ToolResult, len(results))
	for _, r := range results {
		byID[r.Id] = r
	}
	for _, tc := range calls {
		r, ok := byID[tc.Id]
		if !ok || !r.IsError {
			continue
		}
		ac.failures.record(toolCallKey(tc.Name, tc.Arguments), toolFailure{
			tool:     tc.Name,
			detail:   failureDetail(r.Result),
			code:     failureCode(r),
			round:    ac.toolRound,
			envelope: r.ToolError,
		})
	}
	for _, tc := range calls {
		if r, ok := byID[tc.Id]; ok && !r.IsError && isStateChangeTool(tc.Name) {
			ac.failures.clear()
			return
		}
	}
}

// isStateChangeTool 状态变更类工具（C5 清理判据）：成功即「局面变了」，本 Run 的
// 失败记录随之清空。与 progressToolResult 的白名单**同源但不等同**——后者含
// agent_spawn / TaskOutput / 验证命令（那是「进展」判据，不改变可重跑性），本判据只认
// 真正改变工作区或计划的工具。todo_* 按前缀匹配（本仓只有 todo_add / todo_update）。
func isStateChangeTool(name string) bool {
	switch name {
	case "write_file", "edit_file":
		return true
	}
	return strings.HasPrefix(name, "todo_")
}

// failureCode 结构化 code（C6 渲染用）：工具信封显式声明的 code 优先；无信封时取命令
// 执行结局 Exec —— 退出码/超时/取消是**结构化事实**，不是文本分类（兜底分类器仍在
// tools 侧且只进事件遥测，本账本不复制它的推断结果，避免把「unclassified」写成事实）。
func failureCode(r core.ToolResult) string {
	if r.ToolError != nil && r.ToolError.Code != "" {
		return r.ToolError.Code
	}
	if r.Exec != nil {
		switch {
		case r.Exec.Canceled:
			return "canceled" // 与 tools 侧事件 code 同词（errorCodeCanceled）
		case r.Exec.TimedOut:
			return "timeout"
		case r.Exec.ExitCode != 0:
			return fmt.Sprintf("exit %d", r.Exec.ExitCode)
		}
	}
	return ""
}

// failureDetail 提取失败结果的首行摘要（C6 渲染 / C5 文案数据源）。
//
// TOOL_ERROR 信封首行恒为 "TOOL_ERROR"（无信息量）⇒ 优先取 detail: 行，其次取首个
// 「非信封键」行；bash 等无信封结果就是输出首行（"FAIL go-code/agents [build failed]"）。
// 单行化 + 截断：交接段进提示词，多行会把段结构冲散、长行会稀释预算。
func failureDetail(result string) string {
	// 参数校验失败路径把信封前置 "invalid params: "（见 tools.parseToolError 同款剥法），
	// 不剥的话首行会退化成 "invalid params: TOOL_ERROR"。
	result = strings.TrimPrefix(result, "invalid params: ")
	var first, detailLine string
	for _, line := range strings.Split(result, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "TOOL_ERROR" {
			continue
		}
		if v, ok := strings.CutPrefix(line, "detail: "); ok {
			if detailLine == "" {
				detailLine = v
			}
			continue
		}
		if first == "" && !envelopeFieldLine(line) {
			first = line
		}
	}
	if detailLine != "" {
		return clipSingleLine(detailLine)
	}
	return clipSingleLine(first)
}

// envelopeFieldLine 判定一行是否为 TOOL_ERROR 信封的 `key: value` 行（兜底摘要要跳过
// 这些行，否则摘要会退化成 "code: unknown_tool" 这类零信息量内容）。
func envelopeFieldLine(line string) bool {
	key, _, ok := strings.Cut(line, ": ")
	if !ok {
		return false
	}
	switch key {
	case "code", "changed", "retryable", "retry_with_same_arguments", "next_action", "fix", "recovery":
		return true
	}
	return false
}

// clipSingleLine 单行化（换行/连续空白折叠成单空格）并按 failureDetailMaxRunes 截断。
func clipSingleLine(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) > failureDetailMaxRunes {
		return strings.TrimSpace(string(r[:failureDetailMaxRunes])) + "..."
	}
	return s
}

// ---------------------------------------------------------------------------
// C5 拦截结果构造
// ---------------------------------------------------------------------------

// repeatedCallNextAction / repeatedCallFix：无信封失败（bash 等）命中拦截时的**通用
// 可操作指引**。刻意不写成「重复调用已阻止」——那只是现象描述，模型仍然不知道下一步
// 该干什么，于是会原样再发一次（这是「拦截必须自带出路」的既有教训）。
// 单行是硬约束：TOOL_ERROR 的 `key: value` 解析按行切分，多行会让整封信封解析失败。
const (
	repeatedCallNextAction = "change the state before retrying (edit a file, update the todo list, or change the command/arguments); if nothing can be changed, use a different call"
	repeatedCallFix        = "this exact call failed in the previous tool round and nothing changed since — repeating it unchanged will fail the same way; its error output is still in context"
)

// blockedResult 构造拦截结果（IsError + 信封 + 可操作文案）。
//
// 信封是本包**自造**的（Code=repeated_failed_call，字段取现网统一缺省值
// Changed="false"/Retryable=true/RetryWithSameArguments=false），不是原始信封的转发
// ——文案与信封必须说同一件事（旧实现转发原信封，文本说 repeated_failed_call、信封
// 却带着原 code，两者不一致）。原始 code 不丢：它就在上一轮的工具结果里，也进
// [RecentFailures] 段。next_action/fix 优先用原始信封声明的（工具最懂自己的恢复方式）。
func (f toolFailure) blockedResult(id string) core.ToolResult {
	next, fix := repeatedCallNextAction, repeatedCallFix
	if f.envelope != nil {
		if s := clipSingleLine(f.envelope.NextAction); s != "" {
			next = s
		}
		if s := clipSingleLine(f.envelope.Recovery); s != "" {
			fix = s
		}
	}
	return core.ToolResult{
		Id:      id,
		IsError: true,
		Result: fmt.Sprintf("TOOL_ERROR\ncode: repeated_failed_call\nnext_action: %s\ndetail: this exact call (%s) already failed in the immediately preceding tool round, and no file/todo change happened since\nfix: %s",
			next, f.tool, fix),
		ToolError: &core.ToolError{
			Code:       "repeated_failed_call",
			Changed:    "false",
			Retryable:  true,
			NextAction: next,
			Recovery:   fix,
		},
	}
}
