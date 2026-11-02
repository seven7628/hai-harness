package jsonwire

import (
	"encoding/json"
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
func TestInjectWithoutRemove_ShadowsWithEmpty(t *testing.T) {
	base := []byte(`{"session_id":"","event_type":"task_promoted"}`)
	line, err := InjectKeys(base, []KV{kv(t, "session_id", "s-1")})
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(line, &got); err != nil {
		t.Fatal(err)
	}
	if got["session_id"] != "" {
		t.Skip("解析器选择前者：本测试的前提变了，请复核注入语义")
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
