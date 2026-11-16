package main

// Dream 运行记录详情（dream_detail）的测试：字段往返、体积上限、命令三态。
//
// 详情视图看的三样东西都属于**记录文件本身**：收尾正文全文、工具时间线、
// 逐库台账。它们比列表摘要大一个量级，因此每条测试都同时盯"内容对不对"
// 和"体积有没有被收住"。

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/events"
)

// dreamToolEvent 造一条工具结束事件（台账/时间线的输入）。
func dreamToolEvent(name, arguments, result string) *events.ToolResponse {
	return &events.ToolResponse{
		Name:      name,
		Arguments: arguments,
		Result:    result,
		Timestamp: time.Date(2026, 11, 15, 22, 29, 14, 0, time.Local),
	}
}

// dreamToolFail 造一条失败的工具结束事件（宿主工具失败恒带 IsError）。
func dreamToolFail(name, arguments, result string) *events.ToolResponse {
	ev := dreamToolEvent(name, arguments, result)
	ev.IsError = true
	return ev
}

// TestDreamRunDetailRoundTrip result/steps 落盘后能原样读回。
//
// 关键是 result 的**换行**：摘要压成一行，正文必须保留结构，否则详情视图里
// 模型写的分点/表格会糊成一段。
func TestDreamRunDetailRoundTrip(t *testing.T) {
	dir := withDreamDir(t)
	start := time.Date(2026, 11, 15, 22, 0, 0, 0, time.Local)
	appendDreamRun(dreamRun{StartedAt: start, Provider: "deepseek", Model: "deepseek-chat", SessionID: "sRT"})

	final := "巡检完成，处理 7 个库。\n\n| 库 | 条目 | 动作 |\n|---|---|---|\n| repo-a | 80 | 保持 |\n" +
		"  缩进也保留\n"
	steps := []dreamStep{
		{T: "2026-11-15T22:29:14-08:00", Tool: "memory_dream", Arg: "repos", OK: true, Note: "4 个库"},
		{T: "2026-11-15T22:29:20-08:00", Tool: "memory_dream", Arg: "apply repo-a", OK: true, Note: "归档 2 条"},
		{T: "2026-11-15T22:29:31-08:00", Tool: "bash", Arg: "ls -la", OK: false},
	}
	fin := start.Add(9 * time.Minute)
	completeDreamRun("sRT", fin, dreamSummaryOf(final, nil), "", nil, dreamRunDetail{Result: final, Steps: steps})

	records := readDreamRuns(0)
	if len(records) != 1 {
		t.Fatalf("应只有一条记录，得到 %d 条", len(records))
	}
	got := records[0]
	if got.Result != strings.TrimSpace(final) {
		t.Fatalf("result 往返后变了：\n got %q\nwant %q", got.Result, strings.TrimSpace(final))
	}
	if !strings.Contains(got.Result, "\n") {
		t.Fatal("result 必须保留换行（详情视图要按原文排版）")
	}
	if len(got.Steps) != 3 {
		t.Fatalf("steps 应往返 3 条，得到 %d 条: %+v", len(got.Steps), got.Steps)
	}
	if got.Steps[1].Tool != "memory_dream" || got.Steps[1].Arg != "apply repo-a" ||
		!got.Steps[1].OK || got.Steps[1].Note != "归档 2 条" {
		t.Fatalf("第 2 步往返不对: %+v", got.Steps[1])
	}
	if got.Steps[1].T != "2026-11-15T22:29:20-08:00" {
		t.Fatalf("步骤时间应原样返回，得到 %q", got.Steps[1].T)
	}
	// 摘要与正文并存：列表要一行，详情要原文。
	if got.Summary == "" || strings.Contains(got.Summary, "\n") {
		t.Fatalf("summary 应仍是一行摘要: %q", got.Summary)
	}
	// 落盘后仍是合法 JSONL（steps 里没有会被序列化破坏的字符）。
	raw, err := os.ReadFile(filepath.Join(dir, "dream_runs.jsonl"))
	if err != nil {
		t.Fatalf("read runs: %v", err)
	}
	if n := strings.Count(strings.TrimSpace(string(raw)), "\n"); n != 0 {
		t.Fatalf("一条记录必须是一行，实际 %d 个换行", n)
	}
}

