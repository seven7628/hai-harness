package subagent

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"github.com/seven7628/hai-harness/core"
	"strings"
	"sync"
	"time"
)

// taskIDAlphabet 随机 task id 字符集（小写字母+数字，去歧义字符 0/o/1/l/i）。
// 8 字符 × 32 字母表 = 40 bit 熵：跨 Registry/进程/会话碰撞概率可忽略，
// 彻底消除旧 task-N 递增序列在多 registry（每会话一个）下产生的 task-1/task-2 重复。
const taskIDAlphabet = "abcdefghjkmnpqrstuvwxyz23456789"

// newTaskID 生成 task-<8位随机>（crypto/rand，不依赖 registry 共享递增序列）。
func newTaskID() string {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		// crypto/rand 失败（极罕见）：回退时间戳混合，仍保证唯一性
		return fmt.Sprintf("task-%08x", time.Now().UnixNano()&0xffffffff)
	}
	for i := range b {
		b[i] = taskIDAlphabet[int(b[i])%len(taskIDAlphabet)]
	}
	return "task-" + string(b[:])
}

// 任务注册表（异步多 agent）：Session 持单例（WithRegistry 注入），
// 后台子任务跨 Run 生命周期存活（Run 结束子任务仍跑，结果自动回传主 Runtime）。
// 持久化语义：后台任务内存态——崩溃恢复后 AbandonAll 标记 abandoned
// （序列化运行中 goroutine 不现实，CC/Codex 同样不承诺，文档写明）。
//
// 批超时豁免：Wait 的阻塞与并发槽获取用 Session 级 ctx（SetSessionCtx 注入），
// 不走工具调用的批 ctx —— Wait 挂起超过 ToolBatchTimeout 不被整批误杀。
// 代价：槽满时等待**没有时限**（只有会话关闭能解除），所以容量是安全边界而非限速器
// ——默认放到 DefaultConcurrency（1000）见 slots.go；调小需自行承担「工具调用被槽卡住」。

// TaskStatus 后台任务状态。
type TaskStatus string

const (
	TaskRunning      TaskStatus = "running"
	TaskInterrupting TaskStatus = "interrupting"
	TaskCompleted    TaskStatus = "completed"
	TaskInterrupted  TaskStatus = "interrupted"
	TaskFailed       TaskStatus = "failed"
	TaskAbandoned    TaskStatus = "abandoned" // 崩溃恢复后（AbandonAll 标记）
)

// Task 一个后台子任务。
type Task struct {
	ID       string
	Name     string // 短标签（agent_spawn 的 name：这个 agent 在干什么）
	ToolName string
	Status   TaskStatus
	Result   string // 完成后最终回复
	Usage    core.Usage
	Err      error

	// outputFile journal 路径（"" = 未启用落盘）。构造期赋值（StartOptions.OutputFile）、
	// 发布（map 登记 + goroutine 启动）后不可变 → OutputFile() 免锁直读。
	// 勿改为运行中可写：那需要重审锁语义（docs/SUBAGENT_OUTPUT_JOURNAL_TASKOUTPUT.md §3.2）。
	outputFile string

	abort          func() // 子 Run 的 ac.Abort（OnRunning 捕获）——agent_interrupt 调用（非失败终止）
	abortRequested bool   // interrupt 早于 OnRunning 时的待处理请求（消除捕获窗口）

	done  chan struct{}
	inbox chan string // agent_send 投递 → 子 RunStream Poll drain

	waiting bool // 宿主 Wait 在途（结果已由 Wait 消费 → 完成时不触发 OnTaskDone）

	mu sync.Mutex // Status/Result/Usage/Err/abort/waiting 保护（后台 goroutine 写、主线程读）
}

// OutputFile 返回 journal 路径；构造期赋值、发布后不可变，免锁直读。
func (t *Task) OutputFile() string { return t.outputFile }

// setAbort 捕获子 Run 的中止句柄（OnRunning 回调里调用）。
// 若 interrupt 已先行（abortRequested），立即执行——消除「中断早于子 Run
// 启动」的丢失窗口。
func (t *Task) setAbort(f func()) {
	t.mu.Lock()
	t.abort = f
	req := t.abortRequested
	t.mu.Unlock()
	if req && f != nil {
		f()
	}
}

