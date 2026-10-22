package subagent

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
)

// ---------- TaskOutput 重做（2026-10 用户裁定）----------
//
// 用户原话：「这个工具需要优化！很多不必要的信息；主智能体需要知道进度、需要知道输出的
// 文件，需要知道 output 内容，而不仅仅只是轮次；也不需要知道 Tokens 消耗。同时方便查看
// 子智能体会话 session 文件，自主决定查看哪些信息，按行查询」。
//
// 因此本轮把默认视图从「原始 journal 记录瀑布（含 usage 全字段）」换成
// 「进度 + 产出正文 + 产出文件 + 行号索引」，并把「看全文」变成 mode=lines 的按行查询。

// TestProducedFilesFromToolCalls 产出文件提取：只认写入类工具，参数坏/无路径键一律不算。
func TestProducedFilesFromToolCalls(t *testing.T) {
	calls := []core.ToolCall{
		{Name: "write_file", Arguments: `{"path":"/w/a.go","content":"x"}`},
		{Name: "edit_file", Arguments: `{"path":"/w/b.go"}`},
		{Name: "write_file", Arguments: `{"path":"/w/a.go"}`},        // 同一文件去重
		{Name: "read_file", Arguments: `{"path":"/w/read-only.go"}`}, // 只读不是产出
		{Name: "bash", Arguments: `{"command":"ls"}`},                // 非文件工具
		{Name: "write_file", Arguments: `not json`},                  // 参数损坏 → 跳过
		{Name: "write_file", Arguments: `{"content":"no path key"}`}, // 无路径 → 跳过
		{Name: "notebook_edit", Arguments: `{"notebook_path":"/w/c.ipynb"}`},
	}
	got := producedFiles(calls)
	want := []string{"/w/a.go", "/w/b.go", "/w/c.ipynb"}
	if len(got) != len(want) {
		t.Fatalf("产出文件 = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("产出文件 = %v, want %v（保序去重）", got, want)
		}
	}
	if pf := producedFiles(nil); pf != nil {
		t.Fatalf("无工具调用 → nil: %v", pf)
	}
}

// startProgressTask 起一个已结束的工具任务（journal 路径可控），返回 reg/tool/路径。
func startProgressTask(t *testing.T, name string) (*Registry, *taskOutputTool, string) {
	t.Helper()
	reg := NewRegistry()
	var jp string
	task, err := reg.StartToolWithOptions(context.Background(), StartOptions{
		Name: name,
		JournalPath: func(taskID string) (string, error) {
			jp = t.TempDir() + "/" + taskID + ".jsonl"
			return jp, nil
		},
	}, func(context.Context, *Task) (string, core.Usage, error) { return "r", core.Usage{}, nil })
	if err != nil {
		t.Fatal(err)
	}
	waitDone(t, task)
	return reg, &taskOutputTool{reg: reg}, jp
}

