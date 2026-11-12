package main

// 事件日志字段裁剪的回归（2026-11-11）。
//
// 这是「唯一事实来源」上的手术：裁错就是丢事件。所以每条规则都要钉住 ——
// 尤其是**嵌套同名键不能被误删**、**字符串里出现的字段名不能被误删**、
// **裁剪后必须仍是合法 JSON**。

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestStripTopLevelField(t *testing.T) {
	cases := []struct {
		name string
		in   string
		key  string
		want string
	}{
		{
			name: "去掉中间的数组字段",
			in:   `{"event_type":"agent_end","run_id":"r1","tool_calls":[{"id":"c1","name":"bash","arguments":"{\"cmd\":\"ls\"}"}],"usage":{"input":10}}`,
			key:  "tool_calls",
			want: `{"event_type":"agent_end","run_id":"r1","usage":{"input":10}}`,
		},
		{
			name: "去掉最后一个字段",
			in:   `{"a":1,"tool_calls":[1,2]}`,
			key:  "tool_calls",
			want: `{"a":1}`,
		},
		{
			name: "去掉第一个字段",
			in:   `{"tool_calls":[1,2],"a":1}`,
			key:  "tool_calls",
			want: `{"a":1}`,
		},
		{
			name: "去掉字符串字段",
			in:   `{"a":1,"arguments":"{\"x\":1}","b":2}`,
			key:  "arguments",
			want: `{"a":1,"b":2}`,
		},
		{
			name: "嵌套同名键不能被删",
			in:   `{"outer":{"tool_calls":[1]},"tool_calls":[2]}`,
			key:  "tool_calls",
			want: `{"outer":{"tool_calls":[1]}}`,
		},
		{
			name: "字符串值里的字段名不能被删",
			in:   `{"text":"看这个 {\"tool_calls\":[9]} 字段","tool_calls":[2]}`,
			key:  "tool_calls",
			want: `{"text":"看这个 {\"tool_calls\":[9]} 字段"}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stripTopLevelField([]byte(c.in), c.key)
			if string(got) != c.want {
				t.Fatalf("得到 %s\n期望 %s", got, c.want)
			}
			// 裁剪后必须仍是合法 JSON
			var v map[string]any
			if err := json.Unmarshal(got, &v); err != nil {
				t.Fatalf("裁剪后不是合法 JSON: %v (%s)", err, got)
			}
		})
	}
}

func TestStripTopLevelFieldNoop(t *testing.T) {
	cases := []string{
		`{"a":1}`,
		`{"a":"tool_calls"}`,
		`[1,2,3]`,
		`not json at all`,
		`{"a":1,"b":{"c":[1,2]}`,
	}
	for _, in := range cases {
		got := stripTopLevelField([]byte(in), "tool_calls")
		if string(got) != in {
			t.Fatalf("不该改动：%s → %s", in, got)
		}
	}
}

// TestStripAgentEndToolCalls 真实形态的 agent_end 事件裁剪后仍可用。
func TestStripAgentEndToolCalls(t *testing.T) {
	line := []byte(`{"event_type":"agent_end","run_id":"r1","index":3,"content":"做完了","reasoning":"` +
		strings.Repeat("思", 500) + `","tool_calls":[{"id":"call-1","name":"bash","arguments":"{\"command\":\"ls -la\"}","result":"ok"}],` +
		`"finish_reason":"stop","usage":{"input":100,"output":50},"timestamp":"2026-11-11T00:00:00Z"}`)
	got := stripTopLevelField(line, "tool_calls")
	if strings.Contains(string(got), "tool_calls") {
		t.Fatalf("tool_calls 未被裁掉：%s", got)
	}
	var v map[string]any
	if err := json.Unmarshal(got, &v); err != nil {
		t.Fatalf("裁剪后不是合法 JSON: %v", err)
	}
	if v["event_type"] != "agent_end" || v["content"] != "做完了" {
		t.Fatalf("其它字段被破坏了: %+v", v)
	}
	if _, ok := v["usage"]; !ok {
		t.Fatal("usage 丢了")
	}
}
