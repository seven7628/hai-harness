package tools

import (
	"context"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// Phase 0 验收测试。核心目标：把「初稿设计缺陷」与「实现遗漏」钉死成回归。
//
// 特别地，TestAcquireFirstCallDoesNotDeadlock 与 TestAcquireReleaseIdempotent
// 是为初稿gate() 的两处致命 bug 设立的：初稿用 `prev := s.tail; <-prev`，
// 首调时 prev 为 nil channel，接收永久阻塞 —— 第一次进入串行分支即挂死。
// 这两个测试若失败说明实现退化回了那个 bug。

// stubTool 最小工具实现（嵌入 BaseTool 补齐 ValidParams 等方法）。
type stubTool struct {
	BaseTool
	result string
}

func (t *stubTool) Call(context.Context, string, string) (string, error) { return t.result, nil }
func (t *stubTool) ValidParams(context.Context, string, string) error    { return nil }

// newNestedEngine 建一个只含 count 工具的引擎。
func newNestedEngine(t *testing.T) *Engine {
	t.Helper()
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &stubTool{
		BaseTool: BaseTool{Name_: "count", Description_: "count", Params_: map[string]any{}},
		result:   "ok",
	})
	return e
}

// TestExecuteOneReturnsToolResultShape 校验单返回契约：永不返回 error，
// 成功路径返回非 IsError 结果。
func TestExecuteOneReturnsToolResultShape(t *testing.T) {
	e := newNestedEngine(t)
	r := e.ExecuteOne(context.Background(), core.ToolCall{Id: "c1", Name: "count"}, ExecuteOpts{})
	if r.Id != "c1" {
		t.Fatalf("Id = %q, want c1", r.Id)
	}
	if r.IsError {
		t.Fatalf("IsError = true, want false (result=%q)", r.Result)
	}
	if r.Result != "ok" {
		t.Fatalf("Result = %q, want ok", r.Result)
	}
}

// TestExecuteOneUnknownToolNeverErrors 未知工具必须包成 ToolResult{IsError:true}，
// **不能** 返回 error —— 这是与engine.execute() 的既有契约一致的要点。
func TestExecuteOneUnknownToolNeverErrors(t *testing.T) {
	e := newNestedEngine(t)
	r := e.ExecuteOne(context.Background(), core.ToolCall{Id: "c1", Name: "nope"}, ExecuteOpts{})
	if !r.IsError {
		t.Fatalf("IsError = false, want true (unknown tool should be an error result)")
	}
	if !strings.Contains(r.Result, "not registered") {
		t.Fatalf("Result = %q, want it to mention 'not registered'", r.Result)
	}
}

// TestExecuteOneDepthGuard 深度守卫：超过 NestedMaxDepth 必须被拒，且原因可读。
// 对应 pi 给 codemode 标 exposure:"model-only"（tool.ts:424）硬封死递归。
func TestExecuteOneDepthGuard(t *testing.T) {
	e := newNestedEngine(t)
	r := e.ExecuteOne(context.Background(), core.ToolCall{Id: "c1", Name: "count"},
		ExecuteOpts{Depth: core.NestedMaxDepth + 1})
	if !r.IsError {
		t.Fatalf("IsError = false, want true (depth %d should be refused)", core.NestedMaxDepth+1)
	}
	if !strings.Contains(r.Result, "nesting depth") {
		t.Fatalf("Result = %q, want a readable depth-refusal reason", r.Result)
	}
	// 恰好等于上限应放行。
	r2 := e.ExecuteOne(context.Background(), core.ToolCall{Id: "c2", Name: "count"},
		ExecuteOpts{Depth: core.NestedMaxDepth})
	if r2.IsError {
		t.Fatalf("depth == limit should be allowed, got error: %q", r2.Result)
	}
}

// TestExecuteOneRecordsNestedCalls 嵌套记录：3 次调用 → 外层拿到 3 条。
func TestExecuteOneRecordsNestedCalls(t *testing.T) {
	e := newNestedEngine(t)
	rec := core.NewNestedRecorder()
	ctx := events.WithNestedRecorder(context.Background(), rec)

	for _, id := range []string{"n1", "n2", "n3"} {
		e.ExecuteOne(ctx, core.ToolCall{Id: id, Name: "count", Arguments: `{"x":1}`}, ExecuteOpts{Depth: 1})
	}
	got := rec.TakeRecord()
	if len(got) != 3 {
		t.Fatalf("records = %d, want 3", len(got))
	}
	for _, r := range got {
		if r.Name != "count" || r.Id == "" {
			t.Fatalf("record %+v missing name/id", r)
		}
		if r.Duration <= 0 {
			t.Fatalf("record %+v Duration = %v, want > 0", r, r.Duration)
		}
	}
	// TakeRecord 语义：取出后清空。
	if again := rec.TakeRecord(); len(again) != 0 {
		t.Fatalf("second TakeRecord = %d records, want 0 (must clear)", len(again))
	}
}

