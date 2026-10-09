package main

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/session"
)

// codemode 宿主渲染（P2-c）在 **checkpoint 侧**的两条规则 —— 与 desktop/app 的
// useAppStore（实时视图）必须一致，否则刷新/恢复后界面与实时看到的不是一回事：
//  1. 带 parent_call_id 的工具事件不得生成独立工具块/链路节点（设计文档 §17 第一条：
//     嵌套调用不是模型发起的工具调用，pi 的 renderer 直接吞掉带 parent 的事件）；
//  2. 外层 ToolResponse.NestedCalls 必须进 checkpoint（session 恢复后清单仍在），
//     Snapshot → JSON → Restore 往返不丢。
//
// 「会话级事实照常入档」也一并钉住：子调用的 diff 是真实发生的文件变更，不能因为
// 不显示成一行就丢掉（Git 面板会变错）。

// evToolStartParented / evToolEndParented 构造嵌套子调用（parent_call_id 非空）事件。
func evToolStartParented(t *testing.T, runID, id, parent, name, args string, ts time.Time) *events.ToolStart {
	t.Helper()
	e := evToolStart(t, runID, id, name, args, ts)
	e.ParentCallId = parent
	return e
}

func evToolEndParented(t *testing.T, runID, id, parent, name, result string, diff *core.FileDiff, ts time.Time) *events.ToolResponse {
	t.Helper()
	e := evToolEnd(t, runID, id, name, result, false, diff, ts)
	e.ParentCallId = parent
	return e
}

func codeModeRecords() []core.NestedCallRecord {
	return []core.NestedCallRecord{
		{Id: "n1", Name: "read_file", Args: `{"path":"/tmp/a.txt"}`, Result: "hello", Duration: 1500 * time.Microsecond},
		{Id: "n2", Name: "bash", Args: `{"command":"echo hi"}`, IsError: true, Error: "boom", Duration: 2 * time.Second},
	}
}

// TestReducerParentedToolEventsCreateNoRows 带 parent 的事件被吞掉：不建块、不建链路节点；
// 但 diff 作为会话级事实入档。
func TestReducerParentedToolEventsCreateNoRows(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Minute)

	// 外层 codemode 调用（顶层）
	mustApply(t, r, evToolStart(t, "root-1", "outer-1", "codemode", `{"code":"…"}`, ts))
	// 脚本内的两个子调用：start + end 全带 parent
	mustApply(t, r, evToolStartParented(t, "root-1", "nested-1", "outer-1", "bash", `{"command":"echo hi"}`, ts.Add(time.Second)))
	mustApply(t, r, evToolEndParented(t, "root-1", "nested-1", "outer-1", "bash", "hi", &core.FileDiff{Path: "out.txt", Added: 1, Removed: 0}, ts.Add(2*time.Second)))
	// 外层收尾（自带嵌套清单）
	outer := evToolEnd(t, "root-1", "outer-1", "codemode", "done", false, nil, ts.Add(3*time.Second))
	outer.NestedCalls = codeModeRecords()
	mustApply(t, r, outer)

	cp := r.Snapshot()
	tools := blockByKind(t, cp, "tool")
	if len(tools) != 1 {
		t.Fatalf("工具块数 = %d, want 1（嵌套子调用不得生成独立工具行）: %+v", len(tools), tools)
	}
	if tools[0].ToolID != "outer-1" || tools[0].Name != "codemode" {
		t.Fatalf("唯一工具块应是外层调用，得到 %+v", tools[0])
	}
	for _, n := range cp.RunNodes {
		if strings.Contains(n.ID, "nested-1") {
			t.Fatalf("嵌套子调用不得生成链路节点: %+v", n)
		}
	}
	if len(cp.Diffs) != 1 || cp.Diffs[0].Path != "out.txt" {
		t.Fatalf("子调用的 diff 是真实发生的文件变更，必须入档: %+v", cp.Diffs)
	}
}

