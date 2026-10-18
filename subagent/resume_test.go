package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/tools"
)

// ---------- agent_resume：从 journal 恢复已结束的子 agent ----------

// resumeSpawnTool 构造带 journal 解析器的 agent_spawn + agent_resume 一对（共用同一
// spawn loop / 注册表 / 目录），返回两个工具与注册表 —— 恢复必须与派发同链路，故成对构造。
func resumeSpawnTool(t *testing.T, dir string, loop *agents.AgentLoop) (spawn *Tool, resume *agentResumeTool, reg *Registry) {
	t.Helper()
	spawn, reg = rzJournalSpawnTool(t, dir, loop)
	resume = &agentResumeTool{spawn: spawn, reg: reg}
	return spawn, resume, reg
}

// resumeInputCapturingLoop 返回一个 loop，其 provider 把每次运行的**首条 user 消息**
// 记录下来（恢复的正确性判据 = 新任务收到的简报里有没有原任务的原始指令与进度）。
func resumeInputCapturingLoop() (*agents.AgentLoop, *resumeCaptureProvider) {
	p := &resumeCaptureProvider{}
	return agents.NewAgentLoop(agents.WithProvider(p)), p
}

type resumeCaptureProvider struct {
	mu     sync.Mutex
	inputs []string
}

func (p *resumeCaptureProvider) Stream(_ context.Context, req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	for _, m := range req.Messages {
		if m.Role == core.User {
			for _, c := range m.Content {
				if c.Type == core.ContentTypeText && c.Content != "" {
					p.mu.Lock()
					p.inputs = append(p.inputs, c.Content)
					p.mu.Unlock()
				}
			}
			break
		}
	}
	return handler(provider.LLMEndEvent{FinishReason: core.FinishReasonStop, Content: "续跑完成"})
}

// input 第 i 次运行的输入（测试 goroutine 读，运行在后台 goroutine 写 → 走锁，
// 与 captureReqProvider 同一约定：读侧只用快照）。
func (p *resumeCaptureProvider) input(i int) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i >= len(p.inputs) {
		return ""
	}
	return p.inputs[i]
}

func (p *resumeCaptureProvider) count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.inputs)
}

// TestResumeRebuildsHandoffBrief 恢复的核心契约：新任务收到的输入 = 从原任务 journal
// 重建的交接简报 —— 必须含原始指令全文、已完成轮次、以及终态原因，且带上 resumed_from。
func TestResumeRebuildsHandoffBrief(t *testing.T) {
	dir := t.TempDir()
	// 原任务：两轮（一轮工具 + 一轮收尾），journal 里因此有 start(全文)+turn×2+result。
	spawn, resume, reg := resumeSpawnTool(t, dir, rzTwoRoundLoop(nil))
	const origTask = "研究 X 方案并把结论写到 report.md；只读源码，不要改任何文件。"

	out, err := spawn.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"name":"research X","task":`+jsonString(origTask)+`}`)
	if err != nil {
		t.Fatal(err)
	}
	old := rzParseSpawn(t, out)
	rzWaitSnapshot(t, reg, old.TaskID, TaskCompleted)

	// 恢复：换一个 recording loop（独立 loop 实例，模拟「同一人设、新的运行」）。
	recLoop, cap := resumeInputCapturingLoop()
	resume.spawn.spec.Loop = recLoop

	rout, err := resume.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"task_id":"`+old.TaskID+`"}`)
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	got := rzParseSpawn(t, rout)
	if got.TaskID == "" || got.TaskID == old.TaskID {
		t.Fatalf("恢复必须产生**新** task_id：old=%s new=%s", old.TaskID, got.TaskID)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(rout), &raw); err != nil {
		t.Fatalf("resume 返回非 JSON: %v\n%s", err, rout)
	}
	if raw["resumed_from"] != old.TaskID {
		t.Fatalf("resume 返回应带 resumed_from=%s，got %v", old.TaskID, raw["resumed_from"])
	}
	rzWaitSnapshot(t, reg, got.TaskID, TaskCompleted)

	if cap.count() == 0 {
		t.Fatal("续跑任务未收到任何 user 输入")
	}
	brief := cap.input(0)
	for _, want := range []string{
		origTask,                // 原始指令全文（start.Task，非 200 字符摘要）
		old.TaskID,              // 交接对象的身份
		"Progress already made", // 已完成轮次段
		"Your job now",          // 接手指令段
	} {
		if !strings.Contains(brief, want) {
			t.Fatalf("交接简报缺少 %q：\n%s", want, brief)
		}
	}
	// 原任务的 journal 保留（审计），新任务另有一份自己的 journal。
	if _, err := os.Stat(filepath.Join(dir, old.TaskID+".jsonl")); err != nil {
		t.Fatalf("原任务 journal 不应被覆盖/删除: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, got.TaskID+".jsonl")); err != nil {
		t.Fatalf("续跑任务应有自己的 journal: %v", err)
	}
}

