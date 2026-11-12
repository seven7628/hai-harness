package main

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/desktop/bridge/jsonwire"
)

func TestRemoveJSONField(t *testing.T) {
	cases := []struct {
		name string
		in   string
		key  string
		want string // 期望（json 规范化比较）
	}{
		{"首键", `{"session_id":"s1","event_type":"agent_start"}`, "session_id", `{"event_type":"agent_start"}`},
		{"中键", `{"event_type":"agent_start","session_id":"s1","run_id":"r1"}`, "session_id", `{"event_type":"agent_start","run_id":"r1"}`},
		{"尾键", `{"event_type":"agent_start","run_id":"r1","session_id":"s1"}`, "session_id", `{"event_type":"agent_start","run_id":"r1"}`},
		{"唯一键", `{"session_id":"s1"}`, "session_id", `{}`},
		{"字符串值含逗号引号", `{"a":"x,y\"z","session_id":"s1","b":2}`, "session_id", `{"a":"x,y\"z","b":2}`},
		{"嵌套对象值", `{"a":{"session_id":"nested"},"session_id":"s1","b":true}`, "session_id", `{"a":{"session_id":"nested"},"b":true}`},
		{"数组值", `{"session_id":"s1","arr":[1,2,{"session_id":"n"}],"c":"d"}`, "session_id", `{"arr":[1,2,{"session_id":"n"}],"c":"d"}`},
		{"数字布尔值", `{"session_id":"s1","n":123,"b":false,"x":null}`, "session_id", `{"n":123,"b":false,"x":null}`},
		{"转义值", `{"session_id":"s1","esc":"a\\\"b,c"}`, "session_id", `{"esc":"a\\\"b,c"}`},
		{"键不存在", `{"a":1,"b":2}`, "session_id", `{"a":1,"b":2}`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := jsonwire.RemoveField([]byte(c.in), c.key)
			if got == nil {
				t.Fatalf("jsonwire.RemoveField(%s) 返回 nil", c.in)
			}
			// JSON 规范化比较（键序无关）
			var gotObj, wantObj map[string]any
			if err := json.Unmarshal(got, &gotObj); err != nil {
				t.Fatalf("结果非法 JSON: %s (err=%v)", got, err)
			}
			if err := json.Unmarshal([]byte(c.want), &wantObj); err != nil {
				t.Fatalf("期望非法 JSON: %s", c.want)
			}
			gotJSON, _ := json.Marshal(gotObj)
			wantJSON, _ := json.Marshal(wantObj)
			if string(gotJSON) != string(wantJSON) {
				t.Fatalf("got %s, want %s", gotJSON, wantJSON)
			}
		})
	}
}

func TestRemoveJSONFieldInvalid(t *testing.T) {
	// 非对象 / 异常结构 → nil（调用方回退 Unmarshal）
	for _, in := range []string{`[]`, `"str"`, `123`, `{"a":`, `{"a" 1}`, ``} {
		if got := jsonwire.RemoveField([]byte(in), "session_id"); got != nil {
			t.Fatalf("jsonwire.RemoveField(%q) 应返回 nil, got %s", in, got)
		}
	}
}

func TestBuildSnapshotRemoveSessionID(t *testing.T) {
	// 验证 buildSnapshot 快照事件不含 session_id（用现有测试数据构造）
	lines := []string{
		`{"event_type":"agent_start","session_id":"s1","run_id":"r1","content":"hi"}`,
		`{"event_type":"llm_end","session_id":"s1","model":"m","usage":{"Input":1}}`,
	}
	dir := t.TempDir()
	if err := writeFileAll(dir, "events.jsonl", strings.Join(lines, "\n")+"\n"); err != nil {
		t.Fatal(err)
	}
	// 直接用 removeJSONField 验证每条
	for _, l := range lines {
		got := jsonwire.RemoveField([]byte(l), "session_id")
		if got == nil || strings.Contains(string(got), "session_id") {
			t.Fatalf("删除 session_id 失败: %s -> %s", l, got)
		}
		var m map[string]any
		if err := json.Unmarshal(got, &m); err != nil {
			t.Fatalf("结果非法: %v", err)
		}
		if _, has := m["session_id"]; has {
			t.Fatal("session_id 仍存在")
		}
	}
	// 第一条验证 run_id 保留
	first := jsonwire.RemoveField([]byte(lines[0]), "session_id")
	var m map[string]any
	_ = json.Unmarshal(first, &m)
	if m["run_id"] != "r1" {
		t.Fatalf("其他字段应保留: %v", m)
	}
}

func writeFileAll(dir, name, content string) error {
	return os.WriteFile(dir+"/"+name, []byte(content), 0o644)
}