// addUsage 把一轮已完成 LLMEnd 的用量实时入账到任务（runBackground 的 handler 包装调用）。
// 意义：子任务未达终态时（会话/宿主提前结束、后台任务被切断），其已完成轮的用量
// 仍计入 Registry.TotalUsage —— 否则这些已计费 token 只进事件流、不进 Session.Usage
// 聚合（真实场景验证 2026-08-16：审查子 agent 3 轮 23,001 token 因此丢失）。
// 任务完成时 Start 收尾会用 run 返回值覆盖 t.Usage（权威全量），增量仅对 running 阶段有意义。
func (t *Task) addUsage(u core.Usage) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Usage.Input += u.Input
	t.Usage.Output += u.Output
	t.Usage.CacheRead += u.CacheRead
	t.Usage.CacheWrite += u.CacheWrite
	t.Usage.Reasoning += u.Reasoning
	t.Usage.TotalTokens += u.TotalTokens
	t.Usage.Cost.Input += u.Cost.Input
	t.Usage.Cost.Output += u.Cost.Output
	t.Usage.Cost.CacheRead += u.Cost.CacheRead
	t.Usage.Cost.CacheWrite += u.Cost.CacheWrite
	t.Usage.Cost.Total += u.Cost.Total
}

// Abort 调子 Run 的 ac.Abort（AgentEnd finishReason=abort，非失败）；
// 无论子 Run 是否已启动都记录请求，供任务收尾判定 interrupted；子 Run 尚未启动
// （abort 未捕获）时，OnRunning 捕获后立即执行。
func (t *Task) Abort() {
	t.mu.Lock()
	t.abortRequested = true
	f := t.abort
	t.mu.Unlock()
	if f != nil {
		f()
	}
}

// TaskInfo TaskList 枚举项 / Snapshot 单任务快照。
type TaskInfo struct {
	ID         string     `json:"task_id"`
	Kind       string     `json:"kind,omitempty"` // agent = 子 agent；tool = promoted 工具任务（tooltask-*）
	Name       string     `json:"name,omitempty"`
	ToolName   string     `json:"tool_name,omitempty"`
	Status     TaskStatus `json:"status"`
	Error      string     `json:"error,omitempty"`
	OutputFile string     `json:"output_file,omitempty"` // journal 路径（"" = 未启用落盘）
}

// Kind 取值：TaskList/压缩交接 [TaskStates] 里区分子 agent 与 promoted 工具任务
// （后者结果自动回传主会话、无 output_file，TaskOutput 不适用）。
const (
	TaskKindAgent = "agent"
	TaskKindTool  = "tool"
)

// ToolTaskInfo promoted 工具任务（tooltask-*）枚举项：宿主（Session）经
// SetToolTaskProvider 注入拉取闭包，与子 agent 任务在 TaskList / [TaskStates] 合并展示。
type ToolTaskInfo struct {
	ID           string
	Name         string
	Interrupting bool
}

var (
	// ErrTaskNotFound 任务不存在（未知 taskId）。
	ErrTaskNotFound = errors.New("task not found")
	// ErrTaskFinished 任务已结束/被弃置（agent_send 投递给非 running 任务）。
	ErrTaskFinished = errors.New("task already finished")
	// ErrTaskInboxFull 任务消息队列已满（非阻塞写丢弃）。
	ErrTaskInboxFull = errors.New("task inbox full")
	// ErrTaskAbandoned 任务已弃置（恢复场景，Wait 拒绝）。
	ErrTaskAbandoned = errors.New("task abandoned")
)

// Registry 后台任务注册表（并发安全）。
// defaultAsyncTaskTimeout 后台子 agent 任务的执行上限（超时 = 终止并按「超时」上报，
// 与逻辑失败区分）。默认 12h 是刻意的宽松值：1h 会砍掉仍在推进的长任务
// （大型迁移/长构建/长回归），而长时运行正是本 harness 的定位。
// 可用 SetAsyncTimeout 覆盖；不提供「无限」——无上限任务会一直占并发槽。
const defaultAsyncTaskTimeout = 12 * time.Hour

