// trace.go —— 链路 tab 的历史 trace 投影（docs/TRACE_CHAIN_REDESIGN.md）。
//
// 设计要点：**不给 trace 新开文件**。会话事件日志
// （~/.go-code/events/<wsKey>/<sid>.jsonl，见 main.go logEventLine）本身就是完整的
// trace store —— 每个 run 的 agent/llm/tool/compress/ask 起止 + run_id + parent_run_id
// + timestamp 全在盘上。这里按需把事件流**投影**成 trace（不落盘、零新增存储）。
//
// 投影 vs 抄正文：
//   - 只保留元数据（时间/状态/用量/name），剥离 result/content/arguments 等正文字段。
//     实测 263 B/span：单条 trace 中位 5.8 KB、p99 168 KB、最大 714 KB
//     （对比最大会话事件日志 42 MB）。
//   - 正文永远单份：详情抽屉按 span 的回查锚点（tool_id / request_id）从 checkpoint
//     blocks 取正文，不抄进 trace。
//
// 两个粒度（避免一次推 4 MB）：
//   - list：只回 TraceSummary[]（~150 B/条；实测最坏 112 条 = 16 KB）
//   - 带 id：回该 trace 的完整 span 列表
package main

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Span 类型（前端按 kind 分支渲染详情）。
const (
	traceKindRun      = "run"      // agent_start → agent_end
	traceKindLLM      = "llm"      // llm_start → llm_end
	traceKindTool     = "tool"     // tool_run_start → tool_run_end
	traceKindCompress = "compress" // compress_start → compress_end
	traceKindWait     = "wait"     // ask_user_question（将来含审批）
)

// traceDefaultLimit 列表默认返回的 trace 条数上限（长会话实测最多 112 条）；
// 超出时置 has_more，前端可展开加载全部。
const traceDefaultLimit = 50

// traceSpan 一个 span 的元数据（前端瀑布图的一行）。
// 字段全部可选（omitempty）：旧事件缺字段时不虚构值，前端按存在性降级。
type traceSpan struct {
	Kind     string `json:"kind"`
	ID       string `json:"id,omitempty"`        // tool: 调用 id（正文回查锚点）；llm: request_id
	RunID    string `json:"run_id,omitempty"`    // 所属 run（挂树用）
	ParentID string `json:"parent_id,omitempty"` // run: 父 run（根为空）
	Name     string `json:"name,omitempty"`
	Label    string `json:"label,omitempty"`
	Status   string `json:"status"` // running|done|error|interrupted
	Depth    int    `json:"depth,omitempty"`
	StartAt  int64  `json:"start_at,omitempty"` // 相对 trace 起点的 ms 偏移
	DurMs    int64  `json:"dur_ms,omitempty"`

	// llm 指标
	Model         string  `json:"model,omitempty"`
	InTok         int64   `json:"in_tok,omitempty"`
	OutTok        int64   `json:"out_tok,omitempty"`
	CacheTok      int64   `json:"cache_tok,omitempty"`
	CacheWriteTok int64   `json:"cache_write_tok,omitempty"`
	ReasonTok     int64   `json:"reason_tok,omitempty"`
	CostUSD       float64 `json:"cost_usd,omitempty"`

	// compress
	Before    int    `json:"before,omitempty"`
	After     int    `json:"after,omitempty"`
	CtxTokens int64  `json:"ctx_tokens,omitempty"`
	Reason    string `json:"reason,omitempty"`

	// 其他
	WaitType  string `json:"wait_type,omitempty"` // wait span 的来源（ask_user / approval）
	Questions int    `json:"questions,omitempty"` // wait span 的问题条数
	TaskID    string `json:"task_id,omitempty"`
	IsError   bool   `json:"is_error,omitempty"`
	Promoted  bool   `json:"promoted,omitempty"`

	// 重试标记（llm_error 聚合到所属 llm span）
	Retries int `json:"retries,omitempty"`

	// StartAbs 绝对开始毫秒（内部用：算相对偏移与列表排序；不序列化）
	StartAbs int64 `json:"-"`
}

