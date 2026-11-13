package main

// P4' 的**验收门**：从事件日志重放重建的视图，与 sidecar 里存的视图
// 逐字节一致吗？（2026-11-11，CHECKPOINT_V3_DESIGN §6 P4）
//
// ## 为什么这是「门」
//
// P4'（内容轻量快照 + 事件索引）的方案是：sidecar 不再存正文，只存派生状态
// + 指向事件日志的指针；正文在打开时从事件日志重放重建。
//
// **成立条件只有一个**：重建出来的块必须与当初存进去的块**逐字节相同**。
// 否则刷新一次，用户看到的会话就和刷新前不一样 —— 那不是优化，是数据损坏。
//
// 所以先量这件事，再决定动不动结构。
//
// ## 量什么
//
//   · 块数一致率；
//   · 逐字节相同的块数；
//   · 不一致的块：按 kind 归类 + 第一个不同的字段（定位语义缺口）。
//
// ## 跑法
//
//	HAI_LAB_EVENTS=$HOME/.go-code/events/<ws>/<sid>.jsonl \
//	HAI_LAB_RECORD=$HOME/.go-code/sessions/<ws>/<sid>.record.json \
//	go test ./desktop/bridge/ -run TestProjectionEquivalence -v -timeout 30m

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/events"
)