type Registry struct {
	mu           sync.Mutex
	tasks        map[string]*Task
	sessionCtx   context.Context                                                                   // 批超时豁免的等待 ctx（Session 注入；nil = Background）
	onTaskDone   func(taskID, name, result string, status TaskStatus, usage core.Usage, err error) // 任务完成回调（宿主接线：自动回传主 Runtime）
	slots        *SlotPool                                                                         // 主池：写型子 agent（agent_spawn 等）
	auxSlots     *SlotPool                                                                         // 辅池：只读/轻量子 agent（宿主经 AuxSlots() 取用，如 subagent_explore）
	asyncCtx     context.Context
	asyncCancel  context.CancelFunc
	asyncTimeout time.Duration // 后台任务执行上限（0 = defaultAsyncTaskTimeout）
	wg           sync.WaitGroup
	closed       bool

	// promoted 工具任务（tooltask-*）接入点（Session 组装时接线；nil = 未接线）：
	// toolTasks 枚举在途已摘离任务（TaskList / [TaskStates] 合并展示）；
	// toolTaskInterrupt 精确中断路由（agent_interrupt → Session.InterruptTask → 批控制器）。
	toolTasks         func() []ToolTaskInfo
	toolTaskInterrupt func(taskID string) error
}

// SetToolTaskProvider 注入 promoted 工具任务枚举闭包（Session 组装时接线；nil = 清除）。
func (r *Registry) SetToolTaskProvider(fn func() []ToolTaskInfo) {
	r.mu.Lock()
	r.toolTasks = fn
	r.mu.Unlock()
}

// SetToolTaskInterrupt 注入 promoted 工具任务精确中断路由（Session 组装时接线）。
func (r *Registry) SetToolTaskInterrupt(fn func(taskID string) error) {
	r.mu.Lock()
	r.toolTaskInterrupt = fn
	r.mu.Unlock()
}

// ToolTaskByID 按 id 查询「已摘离为后台的工具任务」（tooltask-*）。第二个返回值 = 是否找到。
// 这类任务没有 Task 实例（无 journal/无 done 通道），只有宿主提供的枚举闭包可查。
// 语义与 List 的合并展示一致：未接线或 id 不在在途集合内 → 未找到。
func (r *Registry) ToolTaskByID(id string) (ToolTaskInfo, bool) {
	r.mu.Lock()
	fn := r.toolTasks
	r.mu.Unlock()
	if fn == nil {
		return ToolTaskInfo{}, false
	}
	for _, t := range fn() {
		if t.ID == id {
			return t, true
		}
	}
	return ToolTaskInfo{}, false
}

// NewRegistry 创建注册表（主/辅两个并发槽池：默认容量 DefaultConcurrency = 1000，
// 可经 SetConcurrency / SetAuxConcurrency 运行期调整）。
func NewRegistry() *Registry {
	ctx, cancel := context.WithCancel(context.Background())
	return &Registry{
		tasks:        make(map[string]*Task),
		slots:        NewSlotPool(DefaultConcurrency),
		auxSlots:     NewSlotPool(DefaultConcurrency),
		asyncCtx:     ctx,
		asyncCancel:  cancel,
		asyncTimeout: defaultAsyncTaskTimeout,
	}
}

// SetConcurrency 调整主池容量（写型子 agent；<=0 = 恢复默认）。
// 在途任务不受影响（各自归还自己获取的池）；对之后的新派发立即生效。
func (r *Registry) SetConcurrency(n int) { r.slots.SetCapacity(n) }

// Concurrency 主池当前容量。
func (r *Registry) Concurrency() int { return r.slots.Capacity() }

// SetAuxConcurrency 调整辅池容量（只读/轻量子 agent，如 subagent_explore；<=0 = 恢复默认）。
func (r *Registry) SetAuxConcurrency(n int) { r.auxSlots.SetCapacity(n) }

// AuxConcurrency 辅池当前容量。
func (r *Registry) AuxConcurrency() int { return r.auxSlots.Capacity() }

