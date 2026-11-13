package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestTraceProjectionCrossImpl 交叉实现一致性：把**同一份事件流 fixture** 分别喂给
// Go（本测试）与前端 TS（src/store/trace.ts），逐字段比对两者的投影输出。
//
// 为什么需要：Go 与 TS 是**两套独立实现**（同一份折叠规则写了两遍）。
// 前端「当前 trace」用 TS 实时投影、历史 trace 用 Go 命令投影 —— 若两者漂移，
// 用户切换 trace 时瀑布图会跳变（同一个 run 前后不一致）。单侧单测各自过，
// 但**无法发现彼此不一致**，故这里生成 golden 供 TS 侧比对。
//
// 本测试产出：desktop/bridge/testdata/trace_fixture.go.golden.json
// TS 侧比对：desktop/app/scripts/test-trace-parity.mjs

// 归一化输出类型（跨实现比对的稳定契约：字段名与顺序固定）。
type normSpan struct {
	Kind       string  `json:"kind"`
	ID         string  `json:"id,omitempty"`
	RunID      string  `json:"run_id,omitempty"`
	ParentID   string  `json:"parent_id,omitempty"`
	Name       string  `json:"name,omitempty"`
	Label      string  `json:"label,omitempty"`
	Status     string  `json:"status"`
	Depth      int     `json:"depth,omitempty"`
	StartAt    int64   `json:"start_at"`
	DurMs      int64   `json:"dur_ms,omitempty"`
	Model      string  `json:"model,omitempty"`
	InTok      int64   `json:"in_tok,omitempty"`
	OutTok     int64   `json:"out_tok,omitempty"`
	CacheTok   int64   `json:"cache_tok,omitempty"`
	CacheWrite int64   `json:"cache_write_tok,omitempty"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
	Before     int     `json:"before,omitempty"`
	After      int     `json:"after,omitempty"`
	CtxToken   int64   `json:"ctx_tokens,omitempty"`
	WaitType   string  `json:"wait_type,omitempty"`
	Questions  int     `json:"questions,omitempty"`
	IsError    bool    `json:"is_error,omitempty"`
	Retries    int     `json:"retries,omitempty"`
}

type normTrace struct {
	ID     string     `json:"id"`
	Label  string     `json:"label"`
	Status string     `json:"status"`
	Spans  []normSpan `json:"spans"`
}

func TestTraceProjectionCrossImpl(t *testing.T) {
	assertNoUserPrices(t)
	fixture := filepath.Join("testdata", "trace_fixture.json")
	raw, err := os.ReadFile(fixture)
	if err != nil {
		t.Skipf("fixture 不存在: %v", err)
	}
	var lines []map[string]any
	if err := json.Unmarshal(raw, &lines); err != nil {
		t.Fatal(err)
	}
	// 逐行序列化后走与生产一致的解析（含白名单过滤）
	var evs []traceEvent
	for _, l := range lines {
		b, err := json.Marshal(l)
		if err != nil {
			t.Fatal(err)
		}
		if e, ok := parseTraceEvent(string(b)); ok {
			evs = append(evs, e)
		}
	}

	details := buildTraces(evs, "")

	// 归一化输出：只保留跨实现应一致的字段（字段名与顺序固定，便于逐字节比对）。
	out := make([]normTrace, 0, len(details))
	for _, d := range details {
		nt := normTrace{ID: d.Summary.ID, Label: d.Summary.Label, Status: d.Summary.Status}
		for _, s := range d.Spans {
			nt.Spans = append(nt.Spans, normSpan{
				Kind: s.Kind, ID: s.ID, RunID: s.RunID, ParentID: s.ParentID,
				Name: s.Name, Label: s.Label, Status: s.Status, Depth: s.Depth,
				StartAt: s.StartAt, DurMs: s.DurMs, Model: s.Model,
				InTok: s.InTok, OutTok: s.OutTok, CacheTok: s.CacheTok, CacheWrite: s.CacheWriteTok, CostUSD: s.CostUSD,
				Before: s.Before, After: s.After, CtxToken: s.CtxTokens,
				WaitType: s.WaitType, Questions: s.Questions,
				IsError: s.IsError, Retries: s.Retries,
			})
		}
		out = append(out, nt)
	}

	enc, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	golden := fixture + ".go.golden.json"
	if err := os.WriteFile(golden, append(enc, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("Go 投影：%d 条 trace，%d 个 span → %s", len(out), countNormSpans(out), golden)
	if len(out) == 0 {
		t.Fatal("投影结果为空：fixture 未被识别")
	}
}

func countNormSpans(ts []normTrace) int {
	n := 0
	for _, t := range ts {
		n += len(t.Spans)
	}
	return n
}
