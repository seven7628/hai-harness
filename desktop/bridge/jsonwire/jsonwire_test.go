package jsonwire

import (
	"encoding/json"
	"strings"
	"testing"
)

func kv(t *testing.T, key, val string) KV {
	t.Helper()
	b, err := json.Marshal(val)
	if err != nil {
		t.Fatalf("marshal %q: %v", val, err)
	}
	return KV{Key: key, Value: b}
}

// 事故回归（用户 2026-11）：events.TaskPromoted 自带 `"session_id":""`（无 omitempty），
// 注入是前置的，JSON 重复键**后者胜** → 后端读到空串 → 那一行被回退路由到「当前打开的
// 会话」，cybertron-gogroup 的「已摘为后台任务」出现在 agent-server-g 的转录里。
// 修法：先把事件自带的同名键删掉，再注入宿主的唯一一份。
func TestInjectAfterRemove_TaskPromotedCarriesEmptySessionID(t *testing.T) {
	// 即事件真实的 marshal 结果：session_id 为空串且排在前面
	base := []byte(`{"session_id":"","task_id":"tooltask-634fk7um","kind":"tool","name":"bash","event_type":"task_promoted"}`)

	if stripped := RemoveField(base, "session_id"); stripped != nil {
		base = stripped
	}
	line, err := InjectKeys(base, []KV{
		kv(t, "session_id", "s-1792730373441010000-2"),
		kv(t, "workspace", "/Users/admin/cybertron-gogroup"),
	})
	if err != nil {
		t.Fatal(err)
	}

	var got map[string]any
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("注入后不是合法 JSON: %v\n%s", err, line)
	}
	if got["session_id"] != "s-1792730373441010000-2" {
		t.Fatalf("session_id 被事件自带的空串盖掉了: %v\n%s", got["session_id"], line)
	}
	if got["workspace"] != "/Users/admin/cybertron-gogroup" {
		t.Fatalf("workspace 不对: %v", got["workspace"])
	}
	// 事件自己的字段一个都不能丢
	for k, want := range map[string]string{"task_id": "tooltask-634fk7um", "kind": "tool", "name": "bash", "event_type": "task_promoted"} {
		if got[k] != want {
			t.Fatalf("字段 %s 丢了或变了: %v", k, got[k])
		}
	}
	if n := countKey(line, "session_id"); n != 1 {
		t.Fatalf("session_id 应只出现一次，实际 %d 次: %s", n, line)
	}
}

// 反向：不先删就直接注入 —— 这正是修复前的行为，测试把它钉住（后者胜 → 空串）。
// 断言不依赖「解析器挑哪一份」：结构上顶层确实有两份（宿主在前、事件自带的空串在后），
// 语义上 encoding/json 取后者 → 空串胜出，前端随后回退「当前会话」，正是串台根因。
// 修复后的形态由 Envelope（删+注入一次完成）与 emit 级测试锁住，这条只负责说明为什么。
func TestInjectWithoutRemove_ShadowsWithEmpty(t *testing.T) {
	base := []byte(`{"session_id":"","event_type":"task_promoted"}`)
	line, err := InjectKeys(base, []KV{kv(t, "session_id", "s-1")})
	if err != nil {
		t.Fatal(err)
	}
	if n := countKey(line, "session_id"); n != 2 {
		t.Fatalf("只注入不删 → 顶层应有两份 session_id，实际 %d: %s", n, line)
	}
	var got map[string]any
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatal(err)
	}
	if got["session_id"] != "" {
		t.Fatalf("重复键应让事件自带的空串胜出（后者胜），实际 %v: %s", got["session_id"], line)
	}
}

// 事件不带同名键时（绝大多数事件）注入后只有一个键、且顺序在首部。
func TestInjectKeys_NoConflict(t *testing.T) {
	base := []byte(`{"event_type":"tool_run_start","run_id":"r1"}`)
	line, err := InjectKeys(base, []KV{kv(t, "session_id", "s-9"), kv(t, "workspace", "/w")})
	if err != nil {
		t.Fatal(err)
	}
	if string(line) != `{"session_id":"s-9","workspace":"/w","event_type":"tool_run_start","run_id":"r1"}` {
		t.Fatalf("注入结果不对: %s", line)
	}
}

