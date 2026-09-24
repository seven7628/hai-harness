package tools

import (
	"context"
	"encoding/json"
	"github.com/seven7628/hai-harness/core"
	"sync"
	"sync/atomic"
	"time"
)

// toolTask 一次工具调用在批内的执行任务（goroutine + 可摘离 ctx + 可释放等待）。
// promote 语义：宿主 PromoteTask → BatchController.Promote → 批等待循环摘离本任务
// （Detach + 占位结果 + pending--），原 Call 继续在后台跑（存活过 run 结束/abort），
// 完成后经 onDone 自动回传主 Runtime（决策 #1/#3/#4）。
// interrupt 语义：宿主 InterruptTask → BatchController.Interrupt → 取消本任务执行 ctx
// （Detach 后只影响本任务，不级联批/主会话），完成后经 onDone 以 interrupted 终态回传。
type toolTask struct {
	taskID  string
	index   int // 在批内 calls 的下标（结果对齐锚）
	call    core.ToolCall
	rctx    *core.Reparentable // 可摘离上下文（Value 委托父，Detach 后父取消不级联）
	taskCtx context.Context    // 实际执行 ctx（= rctx；tools 设了单次时限时为 liftableTimeout）
	lb      *liftableTimeout   // 单次时限盒（promote 时 Lift 解除 → 后台任务不限时）；nil = 不限时
	done    chan struct{}      // 完成信号（结果已写后关闭）
	result  core.ToolResult

	promoted    atomic.Bool  // 已摘离（promote 后：占位结果 + 完成走 onDone，不收集）
	interrupt   atomic.Bool  // 已请求精确中断（Interrupt 置位；deliver 读取 → interrupted 终态）
	phOnce      sync.Once    // 占位结果/事件只写一次（手动 promote 与自动摘离路径并发安全）
	started     atomic.Int64 // 实际开始执行时间（UnixNano；0 = 未开始）——自动摘离阈值锚
	once        sync.Once    // promote 信号只发一次（防重复入 promoteCh）
	deliverOnce sync.Once    // onDone + taskDone 只执行一次（goroutine 与批等待循环补发竞态安全）
}

// stopTimeout 任务正常结束后回收时限盒（停表 + 退 watcher）。
func (t *toolTask) stopTimeout() {
	if t.lb != nil {
		t.lb.stop()
	}
}

// deliver 摘离任务的完成交付：onDone（Session 接线 → PushTaskResultStatus 自动回传）
// + taskDone（map 清理 / 计数递减 / 控制器回收判定）。deliverOnce 保证只发一次：
// 批等待循环的 done-check 补发与任务 goroutine 的完成路径并发安全。
func (t *toolTask) deliver(b *BatchController) {
	if b == nil {
		return
	}
	t.deliverOnce.Do(func() {
		if !t.promoted.Load() {
			return // 未摘离：结果走批收集，不交付（防御）
		}
		if b.onDone != nil {
			b.onDone(t.taskID, t.result, t.interrupt.Load())
		}
		b.taskDone(t.taskID)
	})
}

// BatchController 批控制器：runTools 期间挂到 ac，宿主经 Session.PromoteTask 摘离运行中
// 工具、经 Session.InterruptTask 精确中断（tooltask-*）。promoteCh 是 runBatch 等待循环
// 的摘离信号（缓冲 = 任务数，防阻塞）。
// 生命周期：runBatch 返回（release）后控制器仍存活，直到批内全部已摘离任务完成
// （promotedRunning == 0）→ 经 onIdle 回调宿主注销（防 Session 注册表泄漏）。
type BatchController struct {
	mu        sync.Mutex
	tasks     map[string]*toolTask
	promoteCh chan string
	onDone    func(taskID string, r core.ToolResult, interrupted bool) // 摘离任务完成回调（Session 接线 → PushTaskResultStatus）

	promotedRunning atomic.Int64 // 已摘离且未交付的任务数（>0 = 控制器仍需保留供精确中断路由）
	released        atomic.Bool  // runBatch 已返回（不再摘离；无摘离任务时立即回收）
	idleOnce        sync.Once    // onIdle 只触发一次
	onIdle          func(b *BatchController)
}

// NewBatchController 创建批控制器。capacity = 批内调用数（promoteCh 缓冲，防阻塞）；
// onDone = 摘离任务完成回调（nil = 摘离结果仅事件、不推回）。
func NewBatchController(capacity int, onDone func(taskID string, r core.ToolResult, interrupted bool)) *BatchController {
	return &BatchController{
		tasks:     make(map[string]*toolTask),
		promoteCh: make(chan string, capacity),
		onDone:    onDone,
	}
}

// SetOnIdle 注入控制器回收回调（宿主注销精确中断路由）：批内全部已摘离任务完成且
// runBatch 已返回时触发一次。nil = 不回收（直接运行、无宿主的场景）。
func (b *BatchController) SetOnIdle(fn func(b *BatchController)) {
	b.mu.Lock()
	b.onIdle = fn
	b.mu.Unlock()
}

