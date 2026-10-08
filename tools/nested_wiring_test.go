package tools

// nested_wiring_test.go：Phase 0 地基的**接线**测试。
//
// 与 nested_test.go（单测限额/串行门等纯逻辑）不同，本文件的每个用例都跑
// 「引擎注入 → 工具发起子调用 → 记录/用量/深度/审批回到调用方」这条**真实链路**。
//
// 为什么单独成文件：这四处缺口有一个共同特征 —— 在「测试手动把 recorder 塞进 ctx」
// 的假路径下全都看起来正常，一按真实编排形态使用（工具在自己的 Call 里读 ctx）
// 就全部失效。评审把它们逐条钉在这里。

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// ---- 1. 记录与嵌套用量必须回到外层 ToolResult ----

// leveragingTool 报告固定用量的子工具（模拟子 agent / 模型调用类工具）。
type leveragingTool struct {
	BaseTool
	u core.Usage
}

func (t *leveragingTool) Call(context.Context, string, string) (string, error) { return "sub-ok", nil }
func (t *leveragingTool) ValidParams(context.Context, string, string) error    { return nil }
func (t *leveragingTool) Usage() core.Usage                                    { return t.u }

// orchestratorTool 真实编排形态：在 Call 内**读** ctx 上的 recorder（引擎注入的那份，
// 不自建）并起子调用 —— 与 Phase 2 codemode 工具的唯一正确形态一致。
type orchestratorTool struct {
	BaseTool
	eng  *Engine
	subs []core.ToolCall
	errs []string
}

func (t *orchestratorTool) Call(ctx context.Context, _, _ string) (string, error) {
	if events.NestedRecorderFrom(ctx) == nil {
		t.errs = append(t.errs, "引擎未在工具 ctx 上注入 recorder（编排方无处拿记录）")
	}
	for _, c := range t.subs {
		r := t.eng.ExecuteOne(ctx, c, ExecuteOpts{ParentCallId: "outer-1", Depth: 1})
		if r.IsError {
			t.errs = append(t.errs, c.Id+": "+r.Result)
		}
	}
	return "script done", nil
}

func (t *orchestratorTool) ValidParams(context.Context, string, string) error { return nil }

// newOrchestratorEngine 建一个含「编排工具 + 3 条子调用目标」的引擎。
func newOrchestratorEngine(t *testing.T, orch *orchestratorTool) *Engine {
	t.Helper()
	e := NewToolEngine()
	// 10 token/次 × 3 次 = 30，便于一眼看出漏计/双计。
	e.RegisterTool(context.Background(), &leveragingTool{
		BaseTool: BaseTool{Name_: "leaf", Description_: "leaf", Params_: map[string]any{}},
		u:        core.Usage{Input: 10, Output: 5, TotalTokens: 15},
	})
	orch.BaseTool = BaseTool{Name_: "orch", Description_: "orch", Params_: map[string]any{}}
	orch.eng = e
	orch.subs = []core.ToolCall{
		{Id: "outer-1/1", Name: "leaf", Arguments: `{"path":"a"}`},
		{Id: "outer-1/2", Name: "leaf", Arguments: `{"path":"b"}`},
		{Id: "outer-1/3", Name: "leaf", Arguments: `{"path":"c"}`},
	}
	e.RegisterTool(context.Background(), orch)
	return e
}

