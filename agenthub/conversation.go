package agenthub

// conversation.go：群内自动对话的控制器（P3b-04，方案 §6.5.6「对话控制器」）。
//
// 这一层是**纯内存状态机**：不做 I/O、不认识会话/模型/存储。分工是：
//   - bridge 在每条人类消息上 BeginHuman + StartHumanRound（开新对话：清队列、清计数，
//     首轮目标仍然**并行**唤醒，不进队列）；
//   - 智能体回复（group_say / final）里的 @ 目标经 Offer 入队：一次 Offer 要么全部
//     入队、要么一条都不入队（护栏命中返回 ErrConversationExhausted + 人话原因）；
//   - bridge 用 NextDue 取「现在可以跑的那一个」，投递成功后 BeginTurn 记账；
//     投递失败（目标忙）就什么都不做 —— wake 还在队里，下一次群 run 结束再取；
//   - 预算/深度/踢皮球/同内容由本层统一判定，命中即 Status=exhausted。
//
// 为什么串行队列在 Hub 层：群会话的忙闩（bridge 的 hubGroupBusy）只保证「同一个
// 智能体不并发跑两轮」，管不住 A→B→C 的链式唤醒。这里给每个群一条**串行**队列，
// 一次只放一个 wake 出去，前一个跑完（AgentEnd）再放下一个。
//
// 人类优先：人类消息不走队列，且 BeginHuman 会清空队列与计数 —— 人一句话就打断
// 自动接龙，不需要等预算耗尽。人类发送也不计入 PairCount（人不是「互踢」的一方）。

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode"
)

// ErrConversationExhausted 对话护栏命中（轮次预算/深度/踢皮球/同内容循环）。
// errors.Is(err, ErrConversationExhausted) 为真；具体原因见 *ExhaustedError.Reason。
var ErrConversationExhausted = errors.New("agenthub: conversation exhausted")

// ExhaustedError 带人话原因（直接写进群系统消息）与命中瞬间的状态。
type ExhaustedError struct {
	GroupID GroupID
	Reason  string
	Status  ConversationStatus
}

func (e *ExhaustedError) Error() string {
	if e == nil {
		return ErrConversationExhausted.Error()
	}
	return fmt.Sprintf("agenthub: 自动对话已停止（群 %s）：%s", e.GroupID, e.Reason)
}

// Unwrap 让 errors.Is(err, ErrConversationExhausted) 成立。
func (e *ExhaustedError) Unwrap() error { return ErrConversationExhausted }

// UnlimitedTurnBudget 表示「生产默认不设自动接龙轮次上限」。
// 深度/同对/冷却/同内容指纹等护栏照常生效，只是不再按轮数拦截。
const UnlimitedTurnBudget = -1

// 对话默认限额（方案 §6.5.6 规则 4/7；D13 = 8 轮，仅作为**显式关闭上限**时的兼容默认）。
const (
	DefaultConversationTurnBudget = 8
	// DefaultConversationMaxDepth 是**每条链**的默认深度上限（D3/P6-05；生产值由
	// settings.json hub.conversation.max_depth 覆盖，默认 1000）。
	DefaultConversationMaxDepth = 1000
	// DefaultConversationPairLimit 是同一对（无序）在窗口内的成功投递上限（D2）。
	DefaultConversationPairLimit = 10
	// DefaultConversationPairWindow 是 pair 计数的滚动窗口（§1.1）：只统计最近 10
	// 分钟内的成功投递，窗口滑过自动恢复，不需要人类操作。
	DefaultConversationPairWindow = 10 * time.Minute
	// DefaultConversationIdleReset 群连续这么久没有 run/wake → 回 idle 并清零计数。
	DefaultConversationIdleReset = 30 * time.Minute
	// DefaultConversationRunTimeout 单轮 run 的最长时长（P6-08 执行中断；这里透传到视图）。
	DefaultConversationRunTimeout = 10 * time.Minute
	// DefaultConversationMaxPendingWakes 队列上限（兜底护栏，§1.2）。
	DefaultConversationMaxPendingWakes = 200
	// DefaultConversationCooldown 默认**无冷却**（D8：cooldown_ms 默认 0）。
	// 冷却只作用于「连续两次自动唤醒之间」，人类消息永远立即生效。
	DefaultConversationCooldown = time.Duration(0)
	// ConversationFingerprintRunes 指纹取前 200 个 rune（§6.5.6 规则 8）。
	ConversationFingerprintRunes = 200
)

// ConversationStatus 对话状态机（方案 §6.5.6 的 Conversation.Status）。
type ConversationStatus string

const (
	// ConversationIdle 该群还没有对话（没人发过消息、也没被 @ 过）。
	ConversationIdle ConversationStatus = "idle"
	// ConversationRunning 对话活着：可能有排队中的 wake，也可能在等下一句。
	ConversationRunning ConversationStatus = "running"
	// ConversationWaitingHuman 队列已排空、没有在跑的轮次（UI 用「等你继续」；
	// 控制器内部不主动设：排空不是终态，Offer 照常可用，状态由 bridge 的视图推导）。
	ConversationWaitingHuman ConversationStatus = "waiting_human"
	// ConversationStopped 人类停止（幂等；再来人类消息会开新对话）。
	ConversationStopped ConversationStatus = "stopped"
	// ConversationExhausted 护栏命中（预算/深度/踢皮球/同内容）。
	ConversationExhausted ConversationStatus = "exhausted"
)

// Terminal 终态：不再接新的 wake，bridge 据此只写一条系统消息。
func (s ConversationStatus) Terminal() bool {
	return s == ConversationStopped || s == ConversationExhausted
}