// traceSummary 列表项（顶部 trace 选择器）。
type traceSummary struct {
	ID        string  `json:"id"` // root run_id
	Label     string  `json:"label"`
	Status    string  `json:"status"`
	StartAt   int64   `json:"start_at"` // 绝对毫秒（列表按时间倒序）
	WallMs    int64   `json:"wall_ms"`  // 墙钟时长
	Spans     int     `json:"spans"`
	Tools     int     `json:"tools"`
	Turns     int     `json:"turns"`
	Subs      int     `json:"subs"` // 子 agent 数
	Errors    int     `json:"errors"`
	InTok     int64   `json:"in_tok"`
	OutTok    int64   `json:"out_tok"`
	CostUSD   float64 `json:"cost_usd"`
	MetaBytes int64   `json:"meta_bytes"` // 该 trace 元数据体积（前端展示存储账）
}

// traceDetail 单个 trace 的完整 span 列表。
type traceDetail struct {
	Summary traceSummary `json:"summary"`
	Spans   []traceSpan  `json:"spans"`
}

// —— 事件解析用的最小字段集（只取投影需要的；正文大字段直接不解析）——

type traceEvent struct {
	EventType string `json:"event_type"`
	RunID     string `json:"run_id"`
	ParentID  string `json:"parent_run_id"`
	Timestamp string `json:"timestamp"`
	Name      string `json:"name"`
	Content   string `json:"content"`
	Model     string `json:"model"`
	Depth     int    `json:"depth"`
	TaskID    string `json:"task_id"`
	ToolID    string `json:"id"`
	RequestID string `json:"request_id"`
	Index     int64  `json:"index"`
	IsError   bool   `json:"is_error"`
	Before    int    `json:"before"`
	After     int    `json:"after"`
	Reason    string `json:"reason"`
	CtxTokens int64  `json:"ctx_tokens"`
	CtxWindow int64  `json:"context_window"`
	DurMS     int64  `json:"duration_ms"`
	Finish    string `json:"finish_reason"`
	Error     string `json:"error"`
	ErrorKind string `json:"error_kind"`

	// usage（llm_end 携带；键名与 core.Usage 一致）
	Usage *struct {
		Input      int64 `json:"Input"`
		Output     int64 `json:"Output"`
		CacheRead  int64 `json:"CacheRead"`
		CacheWrite int64 `json:"CacheWrite"`
		Reasoning  int64 `json:"Reasoning"`
	} `json:"usage"`
	CostUSD float64 `json:"cost_usd"`

	// ask_user_question
	Questions []struct {
		Question string   `json:"question"`
		Options  []string `json:"options"`
	} `json:"questions"`
}

// traceSpanTypes 参与 trace 投影的事件类型（白名单）。
// 流式增量（content_chunk/reasoning_chunk）本就不落盘（见 logEventLine 过滤），
// 这里再挡一道：即便读到旧格式文件也不把它们当 span。
var traceSpanTypes = map[string]bool{
	"agent_start": true, "agent_end": true,
	"llm_start": true, "llm_end": true, "llm_error": true,
	"tool_run_start": true, "tool_run_end": true,
	"compress_start": true, "compress_end": true,
	"ask_user_question": true,
}

// parseTraceEvent 解析一行事件（只解码投影需要的字段；正文字段不进结构体）。
// 非事件行/坏行返回 false（与 buildSnapshotFromPath 同策略：跳过不致命）。
func parseTraceEvent(line string) (traceEvent, bool) {
	var e traceEvent
	if !strings.Contains(line, `"event_type"`) {
		return e, false
	}
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return e, false
	}
	if e.EventType == "" || !traceSpanTypes[e.EventType] {
		return e, false
	}
	return e, true
}

// millisFromRFC3339 解析事件时间戳为毫秒；缺失/非法返回 0（调用方按 0 降级）。
func millisFromRFC3339(s string) int64 {
	if s == "" {
		return 0
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		return 0
	}
	return t.UnixMilli()
}

// spanAcc 投影过程中的可变累加器（按 span 身份索引）。
type spanAcc struct {
	span     traceSpan
	startAbs int64 // 绝对开始毫秒（算相对偏移用）
	endAbs   int64
	hasStart bool
	hasEnd   bool
}