// TestReducerCodemodeNestedCallsReachCheckpoint 外层结果的 NestedCalls 进 checkpoint，
// 字段/顺序/纳秒时长与事件一致，且 JSON 里确实有 nested_calls（宿主按它渲染清单）。
func TestReducerCodemodeNestedCallsReachCheckpoint(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Minute)
	mustApply(t, r, evToolStart(t, "root-1", "outer-1", "codemode", `{"code":"…"}`, ts))
	outer := evToolEnd(t, "root-1", "outer-1", "codemode", "done", false, nil, ts.Add(time.Second))
	outer.NestedCalls = codeModeRecords()
	mustApply(t, r, outer)

	cp := r.Snapshot()
	tools := blockByKind(t, cp, "tool")
	if len(tools) != 1 || len(tools[0].NestedCalls) != 2 {
		t.Fatalf("嵌套清单未进 checkpoint: %+v", tools)
	}
	got := tools[0].NestedCalls
	if got[0].ID != "n1" || got[0].Name != "read_file" || got[0].Args != `{"path":"/tmp/a.txt"}` || got[0].IsError {
		t.Fatalf("n1 投影错: %+v", got[0])
	}
	if got[0].Duration != int64(1500*time.Microsecond) {
		t.Fatalf("n1 时长 = %d, want %d（纳秒）", got[0].Duration, int64(1500*time.Microsecond))
	}
	if !got[1].IsError || got[1].Error != "boom" || got[1].Duration != int64(2*time.Second) {
		t.Fatalf("n2 投影错（错误/时长）: %+v", got[1])
	}

	raw, err := json.Marshal(cp)
	if err != nil {
		t.Fatal(err)
	}
	var wire struct {
		Blocks []struct {
			ToolID      string `json:"tool_id"`
			NestedCalls []struct {
				ID       string `json:"id"`
				Name     string `json:"name"`
				IsError  bool   `json:"is_error"`
				Duration int64  `json:"duration"`
			} `json:"nested_calls"`
		} `json:"blocks"`
	}
	if err := json.Unmarshal(raw, &wire); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, b := range wire.Blocks {
		if b.ToolID == "outer-1" {
			found = true
			if len(b.NestedCalls) != 2 || b.NestedCalls[1].Name != "bash" || !b.NestedCalls[1].IsError {
				t.Fatalf("wire 形态的 nested_calls 不对: %+v", b.NestedCalls)
			}
		}
	}
	if !found {
		t.Fatalf("wire 形态找不到外层工具块的 nested_calls: %s", string(raw))
	}
	// 追加字段，不得改变既有 schema 版本（parseCheckpoint 也会按同一版本校验）
	if cp.SchemaVersion != checkpointSchemaVersion {
		t.Fatalf("schema_version = %d, want %d（只许追加字段）", cp.SchemaVersion, checkpointSchemaVersion)
	}
}

// TestReducerNestedCallsSurviveRestore 恢复往返：session 恢复后行内清单仍在（宿主 hydrate
// 读的就是这个字段），并且恢复后到达的嵌套事件依旧不建行。
func TestReducerNestedCallsSurviveRestore(t *testing.T) {
	r := NewViewReducer()
	ts := time.Now().Add(-time.Minute)
	mustApply(t, r, evToolStart(t, "root-1", "outer-1", "codemode", `{"code":"…"}`, ts))
	outer := evToolEnd(t, "root-1", "outer-1", "codemode", "done", false, nil, ts.Add(time.Second))
	outer.NestedCalls = codeModeRecords()
	mustApply(t, r, outer)

	// 走真实恢复路径：Snapshot → JSON → 校验 → Restore
	raw, err := json.Marshal(r.Snapshot())
	if err != nil {
		t.Fatal(err)
	}
	cp, err := parseCheckpoint(session.ViewCheckpoint(raw))
	if err != nil {
		t.Fatalf("parseCheckpoint: %v", err)
	}
	r2 := NewViewReducer()
	if err := r2.Restore(cp); err != nil {
		t.Fatal(err)
	}
	tools := blockByKind(t, r2.Snapshot(), "tool")
	if len(tools) != 1 || len(tools[0].NestedCalls) != 2 || tools[0].NestedCalls[1].Error != "boom" {
		t.Fatalf("恢复后嵌套清单丢失: %+v", tools)
	}
	// 恢复后（去重集合已清空）重放同一条嵌套事件：仍不得建块
	mustApply(t, r2, evToolStartParented(t, "root-1", "nested-9", "outer-1", "bash", "{}", ts.Add(2*time.Second)))
	mustApply(t, r2, evToolEndParented(t, "root-1", "nested-9", "outer-1", "bash", "hi", nil, ts.Add(3*time.Second)))
	tools = blockByKind(t, r2.Snapshot(), "tool")
	if len(tools) != 1 {
		t.Fatalf("恢复后重放嵌套事件又建行了: %+v", tools)
	}
}
