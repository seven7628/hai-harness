package session

import (
	"context"
	"errors"
	"fmt"
	"github.com/seven7628/hai-harness/agents"
	"github.com/seven7628/hai-harness/core"
	"github.com/seven7628/hai-harness/events"
	"github.com/seven7628/hai-harness/provider"
	"github.com/seven7628/hai-harness/subagent"
	"github.com/seven7628/hai-harness/todo"
	"github.com/seven7628/hai-harness/tools"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var (
	// ErrNotFound 会话不存在（Store.Load 未命中，视为新会话）。
	ErrNotFound = errors.New("session not found")
	// ErrBudgetExceeded 会话成本已达上限，拒绝新输入。
	ErrBudgetExceeded = errors.New("session cost budget exceeded")
	// ErrQueueFull 输入队列已满，拒绝新输入。
	ErrQueueFull = errors.New("session input queue full")
	// ErrSessionClosed 会话已关闭（Shutdown 后拒绝新输入与新运行）。
	ErrSessionClosed = errors.New("session closed")
)

// State 会话状态快照：有效上下文 + 累计用量/成本 + 未消费的输入积压。
type State struct {
	Messages  []core.Message `json:"messages"` // 有效上下文（发给模型的，可能压缩过）
	Usage     core.Usage     `json:"usage"`
	Cost      core.Cost      `json:"cost"`
	Pending   []core.Message `json:"pending,omitempty"` // 队列积压（崩溃恢复时用户输入不丢）
	Todo      []todo.Item    `json:"todo,omitempty"`    // 待办清单（独立于对话上下文，压缩不涉及）
	UpdatedAt time.Time      `json:"updated_at"`
}

// SessionData 恢复数据：完整历史（消息日志）+ 最后状态。
// History 与 State.Messages 的区别：History 是 append-only 的全量消息日志
// （压缩/替换永不删除历史行），State.Messages 是当前有效上下文（压缩后形态）。
type SessionData struct {
	History []core.Message `json:"history"`
	State   *State         `json:"state,omitempty"`
}

// Store 会话存储（可插拔：内存 / 文件 / DB）。
// 日志（AppendHistory）与状态（SaveState）分离：恢复时 History 保证完整上下文，
// State 提供续跑所需的有效上下文与成本。
type Store interface {
	Load(ctx context.Context, sessionId string) (*SessionData, error)
	AppendHistory(ctx context.Context, sessionId string, msgs []core.Message) error
	SaveState(ctx context.Context, sessionId string, state *State) error
}

// Locker 可选 Store 接口：会话级排他写锁（单写者保证）。
// 多实例并发运行同一会话会互相踩状态（checkpoint 覆盖 / 成本双计）——
// 实现此接口的 Store（如 FileStore 用 flock）保证同一会话同一时刻
// 只有一个运行实例；MemoryStore 等进程内单例存储无需实现。
type Locker interface {
	// AcquireLock 获取会话排他锁（非阻塞，失败 = 另一实例持有）。
	// 返回的 release 释放锁（锁绑定 Session.Run 生命周期：入口获取、退出释放）。
	AcquireLock(ctx context.Context, sessionId string) (release func(), err error)
}

// CompactionRecorder 可选 Store 接口（O2/G7）：把压缩产物（摘要 / analysis /
// 用量 / 质量警告）落盘为会话 jsonl 的独立 compaction 记录——压缩器决策过程
// 的观测材料与离线重放评估输入。恢复路径忽略该记录（不参与上下文重建）。
// FileStore 已实现；MemoryStore 等进程内存储无需实现。
type CompactionRecorder interface {
	AppendCompaction(ctx context.Context, sessionId string, rec *CompactionRecord) error
}

// MetaRecorder 可选 Store 接口（AI 标题）：把会话展示元数据（AI 生成的标题）落盘为
// 会话 jsonl 的独立 meta 记录（append-only；恢复路径尾扫读取）。FileStore 已实现；
// MemoryStore 等进程内存储无需实现。
type MetaRecorder interface {
	AppendMeta(ctx context.Context, sessionId string, rec *MetaRecord) error
}

// Config Session 配置（创建时经 Option 构建，不可变）。
type Config struct {
	MaxCost core.Cost // 成本上限（Total > 0 时启用；超限拒绝新输入）

	// ApprovalTimeout 工具审批等待时限（默认 60s；超时 = 拒绝并 Block 回传）。
	// 0 = 不启用审批（保持现状：工具直接执行）。
	ApprovalTimeout time.Duration

	// EventHandler Session 级事件回调（持久化失败、预算超限），nil 表示不发送。
	// Run 级事件仍走 Run 的 handler 参数（透传 AgentLoop），两者互不影响。
	EventHandler events.EventHandler

	// Hooks 三个内核拦截点（E：PreToolUse/PostToolBatch/PreCompact），
	// Run 组装时透传进 RunOptions；nil = 不拦截。
	//
	// hooks 能力扩展（2026-09-20）：ToolHooks 另含 PostToolUse / Stop / SessionStart /
	// UserPromptSubmit 四个钩子（见 events/hooks.go）。SessionStart 与 UserPromptSubmit
	// 由 Session 自己派发（前者每会话一次 → 缓存为 system 层注入；后者在 Ask 时追加到
	// 本轮 user 消息），其余透传进 RunOptions。
	//
	// 只作为**初值**：NewSession 时存入 hooks 原子指针（见 Session.hooks 与 SetHooks），
	// 运行期读取一律走该指针，热替换不改这里（保持 Option 语义"创建时配置"）。
	Hooks *events.ToolHooks

	// HookCwd hooks 外部命令的工作目录（同时作为 GOCODE_PROJECT_DIR 导出）；由宿主注入。
	HookCwd string

	// registry 后台任务注册表（异步多 agent）：nil = NewSession 自动创建。
	registry *subagent.Registry

	// questionWaiter 模型→用户提问等待器（ask_user 工具 + Session.AnswerQuestions
	// 共享）：nil = NewSession 自动创建（不接线 UI 则按默认窗口等待、超时告知模型「用户未作答」）。
	questionWaiter *events.QuestionWaiter

	// recoveryState checkpoint-first 恢复源（SessionRecord.sdk_state）：非 nil 时
	// NewSession 优先用它恢复上下文/用量/待办/积压，store.Load 仅作兜底
	// （record 缺失或损坏时仍回退 legacy FileStore）。
	recoveryState *State

	// aiTitle 恢复注入的 AI 标题（jsonl meta 尾扫；NewSession 后置入 Session.aiTitle）。
	aiTitle string
}

type Option func(*Config)

// WithAITitle 注入恢复的 AI 标题（冷启动从会话 jsonl meta 尾扫得到；NewSession 前调用）。
func WithAITitle(title string) Option {
	return func(cfg *Config) { cfg.aiTitle = title }
}

// WithHookCwd 注入 hooks 外部命令的工作目录（同时作为 GOCODE_PROJECT_DIR 导出给 hook 进程）。
// 由宿主（bridge）传入工作区根；空 = hook 拿不到项目根（仍会执行，只是缺少该变量）。
func WithHookCwd(cwd string) Option {
	return func(cfg *Config) { cfg.HookCwd = cwd }
}

// WithRecoveryState 注入 checkpoint-first 的 SDK 恢复状态（SessionRecord.sdk_state）。
// 由宿主（bridge）在创建会话时从合法 record 解析传入；nil = 走 legacy store 恢复。
func WithRecoveryState(state *State) Option {
	return func(cfg *Config) { cfg.recoveryState = state }
}

func WithMaxCost(c core.Cost) Option {
	return func(cfg *Config) { cfg.MaxCost = c }
}

// WithApprovalTimeout 启用工具审批（HITL）并设置等待时限；0 = 不启用。
func WithApprovalTimeout(d time.Duration) Option {
	return func(cfg *Config) { cfg.ApprovalTimeout = d }
}

// WithEventHandler 注入 Session 级事件回调（如 UI 警示「会话未保存」）。
func WithEventHandler(h events.EventHandler) Option {
	return func(cfg *Config) { cfg.EventHandler = h }
}

// WithHooks 注入三个内核拦截点（PreToolUse/PostToolBatch/PreCompact）。
// 子 agent 默认继承（Spec.DisableHookInherit 可关），见 agents/hooks.go。
// 语义：**初值**——创建后可用 SetHooks 热替换（下一次 Run 生效）。
func WithHooks(h *events.ToolHooks) Option {
	return func(cfg *Config) { cfg.Hooks = h }
}

// WithRegistry 注入后台任务注册表（异步多 agent）：默认 NewSession 自动创建，
// 需要与工具注册共享同一实例时显式注入（工具经 subagent.NewBackgroundTool
// 持有；Session 恢复分支对其 AbandonAll、Shutdown 经 Session 级 ctx 级联）。
func WithRegistry(reg *subagent.Registry) Option {
	return func(cfg *Config) { cfg.registry = reg }
}

// WithQuestionWaiter 注入模型→用户提问等待器（ask_user 工具与
// Session.AnswerQuestions 共享同一实例）：默认 NewSession 自动创建。
func WithQuestionWaiter(w *events.QuestionWaiter) Option {
	return func(cfg *Config) { cfg.questionWaiter = w }
}

// Session 是长时运行 Agent Harness 的会话容器：
// 持有跨任务的累积上下文，编排 AgentLoop 执行，持久化与恢复，成本累计与限额。
//
// 输入模型：用户可随时 Ask（入队即返回，不阻塞）；连续输入在执行时被
// drain 聚合成一批（inputs []core.Message）整体插入 —— 插话时机由
// AgentLoop 的 LLM End 检查点决定（工具调用轮不插话）。
// 打断：Ask(WithInterrupt()) 可中断当前运行立即响应新输入（abort 非失败，
// 被中断轮次不产生半截上下文 —— 回写发生在工具成功之后，天然协议安全）。
type Session struct {
	id    string
	loop  *agents.AgentLoop
	store Store
	cfg   Config

	inbox    chan core.Message    // 输入队列（缓冲）
	running  *agents.AgentContext // 当前运行句柄（打断扩展用，小锁保护）
	todo     *todo.Store          // 会话级待办清单（跨轮/跨压缩/跨恢复存活）
	approver *approvalWaiter      // 工具审批等待器（WithApprovalTimeout 启用；nil = 不审批）

	mu              sync.Mutex
	messages        []core.Message // 有效上下文（发给模型的）
	history         []core.Message // 完整历史（Ask 输入 + 每轮 RunHistory，append-only）
	persistedCount  int            // 已写入消息日志的历史条数（增量持久化）
	checkpointed    int            // 本轮已并入 history 的 RunHistory 条数（检查点去重）
	usage           core.Usage     // 累计用量与成本（Cost 内嵌于 Usage，无独立成本字段）
	persistErr      error
	closed          bool                   // Shutdown 已调用（拒绝新输入/新运行；mu 保护）
	inRun           bool                   // Run 正在执行（Shutdown 等待收尾的边界；mu 保护）
	runCancel       context.CancelFunc     // Run 的 ctx 取消句柄（Shutdown 窗口期取消用；mu 保护）
	runningSnapshot []core.Message         // 运行中 checkpoint 的上下文快照（record 提交用；mu 保护）
	mode            *agents.Mode           // 会话当前模式（SetMode 设置；Run 开始快照注入，不持久化）
	commands        map[string]CommandFunc // 自定义命令注册表（RegisterCommand；mu 保护）

	// sessionCtx Session 级上下文（跨项基建③）：后台子任务生命周期（A）、
	// Wait/并发槽的批超时豁免等待 ctx。Shutdown 时级联取消。
	// 注意：不是 per-run ctx 的替代——Run 仍用调用方 ctx。
	sessionCtx    context.Context
	sessionCancel context.CancelFunc
	registry      *subagent.Registry     // 后台任务注册表（WithRegistry 可注入；mu 保护）
	questions     *events.QuestionWaiter // 模型→用户提问等待器（WithQuestionWaiter 可注入）

	// toolBatches 工具批控制器注册表（tooltask-* 精确中断路由；mu 保护）：
	// runTools 创建控制器时注册（RunOptions.BatchCreated），批内全部已摘离任务交付
	// 完成后注销（BatchClosed）——runTools 返回后 promoted 任务仍可被精确中断。
	toolBatches map[*tools.BatchController]struct{}

	// aiTitle AI 生成的会话标题（mu 保护）：首次用户输入后异步生成，随 meta 记录
	// 持久化到会话 jsonl；恢复时由宿主注入（WithAITitle），运行中可经 SetAITitle 更新。
	aiTitle string

	// hooks 能力扩展（mu 保护）：
	//   hooksStarted  SessionStart 是否已触发（每次会话一次；首个 Run 前）。
	//   hooksSource   SessionStart 的 source：startup（新会话）/ resume（恢复的会话）。
	//   systemExtra   SessionStart 注入文本（缓存；每次 Run 返回同一字符串 →
	//                 provider 前缀缓存在会话内保持稳定，不被逐轮注入击穿）。
	hooksStarted bool
	hooksSource  string
	systemExtra  string

	// hooks 当前聚合（热替换：配置面板改 hooks 后 SetHooks 换新，下一次 Run 生效）。
	// 用原子指针而不是 s.cfg.Hooks/mu：Ask 与 Run 都在运行路径上高频读它，且设值方
	// （bridge 的 hooks_set 热应用）与读取方（run goroutine）天然并发 —— 加锁读会把
	// 配置热应用卷进运行热路径；原子的语义也最直白：读取方拿到的是**某一份完整快照**。
	hooks atomic.Pointer[events.ToolHooks]
}

// NewSession 创建（或恢复）会话：store 命中则从快照恢复历史/成本/积压输入。
func NewSession(id string, loop *agents.AgentLoop, store Store, opts ...Option) (*Session, error) {
	s := &Session{
		id:          id,
		loop:        loop,
		store:       store,
		inbox:       make(chan core.Message, 64),
		todo:        todo.New(),
		toolBatches: make(map[*tools.BatchController]struct{}),
	}
	// Session 级 ctx（跨项基建③）：后台任务生命周期 + 批超时豁免等待
	s.sessionCtx, s.sessionCancel = context.WithCancel(context.Background())
	for _, o := range opts {
		o(&s.cfg)
	}
	// hooks 初值：WithHooks 注入的聚合存入原子指针（运行期读取一律走它；SetHooks 可换）。
	// nil 也照样存（atomic.Pointer 零值即 nil）——「从未配置」与「配置后被清空」走同一条读取路径。
	s.hooks.Store(s.cfg.Hooks)
	if s.cfg.ApprovalTimeout > 0 {
		s.approver = newApprovalWaiter(s.cfg.ApprovalTimeout)
	}
	if s.cfg.registry != nil {
		s.registry = s.cfg.registry // 与工具注册共享同一实例（异步多 agent）
	} else {
		s.registry = subagent.NewRegistry()
	}
	s.registry.SetSessionCtx(s.sessionCtx) // Wait/并发槽等待用 Session 级 ctx（豁免批超时）
	// promoted 工具任务（tooltask-*）接入后台任务面：TaskList / agent_interrupt / 压缩交接
	// [TaskStates] 与子 agent 同榜 —— 此前后台长工具只在 UI 任务栏可见，模型 TaskList 查询为空、
	// agent_interrupt 报未知任务；接线后可见可停，压缩续跑也不会忘记在途后台工具。
	s.registry.SetToolTaskProvider(s.snapshotToolTasks)
	s.registry.SetToolTaskInterrupt(s.InterruptTask)

	if s.cfg.questionWaiter != nil {
		s.questions = s.cfg.questionWaiter // 与 ask_user 工具注册共享同一实例
	} else {
		s.questions = events.NewQuestionWaiter(0) // 默认窗口 30 分钟（超时告知模型「用户未作答」）
	}

	if store != nil {
		// checkpoint-first：宿主注入的 recoveryState（SessionRecord.sdk_state）优先；
		// 缺失/损坏时回退 legacy store（现有路径）。二者二选一，不叠加。
		if s.cfg.recoveryState != nil {
			rst := s.cfg.recoveryState
			s.registry.AbandonAll("session restored")
			s.messages = rst.Messages
			s.usage = rst.Usage
			s.usage.Cost = rst.Cost // 成本以 State.Cost 为权威（兼容 Usage.Cost 未填充）
			s.todo.Restore(rst.Todo)
			for _, m := range rst.Pending {
				s.inbox <- m
			}
		} else {
			data, err := store.Load(context.Background(), id)
			switch {
			case err == nil && data != nil:
				// 恢复分支：后台任务内存态不可续跑（序列化 goroutine 不现实）——
				// running → abandoned（Wait 拒绝，文档写明）
				s.registry.AbandonAll("session restored")
				s.history = data.History
				s.persistedCount = len(data.History)
				if data.State != nil {
					s.messages = data.State.Messages
					s.usage = data.State.Usage
					s.usage.Cost = data.State.Cost  // 成本以 State.Cost 为权威（兼容 Usage.Cost 未填充的旧快照）
					s.todo.Restore(data.State.Todo) // 待办随快照精确还原（压缩不涉及）
					for _, m := range data.State.Pending {
						s.inbox <- m
					}
				}
			case errors.Is(err, ErrNotFound):
				// 新会话
			case err != nil:
				return nil, fmt.Errorf("load session %q: %w", id, err)
			}
		}
	}
	// 恢复注入的 AI 标题（jsonl meta 尾扫结果；无 = 保持空）
	if s.cfg.aiTitle != "" {
		s.aiTitle = s.cfg.aiTitle
	}
	return s, nil
}

// AskOption Ask 的选项。
type AskOption func(*askConfig)

type askConfig struct{ interrupt bool }

// WithInterrupt 打断当前运行：若 agent 正在执行则中断本轮，新输入立即开新轮。
// 打断不是失败（AgentEnd.FinishReason = abort），被中断的轮次不产生半截上下文。
// 注意：这是"软停"语义（中断消息本身作为用户输入开新 run，模型是否真停不保证）；
// 需要"硬停"（中断后不输出、保持 idle）用 Interrupt。
func WithInterrupt() AskOption {
	return func(c *askConfig) { c.interrupt = true }
}

// Interrupt 硬停：abort 当前运行，不入队任何消息。
// 与 Ask(WithInterrupt) 的区别：中断后不自动开新 run —— 模型不再输出，
// 会话保持 idle，等待用户下一条输入才恢复（用户点"停止"的预期语义）。
// 幂等：无运行中 run 时无事发生。被中断轮次不产生半截上下文
// （AgentEnd.FinishReason = abort，非失败）。
func (s *Session) Interrupt() {
	s.mu.Lock()
	running := s.running
	s.mu.Unlock()
	if running != nil {
		running.Abort()
	}
}

// SetMode 设置会话当前模式（plan / normal / all-pass 或自定义）。
// 快照语义：下一次 Run 开始快照生效，运行中的 Run 不受影响。
// 模式不持久化：恢复会话回到默认（nil = 现状行为：全工具 + 自声明审批）。
// 切换由产品驱动（如 plan_submit 确认后 SetMode(NormalMode())）。
func (s *Session) SetMode(m *agents.Mode) {
	s.mu.Lock()
	s.mode = m
	s.mu.Unlock()
}

// Mode 当前会话模式（nil = 未配置，现状行为）。
func (s *Session) Mode() *agents.Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.mode
}