// ConversationLimits 可注入的限额（零值取默认）。
//
// TurnBudget<0 表示不设自动接龙上限（生产用 UnlimitedTurnBudget=-1）；
// 正好 0（未指定）时补成 DefaultConversationTurnBudget，兼容旧调用方。
type ConversationLimits struct {
	TurnBudget int
	MaxDepth   int
	PairLimit  int
	// PairWindow 无序 pair 计数的滚动窗口（§1.1）。<0 = 关闭过期（测试用）。
	PairWindow time.Duration
	// IdleReset 空闲多久回 idle 并清零。<0 = 关闭（测试用）。
	IdleReset time.Duration
	// RunTimeout 单轮 run 上限（P6-08 用；本层只透传到视图供 UI 显示）。
	RunTimeout time.Duration
	// MaxPendingWakes 队列上限；<=0 = 默认 200。
	MaxPendingWakes int
	Cooldown        time.Duration
}

// withDefaults 把零值补成默认值。**Cooldown<0 表示关闭冷却**（测试用）：
// 「首轮立即唤醒、后续轮之间强制间隔」这条语义需要一个显式的关闭开关。
//
// TurnBudget 只在 ==0 时补默认：负值（UnlimitedTurnBudget）必须原样保留，
// 否则生产注入的「不设上限」会被悄悄改回 8 轮。
// Effective 返回补全默认值后的限额（bridge 读 settings 后要能自证「生效值是多少」，
// 而不是把零值透传出去让调用方猜）。
func (l ConversationLimits) Effective() ConversationLimits { return l.withDefaults() }

func (l ConversationLimits) withDefaults() ConversationLimits {
	if l.TurnBudget == 0 {
		l.TurnBudget = DefaultConversationTurnBudget
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = DefaultConversationMaxDepth
	}
	if l.PairLimit <= 0 {
		l.PairLimit = DefaultConversationPairLimit
	}
	if l.PairWindow == 0 {
		l.PairWindow = DefaultConversationPairWindow
	}
	if l.IdleReset == 0 {
		l.IdleReset = DefaultConversationIdleReset
	}
	if l.RunTimeout == 0 {
		l.RunTimeout = DefaultConversationRunTimeout
	}
	if l.MaxPendingWakes <= 0 {
		l.MaxPendingWakes = DefaultConversationMaxPendingWakes
	}
	// Cooldown：0 = 无冷却（默认），<0 同义（旧测试用 -1 表示关闭）。
	if l.Cooldown < 0 {
		l.Cooldown = 0
	}
	return l
}

// Wake 是一次待跑的群唤醒：谁 @ 了谁、第几跳、带哪条触发消息。
// Turn 在出队（BeginTurn）时才最终确定；NextDue/Peek 给出的是「如果现在跑，会是第几轮」。
type Wake struct {
	GroupID GroupID `json:"group_id"`
	// ID 是投递幂等键（P6-01）：Offer 时分配一次，之后重试/重泵都复用它。
	// 空串 = 未登记（直接构造的 wake，投递时按「首次」处理并补登记）。
	ID string `json:"id,omitempty"`
	// ConversationID 是入队时所处对话的 id：投递方（bridge）在真正投递前用它
	// 确认「这条 wake 还属于当前对话」，停止/人类插话开新对话后旧 wake 立即失效。
	ConversationID string  `json:"conversation_id,omitempty"`
	Sender         AgentID `json:"sender,omitempty"`
	Human          bool    `json:"human,omitempty"` // 人类发送：不计 PairCount/不算循环
	Target         AgentID `json:"target"`
	Text           string  `json:"text,omitempty"`
	// Attachments 是触发消息的附件（人类群消息的图片）。群唤醒要把它拼进模型
	// 上下文（见 bridge wakeGroupAgent）；智能体之间的 @ 唤醒留空。
	Attachments []Attachment `json:"attachments,omitempty"`
	Depth       int          `json:"depth"`
	Turn        int          `json:"turn"`
	TriggerSeq  int64        `json:"trigger_seq,omitempty"`
	RoundID     string       `json:"round_id,omitempty"`
	Fingerprint string       `json:"fingerprint,omitempty"`
	EnqueuedAt  time.Time    `json:"enqueued_at"`
	// ReadyAt 是可跑时刻：max(入队时间, 上一次唤醒 + 冷却)。冷却只作用于
	// **连续两次智能体唤醒之间**，所以新对话的第一个 wake 永远是「现在就绪」。
	ReadyAt time.Time `json:"ready_at"`
}

// ConversationOffer 是一次 Offer 请求（一条智能体/人类消息上的全部 @ 目标）。
type ConversationOffer struct {
	GroupID GroupID
	Sender  AgentID
	Human   bool // true = 人类发送（Sender 为空）：不计 PairCount、不算指纹循环
	Targets []AgentID
	Text    string // 触发这条唤醒的消息正文（指纹来源）
	// Attachments 是触发消息的附件（人类群消息的图片），随 wake 一起入队：
	// 群会话要在 hidden 快照之后把图片拼进模型上下文。
	Attachments []Attachment
	// ConversationID 是**发起者那一轮所属**的对话 id（来自它自己的 wake header）。
	// 非空且与当前对话不同 = 旧 generation 的在飞 run 回来 @ —— 不得向新对话路由
	//（§1.1/§1.5）。空 = 不校验（人类消息/内部调用）。
	ConversationID string
	Depth          int // 这批 wake 的跳数（人类首轮 = 1）
	TriggerSeq     int64
	RoundID        string
	At             time.Time
}

