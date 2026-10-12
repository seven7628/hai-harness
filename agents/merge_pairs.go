package agents

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/seven7628/hai-harness/core"
)

// 工具对合并（E）：把**整组**「assistant 工具调用 + 其 tool 结果」消息从上下文里
// 删掉，只留一组代表。与 C/D 段的区别是关键的一层：
//
//	C/D 段 = 替换**结果文本**（消息还在，角色 + tool_call JSON + tool_call_id +
//	         "[已折叠]" 占位行都还在）
//	E 段   = 删**消息本身**（结构开销一起省）
//
// 小结果上这笔开销远大于 payload 本身：一次 `bash: go build` 返回 "ok\n"（3 字节），
// 但它在上下文里占的是一条 assistant 的 tool_calls JSON（工具名 + 参数 + id，
// 约 60~120 字节）**加**一条 tool 消息（role + tool_call_id，约 30 字节）。
// 重复 20 次，光包装就吃掉几千 token —— 而 payload 合计不到一百字节。
//
// 三条不变式（E 段比 C/D 段更硬，因为动了结构）：
//
//  1. **配对不可破**：每个 assistant.ToolCalls 项都必须有对应的 tool 结果消息，
//     反之亦然。删一条 tool 消息就必须同时删掉发起它的那一个 tool call；
//     assistant 因而不剩任何 tool call 且无文本/推理时整条删除。合并后跑
//     assertNoOrphanToolMsgs 自检（测试与运行时双保险）。
//  2. **下标全失效**：删消息会移动其后所有元素的下标 —— C/D 段所有按下标记账的
//     状态（readIndex/reads/rerunIndex/roundStart）连同占位文本里的 `#N` 引用
//     一并失效。故合并后必须 slim.reset()，且**在同一次遍历里**把幸存占位
//     文本中的 `#旧下标` 改写为 `#新下标`（否则模型会去读一条错位的消息）。
//  3. **只删已被证冗余的**：只合并两类 —— 结果已是 slim 占位（其信息量已由
//     代表条目承载），或与后一条同参数结果逐字相同。互补的局部读（L10-40 与
//     L100-140）不满足「逐字相同」，天然不合并。
//
// 位置：结算段、压缩之前（与 applyStale 同处），让压缩输入体积也跟着降。

// slimPlaceholderPrefix 占位文本统一前缀：让「这条结果已被瘦身过」成为可机检
// 的事实（isSlimPlaceholder），而不是靠匹配各家文案。所有**会整条被删**的
// 占位都必须带此前缀。
const slimPlaceholderPrefix = "[slim:"

// refRe 占位文本里的消息下标引用（"#42"）。
var refRe = regexp.MustCompile(`#(\d+)`)

// defaultMergeProtectRounds 末尾保护轮数：最近这几组工具调用不合并 —— 模型
// 正在引用它们，且它们本来也不是重复成本的来源。
const defaultMergeProtectRounds = 2

// toolPair 一组「assistant 工具调用 + 其 tool 结果」在 ac.Messages 中的位置。
type toolPair struct {
	assistantIdx int
	callIdx      int
	toolIdx      int
	name         string
	args         string
	text         string // tool 结果文本（图片结果不参与合并，text 为空）
	image        bool
}

// collectToolPairs 扫描消息序列，配出全部工具调用组。配对靠 tool_call_id 反查
// assistant（不假设「结果紧跟其调用」）—— 中间可能插入注入消息。
func collectToolPairs(msgs []core.Message) []toolPair {
	type callRef struct{ assistantIdx, callIdx int }
	byID := make(map[string]callRef)
	for i, m := range msgs {
		if m.Role != core.Assistant {
			continue
		}
		for j, tc := range m.ToolCalls {
			byID[tc.Id] = callRef{i, j}
		}
	}
	var pairs []toolPair
	for i, m := range msgs {
		if m.Role != core.Tool {
			continue
		}
		ref, ok := byID[m.ToolCallId]
		if !ok {
			continue // 孤儿结果（理论上不该有）：不参与合并，交给自检报错
		}
		tc := msgs[ref.assistantIdx].ToolCalls[ref.callIdx]
		p := toolPair{assistantIdx: ref.assistantIdx, callIdx: ref.callIdx, toolIdx: i, name: tc.Name, args: tc.Arguments}
		for _, c := range m.Content {
			if c.Type == core.ContentTypeImage {
				p.image = true
			}
		}
		if len(m.Content) > 0 && m.Content[0].Type != core.ContentTypeImage {
			p.text = m.Content[0].Content
		}
		pairs = append(pairs, p)
	}
	return pairs
}

