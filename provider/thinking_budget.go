package provider

import "github.com/seven7628/hai-harness/core"

// —— 思考预算管理（对齐 pi simple-options.ts：DEFAULT_THINKING_BUDGETS /
// adjustMaxTokensForThinking / clampMaxTokensToContext / clampThinkingBudgetToAnswerRoom）——
//
// 背景：Anthropic 旧模型（budget-based thinking）的 thinking 与答案共享 max_tokens。
// 若不设预算，思考阶段可能吃满整个响应额度、最终没有答案也没有工具调用（pi 注释原话）。
// pi 对每档 effort 有独立预算，并为 thinking 扩容 max_tokens，再 clamp 到上下文剩余空间。

// DefaultThinkingBudgets 每档思考预算（tokens），对齐 pi DEFAULT_THINKING_BUDGETS：
// minimal 1024 / low 2048 / medium 8192 / high 16384。
// xhigh/max 无独立预算（clamp 到 high，pi clampReasoning 同语义）。
var DefaultThinkingBudgets = map[ReasoningEffortLevel]int64{
	ReasoningEffortLevelMinimal: 1024,
	ReasoningEffortLevelLow:     2048,
	ReasoningEffortLevelMedium:  8192,
	ReasoningEffortLevelHigh:    16384,
}

// MinAnswerTokens 答案保底（thinking 与答案共享响应额度时，答案至少保留的 token 数）。
const MinAnswerTokens = int64(1024)

// ContextSafetyTokens 上下文安全余量（clampMaxTokensToContext 从窗口扣除，对齐 pi 4096）。
const ContextSafetyTokens = int64(4096)

// clampReasoning 把 xhigh/max 收敛到 high（pi clampReasoning：这两档没有独立预算）。
func ClampReasoning(e ReasoningEffortLevel) ReasoningEffortLevel {
	if e == ReasoningEffortLevelXHigh || e == ReasoningEffortLevelMax {
		return ReasoningEffortLevelHigh
	}
	return e
}

// thinkingBudgetForLevel 某档位的思考预算（自定义预算覆盖默认；未知档位回退 high 档）。
func ThinkingBudgetForLevel(level ReasoningEffortLevel) int64 {
	b, ok := DefaultThinkingBudgets[ClampReasoning(level)]
	if !ok {
		b = DefaultThinkingBudgets[ReasoningEffortLevelHigh]
	}
	return b
}

// clampThinkingBudgetToAnswerRoom 钳制思考预算，保证答案至少 MinAnswerTokens 空间
// （对齐 pi：min(budget, max(0, ceiling - MIN_ANSWER_TOKENS))）。
func ClampThinkingBudgetToAnswerRoom(budget, ceiling int64) int64 {
	if room := ceiling - MinAnswerTokens; room > 0 && budget > room {
		return room
	}
	if ceiling <= MinAnswerTokens {
		return 0
	}
	return budget
}

// adjustMaxTokensForThinking 为 thinking 调整 max_tokens 与预算（对齐 pi）：
//   - 未显式指定上限 → 用模型上限，预算塞进模型上限内；
//   - 显式指定 → maxTokens = min(base + budget, modelCap)（为思考扩容）；
//   - 若上限 ≤ 预算 → 预算钳到「上限 - 答案保底」。
func AdjustMaxTokensForThinking(baseMaxTokens, modelMaxTokens int64, level ReasoningEffortLevel) (maxTokens, thinkingBudget int64) {
	budget := ThinkingBudgetForLevel(level)
	maxTokens = modelMaxTokens
	if baseMaxTokens > 0 {
		if baseMaxTokens+budget < modelMaxTokens {
			maxTokens = baseMaxTokens + budget
		} else {
			maxTokens = modelMaxTokens
		}
	}
	if maxTokens <= budget {
		budget = ClampThinkingBudgetToAnswerRoom(budget, maxTokens)
	}
	return maxTokens, budget
}

// estimateContextTokens 消息集上下文估算（字符/4 近似；图片按固定 visual tokens）。
// 对齐 pi estimateContextTokens（CHARS_PER_TOKEN=4；图片 ESTIMATED_IMAGE_CHARS）。
// 用于 clampMaxTokensToContext 的「已用上下文」估算。
func EstimateContextTokens(msgs []core.Message) int64 {
	var chars int64
	for _, m := range msgs {
		for _, c := range m.Content {
			if c.Type == core.ContentTypeImage {
				chars += 4800 * 4 // pi ESTIMATED_IMAGE_CHARS=4800（字符口径）
				continue
			}
			chars += int64(len(c.Content))
		}
		chars += int64(len(m.Reasoning))
		for _, tc := range m.ToolCalls {
			chars += int64(len(tc.Name) + len(tc.Arguments))
		}
	}
	return chars / 4
}

// clampMaxTokensToContext 把 max_tokens 钳到上下文剩余空间（对齐 pi）：
// min(maxTokens, max(1, window - 已用 - safety))。
func ClampMaxTokensToContext(modelMaxTokens, contextWindow, usedTokens, maxTokens int64) int64 {
	if contextWindow <= 0 {
		if maxTokens < 1 {
			return 1
		}
		return maxTokens
	}
	available := contextWindow - usedTokens - ContextSafetyTokens
	if available < 1 {
		available = 1
	}
	if maxTokens > available {
		return available
	}
	if maxTokens < 1 {
		return 1
	}
	return maxTokens
}
