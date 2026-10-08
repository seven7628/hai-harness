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
// （审批 → ValidParams → 超时 → BeforeCall → Call → 截断 → AfterCall）。
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
// # 编排方怎么用（唯一正确形态）
//
//	ctx 里已有引擎注入的 recorder（events.EnsureNestedRecorder，勿自建）
//	rec := events.NestedRecorderFrom(ctx)
//	e.ExecuteOne(ctx, call, tools.ExecuteOpts{ParentCallId: 本次调用 id, Depth: 1})
//
// 记录与嵌套用量由**引擎**在本次调用收尾时自动挂到外层 ToolResult
// （NestedCalls + Usage）。编排方**不要**自建 recorder —— 自建那份挂不到自己的
// ToolResult 上（Tool.Call 只返回 (string, error)，没有回写通道，见
// events.EnsureNestedRecorder）；也**不要**把嵌套用量经 ToolUsageProvider 再上报
// 一次 —— 引擎已从 recorder 合并过一次（engine.go finish），再报即双计。
//
// # 与 runBatch 单条的差异
//
//  1. 填 opts.ParentCallId / opts.Depth：既写进事件，也把 opts.Depth 叠加到
//     events.ToolContext.Depth 上交给工具（否则 subagent 的 maxSpawnDepth 读到的是
//     外层运行深度，脚本可借道 agent_spawn 隐掉一层嵌套 —— 设计文档 §20.1）；
//  2. 子调用自动汇入 ctx 上的 core.NestedRecorder（记录 + 用量）；
//  3. **不发射工具事件**（ToolStart/ToolResponse）：带 ParentCallId 的嵌套事件流是
//     Phase 2 的事。但**审批事件必须透出** —— 请求不到 UI，一次 manual 模式的嵌套
//     调用会白等满审批窗口并占住会话审批队列（设计文档 §16.2）。
//
// # 已知降级（Phase 0，负向测试钉住）
//
//   - **PreToolUse 不覆盖嵌套调用**：PreToolUse 在 agents 层
//     （agents/agent_loop.go:2251），引擎侧 execute() 没有它，故本路径天然跳过。
//     这是产品级降级。将来把 hooks 下沉进引擎时，tools/nested_test.go 里的
//     TestExecuteOneDoesNotCoverPreToolUse 需要翻转。
func (e *Engine) ExecuteOne(ctx context.Context, call core.ToolCall, opts ExecuteOpts) core.ToolResult {
	call.ParentCallId = opts.ParentCallId
	call.Depth = opts.Depth

	// 误用纠正：带了 ParentCallId（= 编排方明确说「这是某次调用的子调用」）却把 Depth 留 0，
	// 是**静默毁数据**的组合，必须纠正而不是照做：
	//   - finish() 只在 Depth==0 时取记录/用量 ⇒ 子调用的记录与嵌套用量会被内层 finish 取走，
	//     外层拿到空的 NestedCalls 与 Usage（会话**少计费**，§16.4 的唯一通道断掉）；
	//   - WithScriptCall 只在 Depth>0 注入 ⇒ 脚本里 tools.bash(...) 静默退回 20KB 文本。
	// 两者都不报错、只在行为上少东西，是 Wave 2 最容易踩的一处（复核实测复现）。
	if call.ParentCallId != "" && opts.Depth <= 0 {
		opts.Depth = 1
		call.Depth = opts.Depth
	}

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

	// 深度接线必须先于执行：工具（subagent 的 maxSpawnDepth、审计等）读的就是
	// 本次执行拿到的这个 ctx。
	ctx = withNestedCallDepth(ctx, opts.Depth)
	if opts.Depth > 0 {
		// 脚本内调用标记：Depth>0 即「由编排脚本发起」。工具据此决定要不要为本次调用
		// 产出只在脚本路径有意义的产物（结构化结果、超限落盘 —— 见 WithScriptCall）。
		// Depth==0 是编排工具自己被模型直呼，不进脚本，不标记。
		ctx = WithScriptCall(ctx)
	}
	handler := nestedApprovalHandler(ctx)

	rec := events.NestedRecorderFrom(ctx)
	if rec == nil {
		return e.executeApproved(ctx, call, handler)
	}

	start := time.Now()
	r := e.executeApproved(ctx, call, handler)

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
		// 用量独立累加：即使 Add 因限额丢弃了记录，用量仍必须计入 ——
		// 由引擎在本次编排调用收尾时合并进外层 ToolResult.Usage（engine.go finish）。
		rec.AddUsage(r.Usage)
	}
	return r
}

// ScriptCallKey 脚本内调用的 ctx 标记键（见 WithScriptCall / IsScriptCall）。
type ScriptCallKey struct{}

