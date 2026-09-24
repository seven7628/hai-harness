package main

import (
	"github.com/seven7628/hai-harness/agents"
)

// ctx 占用面板（Composer ctx 环 hover）：SDK 分区估算 → 锚点结算 → 前端 payload。
//
// 口径（2026-09-20 重定，见 docs/CTX_ACCOUNTING_FIX_2026-09-20.md）：
//
//	静态分区（系统提示词/技能/工具/MCP/其他）= 请求组装字节估算（chars/4），**固定不缩放**。
//	  这些东西每一轮原样进请求，字节不变 → 展示值必须稳定（用户诉求：固定大小）。
//	消息上下文 = 锚点 − 静态分区合计（残差）。锚点来自真实 usage 时，残差就是
//	  「真实总量里除前缀之外的部分」；估算误差全部落在消息分区，静态分区不再被摊派。
//	总量 = 锚点（与 ctx 环逐字一致）；无锚点时退化为各分区估算合计并标 estimated。
//
// 为什么不再等比校准：旧实现把「真实总量 − 估算合计」按比例摊到**每个**分区上，
// 于是会话越长、估算越偏，工具/系统提示词这类固定块被放得越大（实测 6,025 token 的
// 工具 schema 在 93k 上下文里显示成 82,584 token）——面板看起来在描述工具，其实在
// 描述「锚点减不掉的差额」。
//
// degraded：锚点小于静态分区估计（刚压缩完 / 切了窗口更小的模型 / 锚点是上一会话残留）
// 时消息分区只能是 0，此时占比合计会超过总量，前端按「锚点过期」文案提示。
func ctxBreakdownPayload(bd agents.ContextBreakdown, anchor ctxAnchor) map[string]any {
	static := staticTokens(bd)
	parts := make([]map[string]any, 0, len(bd.Parts))
	var messages int64 // 无锚点时取估算原值；有锚点时被残差覆盖
	for _, p := range bd.Parts {
		if p.Key == agents.ContextPartMessages {
			messages = p.Tokens
		}
		parts = append(parts, map[string]any{"key": p.Key, "tokens": p.Tokens})
	}

	total := static + messages
	degraded := false
	if anchor.Tokens > 0 {
		total = anchor.Tokens
		messages = anchor.Tokens - static
		if messages < 0 { // 锚点比静态前缀还小：锚点过期（刚压缩 / 换模型），消息分区钳到 0
			messages = 0
			degraded = true
		}
		for i := range parts {
			if parts[i]["key"].(string) == agents.ContextPartMessages {
				parts[i]["tokens"] = messages
				break
			}
		}
	}

	source := "real"
	switch {
	case anchor.Tokens <= 0:
		source = "none"
	case anchor.Estimated:
		source = "estimate"
	}
	return map[string]any{
		"parts":         parts,
		"total":         total,
		"estimated":     source != "real", // true = 总量不是 provider 真实 usage
		"anchor_source": source,           // real | estimate | none
		"degraded":      degraded,         // true = 锚点 < 静态分区估计（消息分区被钳到 0）
		"message_count": bd.MessageCount,
	}
}

// staticTokens 静态分区合计（请求组装口径，每轮不变直到工具/技能/模式变化）。
// 注意「其他」计入静态侧：它的主体是子 agent 清单 + 每条消息的框架开销——框架开销
// 随消息数微增，但只有几 token/条，量级远小于消息正文，放静态侧可让消息分区保持
// 「就是消息内容」的语义。
func staticTokens(bd agents.ContextBreakdown) int64 {
	var n int64
	for _, p := range bd.Parts {
		if p.Key == agents.ContextPartMessages {
			continue
		}
		n += p.Tokens
	}
	return n
}

// ctxAnchor 上下文锚点：展示总量与分区结算的共同口径来源。
//
//	Tokens    锚点值（真实 usage 的 Input / 压缩后的字符估算 / 0 = 无锚点）
//	Estimated true = 锚点本身是估算（compress_end），不是 provider 报的真实 usage
type ctxAnchor struct {
	Tokens    int64
	Estimated bool
}
