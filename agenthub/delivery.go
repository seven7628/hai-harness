package agenthub

// delivery.go：群唤醒的**投递账本**（P6-01）。
//
// 一条 @ 会变成 N 条 wake，每条都有独立命运：queued（排队等目标空闲）→
// running（已交给目标会话）→ done（run 收尾）；或在护栏/停止/错误处分叉成
// skipped / failed。以前这些状态只隐含在「队里还有没有这条 wake」和一条告警
// 字符串里 —— 人看不到 12 人群 @all 里到底谁被叫醒了、谁还在排队、谁被 pair
// 上限挡了。
//
// 账本同时是**幂等**的唯一判据：同一个 wake id 只允许从 queued 进 running 一次。
// 断线重试、重复泵、两个 goroutine 同时看到空闲，都不会让目标会话收到第二份快照
// （两份快照共用一次 AgentEnd，final 会归错轮）。
//
// 生命周期：每条 wake 在 Offer（或人类首轮投递）时登记，终态后仍保留一段时间
// （有界，见 MaxConversationDeliveries），供 UI 与命令回执展示「这一轮发生了什么」。

import "time"

// DeliveryStatus 是单目标投递状态。状态机（只允许单向推进）：
//
//	queued → running → done
//	   │        │
//	   ├────────┴→ skipped（护栏/停止/换代：这一轮不会跑）
//	   └─────────→ failed（投递出错；可重试的错误会退回 queued）
type DeliveryStatus string

const (
	// DeliveryQueued 已入队，还没交给目标会话（目标忙/冷却/等调度器放行）。
	DeliveryQueued DeliveryStatus = "queued"
	// DeliveryRunning 已投递给目标会话，run 在飞（AgentStart..AgentEnd）。
	DeliveryRunning DeliveryStatus = "running"
	// DeliveryDone 这一轮 run 已收尾。
	DeliveryDone DeliveryStatus = "done"
	// DeliverySkipped 有意不跑（pair/深度护栏、人类停止、换代、预算不足）。
	DeliverySkipped DeliveryStatus = "skipped"
	// DeliveryFailed 投递失败（会话装配/存储错误等）。
	DeliveryFailed DeliveryStatus = "failed"
)

// Terminal 终态：不会再推进（done/skipped/failed）。
func (s DeliveryStatus) Terminal() bool {
	return s == DeliveryDone || s == DeliverySkipped || s == DeliveryFailed
}

// Active 还在进行中（queued/running）。
func (s DeliveryStatus) Active() bool {
	return s == DeliveryQueued || s == DeliveryRunning
}

// Valid 是否是已知状态（空串 = 未知，按 queued 处理）。
func (s DeliveryStatus) Valid() bool {
	switch s {
	case DeliveryQueued, DeliveryRunning, DeliveryDone, DeliverySkipped, DeliveryFailed:
		return true
	}
	return false
}

// 投递被跳过/失败的人话原因码。UI 按码本地化，报告/日志按码聚合；
// 真正的解释文案放 Delivery.Reason（可读），两者都留。
const (
	DeliveryReasonBusy        = "target_busy"        // 目标上一轮还没结束 → 留队重试
	DeliveryReasonRateLimit   = "rate_limit"         // provider 429 → 退避后重试，不算失败
	DeliveryReasonPairLimit   = "pair_limit"         // 无序 pair 到限（P6-04）
	DeliveryReasonDepthLimit  = "depth_limit"        // 这条链超过 max_depth（P6-05）
	DeliveryReasonBudget      = "budget"             // 预算不足，不启动 run（P6-03）
	DeliveryReasonStopped     = "stopped"            // 人类停止自动对话
	DeliveryReasonStale       = "stale_conversation" // 旧 generation 的 wake，丢弃
	DeliveryReasonDuplicate   = "duplicate"          // 同一 wake id 重复投递（幂等命中）
	DeliveryReasonInterrupted = "interrupted"        // run 被中断（停止/关停/按 agent 停止）
	DeliveryReasonTimeout     = "run_timeout"        // 单轮 run 超过 run_timeout_ms，被中断（P6-08）
	DeliveryReasonShutdown    = "shutdown"           // Hub 正在关停
	DeliveryReasonInvalid     = "invalid"            // 群/成员/会话已不可用
	DeliveryReasonQueueFull   = "queue_full"         // 待投递队列到达 max_pending_wakes
)

// Delivery 是一条 wake 的投递记录（一个 target 一条）。
type Delivery struct {
	// WakeID 是投递幂等键：同一条 wake 的重复投递必须复用同一个 id。
	WakeID string `json:"wake_id"`
	// ConversationID 是入队时所属对话 generation（人类一次输入一代）。
	ConversationID string  `json:"conversation_id,omitempty"`
	GroupID        GroupID `json:"group_id"`
	Target         AgentID `json:"target"`
	Sender         AgentID `json:"sender,omitempty"`
	// Human 表示这条 wake 由人类消息触发（人类目标优先、不计 pair）。
	Human      bool   `json:"human,omitempty"`
	Depth      int    `json:"depth,omitempty"`
	Turn       int    `json:"turn,omitempty"`
	TriggerSeq int64  `json:"trigger_seq,omitempty"`
	RoundID    string `json:"round_id,omitempty"`

	Status DeliveryStatus `json:"status"`
	// Reason 是**原因码**（target_busy/pair_limit/depth_limit/budget/...），UI 按码
	// 本地化；Note 是同一件事的人话说明（写进系统消息/排障日志）。两者都留：
	// 只存文案的话前端无法翻译，只存码的话排障时看不出「跟谁、几次、窗口多长」。
	Reason string `json:"reason,omitempty"`
	Note   string `json:"note,omitempty"`
	// Attempts 投递尝试次数（busy/429 重试会累加）。
	Attempts int `json:"attempts,omitempty"`
	// ReadyAt 是「最早可以再试」的时刻（429 退避用；零值 = 随时可试）。
	ReadyAt *time.Time `json:"ready_at,omitempty"`

	EnqueuedAt time.Time `json:"enqueued_at,omitempty"`
	UpdatedAt  time.Time `json:"updated_at,omitempty"`
}

