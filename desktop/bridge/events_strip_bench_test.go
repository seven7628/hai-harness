package main

// stripTopLevelField 的成本基准（2026-11-19，爬 2 前置）。
//
// 为什么要有这条基准：落盘侧裁 `llm_end.tool_calls` 会在**每一条 llm_end** 上跑一次
// 顶层字段扫描（llm_end 占事件日志 27%，单行可达数 KB）⇒ 这是 bridge 的热路径。
// 基准的作用是把「这个裁剪值不值」变成可测的数字，而不是靠感觉。

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

func benchLLMEnd() []byte {
	e := &events.LLMEnd{
		RunId:     "b9e27edabfe9efaf",
		Content:   "这是一段模拟的模型输出正文，长度约两百字节，用来让基准贴近真实行的体量。" + "补一些长度让行更接近真实规模。",
		Reasoning: "思考内容，同样补长一点，让这一行接近真实 llm_end 的体量（真实常见 1~4KB）。",
		ToolCalls: []core.ToolCall{{Id: "call_00_x", Index: 0, Name: "bash", Arguments: `{"command":"cd /Users/admin/workspace/go-code && ls && git log --oneline -5"}`}},
		Model:     "deepseek/deepseek-v4.1-flash",
		RequestId: "chatcmpl-202611190703386837338908268d9d6j2RytN03",
		EventType: events.LLMEndType,
	}
	b, _ := json.Marshal(e)
	return b
}

func BenchmarkStripTopLevelField(b *testing.B) {
	line := benchLLMEnd()
	b.SetBytes(int64(len(line)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		out := stripTopLevelField(line, "tool_calls")
		if len(out) >= len(line) {
			b.Fatal("裁剪没生效")
		}
	}
}

func BenchmarkMarshalLLMEnd(b *testing.B) {
	e := &events.LLMEnd{
		RunId:     "b9e27edabfe9efaf",
		Content:   "正文",
		ToolCalls: []core.ToolCall{{Id: "c1", Name: "bash", Arguments: `{"command":"ls"}`}},
		EventType: events.LLMEndType,
	}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := json.Marshal(e); err != nil {
			b.Fatal(err)
		}
	}
}

// TestStripTopLevelFieldRemovesOnlyTarget：裁剪只动目标字段，其余字节保持。
func TestStripTopLevelFieldRemovesOnlyTarget(t *testing.T) {
	line := benchLLMEnd()
	out := stripTopLevelField(line, "tool_calls")
	var m map[string]json.RawMessage
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatalf("裁剪后不是合法 JSON: %v\n%s", err, out)
	}
	if _, ok := m["tool_calls"]; ok {
		t.Fatalf("tool_calls 还在: %s", out)
	}
	for _, k := range []string{"run_id", "content", "reasoning", "model", "request_id", "event_type"} {
		if _, ok := m[k]; !ok {
			t.Fatalf("裁剪误伤了字段 %q: %s", k, out)
		}
	}
	// 字段顺序保持（除被删的那个）
	origKeys := jsonKeysInOrder(t, line)
	newKeys := jsonKeysInOrder(t, out)
	if len(newKeys) != len(origKeys)-1 {
		t.Fatalf("键数不对: %d → %d（%v → %v）", len(origKeys), len(newKeys), origKeys, newKeys)
	}
	// 被删的键之外，其余键**顺序**一致
	filtered := make([]string, 0, len(origKeys))
	for _, k := range origKeys {
		if k != "tool_calls" {
			filtered = append(filtered, k)
		}
	}
	if len(filtered) != len(newKeys) {
		t.Fatalf("键集合大小不符")
	}
	for i := range filtered {
		if filtered[i] != newKeys[i] {
			t.Fatalf("键顺序变了: %v vs %v", filtered, newKeys)
		}
	}
}

// jsonKeysInOrder 按 JSON 原文顺序取顶层键名。
func jsonKeysInOrder(t *testing.T, raw []byte) []string {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(raw))
	tok, err := dec.Token()
	if err != nil {
		t.Fatalf("取首个 token 失败: %v", err)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		t.Fatalf("不是 JSON 对象: %v", tok)
	}
	var keys []string
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			t.Fatalf("取键失败: %v", err)
		}
		k, _ := kt.(string)
		keys = append(keys, k)
		// 跳过值
		var v json.RawMessage
		if err := dec.Decode(&v); err != nil {
			t.Fatalf("跳值失败: %v", err)
		}
	}
	return keys
}

var _ = os.Getenv
