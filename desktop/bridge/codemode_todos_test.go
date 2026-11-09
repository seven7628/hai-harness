package main

// 编排型工具（codemode）的待办快照入档回归测试（用户 2026-11 报障）。
//
// 症状：codemode 里调 todo_update 标完成，右上角待办面板纹丝不动（直呼 todo_* 正常）。
// 根因在事件层已经修好（脚本里的 todo_* 由外层 codemode 响应代发快照，见
// tools/engine.go finish）—— 但宿主还有两道「只看工具名」的闸：
//   - emit 的 extras 注入（main.go）
//   - reducer 的 meta.Todos 注入（checkpoint.go observe）
// 两道闸都改成「事件带快照」后，桥这一侧才真的把清单送进视图与事件流。
//
// 这里钉三件事：
//  1. 判据覆盖 codemode 响应（带快照 → 入档；不带 → 不凭空乱写）；
//  2. 旧事件日志（todo_* 无 Todos 字段）仍走名字判据（回退会话 store，行为不变）；
//  3. 归属规则不因外层改名而失效：子 run 的 codemode 快照只落它自己的卡片。

import (
	"testing"
	"time"

	"github.com/seven7628/hai-harness/events"
)

func TestTodoResponseSnapshotGate(t *testing.T) {
	cases := []struct {
		name string
		ev   *events.ToolResponse
		want bool
	}{
		{"codemode 带快照（脚本改过清单）", &events.ToolResponse{Name: "codemode", Todos: todoSnap("t1", "甲", true)}, true},
		{"codemode 无快照（脚本没碰待办）", &events.ToolResponse{Name: "codemode"}, false},
		{"旧日志：todo_* 无快照字段", &events.ToolResponse{Name: "todo_add"}, true},
		{"普通工具", &events.ToolResponse{Name: "bash"}, false},
	}
	for _, c := range cases {
		if got := todoResponseCarriesSnapshot(c.ev); got != c.want {
			t.Errorf("%s: todoResponseCarriesSnapshot = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCodemodeTodoSnapshotReachesSessionList(t *testing.T) {
	r := NewViewReducer()
	bs := &bridgeSession{reducer: r, provider: "deepseek"}
	ts := time.Now().Add(-time.Minute)

	bs.observe(evAgentStart(t, "root", "", "", "", ts))
	bs.observe(evToolStart(t, "root", "outer", "codemode", `{"code":"tools.todo_update(...)"}`, ts))
	bs.observe(&events.ToolResponse{
		RunId: "root", Id: "outer", Name: "codemode", Result: "script ok",
		Todos:     todoSnap("t1", "确认 auth.go 错误路径", true),
		Timestamp: ts, EventType: events.ToolRunEndType,
	})

	cp := r.Snapshot()
	if len(cp.Todos) != 1 || cp.Todos[0].ID != "t1" || !cp.Todos[0].Done {
		t.Fatalf("会话清单 = %+v, want [t1 done]（codemode 里的 todo_* 必须生效）", cp.Todos)
	}
	// 工具行本身不该被当成计划卡的数据源（todos 只挂在工具块上做定格，渲染按工具名判）
	tools := blockByKind(t, cp, "tool")
	if len(tools) != 1 || tools[0].Name != "codemode" {
		t.Fatalf("tool 块 = %+v, want 单条 codemode", tools)
	}
}

func TestCodemodeTodoSnapshotInSubRunStaysOffSessionList(t *testing.T) {
	r := NewViewReducer()
	bs := &bridgeSession{reducer: r, provider: "deepseek"}
	ts := time.Now().Add(-time.Minute)

	bs.observe(evAgentStart(t, "root", "", "", "", ts))
	bs.observe(evAgentStart(t, "sub-1", "root", "review", "task-s1", ts.Add(time.Second)))
	bs.observe(evToolStart(t, "sub-1", "s-outer", "codemode", `{"code":"…"}`, ts.Add(2*time.Second)))
	bs.observe(&events.ToolResponse{
		RunId: "sub-1", Id: "s-outer", Name: "codemode", Result: "script ok",
		Todos:     todoSnap("t1", "子任务 X", false),
		Timestamp: ts.Add(2 * time.Second), EventType: events.ToolRunEndType,
	})

	cp := r.Snapshot()
	if len(cp.Todos) != 0 {
		t.Fatalf("会话清单 = %+v, want 空（子 run 的脚本不能顶掉主会话清单）", cp.Todos)
	}
	agent := blockByKind(t, cp, "agent")
	if len(agent) != 1 || len(agent[0].Todos) != 1 || agent[0].Todos[0].Title != "子任务 X" {
		t.Fatalf("子 agent 卡片 todos = %+v, want [子任务 X]", agent)
	}
}

// TestEmitInjectsCodemodeTodosExtra 真实 emit 漏斗：脚本改过待办后，前端收到的
// tool_run_end 行必须带 todos 字段（这是待办面板的唯一数据源），同一事件也要入 reducer。
func TestEmitInjectsCodemodeTodosExtra(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	m, buf := newManagerWithAppData(t, t.TempDir())
	bs, err := m.create(t.TempDir(), "s1", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ts := time.Now().Add(-time.Second)
	bs.emit(&events.ToolStart{RunId: "r1", Id: "outer", Name: "codemode", Arguments: `{"code":"…"}`, Timestamp: ts, EventType: events.ToolRunStartType})
	bs.emit(&events.ToolResponse{
		RunId: "r1", Id: "outer", Name: "codemode", Result: "script ok",
		Todos:     todoSnap("t1", "确认 auth.go 错误路径", true),
		Timestamp: ts.Add(time.Second), EventType: events.ToolRunEndType,
	})

	line := findFirst(buf.String(), func(mm map[string]any) bool {
		return mm["event_type"] == "tool_run_end" && mm["id"] == "outer"
	})
	if line == nil {
		t.Fatalf("没找到 codemode 的 tool_run_end:\n%s", buf.String())
	}
	list, ok := line["todos"].([]any)
	if !ok || len(list) != 1 {
		t.Fatalf("事件行 todos = %v, want 1 条（缺了它待办面板就不动）", line["todos"])
	}
	item, _ := list[0].(map[string]any)
	if item["id"] != "t1" || item["done"] != true {
		t.Fatalf("事件行 todos[0] = %v, want {id:t1, done:true}", item)
	}

	// 同一事件已入 reducer：checkpoint 与实时视图同源（刷新/重开读到的是同一份清单）。
	cp := bs.reducer.Snapshot()
	if len(cp.Todos) != 1 || cp.Todos[0].ID != "t1" || !cp.Todos[0].Done {
		t.Fatalf("checkpoint todos = %+v, want [t1 done]", cp.Todos)
	}
}