// SetLoop 会话期间切换运行时（Persona 切换：新系统提示词/工具集重建的 AgentLoop）。
// 历史/usage/待办保留（只换执行器）；运行中的 Run 用旧 loop 收尾，下一次 Run 用新 loop。
func (s *Session) SetLoop(loop *agents.AgentLoop) {
	s.mu.Lock()
	s.loop = loop
	s.mu.Unlock()
}

// SetHooks 热替换本会话的 hooks 聚合（配置面板保存 hooks 后由宿主推入）。
//
// 快照语义（与 SetLoop/SetMode 一致，见契约 §7）：**下一次 Run（及下一次 Ask）生效**——
// Run 在入口处取一次快照后用到底，运行中的 Run 不会被换掉（否则同一轮里 PreToolUse 是
// 新配置、PostToolUse 是旧配置，行为不可解释）；Ask 的 UserPromptSubmit 每次输入取当时的值。
//
// 并发安全：读写走 atomic.Pointer，调用方可以与 run goroutine 并发调用（不阻塞运行）。
// 信任状态不需要重新装配（派发时查信任库），但**配置**（事件/matcher/命令）需要换 —— 这就是本方法。
func (s *Session) SetHooks(h *events.ToolHooks) { s.hooks.Store(h) }

// Hooks 当前 hooks 聚合快照（nil = 本会话没有 hooks：读取方全部走零开销路径）。
// 运行期内部读取（Ask/Run）与宿主自检（"配置改完到底装上没有"）都用它。
func (s *Session) Hooks() *events.ToolHooks { return s.hooks.Load() }

