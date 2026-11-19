package main

// 「落盘侧裁 `llm_end.tool_calls`」的**等价性测试**（2026-11-19，爬 2）。
//
// ## 要证明什么
//
// 爬 2 在**落盘**那一侧裁掉 `llm_end.tool_calls`（占事件日志 9.4%，零消费方）。
// 这条测试证明：**裁掉它之后，从事件重建出来的 checkpoint 逐字节相同**。
//
//	路 A（旧）：事件原样 → reducer → checkpoint
//	路 B（新）：事件裁掉 llm_end.tool_calls → reducer → checkpoint
//
// ⇒ checkpoint 相同 ⇒ **会话恢复 / legacy 迁移重放 / UI 视图全都不受影响**。
//
// ## 为什么这条特别重要
//
// 事件文件是 legacy 会话（无 record）的**唯一重建来源**
// （`migrateLegacyToCheckpoint`：读 events.jsonl 重放进 reducer 生成 checkpoint）。
// ⇒ 一旦裁掉的字段 reducer 其实要用，**老会话会静默丢数据**（而且是恢复时才暴露）。
// 这条测试把那个「静默」变成「当场红」。
//
// ## 两种数据源
//
//  1. **合成事件**（默认，CI 覆盖）：手工构造一批含 tool_calls 的事件，两条路重放比对。
//  2. **真实会话数据**（环境变量门控，不进常规 CI）：
//
//	cp -c ~/.go-code/events/<ws>/<sid>.jsonl /tmp/lab/
//	HAI_LAB_EVENTS=/tmp/lab/<sid>.jsonl \
//	go test ./desktop/bridge/ -run TestEventsToolCallsEquivalence -v -timeout 30m

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// stripLLMEndToolCalls 模拟生产落盘侧的裁剪（main.go 的 logEventLine）。
func stripLLMEndToolCalls(line []byte) []byte {
	if strings.Contains(string(line), `"event_type":"llm_end"`) {
		return stripTopLevelField(line, "tool_calls")
	}
	return line
}

// replayToCheckpoint 把一批事件行重放进 reducer，返回 checkpoint 的 body 字节。
func replayToCheckpoint(t *testing.T, lines [][]byte) []byte {
	t.Helper()
	red := NewViewReducer()
	for i, ln := range lines {
		ev, err := parseLegacyEvent(string(ln))
		if err != nil || ev == nil {
			continue
		}
		if err := red.Apply(ev, EventMeta{}); err != nil {
			t.Fatalf("第 %d 行 Apply 失败: %v", i, err)
		}
	}
	red.NormalizeForRestore()
	bs, err := red.MarshalBodyWithSpans()
	if err != nil {
		t.Fatalf("MarshalBodyWithSpans 失败: %v", err)
	}
	return bs.Body
}

// syntheticEventLines 构造一批含 llm_end.tool_calls 的事件行（JSON 文本）。
func syntheticEventLines(t *testing.T) [][]byte {
	t.Helper()
	fixed := time.Unix(1700000000, 0).UTC()
	evs := []events.Event{
		&events.AgentStart{
			RunId: "run-1", Name: "主", EventType: events.AgentStartType,
			Timestamp: fixed,
		},
		&events.UserInputsConsumed{
			UserTexts: []string{"问题"}, EventType: events.UserInputsConsumedType,
			Timestamp: fixed,
		},
		&events.LLMStart{
			RunId: "run-1", Model: "m1", RequestId: "req-1", EventType: events.LLMStartType,
			Timestamp: fixed,
		},
		&events.LLMEnd{
			RunId: "run-1", Content: "回答正文", Reasoning: "思考",
			ToolCalls: []core.ToolCall{{
				Id: "call-1", Index: 0, Name: "bash",
				Arguments: `{"command":"ls -la"}`,
			}},
			FinishReason: core.FinishReasonToolCall,
			Model:        "m1", RequestId: "req-1", EventType: events.LLMEndType,
			Timestamp: fixed,
		},
		&events.ToolStart{
			RequestId: "req-1", Index: 0, Id: "call-1", Name: "bash",
			RunId: "run-1", Arguments: `{"command":"ls -la"}`,
			TaskId: "tooltask-abc", EventType: events.ToolRunStartType,
			Timestamp: fixed,
		},
		&events.ToolResponse{
			RequestId: "req-1", Index: 0, Id: "call-1", Name: "bash",
			RunId: "run-1", Arguments: `{"command":"ls -la"}`,
			Result: "文件列表", EventType: events.ToolRunEndType,
			Timestamp: fixed,
		},
		&events.LLMEnd{
			RunId: "run-1", Content: "收尾", FinishReason: core.FinishReasonStop,
			Model: "m1", RequestId: "req-2", EventType: events.LLMEndType,
			Timestamp: fixed,
		},
		&events.AgentEnd{
			RunId: "run-1", FinishReason: core.FinishReasonStop,
			EventType: events.AgentEndType,
			Timestamp: fixed,
		},
	}
	lines := make([][]byte, 0, len(evs))
	for _, e := range evs {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		lines = append(lines, b)
	}
	return lines
}