// TestNestedRecordLimitsDropButStillCountUsage 记录超限被丢弃，**但用量仍累加**。
// 计费不因记录截断而漏 —— 这是Add / AddUsage 分离的原因。
func TestNestedRecordLimitsDropButStillCountUsage(t *testing.T) {
	rec := core.NewNestedRecorder()
	recorded := 0
	for i := 0; i < core.NestedMaxCalls+50; i++ {
		if rec.Add(core.NestedCallRecord{Id: "x"}, 8) {
			recorded++
		}
		rec.AddUsage(core.Usage{Input: 10, Output: 5, TotalTokens: 15})
	}
	if recorded != core.NestedMaxCalls {
		t.Fatalf("recorded = %d, want exactly NestedMaxCalls (%d)", recorded, core.NestedMaxCalls)
	}
	if dropped := rec.Stats(); dropped != 50 {
		t.Fatalf("dropped = %d, want 50", dropped)
	}
	// 关键断言：全部 306 次的用量都要计入。
	wantTotal := int64(core.NestedMaxCalls+50) * 10
	if got := rec.Usage().Input; got != wantTotal {
		t.Fatalf("accumulated Input = %d, want %d (usage must not be lost when records are dropped)", got, wantTotal)
	}
}

// TestAcquireFirstCallDoesNotDeadlock 首调串行门必须立即返回。
// 这是初稿 gate() 的致命 bug 回归测试：`prev := s.tail` 在首调时是 nil，
// 对nil channel 接收永久阻塞。
func TestAcquireFirstCallDoesNotDeadlock(t *testing.T) {
	s := NewExecScope(nil, 0)
	done := make(chan struct{})
	go func() {
		rel := s.Acquire(true, false)
		rel()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire(true) deadlocked on first call — nil channel must be skipped")
	}
}

// TestAcquireSerializesAcrossGoroutines 第二个持有者必须等第一个释放。
func TestAcquireSerializesAcrossGoroutines(t *testing.T) {
	s := NewExecScope(nil, 0)
	rel1 := s.Acquire(true, false)

	acquired := make(chan struct{})
	go func() {
		rel2 := s.Acquire(true, false)
		close(acquired)
		rel2()
	}()

	select {
	case <-acquired:
		t.Fatal("second Acquire returned before release —串行门失效")
	case <-time.After(200 * time.Millisecond):
		// 正确：仍在等待。
	}
	rel1()
	select {
	case <-acquired:
	case <-time.After(2 * time.Second):
		t.Fatal("second Acquire never acquired after release")
	}
}

// TestAcquireReleaseIdempotent release 必须幂等：重复 close 会 panic。
func TestAcquireReleaseIdempotent(t *testing.T) {
	s := NewExecScope(nil, 0)
	rel := s.Acquire(true, false)
	rel()
	rel() // 二次调用不得 panic
	rel()
}

