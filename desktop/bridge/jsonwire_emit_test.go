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
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/desktop/bridge/jsonwire"
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

// TestEmitTodoSnapshotExactlyOnce 盯的是**附加字段信封**（BR-03 的另一半）：
// 宿主附加字段（cost_usd/todos/diff）此前只注入不删，事件自带同名字段时「后者胜」
// 又把宿主的修正值盖回去。这条测试要求 stdout 行里顶层 `todos` 恰好一份。
//
// 为什么断言的是 `todos` 而不是 session_id：ToolResponse.Todos 没有 omitempty 之外的
// 任何保护 —— 事件自己 marshal 出来就带一份 `todos`（测试里先验这个前提），宿主那份
// 必须把它**删掉再注入**，否则同一行两份。结构上数得到 1 份，就说明 extras Envelope
// 的删除分支真的跑了；负控（只注入不删 → 2 份）钉住这个判断不是空真。
//
// 不可达分支（不伪造覆盖）：
//   · Envelope/ForceKeys 的失败回退：入参来自 json.Marshal（恒为合法对象、键名恒为
//     结构体 tag 的普通 ASCII），RemoveField 只会在「非对象/结构异常」时返回 nil；
//     emit 这条路上两者都不可能，故该回退**不可达**，没有 emit 级测试能覆盖它
//     （原语层由 jsonwire_test.go 的 TestEnvelope_Fallback 等覆盖）。
//   · 丢行分支（ForceKeys 也失败）：同上不可达。
func TestEmitTodoSnapshotExactlyOnce(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, buf := newManagerWithAppData(t, t.TempDir())
	bs, err := m.create(t.TempDir(), "s-br03-todos", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ts := time.Now().Add(-time.Second)
	buf.Reset()

	ev := &events.ToolResponse{
		RunId: "r-todos", Id: "tool-todos", Name: "todo_write",
		Todos: []events.TodoSnapshot{
			{Id: "t1", Title: "写测试", Done: false, BlockedBy: []string{"t0"}},
			{Id: "t2", Title: "收口", Done: true},
		},
		Timestamp: ts, EventType: events.ToolRunEndType,
	}
	// 前提①：事件自己 marshal 出来的行确实带一份顶层 todos（没有它就测不到「删」）。
	rawEv, err := json.Marshal(ev)
	if err != nil {
		t.Fatal(err)
	}
	if n := countTopLevelToken(t, rawEv, "todos"); n != 1 {
		t.Fatalf("前提不成立：事件自带 %d 份 todos（应为 1）", n)
	}
	// 前提②（负控）：只注入不删 —— 修复前的形态 —— 同一行会数到 2 份。
	onlyInject, err := jsonwire.InjectKeys(rawEv, []jsonwire.KV{{Key: "todos", Value: []byte(`[]`)}})
	if err != nil {
		t.Fatal(err)
	}
	if n := countTopLevelToken(t, onlyInject, "todos"); n != 2 {
		t.Fatalf("负控失败：只注入不删应数到 2，实际 %d", n)
	}

	bs.emit(ev)
	bs.out.flushNow()

	var line string
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.TrimSpace(l) != "" {
			line = l
		}
	}
	if line == "" {
		t.Fatalf("没有写出事件行:\n%s", buf.String())
	}
	var got map[string]any
	if err := json.Unmarshal([]byte(line), &got); err != nil {
		t.Fatalf("事件行不是合法 JSON: %v\n%s", err, line)
	}
	// 信封：身份键各一份（回归），附加字段也只有宿主那一份。
	if n := countTopLevelToken(t, []byte(line), "session_id"); n != 1 {
		t.Fatalf("顶层 session_id 应恰好 1 份，实际 %d: %s", n, line)
	}
	if n := countTopLevelToken(t, []byte(line), "todos"); n != 1 {
		t.Fatalf("顶层 todos 应恰好 1 份（删掉事件自带的、注入宿主的），实际 %d: %s", n, line)
	}
	// 值是宿主写的那份（todosOf 的 TodoItem 形状与顺序），不是事件里那份原始数组。
	var want []any
	if err := json.Unmarshal([]byte(`[{"id":"t1","title":"写测试","done":false,"blocked_by":["t0"]},{"id":"t2","title":"收口","done":true}]`), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got["todos"], want) {
		t.Fatalf("todos 不是宿主值: got %#v want %#v", got["todos"], want)
	}
	// 事件自己的字段不能因为删/注入而丢。
	if got["event_type"] != string(events.ToolRunEndType) || got["id"] != "tool-todos" || got["name"] != "todo_write" {
		t.Fatalf("事件字段丢失: %s", line)
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
