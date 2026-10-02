package tools

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
)

// 编排（codemode）能力的宿主侧支撑：单条重入执行 + 作用域（调用栈/串行门/墙钟）。
//
// 设计要点（与设计文档 §16.1 对应，修正了初稿的三处必错）：
//   - 串行门复用 runBatch 的既有模式（isSerial/serialPrev + done channel，
//     tools/engine.go:466-477 与 :506-515），**不另造闸门**；
//   - 令牌为 nil 时表示「无持有者」，nil channel 接收永久阻塞 —— 故首调必须
//     跳过等待，否则首调即死锁（初稿 `gate()` 的 `<-prev` 就是这个 bug）；
//   - release 用 sync.Once 保证幂等，防止重复 close 触发 panic。

// ExecuteOpts 单次嵌套执行的可选参数。顶层调用全零值。
type ExecuteOpts struct {
	// ParentCallId 发起本次调用的外层工具调用 id（空 = 顶层）。
	ParentCallId string
	// Depth 嵌套深度（顶层 = 0）。与 core.NestedMaxDepth 配套。
	Depth int
}

// ExecuteOneEngine 扩展接口：暴露单条重入执行。
//
// 刻意**不直接加进 ToolEngine** —— 该接口被 agents 与 desktop/bridge 广泛持有，
// 所有测试替身都实现了它，加方法会波及每一个实现者。改为在 tools 内定义
// 扩展接口，由编排型工具做类型断言，失败则降级为「仅文本模式」
// （工具仍可用，只是不能编排子调用）。这与本仓「可选接口按需启用」范式一致。
type ExecuteOneEngine interface {
	ToolEngine
	ExecuteOne(ctx context.Context, call core.ToolCall, opts ExecuteOpts) core.ToolResult
}

// 编译期断言：Engine 必须满足扩展接口。
var _ ExecuteOneEngine = (*Engine)(nil)

// ExecScope 一次编排调用的作用域：调用栈 + 记录器 + 串行门 + 墙钟预算。
//
// 并发安全：脚本内可用 Promise.all 并发发起嵌套调用，故所有字段受mu 保护。
type ExecScope struct {
	mu    sync.Mutex
	stack []string // 调用者 id 栈（首项 = 外层调用）

	recorder *core.NestedRecorder

	tail chan struct{} // 串行门尾令牌；nil = 当前无持有者

	wallStart time.Time
	wallLimit time.Duration // 0 = 不限

	execCount int
}

// NewExecScope 创建作用域。recorder 可为 nil（不记账）。
// wallLimit 0 = 不限墙钟（对齐 pi：execute.ts:277 用 POSITIVE_INFINITY）。
func NewExecScope(recorder *core.NestedRecorder, wallLimit time.Duration) *ExecScope {
	return &ExecScope{
		recorder:  recorder,
		wallStart: time.Now(),
		wallLimit: wallLimit,
	}
}

// Recorder 返回本作用域的记录器（可 nil）。
func (s *ExecScope) Recorder() *core.NestedRecorder {
	if s == nil {
		return nil
	}
	return s.recorder
}

// Acquire 取串行门，返回的 release **必须由调用方 defer**（漏 defer = 闸门永不
// 释放，后续串行调用全部阻塞）。
//
// # exclusive 与 inherited 为何是两个参数
//
// exclusive：本帧自己是否需要独占（如「本次要写文件」）。
// inherited：**祖先帧是否已持锁** —— 对齐 pi 的 holdsQueue 语义
// 「上层拿过串行锁 ⇒ 深层全部串行」，杜绝 Promise.all 并行写文件。
//
// inherited 必须由**调用方按帧传入**，不能做成 scope 上的状态。初稿把
// holdsQueue 存成 scope 级字段，结果并发兄弟帧（Promise.all 发起的多条嵌套
// 调用）看到 holdsQueue=true 就**全部绕过闸门** —— 恰好是本闸门要防的情况。
// 序列化是「跨并发帧」的维度，祖先关系是「跨嵌套层」的维度，两者不能共用一个字段。
// 该缺陷由 TestAcquireSerializesAcrossGoroutines 捕获（曾失败）。
func (s *ExecScope) Acquire(exclusive, inherited bool) (release func()) {
	if s == nil {
		return func() {}
	}
	s.mu.Lock()
	if !exclusive || inherited {
		s.mu.Unlock()
		return func() {}
	}
	prev := s.tail // nil 表示无前驱
	cur := make(chan struct{})
	s.tail = cur
	s.mu.Unlock()

	if prev != nil { // ← 有前驱才等；nil 跳过 ⇒ 首调不死锁
		<-prev
	}
	var once sync.Once
	return func() { once.Do(func() { close(cur) }) }
}

