package agents

// 待办快照归属的回归（用户 2026-10 报障：子智能体里显示主会话的 TODO）。
//
// 这条链有三段，任何一段错都能让 UI 显示错清单：
//   1. RunStream 未注入 Todo → 自建独立 store（子 agent 不继承父的）；
//   2. 工具轮把 ac.Todo 的快照provider 注入工具上下文（WithTodoSnapshot）；
//   3. 引擎在 todo_* 结束时把快照挂到 ToolResponse.Todos。
// 本文件钉的是 2+3（1 已由 TestTodoDefaultIndependentStore 钉住）：**事件里的清单必须
// 是本次运行自己的**，并且两次调用之间会更新（不是首次快照的复印件）。

import (
	"context"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/todo"
	"github.com/seven7628/hai-harness/tools"
)

// TestToolResponseCarriesRunOwnTodos 断言：todo_add → ToolResponse.Todos 就是该运行的清单，
// 且第二次调用带回累积后的两条（快照是取值时刻的状态，不是第一次的缓存）。
func TestToolResponseCarriesRunOwnTodos(t *testing.T) {
	engine := tools.NewToolEngine()
	for _, tl := range todo.Tools() {
		engine.RegisterTool(context.Background(), tl)
	}
	p := &mockProvider{rounds: []func(handler func(e provider.StreamEvent) error) error{
		func(handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonToolCall,
				ToolCalls: []core.ToolCall{
					{Id: "c1", Name: "todo_add", Arguments: `{"title":"甲"}`},
					{Id: "c2", Name: "todo_add", Arguments: `{"title":"乙"}`},
				},
			})
		},
		func(handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
		},
	}}
	loop := NewAgentLoop(WithProvider(p), WithToolEngine(engine), WithModel("mock-model"))

	handler, list := collectEvents()
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "go"})}, handler)
	if ac.Err != nil {
		t.Fatalf("run: %v", ac.Err)
	}

	byTool := map[string][]events.TodoSnapshot{}
	for _, e := range *list {
		if r, ok := e.(*events.ToolResponse); ok && r.Name == "todo_add" {
			byTool[r.Id] = r.Todos
		}
	}
	if got := byTool["c1"]; len(got) != 1 || got[0].Title != "甲" {
		t.Fatalf("第一次调用快照 = %+v, want [甲]", got)
	}
	if got := byTool["c2"]; len(got) != 2 || got[0].Title != "甲" || got[1].Title != "乙" {
		t.Fatalf("第二次调用快照 = %+v, want [甲 乙]（同一 run 累积，不是首次的复印件）", got)
	}
}

// TestSubagentTodosDoNotLeakToParent 父运行注入会话清单时，子运行的快照仍来自它自己的 store。
// 这是「子 agent 不继承父 Todo」在**事件层**的验证（父清单一条都不能出现在子事件里）。
func TestSubagentTodosDoNotLeakToParent(t *testing.T) {
	engine := tools.NewToolEngine()
	for _, tl := range todo.Tools() {
		engine.RegisterTool(context.Background(), tl)
	}
	parentStore := todo.New()
	if _, err := parentStore.Add("父任务", nil); err != nil {
		t.Fatalf("seed parent: %v", err)
	}

	p := &mockProvider{rounds: []func(handler func(e provider.StreamEvent) error) error{
		func(handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonToolCall,
				ToolCalls:    []core.ToolCall{{Id: "c1", Name: "todo_add", Arguments: `{"title":"子任务"}`}},
			})
		},
		func(handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "ok"})
		},
	}}
	loop := NewAgentLoop(WithProvider(p), WithToolEngine(engine), WithModel("mock-model"))

	handler, list := collectEvents()
	// 不注入 Todo：模拟子 agent（subagent.buildOptions 刻意不传 Todo）
	ac := loop.Run(context.Background(), []core.Message{core.NewUserMessage(core.Content{Type: "text", Content: "go"})}, handler)
	if ac.Err != nil {
		t.Fatalf("run: %v", ac.Err)
	}
	for _, e := range *list {
		r, ok := e.(*events.ToolResponse)
		if !ok || r.Name != "todo_add" {
			continue
		}
		for _, it := range r.Todos {
			if it.Title == "父任务" {
				t.Fatalf("子运行的事件里出现了父会话的待办：%+v", r.Todos)
			}
		}
		if len(r.Todos) != 1 || r.Todos[0].Title != "子任务" {
			t.Fatalf("子运行快照 = %+v, want [子任务]", r.Todos)
		}
	}
	// 父 store 没被写（子 agent 的调用不作用于父清单）
	if items := parentStore.Items(); len(items) != 1 || items[0].Title != "父任务" {
		t.Fatalf("父清单 = %+v, want [父任务]（未被子运行污染）", items)
	}
}
