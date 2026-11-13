package main

import (
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
)

// 取消文案的归因回归（2026-09-24）。
//
// 背景（实测日志）：48 条 llm_error 里 6 条是 context 取消，**全部**是 attempt=1 +
// will_retry=false —— 取消必然发生在第 1 次尝试上（第 1 次还没回来用户就点了停止）。
// 此前 UI 只看 !WillRetry 就输出「重试已耗尽」，用户读到的却是「重试机制没了」，
// 实际一次都没重试。修法：把取消来源拆成两个 error_kind（都在 agents 层按 abort 标志判定，
// 见 agents.streamWithRetry.llmErrorKind），文案分三路：
//
//	aborted  用户点停止 / agent_interrupt → 「已中断」（是用户自己的动作）
//	canceled 会话关闭 / 后台任务终止      → 「已取消」（用户没按停止，不归因给用户）
//	其余     真正重试跑完                → 保留「重试已耗尽」
func TestReducerCanceledVsAbortedText(t *testing.T) {
	ts := time.Now().Add(-time.Hour)

	// 子 agent 卡片路径：中断后卡片状态由 agent_end(abort) 收敛为 interrupted，
	// 但 applyLLMErrorLocked 写进 items 的错误项不会被摘掉（abortAgentCompressionItems
	// 只管压缩项）——所以这句文案用户是**真看得见**的，必须归因正确。
	t.Run("sub agent aborted by user", func(t *testing.T) {
		r := NewViewReducer()
		mustApply(t, r, evAgentStart(t, "root-1", "", "主会话", "", ts))
		mustApply(t, r, evAgentStart(t, "sub-1", "root-1", "调查", "task-9", ts.Add(time.Second)))
		mustApply(t, r, &events.LLMError{
			RunId: "sub-1", Message: "context canceled", Attempt: 1, MaxAttempts: 11,
			WillRetry: false, ErrorKind: provider.ErrorKindAborted,
			Timestamp: ts.Add(2 * time.Second), EventType: events.LLMErrorType,
		})

		text := agentCardErrorText(t, r)
		if !strings.Contains(text, "已中断") {
			t.Fatalf("用户中断必须说「已中断」: %q", text)
		}
		if strings.Contains(text, "重试已耗尽") {
			t.Fatalf("用户中断一次都没重试，不得说「重试已耗尽」: %q", text)
		}
		if strings.Contains(text, "重试第") {
			t.Fatalf("中断不重试，不得渲染重试进度: %q", text)
		}
		if !strings.Contains(text, "context canceled") {
			t.Fatalf("原始错误文本必须保留（诊断用）: %q", text)
		}
	})

	t.Run("system cancel without user abort", func(t *testing.T) {
		r := NewViewReducer()
		mustApply(t, r, evAgentStart(t, "root-1", "", "主会话", "", ts))
		mustApply(t, r, evAgentStart(t, "sub-1", "root-1", "调查", "task-9", ts.Add(time.Second)))
		mustApply(t, r, &events.LLMError{
			RunId: "sub-1", Message: "context canceled", Attempt: 1, MaxAttempts: 11,
			WillRetry: false, ErrorKind: provider.ErrorKindCanceled,
			Timestamp: ts.Add(2 * time.Second), EventType: events.LLMErrorType,
		})

		text := agentCardErrorText(t, r)
		if !strings.Contains(text, "已取消") {
			t.Fatalf("系统取消必须说「已取消」: %q", text)
		}
		// 用户没按停止：不得把取消归因成用户的动作
		if strings.Contains(text, "用户中断") {
			t.Fatalf("系统取消不得归因给用户: %q", text)
		}
		if strings.Contains(text, "重试已耗尽") {
			t.Fatalf("系统取消一次都没重试，不得说「重试已耗尽」: %q", text)
		}
	})

	// 对照：真正的重试耗尽（11 次跑完）文案不能被上面两个 early return 影响。
	t.Run("genuine retries exhausted", func(t *testing.T) {
		r := NewViewReducer()
		mustApply(t, r, evLLMStart(t, "r1", "q1", "m1", ts))
		mustApply(t, r, &events.LLMError{
			RunId: "r1", Message: "boom", Attempt: 11, MaxAttempts: 11,
			WillRetry: false, ErrorKind: provider.ErrorKindGeneric,
			Timestamp: ts.Add(2 * time.Second), EventType: events.LLMErrorType,
		})

		errs := blockByKind(t, r.Snapshot(), "error")
		if len(errs) != 1 {
			t.Fatalf("error blocks = %d, want 1", len(errs))
		}
		if !strings.Contains(errs[0].Text, "第 11/11 次尝试失败") || !strings.Contains(errs[0].Text, "重试已耗尽") {
			t.Fatalf("真耗尽文案被改坏了: %q", errs[0].Text)
		}
	})

	// 主 run：abort 时叙述通道的 error 块会被 agent_end(abort) 清掉（既有语义），
	// 但 lastError 仍记录本次失败——Kind 必须是 aborted，供上层区分「用户停的」。
	t.Run("last error kind", func(t *testing.T) {
		r := NewViewReducer()
		mustApply(t, r, evLLMStart(t, "r1", "q1", "m1", ts))
		mustApply(t, r, &events.LLMError{
			RunId: "r1", Message: "context canceled", Attempt: 1, MaxAttempts: 11,
			WillRetry: false, ErrorKind: provider.ErrorKindAborted,
			Timestamp: ts, EventType: events.LLMErrorType,
		})

		cp := r.Snapshot()
		if cp.LastError == nil || cp.LastError.Kind != provider.ErrorKindAborted {
			t.Fatalf("lastError.Kind = %+v, want %q", cp.LastError, provider.ErrorKindAborted)
		}
	})
}