// TestOrchestratorRecordsReachOuterResult 编排工具在 Call 内发起 3 次子调用 →
// 外层 ToolResult 必须拿到 3 条记录**与**合并后的用量。
//
// 这是「接得上」的核心钉子：Tool.Call 只返回 (string, error)，记录/用量回到外层的
// **唯一**通道是引擎注入的 recorder（events.EnsureNestedRecorder → finish 取出）。
func TestOrchestratorRecordsReachOuterResult(t *testing.T) {
	orch := &orchestratorTool{}
	e := newOrchestratorEngine(t, orch)

	r := e.ExecuteOne(context.Background(), core.ToolCall{Id: "outer-1", Name: "orch", Arguments: "{}"}, ExecuteOpts{})

	if len(orch.errs) > 0 {
		t.Fatalf("编排方拿不到 recorder 或子调用失败：%v", orch.errs)
	}
	if len(r.NestedCalls) != 3 {
		t.Fatalf("外层 NestedCalls = %d, want 3（记录没接上）", len(r.NestedCalls))
	}
	for i, want := range []string{"outer-1/1", "outer-1/2", "outer-1/3"} {
		if r.NestedCalls[i].Id != want {
			t.Fatalf("记录顺序/身份丢了：got[%d].Id = %q, want %q", i, r.NestedCalls[i].Id, want)
		}
		if r.NestedCalls[i].Name != "leaf" || r.NestedCalls[i].Usage == nil {
			t.Fatalf("记录字段不全：%+v", r.NestedCalls[i])
		}
	}
	// 计费：3 × 15 token 必须合并进外层结果（嵌套调用不经 runBatch，
	// AgentLoop 的父累加看不到它们 —— 这是唯一入口）。
	if got := r.Usage.TotalTokens; got != 45 {
		t.Fatalf("外层 Usage.TotalTokens = %d, want 45（嵌套用量漏进/漏出）", got)
	}
}

// TestOrchestratorUsageNotAccumulatedAcrossCalls 每次顶层调用独立记账：
// 第二次调用不得看到上一次的记录/用量（引擎每次调用注入**新** recorder）。
func TestOrchestratorUsageNotAccumulatedAcrossCalls(t *testing.T) {
	orch := &orchestratorTool{}
	e := newOrchestratorEngine(t, orch)

	first := e.ExecuteOne(context.Background(), core.ToolCall{Id: "outer-1", Name: "orch", Arguments: "{}"}, ExecuteOpts{})
	second := e.ExecuteOne(context.Background(), core.ToolCall{Id: "outer-2", Name: "orch", Arguments: "{}"}, ExecuteOpts{})

	if len(first.NestedCalls) != 3 || len(second.NestedCalls) != 3 {
		t.Fatalf("两次调用的记录数 = %d / %d, want 3 / 3", len(first.NestedCalls), len(second.NestedCalls))
	}
	if got := second.Usage.TotalTokens; got != 45 {
		t.Fatalf("第二次调用用量 = %d, want 45（跨调用累积了 = 双计）", got)
	}
}

