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

// 对话默认限额（方案 §6.5.6 规则 4/7；D13 = 8 轮）。
const (
	DefaultConversationTurnBudget = 8
	DefaultConversationMaxDepth   = 4
	DefaultConversationPairLimit  = 2
	DefaultConversationCooldown   = 1500 * time.Millisecond
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
type ConversationLimits struct {
	TurnBudget int
	MaxDepth   int
	PairLimit  int
	Cooldown   time.Duration
}

// withDefaults 把零值补成默认值。**Cooldown<0 表示关闭冷却**（测试用）：
// 「首轮立即唤醒、后续轮之间强制间隔」这条语义需要一个显式的关闭开关。
func (l ConversationLimits) withDefaults() ConversationLimits {
	if l.TurnBudget <= 0 {
		l.TurnBudget = DefaultConversationTurnBudget
	}
	if l.MaxDepth <= 0 {
		l.MaxDepth = DefaultConversationMaxDepth
	}
	if l.PairLimit <= 0 {
		l.PairLimit = DefaultConversationPairLimit
	}
	if l.Cooldown == 0 {
		l.Cooldown = DefaultConversationCooldown
	}
	if l.Cooldown < 0 {
		l.Cooldown = 0
	}
	return l
}

// Wake 是一次待跑的群唤醒：谁 @ 了谁、第几跳、带哪条触发消息。
// Turn 在出队（BeginTurn）时才最终确定；NextDue/Peek 给出的是「如果现在跑，会是第几轮」。
type Wake struct {
	GroupID     GroupID   `json:"group_id"`
	Sender      AgentID   `json:"sender,omitempty"`
	Human       bool      `json:"human,omitempty"` // 人类发送：不计 PairCount/不算循环
	Target      AgentID   `json:"target"`
	Text        string    `json:"text,omitempty"`
	Depth       int       `json:"depth"`
	Turn        int       `json:"turn"`
	TriggerSeq  int64     `json:"trigger_seq,omitempty"`
	RoundID     string    `json:"round_id,omitempty"`
	Fingerprint string    `json:"fingerprint,omitempty"`
	EnqueuedAt  time.Time `json:"enqueued_at"`
	// ReadyAt 是可跑时刻：max(入队时间, 上一次唤醒 + 冷却)。冷却只作用于
	// **连续两次智能体唤醒之间**，所以新对话的第一个 wake 永远是「现在就绪」。
	ReadyAt time.Time `json:"ready_at"`
}

// ConversationOffer 是一次 Offer 请求（一条智能体/人类消息上的全部 @ 目标）。
type ConversationOffer struct {
	GroupID    GroupID
	Sender     AgentID
	Human      bool // true = 人类发送（Sender 为空）：不计 PairCount、不算指纹循环
	Targets    []AgentID
	Text       string // 触发这条唤醒的消息正文（指纹来源）
	Depth      int    // 这批 wake 的跳数（人类首轮 = 1）
	TriggerSeq int64
	RoundID    string
	At         time.Time
}

// ConversationView 是给事件/UI/命令回执的快照（JSON 直接可读）。
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
	LastWakeAt *time.Time     `json:"last_wake_at,omitempty"`
	StopReason string         `json:"stop_reason,omitempty"`
	StartedAt  *time.Time     `json:"started_at,omitempty"`
	UpdatedAt  *time.Time     `json:"updated_at,omitempty"`
}

// Terminal 终态（stopped/exhausted）；idle/running 都不算。
func (v ConversationView) Terminal() bool { return v.Status.Terminal() }

// ConversationPairKey 是 PairCount 的键（A→B）。
func ConversationPairKey(from, to AgentID) string {
	return string(from) + "→" + string(to)
}

// ConversationOptions 构造参数：Limits 零值取默认，Now 为空用 time.Now。
type ConversationOptions struct {
	Limits ConversationLimits
	Now    func() time.Time
}

type agentPair struct{ From, To AgentID }

type conversation struct {
	id        string
	group     GroupID
	humanSeq  int64
	status    ConversationStatus
	stop      string
	turns     int
	depth     int
	queue     []Wake
	pairs     map[agentPair]int
	prints    map[agentPair]map[string]bool
	lastWake  time.Time
	startedAt time.Time
	updatedAt time.Time
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
			group:     group,
			status:    ConversationIdle,
			pairs:     map[agentPair]int{},
			prints:    map[agentPair]map[string]bool{},
			startedAt: now,
			updatedAt: now,
		}
		c.convs[group] = conv
	}
	return conv
}