// isSlimPlaceholder 该结果是否已是 slim 占位（信息量已由代表条目承载）。
func isSlimPlaceholder(text string) bool {
	return strings.HasPrefix(text, slimPlaceholderPrefix)
}

// hasVisibleValue assistant 消息在移除 tool call 后是否还有值得保留的东西。
func hasVisibleValue(m core.Message) bool {
	if strings.TrimSpace(m.Reasoning) != "" || strings.TrimSpace(m.ReasoningSignature) != "" {
		return true
	}
	for _, c := range m.Content {
		if c.Type != core.ContentTypeText || strings.TrimSpace(c.Content) != "" {
			return true
		}
	}
	return false
}

// mergeToolPairs 合并冗余工具对。返回是否真的删了消息（调用方据此决定是否
// slim.reset()）。见文件头三条不变式。
func (a *AgentLoop) mergeToolPairs(ac *AgentContext, protectRounds int) bool {
	msgs := ac.Messages
	pairs := collectToolPairs(msgs)
	if len(pairs) < 2 {
		return false
	}
	if protectRounds <= 0 {
		protectRounds = defaultMergeProtectRounds
	}
	// 末尾保护：最近 protectRounds 组不参与
	protected := map[int]bool{}
	for i := len(pairs) - protectRounds; i < len(pairs); i++ {
		if i >= 0 {
			protected[pairs[i].toolIdx] = true
		}
	}

	// 按「工具名+参数」分组，保留**最早**一条，其余冗余者删除。
	//
	// 为何保留最早而非最近：纯重跑（内容逐字相同）⇒ 语义等价，而保留最早能让
	// 被删消息全部落在它**之后** —— 提示缓存前缀（到该条为止）逐字不变，失效
	// 起点最靠后。
	//
	// 三条纪律：
	//  1. 保护区（末尾 N 组）既不当代表也不被删 —— 直接跳过，**且不写 byKey**
	//     （早期版本让保护区条目顶替代表，把 byKey 指向后一轮分组，导致更早
	//      那组已收集的冗余条目全部失联、永远删不掉）。
	//  2. 占位条目的冗余性与顺序无关（信息量已由同 key 的真实读取承载），
	//     故先摘出、末轮统一挂到代表上 —— 单趟扫描会让「开头就是占位」的情况
	//     永远找不到代表。
	//  3. **同参数但内容不同**（同一文件被改过后的重读）也算冗余，保留最新：
	//     这是长任务里最高频的重复模式（读 A → 改 A → 再读 A），旧值已被
	//     后续编辑作废，留着只会让模型看到过时内容。判定前提是「同参数」——
	//     参数不同（读的是别的区间/别的文件）一律不合并。
	type group struct {
		keep   toolPair
		victim []toolPair
	}
	var groups []*group
	byKey := make(map[string]*group)
	var pending []toolPair // 待挂的占位条目
	for _, p := range pairs {
		if p.image {
			continue // 图片是模型仍在用的视觉内容，不参与结构合并
		}
		if protected[p.toolIdx] {
			continue
		}
		k := p.name + "\x00" + p.args
		if isSlimPlaceholder(p.text) {
			pending = append(pending, p)
			continue
		}
		g, ok := byKey[k]
		if !ok {
			g = &group{keep: p}
			byKey[k] = g
			groups = append(groups, g)
			continue
		}
		if p.text == g.keep.text {
			g.victim = append(g.victim, p) // 纯重跑：删
			continue
		}
		// 同参数但内容不同。分两种工具：
		//
		//  · read_file —— 同一文件被改过后的重读。旧值已被后续编辑作废（文件
		//    磁盘上就只剩新值），留着只会让模型对着过时内容推理。**保留最新、
		//    删旧的**：把旧条目降为本次的冗余。
		//  · 其余（bash: 编译失败→修复→重编）—— 是真实的信息演进，两条都留，
		//    另起一组。
		if p.name == "read_file" {
			g.victim = append(g.victim, p)
			g.keep = p // 代表换成最新：被删的都在它之前，缓存前缀仍从它起算
			continue
		}
		g = &group{keep: p}
		byKey[k] = g
		groups = append(groups, g)
	}
	for _, p := range pending {
		k := p.name + "\x00" + p.args
		if g, ok := byKey[k]; ok {
			g.victim = append(g.victim, p)
		}
		// 无代表（该 key 全是占位或全在保护区）：保持原样，不删
	}

	// 汇总要删的
	victimTool := map[int]bool{}
	victimCall := map[int]map[int]bool{} // assistantIdx → callIdx 集合
	absorbed := map[int]int{}            // 代表 toolIdx → 吸收掉的条数
	for _, g := range groups {
		if len(g.victim) == 0 {
			continue
		}
		if protected[g.keep.toolIdx] {
			continue // 代表自身在保护区：整组放弃（保守）
		}
		for _, v := range g.victim {
			victimTool[v.toolIdx] = true
			if victimCall[v.assistantIdx] == nil {
				victimCall[v.assistantIdx] = map[int]bool{}
			}
			victimCall[v.assistantIdx][v.callIdx] = true
		}
		absorbed[g.keep.toolIdx] += len(g.victim)
	}
	if len(victimTool) == 0 {
		return false
	}

	// 重建消息序列：摘掉 tool 消息、清 tool call、必要时整条删 assistant
	keepMsg := make([]bool, len(msgs))
	for i := range keepMsg {
		keepMsg[i] = true
	}
	for i := range victimTool {
		keepMsg[i] = false
	}
	dropAssistant := map[int]bool{}
	for ai, calls := range victimCall {
		m := msgs[ai]
		kept := make([]core.ToolCall, 0, len(m.ToolCalls))
		for j, tc := range m.ToolCalls {
			if calls[j] {
				continue
			}
			kept = append(kept, tc)
		}
		m.ToolCalls = kept
		if len(kept) == 0 && !hasVisibleValue(m) {
			dropAssistant[ai] = true
			keepMsg[ai] = false
		} else {
			msgs[ai] = m
		}
	}

	out := make([]core.Message, 0, len(msgs))
	oldToNew := make(map[int]int, len(msgs))
	for i, m := range msgs {
		if !keepMsg[i] {
			continue
		}
		oldToNew[i] = len(out)
		out = append(out, m)
	}
	// 幸存占位里的 #N 引用改写（不变式 2）
	for i, m := range out {
		if m.Role != core.Tool || !isSlimPlaceholder(m.Content[0].Content) {
			continue
		}
		body := refRe.ReplaceAllStringFunc(m.Content[0].Content, func(s string) string {
			n, err := strconv.Atoi(s[1:])
			if err != nil {
				return s
			}
			if nn, ok := oldToNew[n]; ok {
				return "#" + strconv.Itoa(nn)
			}
			return s // 指向已删消息：保持原样（删除的是本就被折叠的旧条目）
		})
		m.Content[0].Content = body
		out[i] = m
	}
	// 代表条目记一笔合并条数（语义完整：模型知道这里发生过 N 次重复调用）
	for toolIdx, n := range absorbed {
		nn, ok := oldToNew[toolIdx]
		if !ok {
			continue
		}
		m := out[nn]
		if m.Role != core.Tool || len(m.Content) == 0 {
			continue
		}
		m.Content[0].Content += fmt.Sprintf("\n[slim: 已合并 %d 次重复工具调用（同参数同结果，保留最早一次）]", n)
		out[nn] = m
	}
	ac.Messages = out
	return true
}

// assertNoOrphanToolMsgs 合并后自检：不得存在无主的 tool 消息，也不得存在
// 悬空的 tool call。返回违规描述（空串 = 通过）。
func assertNoOrphanToolMsgs(msgs []core.Message) string {
	called := map[string]bool{}
	for _, m := range msgs {
		for _, tc := range m.ToolCalls {
			called[tc.Id] = true
		}
	}
	answered := map[string]bool{}
	for _, m := range msgs {
		if m.Role != core.Tool {
			continue
		}
		if !called[m.ToolCallId] {
			return "orphan tool result: " + m.ToolCallId
		}
		answered[m.ToolCallId] = true
	}
	for id := range called {
		if !answered[id] {
			return "dangling tool call: " + id
		}
	}
	return ""
}