// TestTaskOutputProgressOutputAndFiles 默认视图直给「产出正文 + 产出文件 + 行号」，
// 且**不含** token/usage（账单口径不进工具结果）。
func TestTaskOutputProgressOutputAndFiles(t *testing.T) {
	reg, tool, jp := startProgressTask(t, "写代码")
	taskID := taskIDFromPath(jp)

	j, err := OpenTaskJournal(jp)
	if err != nil {
		t.Fatal(err)
	}
	_ = j.Append(JournalRecord{Type: "start", TaskId: taskID, Name: "写代码", Text: "改一下 auth"})
	_ = j.Append(JournalRecord{
		Type: "turn", TaskId: taskID, Round: 1, Text: "先读再改", Ts: time.Now(),
		ToolNames: []string{"read_file", "write_file"}, Files: []string{"/w/auth.go"},
		Usage: &core.Usage{Input: 1200, Output: 300},
	})
	_ = j.Append(JournalRecord{
		Type: "turn", TaskId: taskID, Round: 2, Text: "补测试", Ts: time.Now(),
		ToolNames: []string{"write_file"}, Files: []string{"/w/auth_test.go"},
	})
	_ = j.Append(JournalRecord{
		Type: "result", TaskId: taskID, Status: "completed",
		Text: "**改完了**：auth.go 加了 401 映射，补了 auth_test.go", Ts: time.Now(),
		Files: []string{"/w/auth.go", "/w/auth_test.go"},
	})
	_ = j.Close()

	res, err := tool.Call(context.Background(), "", `{"task_id":"`+taskID+`","mode":"all"}`)
	if err != nil {
		t.Fatal(err)
	}
	var v taskOutputView
	if err := json.Unmarshal([]byte(res), &v); err != nil {
		t.Fatalf("返回非 JSON: %v\n%s", err, res)
	}
	if !strings.Contains(v.Output, "改完了") {
		t.Errorf("output 应直给最终结论，got %q", v.Output)
	}
	if len(v.Files) != 2 || v.Files[0] != "/w/auth.go" {
		t.Errorf("files 应列出本次委派写出/修改的文件（累计去重），got %v", v.Files)
	}
	if v.Rounds != 2 || len(v.Recent) != 2 {
		t.Errorf("rounds/recent 应反映 2 轮，got rounds=%d recent=%d", v.Rounds, len(v.Recent))
	}
	if len(v.Recent[0].Files) != 1 || v.Recent[0].Files[0] != "/w/auth.go" {
		t.Errorf("每轮要带本轮产出的文件，got %v", v.Recent[0].Files)
	}
	if strings.Contains(v.Doing, "usage") {
		t.Errorf("doing 不该是记录原文，got %q", v.Doing)
	}
	if v.Lines == nil || v.Lines.Total != 4 {
		t.Errorf("应给出文件行号索引（4 行），got %+v", v.Lines)
	}
	if v.Recent[0].Line == 0 {
		t.Error("每轮要带物理行号（供 mode=lines 直接跳过去看）")
	}
	// 账单口径不得出现在工具结果里
	for _, noise := range []string{`"usage"`, `"Input":`, `"cost_usd"`, `"total_tokens"`, `"records"`} {
		if strings.Contains(res, noise) {
			t.Fatalf("TaskOutput 不应回 %s：%s", noise, res)
		}
	}
	_ = reg
}

