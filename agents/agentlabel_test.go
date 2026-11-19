package agents

// 子智能体身份标签（`agent_task_id`）的行为测试（2026-11-19）。
//
// 三条不变量，逐条钉住：
//
//  1. **后台子运行**（RunOptions.TaskId 非空）发出的事件带 agent_task_id。
//  2. **主会话运行**（TaskId 空）的事件**一个都不带** —— 且序列化后的 JSON
//     **逐字节不含** "agent_task_id" 这个键。
//     ⇒ 这是「不影响 checkpoint / 会话恢复 / 前端」这条约束的**可执行证明**：
//     主会话的事件流落盘内容与改造前完全一致。
//  3. **同步子运行**（TaskId 空但有 ParentRunId）也不带 —— 它不属于任何后台任务。

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
)

func oneTextRound() []func(func(e provider.StreamEvent) error) error {
	return []func(func(e provider.StreamEvent) error) error{
		func(handler func(e provider.StreamEvent) error) error {
			_ = handler(provider.LLMStartEvent{Model: "m1"})
			_ = handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "hi"})
			return nil
		},
	}
}

// TestAgentTaskIDStampedOnSubagentRun：后台子运行的事件带 agent_task_id。
func TestAgentTaskIDStampedOnSubagentRun(t *testing.T) {
	p := &mockProvider{rounds: oneTextRound()}
	loop := NewAgentLoop(WithProvider(p), WithModel("m1"))
	h, list := collectEvents()

	loop.RunStream(context.Background(),
		[]core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "q"})},
		h,
		&RunOptions{TaskId: "task-abc12345", Depth: 1, Name: "子"})

	var sawStart, sawEnd bool
	for _, e := range *list {
		switch ev := e.(type) {
		case *events.LLMStart:
			sawStart = true
			if ev.AgentTaskId != "task-abc12345" {
				t.Fatalf("LLMStart 未盖子智能体身份: %+v", ev)
			}
		case *events.LLMEnd:
			sawEnd = true
			if ev.AgentTaskId != "task-abc12345" {
				t.Fatalf("LLMEnd 未盖子智能体身份: %+v", ev)
			}
		}
	}
	if !sawStart || !sawEnd {
		t.Fatalf("没抓到 LLMStart/LLMEnd（sawStart=%v sawEnd=%v）", sawStart, sawEnd)
	}
}

// TestMainSessionEventsUnchanged：主会话运行的事件**逐字节**不含 agent_task_id。
//
// 这是本改动的核心安全网：主会话事件流是 checkpoint / legacy 迁移重放的输入，
// 字节不变 ⇒ reducer 行为不变 ⇒ checkpoint 不变 ⇒ 会话恢复不变。
func TestMainSessionEventsUnchanged(t *testing.T) {
	p := &mockProvider{rounds: oneTextRound()}
	loop := NewAgentLoop(WithProvider(p), WithModel("m1"))
	h, list := collectEvents()

	loop.Run(context.Background(),
		[]core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "q"})}, h)

	if len(*list) == 0 {
		t.Fatal("没抓到任何事件")
	}
	for _, e := range *list {
		raw, err := json.Marshal(e)
		if err != nil {
			t.Fatalf("序列化失败: %v", err)
		}
		if strings.Contains(string(raw), "agent_task_id") {
			t.Fatalf("主会话事件里出现了 agent_task_id（必须逐字节不变）：%T → %s", e, raw)
		}
	}
}

// TestSyncSubrunNotStamped：同步子运行（无 TaskId）不带标记 —— 它不属于任何后台任务。
func TestSyncSubrunNotStamped(t *testing.T) {
	p := &mockProvider{rounds: oneTextRound()}
	loop := NewAgentLoop(WithProvider(p), WithModel("m1"))
	h, list := collectEvents()

	loop.RunStream(context.Background(),
		[]core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "q"})},
		h,
		&RunOptions{ParentRunId: "parent-run", Depth: 1, Name: "同步子"})

	for _, e := range *list {
		switch ev := e.(type) {
		case *events.LLMStart:
			if ev.AgentTaskId != "" {
				t.Fatalf("同步子运行不该有 agent_task_id: %+v", ev)
			}
		case *events.LLMEnd:
			if ev.AgentTaskId != "" {
				t.Fatalf("同步子运行不该有 agent_task_id: %+v", ev)
			}
		}
	}
}

// TestStampedEventSerializesField：盖章后 JSON 里**确实**出现该键
// （否则上一条测试会因为"根本没序列化出去"而假绿）。
func TestStampedEventSerializesField(t *testing.T) {
	le := &events.LLMEnd{RunId: "r1", Content: "x"}
	events.StampAgentTaskID(le, "task-abc12345")
	raw, err := json.Marshal(le)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"agent_task_id":"task-abc12345"`) {
		t.Fatalf("盖章后 JSON 里没有 agent_task_id: %s", raw)
	}
}