// TestProjectionEquivalence 重放事件日志，与 sidecar 存的块逐字节比对。
func TestProjectionEquivalence(t *testing.T) {
	assertNoUserPrices(t)
	eventsPath := os.Getenv("HAI_LAB_EVENTS")
	recordPath := os.Getenv("HAI_LAB_RECORD")
	if eventsPath == "" || recordPath == "" {
		t.Skip("需要 HAI_LAB_EVENTS + HAI_LAB_RECORD")
	}
	provider := os.Getenv("HAI_LAB_PROVIDER")

	// ① 磁盘上的真值：record 引用 + sidecar 的块字节。
	store, err := newRecordStore()
	if err != nil {
		t.Fatal(err)
	}
	wsKey := wsKeyFromRecordPath(t, recordPath)
	sid := sidFromRecordPath(t, recordPath)
	rec, err := store.LoadRecord(context.Background(), wsKey, sid)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.LoadedBlockSpans) == 0 {
		t.Fatal("record 没有解析出块")
	}
	// **按内容键对齐，不按 id**。
	//
	// 为什么：block id **不可从事件重建** —— live 的 reducer 给被删掉的块
	// （filterBlocks 清掉的 error 块等）也消耗了 id ⇒ id 空间是稀疏的
	// （实测 38,198 块但最大 id 41,781 ⇒ 3,583 个 id 空洞）。
	// 重放时 reducer 从 1 连续编号 ⇒ 同一 id 在两边**指向不同的块**
	// （实测工具块的 args 互相串位）。按 id 比对会得出完全误导的结论。
	//
	// 内容键：tool_id / request_id / run_id（块的自然身份）→ 都���有则退化到 id。
	stored := map[string][]byte{}
	for _, span := range rec.LoadedBlockSpans {
		k := blockKey(span)
		if _, dup := stored[k]; dup {
			continue
		}
		stored[k] = span
	}
	t.Logf("磁盘真值：%d 块（%d 个唯一内容键）", len(rec.LoadedBlockSpans), len(stored))

	// ② 重放事件日志（meta 规则与 observe() 一致）。
	//
	// **必须按 record 的提交时刻截断**：会话是活的 → 事件日志比 record 新
	// （实测重放多出 3,243 块）→ 不截断就是拿两个不同时刻的视图对比。
	// record.UpdatedAt = 提交时刻；事件的 timestamp = 事件发生时刻 ⇒ 以它为界。
	cutoff := rec.UpdatedAt.Add(2 * time.Second)
	t.Logf("截断时刻: %s", cutoff.Format(time.RFC3339Nano))
	red := NewViewReducer()
	f, err := os.Open(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), maxEventLineBytes)
	var applied, skipped int
	start := time.Now()
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		ev, perr := parseLegacyEvent(line)
		if perr != nil || ev == nil {
			skipped++
			continue
		}
		if evTimestamp(ev).After(cutoff) {
			skipped++
			continue
		}
		_ = red.Apply(ev, projectionMeta(provider, ev))
		applied++
	}
	if serr := sc.Err(); serr != nil {
		t.Fatalf("扫描失败: %v", serr)
	}
	t.Logf("重放：%d 条（跳过 %d），%v", applied, skipped, time.Since(start).Round(time.Millisecond))

	// ③ 重放出来的块。
	spans, err := red.MarshalBodyWithSpans()
	if err != nil {
		t.Fatal(err)
	}
	replayed := map[string][]byte{}
	for _, span := range spans.Spans {
		k := blockKey(span)
		if _, dup := replayed[k]; dup {
			continue
		}
		replayed[k] = span
	}
	t.Logf("重放结果：%d 块（%d 个唯一内容键）", len(spans.Spans), len(replayed))

	// ④ 比对。
	var same, sameNoID, sameContentOnly, diff, missing int
	diffKinds := map[string]int{}
	diffFields := map[string]int{}
	examples := map[string]string{}
	var argsExamples []string
	var durExamples []string
	var itemsExamples []string
	var runExamples []string
	keys := make([]string, 0, len(stored))
	for k := range stored {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, id := range keys {
		want := stored[id]
		got, ok := replayed[id]
		if !ok {
			missing++
			continue
		}
		if string(want) == string(got) {
			same++
			continue
		}
		// **排除 id 字段**再比一次：id 本来就该由快照携带（它不可从事件重建），
		// 所以「除 id 外逐字节相同」才是 P4' 的真正判据。
		if sameExceptID(want, got) {
			sameNoID++
			continue
		}
		// **只比内容字段** —— 这才是 P4' 的判据：派生状态（id/计时/token/状态）
		// 由快照原样携带（它本来就不可从事件重建），需要重建的只有**内容**。
		if sameContent(want, got) {
			sameContentOnly++
			continue
		}
		diff++
		kind, fields := allBlockDifferences(want, got)
		diffKinds[kind]++
		for _, f := range fields {
			diffFields[f]++
		}
		if _, seen := examples[kind]; !seen {
			examples[kind] = idFieldSummary(want, got, fields[0])
		}
		for _, f := range fields {
			if f == "run_id" && len(runExamples) < 3 {
				runExamples = append(runExamples, kind+" "+idFieldSummary(want, got, f))
			}
			if f == "dur_ms" && len(durExamples) < 3 {
				durExamples = append(durExamples, kind+" "+idFieldSummary(want, got, f))
			}
			if f == "items" && len(itemsExamples) < 2 {
				itemsExamples = append(itemsExamples, kind+" "+itemsDiff(want, got))
			}
		}
	}
	t.Logf("=== 结果 ===")
	t.Logf("逐字节相同: %d", same)
	t.Logf("除 id 外相同: %d", sameNoID)
	t.Logf("仅内容字段相同: %d  ← P4' 的真正判据（派生状态由快照携带）", sameContentOnly)
	t.Logf("不同      : %d", diff)
	t.Logf("重放里没有: %d", missing)
	if len(diffKinds) > 0 {
		t.Logf("不同的 kind 分布: %v", diffKinds)
		t.Logf("差异字段统计（全部字段，不只是第一个）: %v", diffFields)
		for k, v := range examples {
			t.Logf("  [%s] %s", k, v)
		}
		for i, v := range runExamples {
			t.Logf("  [run_id #%d] %s", i, v)
		}
		for i, v := range itemsExamples {
			t.Logf("  [items #%d] %s", i, v)
		}
		for i, v := range durExamples {
			t.Logf("  [dur_ms #%d] %s", i, v)
		}
		for i, v := range argsExamples {
			t.Logf("  [args #%d] %s", i, v)
		}
	}
}

// itemsDiff 定位两个 agent 卡的 items 数组里第一处不同（结构化）。
func itemsDiff(a, b []byte) string {
	var ma, mb struct {
		Items []map[string]json.RawMessage `json:"items"`
	}
	if err := json.Unmarshal(a, &ma); err != nil {
		return "<解析失败>"
	}
	if err := json.Unmarshal(b, &mb); err != nil {
		return "<解析失败>"
	}
	if len(ma.Items) != len(mb.Items) {
		return fmt.Sprintf("items 条数不同：存=%d 重放=%d", len(ma.Items), len(mb.Items))
	}
	for i := range ma.Items {
		ia, ib := ma.Items[i], mb.Items[i]
		keys := map[string]bool{}
		for k := range ia {
			keys[k] = true
		}
		for k := range ib {
			keys[k] = true
		}
		names := make([]string, 0, len(keys))
		for k := range keys {
			names = append(names, k)
		}
		sort.Strings(names)
		for _, k := range names {
			va, oka := ia[k]
			vb, okb := ib[k]
			if !oka || !okb || string(va) != string(vb) {
				return fmt.Sprintf("第 %d 条的 %s 不同：存=%s 重放=%s", i, k, trunc(va), trunc(vb))
			}
		}
	}
	return "items 逐条相同（差异在别处）"
}

