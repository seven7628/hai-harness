package agenthub

import (
	"errors"
	"strings"
	"testing"
)

func TestTaskHeaderRoundTrip(t *testing.T) {
	task := &Task{
		ID: "task_abc", RootTaskID: "task_abc",
		Origin:   TaskOrigin{Kind: OriginDM, AgentID: "ag_1", AgentName: "审稿"},
		Callback: CallbackAgentEnd,
	}
	raw, err := EncodeTaskHeader(task)
	if err != nil {
		t.Fatal(err)
	}
	h, err := ParseTaskHeader(raw)
	if err != nil {
		t.Fatal(err)
	}
	if h.TaskID != task.ID || h.RootTaskID != task.RootTaskID || h.Origin.Kind != OriginDM ||
		h.Origin.AgentID != "ag_1" || h.Callback != CallbackAgentEnd {
		t.Fatalf("round trip = %+v", h)
	}
	// 正文不会被当成 header；坏 JSON/缺 id 明确报错。
	for _, bad := range []string{"", "  ", "{", `{"task_id":"x"}`, `{"task_id":""}`} {
		if _, err := ParseTaskHeader(bad); !errors.Is(err, ErrInvalid) {
			t.Errorf("ParseTaskHeader(%q) = %v, want ErrInvalid", bad, err)
		}
	}
	if _, err := EncodeTaskHeader(nil); !errors.Is(err, ErrInvalid) {
		t.Fatal("nil task must be rejected")
	}
	// header 只带协议字段，不把 instruction 复制进去（上下文体积 + 敏感内容）。
	if strings.Contains(raw, "instruction") {
		t.Fatalf("header leaked instruction: %s", raw)
	}
}