// agentCardErrorText 取子 agent 卡片上最后一条错误项文本（用户实际看到的那句）。
func agentCardErrorText(t *testing.T, r *ViewReducer) string {
	t.Helper()
	cp := r.Snapshot()
	for _, b := range cp.Blocks {
		if b.Kind != "agent" {
			continue
		}
		for i := len(b.Items) - 1; i >= 0; i-- {
			if b.Items[i].IsError {
				return b.Items[i].Text
			}
		}
	}
	t.Fatal("子 agent 卡片上找不到错误项")
	return ""
}

// TestReducerAbortKeepsSubAgentErrorText 中断收尾不得误伤：子 agent 卡片的错误项在
// agent_end(abort) 后仍然保留（用户要能看见「为什么这个子任务停了」），且卡片终态为 interrupted。
func TestReducerAbortKeepsSubAgentErrorText(t *testing.T) {
	ts := time.Now().Add(-time.Hour)
	r := NewViewReducer()
	mustApply(t, r, evAgentStart(t, "root-1", "", "主会话", "", ts))
	mustApply(t, r, evAgentStart(t, "sub-1", "root-1", "调查", "task-9", ts.Add(time.Second)))
	mustApply(t, r, &events.LLMError{
		RunId: "sub-1", Message: "context canceled", Attempt: 1, MaxAttempts: 11,
		WillRetry: false, ErrorKind: provider.ErrorKindAborted,
		Timestamp: ts.Add(2 * time.Second), EventType: events.LLMErrorType,
	})
	mustApply(t, r, evAgentEnd(t, "sub-1", core.FinishReasonAbort, ts.Add(3*time.Second)))

	cp := r.Snapshot()
	for _, b := range cp.Blocks {
		if b.Kind != "agent" {
			continue
		}
		if b.Status != "interrupted" {
			t.Fatalf("子 agent 卡片终态 = %q, want interrupted", b.Status)
		}
		if text := agentCardErrorText(t, r); !strings.Contains(text, "已中断") {
			t.Fatalf("中断后卡片仍应保留归因正确的错误项: %q", text)
		}
	}
}