func trunc(v json.RawMessage) string {
	if len(v) > 90 {
		return string(v[:90]) + "…"
	}
	return string(v)
}

// contentFields 是**内容**字段（= sidecar 里占体积的大头，也是 P4' 要从
// 事件日志重建的那些）。其余字段是派生状态（id/状态/计时/token/归属）——
// 它们由快照原样携带，不需要重建。
var contentFields = []string{
	"text", "thinking", "result", "args", "contents", "images", "items",
	"nested_calls", "diff", "task_result", "task_error", "summary", "artifacts",
	"todos", "analysis", "host_ops", "label",
}

// sameContent 比较两个块的**内容字段**是否逐字节相同。
func sameContent(a, b []byte) bool {
	var ma, mb map[string]json.RawMessage
	if err := json.Unmarshal(a, &ma); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &mb); err != nil {
		return false
	}
	for _, k := range contentFields {
		va, oka := ma[k]
		vb, okb := mb[k]
		if !oka && !okb {
			continue
		}
		if !oka || !okb || string(va) != string(vb) {
			return false
		}
	}
	return true
}

// sameExceptID 比较两个块（忽略 id 字段）是否逐字节相同。
func sameExceptID(a, b []byte) bool {
	var ma, mb map[string]json.RawMessage
	if err := json.Unmarshal(a, &ma); err != nil {
		return false
	}
	if err := json.Unmarshal(b, &mb); err != nil {
		return false
	}
	delete(ma, "id")
	delete(mb, "id")
	ja, err1 := json.Marshal(ma)
	jb, err2 := json.Marshal(mb)
	if err1 != nil || err2 != nil {
		return false
	}
	return string(ja) == string(jb)
}

// blockKey 是块的自然身份（用于跨两侧对齐）。
//
// 优先用 tool_id / request_id / run_id —— 它们是**块自己的**标识，
// 与「谁分配了 id」无关 ⇒ 重建出来的块只要内容对就能对上。
// 都没有的（user/divider/system…）退化到 id（这类块少，且多不带内容）。
func blockKey(b []byte) string {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return "?"
	}
	kind := string(m["kind"])
	for _, k := range []string{"tool_id", "request_id", "run_id", "task_id", "id"} {
		v, ok := m[k]
		if !ok || string(v) == "null" || string(v) == `""` {
			continue
		}
		if k == "id" {
			return kind + "|id=" + string(v)
		}
		return kind + "|" + k + "=" + string(v)
	}
	return kind + "|?"
}

// evTimestamp 取事件的时间戳（零值的事件无法参与截断判定）。
func evTimestamp(ev events.Event) time.Time {
	switch e := ev.(type) {
	case *events.ContentChunk:
		return e.Timestamp
	case *events.ReasoningChunk:
		return e.Timestamp
	case *events.LLMStart:
		return e.Timestamp
	case *events.LLMEnd:
		return e.Timestamp
	case *events.LLMError:
		return e.Timestamp
	case *events.ToolStart:
		return e.Timestamp
	case *events.ToolResponse:
		return e.Timestamp
	case *events.ToolApprovalRequested:
		return e.Timestamp
	case *events.TaskStarted:
		return e.Timestamp
	case *events.TaskEnd:
		return e.Timestamp
	case *events.TaskResultDelivered:
		return e.Timestamp
	case *events.TaskPromoted:
		return e.Timestamp
	case *events.AgentStart:
		return e.Timestamp
	case *events.AgentEnd:
		return e.Timestamp
	case *events.AgentUserMessage:
		return e.Timestamp
	case *events.CompressStart:
		return e.Timestamp
	case *events.CompressEnd:
		return e.Timestamp
	case *events.CommandResult:
		return e.Timestamp
	case *events.AskUserQuestion:
		return e.Timestamp
	case *events.UserInputsConsumed:
		return e.Timestamp
	case *events.SessionRunError:
		return e.Timestamp
	case *events.ToolHookWarning:
		return e.Timestamp
	case *events.RefsLoaded:
		return e.Timestamp
	}
	return time.Time{}
}