// projectTraces 把事件日志投影成 trace 列表（每个 root run 一条）。
// 返回的 traces 已按开始时间倒序（最近在上）。
func projectTraces(evPath, liveRoot string) []*traceDetail {
	f, err := os.Open(evPath)
	if err != nil {
		return nil
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	var evs []traceEvent
	for sc.Scan() {
		if e, ok := parseTraceEvent(sc.Text()); ok {
			evs = append(evs, e)
		}
	}
	if len(evs) == 0 {
		return nil
	}
	return buildTraces(evs, liveRoot)
}

// buildTraces 从有序事件流构建 trace 列表（纯函数，便于单测）。
func buildTraces(evs []traceEvent, liveRoot string) []*traceDetail {
	// ① run 父子关系（root = parent_run_id 为空）
	runParent := map[string]string{}
	runOrder := []string{} // 首次出现顺序（root 排序兜底）
	for _, e := range evs {
		if e.EventType == "agent_start" && e.RunID != "" {
			if _, seen := runParent[e.RunID]; !seen {
				runParent[e.RunID] = e.ParentID
				runOrder = append(runOrder, e.RunID)
			}
		}
	}
	if len(runParent) == 0 {
		return nil
	}
	kids := map[string][]string{}
	for _, rid := range runOrder {
		kids[runParent[rid]] = append(kids[runParent[rid]], rid)
	}

	// ② 事件按 run 归桶（保序）
	byRun := map[string][]traceEvent{}
	for _, e := range evs {
		if e.RunID == "" {
			continue
		}
		byRun[e.RunID] = append(byRun[e.RunID], e)
	}

	// ③ 每个 root 一条 trace：收敛子树事件 → 折叠 span
	var out []*traceDetail
	for _, root := range runOrder {
		if runParent[root] != "" {
			continue // 非 root：由父 trace 收敛
		}
		subtree := map[string]bool{}
		stack := []string{root}
		for len(stack) > 0 {
			x := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			if subtree[x] {
				continue
			}
			subtree[x] = true
			stack = append(stack, kids[x]...)
		}
		var runEvs []traceEvent
		for _, rid := range runOrder { // 保序：按 run 首次出现顺序拼接
			if subtree[rid] {
				runEvs = append(runEvs, byRun[rid]...)
			}
		}
		if d := buildTraceDetail(root, runEvs, liveRoot); d != nil {
			out = append(out, d)
		}
	}
	// 最近在上（start_at 相同则按 root 出现顺序的逆序，保持稳定）
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Summary.StartAt > out[j].Summary.StartAt
	})
	return out
}