// AuxSlots 辅池（宿主把它注入轻量工具的 Spec.Slots：explore 类任务占满辅池时，
// 主池的 agent_spawn 不被饿死——分池理由见 slots.go）。
func (r *Registry) AuxSlots() *SlotPool { return r.auxSlots }

// SetAsyncTimeout 覆盖后台子 agent 任务的执行上限（<=0 = 恢复默认 12h）。
// 大型迁移/长构建等单任务远超默认值的场景由宿主显式放大；
// 时间点语义 = 任务从入槽开跑起算（不含排队等待）。
func (r *Registry) SetAsyncTimeout(d time.Duration) {
	if d <= 0 {
		d = defaultAsyncTaskTimeout
	}
	r.mu.Lock()
	r.asyncTimeout = d
	r.mu.Unlock()
}

// currentAsyncTimeout 读取当前上限（未设置 → 默认）。
func (r *Registry) currentAsyncTimeout() time.Duration {
	r.mu.Lock()
	d := r.asyncTimeout
	r.mu.Unlock()
	if d <= 0 {
		return defaultAsyncTaskTimeout
	}
	return d
}

// SetSessionCtx 注入 Session 级 ctx：Wait 阻塞与槽等待豁免批超时的依据
// （Session 组装时调用；nil = 不注入，Wait 用 Background）。
func (r *Registry) SetSessionCtx(ctx context.Context) {
	r.mu.Lock()
	r.sessionCtx = ctx
	r.mu.Unlock()
}

// SetOnTaskDone 注入任务完成回调（线程安全；宿主组装时调用）。任务收尾时若
// 未被 AbandonAll 弃置且无宿主 Wait 在途 → 携最终状态与用量调用一次。
// usage = 该任务全量用量（含成本 µUSD，与 TaskEnd.Usage / Registry.Task.Usage 同源）——
// 宿主据此把「任务花了多少」随结果一并回传（Session.PushTaskResultUsage → 事件携
// TaskResultDelivered.Usage）；后台任务成本不实现 ToolUsageProvider 单通道，
// 本回调即宿主在**结果返回时刻**拿到用量的唯一入口。
// 宿主接线典型动作：Session.PushTaskResultUsage 推结果进主会话 + 唤醒 run loop。
func (r *Registry) SetOnTaskDone(fn func(taskID, name, result string, status TaskStatus, usage core.Usage, err error)) {
	r.mu.Lock()
	r.onTaskDone = fn
	r.mu.Unlock()
}

// waitCtx 批超时豁免的等待 ctx。
func (r *Registry) waitCtx() context.Context {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.sessionCtx != nil {
		return r.sessionCtx
	}
	return context.Background()
}

// StartOptions 后台任务启动参数（结构化，避免 Start* 方法签名继续膨胀）。
type StartOptions struct {
	ToolName  string
	Name      string
	OnStarted func(taskID string) // 任务登记后、goroutine 启动前同步回调（TaskStarted 占位契约）
	// Slots 本任务使用的并发槽池（nil = Registry 主池）。宿主可注入独立池
	// （如 Registry.AuxSlots()）让不同工具族互不饿死。
	Slots *SlotPool
	// JournalPath journal 路径解析器：start() 在 Task 构造期调用（此处才拿到生成 id），
	// 产物随构造赋值 → 发布前就绪、无竞态窗口；返回错误/nil 解析器 = 不落盘。
	JournalPath func(taskID string) (string, error)
}

// Start 启动后台任务：并发槽阻塞获取（ctx 感知——调用方传 Session 级 ctx，
// 与批 ctx 无关，槽满排队不被批超时误杀）。run 在独立 goroutine 执行，
// 任务对象经参数传入（闭包引用返回值会撞「Start 返回前 goroutine 先跑」竞态）。
//
// 注意：槽满时本调用会**同步阻塞在调用方 goroutine**（历史上这是主 Agent 被
// 卡死的根因，见 slots.go DefaultConcurrency 注释）——调用方若是工具 Call，
// 整个工具批都会挂住。默认容量已抬到 1000，宿主也可按需调小/调大。
func (r *Registry) Start(parent context.Context, name string, run func(context.Context, *Task) (string, core.Usage, error)) (*Task, error) {
	return r.StartToolWithOptions(parent, StartOptions{Name: name}, run)
}

