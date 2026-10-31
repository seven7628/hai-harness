package main

// session_closed 的运行账（设计 v2：「18 runs · 17 consumed · 1 abandoned mid-tool」）。
//
// 这条线横跨三层：agents（死在工具上与否）→ session（记账）→ bridge（转发 JSON）→
// 前端（signalFacts 拼副题）。字段名一旦改错，UI 只会安静地少一行副题
// （omitempty + 缺字段不画），任何前端单测都不会红。所以这里把**线格式**钉死。

import (
	"context"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/tools"
)

func stopRound(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
}

// TestSessionClosedCarriesRunLedger 正常跑一轮再 shutdown：
// 事件里必须带 runs=1 / consumed=1，且 abandoned_mid_tool 不出现（0 省略）。
func TestSessionClosedCarriesRunLedger(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, buf := testManager(t)
	ws := t.TempDir()
	sid := newSessionID(t, m, buf, ws)

	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session not found")
	}
	p := &bridgeMockProvider{rounds: []func(*provider.StreamRequest, func(e provider.StreamEvent) error) error{stopRound, stopRound}}
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(p)))

	buf.Reset()
	m.dispatch(cmd(1, "ask", map[string]any{"workspace": ws, "session_id": sid, "text": "hi"}))
	waitBridgeString(t, buf, func(s string) bool { return countAgentEnd(s) >= 1 }, 5*time.Second)

	buf.Reset()
	m.dispatch(cmd(2, "shutdown", map[string]any{"workspace": ws}))
	waitBridgeString(t, buf, func(s string) bool {
		return findFirst(s, func(mm map[string]any) bool { return mm["event_type"] == "session_closed" }) != nil
	}, 5*time.Second)

	ev := findFirst(buf.String(), func(mm map[string]any) bool { return mm["event_type"] == "session_closed" })
	if ev == nil {
		t.Fatal("want session_closed")
	}
	if got := ev["runs"]; got != float64(1) {
		t.Errorf("runs = %v, want 1（线上字段名必须是 runs）", got)
	}
	if got := ev["consumed"]; got != float64(1) {
		t.Errorf("consumed = %v, want 1", got)
	}
	if got, ok := ev["abandoned_mid_tool"]; ok {
		t.Errorf("abandoned_mid_tool 应为 0 且省略（omitempty），实际 %v", got)
	}
	if _, ok := ev["session_id"]; !ok {
		t.Error("session_closed 缺 session_id")
	}
}

// TestSessionClosedAbandonedMidToolWire Shutdown 时正卡在工具里：
// abandoned_mid_tool=1 且 consumed=0 —— 前端据此说「1 次停在工具中间」。
func TestSessionClosedAbandonedMidToolWire(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, buf := testManager(t)
	ws := t.TempDir()
	sid := newSessionID(t, m, buf, ws)

	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session not found")
	}
	block := &bridgeBlockTool{gate: make(chan struct{})}
	defer block.Release()
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), block)
	p := &bridgeMockProvider{rounds: []func(*provider.StreamRequest, func(e provider.StreamEvent) error) error{
		func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonToolCall,
				ToolCalls:    []core.ToolCall{{Id: "c1", Index: 0, Name: "block", Arguments: `{}`}},
			})
		},
		stopRound,
	}}
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(p), agents.WithToolEngine(eng), agents.WithMaxIterations(3)))

	buf.Reset()
	m.dispatch(cmd(1, "ask", map[string]any{"workspace": ws, "session_id": sid, "text": "run"}))
	// 等工具真的开始跑（tool_run_start）；此时 shutdown = 打断在工具中间
	waitBridgeString(t, buf, func(s string) bool {
		return findFirst(s, func(mm map[string]any) bool { return mm["event_type"] == "tool_run_start" }) != nil
	}, 5*time.Second)

	buf.Reset()
	m.dispatch(cmd(2, "shutdown", map[string]any{"workspace": ws}))
	waitBridgeString(t, buf, func(s string) bool {
		return findFirst(s, func(mm map[string]any) bool { return mm["event_type"] == "session_closed" }) != nil
	}, 8*time.Second)

	ev := findFirst(buf.String(), func(mm map[string]any) bool { return mm["event_type"] == "session_closed" })
	if ev == nil {
		t.Fatal("want session_closed")
	}
	if got := ev["runs"]; got != float64(1) {
		t.Errorf("runs = %v, want 1", got)
	}
	if got, ok := ev["consumed"]; ok {
		t.Errorf("consumed 应为 0 且省略，实际 %v", got)
	}
	if got := ev["abandoned_mid_tool"]; got != float64(1) {
		t.Errorf("abandoned_mid_tool = %v, want 1", got)
	}
}