// buildTraceDetail 把单个 trace 的事件折叠成 span 列表。
//
// liveRoot：**当前仍在运行**的 root run id（无则空）。语义关键 —— 「没有 end」有两种含义：
//   - 该 run 正在跑（agent_start 已发、agent_end 还没到）→ status=running
//   - 该 run 已中断/崩溃残留（会话已不在运行）→ status=interrupted
//
// 若不区分，运行中的链路会被显示成「未完成」，与「运行中」的实际状态矛盾。
// 前端 trace.ts:projectTrace 有同名参数，语义必须一致（交叉比对固定）。
func buildTraceDetail(root string, evs []traceEvent, liveRoot string) *traceDetail {
	if len(evs) == 0 {
		return nil
	}
	// 事件已按 run 分组拼接，可能不严格全局有序 → 按时间排序。
	// 缺时间戳的极旧事件：用「前一条的排序键」顺延（保持文件顺序相对关系），
	// 避免给它们编造时间，也避免 comparator 不可传递（返回 false 会破坏排序语义）。
	sortKey := make([]int64, len(evs))
	var carry int64
	for i, e := range evs {
		if t := millisFromRFC3339(e.Timestamp); t > 0 {
			carry = t
		}
		sortKey[i] = carry
	}
	idx := make([]int, len(evs))
	for i := range idx {
		idx[i] = i
	}
	sort.SliceStable(idx, func(a, b int) bool { return sortKey[idx[a]] < sortKey[idx[b]] })
	sorted := make([]traceEvent, len(evs))
	for i, j := range idx {
		sorted[i] = evs[j]
	}
	evs = sorted

	var (
		runs     = map[string]*spanAcc{}
		llms     = map[string]*spanAcc{}
		tools    = map[string]*spanAcc{}
		compress = map[string]*spanAcc{} // runID → 进行中的压缩
		order    []*spanAcc              // 保持首次出现顺序（同 start 时稳定）
	)
	push := func(a *spanAcc) { order = append(order, a) }

	// retries 归并到所属 run 最近一个 llm span（llm_error 无 request_id）
	pendingRetry := map[string]int{}

	for _, e := range evs {
		switch e.EventType {
		case "agent_start":
			if _, dup := runs[e.RunID]; dup {
				continue // 同 run 重复 start：幂等
			}
			a := &spanAcc{span: traceSpan{
				Kind: traceKindRun, RunID: e.RunID, ParentID: e.ParentID,
				Label: traceLabel(e.Name, e.Content), Name: e.Name,
				Status: "running", Depth: e.Depth, Model: e.Model,
			}, startAbs: millisFromRFC3339(e.Timestamp), hasStart: true}
			runs[e.RunID] = a
			push(a)

		case "agent_end":
			if a := runs[e.RunID]; a != nil {
				a.hasEnd = true
				a.endAbs = millisFromRFC3339(e.Timestamp)
				a.span.DurMs = e.DurMS
				a.span.Status = traceRunStatus(e)
				a.span.TaskID = e.TaskID
			}

		case "llm_start":
			key := e.RequestID
			if key == "" {
				key = e.RunID + "\x00" + e.Timestamp
			}
			if _, dup := llms[key]; dup {
				continue
			}
			a := &spanAcc{span: traceSpan{
				Kind: traceKindLLM, ID: e.RequestID, RunID: e.RunID,
				Model: e.Model, Status: "running", Label: "llm",
			}, startAbs: millisFromRFC3339(e.Timestamp), hasStart: true}
			llms[key] = a
			push(a)

		case "llm_end":
			key := e.RequestID
			if key == "" {
				key = e.RunID + "\x00" + e.Timestamp
			}
			a := llms[key]
			if a == nil {
				// 无 start 的 end（旧格式/截断）：补一个 start 态 span
				a = &spanAcc{span: traceSpan{
					Kind: traceKindLLM, ID: e.RequestID, RunID: e.RunID,
					Model: e.Model, Status: "done", Label: "llm",
				}, startAbs: millisFromRFC3339(e.Timestamp), hasStart: true}
				llms[key] = a
				push(a)
			}
			a.hasEnd = true
			a.endAbs = millisFromRFC3339(e.Timestamp)
			a.span.Status = "done"
			a.span.Model = e.Model
			a.span.Name = e.Model
			if e.Usage != nil {
				a.span.InTok = e.Usage.Input
				a.span.OutTok = e.Usage.Output
				a.span.CacheTok = e.Usage.CacheRead
				a.span.CacheWriteTok = e.Usage.CacheWrite
				a.span.ReasonTok = e.Usage.Reasoning
			}
			a.span.CostUSD = e.CostUSD
			// 该 run 的待定重试计数落到这个 llm span
			if n := pendingRetry[e.RunID]; n > 0 {
				a.span.Retries = n
				delete(pendingRetry, e.RunID)
			}

		case "llm_error":
			// 重试徽章：挂到本 run 的下一个 llm span；若上一个 llm 还没结算则挂它
			if n := pendingRetry[e.RunID]; n > 0 {
				pendingRetry[e.RunID] = n + 1
			} else {
				pendingRetry[e.RunID] = 1
			}

		case "tool_run_start":
			key := e.ToolID
			if key == "" {
				key = e.Name + "\x00" + e.Timestamp
			}
			if _, dup := tools[key]; dup {
				continue
			}
			a := &spanAcc{span: traceSpan{
				Kind: traceKindTool, ID: e.ToolID, RunID: e.RunID,
				Name: e.Name, Label: e.Name, Status: "running", TaskID: e.TaskID,
			}, startAbs: millisFromRFC3339(e.Timestamp), hasStart: true}
			tools[key] = a
			push(a)

		case "tool_run_end":
			key := e.ToolID
			if key == "" {
				key = e.Name + "\x00" + e.Timestamp
			}
			a := tools[key]
			if a == nil {
				a = &spanAcc{span: traceSpan{
					Kind: traceKindTool, ID: e.ToolID, RunID: e.RunID,
					Name: e.Name, Label: e.Name, Status: "done",
				}, startAbs: millisFromRFC3339(e.Timestamp), hasStart: true}
				tools[key] = a
				push(a)
			}
			a.hasEnd = true
			a.endAbs = millisFromRFC3339(e.Timestamp)
			a.span.IsError = e.IsError
			if e.IsError {
				a.span.Status = "error"
			} else {
				a.span.Status = "done"
			}
			a.span.TaskID = e.TaskID

		case "compress_start":
			a := &spanAcc{span: traceSpan{
				Kind: traceKindCompress, RunID: e.RunID, Status: "running",
				Label: "compress", Before: e.Before, Reason: e.Reason,
			}, startAbs: millisFromRFC3339(e.Timestamp), hasStart: true}
			compress[e.RunID] = a
			push(a)

		case "compress_end":
			a := compress[e.RunID]
			if a == nil {
				a = &spanAcc{span: traceSpan{
					Kind: traceKindCompress, RunID: e.RunID, Status: "done",
					Label: "compress",
				}}
				push(a)
			}
			a.hasEnd = true
			a.endAbs = millisFromRFC3339(e.Timestamp)
			a.span.Status = "done"
			a.span.Before = e.Before
			a.span.After = e.After
			a.span.CtxTokens = e.CtxTokens
			a.span.Model = e.Model
			if e.Error != "" {
				a.span.Status = "error"
			}
			delete(compress, e.RunID)

		case "ask_user_question":
			push(&spanAcc{span: traceSpan{
				Kind: traceKindWait, ID: e.ToolID, RunID: e.RunID, Status: "done",
				Label: "ask_user", WaitType: "ask_user", Questions: len(e.Questions),
			}, startAbs: millisFromRFC3339(e.Timestamp), hasStart: true})
		}
	}

	// ④ 结算未闭合的 start（中断/崩溃残留）→ interrupted
	// ⑤ 相对时间：以 trace 最早 start 为 0
	var zero int64
	for _, a := range order {
		if a.hasStart && a.startAbs > 0 && (zero == 0 || a.startAbs < zero) {
			zero = a.startAbs
		}
	}
	// 全无时间戳（极旧文件）：StartAt/DurMs 保持 0（不虚构时间），前端按顺序渲染

	spans := make([]traceSpan, 0, len(order))
	var endMax int64
	for _, a := range order {
		if a.hasStart && a.startAbs > 0 {
			a.span.StartAbs = a.startAbs
			a.span.StartAt = a.startAbs - zero
		} else {
			a.span.StartAt = 0
		}
		switch {
		case a.span.DurMs > 0:
			// 服务端已给（agent_end.duration_ms）
		case a.hasEnd && a.endAbs > 0 && a.startAbs > 0:
			a.span.DurMs = a.endAbs - a.startAbs
		case a.hasStart && !a.hasEnd:
			if a.span.Kind == traceKindRun || a.span.Kind == traceKindLLM ||
				a.span.Kind == traceKindTool || a.span.Kind == traceKindCompress {
				// 区分「正在跑」与「中断残留」：仅当这是活跃 root trace 时算 running。
				if liveRoot != "" && liveRoot == root {
					a.span.Status = "running"
				} else {
					a.span.Status = "interrupted"
				}
			}
		}
		if end := a.span.StartAt + a.span.DurMs; end > endMax {
			endMax = end
		}
		spans = append(spans, a.span)
	}

	sum := traceSummary{
		ID:     root,
		Status: "done",
		WallMs: endMax,
		Spans:  len(spans),
	}
	// Errors 语义 = 「出了几处问题」，不是「几个 span 状态为 error」：
	// run span 的 error 通常由子 span 失败引起（finish_reason=error），直接计数会把
	// 一次失败算两遍（run + tool）。故只数非 run 的失败 span；若 run 自身失败且无
	// 可归因的子失败（如 LLM 错误导致本轮终止），记 1 —— 保证「失败」至少显示 1。
	childErrors := 0
	for i := range spans {
		s := spans[i]
		switch s.Kind {
		case traceKindRun:
			if s.RunID == root {
				sum.Label = s.Label
				sum.Status = s.Status
				sum.StartAt = s.StartAbs
			} else {
				sum.Subs++
			}
		case traceKindLLM:
			sum.Turns++
			sum.InTok += s.InTok
			sum.OutTok += s.OutTok
			sum.CostUSD += s.CostUSD
			if s.Status == "error" {
				childErrors++
			}
		case traceKindTool:
			sum.Tools++
			if s.Status == "error" {
				childErrors++
			}
		default:
			if s.Status == "error" {
				childErrors++
			}
		}
	}
	sum.Errors = childErrors
	if sum.Errors == 0 && sum.Status == "error" {
		sum.Errors = 1
	}
	if sum.Label == "" {
		sum.Label = root
	}
	// 根 run 未闭合时，trace 状态跟根走（interrupted/running）
	if a := runs[root]; a != nil {
		sum.Status = a.span.Status
		if sum.StartAt == 0 && a.startAbs > 0 {
			sum.StartAt = a.startAbs
		}
	}
	// 元数据体积（前端展示存储账；与投影后的 wire 体积一致）
	if b, err := json.Marshal(spans); err == nil {
		sum.MetaBytes = int64(len(b))
	}
	return &traceDetail{Summary: sum, Spans: spans}
}