// TestEventsToolCallsEquivalenceSynthetic：合成事件 ⇒ 裁剪前后 checkpoint 字节相同。
func TestEventsToolCallsEquivalenceSynthetic(t *testing.T) {
	lines := syntheticEventLines(t)

	// 先确认前提：确实有 llm_end 带 tool_calls
	hasTC := false
	for _, ln := range lines {
		if strings.Contains(string(ln), `"event_type":"llm_end"`) && strings.Contains(string(ln), `"tool_calls"`) {
			hasTC = true
		}
	}
	if !hasTC {
		t.Fatal("测试前提不成立：没有带 tool_calls 的 llm_end 行")
	}

	bodyA := replayToCheckpoint(t, lines)

	stripped := make([][]byte, len(lines))
	for i, ln := range lines {
		stripped[i] = stripLLMEndToolCalls(ln)
	}
	bodyB := replayToCheckpoint(t, stripped)

	if string(bodyA) != string(bodyB) {
		t.Fatalf("裁掉 llm_end.tool_calls 后 checkpoint 变了：\n 前 %s\n 后 %s", bodyA, bodyB)
	}
	if len(bodyA) == 0 {
		t.Fatal("checkpoint body 为空（测试没真跑到 reducer）")
	}
	t.Logf("✅ 合成事件：裁剪前后 checkpoint 字节相同（%d 字节）", len(bodyA))
}

// TestEventsToolCallsEquivalenceReal：真实会话数据 ⇒ 裁剪前后 checkpoint 字节相同。
//
// 需要 HAI_LAB_EVENTS=<events.jsonl 路径>。
func TestEventsToolCallsEquivalenceReal(t *testing.T) {
	path := os.Getenv("HAI_LAB_EVENTS")
	if path == "" {
		t.Skip("需要 HAI_LAB_EVENTS=<sid>.jsonl")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读事件文件失败: %v", err)
	}
	var lines [][]byte
	for _, ln := range strings.Split(string(raw), "\n") {
		ln = strings.TrimSpace(ln)
		if ln == "" {
			continue
		}
		lines = append(lines, []byte(ln))
	}
	if len(lines) == 0 {
		t.Fatal("事件文件为空")
	}

	bodyA := normalizeVolatileTS(replayToCheckpoint(t, lines))
	stripped := make([][]byte, len(lines))
	n := 0
	for i, ln := range lines {
		stripped[i] = stripLLMEndToolCalls(ln)
		if string(stripped[i]) != string(ln) {
			n++
		}
	}
	bodyB := normalizeVolatileTS(replayToCheckpoint(t, stripped))

	if string(bodyA) != string(bodyB) {
		t.Fatalf("真实数据：裁掉 llm_end.tool_calls 后 checkpoint 变了（前 %d 字节 / 后 %d 字节）%s",
			len(bodyA), len(bodyB), firstByteDiff(bodyA, bodyB))
	}
	t.Logf("✅ 真实数据 %s：%d 行（其中 %d 行被裁）⇒ checkpoint 字节相同（%d 字节）",
		path, len(lines), n, len(bodyA))
}

var _ = context.Background

// firstDiff 报告两段字节的第一处差异（带上下文），用于失败诊断。
func firstByteDiff(a, b []byte) string {
	n := len(a)
	if len(b) < n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		if a[i] != b[i] {
			lo := i - 60
			if lo < 0 {
				lo = 0
			}
			hi := i + 60
			if hi > n {
				hi = n
			}
			return fmt.Sprintf("\n  差异在第 %d 字节：\n   前 …%s…\n   后 …%s…", i, a[lo:hi], b[lo:hi])
		}
	}
	if len(a) != len(b) {
		return fmt.Sprintf("\n  前缀相同，长度不同（%d vs %d）", len(a), len(b))
	}
	return ""
}

// normalizeVolatileTS 把「13 位连续数字」（unix 毫秒）归一化成固定占位。
//
// 为什么需要：两次重放发生在不同时刻 ⇒ reducer 对**没有 Timestamp 的事件**
// 回退 `nowMillis()`（如 error 块的 at）⇒ checkpoint 里会有毫秒级差异。
// 那不是本测试要验的东西（我们验的是「裁掉 tool_calls 不改变 checkpoint」），
// 所以先把易变时间戳抹平再比。
//
// 不会掩盖真差异：被裁掉的 tool_calls 若是纯数字串，抹平后两侧的
// **JSON 结构**（引号/逗号/括号）仍然不同 ⇒ 照样会被 firstByteDiff 抓到。
func normalizeVolatileTS(b []byte) []byte {
	out := make([]byte, 0, len(b))
	for i := 0; i < len(b); {
		if b[i] >= '0' && b[i] <= '9' {
			j := i
			for j < len(b) && b[j] >= '0' && b[j] <= '9' {
				j++
			}
			if j-i == 13 {
				out = append(out, "0000000000000"...)
			} else {
				out = append(out, b[i:j]...)
			}
			i = j
			continue
		}
		out = append(out, b[i])
		i++
	}
	return out
}