// StartTool starts a task while retaining its originating tool name for clients.
func (r *Registry) StartTool(parent context.Context, toolName, name string, run func(context.Context, *Task) (string, core.Usage, error)) (*Task, error) {
	return r.StartToolWithOptions(parent, StartOptions{ToolName: toolName, Name: name}, run)
}

// StartToolWithOnStarted 同 StartTool，额外在「任务登记后、goroutine 启动前」同步回调
// onStarted(taskID)：宿主借此先发 TaskStarted 事件 —— 保证「占位事件先于任何子运行事件」
// 的顺序契约（否则 agent_start 可能先到，前端拿不到 task_id 关联、任务卡无停止按钮）。
func (r *Registry) StartToolWithOnStarted(parent context.Context, toolName, name string, onStarted func(taskID string), run func(context.Context, *Task) (string, core.Usage, error)) (*Task, error) {
	return r.StartToolWithOptions(parent, StartOptions{ToolName: toolName, Name: name, OnStarted: onStarted}, run)
}

// StartToolWithOptions 结构化启动入口：JournalPath 解析器在 Task 构造期被调用
// （拿到生成 id），产物发布前就绪，供 TaskList/TaskOutput 透出。
func (r *Registry) StartToolWithOptions(parent context.Context, o StartOptions, run func(context.Context, *Task) (string, core.Usage, error)) (*Task, error) {
	return r.start(parent, o, run)
}

func (r *Registry) start(parent context.Context, o StartOptions, run func(context.Context, *Task) (string, core.Usage, error)) (*Task, error) {
	r.mu.Lock()
	closed := r.closed
	r.mu.Unlock()
	if closed {
		return nil, errors.New("task registry closed")
	}
	pool := o.Slots
	if pool == nil {
		pool = r.slots
	}
	release, err := pool.Acquire(parent)
	if err != nil {
		return nil, err
	}
	id := newTaskID()
	// journal 路径构造期解析（此处才拿得到生成 id）；失败降级为空（不落盘、任务照跑）
	outputFile := ""
	if o.JournalPath != nil {
		if p, perr := o.JournalPath(id); perr == nil {
			outputFile = p
		}
	}
	t := &Task{
		ID:         id,
		Name:       o.Name,
		ToolName:   o.ToolName,
		Status:     TaskRunning,
		outputFile: outputFile, // 构造期赋值：早于 map 登记与 goroutine 启动，发布后不可变
		done:       make(chan struct{}),
		inbox:      make(chan string, 16),
	}
	r.mu.Lock()
	r.tasks[id] = t
	r.mu.Unlock()

	// 先于 goroutine 启动回调：TaskStarted（占位）必然先于子运行事件（agent_start 等）到达宿主
	if o.OnStarted != nil {
		o.OnStarted(id)
	}

	r.wg.Add(1)
	go func() {
		defer r.wg.Done()
		defer func() { // 释放槽 + 打开 done（Wait 阻塞点）
			release()
			close(t.done)
		}()
		limit := r.currentAsyncTimeout()
		ctx, cancel := context.WithTimeout(r.asyncCtx, limit)
		defer cancel()
		result, usage, err := run(ctx, t)
		// 超时与逻辑失败区分：时限到点被终止时，err 往往是裸的
		// "context deadline exceeded"，用户会误读为任务本身出错而不知该放大上限还是改方向。
		// 用 r.asyncCtx.Err() 排除「会话关闭/AbandonAll 主动取消」这一类（那种不算超时）。
		if err != nil && ctx.Err() == context.DeadlineExceeded && r.asyncCtx.Err() == nil {
			err = fmt.Errorf("background task exceeded its time limit (%s) and was stopped; %w — the rounds completed so far are kept in its journal (readable via TaskOutput); raise the limit (Registry.SetAsyncTimeout) or split the task into smaller ones", limit, err)
		}
		t.mu.Lock()
		abandoned := t.Status == TaskAbandoned // AbandonAll 已标记 → 不改终态、不通知（防往新会话乱推）
		status := t.Status
		if !abandoned {
			t.Result, t.Usage, t.Err = result, usage, err
			switch {
			case t.abortRequested:
				status = TaskInterrupted
			case err != nil:
				status = TaskFailed
			default:
				status = TaskCompleted
			}
			t.Status = status
		}
		waiting := t.waiting // 宿主 Wait 在途：结果已由 Wait 消费，不重复推送
		t.mu.Unlock()
		if r.onTaskDone != nil && !abandoned && !waiting {
			// usage 同源 t.Usage（run 返回值已在上方写入）——回调里不再读 t，避免锁外读竞态
			r.onTaskDone(t.ID, t.Name, result, status, usage, err)
		}
	}()
	return t, nil
}

