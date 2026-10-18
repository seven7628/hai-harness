package main

// agent_history 分页读取的回归测试。
//
// 这条通道是「checkpoint 只留首屏、全量按需加载」方案的数据面，取代了原先
// 「只留 50 条」的截断做法（用户明确要求能看全量历史）。它必须保证：
//  1. 从 journal 的 state 行取出的是**完整**历史（含 tool args/result），不是有损的 turn 行；
//  2. 分页游标正确 —— 翻完所有页能恰好覆盖全量、不重不漏；
//  3. 大文件从尾部读，不随 journal 体积线性变慢；
//  4. 路径校验拒绝穿越（安全边界）。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeJournal 造一个子任务 journal：多轮 state 行，最后一条含完整 messages。
func writeJournal(t *testing.T, dir, taskID string, msgCount int) string {
	t.Helper()
	path := filepath.Join(dir, taskID+".jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	// 早期轮次：内容少（模拟 journal 逐步累积）
	for round := 1; round <= 3; round++ {
		msgs := buildMessages(t, round*2)
		writeLine(t, f, map[string]any{"type": "state", "ts": time.Now().Format(time.RFC3339),
			"task_id": taskID, "state": map[string]any{"messages": msgs}})
	}
	// 最后一条：完整历史
	msgs := buildMessages(t, msgCount)
	writeLine(t, f, map[string]any{"type": "state", "ts": time.Now().Format(time.RFC3339),
		"task_id": taskID, "state": map[string]any{"messages": msgs}})
	return path
}

func writeLine(t *testing.T, f *os.File, v any) {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		t.Fatal(err)
	}
}

// buildMessages 造 n 轮「assistant 发起 tool_call + tool 结果」的消息序列。
func buildMessages(t *testing.T, n int) []map[string]any {
	t.Helper()
	out := []map[string]any{
		{"role": "user", "content": "任务原文"},
	}
	for i := 0; i < n; i++ {
		callID := fmt.Sprintf("call-%d", i)
		out = append(out, map[string]any{
			"role":    "assistant",
			"content": fmt.Sprintf("第 %d 步思考", i),
			"tool_calls": []map[string]any{
				{"id": callID, "type": "function", "function": map[string]any{
					"name": "read_file", "arguments": fmt.Sprintf(`{"path":"doc-%d.md"}`, i),
				}},
			},
		})
		out = append(out, map[string]any{
			"role":         "tool",
			"tool_call_id": callID,
			"content":      fmt.Sprintf("文件 %d 的内容", i),
		})
	}
	return out
}

// 分页必须不重不漏地覆盖全量，且条目内容完整（tool 的 args 与 result 都在）。
func TestAgentHistoryPaginationCoversAll(t *testing.T) {
	dir := t.TempDir()
	path := writeJournal(t, dir, "task-abc123", 40) // 40 轮 → 1 user + 40 assistant + 40 tool = 81 条

	first, err := readAgentHistoryPage(path, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	if first.Total != 81 {
		t.Fatalf("total 应为 81（1 user + 40 assistant + 40 tool），得到 %d", first.Total)
	}
	if len(first.Entries) != 20 {
		t.Fatalf("首页应有 20 条，得到 %d", len(first.Entries))
	}
	if !first.HasMore {
		t.Fatal("还有更早记录时 has_more 应为 true")
	}

	// 翻完所有页，收集全部 Index，验证无重无漏。
	seen := map[int]bool{}
	total := first.Total
	for offset := 0; offset < total; offset += 20 {
		page, err := readAgentHistoryPage(path, offset, 20)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range page.Entries {
			if seen[e.Index] {
				t.Fatalf("index %d 重复出现", e.Index)
			}
			seen[e.Index] = true
		}
	}
	if len(seen) != total {
		t.Fatalf("翻页覆盖 %d 条，全量 %d 条 —— 有遗漏", len(seen), total)
	}

	// 内容完整性：抽查一条 tool 条目
	var toolEntry *agentHistoryEntry
	for _, e := range first.Entries {
		if e.Kind == "tool" {
			toolEntry = &e
			break
		}
	}
	if toolEntry == nil {
		t.Fatal("首页里应有 tool 条目")
	}
	if toolEntry.Args == "" {
		t.Fatal("tool 条目的 args 丢失")
	}
	if toolEntry.Result == "" {
		t.Fatal("tool 条目的 result 丢失（分页必须给全量，不能像 journal turn 行那样截断）")
	}
	if !strings.Contains(toolEntry.Result, "的内容") {
		t.Fatalf("tool result 内容不对: %q", toolEntry.Result)
	}
}

// 尾部读取必须命中**最后一条** state 行（更早的轮次内容不全）。
func TestAgentHistoryReadsLatestState(t *testing.T) {
	dir := t.TempDir()
	path := writeJournal(t, dir, "task-latest", 30)
	page, err := readAgentHistoryPage(path, 0, 500)
	if err != nil {
		t.Fatal(err)
	}
	// 最后一条 state 有 30 轮 → 61 条；早期轮次只有 2/4/6 轮，若读错会明显更少。
	if page.Total != 61 {
		t.Fatalf("应读到最后一轮 state 的 61 条，得到 %d（可能读到了早期轮次）", page.Total)
	}
}

// offset 超过总量时返回空页而不是报错（UI 边界情况）。
func TestAgentHistoryOffsetBeyondTotal(t *testing.T) {
	dir := t.TempDir()
	path := writeJournal(t, dir, "task-edge", 5)
	page, err := readAgentHistoryPage(path, 9999, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) != 0 {
		t.Fatalf("offset 超界应返回空页，得到 %d 条", len(page.Entries))
	}
	if page.HasMore {
		t.Fatal("超界后 has_more 应为 false")
	}
}

// 单页上限必须生效（防止 UI 传个大 limit 把 stdout 打爆）。
func TestAgentHistoryClampsLimit(t *testing.T) {
	dir := t.TempDir()
	path := writeJournal(t, dir, "task-clamp", 400)
	page, err := readAgentHistoryPage(path, 0, 99999)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Entries) > maxAgentHistoryPage {
		t.Fatalf("单页不应超过 %d 条，得到 %d", maxAgentHistoryPage, len(page.Entries))
	}
}