// TestDreamRunLegacyRecordReadsEmpty 老记录（没有 result/steps 两个字段）读出来是空，不报错。
//
// 向后兼容是这一版最容易被忽略的一半：文件里躺着上百条旧记录，
// 反序列化多一个必填字段就会让整份列表读空。
func TestDreamRunLegacyRecordReadsEmpty(t *testing.T) {
	dir := withDreamDir(t)
	p := filepath.Join(dir, "dream_runs.jsonl")
	legacy := `{"started_at":"2026-11-01T03:00:00+08:00","finished_at":"2026-11-01T03:07:00+08:00",` +
		`"provider":"deepseek","model":"deepseek-chat","session_id":"sOld","summary":"处理 2 个库",` +
		`"repos":[{"id":"repo-a","topics":5,"archived":1}]}` + "\n"
	if err := os.WriteFile(p, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}

	records := readDreamRuns(0)
	if len(records) != 1 {
		t.Fatalf("老记录应能读回，得到 %d 条", len(records))
	}
	if records[0].Result != "" || len(records[0].Steps) != 0 {
		t.Fatalf("老记录的两个新字段应为空: result=%q steps=%+v", records[0].Result, records[0].Steps)
	}
	if records[0].Summary != "处理 2 个库" || records[0].SessionID != "sOld" {
		t.Fatalf("老字段被破坏了: %+v", records[0])
	}

	// 详情命令也要能用老记录回答（found=true，只是没有 result/steps）。
	m, buf := dreamTestManager(t)
	m.dispatch(cmd(1, "dream_detail", map[string]any{"session_id": "sOld"}))
	resps := commandResponses(readLines(buf))
	if len(resps) != 1 || resps[0]["ok"] != true {
		t.Fatalf("老记录的详情应成功: %+v", resps)
	}
	run := resps[0]["data"].(map[string]any)["run"].(map[string]any)
	if run["summary"] != "处理 2 个库" {
		t.Fatalf("详情里应带上摘要（前端拿它当正文的降级显示）: %+v", run)
	}
	if _, has := run["result"]; has {
		t.Fatalf("老记录不该凭空多出 result 字段: %+v", run)
	}
}

// TestDreamStepsCap 步骤上限：前 60 + 后 60 + 中间一条省略占位。
func TestDreamStepsCap(t *testing.T) {
	mk := func(n int) []dreamStep {
		out := make([]dreamStep, n)
		for i := range out {
			out[i] = dreamStep{Tool: "read_file", Arg: fmt.Sprintf("f%d", i), OK: true}
		}
		return out
	}

	// 未超限：原样（不插入占位，不能平白多出一条）。
	for _, n := range []int{0, 1, dreamStepsMax} {
		if got := capDreamSteps(mk(n)); len(got) != n {
			t.Fatalf("%d 步不应被裁剪，得到 %d 步", n, len(got))
		}
	}

	// 超限：前 60 + 占位 + 后 60。
	got := capDreamSteps(mk(300))
	if len(got) != dreamStepsHead+dreamStepsTail+1 {
		t.Fatalf("裁剪后应有 %d 步，得到 %d 步", dreamStepsHead+dreamStepsTail+1, len(got))
	}
	if got[0].Arg != "f0" {
		t.Fatalf("应保留开头: %+v", got[0])
	}
	if got[dreamStepsHead-1].Arg != "f59" {
		t.Fatalf("第 60 步应是 f59: %+v", got[dreamStepsHead-1])
	}
	mid := got[dreamStepsHead]
	if mid.Omitted != 300-dreamStepsHead-dreamStepsTail {
		t.Fatalf("省略条数不对: %+v（want %d）", mid, 300-dreamStepsHead-dreamStepsTail)
	}
	if mid.Tool != "" || mid.OK {
		t.Fatalf("占位条目不该长得像一次成功调用: %+v", mid)
	}
	if got[len(got)-1].Arg != "f299" {
		t.Fatalf("应保留结尾: %+v", got[len(got)-1])
	}

	// 恰好 121 步：省略 1 条（边界上也要如实记录，而不是悄悄丢掉）。
	got = capDreamSteps(mk(121))
	if len(got) != dreamStepsMax+1 || got[dreamStepsHead].Omitted != 1 {
		t.Fatalf("121 步应省 1 条: len=%d mid=%+v", len(got), got[dreamStepsHead])
	}
}