// TestRunBatchOrchestratorRecordsReachOuterResult 生产入口（AgentLoop → RunBatch）：
// 记录/用量同样要落到结果**与事件**（ToolResponse.NestedCalls/Usage 是宿主的取数口）。
func TestRunBatchOrchestratorRecordsReachOuterResult(t *testing.T) {
	orch := &orchestratorTool{}
	e := newOrchestratorEngine(t, orch)

	var resp *events.ToolResponse
	h := func(_ context.Context, ev events.Event) {
		if tr, ok := ev.(*events.ToolResponse); ok {
			resp = tr
		}
	}
	results, err := e.RunBatch(context.Background(),
		[]core.ToolCall{{Id: "outer-1", Name: "orch", Arguments: "{}"}}, h, nil, nil, 0)
	if err != nil {
		t.Fatalf("RunBatch: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("results = %d, want 1", len(results))
	}
	if len(results[0].NestedCalls) != 3 {
		t.Fatalf("ToolResult.NestedCalls = %d, want 3", len(results[0].NestedCalls))
	}
	if got := results[0].Usage.TotalTokens; got != 45 {
		t.Fatalf("ToolResult.Usage.TotalTokens = %d, want 45", got)
	}
	if resp == nil {
		t.Fatal("未收到 ToolResponse 事件")
	}
	if len(resp.NestedCalls) != 3 {
		t.Fatalf("ToolResponse.NestedCalls = %d, want 3（宿主拿不到嵌套明细）", len(resp.NestedCalls))
	}
	if resp.Usage == nil || resp.Usage.TotalTokens != 45 {
		t.Fatalf("ToolResponse.Usage = %+v, want 45 total tokens", resp.Usage)
	}
}

// TestToolEventsCarryParentCallIdAndDepth 事件必须带上调用身份。
// Phase 2 的宿主按 ParentCallId 归组渲染；Phase 0 先钉住「字段能落地」——
// 走 runBatch 路径（ExecuteOne 按降级不发嵌套工具事件，见规范 §3.3）。
func TestToolEventsCarryParentCallIdAndDepth(t *testing.T) {
	e := newNestedEngine(t)
	var starts []*events.ToolStart
	var resps []*events.ToolResponse
	h := func(_ context.Context, ev events.Event) {
		switch v := ev.(type) {
		case *events.ToolStart:
			starts = append(starts, v)
		case *events.ToolResponse:
			resps = append(resps, v)
		}
	}
	calls := []core.ToolCall{{Id: "p1/1", Name: "count", Arguments: "{}", ParentCallId: "p1", Depth: 1}}
	if _, err := e.Sequence(context.Background(), calls, h); err != nil {
		t.Fatalf("Sequence: %v", err)
	}
	if len(starts) != 1 || len(resps) != 1 {
		t.Fatalf("事件数 = %d start / %d response, want 1 / 1", len(starts), len(resps))
	}
	if starts[0].ParentCallId != "p1" || starts[0].Depth != 1 {
		t.Fatalf("ToolStart 身份字段 = %q/%d, want p1/1", starts[0].ParentCallId, starts[0].Depth)
	}
	if resps[0].ParentCallId != "p1" || resps[0].Depth != 1 {
		t.Fatalf("ToolResponse 身份字段 = %q/%d, want p1/1", resps[0].ParentCallId, resps[0].Depth)
	}
}

// ---- 2. 调用嵌套深度必须接线到 ToolContext.Depth ----

// depthProbeTool 报告本次执行拿到的运行深度与 ctx 身份（改写必须原样保留后者）。
type depthProbeTool struct {
	BaseTool
	seen     int
	got      bool
	runID    string
	hasHooks bool
	hasHnd   bool
}

func (t *depthProbeTool) Call(ctx context.Context, _, _ string) (string, error) {
	tc := events.ToolContextFrom(ctx)
	t.got = tc != nil
	t.seen, t.runID, t.hasHooks, t.hasHnd = 0, "", false, false
	if tc != nil {
		t.seen, t.runID = tc.Depth, tc.RunId
		t.hasHooks, t.hasHnd = tc.Hooks != nil, tc.Handler != nil
	}
	return "ok", nil
}
func (t *depthProbeTool) ValidParams(context.Context, string, string) error { return nil }

// TestExecuteOnePublishesDepthToToolContext 规范 §3.2 第 12 条：opts.Depth 必须
// 出现在工具读到的 events.ToolContext.Depth 上（叠加在运行深度之上，不是覆盖）。
//
// 不接线的后果（设计文档 §20.1）：subagent 按 childDepth = tc.Depth + 1 判
// maxSpawnDepth，脚本因此能把嵌套整层隐掉，经 agent_spawn 拿到本不该有的委托深度。
func TestExecuteOnePublishesDepthToToolContext(t *testing.T) {
	e := NewToolEngine()
	p := &depthProbeTool{BaseTool: BaseTool{Name_: "probe", Description_: "probe", Params_: map[string]any{}}}
	e.RegisterTool(context.Background(), p)

	// (a) 无 ToolContext：嵌套深度即有效深度（规范里的场景）。
	e.ExecuteOne(context.Background(), core.ToolCall{Id: "a", Name: "probe"}, ExecuteOpts{Depth: 1})
	if !p.got || p.seen != 1 {
		t.Fatalf("无 ToolContext 时 Depth = %d (got=%v), want 1", p.seen, p.got)
	}

	// (b) 运行深度 1（子 agent 内）+ 嵌套 1 → 2：叠加，不能是 1（丢运行深度）或 0。
	// 同时断言 ctx 改写**原样保留**运行身份/handler/hooks（漏一个就是静默降级：
	// 子 agent 派孙任务投不回本 loop、宿主收不到事件、hooks 失效）。
	h := func(context.Context, events.Event) {}
	runCtx := events.WithToolContext(context.Background(), "run1", 1, h, nil, &events.ToolHooks{})
	e.ExecuteOne(runCtx, core.ToolCall{Id: "b", Name: "probe"}, ExecuteOpts{Depth: 1})
	if p.seen != 2 {
		t.Fatalf("运行深度 1 + 嵌套 1 → Depth = %d, want 2（叠加而非覆盖）", p.seen)
	}
	if p.runID != "run1" || !p.hasHooks || !p.hasHnd {
		t.Fatalf("ctx 改写丢了运行身份：runID=%q hooks=%v handler=%v", p.runID, p.hasHooks, p.hasHnd)
	}

	// (c) Depth = 0（顶层）：不改写运行深度。
	e.ExecuteOne(runCtx, core.ToolCall{Id: "c", Name: "probe"}, ExecuteOpts{})
	if p.seen != 1 {
		t.Fatalf("Depth=0 时 Depth = %d, want 1（未被改写）", p.seen)
	}
}

// TestExecuteOneDepthRewriteKeepsApprover ctx 改写不能把审批器丢掉
// （丢了就是越权：需要审批的嵌套调用会直接跑）。
func TestExecuteOneDepthRewriteKeepsApprover(t *testing.T) {
	e := NewToolEngine()
	p := &depthProbeTool{BaseTool: BaseTool{Name_: "danger-dep", Description_: "d", Params_: map[string]any{}}}
	e.RegisterTool(context.Background(), &approvalOver{depthProbeTool: *p})

	ctx := events.WithToolContext(context.Background(), "run1", 0, nil, denyApprover{}, nil)
	r := e.ExecuteOne(ctx, core.ToolCall{Id: "n1", Name: "danger-dep", Arguments: "{}"}, ExecuteOpts{Depth: 1})
	if !r.IsError || !strings.Contains(r.Result, "rejected") {
		t.Fatalf("ctx 改写后审批器丢失（调用没被拦）: IsError=%v result=%q", r.IsError, r.Result)
	}
}

// approvalOver 需要审批的 depth 探针。
type approvalOver struct {
	depthProbeTool
}

func (a *approvalOver) RequiresApproval(context.Context, core.ToolCall) bool { return true }

// ---- 3. 子调用必须吃自己的单次时限（设计文档 §16.3） ----

// timeoutProbeTool 声明自身时限，并记录自己是「被掐断」还是「跑满」。
type timeoutProbeTool struct {
	BaseTool
	timeout   time.Duration
	sawCancel bool
}

func (t *timeoutProbeTool) ToolTimeout() time.Duration { return t.timeout }
func (t *timeoutProbeTool) Call(ctx context.Context, _, _ string) (string, error) {
	select {
	case <-ctx.Done():
		t.sawCancel = true
		return "cancelled: " + ctx.Err().Error(), nil
	case <-time.After(3 * time.Second):
		return "ran-to-completion", nil
	}
}
func (t *timeoutProbeTool) ValidParams(context.Context, string, string) error { return nil }

// TestExecuteOneAppliesPerCallTimeout 批内的单次时限在嵌套路径上同样生效。
// 曾经：ExecuteOne 直接进 execute()（无 liftableTimeout），声明 200ms 的工具跑满 3s。
func TestExecuteOneAppliesPerCallTimeout(t *testing.T) {
	e := NewToolEngine()
	p := &timeoutProbeTool{
		BaseTool: BaseTool{Name_: "slow", Description_: "slow", Params_: map[string]any{}},
		timeout:  200 * time.Millisecond,
	}
	e.RegisterTool(context.Background(), p)

	start := time.Now()
	r := e.ExecuteOne(context.Background(), core.ToolCall{Id: "t1", Name: "slow", Arguments: "{}"}, ExecuteOpts{Depth: 1})
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Fatalf("ExecuteOne 未按 ToolTimeout 掐断：跑了 %v（工具声明 %v）", elapsed, p.timeout)
	}
	if !p.sawCancel {
		t.Fatal("工具没等到 ctx 取消 —— 时限没落到工具的 ctx 上")
	}
	if !strings.Contains(r.Result, "cancelled") {
		t.Fatalf("Result = %q, want 取消原因", r.Result)
	}
}

