package provider

import (
	"context"
	"testing"

	"github.com/seven7628/hai-harness/core"
)

// 工具调用「双索引」合并（2026-10，对齐 pi openai-completions.ts:485-541）：
// index 是主键、id 是辅键。只用 index 时，下面前两个场景都会把调用拼坏（合并成一个 /
// 凭空多出一个无名调用），且**不会报错** —— 只有走到工具执行才炸。
func TestToolCallDualIndexMerge(t *testing.T) {
	chunk := func(delta map[string]any) map[string]any {
		return map[string]any{"id": "c", "object": "chat.completion.chunk", "model": "m",
			"choices": []any{map[string]any{"index": 0, "delta": delta}}}
	}
	tc := func(fields map[string]any) map[string]any {
		return map[string]any{"tool_calls": []any{fields}}
	}
	finish := map[string]any{"id": "c", "object": "chat.completion.chunk", "model": "m",
		"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}}

	run := func(t *testing.T, chunks []map[string]any) ([]core.ToolCall, int) {
		t.Helper()
		srv, _ := sseServer(t, chunks)
		defer srv.Close()
		p := NewOpenAIProvider(srv.URL, "k")
		var calls []core.ToolCall
		starts := 0
		err := p.Stream(context.Background(), &StreamRequest{
			Model:    "m",
			Messages: []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "hi"})},
		}, func(e StreamEvent) error {
			switch ev := e.(type) {
			case LLMToolCallStartEvent:
				starts++
			case LLMEndEvent:
				calls = ev.ToolCalls
			}
			return nil
		})
		if err != nil {
			t.Fatalf("Stream: %v", err)
		}
		return calls, starts
	}

	t.Run("全程不发 index + 两个并行调用 → 两个调用", func(t *testing.T) {
		calls, starts := run(t, []map[string]any{
			chunk(tc(map[string]any{"id": "call_a", "type": "function",
				"function": map[string]any{"name": "read_file", "arguments": `{"path":"a"}`}})),
			chunk(tc(map[string]any{"id": "call_b", "type": "function",
				"function": map[string]any{"name": "read_file", "arguments": `{"path":"b"}`}})),
			finish,
		})
		if len(calls) != 2 || starts != 2 {
			t.Fatalf("calls=%d starts=%d, want 2/2: %+v", len(calls), starts, calls)
		}
		if calls[0].Id != "call_a" || calls[0].Arguments != `{"path":"a"}` {
			t.Fatalf("第一个调用被污染: %+v", calls[0])
		}
		if calls[1].Id != "call_b" || calls[1].Arguments != `{"path":"b"}` {
			t.Fatalf("第二个调用被污染: %+v", calls[1])
		}
	})

	t.Run("首帧带 index、续帧只带 id → 一个调用、参数完整", func(t *testing.T) {
		calls, starts := run(t, []map[string]any{
			chunk(tc(map[string]any{"index": 1, "id": "call_a", "type": "function",
				"function": map[string]any{"name": "read_file", "arguments": `{"path":`}})),
			chunk(tc(map[string]any{"id": "call_a", "type": "function",
				"function": map[string]any{"arguments": `"a"}`}})),
			finish,
		})
		if len(calls) != 1 || starts != 1 {
			t.Fatalf("calls=%d starts=%d, want 1/1: %+v", len(calls), starts, calls)
		}
		if calls[0].Name != "read_file" || calls[0].Arguments != `{"path":"a"}` {
			t.Fatalf("参数/名字被截断: %+v", calls[0])
		}
	})

	t.Run("两个调用交叉交错（只有 id）→ 各自拼接", func(t *testing.T) {
		calls, _ := run(t, []map[string]any{
			chunk(tc(map[string]any{"id": "a", "type": "function",
				"function": map[string]any{"name": "f1", "arguments": `{"x":`}})),
			chunk(tc(map[string]any{"id": "b", "type": "function",
				"function": map[string]any{"name": "f2", "arguments": `{"y":`}})),
			chunk(tc(map[string]any{"id": "a", "type": "function",
				"function": map[string]any{"arguments": `1}`}})),
			chunk(tc(map[string]any{"id": "b", "type": "function",
				"function": map[string]any{"arguments": `2}`}})),
			finish,
		})
		if len(calls) != 2 {
			t.Fatalf("calls=%d, want 2: %+v", len(calls), calls)
		}
		got := map[string]string{}
		for _, c := range calls {
			got[c.Id] = c.Name + c.Arguments
		}
		if got["a"] != `f1{"x":1}` || got["b"] != `f2{"y":2}` {
			t.Fatalf("交错帧拼错: %+v", got)
		}
	})

	t.Run("规范端点（每帧都带 index）不回归", func(t *testing.T) {
		calls, starts := run(t, []map[string]any{
			chunk(tc(map[string]any{"index": 0, "id": "call_a", "type": "function",
				"function": map[string]any{"name": "read_file", "arguments": `{"path":`}})),
			chunk(tc(map[string]any{"index": 0, "type": "function",
				"function": map[string]any{"arguments": `"a"}`}})),
			finish,
		})
		if len(calls) != 1 || starts != 1 || calls[0].Arguments != `{"path":"a"}` {
			t.Fatalf("规范端点回归: calls=%d starts=%d %+v", len(calls), starts, calls)
		}
	})

	t.Run("两个身份都没有 → 视为同一调用的续帧（不每帧新建）", func(t *testing.T) {
		calls, starts := run(t, []map[string]any{
			chunk(tc(map[string]any{"type": "function",
				"function": map[string]any{"name": "read_file", "arguments": `{"path":`}})),
			chunk(tc(map[string]any{"type": "function",
				"function": map[string]any{"arguments": `"a"}`}})),
			finish,
		})
		if len(calls) != 1 || starts != 1 || calls[0].Arguments != `{"path":"a"}` {
			t.Fatalf("无身份帧应合并: calls=%d starts=%d %+v", len(calls), starts, calls)
		}
	})
}