// detach 摘离标记（promote 公共路径：手动 Promote / 自动阈值 markPromoted 共用）：
// 先计数后置位（消除「promoted 可见但未计数 → 完成交付先于计数」竞态窗口），
// Detach（存活过 run 结束/abort）+ 解除时限（promote 后台任务 = 不限时）。
// 幂等：重复 detach 不重复计数（CAS）。
func (b *BatchController) detach(t *toolTask) {
	if !t.promoted.CompareAndSwap(false, true) {
		return // 已摘离：不重复计数（手动与自动路径并发安全）
	}
	b.promotedRunning.Add(1)
	t.rctx.Detach()
	if t.lb != nil {
		t.lb.lift()
	}
}

// Promote 摘离一个运行中的工具任务。已完成/未知任务返回 false（无副作用）。
// 摘离是同步的：返回 true 时任务已 Detach（存活过 run 结束/abort）——
// 消除「Promote 返回 → 等待循环处理占位」窗口内 run abort 误杀摘离任务（决策 #3）。
// 幂等：重复 promote / 已自动摘离的任务返回 true 但不重复发信号（once）。
func (b *BatchController) Promote(taskID string) bool {
	b.mu.Lock()
	t, ok := b.tasks[taskID]
	b.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case <-t.done: // 已完成：不可摘离（结果已收集或已走 onDone）
		return false
	default:
	}
	b.detach(t)
	t.once.Do(func() { b.promoteCh <- taskID })
	return true
}

// Interrupt 精确中断一个运行中的工具任务（Session.InterruptTask 入口）：
// 置 interrupt 标记 + 取消任务自身执行 ctx（Detach 后只影响本任务——promoted 任务
// 已脱离批量 ctx，不会级联主会话/其他任务；未摘离任务同样只取消自身，其余并行任务不受影响）。
// 返回 true = 已受理本次中断；false = 未知任务 / 已完成 / 已在中断流程中（重复停止）。
// 终态收敛：任务执行被取消后，经 deliver → onDone(interrupted=true) 以 interrupted 状态回传。
func (b *BatchController) Interrupt(taskID string) bool {
	b.mu.Lock()
	t, ok := b.tasks[taskID]
	b.mu.Unlock()
	if !ok {
		return false
	}
	select {
	case <-t.done: // 已完成：无可中断（结果已收集或已走 onDone）
		return false
	default:
	}
	if !t.interrupt.CompareAndSwap(false, true) {
		return false // 已在中断流程中：重复停止返回 false → 上层明确错误
	}
	t.rctx.Cancel()
	return true
}

// ToolTaskSnapshot 后台工具任务（tooltask-*，已摘离且在途）的最小快照：
// TaskList / 压缩交接 [TaskStates] 经 Session.snapshotToolTasks 合并展示用。
type ToolTaskSnapshot struct {
	TaskID       string
	Name         string
	Args         string
	StartedAt    time.Time // 零值 = 尚未开始实际执行（自动摘离阈值锚未打点）
	Interrupting bool      // 已受理精确中断（终态稍后经 onDone 回传）
}

// Snapshot 枚举批内在途已摘离任务。已交付任务经 taskDone 移出 map（天然只含在途）；
// 未摘离任务仍在批内同步执行，不属于「后台任务」，不枚举。
func (b *BatchController) Snapshot() []ToolTaskSnapshot {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]ToolTaskSnapshot, 0, len(b.tasks))
	for _, t := range b.tasks {
		if !t.promoted.Load() {
			continue
		}
		out = append(out, ToolTaskSnapshot{
			TaskID:       t.taskID,
			Name:         t.call.Name,
			Args:         t.call.Arguments,
			StartedAt:    time.Unix(0, t.started.Load()),
			Interrupting: t.interrupt.Load(),
		})
	}
	return out
}

// Has 任务是否仍在本控制器注册表内（精确中断路由的「已知但已终态」判定）。
func (b *BatchController) Has(taskID string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, ok := b.tasks[taskID]
	return ok
}

// taskDone 已摘离任务完成交付后的清理：移出 map（终态任务不可再中断）+ 计数递减
// + 可能触发控制器回收。
func (b *BatchController) taskDone(taskID string) {
	b.mu.Lock()
	delete(b.tasks, taskID)
	b.mu.Unlock()
	b.promotedRunning.Add(-1)
	b.maybeIdle()
}

// release runBatch 返回时调用：标记批已结束；无在途已摘离任务 → 立即回收。
func (b *BatchController) release() {
	b.released.Store(true)
	b.maybeIdle()
}

// maybeIdle 控制器是否已可回收：批已返回且全部已摘离任务交付完成 → onIdle 一次。
func (b *BatchController) maybeIdle() {
	if !b.released.Load() || b.promotedRunning.Load() != 0 {
		return
	}
	b.idleOnce.Do(func() {
		b.mu.Lock()
		fn := b.onIdle
		b.mu.Unlock()
		if fn != nil {
			fn(b)
		}
	})
}

// placeholderResult promote 的调用返回给主 loop 的占位结果：
// 写入历史 tool 消息（模型可见"后台运行中"），真实结果稍后经 sink → PushTaskResult 推回
// （决策 #4；提示词注记引导模型不重复调用，见 base_prompt.go）。
func placeholderResult(t *toolTask) core.ToolResult {
	b, _ := json.Marshal(map[string]any{"task_id": t.taskID, "status": "running", "type": "tool", "name": t.call.Name})
	return core.ToolResult{Id: t.call.Id, Result: string(b), IsError: false}
}
