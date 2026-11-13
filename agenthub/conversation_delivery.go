package agenthub

// conversation_delivery.go：投递账本的控制器入口（P6-01）。
//
// 状态推进只在这里发生，且**只允许单向**：queued → running → done / failed，
// 或 queued → skipped。任何「同一 wake id 再来一次」的调用都返回 false，让调用方
// 当成 no-op —— 这就是「重复投递幂等」的全部实现（断线重试、重复泵、并行调度器
// 抢同一个目标都走这条路）。
//
// 与队列的关系：账本是**旁路**记录，队列仍是「谁在等」的唯一真相。两者靠
// wake id 对齐：BeginDelivery 出队 + 记账一次完成；投递失败（目标忙/429）由
// RequeueDelivery 把 wake 放回队列并把记录退回 queued。

import "time"

// BeginDelivery 出队并记账：queued → running。返回的 wake 才是**允许真的去投**的那一份
// （带最终 Turn/ConversationID/ReadyAt）；第二个返回值为 false 表示：
//   - 这条 wake 已经投递过（running/done/skipped/failed）→ 重复投递，调用方必须 no-op；
//   - 它属于旧 generation（人类已经开了新对话）→ 顺手记成 skipped(stale)。
//
// 人类 wake 不累加 TurnsUsed：人类首轮的 1 个回合已经由 StartHumanRound 记过，
// 再算一次会让「第 N 轮」在人类插话后虚增。
func (c *ConversationController) BeginDelivery(w Wake, at time.Time) (Wake, bool) {
	if c == nil || w.GroupID == "" || w.Target == "" {
		return w, false
	}
	now := c.clockAt(at)
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.ensureLocked(w.GroupID, now)
	if conv.deliveries == nil {
		conv.deliveries = newDeliveryLedger()
	}
	if w.ID == "" {
		w.ID = c.nextWakeIDLocked(conv, now)
	}
	d := conv.deliveries.get(w.ID)
	if d == nil {
		// 直接构造的 wake（没走 Offer）：补登记成 queued，再照常推进。
		conv.deliveries.put(&Delivery{
			WakeID: w.ID, ConversationID: w.ConversationID, GroupID: w.GroupID, Target: w.Target,
			Sender: w.Sender, Human: w.Human, Depth: w.Depth, TriggerSeq: w.TriggerSeq,
			RoundID: w.RoundID, Status: DeliveryQueued, EnqueuedAt: now, UpdatedAt: now,
		})
		d = conv.deliveries.get(w.ID)
	}
	if d == nil {
		return w, false
	}
	if d.Status != DeliveryQueued {
		// 幂等命中：已投/已终结，绝不重复交给目标会话。
		return w, false
	}
	// 换代：旧 generation 的 wake 不得进入新对话（D5/P6-05 的「旧 run 不消耗新链额度」）。
	if d.ConversationID != "" && conv.id != "" && d.ConversationID != conv.id {
		d.Status = DeliverySkipped
		d.Reason = DeliveryReasonStale
		d.UpdatedAt = now
		c.removeQueuedLocked(conv, w.ID)
		return w, false
	}
	c.removeQueuedLocked(conv, w.ID)
	w.ConversationID = conv.id
	// Turn 是**预测值**：真正记账在 CommitDelivery（Ask 成功之后）。人类首轮的 1 个
	// 回合已由 StartHumanRound 记过，所以人类 wake 不再 +1。
	w.Turn = conv.turns
	if !w.Human {
		w.Turn++
	}
	w.ReadyAt = now
	d.Status = DeliveryRunning
	d.Turn = w.Turn
	d.Attempts++
	d.ReadyAt = nil
	d.Reason = ""
	d.UpdatedAt = now
	conv.updatedAt = now
	return w, true
}

// CommitDelivery 投递**成功**之后记账：TurnsUsed++、Depth、冷却起点（lastWake）。
//
// 为什么与 BeginDelivery 分开：BeginDelivery 必须在 Ask 之前（幂等判据），而
// 「目标忙 / 429 退回队列」的尝试不能消耗轮次 —— 否则一个忙目标被重试 3 次就
// 吃掉 3 轮预算，UI 上的「第 N 轮」也会虚增。人类 wake 不累加 TurnsUsed。
//
// 已停止/已收口的记录不再记账（那一轮随停止作废）。
func (c *ConversationController) CommitDelivery(w Wake, at time.Time) Wake {
	if c == nil || w.GroupID == "" {
		return w
	}
	now := c.clockAt(at)
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[w.GroupID]
	if conv == nil || conv.deliveries == nil {
		return w
	}
	d := conv.deliveries.get(w.ID)
	if d == nil || d.Status != DeliveryRunning {
		return w
	}
	if !w.Human {
		conv.turns++
		conv.depth = w.Depth
		conv.lastWake = now
		w.Turn = conv.turns
		d.Turn = w.Turn
	}
	d.UpdatedAt = now
	conv.updatedAt = now
	return w
}

// RequeueDelivery 投递失败可重试（目标忙 / provider 429）时把 wake 放回队列：
// 记录退回 queued（**不是 failed**）、累加尝试次数、记下最早可再试时刻。
// 返回 false 表示这条记录已终结（停止/换代），调用方不要再投。
func (c *ConversationController) RequeueDelivery(w Wake, reason string, readyAt time.Time, at time.Time) bool {
	if c == nil || w.GroupID == "" {
		return false
	}
	now := c.clockAt(at)
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[w.GroupID]
	if conv == nil || conv.deliveries == nil {
		return false
	}
	d := conv.deliveries.get(w.ID)
	if d == nil || d.Status.Terminal() {
		return false
	}
	d.Status = DeliveryQueued
	d.Reason = reason
	d.UpdatedAt = now
	if !readyAt.IsZero() {
		t := readyAt
		d.ReadyAt = &t
	}
	if !c.queuedLocked(conv, w.ID) {
		c.enqueueLocked(conv, w)
	}
	conv.updatedAt = now
	return true
}