func (s *Session) Ask(ctx context.Context, input core.Message, opts ...AskOption) error {
	if s.isClosed() {
		return ErrSessionClosed
	}
	cfg := askConfig{}
	for _, o := range opts {
		o(&cfg)
	}
	if cfg.interrupt {
		s.mu.Lock()
		running := s.running
		s.mu.Unlock()
		if running != nil {
			running.Abort()
		}
	}
	if input.Role != core.User {
		return fmt.Errorf("session input must be user role, got %q", input.Role)
	}
	if s.budgetExceeded() {
		s.emit(&events.SessionBudgetExceeded{
			SessionId: s.id,
			Cost:      s.Cost(),
			MaxCost:   s.cfg.MaxCost,
			EventType: events.SessionBudgetExceededType,
		})
		return ErrBudgetExceeded
	}
	// UserPromptSubmit（hooks 能力扩展）：用户输入被接受、组装请求前的拦截点。
	// 语义与 Claude Code 对齐：① 注入串追加到**本轮 user 消息**（每轮都是"新鲜全价
	// 输入"，所以 Graft 这类工具在这一层只放定位符、不放代码）；② Block → 拒绝该输入
	// （不排队、不落历史，错误信息回传调用方/用户）；③ hook panic → 视为放行（fail-open）。
	// hooks 取当次快照（SetHooks 热替换后新输入立刻用新配置；已在跑的 Run 不受影响）。
	if h := s.Hooks(); h != nil && h.UserPromptSubmit != nil {
		res, panicked := agents.CallUserPromptSubmit(h.UserPromptSubmit, ctx, events.UserPromptSubmitContext{
			SessionId: s.id,
			Cwd:       s.cfg.HookCwd,
			Prompt:    messageTextOf(input),
		})
		if panicked {
			s.emitHookWarning("UserPromptSubmit", "hook panicked, treated as allow")
		}
		if res.Block {
			reason := strings.TrimSpace(res.Reason)
			if reason == "" {
				reason = "blocked by UserPromptSubmit hook"
			}
			return fmt.Errorf("user input rejected by hook: %s", reason)
		}
		input = appendTextToMessage(input, res.AdditionalContext)
	}

	// 线性化闸门（V2 P1-PERSIST-05）：closed 检查 + 入队 + 历史追加在同一临界区，
	// 与 Shutdown 置 closed 互斥——要么 Ask 先完成（Shutdown 等收尾），要么 Shutdown
	// 先置 closed（Ask 在此被拒）。杜绝「关闭边界后输入仍入队」的状态漂移。
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return ErrSessionClosed
	}
	select {
	case s.inbox <- input:
	default:
		s.mu.Unlock()
		return ErrQueueFull
	}
	s.history = append(s.history, input)
	s.mu.Unlock()
	if s.store != nil {
		if s.isRunning() {
			// 运行中：只追加消息日志 —— state 快照由每轮检查点维护。
			// s.messages 是运行开始前的旧状态，全量写会覆盖检查点的新快照
			// （崩溃恢复时上下文倒退，丢运行中产出）。
			if err := s.persistHistory(ctx); err != nil {
				s.recordPersistErr(err) // 与 persist 同一错误语义（persistErr + 事件）
			}
		} else {
			_ = s.persist(ctx, s.messages) // 失败记录 persistErr，不影响入队
		}
	}
	return nil
}

// PushTaskResult 后台任务完成 → 结果全量合成一条消息推入主会话（Ask 同路径：
// inbox + history + persist），并发 TaskResultDelivered 事件（客户端渲染折叠 Markdown 块）。
// 主 loop 下个 Poll/takeBatch 消费 → 模型看到结果续跑。result 是不透明字符串
// （纯文本 / HTML / 脚本等 LLM 输出全量，不截断）。
// 返回错误：ErrSessionClosed / ErrBudgetExceeded / ErrQueueFull（结果仍在 Registry，宿主可 Wait 补取）。
func (s *Session) PushTaskResult(taskID, name, result string, err error) error {
	status := "completed"
	if err != nil {
		status = "failed"
	}
	return s.PushTaskResultStatus(taskID, name, result, status, err)
}