// 尾部扫描必须能处理「state 行比读取块还大」的情况（长上下文任务）。
func TestAgentHistoryLargeStateLine(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "task-big.jsonl")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	// 造一条 >1MB 的 state 行（单条 result 放大）
	big := strings.Repeat("x", 2<<20)
	msgs := []map[string]any{
		{"role": "user", "content": "大任务"},
		{"role": "assistant", "content": "思考", "tool_calls": []map[string]any{
			{"id": "c1", "type": "function", "function": map[string]any{
				"name": "bash", "arguments": `{"command":"ls"}`}},
		}},
		{"role": "tool", "tool_call_id": "c1", "content": big},
	}
	writeLine(t, f, map[string]any{"type": "state", "state": map[string]any{"messages": msgs}})
	f.Close()

	page, err := readAgentHistoryPage(path, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if page.Total != 3 {
		t.Fatalf("应解析出 3 条，得到 %d", page.Total)
	}
	var toolEntry *agentHistoryEntry
	for _, e := range page.Entries {
		if e.Kind == "tool" {
			toolEntry = &e
		}
	}
	if toolEntry == nil || len(toolEntry.Result) != len(big) {
		t.Fatal("超大 state 行的 tool result 未完整读出")
	}
}

// journal 不存在时返回 os.ErrNotExist（命令层据此返回空页，而不是报错）。
func TestAgentHistoryMissingJournal(t *testing.T) {
	_, err := readAgentHistoryPage(filepath.Join(t.TempDir(), "nope.jsonl"), 0, 20)
	if !os.IsNotExist(err) {
		t.Fatalf("应返回 not exist，得到 %v", err)
	}
}

// 路径校验：非法 taskId 必须被拒绝（防路径穿越）。
func TestJournalPathRejectsTraversal(t *testing.T) {
	dir := t.TempDir()
	for _, bad := range []string{"../../etc/passwd", "..", "a/b", "", "task-ok/../../x"} {
		if _, err := journalPathFor(dir, "s1", bad); err == nil {
			t.Fatalf("taskId %q 应被拒绝", bad)
		}
	}
}

// 真实 core.ToolCall 是**扁平**结构（name/arguments 直接在 tool_call 上），
// 与 OpenAI 的嵌套 function.name 不同。这条钉住两种形态都能解析 ——
// 曾经只认嵌套形态，导致真实数据里 tool 的 name/args 全是空。
func TestAgentItemsBothToolCallShapes(t *testing.T) {
	mk := func(tc map[string]any) []json.RawMessage {
		msgs := []map[string]any{
			{"role": "assistant", "content": "思考", "tool_calls": []map[string]any{tc}},
			{"role": "tool", "tool_call_id": tc["id"], "content": "结果"},
		}
		out := make([]json.RawMessage, 0, len(msgs))
		for _, m := range msgs {
			b, _ := json.Marshal(m)
			out = append(out, b)
		}
		return out
	}

	flat := mk(map[string]any{
		"id": "c1", "name": "read_file", "arguments": `{"path":"a.md"}`,
	})
	nested := mk(map[string]any{
		"id": "c2", "type": "function", "function": map[string]any{
			"name": "bash", "arguments": `{"command":"ls"}`,
		},
	})

	for name, msgs := range map[string][]json.RawMessage{"扁平(core.ToolCall)": flat, "嵌套(OpenAI)": nested} {
		entries := agentItemsFromMessages(msgs)
		var tool *agentHistoryEntry
		for i := range entries {
			if entries[i].Kind == "tool" {
				tool = &entries[i]
			}
		}
		if tool == nil {
			t.Fatalf("%s: 没解析出 tool 条目", name)
		}
		if tool.Name == "" {
			t.Fatalf("%s: tool name 为空（形态未识别）", name)
		}
		if tool.Args == "" {
			t.Fatalf("%s: tool args 为空（形态未识别）", name)
		}
		if tool.Result == "" {
			t.Fatalf("%s: tool result 未回填", name)
		}
	}
}
