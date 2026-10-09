package tools

import (
	"context"
	"testing"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// TestFinishTakesRecordOnlyAtTopLevel 验证 finish() 的 `call.Depth == 0` 判别式：
// 最外层调用取走累积记录；嵌套子调用（Depth>0）**不得**取走，
// 否则累积记录会被子调用提前清空（外层最终拿到空）。
//
// 这是 Phase 0 最容易出错的一处 —— 嵌套子调用同样经过 execute()→finish()。
func TestFinishTakesRecordOnlyAtTopLevel(t *testing.T) {
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &stubTool{
		BaseTool: BaseTool{Name_: "leaf", Description_: "leaf", Params_: map[string]any{}},
		result:   "leaf-ok",
	})

	rec := core.NewNestedRecorder()

	// 模拟编排工具的 ctx：注入 recorder。
	ctx := events.WithNestedRecorder(context.Background(), rec)

	// ① 先跑一个**嵌套**调用（Depth=1）：它会走 execute→finish，但不得取记录。
	e.ExecuteOne(ctx, core.ToolCall{Id: "n1", Name: "leaf", Arguments: "{}"}, ExecuteOpts{Depth: 1})
	e.ExecuteOne(ctx, core.ToolCall{Id: "n2", Name: "leaf", Arguments: "{}"}, ExecuteOpts{Depth: 1})

	if got := rec.Stats(); got != 0 {
		t.Fatalf("Stats() = %d, want 0 (nested calls must record fine)", got)
	}

	// ② 再跑**最外层**调用（Depth=0）：它的 finish 应当取走全部 2 条记录。
	out := e.ExecuteOne(ctx, core.ToolCall{Id: "outer", Name: "leaf", Arguments: "{}"}, ExecuteOpts{Depth: 0})

	if len(out.NestedCalls) != 2 {
		t.Fatalf("outer NestedCalls = %d, want 2 (nested records must survive to the outermost finish)", len(out.NestedCalls))
	}
	if out.NestedCalls[0].Id != "n1" || out.NestedCalls[1].Id != "n2" {
		t.Fatalf("record order lost: %+v", out.NestedCalls)
	}
	// 取走后清空。
	if again := rec.TakeRecord(); len(again) != 0 {
		t.Fatalf("second TakeRecord = %d, want 0", len(again))
	}
}

// TestNoRecorderNoNestedField 没有编排上下文时，NestedCalls 必须为 nil
// （普通工具不受影响 —— 零行为变化）。
func TestNoRecorderNoNestedField(t *testing.T) {
	e := newNestedEngine(t)
	r := e.ExecuteOne(context.Background(), core.ToolCall{Id: "c1", Name: "count"}, ExecuteOpts{})
	if r.NestedCalls != nil {
		t.Fatalf("NestedCalls = %+v, want nil for non-orchestrated calls", r.NestedCalls)
	}
}