// PushTaskResultStatus 与 PushTaskResult 相同，但由异步子 Agent 显式传递最终状态，
// 使 interrupted 不会因 err=nil 被误报为 completed。
// usage 不可知（旧签名兼容入口）：任务成本随事件另经 TaskEnd.Usage 回传——
// 需要「结果与用量同帧」的宿主用 PushTaskResultUsage。
func (s *Session) PushTaskResultStatus(taskID, name, result, status string, err error) error {
	return s.PushTaskResultUsage(taskID, name, result, status, core.Usage{}, err)
}

// PushTaskResultUsage 全量入口：在 PushTaskResultStatus 基础上附带任务用量（含成本
// µUSD，见 core.Cost）。usage 全零 = 无用量信息（Event/TaskResultDelivered.Usage 留 nil）。
// 用量的两个来源（与 Session.Usage/Cost 合成口径一致，不新增记账）：
//   - 异步子 agent：Registry.OnTaskDone 的 usage 参数（Task 用量，单通道）；
//   - promoted 工具任务：ToolResult.Usage（ToolUsageProvider 回传；普通工具为零值）。
//
// usage 只随事件给宿主/UI（任务卡片展示 tokens + 成本），**不进 LLM 上下文** ——
// task_result 消息正文不携用量，避免用无关的记账信息占用子任务结果模型的注意力。
func (s *Session) PushTaskResultUsage(taskID, name, result, status string, usage core.Usage, err error) error {
	// TaskResultDelivered 事件先发：客户端任务卡片的终态收敛不依赖入队成功 ——
	// Ask 失败（队列满 / 预算超 / 会话关闭）只说明「模型侧收不到结果」
	//（结果仍在 Registry，宿主可 Wait 补取），UI 不能因此永远停在「执行中/中断中」。
	ev := &events.TaskResultDelivered{
		SessionId: s.id,
		TaskId:    taskID,
		Name:      name,
		Status:    status,
		Result:    result,
		Timestamp: time.Now(),
		EventType: events.TaskResultDeliveredType,
	}
	if !usage.IsZero() {
		ev.Usage = &usage
	}
	if err != nil {
		ev.Error = err.Error()
	}
	// 先入队后发事件（顺序即正确性）：WithEventHandler 在 TaskResultDelivered 里
	// wakeRun 唤醒 run loop——若事件先发、消息后入队，runLoop 可能先醒并发现 inbox
	// 为空直接返回（takeBatch 非阻塞），结果从此滞留队列无人再唤醒。入队失败仍发
	// 事件：客户端任务卡终态收敛不依赖入队成功（结果留在 Registry，宿主可 Wait 补取）。
	if perr := s.Ask(s.Context(), core.NewTaskResultMessageWithStatus(taskID, name, result, status, err)); perr != nil {
		s.emit(ev)
		return perr
	}
	s.emit(ev)
	return nil
}

// PromoteTask 把运行中的工具调用/同步子 agent 摘离为后台任务（不中断进度）：
// 主 loop 立即拿到占位结果 {task_id,status:"running"} 并继续干活；原任务不中断，
// 在后台继续跑，完成后结果自动回传主会话（task_result 消息 + TaskResultDelivered 事件），
// 模型下轮续跑。返回 nil = 已摘离；ErrTaskNotFound = 无此运行中任务（批已结束 / 未知 id）。
// 同步子 agent 本质是"返回很慢的工具调用"，promote 全在引擎层处理（决策 #1），
// 无需单独的子 agent promote 机制。
func (s *Session) PromoteTask(taskID string) error {
	s.mu.Lock()
	ac := s.running
	s.mu.Unlock()
	if ac != nil && ac.PromoteToolTask(taskID) {
		return nil
	}
	// 防御：Registry 里的子 agent（agent_spawn 已异步，正常到不了这）
	return fmt.Errorf("%w: %s", subagent.ErrTaskNotFound, taskID)
}

// InterruptTask 按 taskId 精确中断异步任务（右侧任务栏「停止」入口）：
//   - tooltask-*：promoted 长工具任务 → 路由到工具批控制器（runTools 返回后仍保留，
//     见 toolBatches），取消该任务自身执行 ctx——不中断主会话、不影响其他任务；
//   - task-*（其余 id）：异步子 agent → Registry.Interrupt（同一语义）。
//
// 返回 nil = 已受理（任务最终经 task_result_delivered 收敛为 interrupted）；
// ErrTaskNotFound = 未知任务 id；ErrTaskFinished = 已完成/已在中断流程中（重复停止）。
func (s *Session) InterruptTask(taskID string) error {
	if strings.HasPrefix(taskID, "tooltask-") {
		s.mu.Lock()
		ctls := make([]*tools.BatchController, 0, len(s.toolBatches))
		for c := range s.toolBatches {
			ctls = append(ctls, c)
		}
		s.mu.Unlock()
		known := false
		for _, c := range ctls {
			if c.Has(taskID) {
				known = true
			}
			if c.Interrupt(taskID) {
				return nil
			}
		}
		if known {
			return fmt.Errorf("%w: %s", subagent.ErrTaskFinished, taskID)
		}
		return fmt.Errorf("%w: %s", subagent.ErrTaskNotFound, taskID)
	}
	return s.registry.Interrupt(taskID)
}

// registerBatch 工具批控制器注册（RunOptions.BatchCreated 接线）：控制器创建即可被
// tooltask-* 精确中断路由命中（runTools 期间与返回后均有效）。
func (s *Session) registerBatch(b *tools.BatchController) {
	s.mu.Lock()
	s.toolBatches[b] = struct{}{}
	s.mu.Unlock()
}

// unregisterBatch 工具批控制器注销（BatchClosed 接线）：批内全部已摘离任务交付完成
// （无泄漏；零摘离批在 runBatch 返回即注销）。
func (s *Session) unregisterBatch(b *tools.BatchController) {
	s.mu.Lock()
	delete(s.toolBatches, b)
	s.mu.Unlock()
}

// toolTaskSink 摘离工具任务完成回调（RunOptions.TaskSink 接线）：结果推回主会话 +
// usage 累计。与 PushTaskResult 同路径：inbox 入队 → 主 loop 下轮取走 → 模型续跑；
// usage 计入 Session 累计（摘离任务的成本不丢，决策 #7）。push 失败（会话关闭/预算
// 超/队列满）仅记日志 —— 摘离结果已由工具层消费，宿主可经事件/日志补取。
// interrupted = 任务经精确中断终止：终态 interrupted（非 failed/completed），
// 结果/错误文本仍完整保留（前端折叠卡片展开可见）。
//
// Shutdown 语义（文档 §7 #9）：promote 使任务 Detach，脱离 run ctx 的取消链 —— 任务
// 在会话 Shutdown 后仍会跑完（父 ctx 链经 bridge bs.ctx 独立于 sessionCtx），完成后
// push 失败（ErrSessionClosed）仅忽略。行为安全（无损坏/panic，结果丢弃可由宿主补取）；
// 与「摘离任务独立于父运行」（决策 #8）一致。
func (s *Session) toolTaskSink(taskID string, r core.ToolResult, interrupted bool) {
	status := "completed"
	switch {
	case interrupted:
		status = "interrupted" // 精确中断：非失败但未完成
	case r.IsError:
		status = "failed"
	}
	// TaskEnd 先于结果推送（与异步子 agent 路径同序：subagent.runBackground 发 TaskEnd
	// 后 Registry 才回调 OnTaskDone）：promoted 工具任务此前**只有** ToolResponse +
	// task_result_delivered，任务卡片的终态/用量缺一条独立事件源——宿主（含
	// view_reducer 快照与前端卡片）现在按同一事件收敛两路任务。
	// RunId 缺省：工具任务不属某个 Run（promoted 后脱离该 Run 生命周期，见 detach 注释），
	// 归属由 TaskId 表达。
	end := &events.TaskEnd{
		TaskId:    taskID,
		Status:    status,
		Timestamp: time.Now(),
		EventType: events.TaskEndType,
	}
	if r.IsError {
		end.Error = r.Result
	} else {
		end.Result = r.Result
	}
	if !r.Usage.IsZero() {
		u := r.Usage
		end.Usage = &u
	}
	s.emit(end)
	// push 失败（会话关闭/预算超/队列满）仅忽略：摘离结果已由工具层消费，
	// 与 reg.SetOnTaskDone 的推送失败同语义（宿主可经事件/日志补取）
	_ = s.PushTaskResultUsage(taskID, "", r.Result, status, r.Usage, errOf(r))
	s.mu.Lock()
	s.usage = addUsage(s.usage, r.Usage)
	s.mu.Unlock()
}

