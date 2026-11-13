package agenthub

// conversation_persist.go：对话状态的快照/恢复与落盘时机（P6-09，§1.6）。
//
// 三条硬约束：
//  1. **临界区不碰 I/O**：所有落盘都在 c.mu 之外发生（先取快照、再写盘）。写入用
//     persistMu 串行化，保证「后取的快照后写」，旧快照不会覆盖新状态。
//  2. **写时机是状态变化**，不是 token：enqueue/dequeue/deliver/complete/stop/pair
//     变化各写一次；run 内部的流式 token 一条都不写。
//  3. **坏文件不覆盖**：加载失败（ErrCorrupt）由调用方 BlockPersist 标记，之后的
//     状态变化只告警不写盘 —— 覆盖掉坏文件等于把唯一一份证据删了。

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"time"
)

// RestoreReport 是一次重启恢复的结果（bridge 据此写系统说明/发事件）。
type RestoreReport struct {
	GroupID GroupID
	// Restored 是恢复到队列里的未投递 wake 条数。
	Restored int
	// Interrupted 是「已投递未完成」的条数：重启后一律标 interrupted，**不重放**。
	Interrupted int
	// InterruptedTargets 是被中断的目标（写系统说明用）。
	InterruptedTargets []AgentID
	// ExpiredPairs 是加载时按 pair 窗口过期掉的时间戳条数。
	ExpiredPairs int
}

// BlockPersist 标记某群的对话状态文件不可写（坏文件/不支持的新版本）。
// 之后的落盘只回调告警，绝不覆盖 —— 覆盖坏文件会让排障失去唯一证据。
func (c *ConversationController) BlockPersist(group GroupID) {
	if c == nil || group == "" {
		return
	}
	c.persistMu.Lock()
	c.mu.Lock()
	if c.persistBlocked == nil {
		c.persistBlocked = map[GroupID]bool{}
	}
	c.persistBlocked[group] = true
	c.mu.Unlock()
	c.persistMu.Unlock()
}

// PersistBlocked 该群的状态文件是否被标记为不可写（诊断/测试用）。
func (c *ConversationController) PersistBlocked(group GroupID) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.persistBlocked[group]
}

// saveState 在锁外落盘一次（P6-09）。persister 为 nil 或该群被 BlockPersist 时
// 直接返回。错误只回调 OnPersistError：对话本身不能因为磁盘问题停下。
func (c *ConversationController) saveState(group GroupID) {
	if c == nil || c.persister == nil || group == "" {
		return
	}
	c.persistMu.Lock()
	defer c.persistMu.Unlock()
	c.mu.Lock()
	if c.persistBlocked[group] {
		c.mu.Unlock()
		return
	}
	snap := c.snapshotLocked(group)
	c.mu.Unlock()
	if snap == nil {
		return
	}
	// 状态没变就不写：DueWakes/View 这类读路径也会走到这里，不能读一次就 fsync 一次。
	data, err := json.Marshal(snap)
	if err != nil {
		if c.onPersistError != nil {
			c.onPersistError(group, err)
		}
		return
	}
	if bytes.Equal(data, c.lastSaved[group]) {
		return
	}
	c.lastSaved[group] = data
	if err := c.persister.SaveConversationState(context.Background(), snap); err != nil {
		// N5：写盘失败要回滚「已保存」指纹 —— 否则同一份状态再也走不到写盘
		// （bytes.Equal 命中），一次瞬时磁盘错误就把这一代对话的持久化永久停掉。
		delete(c.lastSaved, group)
		if c.onPersistError != nil {
			c.onPersistError(group, err)
		}
	}
}