// projectionMeta 复刻 observe() 的 meta 注入规则（重放时唯一能补的东西）。
func projectionMeta(provider string, ev events.Event) EventMeta {
	meta := EventMeta{Provider: provider}
	switch e := ev.(type) {
	case *events.LLMEnd:
		if e.Usage != nil {
			meta.CostUSD = costUsd(provider, e.Model, *e.Usage)
		}
	case *events.ToolResponse:
		if todoResponseCarriesSnapshot(e) && e.Todos != nil {
			meta.Todos = todoItemsFromSnapshots(e.Todos)
		}
	case *events.CompressEnd:
		if e.Todos != nil {
			meta.Todos = todoItemsFromSnapshots(e.Todos)
		}
	}
	return meta
}

// todoItemsFromSnapshots 把事件里的待办快照转成展示副本（与 todosOf 同构）。
func todoItemsFromSnapshots(snap []events.TodoSnapshot) []TodoItem {
	out := make([]TodoItem, 0, len(snap))
	for _, it := range snap {
		out = append(out, TodoItem{
			ID: it.Id, Title: it.Title, Done: it.Done,
			BlockedBy: append([]string{}, it.BlockedBy...),
		})
	}
	return out
}

// allBlockDifferences 返回（kind, 全部不同的字段）。
//
// 为什么不是「第一个」：字段是按字母序比对的 ⇒ `ctx_window` 会把 `items`
// 的差异遮住 —— 而 items 恰恰是最贵的那一项（占 sidecar 31.5%）。
func allBlockDifferences(a, b []byte) (string, []string) {
	var ma, mb map[string]json.RawMessage
	if err := json.Unmarshal(a, &ma); err != nil {
		return "?", []string{"<解析失败>"}
	}
	if err := json.Unmarshal(b, &mb); err != nil {
		return "?", []string{"<解析失败>"}
	}
	kind := string(ma["kind"])
	keys := map[string]bool{}
	for k := range ma {
		keys[k] = true
	}
	for k := range mb {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []string
	for _, k := range names {
		va, oka := ma[k]
		vb, okb := mb[k]
		if !oka || !okb || string(va) != string(vb) {
			out = append(out, k)
		}
	}
	return kind, out
}

// firstBlockDifference 返回（kind, 第一个不同的字段）。
func firstBlockDifference(a, b []byte) (string, string) {
	var ma, mb map[string]json.RawMessage
	if err := json.Unmarshal(a, &ma); err != nil {
		return "?", "<解析失败>"
	}
	if err := json.Unmarshal(b, &mb); err != nil {
		return "?", "<解析失败>"
	}
	kind := string(ma["kind"])
	keys := map[string]bool{}
	for k := range ma {
		keys[k] = true
	}
	for k := range mb {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, k := range names {
		va, oka := ma[k]
		vb, okb := mb[k]
		if !oka || !okb || string(va) != string(vb) {
			return kind, k
		}
	}
	return kind, "?"
}

// idFieldSummary 给出不一致块的简要说明。
func idFieldSummary(a, b []byte, field string) string {
	var ma, mb map[string]any
	_ = json.Unmarshal(a, &ma)
	_ = json.Unmarshal(b, &mb)
	sa, _ := json.Marshal(ma[field])
	sb, _ := json.Marshal(mb[field])
	trunc := func(s []byte) string {
		if len(s) > 120 {
			return string(s[:120]) + "…"
		}
		return string(s)
	}
	return "字段 " + field + "：存=" + trunc(sa) + " 重放=" + trunc(sb)
}

// wsKeyFromRecordPath 从 record 路径推出工作区 key。
func wsKeyFromRecordPath(t *testing.T, p string) string {
	t.Helper()
	parts := strings.Split(p, string(os.PathSeparator))
	if len(parts) < 2 {
		t.Fatalf("路径太短: %s", p)
	}
	return parts[len(parts)-2]
}

// sidFromRecordPath 从 record 路径推出会话 id。
func sidFromRecordPath(t *testing.T, p string) string {
	t.Helper()
	base := filepathBase(p)
	return strings.TrimSuffix(base, ".record.json")
}

func filepathBase(p string) string {
	i := strings.LastIndex(p, string(os.PathSeparator))
	if i < 0 {
		return p
	}
	return p[i+1:]
}