// TestAcquireNonExclusiveIsNoop 非独占路径返回空操作（不排队）。
func TestAcquireNonExclusiveIsNoop(t *testing.T) {
	s := NewExecScope(nil, 0)
	done := make(chan struct{})
	go func() { s.Acquire(false, false)(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("non-exclusive Acquire should return immediately")
	}
}

// TestAcquireHoldsQueuePropagates 上层已持锁时，内层 Acquire 不得再排队
// （实现 pi 的 holdsQueue 向下传播语义：杜绝 Promise.all 并行写文件）。
func TestAcquireInheritedSkipsGate(t *testing.T) {
	s := NewExecScope(nil, 0)
	outer := s.Acquire(true, false)
	defer outer()
	// 祖先帧已持锁（inherited=true）时，本帧即使 exclusive=true 也应立即返回。
	done := make(chan struct{})
	go func() { s.Acquire(true, true)(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("inner Acquire should inherit holdsQueue and not block")
	}
}

// TestEnterWallClockBudget 墙钟预算耗尽后拒绝新调用，并给出可读原因。
func TestEnterWallClockBudget(t *testing.T) {
	s := NewExecScope(nil, 30*time.Millisecond)
	time.Sleep(60 * time.Millisecond)
	ok, reason := s.Enter("c1")
	if ok {
		t.Fatal("Enter should refuse after wall-clock budget exhausted")
	}
	if !strings.Contains(reason, "wall-clock budget exhausted") {
		t.Fatalf("reason = %q, want a readable budget message", reason)
	}
	// 不限墙钟时永远放行。
	s2 := NewExecScope(nil, 0)
	if ok, _ := s2.Enter("c1"); !ok {
		t.Fatal("Enter should allow when no wall limit")
	}
}

// TestEnterExitStackDepth 栈深度进出配对。
func TestEnterExitStackDepth(t *testing.T) {
	s := NewExecScope(nil, 0)
	if d := s.StackDepth(); d != 0 {
		t.Fatalf("initial depth = %d, want 0", d)
	}
	s.Enter("a")
	s.Enter("b")
	if d := s.StackDepth(); d != 2 {
		t.Fatalf("depth = %d, want 2", d)
	}
	s.Exit()
	if d := s.StackDepth(); d != 1 {
		t.Fatalf("after one Exit depth = %d, want 1", d)
	}
}

// TestNestedRecorderSharedUsageClone 子记录器与父记录器共享用量累加，
// 但记录互相独立 —— 外层 ToolResult.Usage 必须包含所有内层用量。
func TestNestedRecorderSharedUsageClone(t *testing.T) {
	parent := core.NewNestedRecorder()
	child := parent.Clone()

	child.AddUsage(core.Usage{Input: 7, Output: 3, TotalTokens: 10})
	parent.AddUsage(core.Usage{Input: 1, TotalTokens: 1})

	if got := parent.Usage().Input; got != 8 {
		t.Fatalf("parent Input = %d, want 8 (child usage must flow to parent)", got)
	}
	// 记录独立：子记录了 1 条，父取不到。
	child.Add(core.NestedCallRecord{Id: "inner"}, 4)
	if got := parent.TakeRecord(); len(got) != 0 {
		t.Fatalf("parent records = %d, want 0 (records must be independent)", len(got))
	}
	if got := child.TakeRecord(); len(got) != 1 {
		t.Fatalf("child records = %d, want 1", len(got))
	}
}

// TestNestedRecorderConcurrentAdd 并发安全（脚本内 Promise.all 会并发调用）。
func TestNestedRecorderConcurrentAdd(t *testing.T) {
	rec := core.NewNestedRecorder()
	var wg sync.WaitGroup
	const n = 100
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rec.Add(core.NestedCallRecord{Id: "x"}, 4)
			rec.AddUsage(core.Usage{Input: 1, TotalTokens: 1})
		}()
	}
	wg.Wait()
	if got := rec.Usage().Input; got != n {
		t.Fatalf("Input = %d, want %d", got, n)
	}
	if got := len(rec.TakeRecord()); got > core.NestedMaxCalls {
		t.Fatalf("records = %d, must not exceed limit %d", got, core.NestedMaxCalls)
	}
}

// TestRegisterToolConflict 同名不同实现 → 触发冲突回调；同名同实例 → 静默。
// 关键：不得改成报错 —— exposure "hidden" 档依赖「重注册覆盖」实现撤下工具
// （pi 侧工具无法 unregister，只能重注册为 hidden）。
func TestRegisterToolConflict(t *testing.T) {
	e := NewToolEngine()
	var conflicts []RegisterConflict
	e.SetRegisterConflictHook(func(c RegisterConflict) { conflicts = append(conflicts, c) })

	t1 := &stubTool{BaseTool: BaseTool{Name_: "dup"}, result: "first"}
	e.RegisterTool(context.Background(), t1)

	// 同名同实例：静默通过，不上报。
	e.RegisterTool(context.Background(), t1)
	if len(conflicts) != 0 {
		t.Fatalf("same instance reported conflict: %+v (hidden 档依赖静默覆盖)", conflicts)
	}

	// 同名不同实现：上报，且后注册者胜出（保持历史行为）。
	t2 := &stubTool{BaseTool: BaseTool{Name_: "dup"}, result: "second"}
	e.RegisterTool(context.Background(), t2)
	if len(conflicts) != 1 {
		t.Fatalf("conflicts = %d, want 1", len(conflicts))
	}
	if conflicts[0].Name != "dup" {
		t.Fatalf("conflict name = %q, want dup", conflicts[0].Name)
	}
	got, err := e.GetTool(context.Background(), "dup")
	if err != nil {
		t.Fatalf("GetTool: %v", err)
	}
	if v, _ := got.Call(context.Background(), "", ""); v != "second" {
		t.Fatal("last registration should win (preserve existing semantics)")
	}
}