// snapshotLocked 取一代对话的可持久化快照（调用方持锁）。没有该群时返回 nil。
//
// 队列只收**还在等**和**已投未完成**的 wake：终态记录重启后没有用处（群日志里
// 已经有消息），存下来只会让文件随历史无限增长。
func (c *ConversationController) snapshotLocked(group GroupID) *ConversationState {
	conv := c.convs[group]
	if conv == nil {
		return nil
	}
	st := &ConversationState{
		SchemaVersion:  ConversationStateSchema,
		GroupID:        group,
		ConversationID: conv.id,
		HumanSeq:       conv.humanSeq,
		Status:         conv.status,
		StopReason:     conv.stop,
		Turns:          conv.turns,
		Depth:          conv.depth,
		StartedAt:      conv.startedAt,
		UpdatedAt:      conv.updatedAt,
		LastWakeAt:     conv.lastWake,
	}
	// 队列（未投递）：保持队列顺序（人类优先在前）。
	for _, w := range conv.queue {
		if w.ID == "" || w.Target == "" {
			continue
		}
		st.Queue = append(st.Queue, ConversationQueueEntry{
			WakeID: w.ID, Target: w.Target, Sender: w.Sender, Human: w.Human,
			Depth: w.Depth, Turn: w.Turn, TriggerSeq: w.TriggerSeq, RoundID: w.RoundID,
			EnqueuedAt: w.EnqueuedAt, ReadyAt: wakeReadyAt(conv, w.ID),
			DeliveryStatus: DeliveryQueued,
		})
	}
	// 已投递未完成（running）：重启后要标 interrupted，不能重放。
	if conv.deliveries != nil {
		for _, d := range conv.deliveries.list() {
			if d.Status != DeliveryRunning {
				continue
			}
			st.Queue = append(st.Queue, ConversationQueueEntry{
				WakeID: d.WakeID, Target: d.Target, Sender: d.Sender, Human: d.Human,
				Depth: d.Depth, Turn: d.Turn, TriggerSeq: d.TriggerSeq, RoundID: d.RoundID,
				EnqueuedAt: d.EnqueuedAt, ReadyAt: d.ReadyAt, Reason: d.Reason,
				DeliveryStatus: DeliveryRunning, Attempts: d.Attempts,
			})
		}
	}
	for p, at := range conv.pairs {
		if len(at) == 0 {
			continue
		}
		st.Pairs = append(st.Pairs, ConversationPairState{
			Key: ConversationPairKey(p.From, p.To), At: append([]time.Time(nil), at...),
		})
	}
	for p, ps := range conv.prints {
		list := ps.list()
		if len(list) == 0 {
			continue
		}
		st.Prints = append(st.Prints, ConversationPrintState{
			Key: ConversationPairKey(p.From, p.To), Prints: list,
		})
	}
	return st
}

// wakeReadyAt 取账本里这条 wake 的最早可重试时刻（429 退避），没有就 nil。
func wakeReadyAt(conv *conversation, wakeID string) *time.Time {
	if conv == nil || conv.deliveries == nil {
		return nil
	}
	d := conv.deliveries.get(wakeID)
	if d == nil || d.ReadyAt == nil {
		return nil
	}
	t := *d.ReadyAt
	return &t
}

// Restore 把一份磁盘状态装回控制器（P6-09 重启恢复）。返回报告，调用方据此写
// 「已投递未完成的那几条被中断」的系统说明。
//
// 语义（§1.6）：
//   - 未投递（queued）→ 恢复排队，等调度器继续投；
//   - 已投递未完成（running）→ 账本记 failed(interrupted)，**不重放副作用**；
//   - pair 时间戳按窗口过期（重启期间过期的额度直接还回来）；
//   - 终态（stopped/exhausted）保持原样，不再自动接龙。
//
// 已有活对话的群不会被覆盖（宁可丢恢复，也不能用旧快照盖掉正在跑的状态）。
func (c *ConversationController) Restore(st *ConversationState, at time.Time) RestoreReport {
	return c.RestoreWith(st, at, nil)
}

// RestoreTextFunc 按队列条目取回触发消息正文/附件。状态文件只存协议字段（正文在
// 群日志里，图片 data URL 更不能复制进状态文件），重启后由调用方按 TriggerSeq 补回。
type RestoreTextFunc func(entry ConversationQueueEntry) (string, []Attachment)