// 宿主字段也不能和「事件自带的同名键」共存：删完之后注入，只留一份（workspace 同理）。
func TestRemoveThenInject_WorkspaceAlsoDeduped(t *testing.T) {
	base := []byte(`{"workspace":"","session_id":"","event_type":"x"}`)
	for _, k := range []string{"session_id", "workspace"} {
		if s := RemoveField(base, k); s != nil {
			base = s
		}
	}
	line, err := InjectKeys(base, []KV{kv(t, "session_id", "s-2"), kv(t, "workspace", "/ws")})
	if err != nil {
		t.Fatal(err)
	}
	if countKey(line, "workspace") != 1 || countKey(line, "session_id") != 1 {
		t.Fatalf("应有且各有一份: %s", line)
	}
}

// RemoveField 的既有语义（原来在 snapshot_test.go，一并搬来）：删掉**所有**同名出现、
// 保留其余字段、非对象返回 nil、键不存在时原样返回。
func TestRemoveField(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"单次出现", `{"session_id":"a","x":1}`, `{"x":1}`},
		{"重复出现都删", `{"session_id":"a","x":1,"session_id":""}`, `{"x":1}`},
		{"嵌套同名键不动", `{"a":{"session_id":"in"},"session_id":"out"}`, `{"a":{"session_id":"in"}}`},
		{"值里的转义引号", `{"session_id":"a\"b","x":2}`, `{"x":2}`},
		{"键不存在", `{"x":1}`, `{"x":1}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := RemoveField([]byte(c.in), "session_id")
			if got == nil {
				t.Fatalf("RemoveField(%s) 返回 nil", c.in)
			}
			var a, b any
			if err := json.Unmarshal(got, &a); err != nil {
				t.Fatalf("结果不是合法 JSON: %v (%s)", err, got)
			}
			if err := json.Unmarshal([]byte(c.want), &b); err != nil {
				t.Fatal(err)
			}
			if !deepEqual(a, b) {
				t.Fatalf("got %s want %s", got, c.want)
			}
		})
	}
	if got := RemoveField([]byte(`[1,2]`), "session_id"); got != nil {
		t.Fatalf("非对象应返回 nil, got %s", got)
	}
}

func countKey(line []byte, key string) int {
	n := 0
	pat := []byte(`"` + key + `":`)
	for i := 0; i+len(pat) <= len(line); i++ {
		if string(line[i:i+len(pat)]) == string(pat) {
			n++
		}
	}
	return n
}

func deepEqual(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

// —— Envelope / ForceKeys：宿主键的结构化保证（BR-03 收口） ——
//
// 这组测试锁的是**不变式**而不是某条事故的样子：无论事件自带什么（重复键 / 空串 /
// 缺字段 / 嵌套对象 / 值里嵌一段 JSON 字符串），输出行顶层 `session_id` 恰好一份，
// 且等于宿主值。

func TestEnvelope_TopLevelExactlyOnce(t *testing.T) {
	host := []KV{kv(t, "session_id", "s-host"), kv(t, "workspace", "/ws")}
	cases := []struct{ name, in string }{
		{"事件自带空串（TaskPromoted）", `{"session_id":"","event_type":"task_promoted","task_id":"t1"}`},
		{"同名键重复出现", `{"session_id":"","x":1,"session_id":"last"}`},
		{"缺字段", `{"event_type":"tool_run_start","run_id":"r1"}`},
		{"嵌套对象里的同名键不动", `{"a":{"session_id":"inner"},"session_id":""}`},
		{"值里嵌一段 JSON 字符串", `{"result":"{\"session_id\":\"\"}","session_id":""}`},
		{"值里有转义引号", `{"session_id":"","result":"he said \"session_id\": \"\"","workspace":""}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			line, err := Envelope([]byte(c.in), host)
			if err != nil {
				t.Fatalf("Envelope: %v", err)
			}
			var got map[string]any
			if err := json.Unmarshal(line, &got); err != nil {
				t.Fatalf("输出不是合法 JSON: %v\n%s", err, line)
			}
			if got["session_id"] != "s-host" {
				t.Fatalf("session_id = %v, want 宿主值（重复键后者胜会拿到事件自带的空串）: %s", got["session_id"], line)
			}
			if got["workspace"] != "/ws" {
				t.Fatalf("workspace = %v, want /ws: %s", got["workspace"], line)
			}
			// 顶层出现次数：用 Envelope 的构造性质 + 全程扫描兜底。
			if n := countTopLevelKey(t, line, "session_id"); n != 1 {
				t.Fatalf("顶层 session_id 应恰好 1 份，实际 %d: %s", n, line)
			}
			if n := countTopLevelKey(t, line, "workspace"); n != 1 {
				t.Fatalf("顶层 workspace 应恰好 1 份，实际 %d: %s", n, line)
			}
			// 事件自己的字段一个都不能丢。
			if c.name == "事件自带空串（TaskPromoted）" {
				if got["event_type"] != "task_promoted" || got["task_id"] != "t1" {
					t.Fatalf("事件字段丢失: %s", line)
				}
			}
			// 嵌套/字符串里的同名键必须原样保留（扫描器不能把它们当顶层键删掉）。
			switch c.name {
			case "嵌套对象里的同名键不动":
				a, _ := got["a"].(map[string]any)
				if a == nil || a["session_id"] != "inner" {
					t.Fatalf("嵌套对象被破坏: %s", line)
				}
			case "值里嵌一段 JSON 字符串":
				if got["result"] != `{"session_id":""}` {
					t.Fatalf("字符串值被改写: %v", got["result"])
				}
			}
		})
	}
}