// ConversationView 是给事件/UI/命令回执的快照（JSON 直接可读）。
// TurnBudget<0 = 不设自动接龙上限（生产默认 UnlimitedTurnBudget）；UI 据此显示
// 「第 N 轮」而不是「第 N/8 轮」。
type ConversationView struct {
	ID         string             `json:"id,omitempty"`
	GroupID    GroupID            `json:"group_id"`
	Status     ConversationStatus `json:"status"`
	HumanSeq   int64              `json:"human_seq,omitempty"`
	TurnsUsed  int                `json:"turns_used"`
	TurnBudget int                `json:"turn_budget"`
	Depth      int                `json:"depth"`
	MaxDepth   int                `json:"max_depth"`
	QueueLen   int                `json:"queue_len"`
	PairLimit  int                `json:"pair_limit"`
	// PairCount 键是 ConversationPairKey(from,to)；JSON 对象的键必须是字符串，
	// 所以不用 [2]string（encoding/json 不支持数组键）。
	PairCount  map[string]int `json:"pair_count,omitempty"`
	CooldownMS int64          `json:"cooldown_ms"`
	// PairWindowMS 是 pair 计数的滚动窗口（§1.1）。PairResetAt 是最热那一对的
	// 最早过期时刻 —— UI 用它显示「约 N 分钟后自动恢复」。
	PairWindowMS int64      `json:"pair_window_ms,omitempty"`
	PairResetAt  *time.Time `json:"pair_reset_at,omitempty"`
	// IdleResetMS / RunTimeoutMS / MaxPendingWakes 是全局设置的回显（UI 显示与排障）。
	IdleResetMS     int64      `json:"idle_reset_ms,omitempty"`
	RunTimeoutMS    int64      `json:"run_timeout_ms,omitempty"`
	MaxPendingWakes int        `json:"max_pending_wakes,omitempty"`
	LastWakeAt      *time.Time `json:"last_wake_at,omitempty"`
	StopReason      string     `json:"stop_reason,omitempty"`
	StartedAt       *time.Time `json:"started_at,omitempty"`
	UpdatedAt       *time.Time `json:"updated_at,omitempty"`
	// Deliveries 是最近一轮的投递账本（P6-01）：每个 target 的
	// queued/running/done/skipped/failed + 原因。有界（见 ConversationViewDeliveries），
	// 只给 UI/命令回执看，不参与任何判定。
	Deliveries []Delivery `json:"deliveries,omitempty"`
}

// ConversationViewDeliveries 是 ConversationView 里携带的投递记录条数上限：
// hub_conversation 事件是高频事件，整本账（64 条）塞进去会把事件行撑到十几 KB。
// 进行中的记录一条不丢，终态的只保留最近的几条。
const ConversationViewDeliveries = 20

// Terminal 终态（stopped/exhausted）；idle/running 都不算。
func (v ConversationView) Terminal() bool { return v.Status.Terminal() }

// ConversationPairKey 是 PairCount 的键：**无序**（D2/§1.1）—— A↔B 与 B↔A 是同一个
// 键，否则「A 先 @B 十次」和「B 先 @A 十次」会被当成两对，踢皮球照样洗白。
// 用 ↔ 而不是 → 是刻意的：UI 与日志里一眼能看出这是对称关系。
func ConversationPairKey(from, to AgentID) string {
	a, b := string(from), string(to)
	if b < a {
		a, b = b, a
	}
	return a + "↔" + b
}

// ConversationOptions 构造参数：Limits 零值取默认，Now 为空用 time.Now。
type ConversationOptions struct {
	Limits ConversationLimits
	Now    func() time.Time
}

// agentPair 是 pair 计数的键：**无序**（D2/§1.1）。用 newAgentPair 构造，直接写
// struct literal 会让 A→B 与 B→A 变成两个键（踢皮球洗白）。
type agentPair struct{ From, To AgentID }

// newAgentPair 规范化成无序键（字典序小的一方在 From）。
func newAgentPair(a, b AgentID) agentPair {
	if string(b) < string(a) {
		a, b = b, a
	}
	return agentPair{From: a, To: b}
}

type conversation struct {
	id        string
	group     GroupID
	humanSeq  int64
	status    ConversationStatus
	stop      string
	turns     int
	depth     int
	queue     []Wake
	pairs     map[agentPair][]time.Time
	prints    map[agentPair]map[string]bool
	lastWake  time.Time
	startedAt time.Time
	updatedAt time.Time
	// deliveries 是投递账本（P6-01）：每 target 一条，跨 generation 保留（有界）。
	deliveries *deliveryLedger
	// wakeSeq 是 wake id 的单调序号（同毫秒内不撞车）。
	wakeSeq uint64
}

// ConversationController 每群一份对话状态。并发安全：所有方法持同一把锁，
// 临界区只碰内存（没有任何 I/O），所以可以放心在 emit/工具 goroutine 里调用。
type ConversationController struct {
	mu     sync.Mutex
	limits ConversationLimits
	now    func() time.Time
	convs  map[GroupID]*conversation
	seq    uint64
}

// NewConversationController 构造控制器；opts 零值 = 全部默认。
func NewConversationController(opts ConversationOptions) *ConversationController {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	return &ConversationController{
		limits: opts.Limits.withDefaults(),
		now:    now,
		convs:  map[GroupID]*conversation{},
	}
}

// Limits 返回生效的限额（含默认值），供 bridge/UI 展示。
func (c *ConversationController) Limits() ConversationLimits {
	if c == nil {
		return ConversationLimits{}.withDefaults()
	}
	return c.limits
}