// Enter 进入一次嵌套调用：检查墙钟预算与次数，登记调用栈。
//
// 超预算时返回 ok=false 与**给模型看的可读原因**（不返回 error ——
// ExecuteOne 的契约是永不返回 error，一切失败都是 ToolResult{IsError:true}）。
func (s *ExecScope) Enter(id string) (ok bool, reason string) {
	if s == nil {
		return true, ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.wallLimit > 0 {
		used := time.Since(s.wallStart)
		if used > s.wallLimit {
			return false, fmt.Sprintf(
				"script wall-clock budget exhausted (used %s, limit %s); "+
					"no further tool calls will run — return what you have",
				used.Round(time.Second), s.wallLimit.Round(time.Second))
		}
	}
	s.execCount++
	s.stack = append(s.stack, id)
	return true, ""
}

// Exit 退出一次嵌套调用（与 Enter 配对）。
func (s *ExecScope) Exit() {
	if s == nil {
		return
	}
	s.mu.Lock()
	if n := len(s.stack); n > 0 {
		s.stack = s.stack[:n-1]
	}
	s.mu.Unlock()
}

// ExecCount 返回本作用域已发起的嵌套调用数（含被预算拒绝前已计数的）。
func (s *ExecScope) ExecCount() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.execCount
}

// StackDepth 返回当前嵌套栈深度（0 = 顶层）。
func (s *ExecScope) StackDepth() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.stack)
}

// truncNested 按上限截断 s（超限时尾部标注省略量）。
func truncNested(s string, max int) string {
	if len(s) <= max {
		return s
	}
	dropped := len(s) - max + 16
	marker := fmt.Sprintf("...[+%d bytes]", dropped)
	if max <= len(marker) {
		return s[:max]
	}
	return s[:max-len(marker)] + marker
}

// usageOrNil 返回用量指针；零值用量返回 nil（避免在 JSON 里输出全零对象）。
func usageOrNil(u core.Usage) *core.Usage {
	if u.IsZero() {
		return nil
	}
	uu := u
	return &uu
}

