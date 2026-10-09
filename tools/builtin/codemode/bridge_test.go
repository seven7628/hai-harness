package codemode

// bridge_test.go：codemode 工具**端到端**（真 node + 真引擎，不 mock 子进程）。
//
// 为什么必须真跑：这一层的价值几乎全在「跨三层的真实行为」里 —— 脚本经 stdin 进沙箱、
// 沙箱经 call 帧回调宿主、宿主走 ExecuteOne 完整管线、结果按 §15.4 映射回脚本、最后
// 组装成给模型的文本。mock 掉任何一段，被钉住的东西（结构化 vs 20 KB 文本、记录/用量
// 回传、墙钟与取消、store 只在成功时落盘）就全都测没了。
//
// 用例与交付清单一一对应（①-⑧ 是任务书里的验收项）：
//
//	① const r = await tools.read_file({path}); return r.length → 值交回模型
//	② 脚本里 tools.bash(...) 拿到**结构化对象**而不是 20 KB 文本
//	③ 3 次嵌套 → 外层 NestedCalls 长度 3，且会话成本 == 各次嵌套用量之和（不双计）
//	④ 死循环被 // @options 的 timeout_ms 打断、无残留进程、文本首行声明 TIMEOUT
//	⑤ 脚本抛错时已发生的调用记录仍在、副作用不回滚
//	⑥ hidden / model-only 工具**不可被脚本调用**（分派表不含它们）
//	⑦ 描述字节稳定（MCP 连接/断开前后逐字节相同）
//	⑧ store 成功才落盘、失败不落盘
//
// 另有几条本波次必须兑现的钉子（复核 §7.12 的两条记账）：
// 脚本结束/退出/报错时在途子调用被取消、审批等待不消耗墙钟、只读真并发 / 写类串行、
// 输出预算与 spill。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/execproc"
	"github.com/seven7628/hai-harness/runtime"
	"github.com/seven7628/hai-harness/tools"
)

// requireNode 本包端到端用例的前置。
//
// 与 execproc 的 requireOrSkip 同一开关、同一语义：默认**跳过**（开发者机器没装 node
// 不该让整套测试红），但 GO_CODE_REQUIRE_NODE 非空就**失败** —— CI 上一台没装 node 的
// 机器会让这些用例全部静默跳过，流水线一片绿、实际零覆盖（本仓历史教训）。
func requireNode(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := runtime.NewNodeRuntime(runtime.NodeConfig{}).Ensure(ctx); err != nil {
		if v := os.Getenv("GO_CODE_REQUIRE_NODE"); v != "" {
			t.Fatalf("本机没有可用 node，且 GO_CODE_REQUIRE_NODE=%q（CI 不允许静默跳过）: %v", v, err)
		}
		t.Skipf("跳过：本机没有可用 node（%v）", err)
	}
}

// ---- 测试替身工具（都是真工具：走引擎的完整管线）----

// readFileStub 读文件（纯文本路径）。测试 ① 用。
type readFileStub struct {
	tools.BaseTool
	calls atomic.Int64
}

func newReadFileStub() *readFileStub {
	return &readFileStub{BaseTool: tools.BaseTool{
		Name_:        "read_file",
		Description_: "Read a file and return its contents as text.",
		Params_:      tools.Obj(map[string]any{"path": tools.Str("file path")}, "path"),
		CanParallel_: true,
		ReadOnly_:    true,
	}}
}

func (r *readFileStub) ValidParams(_ context.Context, _, args string) error {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return fmt.Errorf("read_file: %w", err)
	}
	if a.Path == "" {
		return errors.New("read_file: path is required")
	}
	return nil
}

func (r *readFileStub) Call(_ context.Context, _, args string) (string, error) {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return "", err
	}
	r.calls.Add(1)
	b, err := os.ReadFile(a.Path)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// bashStub bash 形态的替身：**文本路径被引擎的 20 KB 无条件截断**，而脚本路径拿结构化
// 对象（全量）。形状与行为照抄 tools/builtin/bash.go：声明 OutputSchemaProvider，且只在
// tools.IsScriptCall(ctx) 为真时写结构化槽。
type bashStub struct {
	tools.BaseTool
	output string
	calls  atomic.Int64
}

func newBashStub(output string) *bashStub {
	return &bashStub{BaseTool: tools.BaseTool{
		Name_:        "bash",
		Description_: "Run a command and return its merged output.",
		Params_:      tools.Obj(map[string]any{"command": tools.Str("command line")}, "command"),
		CanParallel_: false,
	}, output: output}
}