// errOf 把工具结果翻译为错误：IsError → 结果文本作为错误信息（task_result 走失败文案）。
func errOf(r core.ToolResult) error {
	if r.IsError {
		return errors.New(r.Result)
	}
	return nil
}

// isRunning 当前是否有运行在执行中。
func (s *Session) isRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running != nil
}

// IsRunning 当前是否有运行在执行中（宿主侧只读判断；Interrupt 幂等也可直接调用）。
func (s *Session) IsRunning() bool {
	return s.isRunning()
}

// isClosed 会话是否已关闭（Shutdown 已调用）。
func (s *Session) isClosed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closed
}

// persistHistory 仅追加消息日志（运行中 Ask 使用；积压 Pending 由检查点/Run 后快照负责）。
func (s *Session) persistHistory(ctx context.Context) error {
	if s.store == nil {
		return nil
	}
	return s.appendHistory(ctx)
}

// Run 驱动会话执行：从队列取输入（连续输入聚合成一批）→ 经 AgentLoop 执行 →
// 吸收上下文与成本 → 快照持久化；队列清空后返回。
//
// 单写者保证：store 实现 Locker 时在入口获取会话排他锁、退出释放 ——
// 双实例并发运行同一会话（如崩溃后旧实例未死透）在入口即被拒绝。
func (s *Session) Run(ctx context.Context, handler events.EventHandler) error {
	if s.isClosed() { // 快速路径：已关闭直接拒绝（不取锁）
		return ErrSessionClosed
	}
	if lk, ok := s.store.(Locker); ok {
		release, err := lk.AcquireLock(ctx, s.id)
		if err != nil {
			return err
		}
		defer release()
	}
	// closed 检查与 inRun 置位在同一临界区，与 Shutdown 无竞争：
	// 要么 Run 先拿锁（Shutdown 等待 inRun 收尾后返回），
	// 要么 Shutdown 先置 closed（Run 在此被拒，ErrSessionClosed）。
	// runCancel 也在同一临界区登记：Shutdown 在 OnRunning 回调（s.running 登记）
	// 之前的窗口期内也能取消 Run（V2 P1-RUNTIME-01：防止启动中的 Run 逃过取消）。
	runCtx, runCancel := context.WithCancel(ctx)
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		runCancel()
		return ErrSessionClosed
	}
	s.inRun = true
	s.runCancel = runCancel
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inRun = false
		s.runCancel = nil
		s.mu.Unlock()
		runCancel()
	}()
	for {
		batch, ok := s.takeBatch(runCtx)
		if !ok {
			return nil
		}

		s.mu.Lock()
		s.messages = append(s.messages, batch...)
		s.mu.Unlock()

		// 精确告知「这批用户输入已消费进本轮」：桌面端据此把待发送清单中对应的消息移入对话页
		// （其余仍悬浮等待）——而非靠 agent_start 启发式猜测。UserContents 供恢复重放时
		// 重建图片附件（事件日志是 UI 恢复的唯一数据源）。
		if texts, contents := collectUserContents(batch); len(texts) > 0 || len(contents) > 0 {
			s.emit(&events.UserInputsConsumed{
				SessionId:    s.id,
				UserTexts:    texts,
				UserContents: contents,
				Timestamp:    time.Now(),
				EventType:    events.UserInputsConsumedType,
			})
		}

		if err := s.persist(runCtx, s.messages); err != nil { // Run 前快照：保护已有历史与本批输入
			return err
		}

		s.mu.Lock()
		history := append([]core.Message{}, s.messages...)
		s.mu.Unlock()

		s.mu.Lock()
		s.checkpointed = 0 // 新 Run：重置检查点计数
		mode := s.mode     // 模式快照：整个 Run 期间不变（运行中切换只影响下一次 Run）
		loop := s.loop     // loop 快照（V2 P1-RUNTIME-02）：SetLoop 并发写有锁，读必须同锁；
		// 运行中的 Run 用旧 loop 收尾（SetLoop 注释语义），下一次 Run 用新 loop。
		s.mu.Unlock()

		// typed nil 陷阱：nil 指针赋给接口后接口非 nil（带类型），判空失效 ——
		// 必须显式转换（未启用审批时 Approver 为 nil 接口）
		var approver events.Approver
		if s.approver != nil {
			approver = s.approver
		}
		// 审批按模式包装（Auto 同步放行 / Block 同步拒绝 / Required 转 approvalWaiter；
		// mode 为 nil 时原样返回 —— 现状语义）
		var (
			preToolUse       events.PreToolUseHook
			postToolBatch    events.PostToolBatchHook
			preCompact       events.PreCompactHook
			postToolUse      events.PostToolUseHook
			stopHook         events.StopHook
			sessionStartHook events.SessionStartHook
		)
		if h := s.Hooks(); h != nil {
			// hooks 快照在 Run 入口取一次、整轮用到底（SetHooks 热替换下一次 Run 生效）：
			// 同一轮里七个钩子必须来自同一份配置，否则"PreToolUse 拦、PostToolUse 放"不可解释。
			preToolUse = h.PreToolUse
			postToolBatch = h.PostToolBatch
			preCompact = h.PreCompact
			postToolUse = h.PostToolUse
			stopHook = h.Stop
			sessionStartHook = h.SessionStart
		}
		// SessionStart（hooks 能力扩展）：每次会话一次，在首个 Run 之前派发；结果缓存为
		// 会话级 system 层注入（systemExtraForRun 每次返回同一字符串，前缀缓存不受影响）。
		s.dispatchSessionStartOnce(runCtx, sessionStartHook)
		// O2：包装事件回调——成功压缩时把摘要/analysis/用量/警告写入会话 jsonl 的
		// compaction 留档记录（存储实现 CompactionRecorder 时；尽力而为不影响运行）。
		ac := loop.RunStream(runCtx, history, s.wrapCompactionRecorder(handler), &agents.RunOptions{
			Todo:     s.todo,
			Approver: agents.WrapApprover(approver, mode),
			Mode:     mode,
			Poll:     s.pollInputs,
			// 命令框架：自定义命令执行器（注册表查询，miss 报错 → CommandResult 事件）
			CommandHandler: s.dispatchCommand,
			// E hooks 透传（Session 级配置 → RunOptions）
			PreToolUse:    preToolUse,
			PostToolBatch: postToolBatch,
			PreCompact:    preCompact,
			// hooks 能力扩展：逐工具 PostToolUse / 回合结束 Stop / SessionStart 的系统层注入
			PostToolUse: postToolUse,
			Stop:        stopHook,
			SystemExtra: s.systemExtraForRun,
			// 摘离工具任务完成 → 结果自动回传主会话（toolTaskSink：PushTaskResultStatus + usage 累计）
			TaskSink: s.toolTaskSink,
			// 压缩交接提醒的 [TaskStates] 段（S1-B）：后台任务在途状态渲染源
			TaskState: s.taskStateSnapshot,
			// 工具批控制器生命周期：创建注册（tooltask-* 精确中断路由，runTools 返回后
			// promoted 任务仍可被精确停止）、全部摘离任务完成后注销（防泄漏）
			BatchCreated: s.registerBatch,
			BatchClosed:  s.unregisterBatch,
			OnRunning: func(ac *agents.AgentContext) {
				s.mu.Lock()
				s.running = ac
				s.mu.Unlock()
			},
			// 检查点：每轮产出落盘（长时运行崩溃恢复粒度 = 轮），失败不中断运行
			OnTurn: func(ac *agents.AgentContext) {
				s.checkpoint(runCtx, ac)
			},
		})
		s.mu.Lock()
		s.running = nil
		s.mu.Unlock()
		s.absorb(ac)

		if err := s.persist(runCtx, s.messages); err != nil { // Run 后快照：记录本轮结果
			return err
		}
		if ac.Err != nil && !ac.IsAborted() {
			// 运行异常结束：会话级事件提示（UI 等事件消费者统一感知「这次运行没跑完」）。
			// abort（打断/Shutdown）不是失败，不发此事件。
			s.emit(&events.SessionRunError{
				SessionId: s.id,
				Error:     ac.Err.Error(),
				ErrorKind: provider.ClassifyError(ac.Err),
				Timestamp: time.Now(),
				EventType: events.SessionRunErrorType,
			})
			return ac.Err // 打断（abort）不是失败：继续处理队列中的新输入
		}
	}
}