// ExecuteOne 执行单条工具调用，走与批内**完全相同**的管线
// （审批 → ValidParams → 超时 → BeforeCall → Call → 截断 → AfterCall → 事件）。
//
// 供编排型工具（codemode）从工具内部回调引擎。
//
// # 契约：永不返回 error
//
// 审批拒绝、参数校验失败、panic、超时、取消—— 全部包成
// core.ToolResult{IsError: true}。这与 engine.execute() 的既有契约一致
// （tools/engine.go:finish() 一切都包成结果），也避免同一错误被格式化两次：
// 若这里返回 error，桥接层包一次、引擎又包一次，模型看到的是 TOOL_ERROR
// 信封被套在引擎文本里。
//
// # 与 execute() 的差异（仅两处）
//
//  1. 填入 opts.ParentCallId / opts.Depth 供事件标注与深度判定；
//  2. 嵌套调用自动汇入 ctx 上的 core.NestedRecorder（若有）：记录 + 用量。
//
// # 已知降级（Phase 0，负向测试钉住）
//
//   - **PreToolUse 不覆盖嵌套调用**：PreToolUse 在 agents 层
//     （agents/agent_loop.go:2251），引擎侧 execute() 没有它，故本路径天然跳过。
//     这是产品级降级。将来把 hooks 下沉进引擎时，tools/nested_test.go 里的
//     TestPreToolUseDoesNotCoverNestedCalls 需要翻转。
//   - **不发射事件**：handler 传 nil。Phase 2 接入带 ParentCallId 的事件流后移除。
func (e *Engine) ExecuteOne(ctx context.Context, call core.ToolCall, opts ExecuteOpts) core.ToolResult {
	call.ParentCallId = opts.ParentCallId
	call.Depth = opts.Depth

	// 深度守卫：core.NestedMaxDepth 管「单次运行内的编排嵌套」，
	// 与 subagent 的 maxSpawnDepth（subagent/agent_tools.go:27 = 2，管「运行」间
	// 的派生）是独立的另一道闸。两道都要接线，否则脚本可经 agent_spawn 绕过。
	if opts.Depth > core.NestedMaxDepth {
		return core.ToolResult{
			Id:      call.Id,
			IsError: true,
			Result: fmt.Sprintf(
				"nested tool call refused: nesting depth %d exceeds the limit of %d "+
					"(a codemode script must not spawn another codemode script)",
				opts.Depth, core.NestedMaxDepth),
		}
	}

	rec := events.NestedRecorderFrom(ctx)
	if rec == nil {
		return e.executeApproved(ctx, call, nil)
	}

	start := time.Now()
	r := e.executeApproved(ctx, call, nil)

	// 只记录**真正嵌套**的调用（Depth > 0）。Depth == 0 时本次调用就是外层编排调用
	// 本身，它不该把自己记成自己的嵌套记录：finish() 在 execute() 内部就已
	// TakeRecord（见 engine.go finish 的 call.Depth == 0 分支），自记录会落在
	// 取出之后，凭空多出一条幽灵记录。
	if opts.Depth > 0 {
		rec.Add(core.NestedCallRecord{
			Id:       call.Id,
			Name:     call.Name,
			Args:     truncNested(call.Arguments, core.NestedMaxArgsPerCall),
			Result:   truncNested(r.Result, core.NestedMaxArgsPerCall),
			IsError:  r.IsError,
			Duration: time.Since(start),
			Usage:    usageOrNil(r.Usage),
		}, len(call.Arguments))
		// 用量独立累加：即使 Add 因限额丢弃了记录，用量仍必须计入。
		// Depth == 0 的外层调用其用量由 AgentLoop 的父累加路径结算，不在此重复计入。
		rec.AddUsage(r.Usage)
	}
	return r
}

// executeApproved 先走审批再执行 —— 嵌套调用的**权限门必须逐次生效**。
//
// execute() 只**消费** decisions 表，不发起审批；审批发生在 preApprove，
// 而 preApprove 只被 Sequence / Parallel / runBatch 调用（engine.go:451/462/481）。
// 因此嵌套调用**不能**直接调 execute(decisions=nil)，否则声明了 ApprovalRequired
// 的工具（bash / write_file 等）会在完全没有审批的情况下执行 —— 一条真实的越权路径。
// 由TestExecuteOneRespectsApproval捕获（曾观察到 result="SHOULD-NOT-RUN"）。
//
// 审批事件仍传 nil handler：嵌套调用的审批请求与事件归组留给 Phase 2
// （届时接带 ParentCallId 的事件流），但**审批决策本身必须在 Phase 0 就生效**——
// 安全门不能延后。
func (e *Engine) executeApproved(ctx context.Context, call core.ToolCall, handler events.EventHandler) core.ToolResult {
	decisions, err := e.preApprove(ctx, []core.ToolCall{call}, handler)
	if err != nil {
		// preApprove 出错（如 ctx 取消）→ 拒绝执行，与 execute() 的失败语义一致。
		return core.ToolResult{
			Id:      call.Id,
			IsError: true,
			Result:  "approval could not be requested: " + err.Error(),
		}
	}
	return e.execute(ctx, call, handler, decisions, "")
}