// TestExecuteOneApprovalOutlivesToolTimeout 审批等待**不吃**工具时限：
// 人还在看确认弹窗时，工具时限不该把这次调用判死（与批内一致：批前审批用批 ctx）。
func TestExecuteOneApprovalOutlivesToolTimeout(t *testing.T) {
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &timedApprovalTool{
		stubTool: stubTool{BaseTool: BaseTool{Name_: "danger-slow", Description_: "d", Params_: map[string]any{}}, result: "RAN-OK"},
		timeout:  50 * time.Millisecond,
	})
	ctx := events.WithToolContext(context.Background(), "run1", 0, nil, delayedApprover{delay: 400 * time.Millisecond}, nil)

	r := e.ExecuteOne(ctx, core.ToolCall{Id: "n1", Name: "danger-slow", Arguments: "{}"}, ExecuteOpts{Depth: 1})
	if r.IsError || r.Result != "RAN-OK" {
		t.Fatalf("审批等待期间被工具时限判死: IsError=%v result=%q", r.IsError, r.Result)
	}
}

// timedApprovalTool 声明需审批 + 带自身时限。
type timedApprovalTool struct {
	stubTool
	timeout time.Duration
}

func (a *timedApprovalTool) RequiresApproval(context.Context, core.ToolCall) bool { return true }
func (a *timedApprovalTool) ToolTimeout() time.Duration                           { return a.timeout }