// traceRunStatus 由 agent_end 判定 run span 状态。
func traceRunStatus(e traceEvent) string {
	if e.Finish == "error" || e.Error != "" {
		return "error"
	}
	switch e.Finish {
	case "abort", "max_iterations":
		return "interrupted"
	}
	return "done"
}

// traceLabel 取展示标签：优先 name，空则取 content 首行（截断）。
func traceLabel(name, content string) string {
	if name != "" {
		return name
	}
	s := strings.TrimSpace(content)
	if i := strings.IndexAny(s, "\r\n"); i >= 0 {
		s = s[:i]
	}
	const max = 60
	if len(s) > max {
		// 按 rune 截断，避免切断多字节字符
		r := []rune(s)
		if len(r) > max {
			s = string(r[:max]) + "…"
		}
	}
	if s == "" {
		return "run"
	}
	return s
}

// traceListData 处理 `trace` 命令：不带 id → 摘要列表；带 id → 该 trace 详情。
// 数据源 = 会话事件日志（wsEventsDir）；无日志 → 空列表（前端回退现有 runNodes 渲染）。
func (m *manager) traceData(wsPath, sid, traceID string, limit int, wantLatest bool) map[string]any {
	out := map[string]any{"traces": []traceSummary{}, "found": false}
	if wsPath == "" || sid == "" {
		return out
	}
	// 会话若正在跑，其「无 agent_end 的 root run」应显示 running 而非 interrupted。
	// 判据取进程内会话的 IsRunning（权威）；会话不在进程内（冷会话）→ 空 = 一律按残留处理。
	// 注意：不能走 m.get()/m.runtime() —— 后者对未注册工作区会**懒初始化**
	//（构建 skills/subagent/MCP/插件注册表），而 trace 是只读查询，不该有此副作用；
	// 且裸 manager（单测）下 runtime 为 nil 会 panic。这里只做一次无副作用的查表。
	liveRoot := ""
	if m.workspaces != nil {
		m.mu.Lock()
		ws := m.workspaces[wsPath]
		m.mu.Unlock()
		if ws != nil {
			ws.mu.Lock()
			bs := ws.sessions[sid]
			ws.mu.Unlock()
			if bs != nil && bs.reducer != nil {
				liveRoot = bs.reducer.LiveRootRunID()
			}
		}
	}
	evPath := filepath.Join(wsEventsDir(wsPath), sid+".jsonl")
	details := projectTraces(evPath, liveRoot)
	if len(details) == 0 {
		return out
	}
	sums := make([]traceSummary, 0, len(details))
	for _, d := range details {
		sums = append(sums, d.Summary)
	}
	out["total"] = len(sums)
	if limit <= 0 {
		limit = traceDefaultLimit
	}
	if len(sums) > limit {
		sums = sums[:limit]
		out["has_more"] = true
	}
	out["traces"] = sums
	// latest=1：顺带回「最新一条 trace」的详情（列表按最近在上，故 = details[0]）。
	// 用途：前端刷新/切回会话后 traceEvents 为空（checkpoint 不含它），
	// 用它兜底回显链路，而不是误报「尚无运行」。仅列表请求附带，详情请求不必重复。
	if wantLatest && len(details) > 0 {
		out["latest"] = details[0]
	}

	if traceID != "" {
		for _, d := range details {
			if d.Summary.ID == traceID {
				out["found"] = true
				out["trace"] = d
				break
			}
		}
	}
	return out
}