func TestEnvelope_NonObjectReturnsError(t *testing.T) {
	for _, in := range []string{`[1,2]`, `"just a string"`} {
		if _, err := Envelope([]byte(in), []KV{kv(t, "session_id", "s")}); err == nil {
			t.Fatalf("Envelope(%s) 应报错（结构异常不能静默带着重复键出门）", in)
		}
	}
}

func TestForceKeys_HostWinsRegardlessOfDuplicates(t *testing.T) {
	line, err := ForceKeys(
		[]byte(`{"session_id":"","workspace":"","x":1,"session_id":"last"}`),
		[]KV{kv(t, "session_id", "s-host"), kv(t, "workspace", "/ws")},
	)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatalf("输出不是合法 JSON: %v\n%s", err, line)
	}
	if got["session_id"] != "s-host" || got["workspace"] != "/ws" || got["x"] != float64(1) {
		t.Fatalf("ForceKeys 结果不对: %s", line)
	}
	if n := countTopLevelKey(t, line, "session_id"); n != 1 {
		t.Fatalf("顶层 session_id 应恰好 1 份，实际 %d: %s", n, line)
	}
}

func TestForceKeys_RejectsNonObject(t *testing.T) {
	for _, in := range []string{`[1,2]`, `null`, `{"a":`} {
		if _, err := ForceKeys([]byte(in), []KV{kv(t, "session_id", "s")}); err == nil {
			t.Fatalf("ForceKeys(%s) 应报错", in)
		}
	}
}

// countTopLevelKey 只数**顶层**对象的键（嵌套同名键不算）：
// 逐字节走一遍对象的键值对，跳过每个值的内部结构（含字符串转义）。
func countTopLevelKey(t *testing.T, line []byte, key string) int {
	t.Helper()
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(line, &obj); err != nil {
		t.Fatalf("countTopLevelKey: %v", err)
	}
	// map 解析后重复键已合并，数不出来；改用原始字节扫描顶层（复用生产扫描器的跳值逻辑）。
	return countTopLevelKeyRaw(string(line), key)
}

// countTopLevelKeyRaw 只在顶层键值对上匹配，跳过每个值内部（对象/数组/字符串转义），
// 因此嵌套对象里的同名键不会被误数。
func countTopLevelKeyRaw(s, key string) int {
	trimmed := strings.TrimSpace(s)
	if !strings.HasPrefix(trimmed, "{") {
		return 0
	}
	n := 0
	i := strings.IndexByte(trimmed, '{') + 1
	for i < len(trimmed) {
		for i < len(trimmed) && (trimmed[i] == ' ' || trimmed[i] == '\t' || trimmed[i] == '\n' || trimmed[i] == '\r' || trimmed[i] == ',') {
			i++
		}
		if i >= len(trimmed) || trimmed[i] == '}' {
			break
		}
		if trimmed[i] != '"' {
			return n
		}
		keyStart := i
		i++
		for i < len(trimmed) && trimmed[i] != '"' {
			if trimmed[i] == '\\' {
				i++
			}
			i++
		}
		i++ // 闭合引号
		keyEnd := i
		for i < len(trimmed) && (trimmed[i] == ' ' || trimmed[i] == '\t' || trimmed[i] == '\n' || trimmed[i] == '\r') {
			i++
		}
		if i >= len(trimmed) || trimmed[i] != ':' {
			return n
		}
		i++
		for i < len(trimmed) && (trimmed[i] == ' ' || trimmed[i] == '\t' || trimmed[i] == '\n' || trimmed[i] == '\r') {
			i++
		}
		end, ok := skipValue(trimmed, i)
		if !ok {
			return n
		}
		if trimmed[keyStart:keyEnd] == `"`+key+`"` {
			n++
		}
		i = end
	}
	return n
}
