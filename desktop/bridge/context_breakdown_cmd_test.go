package main

import (
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// TestContextBreakdownCommand 端到端：会话（真实工具集 + 系统提示词）跑过一轮后，
// context_breakdown 返回六个分区，总量与 reducer 的真实 ctx_tokens 相等（面板总量
// 与 ctx 环一致），且分区合计恒等于总量（条形图不留空白）。
func TestContextBreakdownCommand(t *testing.T) {
	ws := t.TempDir()
	m, buf := testManager(t)
	sid := newSessionID(t, m, buf, ws)
	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session not found")
	}
	// 真实 usage 锚点：llm_end 的 Input 即 ctx 环显示的占用（reducer 结算）。
	bs.emit(&events.LLMEnd{
		RunId: "r1", RequestId: "q1", Model: "deepseek-v4-flash",
		Usage: &core.Usage{Input: 40000, Output: 120}, FinishReason: core.FinishReasonStop,
		Timestamp: time.Now(), EventType: events.LLMEndType,
	})

	buf.Reset()
	m.dispatch(cmd(7, string(CmdContextBreakdown), map[string]any{"workspace": ws, "session_id": sid}))

	var resp map[string]any
	for _, l := range readLines(buf) {
		if l["event_type"] == "command_response" && l["id"] == float64(7) {
			resp = l
			break
		}
	}
	if resp == nil {
		t.Fatal("context_breakdown 无响应")
	}
	if resp["ok"] != true {
		t.Fatalf("context_breakdown 失败: %v", resp["error"])
	}
	data, _ := resp["data"].(map[string]any)
	if data == nil {
		t.Fatal("响应缺 data")
	}
	raw, ok := data["parts"].([]any)
	// 7 个分区：system_prompt / skills / memory / tools / mcp / messages / other。
	// memory 是 2026-09-29 加的（Project Memory 索引单独计量，见 ContextPartMemory）。
	if !ok || len(raw) != 7 {
		t.Fatalf("parts = %#v, want 7 个分区", data["parts"])
	}
	var sum float64
	seen := map[string]bool{}
	for _, it := range raw {
		p, _ := it.(map[string]any)
		key, _ := p["key"].(string)
		tokens, _ := p["tokens"].(float64)
		seen[key] = true
		sum += tokens
	}
	for _, k := range []string{
		"system_prompt", "skills", "memory", "tools", "mcp", "messages", "other",
	} {
		if !seen[k] {
			t.Errorf("缺分区 %s: %+v", k, seen)
		}
	}
	if got := data["total"].(float64); got != 40000 {
		t.Errorf("total = %v, want 40000（取真实 usage）", got)
	}
	if sum != 40000 {
		t.Errorf("分区合计 = %v, want 40000（余数补给最大分区）", sum)
	}
	if data["estimated"] != false {
		t.Errorf("estimated = %v, want false（有真实锚点）", data["estimated"])
	}
	// 工具/系统提示词确有其物（真实引擎 + 真实 prompt）；空会话里它们的估算值**固定**
	// （不随锚点放大），消息分区取残差 = 总量 − 静态合计。
	byKey := map[string]float64{}
	for _, it := range raw {
		p, _ := it.(map[string]any)
		byKey[p["key"].(string)] = p["tokens"].(float64)
	}
	if byKey["tools"] <= 0 || byKey["system_prompt"] <= 0 {
		t.Errorf("工具/系统提示词分区应 > 0: %+v", byKey)
	}
	// 静态合计**必须含 memory 分区**（2026-09-30 修正）。
	//
	// 这行原来漏了 memory，而它此前恒为 0：记忆目录只在 git 仓库里解析得出来，
	// 而本测试跑在临时目录（非 git）→ 公式虽错却一直成立。记忆回落到项目根
	// （官方 memory#storage-location）之后 memory 分区有了真实字节，公式立刻不对。
	//
	// 形态与本项目那四处误读同源：**"当前恰好等于零"被当成了"这一项不存在"**。
	staticSum := byKey["system_prompt"] + byKey["skills"] + byKey["memory"] +
		byKey["tools"] + byKey["mcp"] + byKey["other"]
	if byKey["messages"] != 40000-staticSum {
		t.Errorf("消息分区 = %v, want %v（残差 = 总量 − 静态合计，含 memory）: %+v",
			byKey["messages"], 40000-staticSum, byKey)
	}
	if data["anchor_source"] != "real" {
		t.Errorf("anchor_source = %v, want real", data["anchor_source"])
	}
	if data["degraded"] != false {
		t.Errorf("degraded = %v, want false", data["degraded"])
	}
	// 会话不存在 → session_not_found（与其余会话级命令同口径）。
	buf.Reset()
	m.dispatch(cmd(8, string(CmdContextBreakdown), map[string]any{"workspace": ws, "session_id": "nope"}))
	got := findFirst(buf.String(), func(mm map[string]any) bool { return mm["id"] == float64(8) })
	if got["ok"] != false || str(got, "code") != "session_not_found" {
		t.Fatalf("未知会话应返回 session_not_found: %+v", got)
	}
}
