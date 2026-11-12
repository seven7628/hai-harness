package main

import (
	"testing"

	"github.com/seven7628/hai-harness/agents"
)

func payloadParts(t *testing.T, data map[string]any) []map[string]any {
	t.Helper()
	raw, ok := data["parts"].([]map[string]any)
	if !ok {
		t.Fatalf("parts 类型 = %T", data["parts"])
	}
	return raw
}

func sumTokens(parts []map[string]any) int64 {
	var n int64
	for _, p := range parts {
		n += p["tokens"].(int64)
	}
	return n
}

// TestCtxBreakdownPayloadStaticFixed 有真实 usage 锚点时：静态分区（系统提示词/技能/
// 工具/MCP/其他）按组装字节**原样**展示（不放大、不摊派），消息分区取残差 =
// 锚点 − 静态合计，合计恒等于锚点（条形图不留空白段）。
func TestCtxBreakdownPayloadStaticFixed(t *testing.T) {
	bd := agents.ContextBreakdown{
		Parts: []agents.ContextPart{
			{Key: agents.ContextPartSystem, Chars: 400, Tokens: 100},
			{Key: agents.ContextPartTools, Chars: 120, Tokens: 30},
			{Key: agents.ContextPartMessages, Chars: 800, Tokens: 200},
			{Key: agents.ContextPartOther, Chars: 40, Tokens: 10},
		},
		TotalChars:  1360,
		TotalTokens: 340,
	}
	data := ctxBreakdownPayload(bd, ctxAnchor{Tokens: 3100})
	parts := payloadParts(t, data)
	if got := sumTokens(parts); got != 3100 {
		t.Errorf("分区合计 = %d, want 3100（残差补给消息分区）", got)
	}
	if got := data["total"].(int64); got != 3100 {
		t.Errorf("total = %d, want 3100", got)
	}
	if data["estimated"].(bool) {
		t.Error("有真实 usage 时 estimated 应为 false")
	}
	if got := data["anchor_source"].(string); got != "real" {
		t.Errorf("anchor_source = %q, want real", got)
	}
	if data["degraded"].(bool) {
		t.Error("锚点 > 静态合计时不应 degraded")
	}
	byKey := map[string]int64{}
	for _, p := range parts {
		byKey[p["key"].(string)] = p["tokens"].(int64)
	}
	// 静态分区固定：原样 100 / 30 / 10（旧实现会把它们放大 10 倍）
	if byKey[agents.ContextPartSystem] != 100 {
		t.Errorf("系统提示词 = %d, want 100（静态不缩放）", byKey[agents.ContextPartSystem])
	}
	if byKey[agents.ContextPartTools] != 30 {
		t.Errorf("工具 = %d, want 30（静态不缩放）", byKey[agents.ContextPartTools])
	}
	if byKey[agents.ContextPartOther] != 10 {
		t.Errorf("其他 = %d, want 10（静态不缩放）", byKey[agents.ContextPartOther])
	}
	// 消息 = 3100 − (100+30+10) = 2960
	if byKey[agents.ContextPartMessages] != 2960 {
		t.Errorf("消息分区 = %d, want 2960（残差）", byKey[agents.ContextPartMessages])
	}
	if parts[0]["key"].(string) != agents.ContextPartSystem {
		t.Errorf("分区键被改写: %v", parts[0]["key"])
	}
}