// Shutdown 优雅关闭会话：幂等、可重复调用。
//
// 语义：
//  1. 置 closed —— 之后 Ask / 新 Run 返回 ErrSessionClosed；队列剩余输入不再消费
//     （积压随最后快照 Pending 保留，下次 NewSession 恢复时重新入队，输入不丢）；
//  2. 正在运行则 Abort 当前轮 —— 走 abort 路径（非失败，AgentEnd.FinishReason=abort，
//     mid-ReAct 无半截上下文），收尾 persist 由 Run 后快照负责（含 Pending drain）；
//  3. 等待 Run 完全收尾（persist 完成后）→ 发 SessionClosed 事件；ctx 超时返回 ctx.Err。
//
// 不主动 persist、不释放写锁（锁归 Run 生命周期，Run 返回时 defer 释放）、
// 不关闭输入队列（关闭会导致并发发送 panic，closed 标志替代）。
// 宿主信号接入：ShutdownOnSignals 一行接入，或 signal.NotifyContext 后自行调用。
func (s *Session) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closed = true
	running := s.running
	runCancel := s.runCancel // 窗口期取消句柄（OnRunning 登记 s.running 之前）
	s.mu.Unlock()

	// Wake every request in the Session-level approval FIFO before aborting the
	// current run. This also releases parallel SubAgents that are waiting for
	// their turn, rather than leaving them behind a closed session.
	if s.approver != nil {
		s.approver.close()
	}
	if running != nil {
		running.Abort()
	} else if runCancel != nil {
		// 窗口期（inRun=true 但 OnRunning 尚未登记）：直接取消 Run 的 ctx——
		// 保证启动中的 Run 也会被 Shutdown 取消（V2 P1-RUNTIME-01）。
		runCancel()
	}
	// 等待 Run 收尾（inRun 含 persist 阶段；未在运行则立即通过）
	for {
		s.mu.Lock()
		inRun := s.inRun
		s.mu.Unlock()
		if !inRun {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Cancel and join asynchronous subagents before declaring the session closed.
	s.sessionCancel()
	if s.registry != nil {
		if err := s.registry.Close(ctx); err != nil {
			return err
		}
	}
	// Session context cancellation is retained for non-registry infrastructure.
	s.emit(&events.SessionClosed{
		SessionId: s.id,
		Timestamp: time.Now(),
		EventType: events.SessionClosedType,
	})
	return nil
}

// History 返回会话完整历史（用户输入 + 每轮产出，含被压缩前的消息，只读视图）。
func (s *Session) History() []core.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.Message{}, s.history...)
}

// Messages 返回当前有效上下文（发给模型的，可能压缩过，只读视图）。
// 命名区别于 AgentContext.Context()（运行根 context.Context）。
func (s *Session) Messages() []core.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]core.Message{}, s.messages...)
}

// ContextBreakdown 估算当前会话「下一次请求」的上下文构成（分区字符/token 估算，
// 宿主 ctx 占用面板数据源）。loop/模式/上下文在同一把锁下取快照：运行中（轮边界会
// 改 ac.Messages）读到的是某一时刻的自洽组合，不会拼出跨轮的混合视图。
// loop 未装配（会话刚建、尚未 SetLoop）→ 空结果，调用方按「无数据」处理。
func (s *Session) ContextBreakdown(ctx context.Context) agents.ContextBreakdown {
	s.mu.Lock()
	loop := s.loop
	mode := s.mode
	messages := append([]core.Message{}, s.messages...)
	s.mu.Unlock()
	if loop == nil {
		return agents.ContextBreakdown{}
	}
	return loop.ContextBreakdown(ctx, messages, mode)
}

// Usage / Cost 返回跨任务的累计值（含后台子任务成本——Registry.TotalUsage 合成，
// 单通道：TaskEnd 事件 + 注册表合计；不持久化，恢复后随 AbandonAll 丢弃）。
func (s *Session) Usage() core.Usage {
	s.mu.Lock()
	defer s.mu.Unlock()
	return addUsage(s.usage, s.registry.TotalUsage())
}

func (s *Session) Cost() core.Cost {
	s.mu.Lock()
	defer s.mu.Unlock()
	return addCost(s.usage.Cost, s.registry.TotalUsage().Cost)
}

// TodoItems 会话待办清单（宿主 UI 展示用，只读；todo_* 工具每次变更后调用）。
func (s *Session) TodoItems() []todo.Item {
	return s.todo.Items()
}

// ID 返回会话 id（Session Mesh 注册表 key / 寻址用；对齐 Context/Registry getter 模式）。
func (s *Session) ID() string {
	return s.id
}

// AITitle 返回 AI 生成的会话标题（空 = 未生成）。
func (s *Session) AITitle() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.aiTitle
}

// SetAITitle 设置 AI 标题：更新内存 + 持久化到会话 jsonl 的 meta 记录（store 实现
// MetaRecorder 时；MemoryStore 等仅内存）。幂等（已有同值不重复写）。
// 供桌面层异步标题生成成功后调用；返回是否发生写入。
func (s *Session) SetAITitle(ctx context.Context, title string) bool {
	title = strings.TrimSpace(title)
	if title == "" {
		return false
	}
	s.mu.Lock()
	if s.aiTitle != "" && s.aiTitle == title {
		s.mu.Unlock()
		return false // 同值幂等
	}
	s.aiTitle = title
	store := s.store
	s.mu.Unlock()
	if store == nil {
		return true
	}
	if mr, ok := store.(MetaRecorder); ok {
		_ = mr.AppendMeta(ctx, s.id, &MetaRecord{Title: title}) // 尽力而为：失败不阻断
	}
	return true
}

// Context 返回 Session 级上下文（跨项基建③）：后台子任务生命周期 ctx
// （subagent.NewBackgroundTool 的 bgCtx 典型提供者）；Shutdown 时级联取消。
// 工具注册先于 NewSession 时用闭包延迟取值（var s *Session; bgCtx: func() { return s.Context() }）。
func (s *Session) Context() context.Context {
	return s.sessionCtx
}

// Registry 返回后台任务注册表（异步多 agent：agent_spawn 工具与 Session 共享）。
func (s *Session) Registry() *subagent.Registry {
	return s.registry
}

// snapshotToolTasks promoted 工具任务枚举（Registry.SetToolTaskProvider 接线）：
// 遍历全部批控制器，合并在途已摘离任务（tooltask-*）。已交付任务经 taskDone 移出
// 控制器 map，天然只含在途；终态与结果已经 task_result_delivered 回传主会话。
func (s *Session) snapshotToolTasks() []subagent.ToolTaskInfo {
	s.mu.Lock()
	ctls := make([]*tools.BatchController, 0, len(s.toolBatches))
	for c := range s.toolBatches {
		ctls = append(ctls, c)
	}
	s.mu.Unlock()
	out := make([]subagent.ToolTaskInfo, 0, len(ctls))
	for _, c := range ctls {
		for _, t := range c.Snapshot() {
			out = append(out, subagent.ToolTaskInfo{ID: t.TaskID, Name: t.Name, Interrupting: t.Interrupting})
		}
	}
	return out
}