// RestoreWith 同 Restore，但允许补回触发正文（nil = 用占位「（群消息）」）。
func (c *ConversationController) RestoreWith(st *ConversationState, at time.Time, load RestoreTextFunc) RestoreReport {
	if c == nil || st == nil || st.GroupID == "" {
		return RestoreReport{}
	}
	now := c.clockAt(at)
	rep := RestoreReport{GroupID: st.GroupID}
	c.mu.Lock()
	if existing := c.convs[st.GroupID]; existing != nil && existing.id != "" {
		c.mu.Unlock()
		return rep // 已经有一代活对话：不动
	}
	conv := c.ensureLocked(st.GroupID, now)
	conv.id = st.ConversationID
	if conv.id == "" {
		conv.id = c.nextIDLocked(now)
	}
	conv.humanSeq = st.HumanSeq
	conv.status = st.Status
	if conv.status == "" {
		conv.status = ConversationIdle
	}
	conv.stop = st.StopReason
	conv.turns = st.Turns
	conv.depth = st.Depth
	conv.startedAt = st.StartedAt
	conv.updatedAt = st.UpdatedAt
	if conv.updatedAt.IsZero() {
		conv.updatedAt = now
	}
	conv.lastWake = st.LastWakeAt
	conv.queue = nil
	conv.pairs = map[agentPair][]time.Time{}
	conv.prints = map[agentPair]*printSet{}
	// pair：按窗口过期（§1.1 滚动窗口在加载时先算一次）。
	cut := now.Add(-c.limits.PairWindow)
	for _, ps := range st.Pairs {
		p, ok := parseConversationPairKey(ps.Key)
		if !ok {
			continue
		}
		kept := make([]time.Time, 0, len(ps.At))
		for _, t := range ps.At {
			if c.limits.PairWindow >= 0 && t.Before(cut) {
				rep.ExpiredPairs++
				continue
			}
			kept = append(kept, t)
		}
		if len(kept) > 0 {
			conv.pairs[p] = kept
		}
	}
	for _, ps := range st.Prints {
		p, ok := parseConversationPairKey(ps.Key)
		if !ok {
			continue
		}
		set := newPrintSet()
		for _, fp := range ps.Prints {
			set.add(fp)
		}
		if len(set.order) > 0 {
			conv.prints[p] = set
		}
	}
	// 队列：queued 恢复排队；running 标 interrupted（不重放）。
	interrupted := map[AgentID]bool{}
	for _, e := range st.Queue {
		if e.Target == "" || e.WakeID == "" {
			continue
		}
		switch e.DeliveryStatus {
		case DeliveryRunning:
			conv.deliveries.put(&Delivery{
				WakeID: e.WakeID, ConversationID: conv.id, GroupID: st.GroupID, Target: e.Target,
				Sender: e.Sender, Human: e.Human, Depth: e.Depth, Turn: e.Turn,
				TriggerSeq: e.TriggerSeq, RoundID: e.RoundID, Status: DeliveryFailed,
				Reason: DeliveryReasonInterrupted, Note: "重启时这一轮还没跑完，已标记中断",
				Attempts: e.Attempts, EnqueuedAt: e.EnqueuedAt, UpdatedAt: now,
			})
			interrupted[e.Target] = true
			rep.Interrupted++
		default:
			w := Wake{
				ID: e.WakeID, GroupID: st.GroupID, ConversationID: conv.id, Sender: e.Sender,
				Human: e.Human, Target: e.Target, Depth: e.Depth, Turn: e.Turn,
				TriggerSeq: e.TriggerSeq, RoundID: e.RoundID, EnqueuedAt: e.EnqueuedAt,
			}
			if load != nil {
				w.Text, w.Attachments = load(e)
			}
			conv.deliveries.put(&Delivery{
				WakeID: e.WakeID, ConversationID: conv.id, GroupID: st.GroupID, Target: e.Target,
				Sender: e.Sender, Human: e.Human, Depth: e.Depth, Turn: e.Turn,
				TriggerSeq: e.TriggerSeq, RoundID: e.RoundID, Status: DeliveryQueued,
				Reason: e.Reason, Attempts: e.Attempts, ReadyAt: e.ReadyAt,
				EnqueuedAt: e.EnqueuedAt, UpdatedAt: now,
			})
			c.enqueueLocked(conv, w)
			rep.Restored++
		}
	}
	for target := range interrupted {
		rep.InterruptedTargets = append(rep.InterruptedTargets, target)
	}
	c.mu.Unlock()
	c.saveState(st.GroupID)
	return rep
}

// parseConversationPairKey 解析 `a↔b`（ConversationPairKey 的口径）。
func parseConversationPairKey(key string) (agentPair, bool) {
	a, b, ok := cutPairKey(key)
	if !ok || a == "" || b == "" {
		return agentPair{}, false
	}
	return newAgentPair(AgentID(a), AgentID(b)), true
}

func cutPairKey(key string) (string, string, bool) {
	for i := 0; i+len("↔") <= len(key); i++ {
		if key[i:i+len("↔")] == "↔" {
			return key[:i], key[i+len("↔"):], true
		}
	}
	return "", "", false
}

// DeleteState 删掉某群的持久化状态（群已删除时清理）。
func (c *ConversationController) DeleteState(ctx context.Context, group GroupID) error {
	if c == nil || c.persister == nil || group == "" {
		return nil
	}
	c.persistMu.Lock()
	defer c.persistMu.Unlock()
	c.mu.Lock()
	delete(c.convs, group)
	delete(c.persistBlocked, group)
	c.mu.Unlock()
	err := c.persister.DeleteConversationState(ctx, group)
	if err != nil && !errors.Is(err, ErrNotFound) && c.onPersistError != nil {
		c.onPersistError(group, err)
	}
	return err
}
