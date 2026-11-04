package core

// 工具批次的协议不变量（OpenAI 系 / Anthropic 系共同要求）：
// 一条带 tool_calls 的 assistant 消息，后面必须**紧接着**同批每个 id 的 tool 回执
// （连续、覆盖全部 id、不夹别的角色）。违反它上游直接 400，且文案与真实原因无关：
//
//	An assistant message with 'tool_calls' must be followed by tool messages
//	responding to each 'tool_call_id'. (insufficient tool messages following tool_calls message)
//
// 2026-11-04 真实事故：压缩注入把一条引擎元数据 assistant 消息插进了同批 3 条工具
// 回执中间（assistant → tool → tool → assistant([TodoSnapshot]) → tool），压完整轮
// 直接不可运行。之后同类破法还会从别的入口冒出来（注入/近窗裁剪/摘离/检查点恢复），
// 所以不变量在这里做**最后一次确定性修复**，而不是只堵一个入口。

// missingToolResultText 缺失回执的占位正文：模型必须知道「这次调用没有结果」，
// 否则会以为自己看过了（最坏的失败模式是按错误记忆继续推理）。
const missingToolResultText = "[no result: this tool call's result is not in the context (dropped during compaction or still in flight). Re-run it if you still need it.]"

// ToolBatchesIntact 判断消息列表是否已满足工具批次不变量（合法即无需修复）。
func ToolBatchesIntact(msgs []Message) bool {
	open := map[string]bool{}
	for _, m := range msgs {
		switch m.Role {
		case Assistant:
			if len(open) > 0 {
				return false // 上一批还没被回执吃完，就来了新的 assistant
			}
			for _, tc := range m.ToolCalls {
				if tc.Id == "" {
					return false
				}
				open[tc.Id] = true
			}
		case Tool:
			if len(open) == 0 {
				return false // 孤儿回执：没有 assistant 认领
			}
			if !open[m.ToolCallId] {
				return false // 不属于当前批（错位）或重复回执
			}
			delete(open, m.ToolCallId)
		default:
			if len(open) > 0 {
				return false // 批次被别的角色打断
			}
		}
	}
	return len(open) == 0
}

// NormalizeToolBatches 把消息列表修复成满足工具批次不变量（幂等；已合法则原样返回，
// 不分配、不改顺序 —— 缓存前缀稳定）。
//
// 修复规则（确定性，不丢用户可见内容）：
//  1. 每个 tool 回执按 id 归位到其 assistant 之后（保持同批原有相对顺序）——
//     被插入消息顶开后错位的回执会被搬回来；
//  2. 缺回执的 tool_call 补一条占位 tool 消息（宁可占位，也不留悬空调用）；
//  3. 无 assistant 认领的孤儿回执、以及重复 id 的第二条回执被丢弃 —— 上游必拒，
//     留着只会让整轮失败（信息量已由摘要/失败账本覆盖）。
func NormalizeToolBatches(msgs []Message) []Message {
	if ToolBatchesIntact(msgs) {
		return msgs
	}
	byCall := make(map[string]Message, 8) // 每个 id 的首条回执
	for _, m := range msgs {
		if m.Role != Tool || m.ToolCallId == "" {
			continue
		}
		if _, ok := byCall[m.ToolCallId]; !ok {
			byCall[m.ToolCallId] = m
		}
	}
	out := make([]Message, 0, len(msgs)+2)
	for _, m := range msgs {
		if m.Role == Tool {
			continue // 回执统一由所属 assistant 搬运（见下）
		}
		out = append(out, m)
		if m.Role != Assistant || len(m.ToolCalls) == 0 {
			continue
		}
		for _, tc := range m.ToolCalls {
			if r, ok := byCall[tc.Id]; ok {
				delete(byCall, tc.Id)
				out = append(out, r)
				continue
			}
			out = append(out, NewToolMessage(tc.Id, missingToolResultText))
		}
	}
	return out
}