func (c *ConversationController) nextIDLocked(now time.Time) string {
	c.seq++
	return fmt.Sprintf("conv_%d_%d", now.UnixMilli(), c.seq)
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
	conv.pairs = map[agentPair]int{}
	conv.prints = map[agentPair]map[string]bool{}
	conv.lastWake = time.Time{}
	conv.startedAt = now
	conv.updatedAt = now
	return conv.view(c.limits)
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
//   - 轮次预算：TurnsUsed >= TurnBudget；
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
	// 人类优先：预算与深度是「自动接龙」的护栏，人类发送不受它们约束（§6.5.6
	// 规则 1/5）。bridge 的人类路径本来就是并行唤醒、不进队列；这里保留同一口径，
	// 免得将来把人类唤醒并进队列时被预算误杀。
	if !req.Human && conv.turns >= limits.TurnBudget {
		return nil, c.exhaustLocked(conv, now,
			fmt.Sprintf("本轮自动对话已达上限 %d 轮，等待人类继续", limits.TurnBudget))
	}
	depth := req.Depth
	if req.Human {
		depth = 1 // 人类唤醒的永远是第一跳（人类自身深度 0）
	} else {
		if depth <= 0 {
			depth = conv.depth + 1
		}
		// 深度只能从**当前对话**的深度推进：人类插话会开新对话并把 depth 归零，
		// 而旧链路上还在跑的那一轮手里还攥着旧 header（比如 depth=4）。不夹紧的话，
		// 它回来 @ 一句就把新对话直接顶到 5 → 新对话一开局就判「深度耗尽」。
		if next := conv.depth + 1; depth > next {
			depth = next
		}
	}
	if !req.Human && depth > limits.MaxDepth {
		return nil, c.exhaustLocked(conv, now,
			fmt.Sprintf("对话深度已达上限 %d 跳（A→B→C 链），等待人类继续", limits.MaxDepth))
	}
	fp := ""
	if !req.Human {
		fp = NormalizeFingerprint(text)
	}
	type plan struct {
		target AgentID
		pair   agentPair
	}
	plans := make([]plan, 0, len(targets))
	for _, target := range targets {
		p := agentPair{From: req.Sender, To: target}
		if !req.Human {
			if got := conv.pairs[p]; got >= limits.PairLimit {
				return nil, c.exhaustLocked(conv, now,
					fmt.Sprintf("%s 与 %s 已互踢 %d 次（上限 %d），等待人类继续", req.Sender, target, got, limits.PairLimit))
			}
			if fp != "" && conv.prints[p][fp] {
				return nil, c.exhaustLocked(conv, now,
					fmt.Sprintf("%s 对 %s 重复了同一段内容，判定为循环对话，等待人类继续", req.Sender, target))
			}
		}
		plans = append(plans, plan{target: target, pair: p})
	}
	wakes := make([]Wake, 0, len(plans))
	for _, p := range plans {
		wakes = append(wakes, Wake{
			GroupID: group, Sender: req.Sender, Human: req.Human, Target: p.target,
			Text: text, Depth: depth, TriggerSeq: req.TriggerSeq, RoundID: req.RoundID,
			Fingerprint: fp, EnqueuedAt: now,
		})
		if req.Human {
			continue // 人类发送不计入踢皮球/循环统计（§6.5.6 规则 1/4）
		}
		conv.pairs[p.pair]++
		if fp != "" {
			if conv.prints[p.pair] == nil {
				conv.prints[p.pair] = map[string]bool{}
			}
			conv.prints[p.pair][fp] = true
		}
	}
	conv.queue = append(conv.queue, wakes...)
	conv.updatedAt = now
	return wakes, nil
}

// NextDue 返回「现在可以跑」的那一个 wake（不改变队列/计数）。
// 返回 false：没有对话 / 不是 running / 队列空 / 还没到 ReadyAt（冷却中）/
// 预算耗尽（这一种会把状态推进到 exhausted 并清队列）。
// 投递失败时调用方**不要**出队：下次（群 run 结束）再取一次即可。
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
	if conv.turns >= c.limits.TurnBudget {
		c.exhaustLocked(conv, now,
			fmt.Sprintf("本轮自动对话已达上限 %d 轮，等待人类继续", c.limits.TurnBudget))
		return Wake{}, false
	}
	w := conv.queue[0]
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
	w.Turn = conv.turns + 1
	w.ReadyAt = c.readyAtLocked(conv, w)
	return w, true
}

// BeginTurn 出队并记账：TurnsUsed++、Depth=该 wake 的跳数、记下「上一次唤醒时间」
// （下一次的冷却从此刻算起）。投递成功后调用；投递失败**不要**调用（wake 留队）。
func (c *ConversationController) BeginTurn(group GroupID, at time.Time) (Wake, bool) {
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
	if conv.turns >= c.limits.TurnBudget {
		c.exhaustLocked(conv, now,
			fmt.Sprintf("本轮自动对话已达上限 %d 轮，等待人类继续", c.limits.TurnBudget))
		return Wake{}, false
	}
	w := conv.queue[0]
	if ready := c.readyAtLocked(conv, w); now.Before(ready) {
		return Wake{}, false
	}
	conv.queue = conv.queue[1:]
	conv.turns++
	conv.depth = w.Depth
	conv.lastWake = now
	conv.updatedAt = now
	w.Turn = conv.turns
	w.ReadyAt = now
	return w, true
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
		return conv.view(c.limits)
	}
	if strings.TrimSpace(reason) == "" {
		reason = "人类停止了自动对话"
	}
	conv.status = ConversationStopped
	conv.stop = reason
	conv.queue = nil
	conv.updatedAt = now
	return conv.view(c.limits)
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
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[group]
	if conv == nil {
		l := c.limits
		return ConversationView{
			GroupID: group, Status: ConversationIdle, TurnBudget: l.TurnBudget,
			MaxDepth: l.MaxDepth, PairLimit: l.PairLimit, CooldownMS: l.Cooldown.Milliseconds(),
		}
	}
	return conv.view(c.limits)
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
	return conv.pairs[agentPair{From: from, To: to}]
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

func (conv *conversation) view(l ConversationLimits) ConversationView {
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
	if len(conv.pairs) > 0 {
		v.PairCount = make(map[string]int, len(conv.pairs))
		for p, n := range conv.pairs {
			v.PairCount[ConversationPairKey(p.From, p.To)] = n
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
