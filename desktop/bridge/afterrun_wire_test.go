package main

import (
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// afterRunJSON 取指定 task_id 的 task_delivery 块里 after_run 的**原始 JSON 形态**。
// 返回值 has=false 表示字段不存在（旧 record 的形态）。
func afterRunJSON(t *testing.T, r *ViewReducer, taskID string) (val string, has bool) {
	t.Helper()
	for _, b := range r.Snapshot().Blocks {
		if b.Kind != blockKindTaskDelivery || b.TaskID != taskID {
			continue
		}
		raw, err := json.Marshal(b)
		if err != nil {
			t.Fatalf("marshal block: %v", err)
		}
		var m map[string]json.RawMessage
		if err := json.Unmarshal(raw, &m); err != nil {
			t.Fatalf("unmarshal block: %v", err)
		}
		v, ok := m["after_run"]
		if !ok {
			return "", false
		}
		return string(v), true
	}
	t.Fatalf("task_delivery for %s not found", taskID)
	return "", false
}

// 记录类块的 AfterRun 必须**两位都写**（false 也要落进 JSON）。
//
// 旧写法 `AfterRun bool json:"after_run,omitempty"` 会把「运行中到达」这个证据吃掉：
// 一次 checkpoint 恢复之后，前端只剩位置兜底 —— 长运行里中间那段正文（narration）被
// 当成「这一轮的结论」，之后到达的回执全被判成交卷后，`Task … completed` 永远排在
// Working 盒子下面（用户 2026-11 截图：盒子里只有思考与 toolcall）。
func TestTaskDeliveryAfterRunIsSerializedForBothStates(t *testing.T) {
	r := NewViewReducer()
	base := time.Now().Add(-time.Minute)

	if err := r.ApplyPlain(&events.AgentStart{
		RunId: "run-live", Content: "go", Timestamp: base, EventType: events.AgentStartType,
	}); err != nil {
		t.Fatalf("agent_start: %v", err)
	}
	// 运行中到达 → false 必须显式落盘（不是「字段缺失」）
	if err := r.ApplyPlain(&events.TaskResultDelivered{
		SessionId: "s1", TaskId: "task-live", Status: "completed", Result: "done",
		Timestamp: base.Add(time.Second), EventType: events.TaskResultDeliveredType,
	}); err != nil {
		t.Fatalf("task_result_delivered(live): %v", err)
	}
	val, has := afterRunJSON(t, r, "task-live")
	if !has {
		t.Fatal(`运行中到达的回执：JSON 里必须有 "after_run"（false 也要写），否则恢复后只能按位置猜`)
	}
	if val != "false" {
		t.Fatalf("运行中到达的回执 after_run=%s，want false", val)
	}

	// 交卷之后到达 → true
	if err := r.ApplyPlain(&events.AgentEnd{
		RunId: "run-live", Content: "done", FinishReason: core.FinishReasonStop,
		Timestamp: base.Add(2 * time.Second), EventType: events.AgentEndType,
	}); err != nil {
		t.Fatalf("agent_end: %v", err)
	}
	if err := r.ApplyPlain(&events.TaskResultDelivered{
		SessionId: "s1", TaskId: "task-after", Status: "completed", Result: "late",
		Timestamp: base.Add(3 * time.Second), EventType: events.TaskResultDeliveredType,
	}); err != nil {
		t.Fatalf("task_result_delivered(after): %v", err)
	}
	val, has = afterRunJSON(t, r, "task-after")
	if !has || val != "true" {
		t.Fatalf("交卷后到达的回执 after_run=%q has=%v，want true", val, has)
	}
}

// 三态：旧 record 没有这一位 → nil（前端按位置兜底）；显式 false 必须能读回来。
// 用 bool + omitempty 时两者在磁盘上无法区分，重写一次就把 false 变成「缺失」。
func TestBlockAfterRunRoundTripsTriState(t *testing.T) {
	var legacy Block
	if err := json.Unmarshal([]byte(`{"kind":"task_delivery","id":1,"task_id":"t"}`), &legacy); err != nil {
		t.Fatalf("legacy: %v", err)
	}
	if legacy.AfterRun != nil {
		t.Fatalf("旧 record 缺字段时应为 nil，得到 %v", *legacy.AfterRun)
	}

	var live Block
	if err := json.Unmarshal([]byte(`{"kind":"task_delivery","id":2,"task_id":"t","after_run":false}`), &live); err != nil {
		t.Fatalf("live: %v", err)
	}
	if live.AfterRun == nil || *live.AfterRun != false {
		t.Fatalf("after_run:false 必须读回「明确不是交卷后」，得到 %v", live.AfterRun)
	}
	raw, err := json.Marshal(live)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if got := fmt.Sprintf("%s", raw); !jsonHasAfterRunFalse(got) {
		t.Fatalf("再序列化后 after_run:false 不能丢：%s", got)
	}
}

func jsonHasAfterRunFalse(s string) bool {
	var m map[string]json.RawMessage
	if json.Unmarshal([]byte(s), &m) != nil {
		return false
	}
	v, ok := m["after_run"]
	return ok && string(v) == "false"
}