// TestResumeSuccessorsResumedFromOnJournal 恢复链可追溯：新任务 journal 的 start 记录
// 带 resumed_from + Task 全文（交接结论），TaskList/TaskInfo 同字段透出。
func TestResumeSuccessorsResumedFromOnJournal(t *testing.T) {
	dir := t.TempDir()
	spawn, resume, reg := resumeSpawnTool(t, dir, rzTwoRoundLoop(nil))
	const origTask = "原始任务的完整指令文本，供恢复重建。"

	out, err := spawn.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"name":"n","task":`+jsonString(origTask)+`}`)
	if err != nil {
		t.Fatal(err)
	}
	old := rzParseSpawn(t, out)
	rzWaitSnapshot(t, reg, old.TaskID, TaskCompleted)

	rout, err := resume.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"task_id":"`+old.TaskID+`"}`)
	if err != nil {
		t.Fatal(err)
	}
	got := rzParseSpawn(t, rout)
	rzWaitSnapshot(t, reg, got.TaskID, TaskCompleted)

	recs, err := ReadAll(filepath.Join(dir, got.TaskID+".jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var start *JournalRecord
	for i := range recs {
		if recs[i].Type == "start" {
			start = &recs[i]
			break
		}
	}
	if start == nil {
		t.Fatalf("续跑任务缺 start 记录: %+v", recs)
	}
	if start.ResumedFrom != old.TaskID {
		t.Fatalf("start.resumed_from = %q, want %q", start.ResumedFrom, old.TaskID)
	}
	if start.Task == "" || !strings.Contains(start.Task, "Original instruction") {
		t.Fatalf("start.task 应存**交接简报全文**（恢复的真实输入），got %q", start.Task)
	}

	// 注册表透出恢复链（TaskList 数据源）。
	info, err := reg.Snapshot(got.TaskID)
	if err != nil {
		t.Fatal(err)
	}
	if info.ResumedFrom != old.TaskID {
		t.Fatalf("TaskInfo.ResumedFrom = %q, want %q", info.ResumedFrom, old.TaskID)
	}
}

// TestResumeRejectsRunningTask 状态闸门：仍在运行的任务不可恢复（那是 agent_send 的活）。
func TestResumeRejectsRunningTask(t *testing.T) {
	dir := t.TempDir()
	gate := make(chan struct{})
	defer close(gate)
	spawn, resume, reg := resumeSpawnTool(t, dir, rzTwoRoundLoop(gate))

	out, err := spawn.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"name":"n","task":"t"}`)
	if err != nil {
		t.Fatal(err)
	}
	sp := rzParseSpawn(t, out)
	// 等它真的在跑（gate 卡住工具轮）。
	rzWaitSnapshot(t, reg, sp.TaskID, TaskRunning)

	_, err = resume.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"task_id":"`+sp.TaskID+`"}`)
	if err == nil {
		t.Fatal("运行中的任务必须拒绝恢复")
	}
	if !strings.Contains(err.Error(), "still running") || !strings.Contains(err.Error(), "agent_send") {
		t.Fatalf("拒绝文案应指向 agent_send/agent_interrupt: %v", err)
	}
}