// delayedApprover 等一会儿再批准（模拟人类看弹窗）。
type delayedApprover struct{ delay time.Duration }

func (d delayedApprover) BeginApproval(core.ToolCall) func(context.Context) (bool, error) {
	return func(ctx context.Context) (bool, error) {
		select {
		case <-time.After(d.delay):
			return true, nil
		case <-ctx.Done():
			return false, ctx.Err()
		}
	}
}

// ---- 4. 嵌套调用的审批请求必须到达运行的事件 handler ----

// TestNestedApprovalEventReachesHandler 审批请求不发事件 = UI 永远看不到有人在等确认
// （调用白等满审批窗口并占住审批队列）。同时钉住：工具事件仍不透出（Phase 0 降级）。
func TestNestedApprovalEventReachesHandler(t *testing.T) {
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &approvalTool{
		stubTool: stubTool{BaseTool: BaseTool{Name_: "danger-ev", Description_: "d", Params_: map[string]any{}}, result: "RAN"},
	})

	var got []events.Event
	h := func(_ context.Context, ev events.Event) { got = append(got, ev) }
	ctx := events.WithToolContext(context.Background(), "run1", 0, h, allowApprover{}, nil)

	r := e.ExecuteOne(ctx, core.ToolCall{Id: "n1", Name: "danger-ev", Arguments: "{}"},
		ExecuteOpts{ParentCallId: "outer-1", Depth: 1})
	if r.IsError {
		t.Fatalf("审批通过却失败: %q", r.Result)
	}

	var req *events.ToolApprovalRequested
	for _, ev := range got {
		switch v := ev.(type) {
		case *events.ToolApprovalRequested:
			req = v
		case *events.ToolStart, *events.ToolResponse:
			t.Fatalf("嵌套调用的工具事件不该透出（Phase 0 降级，规范 §3.3）: %T", ev)
		}
	}
	if req == nil {
		t.Fatal("审批请求没到达运行 handler —— UI 看不到「有人在等确认」")
	}
	if req.Id != "n1" || req.RunId != "run1" {
		t.Fatalf("审批事件锚点 = id %q / run %q, want n1 / run1", req.Id, req.RunId)
	}
	if req.ParentCallId != "outer-1" || req.Depth != 1 {
		t.Fatalf("审批事件来源标注 = %q/%d, want outer-1/1（UI 无法归组到外层脚本）",
			req.ParentCallId, req.Depth)
	}
}

// ---- 5. 撞名检测不得把「原本能注册的工具」变成崩溃 ----

// uncomparableTool 值类型 + 切片字段 ⇒ 接口值比较会 panic。
type uncomparableTool struct {
	*BaseTool
	tags []string
}

func (t uncomparableTool) Call(context.Context, string, string) (string, error) { return "u", nil }
func (t uncomparableTool) ValidParams(context.Context, string, string) error    { return nil }

