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
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/session"
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

	// 账目必须先于事件落定：agent_end 一出现在线上，会话账就已经是最终值
	//（R08f：旧实现把记账放在 RunStream 返回之后，事件已出门、账还是旧的）。
	// 这里不做任何等待 —— 等到 agent_end 才读，读到旧值就是红。
	if runs, consumed, _ := bs.s.RunLedger(); runs != 1 || consumed != 1 {
		t.Errorf("agent_end 到达时账目 runs=%d consumed=%d, want 1/1（事件与账目必须同源同序）", runs, consumed)
	}

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

// TestRunLedgerSettlesBeforeAgentEndReachesConsumer 把 R08f 的偶发窗口拉满：
// 用户在**看到答案的瞬间**点「停止」（AgentEnd 已宣告 stop，产出已交付）。
//
// 旧实现：AgentEnd 发出之后才在 RunStream 返回处记账 → Interrupt/Shutdown 恰好落在
// 这两步之间时，ac.aborted 被翻起来，账目回读它 → 这一轮既不算 consumed 也不算
// abandoned，永久丢失（1/10 偶发：`consumed = <nil>, want 1`）。
// 新实现：账目只认 AgentEnd 宣告的事实，且写在事件交给消费者之前 —— 两个性质一起钉：
//  1. AgentEnd 到达那一刻，RunLedger 已经是最终值（先于事件可见）；
//  2. 事后打断不得翻案（不再回读 ac 的可变状态）。
//
// 为什么用受控 handler 直调 Session.Run：窗口只有几微秒，走 stdin 的 ask→AgentEnd
// 轮询无法稳定命中（那正是它 1/10 偶发的原因）。Run / Interrupt / RunLedger 都是
// 生产入口（bridge 的 runLoop 只是它们的循环包装），线格式同源见上面的用例。
func TestRunLedgerSettlesBeforeAgentEndReachesConsumer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, buf := testManager(t)
	ws := t.TempDir()
	sid := newSessionID(t, m, buf, ws)

	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session not found")
	}
	p := &bridgeMockProvider{rounds: []func(*provider.StreamRequest, func(e provider.StreamEvent) error) error{stopRound}}
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(p)))

	if err := bs.s.Ask(context.Background(), core.NewUserMessage(core.Content{Type: "text", Content: "hi"})); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	var (
		atEnd       [3]int
		sawEnd      bool
		endFinishRs core.FinishReason
	)
	err := bs.s.Run(context.Background(), func(_ context.Context, e events.Event) {
		if ae, ok := e.(*events.AgentEnd); ok {
			atEnd = ledgerOf(bs.s)
			endFinishRs = ae.FinishReason
			sawEnd = true
			bs.s.Interrupt() // 用户看到答案 → 点停止：Abort 落在「事件之后、收尾之前」
		}
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !sawEnd {
		t.Fatal("want AgentEnd")
	}
	if endFinishRs != core.FinishReasonStop {
		t.Fatalf("AgentEnd.FinishReason = %q, want stop（本用例的前提：事件宣告正常收尾）", endFinishRs)
	}
	assertRunLedger(t, "AgentEnd 到达时", atEnd, [3]int{1, 1, 0})
	assertRunLedger(t, "收尾后（事后 Interrupt 不得翻案）", ledgerOf(bs.s), [3]int{1, 1, 0})
}

// ledgerOf 读会话运行账的只读快照：[runs, consumed, abandoned mid-tool]。
func ledgerOf(s *session.Session) [3]int {
	runs, consumed, abandoned := s.RunLedger()
	return [3]int{runs, consumed, abandoned}
}

// assertRunLedger 断言运行账三元组（want 与 RunLedger 同序）。
func assertRunLedger(t *testing.T, when string, got, want [3]int) {
	t.Helper()
	if got != want {
		t.Errorf("%s runs/consumed/abandoned = %v, want %v", when, got, want)
	}
}

// TestRunLedgerEarlyInterruptStaysUnconsumed 反例：打断落在收尾**之前**（流式阶段），
// 那一轮不能算 consumed —— 否则「事件即事实」就退化成「一律算交付」。
func TestRunLedgerEarlyInterruptStaysUnconsumed(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, buf := testManager(t)
	ws := t.TempDir()
	sid := newSessionID(t, m, buf, ws)

	bs := m.get(ws, sid)
	if bs == nil {
		t.Fatal("session not found")
	}
	interrupt := func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
		bs.s.Interrupt() // 模型还没产出最终答复就打断
		return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "半截"})
	}
	p := &bridgeMockProvider{rounds: []func(*provider.StreamRequest, func(e provider.StreamEvent) error) error{interrupt}}
	bs.s.SetLoop(agents.NewAgentLoop(agents.WithProvider(p)))

	if err := bs.s.Ask(context.Background(), core.NewUserMessage(core.Content{Type: "text", Content: "hi"})); err != nil {
		t.Fatalf("Ask: %v", err)
	}
	if err := bs.s.Run(context.Background(), nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	assertRunLedger(t, "收尾前打断（两类都不计）", ledgerOf(bs.s), [3]int{1, 0, 0})
}
