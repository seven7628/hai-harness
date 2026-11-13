package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// writeTraceLog 把事件写入临时 workspace 的事件日志（与 logEventLine 同布局：
// events/<wsKey>/<sid>.jsonl）。
func writeTraceLog(t *testing.T, ws, sid string, evs []map[string]any) {
	t.Helper()
	dir := wsEventsDir(ws)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(filepath.Join(dir, sid+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, e := range evs {
		b, err := json.Marshal(e)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append(b, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

// traceLogFixture 两个 trace（old/new）+ 主/子 agent + 工具/压缩，覆盖投影主路径。
func traceLogFixture() []map[string]any {
	return []map[string]any{
		{"event_type": "agent_start", "run_id": "old", "content": "第一条任务", "timestamp": ts(0)},
		{"event_type": "llm_start", "run_id": "old", "request_id": "o1", "timestamp": ts(10)},
		{"event_type": "llm_end", "run_id": "old", "request_id": "o1", "timestamp": ts(210),
			"usage": map[string]any{"Input": 10, "Output": 2}},
		{"event_type": "agent_end", "run_id": "old", "timestamp": ts(300), "duration_ms": 300},

		{"event_type": "agent_start", "run_id": "new", "content": "第二条任务", "timestamp": ts(1000)},
		{"event_type": "llm_start", "run_id": "new", "request_id": "n1", "timestamp": ts(1010)},
		{"event_type": "llm_end", "run_id": "new", "request_id": "n1", "timestamp": ts(1210),
			"usage": map[string]any{"Input": 100, "Output": 20}},
		{"event_type": "tool_run_start", "run_id": "new", "id": "c1", "name": "bash", "timestamp": ts(1220)},
		{"event_type": "tool_run_end", "run_id": "new", "id": "c1", "name": "bash", "timestamp": ts(1320),
			"result": "SHOULD_NOT_BE_PROJECTED"},
		{"event_type": "compress_start", "run_id": "new", "before": 42, "timestamp": ts(1400)},
		{"event_type": "compress_end", "run_id": "new", "before": 42, "after": 12, "timestamp": ts(1600)},
		{"event_type": "agent_start", "run_id": "sub", "parent_run_id": "new", "name": "worker",
			"depth": 1, "timestamp": ts(1700)},
		{"event_type": "llm_start", "run_id": "sub", "request_id": "s1", "timestamp": ts(1710)},
		{"event_type": "llm_end", "run_id": "sub", "request_id": "s1", "timestamp": ts(1810)},
		{"event_type": "agent_end", "run_id": "sub", "timestamp": ts(1900), "duration_ms": 200},
		{"event_type": "agent_end", "run_id": "new", "timestamp": ts(2000), "duration_ms": 1000},
	}
}

// TestTraceDataListAndDetail：列表粒度只回摘要（不含 span），详情粒度回完整 span，
// 且正文不进投影。
func TestTraceDataListAndDetail(t *testing.T) {
	appDataDirOverride = t.TempDir()
	defer func() { appDataDirOverride = "" }()
	ws := "/tmp/ws-trace-test"
	writeTraceLog(t, ws, "s1", traceLogFixture())

	m := &manager{}

	// ① 列表：不传 run_id → 只有摘要，无 trace 字段
	list := m.traceData(ws, "s1", "", 0, false)
	traces, ok := list["traces"].([]traceSummary)
	if !ok || len(traces) != 2 {
		t.Fatalf("列表应返回 2 条 trace，got %#v", list["traces"])
	}
	if _, has := list["trace"]; has {
		t.Fatal("列表粒度不应返回 trace 详情")
	}
	// 最近在上
	if traces[0].ID != "new" || traces[1].ID != "old" {
		t.Fatalf("排序应为最近在上: %s, %s", traces[0].ID, traces[1].ID)
	}
	nw := traces[0]
	if nw.Subs != 1 || nw.Turns != 2 || nw.Tools != 1 {
		t.Fatalf("new 摘要统计: subs=%d turns=%d tools=%d, want 1/2/1", nw.Subs, nw.Turns, nw.Tools)
	}
	if nw.WallMs != 1000 {
		t.Fatalf("new wall = %d, want 1000", nw.WallMs)
	}
	if nw.Label != "第二条任务" {
		t.Fatalf("new label = %q", nw.Label)
	}
	if nw.MetaBytes <= 0 {
		t.Fatal("MetaBytes 应 > 0（前端展示存储账）")
	}
	if v, ok := list["total"].(int); !ok || v != 2 {
		t.Fatalf("total = %#v, want 2", list["total"])
	}

	// ② 详情：带 run_id → 回该 trace 的 span
	det := m.traceData(ws, "s1", "new", 0, false)
	if det["found"] != true {
		t.Fatalf("应命中 new: %#v", det["found"])
	}
	d, ok := det["trace"].(*traceDetail)
	if !ok || d == nil {
		t.Fatalf("trace 详情类型错误: %#v", det["trace"])
	}
	if d.Summary.ID != "new" {
		t.Fatalf("详情 id = %q", d.Summary.ID)
	}
	// new 子树应含：run(new) + llm(n1) + tool(c1) + compress + run(sub) + llm(s1) = 6
	if len(d.Spans) != 6 {
		t.Fatalf("span 条数 = %d, want 6: %+v", len(d.Spans), d.Spans)
	}
	b, _ := json.Marshal(d)
	if strings.Contains(string(b), "SHOULD_NOT_BE_PROJECTED") {
		t.Fatal("工具 result 正文不应进 trace 投影")
	}

	// ③ 未知 run_id：found=false（不报错）
	miss := m.traceData(ws, "s1", "nope", 0, false)
	if miss["found"] != false {
		t.Fatalf("未知 run_id 应 found=false: %#v", miss["found"])
	}
}

// TestTraceDataNoLog：无事件日志 → 空列表 + found=false（前端回退现有渲染，不报错）。
func TestTraceDataNoLog(t *testing.T) {
	appDataDirOverride = t.TempDir()
	defer func() { appDataDirOverride = "" }()
	m := &manager{}
	out := m.traceData("/tmp/ws-none", "s-nope", "", 0, false)
	if out["found"] != false {
		t.Fatalf("无日志应 found=false: %#v", out)
	}
	if tr, ok := out["traces"].([]traceSummary); !ok || len(tr) != 0 {
		t.Fatalf("无日志应返回空列表: %#v", out["traces"])
	}
	// 空参数不 panic
	if out := m.traceData("", "", "", 0, false); out["found"] != false {
		t.Fatalf("空参数应安全返回: %#v", out)
	}
}

// TestTraceDataLimitAndHasMore：limit 截断列表并置 has_more（长会话 112 条 trace 的场景）。
func TestTraceDataLimitAndHasMore(t *testing.T) {
	appDataDirOverride = t.TempDir()
	defer func() { appDataDirOverride = "" }()
	ws := "/tmp/ws-trace-limit"
	var evs []map[string]any
	for i := 0; i < 7; i++ {
		base := i * 1000
		evs = append(evs,
			map[string]any{"event_type": "agent_start", "run_id": runIDFor(i),
				"content": "task", "timestamp": ts(base)},
			map[string]any{"event_type": "agent_end", "run_id": runIDFor(i),
				"timestamp": ts(base + 100), "duration_ms": 100},
		)
	}
	writeTraceLog(t, ws, "s2", evs)
	m := &manager{}

	out := m.traceData(ws, "s2", "", 3, false)
	tr := out["traces"].([]traceSummary)
	if len(tr) != 3 {
		t.Fatalf("limit=3 应回 3 条，got %d", len(tr))
	}
	if out["has_more"] != true || out["total"] != 7 {
		t.Fatalf("has_more/total = %#v/%#v, want true/7", out["has_more"], out["total"])
	}
	// 默认 limit（0 → traceDefaultLimit）：7 < 50 → 全量且无 has_more
	out2 := m.traceData(ws, "s2", "", 0, false)
	if len(out2["traces"].([]traceSummary)) != 7 {
		t.Fatalf("默认 limit 应回全部 7 条")
	}
	if _, has := out2["has_more"]; has {
		t.Fatal("7 < 默认上限，不应有 has_more")
	}
}

func runIDFor(i int) string {
	return "run-" + string(rune('a'+i))
}

// TestTraceDataLoadMore：limit > 默认上限时应回全部条数（「加载更多」路径）。
// 回归：前端曾不传 limit → 后端每次回落默认上限（50）→ 长会话（实测最多 112 条 trace）
// 点「加载更多」重复拿到同一批，按钮表现为无反应。
func TestTraceDataLoadMore(t *testing.T) {
	appDataDirOverride = t.TempDir()
	defer func() { appDataDirOverride = "" }()
	ws := "/tmp/ws-trace-loadmore"
	const n = 120 // > traceDefaultLimit(50)
	var evs []map[string]any
	for i := 0; i < n; i++ {
		base := i * 1000
		rid := "run-" + strconv.Itoa(i)
		evs = append(evs,
			map[string]any{"event_type": "agent_start", "run_id": rid, "content": "t", "timestamp": ts(base)},
			map[string]any{"event_type": "agent_end", "run_id": rid, "timestamp": ts(base + 100), "duration_ms": 100},
		)
	}
	writeTraceLog(t, ws, "s3", evs)
	m := &manager{}

	// ① 默认（不传 limit）→ 截断到默认上限 + has_more
	def := m.traceData(ws, "s3", "", 0, false)
	if got := len(def["traces"].([]traceSummary)); got != traceDefaultLimit {
		t.Fatalf("默认应回 %d 条，got %d", traceDefaultLimit, got)
	}
	if def["total"] != n || def["has_more"] != true {
		t.Fatalf("total/has_more = %#v/%#v, want %d/true", def["total"], def["has_more"], n)
	}

	// ② 加载更多：显式传 total → 回全部（这是修复后的调用方式）
	all := m.traceData(ws, "s3", "", n, false)
	if got := len(all["traces"].([]traceSummary)); got != n {
		t.Fatalf("limit=%d 应回全部 %d 条，got %d", n, n, got)
	}
	if _, has := all["has_more"]; has {
		t.Fatal("全量返回时不应再有 has_more")
	}
}