// Send 向任务投递消息（agent_send）：非阻塞写 Task.inbox（满则丢弃返回
// ErrTaskInboxFull——工具层转结果文本回传 LLM）。已结束/未知任务报错。
func (r *Registry) Send(id, msg string) error {
	t, err := r.get(id)
	if err != nil {
		return err
	}
	t.mu.Lock()
	done := t.Status != TaskRunning
	t.mu.Unlock()
	if done {
		return ErrTaskFinished
	}
	select {
	case t.inbox <- msg:
		return nil
	default:
		return ErrTaskInboxFull
	}
}

// Wait 阻塞等待任务完成（宿主可选 API，不再面向模型——agent_wait 已移除，结果自动回传）：
// 用 Session 级 ctx（豁免批超时）。返回子运行最终回复与错误（abort 非失败：Err 为空）。
// 在途 Wait 置 waiting：任务完成时跳过 OnTaskDone（结果已由 Wait 消费，不重复推送）。
func (r *Registry) Wait(id string) (string, error) {
	t, err := r.get(id)
	if err != nil {
		return "", err
	}
	t.mu.Lock()
	abandoned := t.Status == TaskAbandoned
	if !abandoned {
		t.waiting = true
	}
	t.mu.Unlock()
	defer func() {
		t.mu.Lock()
		t.waiting = false
		t.mu.Unlock()
	}()
	if abandoned {
		return "", ErrTaskAbandoned
	}
	select {
	case <-t.done:
	case <-r.waitCtx().Done():
		return "", r.waitCtx().Err()
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.Result, t.Err
}

// Interrupt 中断任务（agent_interrupt）：先原子标记 interrupting，再调子 Run 的
// ac.Abort。调用成功表示请求已受理；最终状态在子 Run 收尾后变为 interrupted。
// tooltask-*（promoted 工具任务）不在本注册表 —— 路由宿主精确中断
// （Session.InterruptTask → 批控制器取消该任务自身 ctx）；未接线 → 明确未知任务错误。
func (r *Registry) Interrupt(id string) error {
	if strings.HasPrefix(id, "tooltask-") {
		r.mu.Lock()
		fn := r.toolTaskInterrupt
		r.mu.Unlock()
		if fn == nil {
			return ErrTaskNotFound
		}
		return fn(id)
	}
	t, err := r.get(id)
	if err != nil {
		return err
	}
	t.mu.Lock()
	if t.Status != TaskRunning {
		t.mu.Unlock()
		return ErrTaskFinished
	}
	t.Status = TaskInterrupting
	t.mu.Unlock()
	t.Abort()
	return nil
}

// List 枚举全部任务（TaskList / 压缩交接 [TaskStates]）：子 agent 注册表任务 +
// 宿主注入的 promoted 工具任务（tooltask-*，在途已摘离）合并同榜 —— 后台任务一处可见。
func (r *Registry) List() []TaskInfo {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]TaskInfo, 0, len(r.tasks))
	for _, t := range r.tasks {
		t.mu.Lock()
		info := t.info()
		if t.Err != nil {
			info.Error = t.Err.Error()
		}
		out = append(out, info)
		t.mu.Unlock()
	}
	if r.toolTasks != nil {
		for _, tt := range r.toolTasks() {
			st := TaskRunning
			if tt.Interrupting {
				st = TaskInterrupting
			}
			out = append(out, TaskInfo{
				ID:       tt.ID,
				Kind:     TaskKindTool,
				Name:     tt.Name,
				ToolName: tt.Name,
				Status:   st,
			})
		}
	}
	return out
}