// TestDreamResultTruncateUTF8 收尾正文超长截断到 4000 字符，且不切半个汉字。
func TestDreamResultTruncateUTF8(t *testing.T) {
	// 全中文（每字 3 字节）：按字节切一定会切坏，按 rune 切才对。
	long := strings.Repeat("记忆库巡检完成。", 1000) // 8000 字
	got := dreamResultOf(long)
	if n := len([]rune(got)); n != dreamResultMax {
		t.Fatalf("截断后应是 %d 字符（含省略号），得到 %d", dreamResultMax, n)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("截断后应有省略号: %q", got[len(got)-12:])
	}
	// 不破坏 UTF-8：能原样编码回合法 JSON、且每个字都是完整汉字。
	if !json.Valid([]byte(`"` + got + `"`)) {
		t.Fatal("截断后不是合法 UTF-8 字符串")
	}
	if strings.ContainsRune(got, '\uFFFD') {
		t.Fatal("截断产生了替换字符（切坏了汉字）")
	}
	if !strings.HasPrefix(long, strings.TrimSuffix(got, "…")) {
		t.Fatal("截断应保留前缀原文")
	}

	// 未超限：原样返回（只去掉首尾空白，正文内部的换行必须留着）。
	short := "  第一行\n第二行\n\n第四行  \n"
	if got := dreamResultOf(short); got != "第一行\n第二行\n\n第四行" {
		t.Fatalf("未超限时应原样保留换行: %q", got)
	}
	if got := dreamResultOf("   \n\n"); got != "" {
		t.Fatalf("纯空白应收成空: %q", got)
	}
}

// TestDreamDetailCommand dream_detail 三态：命中 / 记录里没有 / session_id 为空。
func TestDreamDetailCommand(t *testing.T) {
	dir := withDreamDir(t)
	m, buf := dreamTestManager(t)

	start := time.Date(2026, 11, 15, 22, 0, 0, 0, time.Local)
	// 25 条记录：详情要能查到第 21~25 条之外的（dream_get 只给最近 20 条）。
	for i := 0; i < 25; i++ {
		appendDreamRun(dreamRun{
			StartedAt: start.Add(time.Duration(i) * 24 * time.Hour),
			Provider:  "deepseek", Model: "deepseek-chat",
			SessionID: fmt.Sprintf("s%02d", i),
		})
	}
	completeDreamRun("s02", start.Add(9*time.Minute), "处理 3 个库", "", nil, dreamRunDetail{
		Result: "正文第 1 行\n正文第 2 行",
		Steps:  []dreamStep{{Tool: "memory_dream", Arg: "repos", OK: true, Note: "3 个库"}},
	})

	// 命中（s02 是 25 条里的第 3 条，列表里看不到它）。
	m.dispatch(cmd(1, "dream_detail", map[string]any{"session_id": "s02"}))
	resps := commandResponses(readLines(buf))
	if len(resps) != 1 || resps[0]["ok"] != true {
		t.Fatalf("dream_detail 命中应成功: %+v", resps)
	}
	data := resps[0]["data"].(map[string]any)
	if data["found"] != true {
		t.Fatalf("found 应为 true: %+v", data)
	}
	run := data["run"].(map[string]any)
	if run["session_id"] != "s02" || run["result"] != "正文第 1 行\n正文第 2 行" {
		t.Fatalf("详情内容不对: %+v", run)
	}
	if run["summary"] != "处理 3 个库" || run["provider"] != "deepseek" || run["model"] != "deepseek-chat" {
		t.Fatalf("详情应带整条记录（头部字段）: %+v", run)
	}
	if steps, _ := run["steps"].([]any); len(steps) != 1 {
		t.Fatalf("详情应带时间线: %+v", run["steps"])
	}

	// 未命中：ok:false + 那唯一一句文案（不返回空对象让前端猜）。
	m.dispatch(cmd(2, "dream_detail", map[string]any{"session_id": "sNone"}))
	resps = commandResponses(readLines(buf))
	if len(resps) != 1 || resps[0]["ok"] != false {
		t.Fatalf("未命中应 ok:false: %+v", resps)
	}
	if errStr, _ := resps[0]["error"].(string); !strings.Contains(errStr, "200 行") {
		t.Fatalf("未命中文案应说明记录只保留 200 行，得到 %q", errStr)
	}
	if data := resps[0]["data"].(map[string]any); len(data) != 0 {
		t.Fatalf("未命中不该返回数据: %+v", data)
	}

	// session_id 为空（调用方 bug，与"记录过期"文案要能区分）。
	for i, payload := range []map[string]any{
		{},
		{"session_id": ""},
		{"session_id": "   "},
	} {
		m.dispatch(cmd(10+i, "dream_detail", payload))
		resps = commandResponses(readLines(buf))
		if len(resps) != 1 || resps[0]["ok"] != false {
			t.Fatalf("空 session_id 应被拒绝（第 %d 种）: %+v", i, resps)
		}
		errStr, _ := resps[0]["error"].(string)
		if !strings.Contains(errStr, "session_id") {
			t.Fatalf("空 session_id 的文案应点名参数，得到 %q", errStr)
		}
		if strings.Contains(errStr, "200 行") {
			t.Fatalf("空 session_id 不该说「记录过期」（第 %d 种）: %q", i, errStr)
		}
	}

	// 记录文件整个不存在（首次安装）：仍是"过期"而不是崩。
	if err := os.Remove(filepath.Join(dir, "dream_runs.jsonl")); err != nil {
		t.Fatalf("remove: %v", err)
	}
	m.dispatch(cmd(20, "dream_detail", map[string]any{"session_id": "s02"}))
	resps = commandResponses(readLines(buf))
	if len(resps) != 1 || resps[0]["ok"] != false {
		t.Fatalf("无记录文件时应 ok:false: %+v", resps)
	}
}