// clone 深拷贝（*time.Time 需要独立副本：调用方/UI 不该拿到内部指针）。
func (d Delivery) clone() Delivery {
	out := d
	if d.ReadyAt != nil {
		t := *d.ReadyAt
		out.ReadyAt = &t
	}
	return out
}

// MaxConversationDeliveries 每个群保留的投递记录上限。群聊里一轮 @all 最多
// 成员数条，64 足够覆盖最近若干轮；超过就丢最旧的**终态**记录（进行中的永不丢）。
const MaxConversationDeliveries = 64

// deliveryLedger 是每群一份的投递账本。调用方必须持 ConversationController 锁。
type deliveryLedger struct {
	byID  map[string]*Delivery
	order []string
}

func newDeliveryLedger() *deliveryLedger {
	return &deliveryLedger{byID: map[string]*Delivery{}}
}

func (l *deliveryLedger) get(id string) *Delivery {
	if l == nil || id == "" {
		return nil
	}
	return l.byID[id]
}

func (l *deliveryLedger) put(d *Delivery) {
	if l == nil || d == nil || d.WakeID == "" {
		return
	}
	if l.byID == nil {
		l.byID = map[string]*Delivery{}
	}
	if _, ok := l.byID[d.WakeID]; !ok {
		l.order = append(l.order, d.WakeID)
	}
	cp := d.clone()
	l.byID[d.WakeID] = &cp
}

// list 按登记顺序返回快照（旧 → 新）。
func (l *deliveryLedger) list() []Delivery {
	if l == nil || len(l.order) == 0 {
		return nil
	}
	out := make([]Delivery, 0, len(l.order))
	for _, id := range l.order {
		if d := l.byID[id]; d != nil {
			out = append(out, d.clone())
		}
	}
	return out
}

// listRecent 返回最近 n 条快照（旧 → 新）。窗口外还有进行中的记录时继续往前扩：
// 「谁还在排队/谁在跑」必须永远可见，终态记录才可以被截掉。
func (l *deliveryLedger) listRecent(n int) []Delivery {
	all := l.list()
	if n <= 0 || len(all) <= n {
		return all
	}
	start := len(all) - n
	for start > 0 && all[start-1].Status.Active() {
		start--
	}
	return all[start:]
}

// active 该群还在进行中的投递数（queued/running）。
func (l *deliveryLedger) active() int {
	if l == nil {
		return 0
	}
	n := 0
	for _, d := range l.byID {
		if d.Status.Active() {
			n++
		}
	}
	return n
}

// activeFor 某目标还在进行中的投递（每目标最多一条，靠群会话忙闩保证）。
func (l *deliveryLedger) activeFor(target AgentID) *Delivery {
	if l == nil {
		return nil
	}
	var found *Delivery
	for _, id := range l.order {
		d := l.byID[id]
		if d != nil && d.Target == target && d.Status.Active() {
			found = d
		}
	}
	return found
}

// prune 截断到上限：只丢最旧的终态记录，进行中的一条都不丢。
func (l *deliveryLedger) prune() {
	if l == nil || len(l.order) <= MaxConversationDeliveries {
		return
	}
	drop := len(l.order) - MaxConversationDeliveries
	kept := make([]string, 0, len(l.order))
	for _, id := range l.order {
		d := l.byID[id]
		if drop > 0 && (d == nil || d.Status.Terminal()) {
			delete(l.byID, id)
			drop--
			continue
		}
		kept = append(kept, id)
	}
	l.order = kept
}

// dropQueued 丢弃所有还在排队的记录（人类开新对话时旧 wake 作废）。
// 返回丢掉的条数；running/终态记录保留（在飞 run 还能正常收尾/展示）。
func (l *deliveryLedger) dropQueued() int { return l.dropQueuedExcept(nil) }

// drop 删掉一条记录（被新的人类消息取代的 pending 输入用），返回是否删到。
func (l *deliveryLedger) drop(id string) bool {
	if l == nil || id == "" {
		return false
	}
	if _, ok := l.byID[id]; !ok {
		return false
	}
	delete(l.byID, id)
	kept := l.order[:0]
	for _, x := range l.order {
		if _, ok := l.byID[x]; ok {
			kept = append(kept, x)
		}
	}
	l.order = kept
	return true
}

// dropQueuedExcept 同 dropQueued，但保留 keep 里的 wake id：人类自己的「下一个输入」
// 在换代后仍然要投（P6-07 §1.5），账本记录不能跟着旧 generation 一起消失。
func (l *deliveryLedger) dropQueuedExcept(keep map[string]struct{}) int {
	if l == nil {
		return 0
	}
	n := 0
	for id, d := range l.byID {
		if d == nil || d.Status != DeliveryQueued {
			continue
		}
		if _, ok := keep[id]; ok {
			continue
		}
		delete(l.byID, id)
		n++
	}
	if n == 0 {
		return 0
	}
	kept := l.order[:0]
	for _, id := range l.order {
		if _, ok := l.byID[id]; ok {
			kept = append(kept, id)
		}
	}
	l.order = kept
	return n
}
