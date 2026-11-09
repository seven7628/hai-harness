package tools

// 编排脚本里的 todo_* 必须把「改过的清单」带回**外层**工具响应。
//
// 用户 2026-11 报障：codemode 里调 todo_update 标完成，右上角待办面板纹丝不动
//（直呼 todo_update 正常）。根因是快照的取用条件绑在「工具名以 todo_ 开头的最外层
// 调用」上，而脚本里的 todo_* 是嵌套调用：Depth=1、按契约不发射工具事件 —— 快照
// 既没挂在子调用上（不存在），也没挂在外层 codemode 上（名字不匹配）。于是 reducer
// 与前端都收不到 `todos`，清单停在脚本跑之前的状态（刷新/重开后才被会话 store 纠正）。
//
// 这里钉两条：
//  1. 外层工具响应携带本运行快照（脚本改过清单的直接后果）；
//  2. 嵌套调用依旧不发射工具事件（既有契约，不能为了带快照而放开）。

import (
	"context"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// orchestratorStub 最小编排工具（codemode 形态）：Call 内发起一次嵌套调用。
type orchestratorStub struct {
	BaseTool
	engine ExecuteOneEngine
	inner  core.ToolCall
}

func (t *orchestratorStub) Call(ctx context.Context, name string, _ string) (string, error) {
	inner := t.inner
	inner.Id = name + "-nested"
	t.engine.ExecuteOne(ctx, inner, ExecuteOpts{ParentCallId: name, Depth: 1})
	return "script ok", nil
}

func (t *orchestratorStub) ValidParams(context.Context, string, string) error { return nil }

// todoSnapshotProvider 模拟 AgentLoop 注入的本运行待办快照（events.WithTodoSnapshot）。
func todoSnapshotProvider(snap []events.TodoSnapshot) func() []events.TodoSnapshot {
	return func() []events.TodoSnapshot { return snap }
}

func findToolResponse(list []events.Event, id string) *events.ToolResponse {
	for _, ev := range list {
		if tr, ok := ev.(*events.ToolResponse); ok && tr.Id == id {
			return tr
		}
	}
	return nil
}

func TestNestedTodoCallCarriesSnapshotOnOuterResponse(t *testing.T) {
	snap := []events.TodoSnapshot{{Id: "t1", Title: "确认 auth.go 错误路径", Done: true}}
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &stubTool{
		BaseTool: BaseTool{Name_: "todo_update", Description_: "todo", Params_: map[string]any{}},
		result:   "[todo: 0 open, 1 done]",
	})
	e.RegisterTool(context.Background(), &orchestratorStub{
		BaseTool: BaseTool{Name_: "codemode", Description_: "run script", Params_: map[string]any{}},
		engine:   e,
		inner:    core.ToolCall{Name: "todo_update", Arguments: `{"id":"t1","done":true}`},
	})

	handler, list := collectHandler()
	ctx := events.WithToolContext(context.Background(), "run-1", 0, handler, nil, nil)
	ctx = events.WithTodoSnapshot(ctx, todoSnapshotProvider(snap))
	if _, err := e.Sequence(ctx, []core.ToolCall{{Id: "outer", Name: "codemode", Arguments: "{}"}}, handler); err != nil {
		t.Fatalf("sequence: %v", err)
	}

	outer := findToolResponse(*list, "outer")
	if outer == nil {
		t.Fatal("missing outer tool_response")
	}
	if len(outer.Todos) != 1 || outer.Todos[0].Id != "t1" || !outer.Todos[0].Done {
		t.Fatalf("外层 todos = %+v, want 本运行快照（脚本改过清单）", outer.Todos)
	}

	// 契约不变：嵌套调用不发射工具事件（只有外层 start/end 两条）。
	toolEvents := 0
	for _, ev := range *list {
		switch ev.(type) {
		case *events.ToolStart, *events.ToolResponse:
			toolEvents++
		}
	}
	if toolEvents != 2 {
		t.Fatalf("tool events = %d, want 2（嵌套调用必须保持不发射）", toolEvents)
	}
}

// 脚本里**没有** todo_* 时不得凭空多带快照（否则每次 codemode 都会把清单重写一遍，
// 事件体积与 reducer 的 dirty 标记都白涨）。
func TestNestedCallWithoutTodoCarriesNoSnapshot(t *testing.T) {
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &stubTool{
		BaseTool: BaseTool{Name_: "read_file", Description_: "read", Params_: map[string]any{}},
		result:   "ok",
	})
	e.RegisterTool(context.Background(), &orchestratorStub{
		BaseTool: BaseTool{Name_: "codemode", Description_: "run script", Params_: map[string]any{}},
		engine:   e,
		inner:    core.ToolCall{Name: "read_file", Arguments: `{"path":"a.go"}`},
	})

	handler, list := collectHandler()
	ctx := events.WithToolContext(context.Background(), "run-2", 0, handler, nil, nil)
	ctx = events.WithTodoSnapshot(ctx, todoSnapshotProvider([]events.TodoSnapshot{{Id: "t1", Title: "甲", Done: false}}))
	if _, err := e.Sequence(ctx, []core.ToolCall{{Id: "outer", Name: "codemode", Arguments: "{}"}}, handler); err != nil {
		t.Fatalf("sequence: %v", err)
	}
	if outer := findToolResponse(*list, "outer"); outer == nil {
		t.Fatal("missing outer tool_response")
	} else if outer.Todos != nil {
		t.Fatalf("外层 todos = %+v, want nil（脚本没碰待办）", outer.Todos)
	}
}

// 直呼 todo_* 的既有行为不变（快照挂在自己的响应上）。
func TestTopLevelTodoCallStillCarriesSnapshot(t *testing.T) {
	snap := []events.TodoSnapshot{{Id: "t1", Title: "甲", Done: false}}
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &stubTool{
		BaseTool: BaseTool{Name_: "todo_add", Description_: "todo", Params_: map[string]any{}},
		result:   "[todo: 1 open, 0 done]",
	})

	handler, list := collectHandler()
	ctx := events.WithToolContext(context.Background(), "run-3", 0, handler, nil, nil)
	ctx = events.WithTodoSnapshot(ctx, todoSnapshotProvider(snap))
	if _, err := e.Sequence(ctx, []core.ToolCall{{Id: "t", Name: "todo_add", Arguments: `{"title":"甲"}`}}, handler); err != nil {
		t.Fatalf("sequence: %v", err)
	}
	tr := findToolResponse(*list, "t")
	if tr == nil || len(tr.Todos) != 1 || tr.Todos[0].Id != "t1" {
		t.Fatalf("顶层 todo_add todos = %+v, want 本运行快照", tr)
	}
}