// taskStateSnapshot 渲染后台任务状态段（压缩交接提醒的 [TaskStates] 源，S1-B）：
// 枚举注册表全部任务（id/名称/最后已知状态），未见终态的标记 running (awaiting
// result)——续跑模型由此知道哪些任务还挂着，不得假设完成。空 = 无任务（不注入）。
func (s *Session) taskStateSnapshot() string {
	s.mu.Lock()
	reg := s.registry
	s.mu.Unlock()
	if reg == nil {
		return ""
	}
	infos := reg.List()
	if len(infos) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("[TaskStates] (background tasks; no final result = still running — do not assume done, never report their outcomes, and do not declare completion that depends on them)\n")
	for _, ti := range infos {
		label := ti.Name
		if label == "" {
			label = ti.ToolName
		}
		state := string(ti.Status)
		if ti.Status == subagent.TaskRunning || ti.Status == subagent.TaskInterrupting {
			state += " (awaiting result)"
		}
		fmt.Fprintf(&b, "- %s (%s): %s\n", ti.ID, label, state)
	}
	return strings.TrimSuffix(b.String(), "\n")
}

// QuestionWaiter 返回模型→用户提问等待器（ask_user 工具与 Session 共享）。
// 工具注册先于 NewSession 时用闭包延迟取值（var s *Session; w: func() { return s.QuestionWaiter() }）。
func (s *Session) QuestionWaiter() *events.QuestionWaiter {
	return s.questions
}

// AnswerQuestions 注入用户对 ask_user 提问批次的整批回答（HITL 反方向响应入口）：
// batchID 为 AskUserQuestion 事件携带的批次 id，answers 为逐题回答（回传 LLM；
// 缺题视为未回答）。无匹配的待答批次返回 ErrNoPendingQuestion。
func (s *Session) AnswerQuestions(batchID string, answers []events.QuestionAnswer) error {
	s.mu.Lock()
	w := s.questions
	s.mu.Unlock()
	if w == nil {
		return errors.New("session question waiter is not initialized")
	}
	return w.AnswerBatch(batchID, answers)
}

// Approve 注入工具审批决策（HITL 用户响应入口）：
// id 为 ToolCall.Id（审批事件 ToolApprovalRequested 携带的决策锚），
// approve=true 批准执行 / false 拒绝（Block 结果回传 LLM，模型可见后调整）。
// 无匹配的待审批请求返回 ErrNoPendingApproval。
func (s *Session) Approve(id string, approve bool) error {
	s.mu.Lock()
	w := s.approver
	s.mu.Unlock()
	if w == nil {
		return errors.New("session approval is not enabled (WithApprovalTimeout)")
	}
	return w.decide(id, approve)
}

// RequestApproval 运行期注册工具审批请求（沙箱放行等工具执行中的二次决策）。
// 注册先行语义与引擎 preApprove 一致：调用方先注册（拿到 wait）再发
// ToolApprovalRequested 事件，用户经 Approve(id, decision) 注入决策；
// 超时 / ctx 取消 / 会话关闭 = 拒绝（安全默认）。未启用审批 → 错误。
func (s *Session) RequestApproval(ctx context.Context, call core.ToolCall) (func(context.Context) (bool, error), error) {
	s.mu.Lock()
	w := s.approver
	s.mu.Unlock()
	if w == nil {
		return nil, errors.New("session approval is not enabled (WithApprovalTimeout)")
	}
	return w.BeginApprovalWithContext(ctx, call)
}

// PersistErr 返回最近一次快照保存失败的错误（nil 表示正常）。
func (s *Session) PersistErr() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.persistErr
}

// PendingSnapshot 返回当前 inbox 积压的非破坏性快照（不消费队列）。
// 供宿主构造 checkpoint-first 的 sdk_state.pending（崩溃恢复时重新入队，
// 输入不丢）——与 persist 的 drainInputs（破坏性）语义区分：persist 用于
// legacy FileStore 落盘，本方法用于 SessionRecord 的 sdk_state 快照。
func (s *Session) PendingSnapshot() []core.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []core.Message
	for {
		select {
		case m := <-s.inbox:
			out = append(out, m)
			continue
		default:
		}
		break
	}
	// out 是队列内容的副本；按原序放回（FIFO 保持）
	for _, m := range out {
		s.inbox <- m
	}
	return out
}

// Persist 手动触发快照保存。
func (s *Session) Persist(ctx context.Context) error {
	return s.persist(ctx, s.messages)
}

// takeBatch 非阻塞取一批输入：先取一条，再聚合当前全部积压；队列空返回 false。
// 会话已关闭（Shutdown）时不再消费：返回 false 让 Run 循环优雅退出。
func (s *Session) takeBatch(ctx context.Context) ([]core.Message, bool) {
	if s.isClosed() {
		return nil, false
	}
	select {
	case msg := <-s.inbox:
		return append([]core.Message{msg}, s.drainInputs(ctx)...), true
	case <-ctx.Done():
		return nil, false
	default:
		return nil, false
	}
}

// drainInputs 聚合当前积压为一批（AgentLoop 插话检查点回调）。
func (s *Session) drainInputs(context.Context) []core.Message {
	var batch []core.Message
	for {
		select {
		case msg := <-s.inbox:
			batch = append(batch, msg)
			continue
		default:
			return batch
		}
	}
}

// messageTextOf 取出消息里的文本内容（多段以换行连接）；用于把消息交给 hooks 的
// UserPromptSubmit（Prompt 字段）与事件（UserTexts）。
func messageTextOf(m core.Message) string {
	var b strings.Builder
	for _, c := range m.Content {
		if c.Type != "text" || strings.TrimSpace(c.Content) == "" {
			continue
		}
		if b.Len() > 0 {
			b.WriteString("\n")
		}
		b.WriteString(c.Content)
	}
	return b.String()
}

// appendTextToMessage 把 hook 注入串并入消息的文本部分（并入最后一段文本，而不是新增
// 一段 Content —— 保持上游渲染与既有单段文本消息一致；无文本段时追加一段）。
func appendTextToMessage(m core.Message, text string) core.Message {
	if strings.TrimSpace(text) == "" {
		return m
	}
	out := make([]core.Content, len(m.Content))
	copy(out, m.Content)
	for i := len(out) - 1; i >= 0; i-- {
		if out[i].Type == "text" {
			out[i].Content = out[i].Content + "\n\n" + text
			m.Content = out
			return m
		}
	}
	m.Content = append(out, core.Content{Type: "text", Content: text})
	return m
}

// dispatchSessionStartOnce 在首个 Run 之前派发 SessionStart hook（每次会话一次）。
// source：恢复过的会话（有 recoveryState 或已有历史）报 resume，否则 startup。
// 注入文本缓存进 s.systemExtra，由 systemExtraForRun 提供给 RunOptions.SystemExtra。
func (s *Session) dispatchSessionStartOnce(ctx context.Context, h events.SessionStartHook) {
	if h == nil {
		return
	}
	s.mu.Lock()
	if s.hooksStarted {
		s.mu.Unlock()
		return
	}
	s.hooksStarted = true
	source := "startup"
	if s.cfg.recoveryState != nil || len(s.messages) > 0 {
		source = "resume"
	}
	s.hooksSource = source
	cwd := s.cfg.HookCwd
	s.mu.Unlock()

	inject, panicked := agents.CallSessionStart(h, ctx, events.SessionStartContext{
		SessionId: s.id,
		Cwd:       cwd,
		Source:    source,
	})
	if panicked {
		s.emitHookWarning("SessionStart", "hook panicked, injection skipped")
		return
	}
	if strings.TrimSpace(inject) == "" {
		return
	}
	s.mu.Lock()
	s.systemExtra = inject
	s.mu.Unlock()
}

// systemExtraForRun 供 RunOptions.SystemExtra：返回会话级 system 层注入（空 = 不注入）。
// 每次返回同一字符串 —— SystemExtra 在会话内稳定，provider 前缀缓存因此不被击穿。
func (s *Session) systemExtraForRun() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.systemExtra
}

// emitHookWarning 发 ToolHookWarning（hook 异常统一出口；Session 侧无 RunId 时留空）。
func (s *Session) emitHookWarning(name, message string) {
	s.emit(&events.ToolHookWarning{
		Name:      name,
		Message:   message,
		Timestamp: time.Now(),
		EventType: events.ToolHookWarningType,
	})
}