func (c *ConversationController) clockAt(at time.Time) time.Time {
	if !at.IsZero() {
		return at
	}
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

func (c *ConversationController) ensureLocked(group GroupID, now time.Time) *conversation {
	if c.convs == nil {
		c.convs = map[GroupID]*conversation{}
	}
	conv := c.convs[group]
	if conv == nil {
		conv = &conversation{
			group:      group,
			status:     ConversationIdle,
			pairs:      map[agentPair][]time.Time{},
			prints:     map[agentPair]map[string]bool{},
			startedAt:  now,
			updatedAt:  now,
			deliveries: newDeliveryLedger(),
		}
		c.convs[group] = conv
	}
	return conv
}

func (c *ConversationController) nextIDLocked(now time.Time) string {
	c.seq++
	return fmt.Sprintf("conv_%d_%d", now.UnixMilli(), c.seq)
}

// nextWakeIDLocked 分配投递幂等键（P6-01）。同毫秒内靠 wakeSeq 递增不撞车；
// 带群 id 便于日志/UI 直接看出归属。
func (c *ConversationController) nextWakeIDLocked(conv *conversation, now time.Time) string {
	conv.wakeSeq++
	return fmt.Sprintf("wake_%s_%d_%d", conv.group, now.UnixMilli(), conv.wakeSeq)
}

// BeginHuman 人类消息开启新对话：TurnsUsed=0、Depth=0、队列清空、踢皮球计数与
// 指纹清空、Status=running。首轮目标由 bridge 并行唤醒（不走队列）。
func (c *ConversationController) BeginHuman(group GroupID, humanSeq int64, at time.Time) ConversationView {
	if c == nil {
		return ConversationView{GroupID: group, Status: ConversationIdle}
	}
	now := c.clockAt(at)
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.ensureLocked(group, now)
	conv.id = c.nextIDLocked(now)
	conv.humanSeq = humanSeq
	conv.status = ConversationRunning
	conv.stop = ""
	conv.turns = 0
	conv.depth = 0
	conv.queue = nil
	conv.pairs = map[agentPair][]time.Time{}
	conv.prints = map[agentPair]map[string]bool{}
	conv.lastWake = time.Time{}
	conv.startedAt = now
	conv.updatedAt = now
	// 人类开新对话：旧 generation 还在排队的 wake 全部作废（D5）。账本里只丢
	// 这些 queued 记录；running/终态保留，在飞 run 收尾时还能正常落到 done。
	conv.deliveries.dropQueued()
	return conv.view(c.limits, now)
}

// StartHumanRound 记一次人类触发的首轮：n 个目标**并行**唤醒，占 1 个回合预算
// （方案 §6.5.6 规则 2「每个回复带 turn=1」）。n<=0 不占预算，返回 0。
//
// 只被人类发送路径调用；智能体之间的唤醒走 Offer（那是串行队列）。
func (c *ConversationController) StartHumanRound(group GroupID, targets int, at time.Time) int {
	if c == nil || targets <= 0 {
		return 0
	}
	now := c.clockAt(at)
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.ensureLocked(group, now)
	if conv.status.Terminal() {
		return 0
	}
	if conv.status == ConversationIdle {
		conv.status = ConversationRunning
		conv.id = c.nextIDLocked(now)
		conv.startedAt = now
	}
	conv.turns++
	conv.depth = 1
	conv.updatedAt = now
	return conv.turns
}

// Offer 把一条消息上的 @ 目标入队（每个目标一条 Wake）。
//
// 护栏（命中即 exhausted + ErrConversationExhausted，**一条都不入队**）：
//   - 轮次预算：TurnBudget>0 且 TurnsUsed >= TurnBudget（生产默认不限轮次）；
//   - 深度：这批 wake 的 Depth > MaxDepth（人类首轮=1，之后每 @ 一跳 +1）；
//   - 踢皮球：同一对 agent 的 A→B 次数已达 PairLimit（人类发送不计）；
//   - 同内容循环：同一对 agent 重复同一段归一化指纹（人类发送不计）。
//
// 自我 @（target == sender）直接跳过（§6.5.6 规则 10），不报错。
// 对话已被人类停止时静默丢弃（返回 nil, nil）：那是人的意图，不是错误。
func (c *ConversationController) Offer(req ConversationOffer) ([]Wake, error) {
	if c == nil {
		return nil, nil
	}
	now := c.clockAt(req.At)
	group := req.GroupID
	text := strings.TrimSpace(req.Text)
	targets := dedupeTargets(req.Targets, req.Sender)

	c.mu.Lock()
	defer c.mu.Unlock()
	// 没有目标 = 这条消息不产生任何唤醒：**不能**顺手建对话、换 id 或改状态
	// （一条无 @ 的 group_say 不该让 /hub/conversation 多出一段「新对话」）。
	if len(targets) == 0 {
		return nil, nil
	}
	conv := c.ensureLocked(group, now)
	if conv.status == ConversationStopped {
		return nil, nil
	}
	if conv.status == ConversationExhausted {
		return nil, &ExhaustedError{GroupID: group, Reason: conv.stop, Status: conv.status}
	}
	if conv.status == ConversationIdle {
		// 没有人类消息就被 @（例如 assign_task 直接唤醒）：隐式开一条对话，
		// 否则预算/踢皮球对这些轮次完全失效。
		conv.id = c.nextIDLocked(now)
		conv.status = ConversationRunning
		conv.startedAt = now
	}
	limits := c.limits
	c.settleLocked(conv, now)
	// 换代路由（§1.1/§1.5）：旧 generation 的在飞 run 回来 @ —— 丢弃，不进入新对话。
	// 它的 pair 计数留在旧代（这里什么都不加），新代额度一条都不消耗。
	if req.ConversationID != "" && conv.id != "" && req.ConversationID != conv.id {
		skipped := 0
		for _, target := range targets {
			w := Wake{GroupID: group, ConversationID: conv.id, Sender: req.Sender, Human: req.Human,
				Target: target, Text: text, Depth: req.Depth, TriggerSeq: req.TriggerSeq, RoundID: req.RoundID}
			w.ID = c.nextWakeIDLocked(conv, now)
			conv.deliveries.put(&Delivery{
				WakeID: w.ID, ConversationID: conv.id, GroupID: group, Target: target,
				Sender: req.Sender, Human: req.Human, Depth: req.Depth, TriggerSeq: req.TriggerSeq,
				RoundID: req.RoundID, Status: DeliverySkipped, Reason: DeliveryReasonStale,
				Note: "上一轮对话已结束，这条 @ 不再路由", EnqueuedAt: now, UpdatedAt: now,
			})
			skipped++
		}
		conv.deliveries.prune()
		return nil, nil
	}
	// 人类优先：预算与深度是「自动接龙」的护栏，人类发送不受它们约束（§6.5.6
	// 规则 1/5）。
	// 轮次上限只在 TurnBudget>0 时生效：生产默认 UnlimitedTurnBudget（不设上限）。
	if !req.Human && limits.TurnBudget > 0 && conv.turns >= limits.TurnBudget {
		return nil, c.exhaustLocked(conv, now,
			fmt.Sprintf("本轮自动对话已达上限 %d 轮，等待人类继续", limits.TurnBudget))
	}
	depth := req.Depth
	if req.Human {
		depth = 1 // 人类唤醒的永远是第一跳（人类自身深度 0）
	} else if depth <= 0 {
		depth = conv.depth + 1
	}
	fp := ""
	if !req.Human {
		fp = NormalizeFingerprint(text)
	}
	// 逐目标判定（§1.1 到限行为）：**只跳过命中的那个 target**，其他目标继续投递；
	// 全部命中才把对话推到 waiting_human（不是 exhausted —— 人类一句话就能继续）。
	type plan struct {
		target AgentID
		pair   agentPair
		skip   string // 非空 = 这条不入队，账本记 skipped
		note   string
	}
	plans := make([]plan, 0, len(targets))
	for _, target := range targets {
		p := newAgentPair(req.Sender, target)
		if req.Human {
			plans = append(plans, plan{target: target, pair: p})
			continue
		}
		if depth > limits.MaxDepth {
			plans = append(plans, plan{target: target, pair: p, skip: DeliveryReasonDepthLimit,
				note: fmt.Sprintf("对话深度已达上限 %d 跳（A→B→C 链）", limits.MaxDepth)})
			continue
		}
		if got := c.pairCountLocked(conv, p, now); got >= limits.PairLimit {
			plans = append(plans, plan{target: target, pair: p, skip: DeliveryReasonPairLimit,
				note: fmt.Sprintf("%s 与 %s 在最近 %s 内已互踢 %d 次（上限 %d）",
					req.Sender, target, limits.PairWindow, got, limits.PairLimit)})
			continue
		}
		if fp != "" && conv.prints[p][fp] {
			// 同内容指纹：保留原来的整批熔断语义（不是 P6-04 改动的范围）。
			return nil, c.exhaustLocked(conv, now,
				fmt.Sprintf("%s 对 %s 重复了同一段内容，判定为循环对话，等待人类继续", req.Sender, target))
		}
		plans = append(plans, plan{target: target, pair: p})
	}
	queueFull := !req.Human && len(conv.queue)+len(plans) > limits.MaxPendingWakes
	wakes := make([]Wake, 0, len(plans))
	skipped := 0
	for _, p := range plans {
		reason, note := p.skip, p.note
		if reason == "" && queueFull {
			reason, note = DeliveryReasonQueueFull,
				fmt.Sprintf("待投递唤醒已达上限 %d 条", limits.MaxPendingWakes)
		}
		w := Wake{
			GroupID: group, ConversationID: conv.id, Sender: req.Sender, Human: req.Human, Target: p.target,
			Text: text, Attachments: req.Attachments, Depth: depth,
			TriggerSeq: req.TriggerSeq, RoundID: req.RoundID,
			Fingerprint: fp, EnqueuedAt: now,
		}
		// P6-01：入队即登记一条 queued 投递记录（幂等键 wake id 在这里分配一次）。
		w.ID = c.nextWakeIDLocked(conv, now)
		status := DeliveryQueued
		if reason != "" {
			status = DeliverySkipped
			skipped++
		}
		conv.deliveries.put(&Delivery{
			WakeID: w.ID, ConversationID: conv.id, GroupID: group, Target: w.Target,
			Sender: w.Sender, Human: w.Human, Depth: w.Depth, TriggerSeq: w.TriggerSeq,
			RoundID: w.RoundID, Status: status, Reason: reason, Note: note, EnqueuedAt: now, UpdatedAt: now,
		})
		if status == DeliverySkipped {
			continue
		}
		wakes = append(wakes, w)
		if req.Human {
			continue // 人类发送不计入踢皮球/循环统计（§6.5.6 规则 1/4）
		}
		if fp != "" {
			if conv.prints[p.pair] == nil {
				conv.prints[p.pair] = map[string]bool{}
			}
			conv.prints[p.pair][fp] = true
		}
	}
	conv.queue = append(conv.queue, wakes...)
	conv.updatedAt = now
	// 全部目标都被护栏跳过 → waiting_human（不是 exhausted）：写一条系统说明，
	// 人类一句话就能继续（§1.1）。
	if skipped > 0 && len(wakes) == 0 {
		conv.status = ConversationWaitingHuman
		// 说明取被跳过目标的**具体原因**（深度/互踢/队列满），人一眼能看出该做什么。
		notes := make([]string, 0, len(plans))
		for _, p := range plans {
			if p.skip != "" || queueFull {
				if p.note != "" {
					notes = append(notes, p.note)
				}
			}
		}
		if len(notes) == 0 {
			conv.stop = fmt.Sprintf("本轮 %d 个目标都被护栏跳过，等待人类继续", skipped)
		} else {
			conv.stop = strings.Join(notes, "；") + "，等待人类继续"
		}
	}
	conv.deliveries.prune()
	return wakes, nil
}

// NextDue 返回「现在可以跑」的那一个 wake（不改变队列/计数）。
// 返回 false：没有对话 / 不是 running / 队列空 / 还没到 ReadyAt（冷却中）/
// 预算耗尽（这一种会把状态推进到 exhausted 并清队列）。
// 投递失败时调用方**不要**出队：下次（群 run 结束）再取一次即可。
//
// 并行调度器用 DueWakes（一次取全批）；这个「队首一条」的版本留给只要一条的
// 调用方与旧测试。
// DueWakes 返回**现在就能投递**的全部 wake（P6-02 统一调度器的一次取队）。
//
// 并发 = 本轮目标数（§1.3）：这里把队里所有可跑的目标一次交出去，由调用方并行投递。
// 约束：
//   - 每个 target 至多一条（同一智能体不能并行跑两个群 run；后面的留给下一次 pass）；
//   - 人类目标优先，其余按入队顺序（公平性）；
//   - 冷却（Cooldown）与 429 退避（Delivery.ReadyAt）挡住的不交出去；
//   - **不出队**：真正放行由 BeginDelivery 完成（幂等），所以两个 pass 同时取到
//     同一批也只会投一次。
func (c *ConversationController) DueWakes(group GroupID, at time.Time) []Wake {
	if c == nil {
		return nil
	}
	now := c.clockAt(at)
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[group]
	if conv == nil || conv.status != ConversationRunning || len(conv.queue) == 0 {
		return nil
	}
	if c.limits.TurnBudget > 0 && conv.turns >= c.limits.TurnBudget {
		c.exhaustLocked(conv, now,
			fmt.Sprintf("本轮自动对话已达上限 %d 轮，等待人类继续", c.limits.TurnBudget))
		return nil
	}
	seen := make(map[AgentID]bool, len(conv.queue))
	var human, auto []Wake
	for i := range conv.queue {
		w := conv.queue[i]
		if w.Target == "" || seen[w.Target] {
			continue
		}
		if ready := c.readyAtLocked(conv, w); now.Before(ready) {
			continue
		}
		// 429 退避：这条 wake 还要等一会儿（P6-03 写入，调度器只看不说）。
		if d := conv.deliveries.get(w.ID); d != nil && d.ReadyAt != nil && now.Before(*d.ReadyAt) {
			continue
		}
		seen[w.Target] = true
		w.ConversationID = conv.id
		w.Turn = conv.turns + 1
		w.ReadyAt = now
		if w.Human {
			human = append(human, w)
		} else {
			auto = append(auto, w)
		}
	}
	if len(human) == 0 && len(auto) == 0 {
		return nil
	}
	return append(human, auto...)
}

// NextReadyAt 队里最早可以再跑的时刻（都不可跑时用），供调度器排冷却/退避定时器。
func (c *ConversationController) NextReadyAt(group GroupID) (time.Time, bool) {
	if c == nil {
		return time.Time{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[group]
	if conv == nil || conv.status != ConversationRunning || len(conv.queue) == 0 {
		return time.Time{}, false
	}
	var best time.Time
	for i := range conv.queue {
		w := conv.queue[i]
		ready := c.readyAtLocked(conv, w)
		if d := conv.deliveries.get(w.ID); d != nil && d.ReadyAt != nil && d.ReadyAt.After(ready) {
			ready = *d.ReadyAt
		}
		if best.IsZero() || ready.Before(best) {
			best = ready
		}
	}
	if best.IsZero() {
		return time.Time{}, false
	}
	return best, true
}

// NextDue 队首的那一条（保留给「只要一条」的调用方与旧测试）。
func (c *ConversationController) NextDue(group GroupID, at time.Time) (Wake, bool) {
	if c == nil {
		return Wake{}, false
	}
	now := c.clockAt(at)
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[group]
	if conv == nil || conv.status != ConversationRunning || len(conv.queue) == 0 {
		return Wake{}, false
	}
	if c.limits.TurnBudget > 0 && conv.turns >= c.limits.TurnBudget {
		c.exhaustLocked(conv, now,
			fmt.Sprintf("本轮自动对话已达上限 %d 轮，等待人类继续", c.limits.TurnBudget))
		return Wake{}, false
	}
	w := conv.queue[0]
	w.ConversationID = conv.id
	w.Turn = conv.turns + 1
	w.ReadyAt = c.readyAtLocked(conv, w)
	if now.Before(w.ReadyAt) {
		return Wake{}, false
	}
	return w, true
}

// Peek 返回队首 wake（不看冷却，带上算好的 ReadyAt/Turn），供 bridge 设定时器。
func (c *ConversationController) Peek(group GroupID) (Wake, bool) {
	if c == nil {
		return Wake{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[group]
	if conv == nil || conv.status != ConversationRunning || len(conv.queue) == 0 {
		return Wake{}, false
	}
	w := conv.queue[0]
	w.ConversationID = conv.id
	w.Turn = conv.turns + 1
	w.ReadyAt = c.readyAtLocked(conv, w)
	return w, true
}

// BeginTurn 出队并记账：TurnsUsed++、Depth=该 wake 的跳数、记下「上一次唤醒时间」
// （下一次的冷却从此刻算起）。投递成功后调用；投递失败**不要**调用（wake 留队）。
//
// 队首版本：P6-01 起内部走 BeginDelivery（账本 + 幂等），保留给只需要「下一跳」的
// 调用方与旧测试；并行调度器用 BeginDelivery 直接指定 wake id。
func (c *ConversationController) BeginTurn(group GroupID, at time.Time) (Wake, bool) {
	if c == nil {
		return Wake{}, false
	}
	now := c.clockAt(at)
	c.mu.Lock()
	conv := c.convs[group]
	if conv == nil || conv.status != ConversationRunning || len(conv.queue) == 0 {
		c.mu.Unlock()
		return Wake{}, false
	}
	if c.limits.TurnBudget > 0 && conv.turns >= c.limits.TurnBudget {
		c.exhaustLocked(conv, now,
			fmt.Sprintf("本轮自动对话已达上限 %d 轮，等待人类继续", c.limits.TurnBudget))
		c.mu.Unlock()
		return Wake{}, false
	}
	head := conv.queue[0]
	c.mu.Unlock()
	w, fresh := c.BeginDelivery(head, at)
	if !fresh {
		return Wake{}, false
	}
	// 队首版是「出队 + 记账」一步到位的便捷入口（旧调用方/测试用）：投递成功
	// 的记账直接落上。生产路径（调度器 → wakeGroupAgent）分开做，因为 Ask 可能
	// 失败/被限流，那时不能吃轮次。
	return c.CommitDelivery(w, at), true
}

// Stop 停止自动对话：清队列、Status=stopped、记下原因。幂等：已是终态则原样返回
// （不覆盖 exhaust 的原因，避免重复系统消息）。人类优先：下次人类消息会开新对话。
func (c *ConversationController) Stop(group GroupID, reason string, at time.Time) ConversationView {
	if c == nil {
		return ConversationView{GroupID: group, Status: ConversationIdle}
	}
	now := c.clockAt(at)
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.ensureLocked(group, now)
	if conv.status.Terminal() {
		return conv.view(c.limits, now)
	}
	if strings.TrimSpace(reason) == "" {
		reason = "人类停止了自动对话"
	}
	conv.status = ConversationStopped
	conv.stop = reason
	conv.queue = nil
	conv.updatedAt = now
	return conv.view(c.limits, now)
}

// ClearQueue 只清队列（保留 status/计数），返回清掉的条数。
// 人类消息路径用它兜底：BeginHuman 之后队列本来就空，这个方法是防御性入口。
func (c *ConversationController) ClearQueue(group GroupID) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[group]
	if conv == nil {
		return 0
	}
	n := len(conv.queue)
	conv.queue = nil
	return n
}

// View 返回当前对话快照（从未有过对话时 Status=idle）。
func (c *ConversationController) View(group GroupID) ConversationView {
	if c == nil {
		return ConversationView{GroupID: group, Status: ConversationIdle}
	}
	now := c.clockAt(time.Time{})
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[group]
	if conv == nil {
		l := c.limits
		return ConversationView{
			GroupID: group, Status: ConversationIdle, TurnBudget: l.TurnBudget,
			MaxDepth: l.MaxDepth, PairLimit: l.PairLimit, CooldownMS: l.Cooldown.Milliseconds(),
			PairWindowMS: l.PairWindow.Milliseconds(), IdleResetMS: l.IdleReset.Milliseconds(),
			RunTimeoutMS: l.RunTimeout.Milliseconds(), MaxPendingWakes: l.MaxPendingWakes,
		}
	}
	c.settleLocked(conv, now)
	return conv.view(c.limits, now)
}

// PairCount 取 A→B 的次数（View 的 map 之外的便捷读法）。
func (c *ConversationController) PairCount(group GroupID, from, to AgentID) int {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[group]
	if conv == nil {
		return 0
	}
	return c.pairCountLocked(conv, newAgentPair(from, to), c.clockAt(time.Time{}))
}

// pairCountLocked 无序 pair 在**滚动窗口**内的成功投递次数（§1.1）。
// 惰性过期：顺手把窗口外的时刻丢掉（不靠定时器）。
func (c *ConversationController) pairCountLocked(conv *conversation, p agentPair, now time.Time) int {
	if conv == nil || len(conv.pairs) == 0 {
		return 0
	}
	at := conv.pairs[p]
	if len(at) == 0 {
		return 0
	}
	if c.limits.PairWindow < 0 {
		return len(at) // <0 = 关闭过期（测试用）
	}
	cut := now.Add(-c.limits.PairWindow)
	kept := at[:0]
	for _, t := range at {
		if !t.Before(cut) {
			kept = append(kept, t)
		}
	}
	conv.pairs[p] = kept
	return len(kept)
}

// pairResetAtLocked 该 pair 的最早一次计数过期时刻（UI 显示「约 N 分钟后自动恢复」）。
func (c *ConversationController) pairResetAtLocked(conv *conversation, p agentPair) (time.Time, bool) {
	if conv == nil || c.limits.PairWindow < 0 {
		return time.Time{}, false
	}
	at := conv.pairs[p]
	if len(at) == 0 {
		return time.Time{}, false
	}
	return at[0].Add(c.limits.PairWindow), true
}

// settleLocked 惰性结算「空闲重置」（§1.1 ③ / §1.2）：群连续 IdleReset 没有
// run/wake → 回 idle，pair/depth/指纹/队列全部清零。不靠定时器。
func (c *ConversationController) settleLocked(conv *conversation, now time.Time) {
	if conv == nil || c.limits.IdleReset < 0 {
		return
	}
	if conv.status == ConversationIdle {
		return
	}
	last := conv.updatedAt
	if conv.lastWake.After(last) {
		last = conv.lastWake
	}
	if last.IsZero() || now.Sub(last) < c.limits.IdleReset {
		return
	}
	c.resetLocked(conv, now)
}

// resetLocked 清空一代对话的计数并回 idle（人类新输入 / 空闲超时 / 手动重置共用）。
func (c *ConversationController) resetLocked(conv *conversation, now time.Time) {
	conv.status = ConversationIdle
	conv.stop = ""
	conv.turns = 0
	conv.depth = 0
	conv.queue = nil
	conv.pairs = map[agentPair][]time.Time{}
	conv.prints = map[agentPair]map[string]bool{}
	conv.lastWake = time.Time{}
	conv.updatedAt = now
	if conv.deliveries != nil {
		conv.deliveries.dropQueued()
	}
}

// Reset 手动重置本群限制（P6-04 ④）：立即清零 pair/depth/指纹/队列并解除
// waiting_human，**不自动唤醒任何人**（用户下一次 @ 立即生效）。
func (c *ConversationController) Reset(group GroupID, at time.Time) ConversationView {
	if c == nil {
		return ConversationView{GroupID: group, Status: ConversationIdle}
	}
	now := c.clockAt(at)
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[group]
	if conv == nil {
		l := c.limits
		return ConversationView{
			GroupID: group, Status: ConversationIdle, TurnBudget: l.TurnBudget,
			MaxDepth: l.MaxDepth, PairLimit: l.PairLimit, CooldownMS: l.Cooldown.Milliseconds(),
			PairWindowMS: l.PairWindow.Milliseconds(), IdleResetMS: l.IdleReset.Milliseconds(),
			RunTimeoutMS: l.RunTimeout.Milliseconds(), MaxPendingWakes: l.MaxPendingWakes,
		}
	}
	c.resetLocked(conv, now)
	return conv.view(c.limits, now)
}

// exhaustLocked 推进到 exhausted 并返回带原因的 typed error。
func (c *ConversationController) exhaustLocked(conv *conversation, now time.Time, reason string) error {
	if conv.status.Terminal() {
		return &ExhaustedError{GroupID: conv.group, Reason: conv.stop, Status: conv.status}
	}
	conv.status = ConversationExhausted
	conv.stop = reason
	conv.queue = nil
	conv.updatedAt = now
	return &ExhaustedError{GroupID: conv.group, Reason: reason, Status: conv.status}
}

// readyAtLocked 计算可跑时刻：max(入队时间, 上一次智能体唤醒 + 冷却)。
// 新对话（lastWake 为零）永远立即可跑 —— 人类消息不能被冷却拖住。
func (c *ConversationController) readyAtLocked(conv *conversation, w Wake) time.Time {
	at := w.EnqueuedAt
	if conv.lastWake.IsZero() || c.limits.Cooldown <= 0 {
		return at
	}
	if next := conv.lastWake.Add(c.limits.Cooldown); next.After(at) {
		return next
	}
	return at
}

func (conv *conversation) view(l ConversationLimits, now time.Time) ConversationView {
	v := ConversationView{
		ID:         conv.id,
		GroupID:    conv.group,
		Status:     conv.status,
		HumanSeq:   conv.humanSeq,
		TurnsUsed:  conv.turns,
		TurnBudget: l.TurnBudget,
		Depth:      conv.depth,
		MaxDepth:   l.MaxDepth,
		QueueLen:   len(conv.queue),
		PairLimit:  l.PairLimit,
		CooldownMS: l.Cooldown.Milliseconds(),
		StopReason: conv.stop,
	}
	v.PairWindowMS = l.PairWindow.Milliseconds()
	v.IdleResetMS = l.IdleReset.Milliseconds()
	v.RunTimeoutMS = l.RunTimeout.Milliseconds()
	v.MaxPendingWakes = l.MaxPendingWakes
	if len(conv.pairs) > 0 {
		v.PairCount = make(map[string]int, len(conv.pairs))
		var hottest agentPair
		hottestN := -1
		for p, at := range conv.pairs {
			n := len(at)
			if l.PairWindow >= 0 {
				cut := now.Add(-l.PairWindow)
				n = 0
				for _, t := range at {
					if !t.Before(cut) {
						n++
					}
				}
			}
			if n == 0 {
				continue
			}
			v.PairCount[ConversationPairKey(p.From, p.To)] = n
			if n > hottestN {
				hottest, hottestN = p, n
			}
		}
		if hottestN > 0 {
			if len(conv.pairs[hottest]) > 0 && l.PairWindow >= 0 {
				t := conv.pairs[hottest][0].Add(l.PairWindow)
				v.PairResetAt = &t
			}
		}
	}
	if !conv.lastWake.IsZero() {
		t := conv.lastWake
		v.LastWakeAt = &t
	}
	if !conv.startedAt.IsZero() {
		t := conv.startedAt
		v.StartedAt = &t
	}
	if !conv.updatedAt.IsZero() {
		t := conv.updatedAt
		v.UpdatedAt = &t
	}
	if conv.deliveries != nil {
		v.Deliveries = conv.deliveries.listRecent(ConversationViewDeliveries)
	}
	return v
}

// dedupeTargets 去空、去重、去掉自我 @（§6.5.6 规则 10）。
func dedupeTargets(targets []AgentID, sender AgentID) []AgentID {
	if len(targets) == 0 {
		return nil
	}
	seen := make(map[AgentID]struct{}, len(targets))
	out := make([]AgentID, 0, len(targets))
	for _, t := range targets {
		t = AgentID(strings.TrimSpace(string(t)))
		if t == "" || t == sender {
			continue
		}
		if _, dup := seen[t]; dup {
			continue
		}
		seen[t] = struct{}{}
		out = append(out, t)
	}
	return out
}

// NormalizeFingerprint 归一化正文指纹（§6.5.6 规则 8）：去掉 Unicode 空白与标点、
// 转小写，取前 200 个 rune。只用于「同一对 agent 是否在重复同一句话」，不落账。
func NormalizeFingerprint(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	n := 0
	for _, r := range text {
		if unicode.IsSpace(r) || unicode.IsPunct(r) {
			continue
		}
		b.WriteRune(unicode.ToLower(r))
		n++
		if n >= ConversationFingerprintRunes {
			break
		}
	}
	return b.String()
}
