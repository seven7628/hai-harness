package agenthub

import "strings"

// turn_context.go：群内回复的触发上下文（P3b-07 B6，方案 §8.3.5 TurnContextLine）。
//
// 一条回复可能是在「上次读到 → 本次唤醒」之间看到的多条消息促成的。前端只在
// 有 ≥2 个不同智能体发送者时才显示「来自 A、B 的消息」，所以这里只收智能体：
// 人类没有 AgentID，硬塞一个空 AgentRef 会让 chip 点击/名册解析出现幽灵项。
//
// 纯函数：不读时钟、不读存储；调用的 bridge 负责给游标与触发 seq。

// BuildTurnContext 从 (fromSeq, toSeq] 的群消息里提炼触发上下文。
// 跳过 UI-only/owner-only/空正文（与 BuildGroupSnapshot 同口径），跳过回复者自己。
// 没有任何智能体发送者时返回 nil（人类单独触发不画上下文行）。
func BuildTurnContext(agentID AgentID, msgs []Message, fromSeq, toSeq int64) *TurnContext {
	ctx := &TurnContext{}
	seen := make(map[AgentID]bool, 4)
	for _, m := range msgs {
		if m.Seq <= fromSeq {
			continue
		}
		if toSeq > 0 && m.Seq > toSeq {
			continue
		}
		if !m.AgentContextKind() || !m.VisibleTo(false) || strings.TrimSpace(m.Text) == "" {
			continue
		}
		if m.Sender.Kind != SenderAgent || m.Sender.AgentID == "" || m.Sender.AgentID == agentID {
			continue
		}
		ctx.TriggerSeqs = append(ctx.TriggerSeqs, m.Seq)
		if !seen[m.Sender.AgentID] {
			seen[m.Sender.AgentID] = true
			ctx.Senders = append(ctx.Senders, AgentRef{AgentID: m.Sender.AgentID})
		}
	}
	if len(ctx.Senders) == 0 {
		return nil
	}
	if len(ctx.Senders) > 1 {
		ctx.Reason = "multi_agent"
	}
	return ctx
}