// Snapshot 单任务快照（TaskOutput 的数据源）：任意状态任务可得（含 abandoned ——
// get 对全状态返回对象，见 registry.go 底部）；未知 id → ErrTaskNotFound。
// 组装规则与 List 一致（Error 含 t.Err 文本）。
func (r *Registry) Snapshot(id string) (TaskInfo, error) {
	t, err := r.get(id)
	if err != nil {
		return TaskInfo{}, err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	info := t.info()
	if t.Err != nil {
		info.Error = t.Err.Error()
	}
	return info, nil
}

// Done 返回任务完成时关闭的通道（宿主/工具阻塞等待用）。
// 未知任务返回 nil（调用方按「无法等待」处理，退回快照语义）。
func (r *Registry) Done(id string) <-chan struct{} {
	t, err := r.get(id)
	if err != nil {
		return nil
	}
	return t.done
}

// info 组装 TaskInfo（调用方需已持 t.mu；outputFile 不可变字段直读亦安全）。
func (t *Task) info() TaskInfo {
	return TaskInfo{
		ID:         t.ID,
		Kind:       TaskKindAgent,
		Name:       t.Name,
		ToolName:   t.ToolName,
		Status:     t.Status,
		OutputFile: t.outputFile,
	}
}

// Close cancels all asynchronous tasks and waits for their goroutines to exit.
func (r *Registry) Close(ctx context.Context) error {
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		r.asyncCancel()
	}
	r.mu.Unlock()
	done := make(chan struct{})
	go func() { r.wg.Wait(); close(done) }()
	select {
	case <-done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// 同进程共享注册表的场景下旧任务仍会跑到 done，但新 Session 经 Wait 拒绝）。
func (r *Registry) AbandonAll(reason string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, t := range r.tasks {
		t.mu.Lock()
		if t.Status == TaskRunning {
			t.Status = TaskAbandoned
		}
		t.mu.Unlock()
	}
}

// TotalUsage 全部任务用量合计（Session.Usage/Cost 合成——后台任务成本跨 Run 不丢；
// 不持久化，恢复后随 AbandonAll 丢弃）。
// 统计口径：除 TaskAbandoned 外，执行中（含 interrupting）的已完成轮增量和所有终态
// 都计入。目的：会话/宿主在后台任务未完成时提前结束，
// 其已完成轮的已计费 token 仍完整反映在 Session.Usage（真实场景验证 2026-08-16：
// 审查子 agent 3 轮 23,001 token 此前因此丢失，见 registry.go Task.addUsage）。
func (r *Registry) TotalUsage() core.Usage {
	r.mu.Lock()
	defer r.mu.Unlock()
	var u core.Usage
	for _, t := range r.tasks {
		t.mu.Lock()
		if t.Status == TaskRunning || t.Status == TaskInterrupting || t.Status == TaskCompleted || t.Status == TaskInterrupted || t.Status == TaskFailed {
			u.Input += t.Usage.Input
			u.Output += t.Usage.Output
			u.CacheRead += t.Usage.CacheRead
			u.CacheWrite += t.Usage.CacheWrite
			u.Reasoning += t.Usage.Reasoning
			u.TotalTokens += t.Usage.TotalTokens
			u.Cost.Input += t.Usage.Cost.Input
			u.Cost.Output += t.Usage.Cost.Output
			u.Cost.CacheRead += t.Usage.Cost.CacheRead
			u.Cost.CacheWrite += t.Usage.Cost.CacheWrite
			u.Cost.Total += t.Usage.Cost.Total
		}
		t.mu.Unlock()
	}
	return u
}

// get 按 id 查任务（带锁）。
func (r *Registry) get(id string) (*Task, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	t, ok := r.tasks[id]
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrTaskNotFound, id)
	}
	return t, nil
}