// TestCtxBreakdownPayloadDegraded 锚点小于静态前缀估计（刚压缩 / 换窗口更小的模型）：
// 消息分区钳到 0，degraded=true（前端提示锚点过期），静态分区仍原样展示。
func TestCtxBreakdownPayloadDegraded(t *testing.T) {
	bd := agents.ContextBreakdown{
		Parts: []agents.ContextPart{
			{Key: agents.ContextPartSystem, Chars: 400, Tokens: 100},
			{Key: agents.ContextPartMessages, Chars: 800, Tokens: 200},
		},
		TotalTokens: 300,
	}
	data := ctxBreakdownPayload(bd, ctxAnchor{Tokens: 80})
	parts := payloadParts(t, data)
	byKey := map[string]int64{}
	for _, p := range parts {
		byKey[p["key"].(string)] = p["tokens"].(int64)
	}
	if byKey[agents.ContextPartMessages] != 0 {
		t.Errorf("消息分区 = %d, want 0（锚点过小）", byKey[agents.ContextPartMessages])
	}
	if byKey[agents.ContextPartSystem] != 100 {
		t.Errorf("系统提示词 = %d, want 100（仍按估算展示）", byKey[agents.ContextPartSystem])
	}
	if !data["degraded"].(bool) {
		t.Error("锚点 < 静态合计时应 degraded=true")
	}
	if got := data["total"].(int64); got != 80 {
		t.Errorf("total = %d, want 80（总量仍取锚点）", got)
	}
}

// TestCtxBreakdownPayloadEstimatedAnchor 锚点本身是估算（压缩后的字符口径）：
// 静态分区照旧固定，estimated/anchor_source 如实标注，消息取残差。
func TestCtxBreakdownPayloadEstimatedAnchor(t *testing.T) {
	bd := agents.ContextBreakdown{
		Parts: []agents.ContextPart{
			{Key: agents.ContextPartTools, Chars: 120, Tokens: 30},
			{Key: agents.ContextPartMessages, Chars: 400, Tokens: 100},
		},
		TotalTokens: 130,
	}
	data := ctxBreakdownPayload(bd, ctxAnchor{Tokens: 530, Estimated: true})
	if !data["estimated"].(bool) {
		t.Error("估算锚点 estimated 应为 true")
	}
	if got := data["anchor_source"].(string); got != "estimate" {
		t.Errorf("anchor_source = %q, want estimate", got)
	}
	parts := payloadParts(t, data)
	if got := sumTokens(parts); got != 530 {
		t.Errorf("分区合计 = %d, want 530", got)
	}
}

// TestCtxBreakdownPayloadNoAnchor 尚无真实 usage（新会话 / 从未调用）：
// 纯估算总量 + estimated=true + anchor_source=none，供前端标注。
func TestCtxBreakdownPayloadNoAnchor(t *testing.T) {
	bd := agents.ContextBreakdown{
		Parts: []agents.ContextPart{
			{Key: agents.ContextPartSystem, Chars: 800, Tokens: 200},
			{Key: agents.ContextPartMessages, Chars: 1600, Tokens: 400},
		},
		TotalTokens:  600,
		MessageCount: 7,
	}
	data := ctxBreakdownPayload(bd, ctxAnchor{})
	parts := payloadParts(t, data)
	if got := sumTokens(parts); got != 600 {
		t.Errorf("分区合计 = %d, want 600（无锚点：原样估算）", got)
	}
	if got := data["total"].(int64); got != 600 {
		t.Errorf("total = %d, want 600", got)
	}
	if !data["estimated"].(bool) {
		t.Error("无真实 usage 时 estimated 应为 true")
	}
	if got := data["anchor_source"].(string); got != "none" {
		t.Errorf("anchor_source = %q, want none", got)
	}
	if got := data["message_count"].(int); got != 7 {
		t.Errorf("message_count = %d, want 7", got)
	}
}

// TestCtxBreakdownPayloadEmpty 空装配（无工具/无消息/无系统提示词）：
// 不 panic、总量 0、estimated 随真实锚点。
func TestCtxBreakdownPayloadEmpty(t *testing.T) {
	bd := agents.ContextBreakdown{Parts: []agents.ContextPart{{Key: agents.ContextPartSystem}}}
	data := ctxBreakdownPayload(bd, ctxAnchor{})
	if got := data["total"].(int64); got != 0 {
		t.Errorf("total = %d, want 0", got)
	}
	if got := sumTokens(payloadParts(t, data)); got != 0 {
		t.Errorf("分区合计 = %d, want 0", got)
	}
}