// TestResumeAcrossProcessRestart 跨进程恢复（本工具最实的价值，见记忆库
// subagent-longform-doc-budget）：注册表是内存态，进程重启后任务对象不存在，
// 但 journal 在磁盘上。用 taskID + 路径解析器即可把上一进程的半成品续起来。
func TestResumeAcrossProcessRestart(t *testing.T) {
	dir := t.TempDir()
	// 第一个「进程」：跑一个任务并等它终态，journal 落盘。
	spawn1, _, reg1 := resumeSpawnTool(t, dir, rzTwoRoundLoop(nil))
	const origTask = "调研 codemode 的实现并写出报告。"
	out, err := spawn1.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"name":"codemode 调研","task":`+jsonString(origTask)+`}`)
	if err != nil {
		t.Fatal(err)
	}
	old := rzParseSpawn(t, out)
	rzWaitSnapshot(t, reg1, old.TaskID, TaskCompleted)
	_ = reg1.Close(context.Background()) // 进程结束：注册表销毁

	// 第二个「进程」：全新注册表（同目录 journal），注册表里**查不到**旧任务。
	recLoop, cap := resumeInputCapturingLoop()
	reg2 := NewRegistry()
	reg2.SetSessionCtx(context.Background())
	t.Cleanup(func() { _ = reg2.Close(context.Background()) })
	spawn2 := NewBackgroundTool(Spec{
		Name: "agent_spawn", Description: "d", Loop: recLoop, Background: true,
		JournalPath: func(taskID string) (string, error) { return filepath.Join(dir, taskID+".jsonl"), nil },
	}, func() context.Context { return context.Background() }, reg2)
	resume2 := &agentResumeTool{spawn: spawn2, reg: reg2}

	if _, err := reg2.Snapshot(old.TaskID); err == nil {
		t.Fatal("前置：新注册表里不应有旧任务")
	}
	rout, err := resume2.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"task_id":"`+old.TaskID+`"}`)
	if err != nil {
		t.Fatalf("跨进程恢复: %v", err)
	}
	got := rzParseSpawn(t, rout)
	if got.TaskID == old.TaskID {
		t.Fatalf("恢复应产生新 task_id: %s", got.TaskID)
	}
	rzWaitSnapshot(t, reg2, got.TaskID, TaskCompleted)

	if cap.count() == 0 {
		t.Fatal("跨进程续跑任务未收到输入")
	}
	brief := cap.input(0)
	if !strings.Contains(brief, origTask) {
		t.Fatalf("跨进程恢复的简报缺原始指令（journal 未被正确重建）:\n%s", brief)
	}
	if !strings.Contains(brief, old.TaskID) {
		t.Fatalf("简报应标出接手的是哪个旧任务:\n%s", brief)
	}
}