func TestRegisterToolConflictOnUncomparableToolDoesNotPanic(t *testing.T) {
	mk := func(tag string) Tool {
		return uncomparableTool{
			BaseTool: &BaseTool{Name_: "u", Description_: "u", Params_: map[string]any{}},
			tags:     []string{tag},
		}
	}

	// 不同实例：必须只发警告，不 panic。
	e := NewToolEngine()
	var conflicts []RegisterConflict
	e.SetRegisterConflictHook(func(c RegisterConflict) { conflicts = append(conflicts, c) })
	e.RegisterTool(context.Background(), mk("a"))
	e.RegisterTool(context.Background(), mk("b")) // 旧实现：prev != tool → panic
	if len(conflicts) != 1 {
		t.Fatalf("不同实例应上报 1 次撞名，got %d", len(conflicts))
	}

	// 同实例（指针）静默通过 —— hidden 档依赖这条语义。
	e2 := NewToolEngine()
	var conflicts2 []RegisterConflict
	e2.SetRegisterConflictHook(func(c RegisterConflict) { conflicts2 = append(conflicts2, c) })
	pt := &uncomparableTool{BaseTool: &BaseTool{Name_: "p", Description_: "p", Params_: map[string]any{}}, tags: []string{"p"}}
	e2.RegisterTool(context.Background(), pt)
	e2.RegisterTool(context.Background(), pt)
	if len(conflicts2) != 0 {
		t.Fatalf("同实例重复注册应静默，got %d 次警告", len(conflicts2))
	}

	// 已知代价：不可比较的**值类型**拿不到实例标识，同一份值重复注册也会报一次警告
	//（宁多报，不 panic）。这条断言是刻意钉住的，别当成 bug 修回去。
	e3 := NewToolEngine()
	var conflicts3 []RegisterConflict
	e3.SetRegisterConflictHook(func(c RegisterConflict) { conflicts3 = append(conflicts3, c) })
	v := mk("c")
	e3.RegisterTool(context.Background(), v)
	e3.RegisterTool(context.Background(), v)
	if len(conflicts3) != 1 {
		t.Fatalf("不可比较值类型：预期上报 1 次（已知代价），got %d", len(conflicts3))
	}
}

// ---- 6. 脚本内调用标记：只在 Depth>0 注入 ----

// TestExecuteOneMarksScriptCalls ExecuteOne(opts.Depth>0) 必须标记 ctx 为「脚本内调用」——
// 工具据此决定是否为本次调用产出只在脚本路径有意义的产物（bash 的结构化结果与 1 MiB
// 落盘、设计文档 §15.4「完整输出只服务编排路径」）。Depth==0 是编排工具自己被模型直呼，
// 不进脚本，必须**不**标记（否则模型路径也会多产一份没人消费的产物 + 落盘）。
func TestExecuteOneMarksScriptCalls(t *testing.T) {
	e := NewToolEngine()
	marks := make(chan bool, 4)
	e.RegisterTool(context.Background(), &scriptMarkProbe{
		BaseTool: BaseTool{Name_: "mark-probe", Description_: "d", Params_: map[string]any{}},
		marks:    marks,
	})

	e.ExecuteOne(context.Background(), core.ToolCall{Id: "n1", Name: "mark-probe"}, ExecuteOpts{Depth: 1})
	if got := awaitMark(t, marks); !got {
		t.Fatal("Depth=1 的嵌套调用必须带脚本标记（工具侧据此产出结构化结果/落盘）")
	}
	e.ExecuteOne(context.Background(), core.ToolCall{Id: "n0", Name: "mark-probe"}, ExecuteOpts{})
	if got := awaitMark(t, marks); got {
		t.Fatal("Depth=0 是模型直呼编排工具本身，不该带脚本标记")
	}
}

// awaitMark 有界等待探针回执。无界 `<-ch` 是测试事故的温床：探针没被调用（工具名注册错、
// 早退路径命中）时整套测试会挂到 Go 的 -timeout 才报错，现场只剩一个 [chan receive]
// 栈 —— 失败信息远不如一句「探针没被调用」。参见本文件曾经的实际事故。
func awaitMark(t *testing.T, marks <-chan bool) bool {
	t.Helper()
	select {
	case v := <-marks:
		return v
	case <-time.After(5 * time.Second):
		t.Fatal("探针未被调用（工具注册名对不上？引擎走了早退路径？）")
		return false
	}
}

type scriptMarkProbe struct {
	BaseTool
	marks chan bool
}

func (p *scriptMarkProbe) Call(ctx context.Context, _, _ string) (string, error) {
	p.marks <- IsScriptCall(ctx)
	return "ok", nil
}
func (p *scriptMarkProbe) ValidParams(context.Context, string, string) error { return nil }