// TestExecuteOneDoesNotCoverPreToolUse —— 负向测试，钉住已知降级。
//
// PreToolUse 在 agents 层（agents/agent_loop.go:2251 的 runTools 内），引擎侧 execute()
// 没有它，故 ExecuteOne 路径天然跳过。这是**产品级降级**（规范 §3.3 第 14 条要求负向
// 钉住）：PreToolUse 是本仓唯一能做 Block 与参数改写的钩子，嵌套调用不过它。
//
// 断言方式是「注入间谍钩子 + 断言它一次都没被调用」：ToolHooks 挂在 ToolContext 上，
// 任何一层真去跑 hooks 都会命中这个间谍。将来把 hooks 下沉进引擎时，本测试必须翻转
// （改为断言钩子确实被调用、且改写/阻断生效）——
// 0f 之前这里只断言「没报错」，空转的负向测试比没有更坏（给人验过的错觉）。
func TestExecuteOneDoesNotCoverPreToolUse(t *testing.T) {
	e := newNestedEngine(t)
	var called atomic.Int32
	hooks := &events.ToolHooks{
		PreToolUse: func(context.Context, events.PreToolUseContext) events.PreToolUseResult {
			called.Add(1)
			return events.PreToolUseResult{Decision: events.ToolDecisionBlock, Reason: "should not run"}
		},
	}
	ctx := events.WithToolContext(context.Background(), "run1", 0, nil, nil, hooks)

	r := e.ExecuteOne(ctx, core.ToolCall{Id: "n1", Name: "count", Arguments: "{}"}, ExecuteOpts{Depth: 1})

	if called.Load() != 0 {
		t.Fatalf("PreToolUse 被调用了 %d 次 —— 契约变了：本负向测试必须翻转，"+
			"并同步删掉 ExecuteOne godoc 里的「已知降级」说明", called.Load())
	}
	if r.IsError {
		t.Fatalf("降级必须是静默且安全的（不因缺 hooks 报错）: %q", r.Result)
	}
}

// TestExecuteOneAssignsDepthToAuditAnchor ExecuteOne 必须把 ParentCallId/Depth 落到
// **可观测的两处**：审批事件（UI 归组）与工具读到的 ToolContext.Depth（深度判定）。
//
// 这条替换掉早先那个名不副实的用例：原 TestExecuteOneCarriesParentAndDepth 既没断言
// parent 也没断言 depth（Tool.Call 的签名里根本拿不到 ToolCall，记录本身也不含 parent），
// 真正有意义的出口只有上面两处 —— 分别由本用例与
// TestNestedApprovalEventReachesHandler / TestToolEventsCarryParentCallIdAndDepth 覆盖。
func TestExecuteOneAssignsDepthToAuditAnchor(t *testing.T) {
	e := NewToolEngine()
	e.RegisterTool(context.Background(), &approvalTool{
		stubTool: stubTool{BaseTool: BaseTool{Name_: "danger-anchor", Description_: "d", Params_: map[string]any{}}, result: "RAN"},
	})
	var req *events.ToolApprovalRequested
	h := func(_ context.Context, ev events.Event) {
		if v, ok := ev.(*events.ToolApprovalRequested); ok {
			req = v
		}
	}
	// 深度守卫的边界值必须逐一落到事件上（0 = 顶层，1 = 上限内最深）。
	for _, depth := range []int{0, core.NestedMaxDepth} {
		req = nil
		ctx := events.WithToolContext(context.Background(), "run1", 0, h, allowApprover{}, nil)
		e.ExecuteOne(ctx, core.ToolCall{Id: "n1", Name: "danger-anchor", Arguments: "{}"},
			ExecuteOpts{ParentCallId: "outer-9", Depth: depth})
		if req == nil {
			t.Fatalf("depth=%d：没收到审批事件", depth)
		}
		if req.ParentCallId != "outer-9" || req.Depth != depth {
			t.Fatalf("depth=%d：事件身份 = %q/%d, want outer-9/%d",
				depth, req.ParentCallId, req.Depth, depth)
		}
	}
}

// TestTruncNested 截断标注。
func TestTruncNested(t *testing.T) {
	if got := truncNested("short", 100); got != "short" {
		t.Fatalf("short input changed: %q", got)
	}
	long := strings.Repeat("x", 500)
	got := truncNested(long, 100)
	if len(got) > 100 {
		t.Fatalf("len = %d, want <= 100", len(got))
	}
	if !strings.Contains(got, "bytes]") {
		t.Fatalf("truncated = %q, want an elision marker", got)
	}
}
