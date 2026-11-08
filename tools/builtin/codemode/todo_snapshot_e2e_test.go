package codemode

// codemode 里的 todo_* → 外层响应的待办快照（端到端，真 node + 真引擎 + 真 todo 工具）。
//
// 用户 2026-11 报障：脚本里调 todo_update 标完成，右上角待办面板纹丝不动（模型直呼
// todo_* 正常）。链路有三段，任一段断掉都表现成同一个现象：
//   1. 引擎：嵌套调用不发射工具事件，快照必须由**外层** codemode 响应代发
//      （判据是 recorder 上的 todoWrites 计数，见 tools/engine.go finish）；
//   2. 桥：extras/meta 的判据从「工具名以 todo_ 开头」改成「事件带快照」；
//   3. 前端：只认响应上的 todos 数组（不判工具名）。
// 这里从生产入口 RunBatch 真跑一遍脚本，钉住第 1 段 + 事件真带上了快照 —— 桥与前端
// 两段各有自己的回归（desktop/bridge/codemode_todos_test.go、
// desktop/app/src/lib/codemodeNested-test.ts）。

import (
	"context"
	"testing"

	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/todo"
)

// todoSnapshotProviderOf 与 AgentLoop 同构：把会话 store 投影成事件层快照。
func todoSnapshotProviderOf(store *todo.Store) func() []events.TodoSnapshot {
	return func() []events.TodoSnapshot {
		items := store.Items()
		if len(items) == 0 {
			return nil
		}
		out := make([]events.TodoSnapshot, 0, len(items))
		for _, it := range items {
			out = append(out, events.TodoSnapshot{Id: it.Id, Title: it.Title, Done: it.Done, BlockedBy: append([]string{}, it.BlockedBy...)})
		}
		return out
	}
}

func TestScriptTodoCallCarriesSnapshotOnOuterResponse(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	store := todo.New()
	h.add(todo.Tools()...)
	h.installCodemode(nil)

	ctx := events.WithToolContext(context.Background(), "run-1", 0, nil, nil, nil)
	ctx = todo.WithStore(ctx, store)
	ctx = events.WithTodoSnapshot(ctx, todoSnapshotProviderOf(store))

	res, evs := h.run(ctx, `
await tools.todo_add({ title: "确认 auth.go 错误路径" });
await tools.todo_add({ title: "修复过期分支并补用例" });
await tools.todo_update({ id: "t1", done: true });
return "ok";
`)
	if res.IsError {
		t.Fatalf("脚本结果被判为错误：%q", res.Result)
	}
	// 真实状态：会话 store 已改（这一段原本就是对的 —— 错的是它没到 UI）
	items := store.Items()
	if len(items) != 2 || !items[0].Done || items[1].Done {
		t.Fatalf("会话 store = %+v, want [t1 done, t2 open]", items)
	}

	var outer *events.ToolResponse
	for _, ev := range evs {
		if tr, ok := ev.(*events.ToolResponse); ok && tr.Name == ToolName {
			outer = tr
		}
	}
	if outer == nil {
		t.Fatal("没有捕获到 codemode 的 tool_response")
	}
	if len(outer.Todos) != 2 || outer.Todos[0].Id != "t1" || !outer.Todos[0].Done {
		t.Fatalf("外层 todos = %+v, want 脚本执行后的清单（t1 done / t2 open）", outer.Todos)
	}
	// 嵌套调用仍不发事件（只有外层 start/end 两条工具事件）
	toolEvents := 0
	for _, ev := range evs {
		switch ev.(type) {
		case *events.ToolStart, *events.ToolResponse:
			toolEvents++
		}
	}
	if toolEvents != 2 {
		t.Fatalf("工具事件 = %d, want 2（脚本里的 todo_* 不得各自成行）", toolEvents)
	}
}
