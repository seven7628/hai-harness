package main

import (
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/events"
)

// TestReducerUpstreamUnavailableAttribution 上游不可用 + 重试耗尽：主 run 错误块必须给出
// 「供应商侧」归因，否则用户会把 5xx 抖动当成自己的请求/本地环境问题。对照：仍有重试机会时
// 走普通重试文案，不得提前渲染终止归因。
func TestReducerUpstreamUnavailableAttribution(t *testing.T) {
	ts := time.Now().Add(-time.Hour)
	const detail = "503 Service Unavailable: Endpoint is unavailable"

	t.Run("retries exhausted", func(t *testing.T) {
		r := NewViewReducer()
		mustApply(t, r, evLLMStart(t, "r1", "q1", "m1", ts))
		mustApply(t, r, &events.LLMError{
			RunId: "r1", Message: detail, Attempt: 3, WillRetry: false,
			ErrorKind: "upstream_unavailable", Timestamp: ts, EventType: events.LLMErrorType,
		})

		cp := r.Snapshot()
		errs := blockByKind(t, cp, "error")
		if len(errs) != 1 {
			t.Fatalf("error blocks = %d, want 1", len(errs))
		}
		if !strings.Contains(errs[0].Text, "供应商侧瞬时故障") {
			t.Fatalf("上游不可用终止必须说明是供应商侧故障，实际文本 = %q", errs[0].Text)
		}
		if !strings.Contains(errs[0].Text, detail) {
			t.Fatalf("原始错误文本必须保留，实际 = %q", errs[0].Text)
		}
		if cp.LastError == nil || cp.LastError.Kind != "upstream_unavailable" {
			t.Fatalf("lastError mismatch: %+v", cp.LastError)
		}
	})

	t.Run("still retrying", func(t *testing.T) {
		r := NewViewReducer()
		mustApply(t, r, evLLMStart(t, "r1", "q1", "m1", ts))
		mustApply(t, r, &events.LLMError{
			RunId: "r1", Message: detail, Attempt: 1, MaxAttempts: 11, WillRetry: true,
			RetryDelayMs: 1000, ErrorKind: "upstream_unavailable", Timestamp: ts, EventType: events.LLMErrorType,
		})

		errs := blockByKind(t, r.Snapshot(), "error")
		if len(errs) != 1 {
			t.Fatalf("error blocks = %d, want 1", len(errs))
		}
		if strings.Contains(errs[0].Text, "供应商侧瞬时故障") {
			t.Fatalf("仍会重试时不得渲染终止归因: %q", errs[0].Text)
		}
		// 文案语义（2026-09-18 改）：Attempt = 已失败的那一次，必须把「接下来重试第几次/
		// 共几次」写清 —— 旧的「正在重试 1…」在尝试挂死时被读成「卡住/重试没了」。
		if !strings.Contains(errs[0].Text, "第 1/11 次尝试失败") || !strings.Contains(errs[0].Text, "重试第 2/11 次") {
			t.Fatalf("重试进度文案不对: %q", errs[0].Text)
		}
		// 实时状态字段（倒计时/已等待时长的计算依据）一并落库
		if !errs[0].Retrying || errs[0].RetryAttempt != 1 || errs[0].RetryMax != 11 || errs[0].RetryDelayMs != 1000 {
			t.Fatalf("重试进度字段不对: %+v", errs[0])
		}
		if errs[0].Ts != ts.UnixMilli() {
			t.Fatalf("锚点应为事件时刻（重放后倒计时仍准）: got %d want %d", errs[0].Ts, ts.UnixMilli())
		}

		// 第 2 次也失败（Attempt=2）：同一错误块原位更新为「第 2/11 次失败 → 重试第 3/11 次」
		mustApply(t, r, &events.LLMError{
			RunId: "r1", Message: detail, Attempt: 2, MaxAttempts: 11, WillRetry: true,
			RetryDelayMs: 1000, ErrorKind: "upstream_unavailable", Timestamp: ts.Add(2 * time.Second), EventType: events.LLMErrorType,
		})
		errs = blockByKind(t, r.Snapshot(), "error")
		if len(errs) != 1 {
			t.Fatalf("重试不得刷屏（原位更新）: error blocks = %d", len(errs))
		}
		if !strings.Contains(errs[0].Text, "第 2/11 次尝试失败") || errs[0].RetryAttempt != 2 {
			t.Fatalf("第 2 次失败应原位更新进度: %+v", errs[0])
		}
		if errs[0].RetryStartedAt != ts.UnixMilli() {
			t.Fatalf("本轮重试起点应保持首个失败时刻（累计「已等待时长」）: got %d want %d", errs[0].RetryStartedAt, ts.UnixMilli())
		}
	})

	// 子 agent 卡片路径：同一归因文案 + 卡片终态 failed、TaskError 回填原始消息。
	t.Run("sub agent card failed", func(t *testing.T) {
		r := NewViewReducer()
		mustApply(t, r, evAgentStart(t, "root-1", "", "主会话", "", ts))
		mustApply(t, r, evAgentStart(t, "sub-1", "root-1", "调查", "task-9", ts.Add(time.Second)))
		mustApply(t, r, &events.LLMError{
			RunId: "sub-1", Message: detail, Attempt: 3, WillRetry: false,
			ErrorKind: "upstream_unavailable", Timestamp: ts.Add(2 * time.Second), EventType: events.LLMErrorType,
		})

		agents := blockByKind(t, r.Snapshot(), "agent")
		if len(agents) != 1 || agents[0].RunID != "sub-1" {
			t.Fatalf("agent cards mismatch: %+v", agents)
		}
		ag := agents[0]
		items := ag.Items
		if len(items) != 1 || items[0].Kind != "text" || !items[0].IsError ||
			!strings.Contains(items[0].Text, "供应商侧瞬时故障") {
			t.Fatalf("子 agent 错误项应带归因文案: %+v", items)
		}
		if ag.Status != "failed" || ag.TaskError != detail {
			t.Fatalf("重试耗尽的子 agent 卡片应为 failed 并回填 TaskError: %+v", ag)
		}
	})
}
