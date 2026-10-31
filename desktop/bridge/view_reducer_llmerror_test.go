// view_reducer_llmerror_test.go —— 失败 attempt 的记账与**轮次收尾**口径（2026-10）。
//
// 背景：中断发生在 llm_start ↔ llm_end 之间时，这一轮永远不会有 llm_end。此前
// LLMError 只记成本、不记 token、不落 Turn —— 顶栏 in/out 与轮尾账（按 Turns 求和）
// 都少这一份，而 metrics 侧一直记全（Kind=attempt_failed），同一件事两套账。
//
// 三条契约（本文件钉住）：
//  1. 每一次失败 attempt 的 **tokens** 与成本一样进会话/终身累计（拿不到时为零，不编）；
//  2. `will_retry=true` 的中间失败**不是**轮尾 —— 同一个逻辑轮会换 request id 重来，
//     不落 Turn、不动轮起点（否则每次退避重试都会多出一“轮”）；
//  3. `will_retry=false` 的最终失败是轮次收尾：补一条 Turn（含时长与本次用量），
//     并清掉本轮的轮起点；连最终失败都没等到时（退避中被 abort）由 AgentEnd 兜底收尾。
package main

import (
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

func evLLMError(runID, requestID, model string, attempt int, willRetry bool, kind string, u *core.Usage, ts time.Time) *events.LLMError {
	return &events.LLMError{
		RunId: runID, Model: model, RequestId: requestID, Message: "boom",
		Attempt: attempt, WillRetry: willRetry, ErrorKind: kind,
		Usage: u, Timestamp: ts, EventType: events.LLMErrorType,
	}
}

func TestReducerLLMErrorAccountsTokensButRetryIsNotRoundClose(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Minute)
	mustApply(t, r, evLLMStart(t, "r1", "q1", "m", ts))

	a1 := core.Usage{Input: 100, Output: 10, TotalTokens: 110,
		Cost: core.Cost{Input: 1, Output: 1, Total: 2, Priced: true, Source: core.CostSourceBuiltin}}
	// 会重试的失败：记账（tokens 进累计），但不是轮尾
	mustApply(t, r, evLLMError("r1", "q1", "m", 1, true, "rate_limit", &a1, ts.Add(time.Second)))

	cp := r.Snapshot()
	if len(cp.Turns) != 0 {
		t.Fatalf("will_retry=true 的中间失败不得落 Turn（got %d）", len(cp.Turns))
	}
	if cp.Usage.Input != 100 || cp.Usage.Output != 10 {
		t.Fatalf("失败 attempt 的 tokens 必须进累计: %+v", cp.Usage)
	}
	if _, ok := r.reqState["q1"]; ok {
		t.Fatal("失败 attempt 的 reqState 必须精确回收（RequestId 已随事件下发）")
	}

	// 第二次尝试：新 request id，最终失败（aborted）→ 轮次收尾
	mustApply(t, r, evLLMStart(t, "r1", "q2", "m", ts.Add(2*time.Second)))
	a2 := core.Usage{Input: 120, Output: 12, TotalTokens: 132,
		Cost: core.Cost{Input: 2, Output: 1, Total: 3, Priced: true, Source: core.CostSourceBuiltin}}
	mustApply(t, r, evLLMError("r1", "q2", "m", 2, false, "aborted", &a2, ts.Add(3*time.Second)))

	cp = r.Snapshot()
	if cp.Usage.Input != 220 || cp.Usage.Output != 22 {
		t.Fatalf("两次 attempt 的 tokens 都要进累计: %+v", cp.Usage)
	}
	if cp.LifetimeUsage.Input != 220 {
		t.Fatalf("lifetime 也应累计: %+v", cp.LifetimeUsage)
	}
	if len(cp.Turns) != 1 {
		t.Fatalf("最终失败应补一条 Turn（恰好一轮）: got %d", len(cp.Turns))
	}
	turn := cp.Turns[0]
	if turn.RequestID != "q2" {
		t.Fatalf("Turn.RequestID = %q, want q2（归到失败那次 attempt）", turn.RequestID)
	}
	if turn.Input != 120 || turn.Output != 12 {
		t.Fatalf("Turn 用量应只含本次 attempt: %+v", turn)
	}
	if turn.DurMs <= 0 {
		t.Fatalf("Turn.DurMs 应取 llm_start → 失败时刻（got %d）", turn.DurMs)
	}
	if turn.FinishReason != string(core.FinishReasonAbort) {
		t.Fatalf("Turn.FinishReason = %q, want abort", turn.FinishReason)
	}
	if _, ok := r.turnStart["r1"]; ok {
		t.Fatal("轮次收尾后不得残留轮起点（否则 AgentEnd 会再兜底一条）")
	}
	if errs := blockByKind(t, cp, "error"); len(errs) != 1 {
		t.Fatalf("error block = %d, want 1", len(errs))
	}
}

func TestReducerAgentEndClosesRoundThatNeverGotFinalError(t *testing.T) {
	// 典型形态：attempt 1 失败（will_retry=true）→ 退避等待中被 abort → 只有 AgentEnd。
	r := NewViewReducer()
	ts := time.Now().Add(-time.Second)
	mustApply(t, r, evLLMStart(t, "r1", "q1", "m", ts))
	a1 := core.Usage{Input: 50, Output: 5, TotalTokens: 55,
		Cost: core.Cost{Input: 1, Output: 1, Total: 2, Priced: true, Source: core.CostSourceBuiltin}}
	mustApply(t, r, evLLMError("r1", "q1", "m", 1, true, "rate_limit", &a1, ts.Add(500*time.Millisecond)))
	mustApply(t, r, &events.AgentEnd{
		RunId: "r1", Model: "m", FinishReason: core.FinishReasonAbort,
		Timestamp: ts.Add(time.Second), EventType: events.AgentEndType,
	})

	cp := r.Snapshot()
	if len(cp.Turns) != 1 {
		t.Fatalf("AgentEnd 应兜底收尾本轮: turns = %d, want 1", len(cp.Turns))
	}
	turn := cp.Turns[0]
	if turn.Input != 0 || turn.Output != 0 {
		t.Fatalf("兜底 Turn 只带时长（token 不编）: %+v", turn)
	}
	if turn.DurMs <= 0 {
		t.Fatalf("兜底 Turn 应带时长: %+v", turn)
	}
}