// TestResumeUnknownTaskWithoutJournal 注册表无此任务且磁盘无 journal → 明确拒绝
// （不静默造一个新任务）。
func TestResumeUnknownTaskWithoutJournal(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	reg.SetSessionCtx(context.Background())
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	spawn := NewBackgroundTool(Spec{
		Name: "agent_spawn", Description: "d", Loop: rzTwoRoundLoop(nil), Background: true,
		JournalPath: func(taskID string) (string, error) { return filepath.Join(dir, taskID+".jsonl"), nil },
	}, func() context.Context { return context.Background() }, reg)
	resume := &agentResumeTool{spawn: spawn, reg: reg}

	_, err := resume.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"task_id":"task-zzzzzzzz"}`)
	if err == nil {
		t.Fatal("未知任务且无 journal 必须拒绝")
	}
	if !strings.Contains(err.Error(), "no checkpoint") || !strings.Contains(err.Error(), "no usable journal on disk") {
		t.Fatalf("拒绝文案应说明无 checkpoint 也无 journal: %v", err)
	}
}

// TestResumeRejectsMissingJournal 无 journal（未落盘 / 解析失败降级）→ 明确
// 拒绝并指引改派新任务，而不是造一个空简报让子 agent 猜。
func TestResumeRejectsMissingJournal(t *testing.T) {
	reg := NewRegistry()
	reg.SetSessionCtx(context.Background())
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	// JournalPath 报错 → outputFile 为空（D5 降级路径）。
	spawn := NewBackgroundTool(Spec{
		Name: "agent_spawn", Description: "d", Loop: rzTwoRoundLoop(nil), Background: true,
		JournalPath: func(string) (string, error) { return "", errors.New("journal disabled") },
	}, func() context.Context { return context.Background() }, reg)
	resume := &agentResumeTool{spawn: spawn, reg: reg}

	out, err := spawn.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "", `{"name":"n","task":"t"}`)
	if err != nil {
		t.Fatal(err)
	}
	sp := rzParseSpawn(t, out)
	rzWaitSnapshot(t, reg, sp.TaskID, TaskCompleted)

	_, err = resume.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"task_id":"`+sp.TaskID+`"}`)
	if err == nil {
		t.Fatal("无 journal 的任务必须拒绝恢复")
	}
	if !strings.Contains(err.Error(), "no checkpoint") || !strings.Contains(err.Error(), "spawn a fresh task") {
		t.Fatalf("拒绝文案应说明无 checkpoint 并指引重新派发: %v", err)
	}
}

// TestResumeJournalWithoutStartRecord 有 journal 但缺 start 记录（旧版本 / 手删）→
// 无原始指令可重建，明确拒绝（不自造任务）。
func TestResumeJournalWithoutStartRecord(t *testing.T) {
	dir := t.TempDir()
	reg := NewRegistry()
	reg.SetSessionCtx(context.Background())
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	spawn := NewBackgroundTool(Spec{
		Name: "agent_spawn", Description: "d", Loop: rzTwoRoundLoop(nil), Background: true,
		JournalPath: func(taskID string) (string, error) { return filepath.Join(dir, taskID+".jsonl"), nil },
	}, func() context.Context { return context.Background() }, reg)
	resume := &agentResumeTool{spawn: spawn, reg: reg}

	out, err := spawn.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "", `{"name":"n","task":"t"}`)
	if err != nil {
		t.Fatal(err)
	}
	sp := rzParseSpawn(t, out)
	rzWaitSnapshot(t, reg, sp.TaskID, TaskCompleted)

	// 只留 turn 记录（模拟 start 缺失的 journal）。
	if err := os.WriteFile(sp.OutputFile, []byte(`{"type":"turn","task_id":"`+sp.TaskID+`","round":1,"text":"x"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = resume.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"task_id":"`+sp.TaskID+`"}`)
	if err == nil {
		t.Fatal("缺 start 记录必须拒绝恢复")
	}
	if !strings.Contains(err.Error(), "no start record") {
		t.Fatalf("拒绝文案应说明缺 start 记录: %v", err)
	}
}

