package main

// BR-03 的**端到端**锁：不测 jsonwire 原语，而是走真实 emit 漏斗（marshal → Envelope →
// stdout），断言每一行顶层 session_id/workspace 恰好一份且是宿主值。
//
// 为什么原语测试不够：事故发生在「删」与「注入」之间——原语都对，调用方漏一步或
// RemoveField 返回 nil 时静默跳过，重复键就上线了。这条测试盯的是**产物**（stdout 行）。

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/events"
)

func TestEmitEnvelopeSessionIDExactlyOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, buf := newManagerWithAppData(t, t.TempDir())
	bs, err := m.create(t.TempDir(), "s-br03", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ts := time.Now().Add(-time.Second)
	buf.Reset()

	// ① 事故原样：TaskPromoted 结构自带 `session_id:""`（无 omitempty）。
	bs.emit(&events.TaskPromoted{
		SessionId: "", TaskId: "tooltask-br03", Kind: "tool", Name: "bash",
		Timestamp: ts, EventType: events.TaskPromotedType,
	})
	// ② 值里嵌一段「像键一样的 JSON 字符串」：扫描器不能把它当顶层键删掉/复制。
	bs.emit(&events.ToolResponse{
		RunId: "r-br03", Id: "tool-br03", Name: "bash",
		Result: `{"session_id":"","workspace":""}`, Timestamp: ts,
		EventType: events.ToolRunEndType,
	})
	bs.out.flushNow()

	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) < 2 {
		t.Fatalf("应有 2 行事件，实际 %d:\n%s", len(lines), buf.String())
	}
	sawPromoted, sawTool := false, false
	// 负控：计数器必须能看见重复键，否则下面的断言是空真。
	if n := countTopLevelToken(t, []byte(`{"session_id":"a","x":1,"session_id":"b"}`), "session_id"); n != 2 {
		t.Fatalf("负控失败：重复键应数到 2，实际 %d", n)
	}
	if n := countTopLevelToken(t, []byte(`{"a":{"session_id":"in"},"session_id":"out"}`), "session_id"); n != 1 {
		t.Fatalf("负控失败：嵌套同名键不应计入顶层，实际 %d", n)
	}
	for _, line := range lines {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var got map[string]any
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Fatalf("事件行不是合法 JSON: %v\n%s", err, line)
		}
		if n := countTopLevelToken(t, []byte(line), "session_id"); n != 1 {
			t.Fatalf("顶层 session_id 应恰好 1 份，实际 %d: %s", n, line)
		}
		if n := countTopLevelToken(t, []byte(line), "workspace"); n != 1 {
			t.Fatalf("顶层 workspace 应恰好 1 份，实际 %d: %s", n, line)
		}
		if got["session_id"] != bs.id {
			t.Fatalf("session_id = %v, want 宿主 %q（事件自带的空串盖掉了注入）: %s", got["session_id"], bs.id, line)
		}
		switch got["event_type"] {
		case string(events.TaskPromotedType):
			sawPromoted = true
			if got["task_id"] != "tooltask-br03" || got["kind"] != "tool" || got["name"] != "bash" {
				t.Fatalf("TaskPromoted 字段丢失: %s", line)
			}
		case string(events.ToolRunEndType):
			sawTool = true
			if got["result"] != `{"session_id":"","workspace":""}` {
				t.Fatalf("字符串值里的 JSON 被改写: %v", got["result"])
			}
		}
	}
	if !sawPromoted || !sawTool {
		t.Fatalf("缺少预期事件行（promoted=%v tool=%v）:\n%s", sawPromoted, sawTool, buf.String())
	}
}

// countTopLevelToken 用 json.Decoder 的 token 流数顶层键（重复键在 token 流里仍可见，
// 而 Unmarshal 进 map 会合并掉）。嵌套对象/数组里的同名键不计。
func countTopLevelToken(t *testing.T, line []byte, key string) int {
	t.Helper()
	dec := json.NewDecoder(bytes.NewReader(line))
	first, err := dec.Token()
	if err != nil || first != json.Delim('{') {
		t.Fatalf("不是 JSON 对象: %v (%s)", err, line)
	}
	n := 0
	depth := 0
	expectKey := true
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("token 解析失败: %v (%s)", err, line)
		}
		if d, ok := tok.(json.Delim); ok {
			switch d {
			case '{', '[':
				if depth > 0 {
					depth++
				} else if depth == 0 {
					depth = 1 // 顶层值（对象/数组）开始：里面都是值，不数键
				}
			case '}', ']':
				if depth > 1 {
					depth--
				} else if depth == 1 {
					depth = 0
					expectKey = true // 顶层值结束，下一个 token 又是键
				}
			}
			continue
		}
		if depth == 0 {
			if expectKey {
				if s, ok := tok.(string); ok && s == key {
					n++
				}
				expectKey = false
			} else {
				expectKey = true // 标量值结束
			}
		}
	}
	return n
}