// FinishDelivery 把一条进行中的记录推进到终态（done/skipped/failed）。
// 已终结的记录原样返回 false（幂等：AgentEnd 与停止路径可能都来收口）。
func (c *ConversationController) FinishDelivery(group GroupID, wakeID string, status DeliveryStatus, reason string, at time.Time) bool {
	if c == nil || wakeID == "" || !status.Terminal() {
		return false
	}
	now := c.clockAt(at)
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[group]
	if conv == nil || conv.deliveries == nil {
		return false
	}
	d := conv.deliveries.get(wakeID)
	if d == nil || d.Status.Terminal() {
		return false
	}
	d.Status = status
	d.Reason = reason
	d.UpdatedAt = now
	d.ReadyAt = nil
	conv.updatedAt = now
	return true
}

// FinishAgentDeliveries 收口某目标**正在跑**的投递（run 结束时调用）。
// 每目标同时最多一条 running（群会话忙闩保证），所以不需要 wake id 也能精确收口。
func (c *ConversationController) FinishAgentDeliveries(group GroupID, agent AgentID, status DeliveryStatus, reason string, at time.Time) int {
	if c == nil || !status.Terminal() {
		return 0
	}
	now := c.clockAt(at)
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[group]
	if conv == nil || conv.deliveries == nil {
		return 0
	}
	n := 0
	for _, d := range conv.deliveries.byID {
		if d == nil || d.Target != agent || d.Status != DeliveryRunning {
			continue
		}
		d.Status = status
		d.Reason = reason
		d.UpdatedAt = now
		n++
	}
	if n > 0 {
		conv.updatedAt = now
	}
	return n
}

// SkipQueuedDeliveries 把还在排队的记录记成 skipped（停止/关停/预算门用），
// 返回被跳过的条数。run 中的记录不动（它们由 FinishAgentDeliveries 收口）。
func (c *ConversationController) SkipQueuedDeliveries(group GroupID, reason string, at time.Time) int {
	if c == nil {
		return 0
	}
	now := c.clockAt(at)
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[group]
	if conv == nil || conv.deliveries == nil {
		return 0
	}
	n := 0
	for _, d := range conv.deliveries.byID {
		if d == nil || d.Status != DeliveryQueued {
			continue
		}
		d.Status = DeliverySkipped
		d.Reason = reason
		d.UpdatedAt = now
		n++
	}
	if n > 0 {
		conv.updatedAt = now
	}
	return n
}

// InterruptRunningDeliveries 把正在跑的记录记成 failed(interrupted)：run 已经开始，
// 但被停止/关停打断，不会再正常收尾成 done。返回收口条数。
func (c *ConversationController) InterruptRunningDeliveries(group GroupID, reason string, at time.Time) int {
	if c == nil {
		return 0
	}
	now := c.clockAt(at)
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[group]
	if conv == nil || conv.deliveries == nil {
		return 0
	}
	n := 0
	for _, d := range conv.deliveries.byID {
		if d == nil || d.Status != DeliveryRunning {
			continue
		}
		d.Status = DeliveryFailed
		d.Reason = reason
		d.UpdatedAt = now
		n++
	}
	if n > 0 {
		conv.updatedAt = now
	}
	return n
}

// Deliveries 返回该群投递账本的快照（旧 → 新）。
func (c *ConversationController) Deliveries(group GroupID) []Delivery {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[group]
	if conv == nil || conv.deliveries == nil {
		return nil
	}
	return conv.deliveries.list()
}

// Delivery 取单条投递记录。
func (c *ConversationController) Delivery(group GroupID, wakeID string) (Delivery, bool) {
	if c == nil || wakeID == "" {
		return Delivery{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	conv := c.convs[group]
	if conv == nil || conv.deliveries == nil {
		return Delivery{}, false
	}
	d := conv.deliveries.get(wakeID)
	if d == nil {
		return Delivery{}, false
	}
	return d.clone(), true
}

// removeQueuedLocked 从队列里摘掉指定 wake（不存在则什么都不做）。
func (c *ConversationController) removeQueuedLocked(conv *conversation, wakeID string) bool {
	if conv == nil || wakeID == "" {
		return false
	}
	for i := range conv.queue {
		if conv.queue[i].ID == wakeID {
			conv.queue = append(conv.queue[:i], conv.queue[i+1:]...)
			return true
		}
	}
	return false
}

// queuedLocked 这条 wake 是否还在队列里。
func (c *ConversationController) queuedLocked(conv *conversation, wakeID string) bool {
	if conv == nil || wakeID == "" {
		return false
	}
	for i := range conv.queue {
		if conv.queue[i].ID == wakeID {
			return true
		}
	}
	return false
}

// enqueueLocked 把 wake 放回队列：人类目标优先（插在最后一条人类 wake 之后），
// 智能体 wake 按入队顺序排到队尾（§1.3 公平性：人类目标优先，auto wake 先入先出）。
func (c *ConversationController) enqueueLocked(conv *conversation, w Wake) {
	if conv == nil || w.Target == "" {
		return
	}
	if !w.Human {
		conv.queue = append(conv.queue, w)
		return
	}
	at := len(conv.queue)
	for i := range conv.queue {
		if !conv.queue[i].Human {
			at = i
			break
		}
	}
	conv.queue = append(conv.queue, Wake{})
	copy(conv.queue[at+1:], conv.queue[at:])
	conv.queue[at] = w
}