// TestTaskOutputLinesMode 按行查询：给行号、给类型、给内容（截断），history 行只给预览。
func TestTaskOutputLinesMode(t *testing.T) {
	reg, tool, jp := startProgressTask(t, "按行查")
	taskID := taskIDFromPath(jp)

	history := `{"type":"history","messages":[{"role":"user","content":"` + strings.Repeat("长", 300) + `"}]}`
	lines := []string{
		`{"type":"start","task_id":"` + taskID + `","name":"按行查","text":"任务原始意图"}`,
		`{"type":"turn","round":1,"text":"第一轮正文","tool_names":["bash"],"files":["/w/x.go"],"usage":{"Input":9}}`,
		history,
		`{"type":"turn","round":2,"text":"第二轮正文","tool_names":["write_file"]}`,
		`{"type":"state","state":{"messages":[]}}`,
		`{"type":"result","status":"completed","text":"最终答复"}`,
	}
	if err := os.WriteFile(jp, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := tool.Call(context.Background(), "", `{"task_id":"`+taskID+`","mode":"lines","from_line":2,"to_line":3}`)
	if err != nil {
		t.Fatal(err)
	}
	var v taskOutputLinesView
	if err := json.Unmarshal([]byte(res), &v); err != nil {
		t.Fatalf("返回非 JSON: %v\n%s", err, res)
	}
	if v.From != 2 || v.To != 3 || v.Total != 6 {
		t.Fatalf("from/to/total 不对: %+v", v)
	}
	if len(v.Lines) != 2 {
		t.Fatalf("应给 2 行，got %d", len(v.Lines))
	}
	if v.Lines[0].Type != "turn" || v.Lines[0].Round != 1 || v.Lines[0].Text != "第一轮正文" {
		t.Errorf("turn 行应结构化返回: %+v", v.Lines[0])
	}
	if len(v.Lines[0].Tools) != 1 || v.Lines[0].Files[0] != "/w/x.go" {
		t.Errorf("turn 行应带工具名与产出文件: %+v", v.Lines[0])
	}
	if v.Lines[1].Type != "history" || v.Lines[1].Preview == "" {
		t.Errorf("history 行只给预览（不整行展开全量消息）: %+v", v.Lines[1])
	}
	if v.Lines[1].Bytes == 0 {
		t.Error("应给出该行字节数（判断值不值得整行取）")
	}
	if strings.Contains(v.Lines[0].Preview, "usage") || strings.Contains(res, `"usage"`) {
		t.Fatalf("行内容不应回 usage（token 不进工具输出）: %s", res)
	}

	// 每行截断：max_line_chars=80 时 history 预览不超过 80 rune
	res2, err := tool.Call(context.Background(), "", `{"task_id":"`+taskID+`","mode":"lines","from_line":3,"to_line":3,"max_line_chars":80}`)
	if err != nil {
		t.Fatal(err)
	}
	var v2 taskOutputLinesView
	if err := json.Unmarshal([]byte(res2), &v2); err != nil {
		t.Fatal(err)
	}
	if n := len([]rune(v2.Lines[0].Preview)); n > 80 {
		t.Fatalf("预览应截断到 80 rune，got %d", n)
	}
	if !v2.Lines[0].Truncated {
		t.Error("截断应如实标注 truncated")
	}

	// 未给 from_line → 从「最近一轮」那一行开始（=第 4 行）
	res3, err := tool.Call(context.Background(), "", `{"task_id":"`+taskID+`","mode":"lines"}`)
	if err != nil {
		t.Fatal(err)
	}
	var v3 taskOutputLinesView
	if err := json.Unmarshal([]byte(res3), &v3); err != nil {
		t.Fatal(err)
	}
	if v3.From != 4 {
		t.Fatalf("未给 from_line 应从最近一轮开始（4），got %d", v3.From)
	}
	_ = reg
}

// TestTaskOutputLinesModeCapsAndValidation 单次行数上限 + 参数校验。
func TestTaskOutputLinesModeCapsAndValidation(t *testing.T) {
	reg, tool, jp := startProgressTask(t, "上限")
	taskID := taskIDFromPath(jp)
	var b strings.Builder
	for i := 1; i <= 500; i++ {
		b.WriteString(`{"type":"turn","round":` + itoa(i) + `,"text":"t"}` + "\n")
	}
	if err := os.WriteFile(jp, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}

	res, err := tool.Call(context.Background(), "", `{"task_id":"`+taskID+`","mode":"lines","from_line":1,"to_line":400}`)
	if err != nil {
		t.Fatal(err)
	}
	var v taskOutputLinesView
	if err := json.Unmarshal([]byte(res), &v); err != nil {
		t.Fatal(err)
	}
	if len(v.Lines) != LineRangeCap {
		t.Fatalf("单次最多 %d 行，got %d", LineRangeCap, len(v.Lines))
	}
	if v.Total != 500 {
		t.Fatalf("total_lines 应为全文件行数 500，got %d", v.Total)
	}

	for _, bad := range []string{
		`{"task_id":"` + taskID + `","mode":"lines","from_line":-1}`,
		`{"task_id":"` + taskID + `","mode":"lines","from_line":9,"to_line":3}`,
		`{"task_id":"` + taskID + `","mode":"lines","max_line_chars":5}`,
	} {
		if err := tool.ValidParams(context.Background(), "", bad); err == nil {
			t.Errorf("应拒绝非法参数: %s", bad)
		}
	}
	if err := tool.ValidParams(context.Background(), "", `{"task_id":"`+taskID+`","mode":"lines","from_line":3,"to_line":9}`); err != nil {
		t.Errorf("合法行区间应通过: %v", err)
	}
	_ = reg
}

// TestTaskOutputLinesModeNoFile 无落盘的任务：按行查给出下一步提示（mode=status），不是裸 IO 错误。
func TestTaskOutputLinesModeNoFile(t *testing.T) {
	reg := NewRegistry()
	release := make(chan struct{})
	task, err := reg.StartToolWithOptions(context.Background(), StartOptions{Name: "nofile"},
		func(context.Context, *Task) (string, core.Usage, error) {
			<-release
			return "r", core.Usage{}, nil
		})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { close(release); <-task.done }()

	tool := &taskOutputTool{reg: reg}
	if _, err := tool.Call(context.Background(), "", `{"task_id":"`+task.ID+`","mode":"lines"}`); err == nil ||
		!strings.Contains(err.Error(), "mode=status") {
		t.Fatalf("无 session 文件应提示 mode=status: %v", err)
	}
}

// 小工具：从 journal 路径反推 task id（文件名去掉扩展名）。
func taskIDFromPath(jp string) string {
	if i := strings.LastIndexByte(jp, '/'); i >= 0 {
		jp = jp[i+1:]
	}
	return strings.TrimSuffix(jp, ".jsonl")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [8]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