// TestResumeInterruptedTaskCarriesMessageAndReason 被中断的任务：简报要带中断原因，
// 且 message 增量指令原样进入「Your job now」段（模型据此换方向）。
func TestResumeInterruptedTaskCarriesReasonAndMessage(t *testing.T) {
	dir := t.TempDir()
	gate := make(chan struct{})
	spawn, resume, reg := resumeSpawnTool(t, dir, rzTwoRoundLoop(gate))
	const origTask = "把 auth 模块重构完并跑通测试。"

	out, err := spawn.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"name":"refactor auth","task":`+jsonString(origTask)+`}`)
	if err != nil {
		t.Fatal(err)
	}
	old := rzParseSpawn(t, out)
	rzWaitSnapshot(t, reg, old.TaskID, TaskRunning)
	if err := reg.Interrupt(old.TaskID); err != nil {
		t.Fatal(err)
	}
	close(gate)
	rzWaitSnapshot(t, reg, old.TaskID, TaskInterrupted)

	recLoop, cap := resumeInputCapturingLoop()
	resume.spawn.spec.Loop = recLoop
	const msg = "构建失败了：只需修编译错误，不要重跑重构。"
	rout, err := resume.Call(events.WithToolContext(context.Background(), "p", 0, nil, nil, nil), "",
		`{"task_id":"`+old.TaskID+`","message":`+jsonString(msg)+`}`)
	if err != nil {
		t.Fatalf("resume interrupted: %v", err)
	}
	got := rzParseSpawn(t, rout)
	rzWaitSnapshot(t, reg, got.TaskID, TaskCompleted)

	if cap.count() == 0 {
		t.Fatal("续跑任务未收到输入")
	}
	brief := cap.input(0)
	if !strings.Contains(brief, origTask) {
		t.Fatalf("简报缺原始指令:\n%s", brief)
	}
	if !strings.Contains(brief, msg) {
		t.Fatalf("简报应包含本次增量指令 %q:\n%s", msg, brief)
	}
	if !strings.Contains(brief, "interrupted") {
		t.Fatalf("简报应说明原任务终态为 interrupted:\n%s", brief)
	}
}

// TestResumeValidParams 参数校验：非法 id / 超长 name 在 ValidParams 就被拒（不耗槽）。
func TestResumeValidParams(t *testing.T) {
	r := &agentResumeTool{spawn: NewTool(Spec{Name: "agent_resume"}), reg: NewRegistry()}
	cases := []struct {
		args string
		ok   bool
	}{
		{`{"task_id":"task-abc123fg"}`, true},
		{`{"task_id":""}`, false},
		{`{"task_id":"task-1"}`, false},            // 旧格式/非本注册表 id
		{`{"task_id":"tooltask-a1b2c3d4"}`, false}, // promoted 工具任务无 journal
		{`{}`, false},
		{`not json`, false},
		{`{"task_id":"task-abc123fg","name":"` + strings.Repeat("x", maxSubAgentNameLen+1) + `"}`, false},
	}
	for _, c := range cases {
		err := r.ValidParams(context.Background(), "", c.args)
		if c.ok && err != nil {
			t.Errorf("args %s: want ok, got %v", c.args, err)
		}
		if !c.ok && err == nil {
			t.Errorf("args %s: want error, got nil", c.args)
		}
	}
}

// jsonString Go 字符串 → JSON 字符串字面量（构造含中文/引号的 task 参数）。
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// TestAgentResumeToolRegistered NewAgentTools 注册面包含 agent_resume（宿主装配断言）。
func TestAgentResumeToolRegistered(t *testing.T) {
	list := NewAgentTools(rzTwoRoundLoop(nil), func() context.Context { return context.Background() }, NewRegistry(), nil, nil, nil)
	names := map[string]bool{}
	for _, tl := range list {
		names[tl.Name()] = true
	}
	for _, want := range []string{"agent_spawn", "agent_send", "agent_interrupt", "agent_resume", "TaskList", "TaskOutput"} {
		if !names[want] {
			t.Fatalf("缺少工具 %s: %v", want, names)
		}
	}
}

// 断言 tools.Tool 接口被完整实现（漏方法时编译期就暴露，而不是运行时 panic）。
var _ tools.Tool = (*agentResumeTool)(nil)

// ---------- 本文件自带的测试桩（rz 前缀） ----------
//
// 为什么不复用同包既有的 twoRoundLoop / mustParseSpawn / waitSnapshot / mockProvider：
// 它们定义在**未入库**的测试文件里（本仓 .git/info/exclude 隐藏了 262 个 _test.go），
// 干净检出下这些文件不存在 —— 直接引用会让 subagent 包在干净检出里编译失败
//（本仓已知陷阱，见 CI 只在打 tag 时跑所以从未暴露）。本文件用 rz 前缀自带所需的
// 最小集合：**只提交这一个文件也能编译并通过**，且与既有名字不冲突（将来那些文件入库
// 也不会撞名）。

// rzScriptProvider 按预设脚本逐轮发射 LLM 事件（同 mockProvider 语义，自带一份）。
type rzScriptProvider struct {
	rounds []func(req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error
	calls  int
}

func (m *rzScriptProvider) Stream(_ context.Context, req *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
	round := m.rounds[m.calls]
	if m.calls < len(m.rounds)-1 {
		m.calls++
	}
	return round(req, handler)
}

// rzGateTool 测试桩：release 非 nil 时阻塞直至放行（ctx 取消即退出，供中断路径）。
type rzGateTool struct {
	tools.BaseTool
	release chan struct{}
}

func (g *rzGateTool) ValidParams(context.Context, string, string) error { return nil }

func (g *rzGateTool) Call(ctx context.Context, _ string, _ string) (string, error) {
	if g.release == nil {
		return "echo", nil
	}
	select {
	case <-g.release:
		return "released", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

// rzTwoRoundLoop 两轮子运行：R1 工具调用轮（gate），R2 stop「结论」——journal 因此
// 有 start + turn×2 + result，正是 agent_resume 要重建的形态。
func rzTwoRoundLoop(gate chan struct{}) *agents.AgentLoop {
	eng := tools.NewToolEngine()
	eng.RegisterTool(context.Background(), &rzGateTool{BaseTool: tools.BaseTool{Name_: "echo"}, release: gate})
	p := &rzScriptProvider{rounds: []func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error{
		func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonToolCall,
				ToolCalls:    []core.ToolCall{{Id: "c1", Index: 0, Name: "echo", Arguments: `{}`}},
				Usage:        core.Usage{Input: 10, Output: 5},
			})
		},
		func(_ *provider.StreamRequest, handler func(e provider.StreamEvent) error) error {
			return handler(provider.LLMEndEvent{
				FinishReason: core.FinishReasonStop,
				Content:      "结论",
				Usage:        core.Usage{Input: 20, Output: 8, TotalTokens: 28},
			})
		},
	}}
	return agents.NewAgentLoop(agents.WithProvider(p), agents.WithToolEngine(eng))
}

// rzJournalSpawnTool 构造带 journal 解析器（dir/<taskId>.jsonl）的后台 spawn 工具。
func rzJournalSpawnTool(t *testing.T, dir string, loop *agents.AgentLoop) (*Tool, *Registry) {
	t.Helper()
	reg := NewRegistry()
	reg.SetSessionCtx(context.Background())
	t.Cleanup(func() { _ = reg.Close(context.Background()) })
	tool := NewBackgroundTool(Spec{
		Name: "agent_spawn", Description: "d", Loop: loop, Background: true,
		JournalPath: func(taskID string) (string, error) {
			return filepath.Join(dir, taskID+".jsonl"), nil
		},
	}, func() context.Context { return context.Background() }, reg)
	return tool, reg
}

// rzSpawnJSON 解析 agent_spawn / agent_resume 返回串的关键字段。
type rzSpawnJSON struct {
	TaskID     string `json:"task_id"`
	Status     string `json:"status"`
	OutputFile string `json:"output_file"`
}

func rzParseSpawn(t *testing.T, out string) rzSpawnJSON {
	t.Helper()
	var s rzSpawnJSON
	if err := json.Unmarshal([]byte(out), &s); err != nil {
		t.Fatalf("spawn 返回非 JSON: %v\n%s", err, out)
	}
	return s
}

// rzWaitSnapshot 轮询至任务进入任一期望状态（或超时）。
func rzWaitSnapshot(t *testing.T, reg *Registry, taskID string, want ...TaskStatus) TaskInfo {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if info, err := reg.Snapshot(taskID); err == nil {
			for _, w := range want {
				if info.Status == w {
					return info
				}
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	info, _ := reg.Snapshot(taskID)
	t.Fatalf("任务未达期望状态 %v：now=%s", want, info.Status)
	return TaskInfo{}
}