func (b *bashStub) ValidParams(_ context.Context, _, args string) error {
	var a struct {
		Command string `json:"command"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return fmt.Errorf("bash: %w", err)
	}
	if a.Command == "" {
		return errors.New("bash: command is required")
	}
	return nil
}

func (b *bashStub) OutputSchema() any {
	return tools.Obj(map[string]any{
		"output":    tools.Str("merged output"),
		"truncated": tools.Bool("whether output was cut"),
		"exit_code": tools.Int("exit code"),
	}, "output", "truncated", "exit_code")
}

func (b *bashStub) Call(ctx context.Context, _, _ string) (string, error) {
	b.calls.Add(1)
	if tools.IsScriptCall(ctx) {
		if sink := tools.StructuredSinkFrom(ctx); sink != nil {
			raw, err := json.Marshal(map[string]any{
				"output": b.output, "truncated": false, "exit_code": 0,
			})
			if err == nil {
				sink.Set(raw)
			}
		}
	}
	return b.output, nil
}

// writeStub 写文件（CanParallel=false）—— 测试 ⑤（副作用不回滚）与串行门用。
type writeStub struct {
	tools.BaseTool
	mu       sync.Mutex
	inflight int
	maxInfl  int
	calls    atomic.Int64
}

func newWriteStub() *writeStub {
	return &writeStub{BaseTool: tools.BaseTool{
		Name_:        "write_file",
		Description_: "Write a file.",
		Params_: tools.Obj(map[string]any{
			"path":    tools.Str("file path"),
			"content": tools.Str("file content"),
		}, "path", "content"),
		CanParallel_: false,
	}}
}

func (w *writeStub) ValidParams(_ context.Context, _, args string) error {
	var a struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return fmt.Errorf("write_file: %w", err)
	}
	if a.Path == "" {
		return errors.New("write_file: path is required")
	}
	return nil
}

func (w *writeStub) Call(_ context.Context, _, args string) (string, error) {
	var a struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal([]byte(args), &a); err != nil {
		return "", err
	}
	w.calls.Add(1)
	w.mu.Lock()
	w.inflight++
	if w.inflight > w.maxInfl {
		w.maxInfl = w.inflight
	}
	w.mu.Unlock()
	// 留一点窗口：串行门失效时两个写类调用会在这里重叠。
	time.Sleep(60 * time.Millisecond)
	w.mu.Lock()
	w.inflight--
	w.mu.Unlock()
	if err := os.WriteFile(a.Path, []byte(a.Content), 0o600); err != nil {
		return "", err
	}
	return "wrote " + a.Path, nil
}

func (w *writeStub) peakInflight() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.maxInfl
}

func (u *usageStub) ValidParams(context.Context, string, string) error { return nil }

// sleepStub 只读的慢工具：数并发峰值 + 记录耗时（真并发 vs 串行的对拍）。
type sleepStub struct {
	tools.BaseTool
	sleep    time.Duration
	mu       sync.Mutex
	inflight int
	maxInfl  int
}

func newSleepStub(name string, sleep time.Duration) *sleepStub {
	return &sleepStub{BaseTool: tools.BaseTool{
		Name_: name, Description_: "A read-only tool that waits (concurrency probe).",
		Params_:      tools.Obj(map[string]any{"x": tools.Str("anything")}),
		CanParallel_: true, ReadOnly_: true,
	}, sleep: sleep}
}

func (s *sleepStub) Call(ctx context.Context, _, _ string) (string, error) {
	s.mu.Lock()
	s.inflight++
	if s.inflight > s.maxInfl {
		s.maxInfl = s.inflight
	}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inflight--
		s.mu.Unlock()
	}()
	select {
	case <-time.After(s.sleep):
		return "slept", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (s *sleepStub) peakInflight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxInfl
}

// slowStub 慢工具：把「被取消」与「跑完了」分成两个可断言的信号（取消钉子用）。
type slowStub struct {
	tools.BaseTool
	delay    time.Duration
	done     chan struct{} // 收到 ctx 取消
	doneOnce sync.Once
	finished atomic.Bool
	calls    atomic.Int64
}

func newSlowStub(name string, delay time.Duration) *slowStub {
	return &slowStub{
		BaseTool: tools.BaseTool{
			Name_: name, Description_: "A slow tool (cancellation probe).",
			Params_: tools.Obj(map[string]any{"x": tools.Str("anything")}),
		},
		delay: delay,
		done:  make(chan struct{}),
	}
}

func (s *slowStub) ValidParams(context.Context, string, string) error { return nil }

func (s *slowStub) Call(ctx context.Context, _, _ string) (string, error) {
	s.calls.Add(1)
	select {
	case <-time.After(s.delay):
		s.finished.Store(true)
		return "slow-done", nil
	case <-ctx.Done():
		s.doneOnce.Do(func() { close(s.done) })
		return "", ctx.Err()
	}
}

// usageStub 报告固定用量的子工具（模拟子 agent / 模型调用类工具）。
type usageStub struct {
	tools.BaseTool
	u core.Usage
}

func (u *usageStub) Call(context.Context, string, string) (string, error) { return "ok", nil }
func (u *usageStub) Usage() core.Usage                                    { return u.u }

func (s *sleepStub) ValidParams(context.Context, string, string) error { return nil }

// tierStub 记录「是否被执行过」的档位工具（⑥：hidden/model-only 不可被脚本调用）。
type tierStub struct {
	tools.BaseTool
	exp  tools.ToolExposure
	runs atomic.Int64
}

func newTierStub(name string, exp tools.ToolExposure) *tierStub {
	return &tierStub{
		BaseTool: tools.BaseTool{Name_: name, Description_: name + " tier probe",
			Params_: map[string]any{}},
		exp: exp,
	}
}

func (s *tierStub) Exposure() tools.ToolExposure { return s.exp }

func (s *tierStub) ValidParams(context.Context, string, string) error { return nil }

func (s *tierStub) Call(context.Context, string, string) (string, error) {
	s.runs.Add(1)
	return "RAN", nil
}

// approvalStub 需要人工确认的工具（审批暂停墙钟的钉子）。
type approvalStub struct {
	tools.BaseTool
	calls atomic.Int64
}

func newApprovalStub(name string) *approvalStub {
	return &approvalStub{BaseTool: tools.BaseTool{
		Name_: name, Description_: "A tool that requires human approval.",
		Params_:      tools.Obj(map[string]any{"x": tools.Str("anything")}),
		CanParallel_: false,
	}}
}

func (a *approvalStub) ValidParams(context.Context, string, string) error { return nil }

func (a *approvalStub) RequiresApproval(context.Context, core.ToolCall) bool { return true }

func (a *approvalStub) Call(context.Context, string, string) (string, error) {
	a.calls.Add(1)
	return "approved-ran", nil
}

// delayApprover 审批器替身：等一段时间后放行（模拟「人在看弹窗」）。
//
// 刻意**不**实现 events.ApprovalPolicy：策略是可选接口，引擎在 Approver 实现它时用它
// 覆盖工具自声明。带策略的替身见 policyApprover（另一个类型）—— Go 的接口满足是静态的，
// 用一个 bool 字段「可选地实现接口」是做不到的（原写法让每个实例都实现了策略，连外层
// codemode 调用也被要求审批，用例直接在审批队列里挂住）。
type delayApprover struct {
	delay    time.Duration
	decision bool
	seen     atomic.Int64
}

func (a *delayApprover) BeginApproval(call core.ToolCall) func(context.Context) (bool, error) {
	a.seen.Add(1)
	return func(ctx context.Context) (bool, error) {
		select {
		case <-time.After(a.delay):
			return a.decision, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

func (a *delayApprover) BeginApprovalWithContext(ctx context.Context, call core.ToolCall) (func(context.Context) (bool, error), error) {
	return a.BeginApproval(call), nil
}

// policyApprover 带审批策略的替身：策略只覆盖**嵌套**的 needs_ok 调用
// （manual 模式对特定工具的强制审批），外层 codemode 调用照常放行。
type policyApprover struct {
	*delayApprover
}

func (a *policyApprover) NeedsApproval(call core.ToolCall) bool {
	return call.Name == "needs_ok" && call.ParentCallId != ""
}

// ---- 宿主装配 ----

// harness 一次端到端用例的装配：真引擎 + 真工具 + 真 node。
type harness struct {
	t      *testing.T
	engine *tools.Engine
	reg    []tools.Tool // Options.Tools 的来源（装配点手上的注册表）
	store  *Store
	tool   *Tool
	dir    string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, engine: tools.NewToolEngine(), dir: t.TempDir()}
	st, err := OpenStore(filepath.Join(h.dir, StoreFileName))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	h.store = st
	return h
}

// add 注册工具（并记进注册表 —— Options.Tools 返回的就是它）。
func (h *harness) add(ts ...tools.Tool) {
	h.t.Helper()
	for _, tl := range ts {
		h.engine.RegisterTool(context.Background(), tl)
		h.reg = append(h.reg, tl)
	}
}

// installCodemode 注册 codemode 工具本体（Options.Tools 指向本装配的注册表）。
func (h *harness) installCodemode(mod func(*Options)) *Tool {
	h.t.Helper()
	opts := Options{
		Engine: h.engine,
		Tools:  func() []tools.Tool { return h.reg },
		Exec:   execproc.New(nil, nil),
		Store:  h.store,
		Cwd:    h.dir,
	}
	if mod != nil {
		mod(&opts)
	}
	h.tool = New(opts)
	h.add(h.tool)
	return h.tool
}

// run 跑一次脚本（走生产入口 RunBatch），返回外层结果 + 捕获到的事件。
func (h *harness) run(ctx context.Context, code string) (core.ToolResult, []events.Event) {
	h.t.Helper()
	var mu sync.Mutex
	var got []events.Event
	handler := func(_ context.Context, ev events.Event) {
		mu.Lock()
		got = append(got, ev)
		mu.Unlock()
	}
	args, err := json.Marshal(map[string]string{"code": code})
	if err != nil {
		h.t.Fatalf("marshal args: %v", err)
	}
	results, err := h.engine.RunBatch(ctx,
		[]core.ToolCall{{Id: "outer-1", Name: ToolName, Arguments: string(args)}},
		handler, nil, nil, 0)
	if err != nil {
		h.t.Fatalf("RunBatch: %v", err)
	}
	if len(results) != 1 {
		h.t.Fatalf("results = %d, want 1", len(results))
	}
	mu.Lock()
	defer mu.Unlock()
	return results[0], got
}

// ---- ① 脚本跑通且值交回模型 ----

func TestScriptReadsFileAndReturnsValueToModel(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	read := newReadFileStub()
	h.add(read)
	h.installCodemode(nil)

	target := filepath.Join(h.dir, "a.txt")
	content := "hello codemode"
	if err := os.WriteFile(target, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	res, _ := h.run(context.Background(), fmt.Sprintf(
		"const r = await tools.read_file({path: %q});\nreturn r.length;", target))

	if res.IsError {
		t.Fatalf("结果被判为错误：%q", res.Result)
	}
	if read.calls.Load() != 1 {
		t.Fatalf("read_file 调用次数 = %d, want 1", read.calls.Load())
	}
	lines := strings.Split(strings.TrimRight(res.Result, "\n"), "\n")
	if got := lines[len(lines)-1]; got != strconv.Itoa(len(content)) {
		t.Fatalf("模型拿到的返回值 = %q, want %d\n结果文本:\n%s", got, len(content), res.Result)
	}
	if len(res.NestedCalls) != 1 || res.NestedCalls[0].Name != "read_file" {
		t.Fatalf("嵌套记录 = %+v, want 一条 read_file", res.NestedCalls)
	}
}

// ---- ② 脚本里必须拿到结构化对象，而不是 20 KB 文本 ----

func TestScriptGetsStructuredToolResultNotTruncatedText(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	const full = 30 << 10 // 30 KB：远超引擎对文本的 20 KB 截断
	bash := newBashStub(strings.Repeat("x", full))
	h.add(bash)
	h.installCodemode(nil)

	res, _ := h.run(context.Background(), `
const r = await tools.bash({command: "produce-30kb"});
return { kind: typeof r, keys: Object.keys(r).sort(), len: r.output.length, code: r.exit_code };
`)
	if res.IsError {
		t.Fatalf("结果被判为错误：%q", res.Result)
	}
	var got struct {
		Kind string   `json:"kind"`
		Keys []string `json:"keys"`
		Len  int      `json:"len"`
		Code int      `json:"code"`
	}
	if err := json.Unmarshal([]byte(lastJSONObject(t, res.Result)), &got); err != nil {
		t.Fatalf("解析脚本返回值失败: %v\n结果文本:\n%s", err, res.Result)
	}
	if got.Kind != "object" {
		t.Fatalf("脚本拿到的不是对象（typeof = %q）—— 结构化结果没走通，退化成文本了", got.Kind)
	}
	if want := []string{"exit_code", "output", "truncated"}; strings.Join(got.Keys, ",") != strings.Join(want, ",") {
		t.Fatalf("结构化字段 = %v, want %v", got.Keys, want)
	}
	// 这一条才是钉子：文本路径在引擎里被砍到 20 KB，脚本看到 30 KB 才证明它拿的是结构化原文。
	if got.Len != full {
		t.Fatalf("脚本读到的 output 长度 = %d, want %d（拿到的是被截断的文本？）", got.Len, full)
	}
}

// ---- ③ 记录与计费：3 次嵌套，外层拿全，且不双计 ----

func TestNestedCallsReachOuterResultAndAreBilledOnce(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	// 10 token/次 × 3 次 = 30，一眼看得出漏计/双计。
	h.add(&usageStub{
		BaseTool: tools.BaseTool{Name_: "leaf", Description_: "leaf",
			Params_: tools.Obj(map[string]any{"path": tools.Str("p")})},
		u: core.Usage{Input: 10, Output: 5, TotalTokens: 15},
	})
	h.installCodemode(nil)

	// 「会话成本」按 AgentLoop 的口径对拍：只累加**顶层** ToolResponse 的用量
	//（嵌套调用不经 runBatch，AgentLoop 的父累加看不到它们 —— 引擎已从 recorder
	// 合并过一次，编排方再报一次就会双计）。
	var sessionTokens int64
	handler := func(_ context.Context, ev events.Event) {
		tr, ok := ev.(*events.ToolResponse)
		if !ok || tr.Usage == nil || tr.ParentCallId != "" {
			return // 只累加**顶层**调用（AgentLoop 的口径：嵌套调用不经 runBatch）
		}
		sessionTokens += int64(tr.Usage.TotalTokens)
	}
	args, _ := json.Marshal(map[string]string{"code": `
const rs = await Promise.all([
  tools.leaf({path: "a"}), tools.leaf({path: "b"}), tools.leaf({path: "c"}),
]);
return rs.length;
`})
	results, err := h.engine.RunBatch(context.Background(),
		[]core.ToolCall{{Id: "outer-1", Name: ToolName, Arguments: string(args)}}, handler, nil, nil, 0)
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	res := results[0]
	if res.IsError {
		t.Fatalf("结果被判为错误：%q", res.Result)
	}
	if len(res.NestedCalls) != 3 {
		t.Fatalf("外层 NestedCalls = %d, want 3（记录没接上）", len(res.NestedCalls))
	}
	for i, c := range res.NestedCalls {
		if c.Name != "leaf" || c.Usage == nil || c.Usage.TotalTokens != 15 {
			t.Fatalf("第 %d 条记录字段不全：%+v", i, c)
		}
	}
	if got := int64(res.Usage.TotalTokens); got != 45 {
		t.Fatalf("外层 Usage.TotalTokens = %d, want 45（嵌套用量漏进/漏出）", got)
	}
	// 对拍：会话成本 == 各次嵌套用量之和（15×3），不是 90（那才是双计）。
	if sessionTokens != 45 {
		t.Fatalf("会话成本 = %d, want 45（各次嵌套用量之和；90 = 编排方又经 ToolUsageProvider 报了一次）",
			sessionTokens)
	}
	// 结构上也不许有第二条上报通道：本工具不得实现 ToolUsageProvider。
	if _, ok := any(h.tool).(tools.ToolUsageProvider); ok {
		t.Fatal("codemode 实现了 ToolUsageProvider —— 引擎已从 recorder 合并过用量，再报一次就是双计")
	}
}

// ---- ④ 死循环被 timeout_ms 打断：首行 TIMEOUT + 无残留进程 + 有界返回 ----

func TestWallClockTimeoutKillsLoopAndLeavesNoResidue(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	h.installCodemode(nil)

	pidFile := filepath.Join(h.dir, "pids")
	script := fmt.Sprintf(`// @options: {"timeout_ms": 700}
const { writeFileSync } = await import('node:fs');
const { spawn } = await import('node:child_process');
const kid = spawn('/bin/sh', ['-c', 'while true; do sleep 3600; done']);
writeFileSync(%q, process.pid + ' ' + kid.pid);
while (true) {}
`, pidFile)

	start := time.Now()
	res, _ := h.run(context.Background(), script)
	elapsed := time.Since(start)

	// 有界返回：墙钟 700ms + 沙箱三层 kill 预算（killWaitBudget 4s + escalation 2s）。
	if limit := 700*time.Millisecond + 6*time.Second; elapsed > limit {
		t.Fatalf("返回耗时 %v, want <= %v（文本=%q）", elapsed, limit, res.Result)
	}
	if !res.IsError {
		t.Fatalf("超时的调用必须判失败（IsError=true）: %q", res.Result)
	}
	first := strings.SplitN(res.Result, "\n", 2)[0]
	if !strings.HasPrefix(first, "[TIMEOUT after 700ms") {
		t.Fatalf("首行必须声明 TIMEOUT（§6.5 口径）: %q\n完整文本:\n%s", first, res.Result)
	}
	if !strings.Contains(res.Result, "nothing could be recovered") {
		t.Fatalf("拿不到输出时不得承诺「部分输出」: %s", res.Result)
	}
	if strings.Contains(res.Result, "the output below is PARTIAL") {
		t.Fatalf("无输出却承诺了 PARTIAL: %s", res.Result)
	}
	// 结构化结局：timed_out（不是 canceled —— 用户中断才是取消）。
	var structured codemodeResult
	if err := json.Unmarshal(res.Structured, &structured); err != nil {
		t.Fatalf("结构化结果解析失败: %v (%s)", err, res.Structured)
	}
	if !structured.TimedOut || structured.Canceled {
		t.Fatalf("结构化结局 = %+v, want TimedOut=true Canceled=false", structured)
	}

	// 无残留：脚本在死循环**之前**就写了 pid 文件，读不到即判失败（不是跳过）。
	pids := readPIDs(t, pidFile)
	if len(pids) == 0 {
		t.Fatalf("脚本没写出 pid 文件（%s），残留断言空转:\n%s", pidFile, res.Result)
	}
	waitNoLivePIDs(t, pids, 3*time.Second)
}

// ---- ④b 宿主默认上限（Options.ScriptTimeout）：未声明 timeout_ms 时的墙钟 ----

// TestHostScriptTimeoutAppliesWithoutOptionsLine 宿主配置的默认上限必须对
// 「未写 @options」的脚本生效（Wave 3 的 codemode.budget_seconds 走这一条）。
func TestHostScriptTimeoutAppliesWithoutOptionsLine(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	h.installCodemode(func(o *Options) { o.ScriptTimeout = func() time.Duration { return 700 * time.Millisecond } })

	start := time.Now()
	res, _ := h.run(context.Background(), "while (true) {}")
	elapsed := time.Since(start)

	if limit := 700*time.Millisecond + 6*time.Second; elapsed > limit {
		t.Fatalf("返回耗时 %v, want <= %v（宿主上限没生效？）", elapsed, limit)
	}
	first := strings.SplitN(res.Result, "\n", 2)[0]
	if !strings.HasPrefix(first, "[TIMEOUT after 700ms") {
		t.Fatalf("首行必须声明宿主上限（700ms）: %q\n完整文本:\n%s", first, res.Result)
	}
	var structured codemodeResult
	if err := json.Unmarshal(res.Structured, &structured); err != nil {
		t.Fatalf("结构化结果解析失败: %v (%s)", err, res.Structured)
	}
	if !structured.TimedOut || structured.Canceled {
		t.Fatalf("结构化结局 = %+v, want TimedOut=true Canceled=false", structured)
	}
}

// TestOptionsLineTimeoutBeatsHostDefault 脚本自己声明的 timeout_ms 压过宿主默认：
// 宿主 400ms 而脚本要跑 ~1.2s —— 只有「声明优先」成立时它才跑得完。
func TestOptionsLineTimeoutBeatsHostDefault(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	h.installCodemode(func(o *Options) { o.ScriptTimeout = func() time.Duration { return 400 * time.Millisecond } })

	script := `// @options: {"timeout_ms": 8000}
const { setTimeout: sleep } = await import('node:timers/promises');
await sleep(1200);
return "survived";
`
	res, _ := h.run(context.Background(), script)
	if res.IsError {
		t.Fatalf("脚本声明了 8s 上限却仍失败（宿主默认没被压过？）: %q", res.Result)
	}
	if !strings.Contains(res.Result, "survived") {
		t.Fatalf("结果里应有返回值: %q", res.Result)
	}
}

// ---- ⑤ 脚本抛错：已发生的调用记录在、副作用不回滚 ----

func TestScriptErrorKeepsEarlierCallsAndSideEffects(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	write := newWriteStub()
	h.add(write)
	h.installCodemode(nil)

	target := filepath.Join(h.dir, "written.txt")
	res, _ := h.run(context.Background(), fmt.Sprintf(`
await tools.write_file({path: %q, content: "side-effect"});
throw new Error("boom");
`, target))

	if !res.IsError {
		t.Fatalf("脚本抛错必须判失败: %q", res.Result)
	}
	if !strings.Contains(res.Result, "[script error]") || !strings.Contains(res.Result, "boom") {
		t.Fatalf("结果文本必须带上脚本错误: %s", res.Result)
	}
	if len(res.NestedCalls) != 1 || res.NestedCalls[0].Name != "write_file" || res.NestedCalls[0].IsError {
		t.Fatalf("已发生的调用记录必须保留: %+v", res.NestedCalls)
	}
	// 副作用不回滚（对齐 pi：脚本中途失败不撤销已执行的调用）。
	if b, err := os.ReadFile(target); err != nil || string(b) != "side-effect" {
		t.Fatalf("已发生的副作用被回滚/丢失了: content=%q err=%v", b, err)
	}
	if write.calls.Load() != 1 {
		t.Fatalf("write_file 调用次数 = %d, want 1", write.calls.Load())
	}
}

// ---- ⑥ hidden / model-only 不可被脚本调用 ----

func TestHiddenAndModelOnlyAreNotCallableFromScript(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	gone := newTierStub("gone_tool", tools.ExposureHidden)
	self := newTierStub("ghost_tool", tools.ExposureModelOnly)
	deferred := newTierStub("mcp__dev__latent", tools.ExposureDeferred)
	h.add(gone, self, deferred)
	h.installCodemode(nil)

	// 分派表（目录表）本身就不含这两档 —— 这是「撤下工具」与「禁自嵌套」的物理载体。
	cat, err := h.tool.catalog()
	if err != nil {
		t.Fatalf("catalog: %v", err)
	}
	for _, name := range []string{"gone_tool", "ghost_tool"} {
		if _, ok := cat.Names[name]; ok {
			t.Fatalf("分派表含 %q —— hidden/model-only 不该可编排", name)
		}
	}
	if _, ok := cat.Names["mcp__dev__latent"]; !ok {
		t.Fatalf("分派表缺 deferred 工具（可编排但不列举）: %v", cat.Names)
	}
	if strings.Contains(cat.Description, "gone_tool") || strings.Contains(cat.Description, "ghost_tool") {
		t.Fatalf("hidden/model-only 泄漏进描述:\n%s", cat.Description)
	}

	// 脚本侧：调用它们必须**当场**失败（传输层的 tools 代理会给一条可 catch 的错误），
	// 且工具从未被执行过（分派没走到引擎）。
	res, _ := h.run(context.Background(), `
const out = {};
for (const name of ["gone_tool", "ghost_tool"]) {
  try { await tools[name]({}); out[name] = "RAN"; }
  catch (e) { out[name] = String(e.message).slice(0, 60); }
}
return out;
`)
	body := lastJSONObject(t, res.Result)
	if strings.Contains(body, "RAN") {
		t.Fatalf("hidden/model-only 工具被脚本执行了: %s", body)
	}
	if !strings.Contains(body, "unknown tool") {
		t.Fatalf("拒绝原因不可读（模型该看到「不在工具表里」）: %s", body)
	}
	if gone.runs.Load() != 0 || self.runs.Load() != 0 {
		t.Fatalf("hidden/model-only 被执行过: gone=%d ghost=%d", gone.runs.Load(), self.runs.Load())
	}
}

// ---- ⑦ 描述字节稳定（MCP 连接/断开前后逐字节相同）----

func TestDescriptionBytesStableAcrossMCPChurn(t *testing.T) {
	t.Parallel() // 纯描述生成，不需要 node
	h := newHarness(t)
	h.add(newReadFileStub(), newTierStub("mcp__dev__alpha", tools.ExposureDeferred))
	h.installCodemode(nil)

	snapshot := func() (string, string) {
		params, err := h.engine.ToolParams(context.Background())
		if err != nil {
			t.Fatalf("ToolParams: %v", err)
		}
		raw, err := json.Marshal(params)
		if err != nil {
			t.Fatalf("marshal ToolParams: %v", err)
		}
		desc, err := h.engine.GetTool(context.Background(), ToolName)
		if err != nil {
			t.Fatalf("GetTool: %v", err)
		}
		return string(raw), desc.Description()
	}

	params0, desc0 := snapshot()

	// MCP「连接」：多一个 deferred 工具（可编排、不进描述、不声明给模型）。
	beta := newTierStub("mcp__dev__beta", tools.ExposureDeferred)
	h.add(beta)
	params1, desc1 := snapshot()
	if desc1 != desc0 {
		t.Fatalf("MCP 连接后 codemode 描述字节变了（前缀缓存击穿）:\n--- before\n%s\n--- after\n%s", desc0, desc1)
	}
	if params1 != params0 {
		t.Fatalf("MCP 连接后 ToolParams 字节变了:\n--- before\n%s\n--- after\n%s", params0, params1)
	}
	if strings.Contains(params1, "mcp__dev__beta") {
		t.Fatal("deferred 工具被声明给模型了（deferred 由 tool_search 现查，不该进 ToolParams）")
	}

	// MCP「断开」：同名重注册为 hidden（撤下工具的唯一手段）。
	h.engine.RegisterTool(context.Background(), newTierStub("mcp__dev__beta", tools.ExposureHidden))
	h.reg[len(h.reg)-1] = newTierStub("mcp__dev__beta", tools.ExposureHidden)
	params2, desc2 := snapshot()
	if desc2 != desc0 {
		t.Fatalf("MCP 断开后 codemode 描述字节变了:\n--- before\n%s\n--- after\n%s", desc0, desc2)
	}
	if params2 != params0 {
		t.Fatalf("MCP 断开后 ToolParams 字节变了:\n--- before\n%s\n--- after\n%s", params0, params2)
	}

	// mode:"on" 的 loadout 片段只改**该条自己**，别条逐字节不变。
	frags := h.tool.PrepareLoadout(context.Background(), mustSchemas(t, params0))
	again := h.tool.PrepareLoadout(context.Background(), mustSchemas(t, params0))
	if !equalSchemas(frags, again) {
		t.Fatal("PrepareLoadout 两次调用结果不同（必须字节稳定）")
	}
	if len(frags) != len(mustSchemas(t, params0)) {
		t.Fatal("PrepareLoadout 不得增删条目")
	}
	for i, s := range frags {
		orig := mustSchemas(t, params0)[i]
		if s.Name != orig.Name {
			t.Fatalf("条目名被改了: %q → %q", orig.Name, s.Name)
		}
		switch s.Name {
		case "read_file":
			if !strings.Contains(s.Description, "tools.read_file({...})") {
				t.Fatalf("mode:on 必须给已声明工具追加脚本调用片段: %q", s.Description)
			}
		case ToolName:
			if s.Description != orig.Description {
				t.Fatal("codemode 自己是 model-only（不可编排），不该被追加「也能从脚本调」")
			}
		default:
			if s.Description != orig.Description {
				t.Fatalf("第 %d 条（%s）的描述被改了 —— 片段只能依赖该条自己的名字", i, s.Name)
			}
		}
	}
}

// ---- ⑧ store：成功才落盘 ----

func TestStorePersistsOnlyOnSuccess(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	h.installCodemode(nil)

	// 成功：立刻可读（本地视图）+ 落盘。
	res, _ := h.run(context.Background(), `
store("k1", {n: 1});
const seen = load("k1");
store("k2", "two");
return { seen: seen, all: Object.keys(load()).sort() };
`)
	if res.IsError {
		t.Fatalf("成功脚本被判失败: %q", res.Result)
	}
	if got := h.store.Values(); len(got) != 2 || string(got["k2"]) != `"two"` {
		t.Fatalf("成功后 store 应有两个键: %+v", got)
	}
	// 落盘：重新打开文件 replay 一遍（不是读内存态）。
	reopened, err := OpenStore(h.store.Path())
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	if got := reopened.Values(); len(got) != 2 {
		t.Fatalf("落盘后重新 replay 得到 %d 个键, want 2: %+v", len(got), got)
	}
	if !strings.Contains(lastJSONObject(t, res.Result), `"k1"`) {
		t.Fatalf("脚本内 load 应看得见自己刚写的值: %s", res.Result)
	}

	// 失败：写入不落盘（脚本先写、后抛）。
	before, err := os.ReadFile(h.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	res2, _ := h.run(context.Background(), `
store("fail-key", "should-not-persist");
throw new Error("nope");
`)
	if !res2.IsError {
		t.Fatalf("脚本抛错必须判失败: %q", res2.Result)
	}
	after, err := os.ReadFile(h.store.Path())
	if err != nil {
		t.Fatal(err)
	}
	if string(before) != string(after) {
		t.Fatalf("失败脚本的写入落盘了:\n--- before\n%s\n--- after\n%s", before, after)
	}
	if _, ok := h.store.Values()["fail-key"]; ok {
		t.Fatal("失败脚本的写入进了 store")
	}
}

// TestOversizedStoreWriteFailsInsideTheScript 单值超限必须在**脚本内同步失败**（可 catch），
// 而不是等脚本跑完才被判「写入没生效」—— 教学正文说 "exceeding either fails the call"，
// 越早失败越可行动。限额数值由宿主经 init 送进沙箱（Go 常量是唯一真源）。
func TestOversizedStoreWriteFailsInsideTheScript(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	h.installCodemode(nil)

	res, _ := h.run(context.Background(), `
let msg = "";
try { await store("big", "x".repeat(300000)); msg = "ACCEPTED"; }
catch (e) { msg = String(e.message); }
return { msg: msg, seen: load("big") === undefined ? "absent" : "present" };
`)
	if res.IsError {
		t.Fatalf("脚本自己 catch 了，不该判失败: %q", res.Result)
	}
	body := lastJSONObject(t, res.Result)
	if strings.Contains(body, "ACCEPTED") {
		t.Fatalf("超限写入被接受了: %s", body)
	}
	if !strings.Contains(body, "256 KiB") || !strings.Contains(body, "absent") {
		t.Fatalf("拒绝原因不可读 / 本地视图被污染: %s", body)
	}
	if _, ok := h.store.Values()["big"]; ok {
		t.Fatal("超限写入进了 store")
	}
}

// ---- 取消钉子：脚本结束时仍在跑的调用被取消（可兑现的那一半）----

func TestScriptErrorCancelsInFlightCall(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	slow := newSlowStub("slow_tool", 30*time.Second)
	h.add(slow)
	h.installCodemode(nil)

	// 不 await 的调用先发出去（call 帧在调用时刻就发出去了），脚本随后抛错。
	res, _ := h.run(context.Background(), `
tools.slow_tool({x: 1});
throw new Error("die-now");
`)
	if !res.IsError {
		t.Fatalf("脚本抛错必须判失败: %q", res.Result)
	}
	select {
	case <-slow.done:
		// 好：在途调用收到取消信号。
	case <-time.After(5 * time.Second):
		t.Fatalf("脚本报错后仍在跑的调用没有被取消（教学正文承诺 calls that are still running "+
			"are cancelled）\n结果文本:\n%s", res.Result)
	}
	if slow.finished.Load() {
		t.Fatal("被判为「跑完了」—— 取消没生效")
	}
}

func TestExitCancelsInFlightCall(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	slow := newSlowStub("slow_tool", 30*time.Second)
	h.add(slow)
	h.installCodemode(nil)

	// exit(v) = 传输层的「干净提前收尾」：脚本结束得比那条调用早。
	res, _ := h.run(context.Background(), `
tools.slow_tool({x: 1});
exit("stopped-early");
`)
	if res.IsError {
		t.Fatalf("exit() 是干净的收尾路径，不该判失败: %q", res.Result)
	}
	if !strings.Contains(res.Result, "stopped-early") {
		t.Fatalf("exit(v) 的值应成为结果: %s", res.Result)
	}
	select {
	case <-slow.done:
	case <-time.After(5 * time.Second):
		t.Fatalf("exit() 之后仍在跑的调用没有被取消:\n%s", res.Result)
	}
}

// TestUnawaitedCallAfterPlainReturnPinsTransportLimit 把**已知边界**钉在这里（不是期望行为）。
//
// 脚本 `return` 之后仍有一条未被 await 的调用在跑时，沙箱进程会一直等那条调用的回执
// （prelude_bridge.js 的保活曲线：pending>0 ⇒ ref 住 stdin ⇒ 不退出），ExecuteScript 也就
// 不返回 —— 这个窗口里宿主**没有**「脚本已结束」的信号（传输层没有 pi 的 done 帧）。
// 结论：调用会跑完、本次 codemode 调用要等它（结果是「跑完」，不是「被取消」）。
//
// 修法在传输层（脚本结束时补一条收尾帧）；那一天这条用例要**翻转**成「被取消」。
func TestUnawaitedCallAfterPlainReturnPinsTransportLimit(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	slow := newSlowStub("slow_tool", 300*time.Millisecond)
	h.add(slow)
	h.installCodemode(nil)

	start := time.Now()
	res, _ := h.run(context.Background(), `
tools.slow_tool({x: 1});
return "returned-before-the-call-finished";
`)
	elapsed := time.Since(start)
	if res.IsError {
		t.Fatalf("调用没抛错，不该判失败: %q", res.Result)
	}
	if !slow.finished.Load() {
		t.Fatal("当前传输层语义下这条调用会跑完；若它被取消了，说明修法已落地 —— 请翻转本用例")
	}
	if elapsed < 300*time.Millisecond {
		t.Fatalf("返回耗时 %v：比那条未 await 的调用还短，说明宿主没有等回执（语义变了，请复核）", elapsed)
	}
}

// ---- scaffold 装的全局：逐个点名（含 image 的非法输入必须当场拒掉）----

func TestScaffoldGlobalsAreInstalledAndWork(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	h.add(newReadFileStub(), newTierStub("gone_tool", tools.ExposureHidden))
	h.installCodemode(func(o *Options) {
		o.Namespaces = func() map[string]*tools.ToolNamespace {
			return map[string]*tools.ToolNamespace{
				"fs": {Name: "fs", Description: "filesystem tools",
					Instructions: "Long-form instructions that must NOT go into the description."},
			}
		}
	})
	// 给 read_file 挂一个 namespace（describeTool 的长指引从 namespace 读）。
	h.reg[0].(*readFileStub).BaseTool = tools.BaseTool{
		Name_:        "read_file",
		Description_: "Read a file and return its contents as text.",
		Params_:      tools.Obj(map[string]any{"path": tools.Str("file path")}, "path"),
		CanParallel_: true,
		ReadOnly_:    true,
	}
	nsTool := &nsReadFileStub{readFileStub: *newReadFileStub()}
	h.engine.RegisterTool(context.Background(), nsTool)
	h.reg[0] = nsTool

	pngPath := filepath.Join(h.dir, "pic.png")
	var buf strings.Builder
	_ = buf
	writeTinyPNG(t, pngPath)

	res, _ := h.run(context.Background(), fmt.Sprintf(`
const kinds = {
  text: typeof text, image: typeof image, exit: typeof exit, store: typeof store,
  load: typeof load, searchTools: typeof searchTools, describeTool: typeof describeTool,
  consoleLog: typeof console.log, toolsRead: typeof tools.read_file,
};
text("[text() output line]");
const hits = await searchTools("read");
const one = await describeTool("read_file");
const img = await image(%q);
return { kinds: kinds, hits: hits, one: one, img: img };
`, pngPath))

	if res.IsError {
		t.Fatalf("脚本被判失败: %q", res.Result)
	}
	if !strings.Contains(res.Result, "[text() output line]") {
		t.Fatalf("text() 的内容必须出现在结果里:\n%s", res.Result)
	}
	var got struct {
		Kinds map[string]string `json:"kinds"`
		Hits  []searchHit       `json:"hits"`
		One   describeReply     `json:"one"`
		Img   imageReply        `json:"img"`
	}
	if err := json.Unmarshal([]byte(lastJSONObject(t, res.Result)), &got); err != nil {
		t.Fatalf("解析返回值: %v\n%s", err, res.Result)
	}
	for _, name := range []string{"text", "image", "exit", "store", "load", "searchTools", "describeTool", "consoleLog"} {
		if got.Kinds[name] != "function" {
			t.Errorf("scaffold 未装上全局 %s（typeof = %q）", name, got.Kinds[name])
		}
	}
	if got.Kinds["toolsRead"] != "function" {
		t.Errorf("tools.read_file 不可用（typeof = %q）", got.Kinds["toolsRead"])
	}
	if len(got.Hits) == 0 || got.Hits[0].Name != "read_file" {
		t.Fatalf("searchTools(\"read\") 命中 = %+v, want read_file", got.Hits)
	}
	for _, hit := range got.Hits {
		if strings.Contains(hit.Name, "gone_tool") {
			t.Fatalf("searchTools 泄漏了 hidden 工具: %+v", hit)
		}
	}
	if got.One.Description == "" || len(got.One.Params) != 1 || got.One.Params[0].Name != "path" {
		t.Fatalf("describeTool 结果不全: %+v", got.One)
	}
	if got.One.NamespaceInstructions == "" || strings.Contains(got.One.Description, "Long-form instructions") {
		t.Fatalf("namespace 的 instructions 只能经 describeTool 给，不能进描述: %+v", got.One)
	}
	if got.Img.Mime != "image/png" || got.Img.Bytes == 0 {
		t.Fatalf("image() 回执不对: %+v", got.Img)
	}
	if blocks := res.ImageBlocks(); len(blocks) != 1 || blocks[0].MimeType != "image/png" {
		t.Fatalf("图片没进结果块: %+v", res.ImageBlocks())
	}
}

// TestBadImageIsRejectedBeforeItReachesTheModel 不变量（pi #10215）：坏图片必须**当场**
// 拒掉，绝不能进对话历史 —— 一个坏块会让之后每一个请求 400。
func TestBadImageIsRejectedBeforeItReachesTheModel(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	h.installCodemode(nil)

	bad := filepath.Join(h.dir, "not-an-image.txt")
	if err := os.WriteFile(bad, []byte("plain text, not an image"), 0o600); err != nil {
		t.Fatal(err)
	}
	res, _ := h.run(context.Background(), fmt.Sprintf(`
let msg = "";
try { await image(%q); msg = "ACCEPTED"; }
catch (e) { msg = String(e.message); }
return { msg: msg };
`, bad))
	if res.IsError {
		t.Fatalf("脚本自己 catch 了，不该判失败: %q", res.Result)
	}
	body := lastJSONObject(t, res.Result)
	if strings.Contains(body, "ACCEPTED") {
		t.Fatalf("坏图片被接受了: %s", body)
	}
	if !strings.Contains(body, "not a supported image") {
		t.Fatalf("拒绝原因不可读: %s", body)
	}
	if blocks := res.ImageBlocks(); len(blocks) != 0 {
		t.Fatalf("坏图片进了结果块: %+v", blocks)
	}
}

// ---- 并发：只读真并发 / 写类串行 ----

func TestReadOnlyCallsRunConcurrently(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	const sleep = 80 * time.Millisecond
	probe := newSleepStub("read_probe", sleep)
	h.add(probe)
	h.installCodemode(nil)

	res, _ := h.run(context.Background(), `
const t0 = Date.now();
const ps = [];
for (let i = 0; i < 5; i++) ps.push(tools.read_probe({x: i}));
await Promise.all(ps);
return Date.now() - t0;
`)
	if res.IsError {
		t.Fatalf("并发脚本被判失败: %q", res.Result)
	}
	parallelMs, err := strconv.Atoi(lastLine(t, res.Result))
	if err != nil {
		t.Fatalf("脚本没返回毫秒数: %v\n%s", err, res.Result)
	}
	// 阈值依据：5 × 80ms 串行 ≈ 400ms；真并发的上界是「一次 80ms + 一点点调度开销」。
	// 取串行的一半（200ms）当阈值 —— 既不会被 Go/Node 的几毫秒抖动误伤，也能钉住
	// 「串行门错把所有调用都锁上」与「分派 goroutine 变成串行」这两类回归。
	if parallelMs >= 200 {
		t.Fatalf("5 个只读调用的脚本内耗时 %dms，接近串行（400ms）—— 并发分派没生效\n%s",
			parallelMs, res.Result)
	}
	if got := probe.peakInflight(); got < 4 {
		t.Fatalf("观测到的并发峰值 = %d, want >= 4（宿主侧确实同时有 5 个调用在跑）", got)
	}
}

func TestWriteToolsAreSerialized(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	write := newWriteStub()
	h.add(write)
	h.installCodemode(nil)

	res, _ := h.run(context.Background(), fmt.Sprintf(`
const ps = [];
for (let i = 0; i < 3; i++) ps.push(tools.write_file({path: %q + i, content: "x"}));
const rs = await Promise.allSettled(ps);
return rs.length;
`, filepath.Join(h.dir, "w")))
	if res.IsError {
		t.Fatalf("写脚本被判失败: %q", res.Result)
	}
	if got := write.peakInflight(); got != 1 {
		t.Fatalf("写类工具并发峰值 = %d, want 1（ExecScope 串行门失效 —— Promise.all 让两个写类调用交错了）", got)
	}
}

// ---- 审批等待不消耗墙钟 ----

func TestApprovalWaitDoesNotConsumeWallClock(t *testing.T) {
	requireNode(t)
	// 脚本必须**以 @options 行开头**（前导空行 = 没声明，见 ParseOptionsLine）——
	// 这一点是这个用例的命门：声明没生效时墙钟是 30s，用例会静默变成空转（已实测踩到）。
	const script = "// @options: {\"timeout_ms\": 400}\n" +
		"const r = await tools.needs_ok({x: 1});\n" +
		"return { ran: r, seen: true };"

	for _, withPolicy := range []bool{false, true} {
		name := "self-declared"
		if withPolicy {
			name = "policy"
		}
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			ap := newApprovalStub("needs_ok")
			h.add(ap)
			h.installCodemode(nil)

			approver := &delayApprover{delay: 900 * time.Millisecond, decision: true}
			var injected events.Approver = approver
			if withPolicy {
				injected = &policyApprover{delayApprover: approver}
			}
			ctx := events.WithToolContextForRunTask(context.Background(), "run-1", "", "", 0, nil, injected, nil)

			// 墙钟 400ms < 审批等待 900ms：暂停生效则脚本活着跑完，不生效则必被 [TIMEOUT] 杀掉。
			res, _ := h.run(ctx, script)
			if res.IsError {
				t.Fatalf("审批等待吃掉了墙钟（脚本被超时杀掉）: %q", res.Result)
			}
			if !strings.Contains(res.Result, "approved-ran") {
				t.Fatalf("脚本没跑完: %s", res.Result)
			}
			if ap.calls.Load() != 1 {
				t.Fatalf("审批通过的工具应执行一次，实际 %d", ap.calls.Load())
			}
			if approver.seen.Load() == 0 {
				t.Fatal("approver 没被调用 —— 包装层把审批通道吞了")
			}
			// 真实墙钟必须**超过**预算（证明暂停真的发生了，而不是预算没生效）。
			var st codemodeResult
			if err := json.Unmarshal(res.Structured, &st); err != nil {
				t.Fatalf("结构化结果: %v", err)
			}
			if st.WallTimeSec < 0.9 {
				t.Fatalf("真实墙钟 %.2fs < 审批等待 0.9s，说明没有真的等（用例失去意义）", st.WallTimeSec)
			}
			if st.TimedOut {
				t.Fatalf("结构化结果说超时了: %+v", st)
			}
		})
	}

	// 对照组：同一条 @options 行 + 一个死循环 → 必须在 400ms 左右被杀。
	//
	// 为什么必须有这一半：@options 行**必须是脚本第一行**（前导空行 = 没声明），声明没生效时
	// 墙钟是 30s，上面那两条「脚本活下来了」的断言会静默变成空转 —— 本用例的第一版就是这么
	// 空转的（复核实测：变异「去掉审批暂停」竟然没 FAIL）。有了这一半，「预算真的装上了」
	// 才有证据。
	t.Run("options-line-is-armed", func(t *testing.T) {
		h := newHarness(t)
		h.installCodemode(nil)
		start := time.Now()
		res, _ := h.run(context.Background(), "// @options: {\"timeout_ms\": 400}\nwhile (true) {}")
		elapsed := time.Since(start)
		if !res.IsError {
			t.Fatalf("死循环必须被杀: %q", res.Result)
		}
		if !strings.HasPrefix(res.Result, "[TIMEOUT after 400ms") {
			t.Fatalf("首行应是 TIMEOUT after 400ms（说明 @options 没生效）: %q", res.Result)
		}
		if elapsed > 6*time.Second {
			t.Fatalf("返回耗时 %v 太长（杀进程预算失控）", elapsed)
		}
	})
}

// ---- 输出预算 + spill ----

func TestOutputBudgetTruncatesAndSpillsOutsideWorkspace(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	h.installCodemode(nil)

	// 2000 token ≈ 8000 字节预算；脚本返回 20 KB 的一行 JSON（折叠态预览最怕的形态）。
	// 注意：@options 行必须是脚本的**第一行**（前导空行 = 没声明，见 ParseOptionsLine）。
	res, _ := h.run(context.Background(),
		"// @options: {\"max_output_tokens\": 2000}\nconst big = \"y\".repeat(20000);\nreturn { big: big };")
	if res.IsError {
		t.Fatalf("脚本被判失败: %q", res.Result)
	}
	if len(res.Result) > 9000 {
		t.Fatalf("结果文本 %d 字节，超过预算（2000 token ≈ 8000 字节）+ spill 通知的余量", len(res.Result))
	}
	// 前端 SPILL_PATTERNS[0] 的**逐字**副本（源：desktop/app/src/lib/codemodeNested.ts:287-291）。
	// 拷贝而不是引用是有意的：这一条要钉住的是**跨语言契约**（我的输出 vs 前端的识别模式），
	// 前端改了模式，这条用例会红 —— 那就是「该同步对齐」的信号。
	spillRe := regexp.MustCompile(`\[Full output:\s*([^\s\]]+)`)
	m := spillRe.FindStringSubmatch(res.Result)
	if m == nil {
		t.Fatalf("结果文本没有点名 spill 路径（前端 SPILL_PATTERNS 认不出）:\n%s", res.Result)
	}
	path := m[1]
	// 前端 normalizePath 的两条判据：无空白、且「像路径」（含分隔符/盘符/扩展名）。
	if path == "" || strings.ContainsAny(path, " \t\n") || !regexp.MustCompile(`[/\\]|[A-Za-z]:|\.\w{1,8}$`).MatchString(path) {
		t.Fatalf("spill 路径形态前端认不出来: %q", path)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("spill 文件不存在: %v", err)
	}
	// 刻意写**字面量** 0600 而不是 spillFileMode：拿实现常量去比实现，改权限时两边一起变，
	// 用例就成了空转（本仓的既有教训：断言必须钉住产品事实，不能钉住实现）。
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("spill 权限 = %o, want 0600（脚本输出含私有数据）", perm)
	}
	if strings.HasPrefix(path, h.dir) {
		t.Fatalf("spill 落在了工作区里: %s", path)
	}
	full, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(full), strings.Repeat("y", 20000)) {
		t.Fatal("spill 文件里必须是未截断全文")
	}
	// 结构化通道也要给路径（前端的「优先结构化」通道）。
	var structured codemodeResult
	if err := json.Unmarshal(res.Structured, &structured); err != nil {
		t.Fatalf("结构化结果: %v", err)
	}
	if !structured.Truncated || structured.FullOutputPath != path {
		t.Fatalf("结构化结果 = %+v, want truncated=true path=%s", structured, path)
	}
}

// TestSkipTruncateLetsScriptOutputPastEngineLimit 引擎的 20 KB 无条件截断对编排型工具
// 豁免：脚本已经按 max_output_tokens 筛过一遍，被引擎再砍一刀就是「模型要求 30 KB，
// 实际只给 20 KB」的违约（设计文档 §4.2 阻断项 B）。
func TestSkipTruncateLetsScriptOutputPastEngineLimit(t *testing.T) {
	requireNode(t)
	h := newHarness(t)
	h.installCodemode(nil)

	// 30000 字节 < 10000 token × 4 = 40000 字节预算，故该**原样**回给模型。
	res, _ := h.run(context.Background(),
		"// @options: {\"max_output_tokens\": 10000}\nreturn \"z\".repeat(30000);")
	if res.IsError {
		t.Fatalf("脚本被判失败: %q", res.Result)
	}
	if len(res.Result) < 30000 {
		t.Fatalf("结果被截到 %d 字节（< 30000）—— 引擎的 20 KB 截断没被豁免", len(res.Result))
	}
	if !strings.Contains(res.Result, strings.Repeat("z", 1000)) {
		t.Fatal("结果里看不到脚本的返回值")
	}
}

// ---- 未跑起来（node 缺失/握手不兼容）必须是 error，而不是「脚本写错了」----

func TestSandboxUnavailableIsErrorNotScriptFailure(t *testing.T) {
	t.Setenv("GO_CODE_NODE_CANDIDATES", "/nonexistent/node")
	h := newHarness(t)
	h.installCodemode(nil)
	_, err := h.runExpectError(context.Background(), "return 1;")
	if err == nil {
		t.Fatal("node 不可用时 ExecuteScript 应当返回 error（调用方据此把 codemode 标为不可用）")
	}
}

// ---- 小工具 ----

// runExpectError 跑一次脚本并期望工具层直接报 error（工具链断裂，不是脚本写错）。
func (h *harness) runExpectError(ctx context.Context, code string) (core.ToolResult, error) {
	h.t.Helper()
	args, _ := json.Marshal(map[string]string{"code": code})
	res, err := h.engine.Sequence(ctx, []core.ToolCall{{Id: "outer-1", Name: ToolName, Arguments: string(args)}}, nil)
	if err != nil {
		return core.ToolResult{}, err
	}
	if len(res) != 1 || !res[0].IsError {
		return core.ToolResult{}, nil
	}
	return res[0], errors.New(res[0].Result)
}

// lastLine 最后一行非空文本（脚本 return 的标量就渲染在末尾）。
func lastLine(t *testing.T, text string) string {
	t.Helper()
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	if len(lines) == 0 {
		t.Fatalf("结果文本为空")
	}
	return lines[len(lines)-1]
}

// lastJSONObject 取结果文本里那个**整段合法**的 JSON 对象/数组（脚本 return 值的渲染形态）。
//
// 从头扫：脚本输出里可能有别的括号（例如 text("[x]") 或 console.log("{")），只认能整段
// 解析成 JSON 的那个 —— 猜括号配平是猜不准的，让 json.Valid 当裁判。
func lastJSONObject(t *testing.T, text string) string {
	t.Helper()
	for i := 0; i < len(text); i++ {
		if text[i] != '{' && text[i] != '[' {
			continue
		}
		if v, ok := jsonValueAt(text[i:]); ok {
			return v
		}
	}
	t.Fatalf("结果文本里找不到 JSON 对象/数组:\n%s", text)
	return ""
}

// jsonValueAt 从 s[0] 起取第一个完整且合法的 JSON 值（括号要配平、字符串内的括号不计）。
func jsonValueAt(s string) (string, bool) {
	if s == "" || (s[0] != '{' && s[0] != '[') {
		return "", false
	}
	depth, inStr, esc := 0, false, false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{', '[':
			depth++
		case '}', ']':
			depth--
			if depth == 0 {
				cand := s[:i+1]
				if json.Valid([]byte(cand)) {
					return cand, true
				}
				return "", false
			}
		}
	}
	return "", false
}

// readPIDs 读 pid 文件（"<pid> <pid>" 形态）。
func readPIDs(t *testing.T, path string) []int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var out []int
	for _, f := range strings.Fields(string(b)) {
		if n, err := strconv.Atoi(f); err == nil {
			out = append(out, n)
		}
	}
	return out
}

// pidAlive 进程是否还活着（ps -p）。
func pidAlive(pid int) bool {
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return false
	}
	return strings.Contains(string(out), strconv.Itoa(pid))
}

// waitNoLivePIDs 等所有 pid 消失（超时即失败）。
func waitNoLivePIDs(t *testing.T, pids []int, within time.Duration) {
	t.Helper()
	deadline := time.Now().Add(within)
	for {
		live := 0
		for _, p := range pids {
			if pidAlive(p) {
				live++
			}
		}
		if live == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("仍有 %d 个进程残留（%v）—— 进程组没被杀干净", live, pids)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// mustSchemas 把 ToolParams 的 JSON 快照解回 schema 列表。
func mustSchemas(t *testing.T, raw string) []core.ToolSchema {
	t.Helper()
	var out []core.ToolSchema
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		t.Fatalf("unmarshal ToolParams: %v", err)
	}
	return out
}

// equalSchemas 逐字段比较两个 schema 列表。
func equalSchemas(a, b []core.ToolSchema) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i].Name != b[i].Name || a[i].Description != b[i].Description {
			return false
		}
	}
	return true
}

// nsReadFileStub 带 namespace 的 read_file（describeTool 的 instructions 通道用）。
type nsReadFileStub struct{ readFileStub }

func (n *nsReadFileStub) Namespace() *tools.ToolNamespace {
	return &tools.ToolNamespace{
		Name:         "fs",
		Description:  "filesystem tools",
		Instructions: "Long-form instructions that must NOT go into the description.",
	}
}

// writeTinyPNG 写一张真 PNG（1×1 全透明）—— 用 Go 的编码器而不是硬编码 base64，
// 保证「合法图片」这件事在用例里是真的。
func writeTinyPNG(t *testing.T, path string) {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, image.NewRGBA(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatalf("encode png: %v", err)
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