// collectUserContents 取一批消息里的用户内容（按顺序）：UserTexts = 文本块拼接（向后兼容），
// UserContents = 每条消息的完整内容块（text + image，含 mime_type/data URL）——
// 供 UserInputsConsumed 事件：UI 实时渲染与**事件日志恢复**（快照重放）共用同一数据源，
// 否则图片附件只在客户端暂存队列里（重启即失，恢复后图片消失）。
// 判定"用户输入" = 至少含一个 text/image 块；task_result / summary / command 等内部块不计入
// （与旧 collectUserTexts 语义一致：后台任务结果不会触发 UserInputsConsumed）。
func collectUserContents(msgs []core.Message) (texts []string, contents [][]core.Content) {
	for _, m := range msgs {
		if m.Role != core.User {
			continue
		}
		var t []string
		isInput := false
		for _, c := range m.Content {
			switch c.Type {
			case core.ContentTypeText:
				t = append(t, c.Content)
				isInput = true
			case core.ContentTypeImage:
				isInput = true
			}
		}
		if !isInput {
			continue
		}
		texts = append(texts, t...)
		contents = append(contents, m.Content)
	}
	return texts, contents
}

// pollInputs AgentLoop 插话检查点回调：drain 当前积压 + 发 UserInputsConsumed（前端感知
// 插话消费，与 takeBatch 路径一致），再返回给循环插入下一轮。
func (s *Session) pollInputs(ctx context.Context) []core.Message {
	batch := s.drainInputs(ctx)
	if texts, contents := collectUserContents(batch); len(texts) > 0 || len(contents) > 0 {
		s.emit(&events.UserInputsConsumed{
			SessionId:    s.id,
			UserTexts:    texts,
			UserContents: contents,
			Timestamp:    time.Now(),
			EventType:    events.UserInputsConsumedType,
		})
	}
	return batch
}

// checkpoint 每轮检查点：本轮产出并入完整历史并落盘（长时运行崩溃恢复的粒度 = 轮）。
// 失败仅记录 persistErr（下轮检查点重试），不中断运行。
func (s *Session) checkpoint(ctx context.Context, ac *agents.AgentContext) {
	s.mu.Lock()
	if s.checkpointed < len(ac.RunHistory) {
		s.history = append(s.history, ac.RunHistory[s.checkpointed:]...)
		s.checkpointed = len(ac.RunHistory)
	}
	messages := append([]core.Message{}, ac.Messages...) // 实时上下文（含本轮产出/插话输入）
	// 运行中快照（V2 P1-PERSIST-01）：record 提交的 sdkState 读它而非 s.Messages()——
	// absorb 之前 Session 状态仍是旧值，崩溃恢复时 record 的 SDK 状态会落后于
	// UI（reducer 已 Apply 最新事件）。语义边界统一：checkpoint 时刻的 ac.Messages。
	s.runningSnapshot = append([]core.Message{}, messages...)
	s.mu.Unlock()
	_ = s.persist(ctx, messages)
}

// RunningSnapshot 返回最近一次运行中 checkpoint 的上下文快照（record 提交用）。
// absorb 之后返回 nil（Session 已吸收，sdkState 直接读 s.Messages()）。
func (s *Session) RunningSnapshot() []core.Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.runningSnapshot == nil {
		return nil
	}
	return append([]core.Message{}, s.runningSnapshot...)
}

// absorb 吸收一次运行的结果：整段替换有效上下文，本轮产出并入完整历史（检查点
// 已并入的部分跳过，防重复），累计用量与成本。
func (s *Session) absorb(ac *agents.AgentContext) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.runningSnapshot = nil // 已吸收：record 提交改读 s.Messages()（权威）
	s.messages = ac.Messages
	if s.checkpointed < len(ac.RunHistory) {
		s.history = append(s.history, ac.RunHistory[s.checkpointed:]...)
	}
	s.checkpointed = 0
	s.usage = addUsage(s.usage, ac.Usage) // Cost 内嵌于 Usage，随 addUsage 一并累计
}

// persist 持久化：增量消息日志（AppendHistory）+ 状态行（SaveState）。
// 顺序保证 History 先于 State（完整历史是最重要的资产）；积压随状态保存，
// Save 失败时积压塞回队列继续待消费（队列满则丢弃并记录 persistErr）。
// messages 为快照的上下文来源：运行期间传 ac.Messages（实时），其余场景传 s.messages。
func (s *Session) persist(ctx context.Context, messages []core.Message) error {
	if s.store == nil {
		return nil
	}

	pending := s.drainInputs(ctx)
	s.mu.Lock()
	state := &State{
		// 指令消息不持久化（命令框架）：Messages 与 Pending 均剔除 ——
		// 崩溃恢复后不重放执行（恢复丢指令可接受，文档写明）
		Messages:  filterCommandMessages(messages),
		Usage:     s.usage,
		Cost:      s.usage.Cost,
		Pending:   filterCommandMessages(pending),
		Todo:      s.todo.Items(),
		UpdatedAt: time.Now(),
	}
	s.mu.Unlock()

	err := s.appendHistory(ctx)
	if err == nil {
		err = s.store.SaveState(ctx, s.id, state)
	}

	// 回塞 pending（V2 P1-PERSIST-02）：队列满时**阻塞等待空位**（背压）而非静默丢弃
	// ——pending 是新输入，丢失即用户消息消失。inbox 有 64 缓冲，正常不会满；
	// 满说明消费端停滞，阻塞回塞比丢消息安全（ctx 感知，取消则放弃并计错误）。
	for _, m := range pending {
		select {
		case s.inbox <- m:
		case <-ctx.Done():
			s.recordPersistErr(fmt.Errorf("persist requeue: %w", ctx.Err()))
			return ctx.Err()
		}
	}

	if err != nil {
		s.recordPersistErr(err)
	}
	return err
}

// appendHistory 增量追加消息日志（成功后推进 persistedCount）。
func (s *Session) appendHistory(ctx context.Context) error {
	if s.store == nil {
		return nil
	}
	s.mu.Lock()
	newMsgs := append([]core.Message{}, s.history[s.persistedCount:]...)
	s.mu.Unlock()
	if len(newMsgs) == 0 {
		return nil
	}
	if err := s.store.AppendHistory(ctx, s.id, newMsgs); err != nil {
		return err
	}
	s.mu.Lock()
	s.persistedCount = len(s.history)
	s.mu.Unlock()
	return nil
}

// recordPersistErr 记录持久化错误并发 Session 级事件。
func (s *Session) recordPersistErr(err error) {
	s.mu.Lock()
	s.persistErr = err
	s.mu.Unlock()
	s.emit(&events.SessionPersistError{
		SessionId: s.id,
		Error:     err.Error(),
		EventType: events.SessionPersistErrorType,
	})
}

// wrapCompactionRecorder 包装事件回调（O2/G7）：收到成功压缩（CompressEnd 且无
// Error）时把摘要/analysis/用量/警告写入会话 jsonl 的 compaction 留档记录——仅当
// 存储实现 CompactionRecorder（FileStore）时生效；留档失败不影响运行（尽力而为）。
// 失败降级（Error 非空）不写：无产物可留档。
func (s *Session) wrapCompactionRecorder(next events.EventHandler) events.EventHandler {
	return func(ctx context.Context, e events.Event) {
		if ce, ok := e.(*events.CompressEnd); ok && ce.Error == "" && s.store != nil {
			if rc, ok := s.store.(CompactionRecorder); ok {
				rec := &CompactionRecord{
					Summary:   ce.Summary,
					Analysis:  ce.Analysis,
					Model:     ce.Model,
					Warnings:  ce.Warnings,
					Timestamp: time.Now(),
				}
				if ce.Usage != nil {
					u := *ce.Usage
					rec.Usage = &u
				}
				_ = rc.AppendCompaction(ctx, s.id, rec)
			}
		}
		if next != nil { // 与 agents.RunStream 同语义：handler 可为 nil（测试/无 UI 场景）
			next(ctx, e)
		}
	}
}

// emit 发出 Session 级事件（经 WithEventHandler 注入的回调）。
func (s *Session) emit(e events.Event) {
	if s.cfg.EventHandler != nil {
		s.cfg.EventHandler(context.Background(), e)
	}
}

func (s *Session) budgetExceeded() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg.MaxCost.Total > 0 && s.usage.Cost.Total >= s.cfg.MaxCost.Total
}

// addUsage / addCost 委托 core（累加口径的唯一实现在 core.Usage.Add / core.Cost.Add，
// 字段新增时不会多处漂移）。语义完全一致：逐段相加，口径标记不参与求和。
func addUsage(a, b core.Usage) core.Usage { return a.Add(b) }

func addCost(a, b core.Cost) core.Cost { return a.Add(b) }
