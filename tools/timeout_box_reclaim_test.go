package tools

// 时限盒（liftableTimeout）的回收（第 4 轮爬山）。
//
// 时限盒 = 1 个 time.AfterFunc（timer）+ 1 个 watcher goroutine。任务**正常结束**时
// 会 stop（回收两者）；但有两条路径原来不回收：
//  ① 「取消早退」：任务在等串行前驱 / 等信号量槽位时被取消 ⇒ 直接 return，
//     没停表、没退 watcher ⇒ 每次漏一个还武装着的 timer（到点才自己散掉）；
//  ② 「判卡死放弃」：工具不响应取消，引擎判它卡死并放弃该任务 ⇒ 它的时限盒与
//     rctx watcher 一直留着。
//
// 现在：① 收尾统一走 defer（任何提前 return 都跑得到）；② 引擎判卡死时顺手回收。
// 判据用 activeTimeoutBoxes 计数（构造 +1 / stop -1）——「有没有回收干净」变成可断言的。

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/seven7628/hai-harness/core"
)

// stuckTool 一个**不响应 ctx 取消**的工具（模拟第三方/插件工具）。
type stuckTool struct {
	release chan struct{} // 测试收尾时放它走（免得 goroutine 留在测试进程里）
	once    sync.Once
	started chan struct{}
}

func newStuckTool() *stuckTool {
	return &stuckTool{release: make(chan struct{}), started: make(chan struct{})}
}

func (s *stuckTool) Name() string                                      { return "stuck" }
func (s *stuckTool) Description() string                               { return "stuck tool" }
func (s *stuckTool) Parameters() any                                   { return map[string]any{"type": "object"} }
func (s *stuckTool) CanParallel() bool                                 { return true }
func (s *stuckTool) ToolTimeout() time.Duration                        { return time.Minute }
func (s *stuckTool) ValidParams(context.Context, string, string) error { return nil }
func (s *stuckTool) BeforeCall(context.Context, core.ToolCall) BeforeToolCallResponse {
	return BeforeToolCallResponse{}
}
func (s *stuckTool) AfterCall(context.Context, core.ToolCall) {}

// Call 故意**不看** ctx.Done()（这就是「卡死」的定义）。
func (s *stuckTool) Call(ctx context.Context, name, arguments string) (string, error) {
	s.once.Do(func() { close(s.started) })
	<-s.release
	return "late result", nil
}

// TestStuckToolReclaimsTimeoutBox：判卡死后，时限盒必须被回收（计数回基线）。
//
// 走两条路径：① 第二个任务卡在信号量上（maxParallel=1）⇒ 取消早退；
// ② 第一个任务卡死 ⇒ 引擎判它卡死并放弃。
func TestStuckToolReclaimsTimeoutBox(t *testing.T) {
	old := toolStuckTimeout
	toolStuckTimeout = 200 * time.Millisecond
	defer func() { toolStuckTimeout = old }()

	tool := newStuckTool()
	defer close(tool.release)

	e := NewToolEngine(WithMaxParallel(1))
	e.RegisterTool(context.Background(), tool)

	base := activeTimeoutBoxes.Load()

	ctx, cancel := context.WithCancel(context.Background())
	calls := []core.ToolCall{
		{Id: "c1", Name: "stuck", Arguments: "{}"},
		{Id: "c2", Name: "stuck", Arguments: "{}"},
	}
	// 第一个任务占住唯一的并行槽；第二个在等槽位 ⇒ 取消时走「取消早退」。
	go func() {
		<-tool.started
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	_, _ = e.Parallel(ctx, calls, nil)

	// 回收是异步的（goroutine 的 defer / 引擎的 abandon）：给一点时间。
	deadline := time.Now().Add(3 * time.Second)
	got := activeTimeoutBoxes.Load()
	for time.Now().Before(deadline) {
		got = activeTimeoutBoxes.Load()
		if got <= base {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got > base {
		t.Fatalf("时限盒没回收干净：在飞 %d 个（基线 %d）—— 取消早退/判卡死两条路径漏了 stop()",
			got, base)
	}
}

// TestCancelledTaskReclaimsTimeoutBox：任务在**等串行前驱**时被取消 ⇒ 早退路径
// 也要回收时限盒（这条路径原来直接 return，什么都不收）。
func TestCancelledTaskReclaimsTimeoutBox(t *testing.T) {
	old := toolStuckTimeout
	toolStuckTimeout = 200 * time.Millisecond
	defer func() { toolStuckTimeout = old }()

	tool := newStuckTool()
	defer close(tool.release)

	e := NewToolEngine(WithMaxParallel(1))
	e.RegisterTool(context.Background(), tool)

	base := activeTimeoutBoxes.Load()

	ctx, cancel := context.WithCancel(context.Background())
	calls := []core.ToolCall{
		{Id: "c1", Name: "stuck", Arguments: "{}"},
		{Id: "c2", Name: "stuck", Arguments: "{}"},
	}
	go func() {
		<-tool.started
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	_, _ = e.Parallel(ctx, calls, nil)

	deadline := time.Now().Add(3 * time.Second)
	got := activeTimeoutBoxes.Load()
	for time.Now().Before(deadline) {
		got = activeTimeoutBoxes.Load()
		if got <= base {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if got > base {
		t.Fatalf("时限盒没回收干净：在飞 %d 个（基线 %d）", got, base)
	}
}

// TestMixedBatchSerialWaitsPreviousSerial：混批语义钉死（第 4 轮爬山第 7 步）。
//
// 契约（见 engine.go runBatch 的注释）：
//   - 串行工具等**前一个串行工具**（不等并行工具 —— 因为并行工具可能被 promote 摘离成
//     后台任务，严格互斥会被它无限拖住）；
//   - 并行工具立即启动。
//
// 这条测试的作用是「把语义钉住」：将来谁想改成严格互斥，会先看到它与 promote 的冲突。
func TestMixedBatchSerialWaitsPreviousSerial(t *testing.T) {
	e := NewToolEngine(WithMaxParallel(4))
	serial := &mockTool{name: "serial", canParallel: false, delay: 80 * time.Millisecond}
	par := &mockTool{name: "par", canParallel: true, delay: 80 * time.Millisecond}
	e.RegisterTool(context.Background(), serial)
	e.RegisterTool(context.Background(), par)

	before := serial.calls.Load()
	calls := []core.ToolCall{
		{Id: "p1", Name: "par", Arguments: "{}"},
		{Id: "s1", Name: "serial", Arguments: "{}"},
		{Id: "s2", Name: "serial", Arguments: "{}"},
	}
	if _, err := e.Parallel(context.Background(), calls, nil); err != nil {
		t.Fatalf("Parallel 失败: %v", err)
	}
	if got := serial.calls.Load() - before; got != 2 {
		t.Fatalf("串行工具应被调用 2 次，got %d", got)
	}
	// 两个串行调用**不得重叠**（串行语义的核心）：maxInflight 必须是 1。
	if got := serial.maxInflight.Load(); got != 1 {
		t.Fatalf("两个串行调用重叠了（maxInflight=%d）—— 串行语义被破坏", got)
	}
}
