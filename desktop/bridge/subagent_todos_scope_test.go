package main

// 子 agent 待办归属的回归测试（用户 2026-10 报障：子智能体里显示的还是主会话的 TODO）。
//
// 三条断言对应三个不同的环节，缺任何一条这个 bug 都能"看起来修好了"：
//   1. 事件层：todo_* 的 ToolResponse 必须携带**发起运行自己**的清单（`Todos`），
//      而不能让宿主一律读会话 store（那正是根因）；
//   2. reducer：子 run 的清单落到**它自己的卡片**（Block.Todos / AgentItem.Todos），
//      会话清单 `Checkpoint.Todos` 一个字节都不许动；
//   3. 恢复：卡片级 todos 随 checkpoint 往返（否则刷新/重开后子 agent 的 TODO 又变回空，
//      用户会以为"修了个寂寞"）。

import (
	"testing"
	"time"

	"github.com/seven7628/hai-harness/events"
)

// applyMeta 与 observe() 同构：meta.Todos 由事件自带（todosOf 的入参），不再是会话 store。
func applyMeta(t *testing.T, r *ViewReducer, ev events.Event, meta EventMeta) {
	t.Helper()
	if err := r.Apply(ev, meta); err != nil {
		t.Fatalf("apply %s: %v", ev.Type(), err)
	}
}

func todoSnap(id, title string, done bool) []events.TodoSnapshot {
	return []events.TodoSnapshot{{Id: id, Title: title, Done: done}}
}

// TestSubagentTodosStayOffSessionList 子 agent 的 todo_* 不得改写会话清单。
func TestSubagentTodosStayOffSessionList(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Minute)

	// 主 run 记两条待办（会话清单）
	mustApply(t, r, evAgentStart(t, "root", "", "", "", ts))
	for i, title := range []string{"主任务 A", "主任务 B"} {
		id := "m" + string(rune('1'+i))
		start := evToolStart(t, "root", id, "todo_add", `{"title":"`+title+`"}`, ts)
		applyMeta(t, r, start, EventMeta{})
		snap := append([]events.TodoSnapshot{}, todoSnap("t1", "主任务 A", false)...)
		if i == 1 {
			snap = append(snap, events.TodoSnapshot{Id: "t2", Title: "主任务 B", Done: false})
		}
		end := evToolEnd(t, "root", id, "todo_add", "ok", false, nil, ts)
		end.Todos = snap
		applyMeta(t, r, end, EventMeta{Todos: []TodoItem{{ID: "t1", Title: "主任务 A"}, {ID: "t2", Title: "主任务 B"}}})
	}
	cp := r.Snapshot()
	if len(cp.Todos) != 2 {
		t.Fatalf("会话清单 = %+v, want 2 条", cp.Todos)
	}

	// 子 agent 自己的 todo_add：清单挂在它的卡片上，会话清单保持原样
	mustApply(t, r, evAgentStart(t, "sub-1", "root", "review", "task-s1", ts.Add(time.Second)))
	start := evToolStart(t, "sub-1", "c1", "todo_add", `{"title":"子任务 X"}`, ts.Add(2*time.Second))
	applyMeta(t, r, start, EventMeta{})
	end := evToolEnd(t, "sub-1", "c1", "todo_add", "ok", false, nil, ts.Add(2*time.Second))
	end.Todos = todoSnap("t1", "子任务 X", false)
	applyMeta(t, r, end, EventMeta{Todos: []TodoItem{{ID: "t1", Title: "子任务 X"}}})

	cp = r.Snapshot()
	if len(cp.Todos) != 2 || cp.Todos[0].Title != "主任务 A" || cp.Todos[1].Title != "主任务 B" {
		t.Fatalf("子 agent 调用后会话清单被改写 = %+v（必须保持主会话的两条）", cp.Todos)
	}
	agent := blockByKind(t, cp, "agent")
	if len(agent) != 1 {
		t.Fatalf("agent 块 = %d, want 1", len(agent))
	}
	if len(agent[0].Todos) != 1 || agent[0].Todos[0].Title != "子任务 X" {
		t.Fatalf("子 agent 卡片 todos = %+v, want [子任务 X]", agent[0].Todos)
	}
	// 工具项上的定格快照同样归属子 agent
	var itemTodos []TodoItem
	for _, it := range agent[0].Items {
		if it.Kind == agentItemTool && it.ToolID == "c1" {
			itemTodos = it.Todos
		}
	}
	if len(itemTodos) != 1 || itemTodos[0].Title != "子任务 X" {
		t.Fatalf("子 agent 工具项 todos = %+v, want [子任务 X]", itemTodos)
	}
}