// TestDreamDetailAfterRotation 轮转之后的老记录：详情如实报"已不在本地记录里"。
//
// 这是**正常状态**而不是故障：dreamRunsKeep 只留最近 200 行，更老的一轮
// 列表里看不到、详情也查不到，文案要让人明白是"没留在本地"。
func TestDreamDetailAfterRotation(t *testing.T) {
	dir := withDreamDir(t)
	m, buf := dreamTestManager(t)
	_ = dir // 记录落点由 withDreamDir 切好，这里只看命令表现

	appendDreamRun(dreamRun{StartedAt: time.Now(), Provider: "deepseek", Model: "deepseek-chat", SessionID: "sNow"})

	// 模拟"这条是我半年前点的、早被轮转掉"：同一个命令、一个不存在的 id。
	m.dispatch(cmd(1, "dream_detail", map[string]any{"session_id": "sRotatedAway"}))
	resps := commandResponses(readLines(buf))
	if len(resps) != 1 || resps[0]["ok"] != false {
		t.Fatalf("轮转掉的记录应 ok:false: %+v", resps)
	}
	if errStr, _ := resps[0]["error"].(string); errStr != dreamDetailGone {
		t.Fatalf("文案应是 dreamDetailGone，得到 %q", errStr)
	}
}

// TestDreamTallyStepsFromToolResults 时间线采集：每种工具的 arg/ok/note 各取什么。
func TestDreamTallyStepsFromToolResults(t *testing.T) {
	tl := newDreamTally()

	tl.observe(dreamToolEvent("memory_dream", `{"action":"repos"}`, `{"count":4,"repos":[{"id":"a","topics":9},{"id":"b","topics":0}]}`))
	tl.observe(dreamToolEvent("memory_dream", `{"action":"apply","repo":"a"}`, "记忆归档：归档 2 条：x.md、y.md；快照 .state/dream/1"))
	tl.observe(dreamToolEvent("bash", `{"command":"rg -n 锚点 memory/repos\n  --glob '*.md'"}`, "一行命中"))
	tl.observe(dreamToolFail("read_file", `{"path":"/tmp/nope.md"}`, "read_file: file not found: /tmp/nope.md"))
	tl.observe(dreamToolEvent("write_file", `{"path":"memory/repos/a/x.md","content":"很长很长的正文"}`, "已写入"))
	// IsError 为假但结果看着是错误（MCP/外部命令的常见形态）：宁可判失败。
	tl.observe(dreamToolEvent("mcp__foo__bar", `{}`, "Error: upstream 502"))
	// 成功结果里恰好出现 "error" 这个词（读到的日志/代码）：不能误判。
	tl.observe(dreamToolEvent("read_file", `{"path":"memory/repos/a/x.md"}`, "package main\n// error handling 说明\n"))

	steps, repos := tl.snapshot()
	if len(steps) != 7 {
		t.Fatalf("应记 7 步，得到 %d: %+v", len(steps), steps)
	}
	if len(repos) != 2 {
		t.Fatalf("台账应记 2 个库（时间线与台账同源），得到 %+v", repos)
	}
	if repos[0].ID != "a" || repos[0].Topics != 9 || repos[0].Archived != 2 {
		t.Fatalf("apply 应并进已列出的库，而不是新开一条: %+v", repos[0])
	}
	want := []struct {
		tool, arg, note string
		ok              bool
	}{
		{"memory_dream", "repos", "4 个库", true},
		{"memory_dream", "apply a", "归档 2 条", true},
		{"bash", "rg -n 锚点 memory/repos --glob '*.md'", "", true},
		// read_file 不在"带参数"的清单里（规格：只有 memory_dream / bash / 写文件带
		// arg），所以即便是失败步也只记工具名——要扩就在 dreamStepArg 里加 case。
		{"read_file", "", "", false},
		{"write_file", "memory/repos/a/x.md", "", true},
		{"mcp__foo__bar", "", "", false},
		{"read_file", "", "", true},
	}
	for i, w := range want {
		if steps[i].Tool != w.tool || steps[i].Arg != w.arg || steps[i].OK != w.ok || steps[i].Note != w.note {
			t.Fatalf("第 %d 步不对：got %+v, want %+v", i, steps[i], w)
		}
		if steps[i].T == "" {
			t.Fatalf("第 %d 步应有时间戳: %+v", i, steps[i])
		}
	}

	// bash 命令超长：按 rune 截到 60（不切半个汉字）。
	long := newDreamTally()
	long.observe(dreamToolEvent("bash", `{"command":"`+strings.Repeat("记", 100)+`"}`, "ok"))
	ls, _ := long.snapshot()
	if r := []rune(ls[0].Arg); len(r) != 61 { // 60 + 省略号
		t.Fatalf("bash 命令应截到 60 字 + 省略号，得到 %d: %q", len(r), ls[0].Arg)
	}

	// nil 台账安全（非 Dream 会话走不到，但守卫要挡住 nil 解引用）。
	var empty *dreamTally
	empty.observe(dreamToolEvent("memory_dream", `{"action":"repos"}`, "{}"))
	if s, r := empty.snapshot(); len(s) != 0 || len(r) != 0 {
		t.Fatal("nil 台账应返回空快照")
	}
	// 参数不是 JSON / 工具名为空：记步骤但不炸（模型偶尔给半截参数）。
	odd := newDreamTally()
	odd.observe(dreamToolEvent("bash", "{not json", "ok"))
	odd.observe(dreamToolEvent("  ", `{}`, "ok"))
	if s, _ := odd.snapshot(); len(s) != 1 || s[0].Arg != "" {
		t.Fatalf("坏参数应记成空 arg 的一步，得到 %+v", s)
	}
}

// TestDreamStepsSnapshotIsCopy 快照是拷贝：收口之后再到的事件不能改到已取走的数据。
//
// 并发形态：观察在会话 goroutine（每条 ToolRunEnd），收口在主会话 emit 尾部。
// 快照若不拷贝，收口序列化时另一条 append 会踩同一块底层数组（-race 能测出来）。
func TestDreamStepsSnapshotIsCopy(t *testing.T) {
	tl := newDreamTally()
	tl.observe(dreamToolEvent("bash", `{"command":"a"}`, "ok"))
	steps, _ := tl.snapshot()
	tl.observe(dreamToolEvent("bash", `{"command":"b"}`, "ok"))
	if len(steps) != 1 {
		t.Fatalf("快照不应被后续观察追加，得到 %d 步", len(steps))
	}
	if steps2, _ := tl.snapshot(); len(steps2) != 2 {
		t.Fatalf("台账内部应有 2 步，得到 %d 步", len(steps2))
	}
}