// WithScriptCall 标记「本次工具调用来自编排脚本」（引擎在 ExecuteOne 里注入，Depth>0）。
//
// 为什么需要这个标记：工具的结果有两条消费路径 —— 直接进模型上下文，或进脚本。
// 有些「为脚本准备的能力」在模型路径是纯成本：
//   - 结构化结果（tools.OutputSchemaProvider）：脚本按字段过滤/聚合，模型只看文本；
//   - 超限落盘（bash 的 1 MiB spill）：脚本能读回全文，模型路径没人消费，
//     却在磁盘上留下一份命令输出（永不删除，隐私面 + 磁盘泄漏）。
//
// 引擎用它区分，工具据此决定「要不要为本次调用多做一份更重的产物」。
// 除引擎（tools.ExecuteOne）与测试外，任何调用方都不该手工设置它。
func WithScriptCall(ctx context.Context) context.Context {
	return context.WithValue(ctx, ScriptCallKey{}, true)
}

// IsScriptCall 报告本次调用是否来自编排脚本（见 WithScriptCall）。
func IsScriptCall(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(ScriptCallKey{}).(bool)
	return v
}

// withNestedCallDepth 把「单次运行内的调用嵌套深度」叠加到运行深度上交给工具。
//
// 组合而非覆盖：events.ToolContext.Depth 是**运行**深度（AgentLoop 按「运行」注入，
// agents/agent_loop.go:2324），opts.Depth 是本次调用在**运行内**的嵌套层数。工具要的
// 是两者之和 —— subagent 按 childDepth = tc.Depth + 1 判定 maxSpawnDepth
// （subagent/subagent.go:180）。覆盖会丢掉运行深度（子 agent 内跑脚本时按 0 重算），
// 不接则脚本能把嵌套整层隐掉。
func withNestedCallDepth(ctx context.Context, depth int) context.Context {
	if depth <= 0 {
		return ctx
	}
	tc := events.ToolContextFrom(ctx)
	if tc == nil {
		// 无 ToolContext（framework 外调用/测试）：至少把调用嵌套深度立起来，
		// 别让下游读到 0。无运行可关联，故 RunId 留空。
		return events.WithToolContextForRunTask(ctx, "", "", "", depth, nil, nil, nil)
	}
	return events.WithToolContextForRunTask(ctx, tc.RunId, tc.ParentRunId, tc.TaskId,
		tc.Depth+depth, tc.Handler, tc.Approver, tc.Hooks)
}

// nestedApprovalHandler 嵌套调用的事件出口：**只**透传审批请求，其余事件一律不发。
//
// 为什么不能整个传 nil（原实现）：preApprove 只在 handler != nil 时发
// ToolApprovalRequested（tools/engine.go），而审批等待器是「按 id 注册后阻塞等待」
// 的（session/approval.go，默认 30 分钟窗口）—— 请求不发事件，UI 就永远看不到有人
// 在等确认，调用会白等满窗口再被当成「超时未决」拒绝，并在此期间占住审批队列
// （等人类不是脚本在消耗预算，但这里连人都不会被叫到）。
//
// 为什么不能透传全部事件：带 ParentCallId 的 ToolStart/ToolResponse 归组渲染是
// Phase 2 才接的（规范 §3.3），先透传会让 UI 把嵌套调用提前显示成独立工具行。
//
// 无 ToolContext / 无 handler 时返回 nil（与传入 nil 等价：不发事件）。
func nestedApprovalHandler(ctx context.Context) events.EventHandler {
	tc := events.ToolContextFrom(ctx)
	if tc == nil || tc.Handler == nil {
		return nil
	}
	h := tc.Handler
	return func(c context.Context, ev events.Event) {
		if _, ok := ev.(*events.ToolApprovalRequested); ok {
			h(c, ev)
		}
	}
}

// executeApproved 先走审批再执行 —— 嵌套调用的**权限门必须逐次生效**。
//
// execute() 只**消费** decisions 表，不发起审批；审批发生在 preApprove，
// 而 preApprove 只被 Sequence / Parallel / runBatch 调用（engine.go:451/462/481）。
// 因此嵌套调用**不能**直接调 execute(decisions=nil)，否则声明了 ApprovalRequired
// 的工具（bash / write_file 等）会在完全没有审批的情况下执行 —— 一条真实的越权路径。
// 由TestExecuteOneRespectsApproval捕获（曾观察到 result="SHOULD-NOT-RUN"）。
//
// 时限语义与批内对齐（设计文档 §16.3）：
//   - **审批**用调用方 ctx（批前统一审批，不吃单次工具时限 —— 否则人还在看确认弹窗，
//     工具就先被自己的时限判死）；
//   - **执行**用 timeoutFor 算出的单次时限（工具级 ToolTimeout/ToolTimeoutProvider
//     声明 → PerCallTimeout 按参数覆盖），落在可摘离的 liftableTimeout 上
//     ——与 runBatch 同一份实现，不另造闸门。
//
// 审批事件走调用方传入的 handler（ExecuteOne 传的是「只透传审批」的过滤器）。
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

	execCtx := ctx
	if timeout := e.timeoutFor(ctx, call); timeout > 0 {
		lb := newLiftableTimeout(ctx, timeout)
		defer lb.stop() // 回收时限盒（停表 + 退 watcher），防 timer 泄漏
		execCtx = lb
	}
	return e.execute(execCtx, call, handler, decisions, "")
}