// TestSubagentTodosRecursive 孙 agent 的清单也归它自己，不污染父级子 agent 与会话。
func TestSubagentTodosRecursive(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Minute)
	mustApply(t, r, evAgentStart(t, "root", "", "", "", ts))
	mustApply(t, r, evAgentStart(t, "sub-1", "root", "child", "task-1", ts.Add(time.Second)))
	mustApply(t, r, evAgentStart(t, "sub-2", "sub-1", "grand", "task-2", ts.Add(2*time.Second)))

	// 孙 agent 写自己的 todo
	applyMeta(t, r, evToolStart(t, "sub-2", "g1", "todo_add", `{"title":"孙任务"}`, ts.Add(3*time.Second)), EventMeta{})
	end := evToolEnd(t, "sub-2", "g1", "todo_add", "ok", false, nil, ts.Add(3*time.Second))
	end.Todos = todoSnap("t1", "孙任务", false)
	applyMeta(t, r, end, EventMeta{Todos: []TodoItem{{ID: "t1", Title: "孙任务"}}})

	cp := r.Snapshot()
	if len(cp.Todos) != 0 {
		t.Fatalf("会话清单 = %+v, want 空（孙 agent 的清单不是会话的）", cp.Todos)
	}
	byRun := map[string][]TodoItem{}
	for _, b := range blockByKind(t, cp, "agent") {
		byRun[b.RunID] = b.Todos
	}
	if len(byRun["sub-1"]) != 0 {
		t.Fatalf("父级子 agent todos = %+v, want 空（孙 agent 的清单不能冒泡）", byRun["sub-1"])
	}
	if len(byRun["sub-2"]) != 1 || byRun["sub-2"][0].Title != "孙任务" {
		t.Fatalf("孙 agent todos = %+v, want [孙任务]", byRun["sub-2"])
	}
}

// TestSubagentTodosSurviveRestore 卡片级 todos 随 checkpoint 往返（刷新/重开不丢）。
func TestSubagentTodosSurviveRestore(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Minute)
	mustApply(t, r, evAgentStart(t, "sub-1", "root", "review", "task-s1", ts))
	applyMeta(t, r, evToolStart(t, "sub-1", "c1", "todo_add", `{"title":"子任务 X"}`, ts), EventMeta{})
	end := evToolEnd(t, "sub-1", "c1", "todo_add", "ok", false, nil, ts)
	end.Todos = todoSnap("t1", "子任务 X", true)
	applyMeta(t, r, end, EventMeta{Todos: []TodoItem{{ID: "t1", Title: "子任务 X", Done: true}}})

	cp := r.Snapshot()
	r2 := NewViewReducer()
	if err := r2.Restore(cp); err != nil {
		t.Fatalf("restore: %v", err)
	}
	agent := blockByKind(t, r2.Snapshot(), "agent")
	if len(agent) != 1 || len(agent[0].Todos) != 1 || !agent[0].Todos[0].Done {
		t.Fatalf("恢复后子 agent todos = %+v, want [子任务 X(done)]", agent)
	}
}

// TestEmptySnapshotClearsSessionList 「带了一个空清单」是事实（清单被清空），必须照实生效 ——
// 与「没带快照」（旧事件日志重放，回退会话 store）严格区分。
func TestEmptySnapshotClearsSessionList(t *testing.T) {
	if got := todosOf(nil, nil); got != nil {
		t.Fatalf("nil 快照且无会话 = %+v, want nil（不伪造清单）", got)
	}
	if got := todosOf(nil, []events.TodoSnapshot{}); got == nil || len(got) != 0 {
		t.Fatalf("空快照 = %+v, want 非 nil 的空清单（清空语义）", got)
	}
}
